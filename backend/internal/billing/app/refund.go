package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/li904338203/tabtv/backend/internal/billing/domain"
	"github.com/li904338203/tabtv/backend/internal/billing/repo"
	paymentapp "github.com/li904338203/tabtv/backend/internal/payment/app"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 订单退款与对账（P11-03）。
//
// ⚠️ **三个环节的顺序是设计的一部分**：校验可退 → **本地回收权益 + 建 pending
// 退款单（一个事务）** → 调渠道退钱 → 按结果落 success/failed。
//
// 反过来（先退钱再回收）的话：退钱成功而回收失败，用户白拿权益，
// 而钱已经出去了 —— **没有任何办法自动补救**。
// 现在这个顺序下，回收成功而退钱失败是**可恢复**的：权益已经收回、
// `refund` 行留在 pending，重试或人工介入都能接上。
// 两种都不完美，但只有一种能收场。

// RefundWindow 退款窗口，可由配置覆盖。
//
// ⚠️ **PARAM-03，策略未批准**：7 天是旧系统的做法。
func (s *Service) SetRefundWindow(d time.Duration) {
	if d > 0 {
		s.refundWindow = d
	}
}

func (s *Service) window() time.Duration {
	if s.refundWindow > 0 {
		return s.refundWindow
	}
	return domain.RefundWindow
}

// RefundChannel 由支付域实现：向渠道发起退款。
//
// ⚠️ 端口定义在这里（账本域）而不是复用支付域的 `Channel`：
// 账本域只需要「退这一笔」，不需要知道下单、验签、查单那些。
// 直接依赖整个 `Channel` 的话，账本域会被迫认识回调的形状。
type RefundChannel interface {
	Refund(ctx context.Context, channel string, in paymentapp.RefundRequest) (
		paymentapp.RefundResult, error)
}

func (s *Service) SetRefundChannel(c RefundChannel) { s.refundCh = c }
func (s *Service) SetLogger(l *slog.Logger)         { s.log = l }

// RefundEligibility 可退性判定结果。
type RefundEligibility struct {
	Refundable bool
	// Code / Reason 不可退时说清原因（码给前端分语气，文案给用户看）。
	Code   string
	Reason string
	// AmountCents 能退多少钱。
	AmountCents int64
	// RevokeCredits 会收回多少积分 —— **要在确认之前告诉用户**。
	//
	// ⚠️ 不说的话他以为"退钱就是退钱"，点完发现积分也没了 ——
	// 而那笔积分他可能正准备用。
	RevokeCredits    int
	RevokeMembership bool
}

// CheckRefund 判定一张订单能不能退（只读，可反复调）。
func (s *Service) CheckRefund(ctx context.Context, payerID, orderID int64) (
	RefundEligibility, error) {
	facts, order, err := s.repo.RefundFactsOf(ctx, payerID, orderID)
	if errors.Is(err, repo.ErrNotFound) {
		return RefundEligibility{}, httpx.Err(errcode.NotFound)
	}
	if err != nil {
		return RefundEligibility{}, httpx.Wrap(errcode.Internal, err)
	}
	code := domain.CheckRefundable(facts, time.Now().UTC(), s.window())
	out := RefundEligibility{
		Refundable: code == domain.RefundOK, Code: code,
		Reason: domain.RefundReasonText(code, facts),
	}
	if out.Refundable {
		plan := domain.PlanOrderRefund(order.AmountCents, facts.GrantRemaining,
			facts.IsMembership)
		out.AmountCents = plan.AmountCents
		out.RevokeCredits = plan.RevokeCredits
		out.RevokeMembership = plan.RevokeMembership
	}
	return out, nil
}

// RefundInput 一次退款申请。
type RefundInput struct {
	PayerID int64
	OrderID int64
	UserID  int64
	// AdminID 后台代办时填（审计要分清是用户自己申请还是客服代办）。
	AdminID int64
	Reason  string
}

