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
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 花钱确认服务（MCPT-10，MT-7，开发 01 §5）：**所有花钱工具共用这一份**。
//
//	Begin：报价 → 余额不足直接拒（MCP-F-46，不签发）→ 写 mcp_quote → 返回基线 needsConfirmation
//	       或 elicitation 表单（客户端声明了 elicitation 时）。
//	Verify：同一个 domain.DecideConfirm 判凭证 → 放行前重新报价（价格变了就作废旧 quote 并签发新报价，
//	       MCP-F-41）→ 余额 → CAS 标记已用（并发只有一个进入受理，其余转重放）→ 幂等键。
//
// 基线与 elicitation **只是拿到用户同意的方式不同**：elicitation 的回答先换成同一个 domain.Proof
// （金额取自 quote），再走同一个 Verify —— 判定只有一份。
//
// 页面审批（MCPT-13，MCP-F-44）：凭据开了「花钱需在 seeAitv 页面确认」时，Begin 签发 quote 后返回
// 审批链接（客户端声明了 url 模式 elicitation 时走 url 模式，否则放在 needsConfirmation.approvalUrl），
// 不发 form 表单；Verify 把任何凭证都改判成 domain.ProofPage —— **同意只认 approved_at**，客户端的
// 回显金额与 elicitation 回答都不算数（客户端被设成自动批准工具时，那两样模型自己就给得出来）。
//
// ⚠️ 本服务**不受理、不扣费**：它只回答「这一下能不能花钱、用哪个幂等键」。受理由调用方用返回的
// IdemKey 去做；同一 quote 的所有重试都落到同一个键，受理侧 uk_generation_idem 兜底只出一个任务。

// ConfirmStore 是确认服务对 mcp_quote 的读写（mcp/repo.Repo 实现）。
type ConfirmStore interface {
	InsertQuote(ctx context.Context, q repo.Quote) (repo.Quote, error)
	GetQuote(ctx context.Context, grantID, quoteID int64) (repo.Quote, error)
	MarkQuoteUsed(ctx context.Context, grantID, quoteID int64) (bool, error)
	VoidQuote(ctx context.Context, grantID, quoteID int64, reason string) (bool, error)
}

// BalanceReader 读付费主体当前可用积分（Quoter 满足）。
type BalanceReader interface {
	Balance(ctx context.Context, p Principal) (int, error)
}

// Priced 是一次报价的结论。确认服务不自己算价：价格由调用方的报价函数给（与受理同一条路径）。
type Priced struct {
	Credits int
	// Detail 进 mcp_quote.detail.input（供确认文案与审计）。⚠️ 不得含提示词原文（MCP-N-03）。
	Detail any
	// Summary 一句话说清「做什么」：确认文案与 needsConfirmation.summary 都用它。
	Summary string
}

// PriceFn 报价：业务上报不出价（模型下线、缺价、会员不足……）时返回对应的业务错误。
type PriceFn func(ctx context.Context) (Priced, error)

// ConfirmAsk 是一次花钱调用要确认的东西。
type ConfirmAsk struct {
	// Tool 是 mcp_quote.tool（domain.QuoteTool*）。
	Tool string
	// Fingerprint 是本次参数的规范化指纹（generate 用 GenerateFingerprint）。
	Fingerprint []byte
	Price       PriceFn
}

// Pending 是「需要用户确认」：已签发的报价。
type Pending struct {
	QuoteID        int64
	Credits        int
	Balance        int
	After          int
	PayerAccountID int64
	ExpiresAt      time.Time
	Summary        string
	// ApprovalURL 非空表示这张报价要用户在 seeAitv 审批页上批准（凭据开了页面确认，MCPT-13）。
	ApprovalURL string
	// Elicit 非空表示走 elicitation（客户端声明了支持）；否则是基线 needsConfirmation。
	Elicit *ElicitRequest
}

