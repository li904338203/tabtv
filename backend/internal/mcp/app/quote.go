package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/mcp/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 报价编排（MCPT-05，MCP-F-29）：报价 → 余额 → 是否签发 → 落库。
//
// ⚠️ **MCP 不自己算价**（C-1、C-4）：单项走与 Web 端 `POST /generations/estimate` 同一个
// catalog/app.Service.Quote，计划项走与站内 Agent 规划卡同一个 wiring.QuotePlanItem ——
// 这里只做编排。实现在 internal/wiring/mcp_quote.go。
// ⚠️ **不在 MCP 侧填默认值**（如 count 缺省补 1）：estimate 不补，补了就不再逐字一致（MCP-AC-05）。

// MaxQuotePlanItems 一次计划项报价的上限（与 Agent submit_plan 的 maxPlanItems 同值）。
const MaxQuotePlanItems = 20

// DefaultQuoteTTL 是 quoteId 有效期的缺省值（MCP-PARAM-02，**设计建议值** 10 分钟）。
const DefaultQuoteTTL = 10 * time.Minute

// QuoteItem 是单项报价的参数，字段名与 estimate 请求体**完全相同**（MCP-AC-05）。
// JSON 形态同时是单项报价指纹的输入（FingerprintFor），M2 的 generate 要能复现它。
type QuoteItem struct {
	ModelCode   string         `json:"modelCode,omitempty"`
	Operation   string         `json:"operation,omitempty"`
	Count       int            `json:"count,omitempty"`
	DurationSec int            `json:"durationSec,omitempty"`
	Text        string         `json:"text,omitempty"`
	Inputs      map[string]int `json:"inputs,omitempty"`
}

// PlanQuoteItem 是一条计划项（agent/domain.PlanItem 的报价字段）。
type PlanQuoteItem struct {
	Kind          string         `json:"kind"`
	ModelCode     string         `json:"modelCode,omitempty"`
	OperationCode string         `json:"operationCode,omitempty"`
	Quantity      int            `json:"quantity,omitempty"`
	Unit          string         `json:"unit,omitempty"`
	Inputs        map[string]int `json:"inputs,omitempty"`
}

// QuoteRequest 二选一：Item（单项）或 Items（计划项）。
type QuoteRequest struct {
	Item  *QuoteItem
	Items []PlanQuoteItem
}

// Quoter 是报价端口（wiring 实现）：每项报价**业务上不可用**时放进 QuoteLine（Available=false + 原因），
// 只有内部错误（查库失败）才返回 error —— 一项不可用不该让其余项报不出价。
type Quoter interface {
	QuoteItem(ctx context.Context, p Principal, in QuoteItem) (domain.QuoteLine, error)
	QuotePlanItem(ctx context.Context, p Principal, in PlanQuoteItem) (domain.QuoteLine, error)
	// Balance 是付费主体当前可用积分（账本推导）。
	Balance(ctx context.Context, p Principal) (int, error)
}

// QuoteStore 是 mcp_quote 的写入（mcp/repo.Repo 实现）。
type QuoteStore interface {
	InsertQuote(ctx context.Context, q repo.Quote) (repo.Quote, error)
}

// QuoteResult 是一次报价的结果。QuoteID 为 0 表示未签发（NotIssuedReason 说明原因）。
type QuoteResult struct {
	Tool            string
	Lines           []domain.QuoteLine
	Total           int
	Balance         int
	Shortfall       int
	PayerAccountID  int64
	QuoteID         int64
	ExpiresAt       time.Time
	NotIssuedReason string
}

// QuoteService 是 quote 工具用的报价服务（Quotes 实现；工具层的测试用假实现）。
type QuoteService interface {
	Quote(ctx context.Context, p Principal, req QuoteRequest) (QuoteResult, error)
}

// Quotes 编排报价并签发 quoteId。
type Quotes struct {
	quoter Quoter
	store  QuoteStore
	next   func() int64
	ttl    time.Duration
	now    func() time.Time
}

// NewQuotes 构造报价服务；ttl <= 0 时取 DefaultQuoteTTL。
func NewQuotes(q Quoter, store QuoteStore, next func() int64, ttl time.Duration) *Quotes {
	if ttl <= 0 {
		ttl = DefaultQuoteTTL
	}
	return &Quotes{quoter: q, store: store, next: next, ttl: ttl, now: time.Now}
}