// RefundOrder 执行一次退款。
func (s *Service) RefundOrder(ctx context.Context, in RefundInput) (
	repo.RefundRow, error) {
	facts, order, err := s.repo.RefundFactsOf(ctx, in.PayerID, in.OrderID)
	if errors.Is(err, repo.ErrNotFound) {
		return repo.RefundRow{}, httpx.Err(errcode.NotFound)
	}
	if err != nil {
		return repo.RefundRow{}, httpx.Wrap(errcode.Internal, err)
	}
	if code := domain.CheckRefundable(facts, time.Now().UTC(), s.window()); code != domain.RefundOK {
		return repo.RefundRow{}, httpx.Errf(errcode.InvalidStateTransition,
			domain.RefundReasonText(code, facts))
	}
	plan := domain.PlanOrderRefund(order.AmountCents, facts.GrantRemaining,
		facts.IsMembership)

	// ① 本地回收 + 建 pending 退款单（一个事务）。
	row, err := s.repo.RevokeForRefund(ctx, repo.RevokeInput{
		OrderID: in.OrderID, PayerID: in.PayerID, UserID: in.UserID,
		AdminID: in.AdminID, AmountCents: plan.AmountCents, Reason: in.Reason,
		RevokeCredits: plan.RevokeCredits, RevokeMembership: plan.RevokeMembership,
	})
	if errors.Is(err, repo.ErrRefundRaced) {
		// ⚠️ 申请与回收之间用户刚好花掉了积分（或并发申请了两次）。
		// 整笔中止 —— 这是最后一道防线，前面那次判定只是尽早告诉他。
		return repo.RefundRow{}, httpx.Errf(errcode.InvalidStateTransition,
			"权益在申请期间发生了变化（积分被使用或已有退款在处理），请刷新后重试")
	}
	if err != nil {
		return repo.RefundRow{}, httpx.Wrap(errcode.Internal, err)
	}

	// ② 调渠道退钱。
	//
	// ⚠️ **开发支付（channel=dev）不调渠道**：它本来就没收过钱。
	// 调的话假渠道会报"找不到这笔交易"，而那是一条完全正常的开发单。
	if order.Channel == "" || order.Channel == domainChannelDev ||
		s.refundCh == nil {
		if _, err := s.repo.FinishRefund(ctx, row.ID, true, "",
			"开发支付或未经渠道收款，无需调用渠道"); err != nil {
			return row, httpx.Wrap(errcode.Internal, err)
		}
		row.Status = domain.RefundSuccess
		return row, nil
	}

	res, err := s.refundCh.Refund(ctx, order.Channel, paymentapp.RefundRequest{
		OutTradeNo: itoa64(order.ID), OutRefundNo: itoa64(row.ID),
		TotalCents: order.AmountCents, RefundCents: plan.AmountCents,
		Reason: in.Reason,
	})
	if err != nil {
		// ⚠️ **请求失败 ≠ 退款失败**：连不上/超时时请求可能已经到了上游。
		// 标成 failed 的话用户会再申请一次，而渠道那边可能已经在退了 ——
		// 靠 `out_refund_no` 的幂等兜底，但状态要**留在 pending**
		// 让人去查，而不是给一个"失败了"的错误答案。
		s.warnf("退款请求失败，状态留在 pending 待人工核对", "refundId", row.ID,
			"orderId", order.ID, "error", err)
		return row, httpx.Errf(errcode.Internal,
			"已受理并回收权益，但调用支付渠道时出错（结果不确定，请勿重复申请，客服会核对）")
	}

	changed, ferr := s.repo.FinishRefund(ctx, row.ID, res.Succeeded,
		res.ChannelRefundID, res.RawStatus+" "+res.Detail)
	if ferr != nil {
		return row, httpx.Wrap(errcode.Internal, ferr)
	}
	if !changed {
		s.warnf("退款结果写入时状态已变", "refundId", row.ID)
	}
	if res.Succeeded {
		row.Status = domain.RefundSuccess
	} else {
		row.Status = domain.RefundFailed
		// ⚠️ 渠道明确拒绝时**权益已经收回来了**，要说清这一点 ——
		// 不说的话用户看到"退款失败"会以为什么都没发生，而他的积分没了。
		return row, httpx.Errf(errcode.Internal,
			"支付渠道拒绝了这笔退款（"+res.Detail+"）。权益已回收，客服会为你处理")
	}
	row.ChannelRefundID = res.ChannelRefundID
	return row, nil
}

// domainChannelDev 与 `payment/domain.ChannelDev` 同值。
//
// ⚠️ 这里不 import 支付域的 domain：账本域只需要认得这一个字符串，
// 为它拉一条依赖边不划算。两处必须一致，所以写成常量而不是字面量散在代码里。
const domainChannelDev = "dev"

// RefundsOfOrder 一张订单的退款历史。
func (s *Service) RefundsOfOrder(ctx context.Context, payerID, orderID int64) (
	[]repo.RefundRow, error) {
	// 先校验归属 —— 直接按 orderID 查的话，换一个 ID 就能看别人的退款。
	if _, err := s.Order(ctx, payerID, orderID); err != nil {
		return nil, err
	}
	out, err := s.repo.RefundsOfOrder(ctx, orderID)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return out, nil
}

// ---------------------------------------------------------------- 对账

// ReconcileQuery 由支付域实现：查一笔订单在渠道侧的状态。
type ReconcileQuery interface {
	QueryOrder(ctx context.Context, channel, outTradeNo string) (
		paid bool, cents int64, err error)
}

func (s *Service) SetReconcileQuery(q ReconcileQuery) { s.reconcileQ = q }