// Verified 是通过的确认。
type Verified struct {
	QuoteID int64
	// Credits 是用户确认的金额（quote 上的金额）。
	Credits int
	IdemKey string
	// Replay 这张 quote 之前已被用掉：用同一幂等键受理只会拿回原任务，不新扣费。
	Replay bool
}

// Confirm 是确认服务。
type Confirm struct {
	store    ConfirmStore
	balance  BalanceReader
	next     func() int64
	ttl      time.Duration
	stateKey []byte
	now      func() time.Time
	// approvalBase 是用户端地址（SEEAITV_WEB_PUBLIC_URL），审批链接 = approvalBase + /mcp/approve/{quoteId}。
	approvalBase string
}

// NewConfirm 构造确认服务；ttl <= 0 取 DefaultQuoteTTL。stateKey 是 requestState 的 HMAC 子密钥
// （DeriveStateKey），为空时 elicitation 的回答一律校验失败（只剩基线可用）。
func NewConfirm(store ConfirmStore, balance BalanceReader, next func() int64, ttl time.Duration,
	stateKey []byte) *Confirm {
	if ttl <= 0 {
		ttl = DefaultQuoteTTL
	}
	return &Confirm{store: store, balance: balance, next: next, ttl: ttl, stateKey: stateKey, now: time.Now}
}

// SetApprovalBase 设定审批链接的用户端地址（末尾斜杠去掉）。为空时审批链接只有路径
// （装配层在开发环境回落到本机地址；production 由 config 强制要求 SEEAITV_WEB_PUBLIC_URL）。
func (c *Confirm) SetApprovalBase(base string) { c.approvalBase = strings.TrimRight(base, "/") }

// ApprovalURL 是某张报价的审批页地址（用户端 /mcp/approve/[quoteId]）。
func (c *Confirm) ApprovalURL(quoteID int64) string {
	return c.approvalBase + "/mcp/approve/" + strconv.FormatInt(quoteID, 10)
}

// SetClock 供测试拨表。
func (c *Confirm) SetClock(now func() time.Time) { c.now = now }

// Begin 报价并签发 quote，请用户确认。elicit 为真时返回 elicitation 表单（客户端声明了支持）。
//
// 凭据开了页面确认时不发 form 表单（elicit 被忽略）：签发后给审批链接，客户端声明了 url 模式的
// elicitation 时（ctx 里的 ElicitContext.URLSupported）同时以 url 模式交给客户端。
func (c *Confirm) Begin(ctx context.Context, p Principal, ask ConfirmAsk, elicit bool) (Pending, error) {
	price, err := ask.Price(ctx)
	if err != nil {
		return Pending{}, err
	}
	return c.issue(ctx, p, ask, price, elicit)
}

// BeginPriced 同 Begin，但报价已由调用方算好（MCPT-16：多段工具先报价，金额为 0 时直接执行、不进确认 ——
// 再让 Begin 报一次价就多查一遍目录，而两次之间价格可能变）。
func (c *Confirm) BeginPriced(ctx context.Context, p Principal, ask ConfirmAsk, price Priced, elicit bool) (Pending, error) {
	return c.issue(ctx, p, ask, price, elicit)
}

// QuoteResultStore 是「把受理结果记在 quote 上」（mcp/repo.Repo 实现；MCPT-16）。
// 单独一个可选接口：MCPT-10 的确认存储与它的假实现不必知道它。
type QuoteResultStore interface {
	SetQuoteResult(ctx context.Context, grantID, quoteID int64, result json.RawMessage) (bool, error)
}

