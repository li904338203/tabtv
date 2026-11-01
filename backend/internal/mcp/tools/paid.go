package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 多段工具集（MCPT-16）的花钱闸门：报价 → **金额 > 0 才进确认服务** → 受理 → 把结果引用记在 quote 上。
//
// ⚠️ 是否收费**以报价金额为准，不按方法名硬编码**：提示词合成的 template 模式、全部已就绪被跳过的合成、
// 视频组的 draft 建组都是 0 积分 —— 这些不进确认、直接执行（让用户为 0 积分点一次确认只会教会他乱点确认）。
// ⚠️ 报价一律取各域既有的报价 / dryRun 路径（Scripts.Quote* / Workflows.Plan / Timelines.PrecheckRender），
// 这里不算价（C-1、C-4）。
// ⚠️ 防重：这些方法大多没有受理级幂等键（分镜、整图运行……），同一确认的重试靠 quote 的 CAS 只放行一次，
// 其余走 Replay —— Replay 回放**记在 quote 上的原结果**（Confirm.RecordedResult），还没记上（第一次还在跑）
// 时回「仍在处理」，**绝不再执行一次**。

// paidAsk 是一次多段花钱方法要确认的东西。
type paidAsk struct {
	// Tool 是 mcp_quote.tool（「工具.method」，如 script.storyboard）。
	Tool        string
	Fingerprint []byte
	Price       app.PriceFn
	// ConfirmQuoteID / ConfirmCredits 是本次调用带的基线确认凭证（可空）。
	ConfirmQuoteID string
	ConfirmCredits *int
}

type gateKind int

const (
	// gateRun：可以执行（Verified.QuoteID==0 表示 0 积分、没走确认）。
	gateRun gateKind = iota
	// gateConfirm：需要用户确认（Pending）。
	gateConfirm
	// gateDeclined：用户在表单里拒绝了。
	gateDeclined
	// gateReplay：这个确认之前已经受理过，Result 是记下的原结果。
	gateReplay
	// gateInProgress：这个确认已被用掉，但结果还没记上（第一次还在跑）。
	gateInProgress
)

type paidGate struct {
	Kind     gateKind
	Pending  app.Pending
	Verified app.Verified
	Price    app.Priced
	Result   json.RawMessage
}

// gatePaid 判定这一次调用：要确认 / 已拒绝 / 重放 / 仍在处理 / 可以执行。
func gatePaid(ctx context.Context, d Deps, p app.Principal, ask paidAsk) (paidGate, error) {
	if d.Confirm == nil {
		return paidGate{}, unwired("Confirm")
	}
	confirm := app.ConfirmAsk{Tool: ask.Tool, Fingerprint: ask.Fingerprint, Price: ask.Price}
	el := app.ElicitationFrom(ctx)
	proof, declined, err := confirmProof(ctx, d, p, ask.ConfirmQuoteID, ask.ConfirmCredits, el)
	if err != nil {
		return paidGate{}, err
	}
	if declined {
		return paidGate{Kind: gateDeclined}, nil
	}
	if proof == nil {
		price, err := ask.Price(ctx)
		if err != nil {
			return paidGate{}, err
		}
		if price.Credits == 0 {
			return paidGate{Kind: gateRun, Price: price}, nil // 0 积分：不进确认
		}
		pd, err := d.Confirm.BeginPriced(ctx, p, confirm, price, el.Supported)
		if err != nil {
			return paidGate{}, err
		}
		return paidGate{Kind: gateConfirm, Pending: pd, Price: price}, nil
	}
	v, err := d.Confirm.Verify(ctx, p, confirm, *proof)
	if err != nil {
		return paidGate{}, err
	}
	if v.Replay {
		res, found, err := d.Confirm.RecordedResult(ctx, p, v.QuoteID)
		if err != nil {
			return paidGate{}, err
		}
		if !found {
			return paidGate{Kind: gateInProgress, Verified: v}, nil
		}
		return paidGate{Kind: gateReplay, Verified: v, Result: res}, nil
	}
	app.ReportCredits(ctx, v.Credits)
	return paidGate{Kind: gateRun, Verified: v}, nil
}