// ReconcileResult 一轮对账的结果。
type ReconcileResult struct {
	Checked int
	Matched int
	Diff    int
	// Skipped 查不动的（渠道没配、网络错）——**不算差异**。
	//
	// ⚠️ 把"查不动"记成 diff 的话，一次网络抖动会在对账页上
	// 刷出一屏红字，而真正的差异淹没在里面。
	Skipped int
}

// Reconcile 跑一轮对账。
//
// ⚠️ 对账的价值全在 `diff` 那一格。**必须包含 pending 的订单** ——
// "渠道收了钱而回调丢了"正藏在那里；只对已支付的单对账的话，
// 最该被发现的那一类永远发现不了。
func (s *Service) Reconcile(ctx context.Context, limit int) (ReconcileResult, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	orders, err := s.repo.ReconcileCandidates(ctx, limit)
	if err != nil {
		return ReconcileResult{}, httpx.Wrap(errcode.Internal, err)
	}
	var out ReconcileResult
	for _, o := range orders {
		out.Checked++
		if s.reconcileQ == nil {
			out.Skipped++
			continue
		}
		paid, cents, err := s.reconcileQ.QueryOrder(ctx, o.Channel, itoa64(o.ID))
		if err != nil {
			// 查不动 —— 不下结论，留到下一轮。
			out.Skipped++
			s.warnf("对账查单失败", "orderId", o.ID, "channel", o.Channel,
				"error", err)
			continue
		}
		status, detail := domain.ReconcileVerdict(
			o.PayStatus == repo.PayPaid, o.AmountCents, paid, cents)
		if err := s.repo.MarkReconciled(ctx, o.ID, status, detail); err != nil {
			return out, httpx.Wrap(errcode.Internal, err)
		}
		if status == domain.ReconcileDiff {
			out.Diff++
			// ⚠️ 差异要**打日志**而不只是落库：落库的那一行没人盯着，
			// 而"本地已履约但渠道无此笔收款"是要当天处理的。
			s.warnf("对账差异", "orderId", o.ID, "detail", detail)
		} else {
			out.Matched++
		}
	}
	return out, nil
}

// ReconcileDiffs 列出有差异的订单（管理端对账页）。
func (s *Service) ReconcileDiffs(ctx context.Context, limit int) ([]repo.Order, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	out, err := s.repo.ReconcileDiffs(ctx, limit)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return out, nil
}

func (s *Service) warnf(msg string, args ...any) {
	if s.log != nil {
		s.log.Warn(msg, args...)
	}
}

func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

// PayerOfOrder 后台按订单取付费主体。
//
// ⚠️ 后台**没有工作区上下文**：客服处理的是别人的订单，
// 他手上只有订单号。从请求里取付费主体的话，客服得先知道 payerId。
func (s *Service) PayerOfOrder(ctx context.Context, orderID int64) (int64, error) {
	payer, err := s.repo.PayerOfOrder(ctx, orderID)
	if errors.Is(err, repo.ErrNotFound) {
		return 0, httpx.Err(errcode.NotFound)
	}
	if err != nil {
		return 0, httpx.Wrap(errcode.Internal, err)
	}
	return payer, nil
}

// ---------------------------------------------------------------- 后台订单

// SearchOrders 后台检索 + 汇总。
//
// ⚠️ 列表与汇总用**同一套过滤条件**：分开算的话运营看到的
// "共 132 笔 / ¥8,900" 与下面列出来的不是同一批，而两个数字都"没错"。
func (s *Service) SearchOrders(ctx context.Context, f repo.OrderFilter) (
	[]repo.Order, repo.OrderSummary, error) {
	list, err := s.repo.SearchOrders(ctx, f)
	if err != nil {
		return nil, repo.OrderSummary{}, httpx.Wrap(errcode.Internal, err)
	}
	sum, err := s.repo.SummarizeOrders(ctx, f)
	if err != nil {
		return nil, repo.OrderSummary{}, httpx.Wrap(errcode.Internal, err)
	}
	return list, sum, nil
}

// AdminOrderDetail 后台看一张订单的全貌。
func (s *Service) AdminOrderDetail(ctx context.Context, orderID int64) (
	repo.OrderDetail, error) {
	d, err := s.repo.AdminOrderDetail(ctx, orderID)
	if errors.Is(err, repo.ErrNotFound) {
		return repo.OrderDetail{}, httpx.Err(errcode.NotFound)
	}
	if err != nil {
		return repo.OrderDetail{}, httpx.Wrap(errcode.Internal, err)
	}
	return d, nil
}