// RecordResult 把受理结果的引用记在已用的 quote 上（只写一次，先到先得）。
//
// 没有幂等键可按的花钱方法（分镜、提示词合成、资产批量出图、组、整图运行）防重靠 quote 的 CAS：
// 同一 quoteId 的重试走 Replay，而 Replay 要拿回**原结果**而不是「已使用」—— 后者会让模型以为失败、
// 换个参数再花一次钱（开发 01 §12 R14）。⚠️ 失败也要记（带错误）：钱已经由各域退回，重放要说清是失败了，
// 不能一直显示「仍在处理」。存储不支持时静默跳过（只剩「仍在处理」那句兜底）。
func (c *Confirm) RecordResult(ctx context.Context, p Principal, quoteID int64, result any) error {
	rs, ok := c.store.(QuoteResultStore)
	if !ok || quoteID <= 0 {
		return nil
	}
	b, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = rs.SetQuoteResult(ctx, p.GrantID, quoteID, b)
	return err
}

// RecordedResult 取 quote 上记下的受理结果；没记过时 found=false（受理还在进行，或受理之前崩溃过）。
func (c *Confirm) RecordedResult(ctx context.Context, p Principal, quoteID int64) (json.RawMessage, bool, error) {
	row, err := c.store.GetQuote(ctx, p.GrantID, quoteID)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, httpx.Wrap(errcode.Internal, err)
	}
	return row.Result, len(row.Result) > 0, nil
}

// issue 余额 → 落库 → 组确认内容。余额不足**不签发**（MCP-F-46）：签了也用不了，
// 用户确认完才发现不够更糟。
func (c *Confirm) issue(ctx context.Context, p Principal, ask ConfirmAsk, price Priced, elicit bool) (Pending, error) {
	if price.Credits < 0 || price.Credits > domain.MaxQuoteCredits {
		return Pending{}, httpx.Errf(errcode.Internal, fmt.Sprintf("报价金额非法：%d", price.Credits))
	}
	bal, err := c.balance.Balance(ctx, p)
	if err != nil {
		return Pending{}, err
	}
	if price.Credits > bal {
		return Pending{}, insufficientErr(p, price.Credits, bal)
	}
	detail, err := json.Marshal(map[string]any{"input": price.Detail, "totalCredits": price.Credits,
		"balance": bal, "summary": price.Summary})
	if err != nil {
		return Pending{}, httpx.Wrap(errcode.Internal, err)
	}
	q, err := c.store.InsertQuote(ctx, repo.Quote{ID: c.next(), GrantID: p.GrantID, Tool: ask.Tool,
		Fingerprint: ask.Fingerprint, Credits: price.Credits, PayerAccountID: p.PayerAccountID, Detail: detail,
		ExpiresAt: c.now().Add(c.ttl).UTC()})
	if err != nil {
		return Pending{}, httpx.Wrap(errcode.Internal, err)
	}
	pd := Pending{QuoteID: q.ID, Credits: q.Credits, Balance: bal, After: bal - q.Credits,
		PayerAccountID: p.PayerAccountID, ExpiresAt: q.ExpiresAt, Summary: price.Summary}
	if p.RequirePageConfirm {
		pd.ApprovalURL = c.ApprovalURL(q.ID)
		if ElicitationFrom(ctx).URLSupported {
			pd.Elicit = &ElicitRequest{Message: approvalMessage(pd), URL: pd.ApprovalURL,
				ElicitationID: strconv.FormatInt(q.ID, 10), State: SignState(c.stateKey, q.ID, q.ExpiresAt)}
		}
	} else if elicit {
		pd.Elicit = &ElicitRequest{Message: elicitMessage(pd), Schema: confirmSchema(pd.Credits),
			State: SignState(c.stateKey, q.ID, q.ExpiresAt)}
	}
	ReportNeedsConfirm(ctx)
	return pd, nil
}

// ProofFromElicitation 把 elicitation 的回答换成同一个 Proof：先验 requestState 的签名与过期，
// 金额取 quote 上的（用户在表单里看到的就是它）。
func (c *Confirm) ProofFromElicitation(ctx context.Context, p Principal, state string) (domain.Proof, error) {
	id, err := VerifyState(c.stateKey, state, c.now())
	if err != nil {
		return domain.Proof{}, invalidErr(domain.ReasonNotFound, 0)
	}
	q, err := c.store.GetQuote(ctx, p.GrantID, id)
	if errors.Is(err, repo.ErrNotFound) {
		return domain.Proof{}, invalidErr(domain.ReasonNotFound, id)
	}
	if err != nil {
		return domain.Proof{}, httpx.Wrap(errcode.Internal, err)
	}
	return domain.Proof{QuoteID: id, Credits: q.Credits, Via: domain.ProofElicitation}, nil
}