// SetClock 供测试拨表。
func (s *Quotes) SetClock(now func() time.Time) { s.now = now }

// FingerprintFor 返回报价绑定的工具名与参数指纹（mcp/domain.Fingerprint，与调用审计同一个实现）。
//
// 规范形态：单项 → Fingerprint("generate", QuoteItem 的 JSON)；计划项 →
// Fingerprint("plan", {"items":[PlanQuoteItem…]})。键序、数字写法、顶层 confirm* 字段都不影响结果；
// 零值字段（omitempty）与缺省等价 —— 与 estimate 的语义一致（0 = 没传）。
func FingerprintFor(req QuoteRequest) (string, []byte, error) {
	if req.Item != nil {
		fp, err := GenerateFingerprint(*req.Item)
		if err != nil {
			return "", nil, err
		}
		return domain.QuoteToolGenerate, fp, nil
	}
	b, err := json.Marshal(struct {
		Items []PlanQuoteItem `json:"items"`
	}{req.Items})
	if err != nil {
		return "", nil, err
	}
	return domain.QuoteToolPlan, domain.Fingerprint(domain.QuoteToolPlan, b), nil
}

// CanonicalGenerateItem 是 `quote` 单项与 `generate` **共用的唯一规范形态**（MCPT-10）：两边都先落到它，
// 再取 Fingerprint("generate", ·) —— 所以 quote 工具签发的 quoteId 能直接当 generate 的 confirmQuoteId。
//
// 规则（逐字段）：
//   - modelCode / operation：去首尾空白（是代码不是内容；generate 入参本来就 trim）；
//   - count：**0、缺省与 1 等价，一律记成 1** —— 计价（catalog CalcCredits）与受理（RequestCount = max(count,1)）
//     都把 ≤0 当 1。MCPT-15 实测：Cursor 的模型先 `quote {count:1}`、再 `generate` 不带 count 去确认，
//     两边的钱与要做的事完全一样，却得到 40910 fingerprint_mismatch，模型只好换着参数重试三次；
//   - durationSec：按值；0 与缺省等价（estimate 的语义：0 = 没传）。**不**把 0 补成默认 5 秒：0 秒在受理侧
//     不会被改写成 5，发给上游的是「没传」，由模型自己的默认时长决定，与显式 5 秒不一定是同一件事。
//     generate 取**归一后**的值
//     （canvasgen.Prepare：params.duration / params.count 会同步到这里），即实际计价与受理用的那个数；
//   - text：**原样**，不 trim（提示词前后空格也是内容）。generate 取 prompt 原文；
//   - inputs：按输入角色计数，丢掉 ≤0 的项，空表与缺省等价。generate 的输入是 ID 声明，
//     角色数由**服务端解析**（canvas ResolveForAccept）得出，与受理侧能力矩阵用的是同一份。
//
// ⚠️ 只绑「花多少钱、做什么」这一组字段：projectId / nodeId / params（画幅等）/ 具体是哪张参考图
// 不在指纹里 —— quote 工具不认识它们，绑进去的话 quote 签发的 quoteId 永远对不上 generate。
// 同一授权、同一工作区之内这是可接受的（确认文案展示的也正是这组字段）；已用的 quote 换参数重放
// 只会拿回原任务，不会受理第二次。
func CanonicalGenerateItem(it QuoteItem) QuoteItem {
	out := QuoteItem{ModelCode: strings.TrimSpace(it.ModelCode), Operation: strings.TrimSpace(it.Operation),
		Count: max(it.Count, 1), DurationSec: max(it.DurationSec, 0), Text: it.Text}
	for role, n := range it.Inputs {
		if n <= 0 {
			continue
		}
		if out.Inputs == nil {
			out.Inputs = map[string]int{}
		}
		out.Inputs[role] = n
	}
	return out
}

// GenerateFingerprint = Fingerprint("generate", json(CanonicalGenerateItem(it)))。
func GenerateFingerprint(it QuoteItem) ([]byte, error) {
	b, err := json.Marshal(CanonicalGenerateItem(it))
	if err != nil {
		return nil, err
	}
	return domain.Fingerprint(domain.QuoteToolGenerate, b), nil
}