// recordPaid 把受理结果（或失败）记在 quote 上（0 积分直接执行的没有 quote，不记）。失败只告警：
// 记不上的后果是重放时回「仍在处理」，而受理本身已经发生了。
// ⚠️ 用脱离请求取消的上下文：受理已经发生（钱可能已经扣了），客户端断开不能让这条记录丢掉。
func recordPaid(ctx context.Context, d Deps, p app.Principal, g paidGate, result any) {
	if g.Verified.QuoteID == 0 || d.Confirm == nil {
		return
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := d.Confirm.RecordResult(rctx, p, g.Verified.QuoteID, result); err != nil {
		slog.Default().Warn("MCP 受理结果没能记在报价上（重放时只能回「仍在处理」）", "quote", g.Verified.QuoteID,
			"grant", p.GrantID, "err", err)
	}
}

// paidFailure 是记在 quote 上的失败结果：重放时说清「这次失败了、费用已由原域处理」，而不是一直「仍在处理」。
type paidFailure struct {
	Failed  bool   `json:"failed"`
	Code    int    `json:"code,omitempty"`
	Message string `json:"message"`
}

func failureOf(err error) paidFailure {
	f := paidFailure{Failed: true, Message: err.Error()}
	if be, ok := err.(*httpx.BizError); ok {
		f.Code = int(be.Code)
	}
	return f
}

// replayedFailure 判断记下的结果是不是一次失败；是的话返回给模型的错误（不再执行、不再扣费）。
func replayedFailure(raw json.RawMessage) error {
	var f paidFailure
	if json.Unmarshal(raw, &f) != nil || !f.Failed {
		return nil
	}
	return httpx.Errf(errcode.Conflict, "这个确认之前已经执行过且失败了："+f.Message+
		"。本次没有再执行、没有扣费；失败的费用已按原路退回。要重试请不带 confirmQuoteId 重新调用以获取新报价").
		WithDetails(map[string]any{"reason": "replayed_failure", "code": f.Code})
}

// confirmProof 取本次调用的确认凭证：基线（confirmQuoteId + confirmCredits）优先，其次 elicitation 的回答。
// 都没有时返回 nil（要报价 / Begin）。用户在表单里拒绝 / 取消时 declined=true（quote 保持未用、不扣费）。
// generate 与多段工具共用这一份（规则同 generateProof）。
func confirmProof(ctx context.Context, d Deps, p app.Principal, quoteID string, credits *int,
	el app.ElicitContext) (*domain.Proof, bool, error) {
	if quoteID != "" || credits != nil {
		if quoteID == "" || credits == nil {
			return nil, false, httpx.Errf(errcode.InvalidArgument, "confirmQuoteId 与 confirmCredits 必须一起给（金额等于报价）")
		}
		id, err := parseID("confirmQuoteId", quoteID)
		if err != nil {
			return nil, false, err
		}
		return &domain.Proof{QuoteID: id, Credits: *credits, Via: domain.ProofBaseline}, false, nil
	}
	if el.Answer == nil {
		return nil, false, nil
	}
	// 页面确认（MCPT-13）：url 模式的 accept 只表示用户去打开了审批页，不是同意。
	if p.RequirePageConfirm {
		if el.Answer.Action != "accept" {
			return nil, true, nil
		}
	} else if !el.Answer.Accepted() {
		return nil, true, nil
	}
	pr, err := d.Confirm.ProofFromElicitation(ctx, p, el.State)
	if err != nil {
		return nil, false, err
	}
	return &pr, false, nil
}

// pendingOut 把待确认的报价翻成基线 needsConfirmation 与给模型的话（call 是要原样重调的「工具.method」）。
func pendingOut(pd app.Pending, call string) (*needsConfirmationOut, string, string) {
	nc := &needsConfirmationOut{QuoteID: idStr(pd.QuoteID), Credits: pd.Credits, Balance: pd.Balance, After: pd.After,
		PayerAccountID: idStr(pd.PayerAccountID), ExpiresAt: ts(pd.ExpiresAt), Summary: pd.Summary,
		ApprovalURL: pd.ApprovalURL}
	if pd.ApprovalURL != "" {
		msg := fmt.Sprintf("此凭据要求用户在 seeAitv 页面确认费用：%s，%d 积分（当前余额 %d，确认后剩余 %d）。还没有扣费。"+
			"把审批链接 %s 发给用户，请用户打开并点「确认」；用户批准后带 confirmQuoteId=%q、confirmCredits=%d、"+
			"其余参数完全不变再调用 %s（%s 前有效）。在客户端里确认不算数，不要自行重调直到用户说已批准。",
			pd.Summary, pd.Credits, pd.Balance, pd.After, pd.ApprovalURL, nc.QuoteID, pd.Credits, call, nc.ExpiresAt)
		return nc, msg, fmt.Sprintf("需要在 seeAitv 页面确认：%d 积分，审批链接 %s", pd.Credits, pd.ApprovalURL)
	}
	msg := fmt.Sprintf("需要用户确认费用：%s，%d 积分（当前余额 %d，确认后剩余 %d）。还没有扣费。把金额告诉用户；"+
		"用户同意后带 confirmQuoteId=%q、confirmCredits=%d、其余参数完全不变再调用 %s（%s 前有效）。",
		pd.Summary, pd.Credits, pd.Balance, pd.After, nc.QuoteID, pd.Credits, call, nc.ExpiresAt)
	return nc, msg, fmt.Sprintf("需要确认：%d 积分（quoteId %s）", pd.Credits, nc.QuoteID)
}

const (
	declinedMessage   = "用户在确认表单里拒绝了这一步，没有扣费。不要重试，除非用户改变主意。"
	inProgressMessage = "这个确认已经受理，仍在处理中（结果还没出来）。不要重新提交；稍后用同一工具的 get 查看状态。"
	paidReplayMessage = "这个确认之前已经受理过，返回的是原结果，本次没有再执行、没有扣费。不要重新提交。"
	// paidDescTail 是付费方法的统一说明（MCPT-16 范围 8）。
	paidDescTail = "付费步骤直接调用，不要先在聊天里问用户；平台会请用户确认费用（首次调用返回 status=needs_confirmation 与 quoteId、金额，" +
		"用户同意后带 confirmQuoteId 与 confirmCredits、其余参数完全不变再调用一次；0 积分的步骤不需要确认）。" +
		"提交后用 tasks 查询，宿主超时不代表失败，不要重复提交。"
)

// detachedCallTimeout 是同步模型调用（分镜、AI 提示词合成）脱离单次工具调用后的上限：与用户端 HTTP
// 同一类端点的 15 分钟一致（CD-76）。
const detachedCallTimeout = 15 * time.Minute

// detachMargin 是在单次调用截止之前留给结果序列化与传输的余量。
const detachMargin = 5 * time.Second

// runDetached 让同步的模型调用**脱离单次工具调用的 60 秒截止**（transport.DefaultCallTimeout）：
// 分镜要一两分钟，绑在调用的 ctx 上的话一到 60 秒模型请求就被掐断 —— 脚本域按失败退款，用户白等一场，
// 而重试又是同样的结局（CD-76 在用户端踩过同一个坑）。
//
// fn 在脱离取消的上下文里跑（上限 detachedCallTimeout）；在调用截止前做完就直接返回结果（done=true），
// 做不完就先返回 done=false（调用方回「仍在处理」），fn 跑完后由 finish 收尾（记结果）。
// ⚠️ finish 只在后台完成时调用一次；同步返回的结果由调用方自己收尾（避免两处都记）。
func runDetached[T any](ctx context.Context, fn func(context.Context) (T, error), finish func(T, error)) (T, bool, error) {
	var (
		mu       sync.Mutex
		finished bool // 后台做完了、结果交给了调用方
		handOff  bool // 调用方已经返回「仍在处理」，结果归后台收尾
		v        T
		err      error
	)
	done := make(chan struct{})
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), detachedCallTimeout)
	go func() {
		defer cancel()
		rv, rerr := fn(bg)
		mu.Lock()
		if handOff {
			mu.Unlock()
			finish(rv, rerr)
			return
		}
		v, err, finished = rv, rerr, true
		mu.Unlock()
		close(done)
	}()
	wait := detachedCallTimeout
	if dl, ok := ctx.Deadline(); ok {
		wait = time.Until(dl) - detachMargin
	}
	if wait <= 0 {
		wait = time.Millisecond
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	case <-ctx.Done():
	}
	mu.Lock()
	defer mu.Unlock()
	if finished { // 恰好在这一刻做完的也算同步结果（由调用方收尾，后台不再调 finish）
		return v, true, err
	}
	handOff = true
	var zero T
	return zero, false, nil
}