// Verify 判定确认凭证；通过时返回幂等键（Replay 表示 quote 早已被用掉）。
func (c *Confirm) Verify(ctx context.Context, p Principal, ask ConfirmAsk, proof domain.Proof) (Verified, error) {
	if p.RequirePageConfirm {
		// 客户端的凭证只用来指出是哪一张 quote；同意本身只认审批页上的 approved_at（domain.DecideConfirm 6'）。
		proof.Via = domain.ProofPage
	}
	row, q, err := c.load(ctx, p.GrantID, proof.QuoteID)
	if err != nil {
		return Verified{}, err
	}
	now := c.now()
	d := domain.DecideConfirm(q, &proof, ask.Fingerprint, p.GrantID, domain.NotRequoted, now)
	if d.Kind != domain.DecideProceed {
		return c.settle(d, proof.QuoteID, row)
	}
	// 放行之前重新报价（与受理同一条路径）：确认之后价格变了不按新价扣（MCP-AC-08）。
	price, err := ask.Price(ctx)
	if err != nil {
		return Verified{}, err // quote 保持未用：模型恢复可用后可以原样重试
	}
	if d = domain.DecideConfirm(q, &proof, ask.Fingerprint, p.GrantID, price.Credits, now); d.Kind ==
		domain.DecidePriceChanged {
		ok, err := c.store.VoidQuote(ctx, p.GrantID, proof.QuoteID, string(domain.DecidePriceChanged))
		if err != nil {
			return Verified{}, httpx.Wrap(errcode.Internal, err)
		}
		if !ok {
			return c.lostRace(ctx, p, ask, proof)
		}
		return Verified{}, c.priceChanged(ctx, p, ask, row.Credits, price)
	}
	// 余额在确认之后可能被别的任务花掉了：在占用 quote 之前拒绝，quote 保持未用（充值后可原样重试）。
	bal, err := c.balance.Balance(ctx, p)
	if err != nil {
		return Verified{}, err
	}
	if bal < row.Credits {
		return Verified{}, insufficientErr(p, row.Credits, bal)
	}
	ok, err := c.store.MarkQuoteUsed(ctx, p.GrantID, proof.QuoteID)
	if err != nil {
		return Verified{}, httpx.Wrap(errcode.Internal, err)
	}
	if !ok {
		return c.lostRace(ctx, p, ask, proof)
	}
	return Verified{QuoteID: row.ID, Credits: row.Credits, IdemKey: domain.IdemKey(p.GrantID, row.ID)}, nil
}

// PriceChanged 用于重放路径：quote 已用、按幂等键却找不到任务（CAS 之后、受理之前进程崩溃），
// 而现在的价格又与用户确认的不同 —— 不能按新价受理，签发新报价请用户重新确认。
func (c *Confirm) PriceChanged(ctx context.Context, p Principal, ask ConfirmAsk, oldCredits int, price Priced) error {
	return c.priceChanged(ctx, p, ask, oldCredits, price)
}

func (c *Confirm) priceChanged(ctx context.Context, p Principal, ask ConfirmAsk, oldCredits int, price Priced) error {
	pd, err := c.issue(ctx, p, ask, price, false)
	if err != nil {
		return err // 新价下余额不足：直接说差额，不签发
	}
	next := "请把新价格告诉用户，同意后"
	if pd.ApprovalURL != "" { // 页面确认：新报价同样要用户本人在审批页上批准
		next = "请把新的审批链接 " + pd.ApprovalURL + " 发给用户，用户在页面上批准后"
	}
	return httpx.Errf(errcode.McpQuotePriceChanged, fmt.Sprintf(
		"价格已从 %d 积分变为 %d 积分，原报价已作废、没有扣费。%s带 confirmQuoteId=%d、"+
			"confirmCredits=%d 重新调用（其余参数不变）", oldCredits, pd.Credits, next, pd.QuoteID, pd.Credits)).
		WithDetails(map[string]any{"reason": string(domain.DecidePriceChanged), "oldCredits": oldCredits,
			"quote": PendingDetails(pd)})
}