// MarkReconcileManual 人工标记对账结论。
//
// ⚠️ 人工标记与自动对账**写同一列**：另开一列的话"这笔到底对没对上"
// 会有两个答案，而报表只会读其中一个。人工标记的痕迹靠 detail 里
// 带上操作人 —— 覆盖自动结论是财务的正当权力，但要留下是谁覆盖的。
func (s *Service) MarkReconcileManual(ctx context.Context, adminID, orderID int64,
	status, note string) error {
	switch status {
	case domain.ReconcileMatched, domain.ReconcileDiff, domain.ReconcilePending,
		domain.ReconcileNone:
	default:
		return httpx.Errf(errcode.InvalidArgument,
			"对账状态只能是 none/pending/matched/diff")
	}
	detail := note
	if detail == "" {
		detail = "人工标记"
	}
	detail = "人工（管理员 " + itoa64(adminID) + "）：" + detail
	if err := s.repo.MarkReconciled(ctx, orderID, status, detail); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return httpx.Err(errcode.NotFound)
		}
		return httpx.Wrap(errcode.Internal, err)
	}
	if s.audit != nil {
		s.audit.Write(ctx, audit.Entry{
			ActorType: audit.ActorAdmin, ActorID: adminID,
			Action: "order.reconcile_manual", ObjectType: "purchase_order",
			ObjectID: orderID,
			After:    map[string]any{"status": status, "note": note},
		})
	}
	return nil
}

// ---------------------------------------------------------------- 人工调账

// AdjustInput 一次人工调账。
type AdjustInput struct {
	PayerID int64
	AdminID int64
	Amount  int
	Reason  string
	// IdempotencyKey **必填**：后台按钮一慢就会被点第二次，而这一下是直接改钱。
	IdempotencyKey string
	ExpireAt       *time.Time
}

// AdjustCredits 人工调账。
//
// ⚠️ **原因必填**：事后审计时"这 5000 积分是谁、为什么给的"只有这一行能回答。
// 给个兜底文案等于没有 —— 所以这里是**拒绝**而不是补默认值
// （与后台代办退款不同：那边用户在等着，这边是管理员自己在操作）。
func (s *Service) AdjustCredits(ctx context.Context, in AdjustInput) (
	repo.AdjustResult, error) {
	if in.Amount == 0 {
		return repo.AdjustResult{}, httpx.Errf(errcode.InvalidArgument,
			"调账金额不能为 0")
	}
	if in.Reason == "" {
		return repo.AdjustResult{}, httpx.Errf(errcode.InvalidArgument,
			"必须填写调账原因（事后审计只有这一行能回答「这笔为什么调」）")
	}
	if in.IdempotencyKey == "" {
		return repo.AdjustResult{}, httpx.Errf(errcode.InvalidArgument,
			"调账必须带 Idempotency-Key（防止重复点击直接改钱）")
	}
	res, err := s.repo.AdjustCredits(ctx, repo.AdjustInput{
		PayerID: in.PayerID, AdminID: in.AdminID, Amount: in.Amount,
		Reason: in.Reason, IdempotencyKey: in.IdempotencyKey, ExpireAt: in.ExpireAt,
	})
	switch {
	case errors.Is(err, repo.ErrAdjustDuplicate):
		return repo.AdjustResult{}, httpx.Errf(errcode.InvalidStateTransition,
			"这个调账单号已经处理过了（同一个单号只会生效一次）")
	case errors.Is(err, domain.ErrInsufficient):
		// ⚠️ 余额不足**整笔拒绝**，不扣到 0 —— 扣到 0 的话管理员以为
		// 扣了 500 而实际只扣了 120，审计里记的又是 500，三方对不上。
		bal, _ := s.repo.Balance(ctx, in.PayerID)
		return repo.AdjustResult{}, httpx.Errf(errcode.InvalidStateTransition,
			"余额不足：当前 "+itoa64(int64(bal))+" 积分，无法扣减 "+
				itoa64(int64(-in.Amount)))
	case err != nil:
		return repo.AdjustResult{}, httpx.Wrap(errcode.Internal, err)
	}
	if s.audit != nil {
		// ⚠️ 调账**必须进审计**：这是后台唯一能直接改用户资产的操作。
		s.audit.Write(ctx, audit.Entry{
			ActorType: audit.ActorAdmin, ActorID: in.AdminID,
			Action: "credits.adjust", ObjectType: "payer_account",
			ObjectID: in.PayerID,
			After: map[string]any{
				"amount": in.Amount, "reason": in.Reason,
				"idempotencyKey": in.IdempotencyKey,
				"balanceAfter":   res.BalanceAfter,
			},
		})
	}
	return res, nil
}

// PayerOverview 付费主体全貌（后台用户/团队详情）。
func (s *Service) PayerOverview(ctx context.Context, payerID int64) (
	repo.PayerView, error) {
	v, err := s.repo.PayerOverview(ctx, payerID)
	if errors.Is(err, repo.ErrNotFound) {
		return repo.PayerView{}, httpx.Err(errcode.NotFound)
	}
	if err != nil {
		return repo.PayerView{}, httpx.Wrap(errcode.Internal, err)
	}
	return v, nil
}
