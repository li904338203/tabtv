package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/mcp/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 页面审批（MCPT-13，MCP-F-44，MCP-AC-15）：凭据开了「花钱需在 seeAitv 页面确认」时，花钱工具签发的报价
// 由**发起授权的用户本人**在用户端 /mcp/approve/[quoteId] 上批准或拒绝；确认服务（Confirm.Verify）只认
// 这里写下的 approved_at。
//
// 判定顺序（GET / approve / reject 同一份）：
//
//	quote 不存在                       → 404
//	grant.user_id ≠ 当前用户           → 403 MCP_APPROVAL_NOT_OWNER，**不返回任何报价内容**
//	grant 已撤销                       → 409（INVALID_STATE_TRANSITION）
//	其余                               → 200 + 当前状态（pending / approved / rejected / used / voided / expired）
//
// ⚠️ 「同一用户才能批准」是防钓鱼的核心：攻击者把自己发起的审批链接发给受害者，受害者一点「确认」，
// 花掉的就是受害者有份的那个（团队）工作区的钱 —— 只要校验的是 quote 所属 grant 的 user_id，这条路就不存在。
// 别为了「团队管理员代批」放宽它。
// ⚠️ 非本人时不回报价内容：回了等于把别人的项目、模型、金额摘要给了任意登录用户。

// 审批状态（对外 McpApproval.state）。
const (
	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalRejected = "rejected"
	ApprovalUsed     = "used"
	ApprovalVoided   = "voided"
	ApprovalExpired  = "expired"
)

// 平台审计动作。
const (
	AuditQuoteApprove = "mcp.quote.approve"
	AuditQuoteReject  = "mcp.quote.reject"
)

// ApprovalStore 是审批要的 mcp 表读写（mcp/repo.Repo 实现）。
type ApprovalStore interface {
	GetQuoteForApproval(ctx context.Context, quoteID int64) (repo.QuoteApproval, error)
	ApproveQuote(ctx context.Context, userID, quoteID int64, now time.Time) (bool, error)
	RejectQuote(ctx context.Context, userID, quoteID int64, now time.Time) (bool, error)
}

// WorkspaceInfo 是审批页展示的工作区（名称与种类：个人 / 团队）。
type WorkspaceInfo struct {
	Name string
	Kind string // personal | team
}

// ApprovalWorkspaces 取工作区名称与种类（协作域经 wiring 实现）。
type ApprovalWorkspaces interface {
	Describe(ctx context.Context, workspaceID int64) (WorkspaceInfo, error)
}

// PayerBalances 读付费主体的当前可用积分（billing 经 wiring 实现）。
type PayerBalances interface {
	BalanceOf(ctx context.Context, payerAccountID int64) (int, error)
}

// ApprovalItem 是要做的一件事与它的积分。
type ApprovalItem struct {
	Label   string
	Credits int
}

// ApprovalView 是审批页数据（只在本人时给出）。
type ApprovalView struct {
	QuoteID        int64
	State          string
	ClientName     string
	WorkspaceID    int64
	WorkspaceName  string
	WorkspaceKind  string
	Tool           string
	Summary        string
	Items          []ApprovalItem
	Credits        int
	PayerAccountID int64
	Balance        int
	After          int
	Shortfall      int
	ExpiresAt      time.Time
	ApprovedAt     *time.Time
	RejectedAt     *time.Time
	UsedAt         *time.Time
}

// Approvals 是页面审批服务。
type Approvals struct {
	store      ApprovalStore
	workspaces ApprovalWorkspaces
	balances   PayerBalances
	audit      Auditor
	now        func() time.Time
}

// NewApprovals 构造。audit 可为 nil（仅测试）。
func NewApprovals(store ApprovalStore, ws ApprovalWorkspaces, bal PayerBalances, audit Auditor) *Approvals {
	return &Approvals{store: store, workspaces: ws, balances: bal, audit: audit, now: time.Now}
}

// SetClock 供测试拨表（过期判定与签发同一个进程时钟）。
func (s *Approvals) SetClock(now func() time.Time) { s.now = now }

// Get 是 GET /mcp/approvals/{quoteId}。页面打开时只调它（只读）。
func (s *Approvals) Get(ctx context.Context, userID, quoteID int64) (ApprovalView, error) {
	a, err := s.load(ctx, userID, quoteID)
	if err != nil {
		return ApprovalView{}, err
	}
	return s.view(ctx, a)
}

// Approve 批准。不是 pending 时**不报错**，回当前真实状态（重复点、两个标签页同时点都会发生）；
// 余额不足 → 422（前端本就禁用按钮，这是兜底：批准一笔付不起的报价只会让客户端白跑一趟）。
func (s *Approvals) Approve(ctx context.Context, userID, quoteID int64) (ApprovalView, error) {
	return s.decide(ctx, userID, quoteID, true)
}