// PendingDetails 是报价的对外形态（错误 details 与工具结果共用）。ID 一律字符串。
func PendingDetails(pd Pending) map[string]any {
	return map[string]any{"quoteId": strconv.FormatInt(pd.QuoteID, 10), "credits": pd.Credits,
		"balance": pd.Balance, "after": pd.After, "payerAccountId": strconv.FormatInt(pd.PayerAccountID, 10),
		"expiresAt": pd.ExpiresAt.UTC().Format(time.RFC3339), "summary": pd.Summary, "approvalUrl": pd.ApprovalURL}
}

// lostRace：CAS 写 used_at 时 0 行 —— 同一张 quote 被并发的另一次调用用掉或作废了。重新读一次再判：
// 用掉了就转重放（同一幂等键只会拿回那次的任务），作废了就是无效。
func (c *Confirm) lostRace(ctx context.Context, p Principal, ask ConfirmAsk, proof domain.Proof) (Verified, error) {
	row, q, err := c.load(ctx, p.GrantID, proof.QuoteID)
	if err != nil {
		return Verified{}, err
	}
	d := domain.DecideConfirm(q, &proof, ask.Fingerprint, p.GrantID, domain.NotRequoted, c.now())
	if d.Kind == domain.DecideProceed { // 不应发生：CAS 失败说明 used_at 已非空
		return Verified{}, httpx.Errf(errcode.Conflict, "确认凭证并发冲突，请稍后重试")
	}
	return c.settle(d, proof.QuoteID, row)
}

// settle 把非放行的结论翻成结果或错误。
func (c *Confirm) settle(d domain.Decision, quoteID int64, row repo.Quote) (Verified, error) {
	switch d.Kind {
	case domain.DecideReplay:
		return Verified{QuoteID: row.ID, Credits: row.Credits, IdemKey: domain.IdemKey(row.GrantID, row.ID),
			Replay: true}, nil
	case domain.DecideInvalid:
		return Verified{}, invalidErr(d.Reason, quoteID)
	case domain.DecideAwaitApproval:
		return Verified{}, c.awaitApprovalErr(row)
	}
	return Verified{}, httpx.Errf(errcode.Internal, "确认判定结论异常："+string(d.Kind))
}

func (c *Confirm) load(ctx context.Context, grantID, quoteID int64) (repo.Quote, *domain.Quote, error) {
	if quoteID <= 0 {
		return repo.Quote{}, nil, nil
	}
	row, err := c.store.GetQuote(ctx, grantID, quoteID)
	if errors.Is(err, repo.ErrNotFound) {
		return repo.Quote{}, nil, nil
	}
	if err != nil {
		return repo.Quote{}, nil, httpx.Wrap(errcode.Internal, err)
	}
	return row, &domain.Quote{ID: row.ID, GrantID: row.GrantID, Fingerprint: row.Fingerprint, Credits: row.Credits,
		ExpiresAt: row.ExpiresAt, Used: row.UsedAt != nil, VoidedReason: row.VoidedReason,
		Approved: row.ApprovedAt != nil, Rejected: row.RejectedAt != nil}, nil
}