// Quote 报价：逐项报价 → 合计（查溢出）→ 余额与差额 → 有不可用项不签发 → 落库签发。
func (s *Quotes) Quote(ctx context.Context, p Principal, req QuoteRequest) (QuoteResult, error) {
	if (req.Item == nil) == (len(req.Items) == 0) {
		return QuoteResult{}, httpx.Errf(errcode.InvalidArgument, "item 与 items 必须且只能给一个")
	}
	if len(req.Items) > MaxQuotePlanItems {
		return QuoteResult{}, httpx.Errf(errcode.InvalidArgument, "items 一次最多 20 项")
	}
	tool, fp, err := FingerprintFor(req)
	if err != nil {
		return QuoteResult{}, httpx.Wrap(errcode.Internal, err)
	}
	res := QuoteResult{Tool: tool, PayerAccountID: p.PayerAccountID}
	if req.Item != nil {
		l, err := s.quoter.QuoteItem(ctx, p, *req.Item)
		if err != nil {
			return QuoteResult{}, err
		}
		res.Lines = []domain.QuoteLine{l}
	} else {
		res.Lines = make([]domain.QuoteLine, 0, len(req.Items))
		for _, it := range req.Items {
			l, err := s.quoter.QuotePlanItem(ctx, p, it)
			if err != nil {
				return QuoteResult{}, err
			}
			res.Lines = append(res.Lines, l)
		}
	}
	credits := make([]int, len(res.Lines))
	for i, l := range res.Lines {
		credits[i] = l.Credits
	}
	res.Total, err = domain.SumCredits(credits)
	if errors.Is(err, domain.ErrCreditsOverflow) {
		return QuoteResult{}, httpx.Errf(errcode.InvalidArgument, "报价合计金额溢出：请减少数量或拆成几次")
	}
	if err != nil {
		return QuoteResult{}, httpx.Wrap(errcode.Internal, err)
	}
	if res.Balance, err = s.quoter.Balance(ctx, p); err != nil {
		return QuoteResult{}, err
	}
	res.Shortfall = domain.Shortfall(res.Total, res.Balance)

	d := domain.DecideQuote(res.Lines)
	if !d.Issue {
		res.NotIssuedReason = d.Reason
		return res, nil
	}
	detail, err := json.Marshal(quoteDetail{Input: detailInput(req), Items: res.Lines, TotalCredits: res.Total,
		Balance: res.Balance})
	if err != nil {
		return QuoteResult{}, httpx.Wrap(errcode.Internal, err)
	}
	q, err := s.store.InsertQuote(ctx, repo.Quote{ID: s.next(), GrantID: p.GrantID, Tool: tool, Fingerprint: fp,
		Credits: res.Total, PayerAccountID: p.PayerAccountID, Detail: detail, ExpiresAt: s.now().Add(s.ttl).UTC()})
	if err != nil {
		return QuoteResult{}, httpx.Wrap(errcode.Internal, err)
	}
	res.QuoteID, res.ExpiresAt = q.ID, q.ExpiresAt
	return res, nil
}

// quoteDetail 是 mcp_quote.detail：逐项报价 + 报价时的输入（供确认文案与审计）。
type quoteDetail struct {
	Input        any                `json:"input"`
	Items        []domain.QuoteLine `json:"items"`
	TotalCredits int                `json:"totalCredits"`
	Balance      int                `json:"balance"`
}

// detailItem 是落库的单项输入：**文本只记长度（码点）不记原文** —— 原文是提示词，
// 报价表不该变成新的泄漏面（同调用审计只存指纹与脱敏摘要，MCP-N-03）。
type detailItem struct {
	ModelCode   string         `json:"modelCode,omitempty"`
	Operation   string         `json:"operation,omitempty"`
	Count       int            `json:"count,omitempty"`
	DurationSec int            `json:"durationSec,omitempty"`
	TextChars   int            `json:"textChars,omitempty"`
	Inputs      map[string]int `json:"inputs,omitempty"`
}

func detailInput(req QuoteRequest) any {
	if it := req.Item; it != nil {
		return detailItem{ModelCode: it.ModelCode, Operation: it.Operation, Count: it.Count,
			DurationSec: it.DurationSec, TextChars: utf8.RuneCountInString(it.Text), Inputs: it.Inputs}
	}
	return req.Items
}