// Reject 拒绝。语义同 Approve（不查余额）。拒绝之后这张报价对任何凭证都无效、不扣费。
func (s *Approvals) Reject(ctx context.Context, userID, quoteID int64) (ApprovalView, error) {
	return s.decide(ctx, userID, quoteID, false)
}

func (s *Approvals) decide(ctx context.Context, userID, quoteID int64, approve bool) (ApprovalView, error) {
	a, err := s.load(ctx, userID, quoteID)
	if err != nil {
		return ApprovalView{}, err
	}
	now := s.now()
	if approvalState(a.Quote, now) != ApprovalPending {
		return s.view(ctx, a)
	}
	if approve {
		bal, err := s.balances.BalanceOf(ctx, a.Quote.PayerAccountID)
		if err != nil {
			return ApprovalView{}, err
		}
		if bal < a.Quote.Credits {
			return ApprovalView{}, httpx.Errf(errcode.InsufficientCredits, fmt.Sprintf(
				"余额不足，还差 %d 积分（本次需要 %d，当前余额 %d）。补足积分后再批准",
				domain.Shortfall(a.Quote.Credits, bal), a.Quote.Credits, bal)).
				WithDetails(map[string]any{"required": a.Quote.Credits, "balance": bal,
					"shortfall": domain.Shortfall(a.Quote.Credits, bal)})
		}
	}
	var ok bool
	if approve {
		ok, err = s.store.ApproveQuote(ctx, userID, quoteID, now)
	} else {
		ok, err = s.store.RejectQuote(ctx, userID, quoteID, now)
	}
	if err != nil {
		return ApprovalView{}, httpx.Wrap(errcode.Internal, err)
	}
	// 0 行：判定与写入之间状态变了（过期、另一个标签页先点了、授权被撤销）—— 重读，按真实状态回。
	a2, err := s.load(ctx, userID, quoteID)
	if err != nil {
		return ApprovalView{}, err
	}
	if ok && s.audit != nil {
		action, field := AuditQuoteReject, "rejectedAt"
		if approve {
			action, field = AuditQuoteApprove, "approvedAt"
		}
		s.audit.Write(ctx, audit.Entry{ActorType: audit.ActorUser, ActorID: userID, WorkspaceID: a2.Grant.WorkspaceID,
			Action: action, ObjectType: "mcp_quote", ObjectID: quoteID, After: map[string]any{
				"quoteId": quoteID, "grantId": a2.Grant.ID, "clientName": a2.Grant.ClientName, "tool": a2.Quote.Tool,
				"credits": a2.Quote.Credits, "payerAccountId": a2.Quote.PayerAccountID, field: now.UTC().Format(time.RFC3339),
			}})
	}
	return s.view(ctx, a2)
}

// load 取报价并按判定顺序把「不存在 / 非本人 / 授权已撤销」翻成错误。
func (s *Approvals) load(ctx context.Context, userID, quoteID int64) (repo.QuoteApproval, error) {
	if quoteID <= 0 {
		return repo.QuoteApproval{}, httpx.Errf(errcode.NotFound, "审批不存在")
	}
	a, err := s.store.GetQuoteForApproval(ctx, quoteID)
	if errors.Is(err, repo.ErrNotFound) {
		return repo.QuoteApproval{}, httpx.Errf(errcode.NotFound, "审批不存在")
	}
	if err != nil {
		return repo.QuoteApproval{}, httpx.Wrap(errcode.Internal, err)
	}
	if a.Grant.UserID != userID {
		// ⚠️ 不带 details、不提客户端名与金额。
		return repo.QuoteApproval{}, httpx.Errf(errcode.McpApprovalNotOwner,
			"这条审批属于另一个 seeAitv 账号。请退出后用发起授权的账号登录再打开此链接")
	}
	if a.Grant.Status != repo.GrantActive {
		return repo.QuoteApproval{}, httpx.Errf(errcode.InvalidStateTransition,
			"发起这笔花费的连接已被撤销，这笔不会执行").WithDetails(map[string]any{"reason": "grant_revoked"})
	}
	return a, nil
}

// approvalState 由事实算出来（不存状态列）：作废 > 已用 > 已拒绝 > 已过期 > 已批准 > 待确认。
// 已批准而过期未用的算「已过期」：它再也用不了了，告诉用户「已批准，回客户端继续」会让他白跑一趟。
func approvalState(q repo.Quote, now time.Time) string {
	switch {
	case q.VoidedReason != "":
		return ApprovalVoided
	case q.UsedAt != nil:
		return ApprovalUsed
	case q.RejectedAt != nil:
		return ApprovalRejected
	case !now.Before(q.ExpiresAt):
		return ApprovalExpired
	case q.ApprovedAt != nil:
		return ApprovalApproved
	}
	return ApprovalPending
}