// awaitApprovalErr：页面确认模式下用户还没批准（MCPT-13）。quote 保持未用、没有扣费；
// 给出审批链接，用户批准后带同一个 confirmQuoteId 原样重调即可。
func (c *Confirm) awaitApprovalErr(row repo.Quote) error {
	u := c.ApprovalURL(row.ID)
	return httpx.Errf(errcode.McpApprovalPending, fmt.Sprintf(
		"此凭据要求用户在 seeAitv 页面确认花费，用户还没有批准，没有扣费。请把审批链接 %s 发给用户；"+
			"用户批准后带 confirmQuoteId=%d、confirmCredits=%d、其余参数完全不变重新调用（%s 前有效）。"+
			"不要自行再次确认或换参数重试", u, row.ID, row.Credits, row.ExpiresAt.UTC().Format(time.RFC3339))).
		WithDetails(map[string]any{"reason": "approval_pending", "quoteId": strconv.FormatInt(row.ID, 10),
			"credits": row.Credits, "approvalUrl": u, "expiresAt": row.ExpiresAt.UTC().Format(time.RFC3339)})
}

func insufficientErr(p Principal, need, bal int) error {
	return httpx.Errf(errcode.InsufficientCredits, fmt.Sprintf(
		"余额不足，还差 %d 积分（本次需要 %d，当前余额 %d）。未签发报价、没有扣费；请告诉用户补足积分后再试，不要换参数重试",
		domain.Shortfall(need, bal), need, bal)).
		WithDetails(map[string]any{"required": need, "balance": bal, "shortfall": domain.Shortfall(need, bal),
			"payerAccountId": strconv.FormatInt(p.PayerAccountID, 10)})
}

var invalidMessages = map[domain.InvalidReason]string{
	domain.ReasonNotFound:            "确认凭证不存在或不属于当前凭据",
	domain.ReasonFingerprintMismatch: "参数与报价时不一致（改了任何一个参数都要重新报价；先用 quote 工具报价再确认的，quote 的各字段 —— 含 text，即提示词原文 —— 必须与这次调用完全相同）",
	domain.ReasonAmountMismatch:      "confirmCredits 与报价金额不一致",
	domain.ReasonVoided:              "该报价已因价格变化作废，请用随作废一起返回的新报价",
	domain.ReasonExpired:             "报价已过期",
	domain.ReasonRejected:            "用户已在 seeAitv 审批页上拒绝了这笔花费，不会执行",
}

func invalidErr(r domain.InvalidReason, quoteID int64) error {
	d := map[string]any{"reason": string(r)}
	if quoteID > 0 {
		d["quoteId"] = strconv.FormatInt(quoteID, 10)
	}
	return httpx.Errf(errcode.McpQuoteInvalid, invalidMessages[r]+"：没有扣费。请不带 confirmQuoteId 重新调用以获取新报价").
		WithDetails(d)
}

// approvalMessage 是 url 模式（审批页）的说明：去哪里、确认什么。
func approvalMessage(pd Pending) string {
	return fmt.Sprintf("%s\n将花费 %d 积分（付费账户 %d）。请在 seeAitv 审批页上查看金额并确认：%s\n报价 %s 前有效。",
		pd.Summary, pd.Credits, pd.PayerAccountID, pd.ApprovalURL, pd.ExpiresAt.UTC().Format(time.RFC3339))
}

// elicitMessage 是 elicitation 表单的说明：做什么、积分、付费账户、余额、确认后剩余（MCP-AC-07）。
func elicitMessage(pd Pending) string {
	return fmt.Sprintf("%s\n将花费 %d 积分（付费账户 %d）。当前余额 %d，确认后剩余 %d。\n报价 %s 前有效。",
		pd.Summary, pd.Credits, pd.PayerAccountID, pd.Balance, pd.After, pd.ExpiresAt.UTC().Format(time.RFC3339))
}

// confirmSchema 是确认表单：只有一个必填的布尔「确认」（只允许顶层字段，规范要求）。
func confirmSchema(credits int) map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{ElicitConfirmField: map[string]any{
			"type": "boolean", "title": fmt.Sprintf("确认花费 %d 积分", credits),
			"description": "勾选并提交即同意本次扣费；取消或拒绝不会扣费",
		}},
		"required": []string{ElicitConfirmField},
	}
}