func (s *Approvals) view(ctx context.Context, a repo.QuoteApproval) (ApprovalView, error) {
	q := a.Quote
	v := ApprovalView{QuoteID: q.ID, State: approvalState(q, s.now()), ClientName: a.Grant.ClientName,
		WorkspaceID: a.Grant.WorkspaceID, Tool: q.Tool, Credits: q.Credits, PayerAccountID: q.PayerAccountID,
		ExpiresAt: q.ExpiresAt, ApprovedAt: q.ApprovedAt, RejectedAt: q.RejectedAt, UsedAt: q.UsedAt}
	v.Summary, v.Items = approvalItems(q.Detail, q.Credits)
	w, err := s.workspaces.Describe(ctx, a.Grant.WorkspaceID)
	if err != nil {
		return ApprovalView{}, err
	}
	v.WorkspaceName, v.WorkspaceKind = w.Name, w.Kind
	bal, err := s.balances.BalanceOf(ctx, q.PayerAccountID)
	if err != nil {
		return ApprovalView{}, err
	}
	v.Balance, v.After, v.Shortfall = bal, bal-q.Credits, domain.Shortfall(q.Credits, bal)
	return v, nil
}

// approvalItems 从 mcp_quote.detail 里取「要做什么」与逐项积分。两种形状：
//
//	generate（Confirm.issue）：{input:{modelCode, operation, count?, durationSec?, ...}, summary, totalCredits}
//	quote 工具（Quotes.Quote）：{input: 单项 | 计划项数组, items:[{credits, unit, ...}], totalCredits}
//
// ⚠️ detail 里本就没有提示词原文（只记长度），这里也只拼模型、操作与数量。解不开时退回一行合计 ——
// 审批页宁可少说细节，也不能因为一个历史形状打不开。
func approvalItems(raw json.RawMessage, total int) (string, []ApprovalItem) {
	var d struct {
		Input   json.RawMessage `json:"input"`
		Summary string          `json:"summary"`
		Items   []struct {
			Credits int `json:"credits"`
		} `json:"items"`
	}
	_ = json.Unmarshal(raw, &d)
	var inputs []map[string]any
	if len(d.Input) > 0 && d.Input[0] == '[' {
		_ = json.Unmarshal(d.Input, &inputs)
	} else if len(d.Input) > 0 {
		var one map[string]any
		if json.Unmarshal(d.Input, &one) == nil && one != nil {
			inputs = []map[string]any{one}
		}
	}
	summary := d.Summary
	if len(d.Items) == 0 { // generate：一项，金额即合计
		label := summary
		if len(inputs) == 1 {
			label = itemLabel(inputs[0])
		}
		if label == "" {
			label = "一次生成"
		}
		if summary == "" {
			summary = label
		}
		return summary, []ApprovalItem{{Label: label, Credits: total}}
	}
	items := make([]ApprovalItem, 0, len(d.Items))
	for i, it := range d.Items {
		label := fmt.Sprintf("第 %d 项", i+1)
		if i < len(inputs) {
			if l := itemLabel(inputs[i]); l != "" {
				label = l
			}
		}
		items = append(items, ApprovalItem{Label: label, Credits: it.Credits})
	}
	if summary == "" {
		summary = fmt.Sprintf("共 %d 项", len(items))
	}
	return summary, items
}

// itemLabel：「模型 · 操作 · 数量」。字段名覆盖单项（modelCode / operation / count / durationSec）
// 与计划项（kind / modelCode / operationCode / quantity / unit）两种输入。
func itemLabel(m map[string]any) string {
	str := func(k string) string { v, _ := m[k].(string); return strings.TrimSpace(v) }
	num := func(k string) int { v, _ := m[k].(float64); return int(v) }
	var parts []string
	if k := str("kind"); k != "" {
		parts = append(parts, k)
	}
	if v := str("modelCode"); v != "" {
		parts = append(parts, v)
	}
	op := str("operation")
	if op == "" {
		op = str("operationCode")
	}
	if op != "" {
		parts = append(parts, op)
	}
	switch {
	case num("durationSec") > 0:
		parts = append(parts, strconv.Itoa(num("durationSec"))+" 秒")
	case str("unit") == "second" && num("quantity") > 0:
		parts = append(parts, strconv.Itoa(num("quantity"))+" 秒")
	case num("quantity") > 0:
		parts = append(parts, "× "+strconv.Itoa(num("quantity")))
	}
	if n := num("count"); n > 0 {
		parts = append(parts, "× "+strconv.Itoa(n))
	}
	if n := num("textChars"); n > 0 {
		parts = append(parts, fmt.Sprintf("描述 %d 字", n))
	}
	return strings.Join(parts, " · ")
}
