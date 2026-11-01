package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/li904338203/tabtv/backend/internal/payment/domain"
)

// 回调编排的分支矩阵。
//
// ⚠️ 这一层用**假渠道**测，不是偷懒：真实沙箱**不会替你构造**
// 「签名对但金额不对」「验签过了但订单不存在」「履约炸了」这些回调 ——
// 而它们恰恰是丢钱的那几条路。真实凭据能验的是"我们拼的请求上游认不认"，
// 那是 PARAM-03。

type fakeChannel struct {
	event domain.CallbackEvent
	err   error
	query domain.CallbackEvent
	qErr  error
}

func (f *fakeChannel) Code() string { return "fake" }
func (f *fakeChannel) CreatePayment(context.Context, OrderRef) (PayParams, error) {
	return PayParams{Channel: "fake", Kind: "none"}, nil
}
func (f *fakeChannel) VerifyAndParse(http.Header, []byte, time.Time) (
	domain.CallbackEvent, error) {
	return f.event, f.err
}
func (f *fakeChannel) QueryOrder(context.Context, string) (domain.CallbackEvent, error) {
	return f.query, f.qErr
}
func (f *fakeChannel) Refund(context.Context, RefundRequest) (RefundResult, error) {
	return RefundResult{ChannelRefundID: "fake-refund", Succeeded: true}, nil
}
func (f *fakeChannel) CallbackResponse() (int, string, []byte) {
	return http.StatusOK, "application/json", []byte(`{"code":"SUCCESS"}`)
}

type fakeRecorder struct {
	records []domain.CallbackRecord
	stages  []string
	dup     bool
	recErr  error
}

func (r *fakeRecorder) Record(_ context.Context, in domain.CallbackRecord) (bool, error) {
	if r.recErr != nil {
		return false, r.recErr
	}
	r.records = append(r.records, in)
	return r.dup, nil
}

func (r *fakeRecorder) UpdateStage(_ context.Context, _, _, stage, _ string, _ int64) error {
	r.stages = append(r.stages, stage)
	return nil
}

func (r *fakeRecorder) lastStage() string {
	if len(r.stages) == 0 {
		return ""
	}
	return r.stages[len(r.stages)-1]
}

type fakeOrders struct {
	order       OrderInfo
	findErr     error
	fulfilErr   error
	alreadyDone bool
	fulfilCalls int
	syncNotes   []string
}

func (o *fakeOrders) FindByOutTradeNo(context.Context, string) (OrderInfo, error) {
	return o.order, o.findErr
}

func (o *fakeOrders) Fulfil(context.Context, int64, int64, string, string) (bool, error) {
	o.fulfilCalls++
	return o.alreadyDone, o.fulfilErr
}

func (o *fakeOrders) MarkSynced(_ context.Context, _ int64, note string) error {
	o.syncNotes = append(o.syncNotes, note)
	return nil
}

func setup(ch *fakeChannel, rec *fakeRecorder, ord *fakeOrders) *Service {
	reg := NewRegistry()
	reg.Register(ch)
	return New(reg, rec, ord, nil)
}

func goodEvent() domain.CallbackEvent {
	return domain.CallbackEvent{
		EventID: "evt-1", OutTradeNo: "ORD-1", ChannelTxnID: "txn-1",
		AmountCents: 2900, Succeeded: true, RawStatus: "SUCCESS",
	}
}

func goodOrder() OrderInfo {
	return OrderInfo{ID: 1, PayerID: 9, AmountCents: 2900, PayStatus: "pending"}
}

func TestHandleCallback_正常路径履约一次(t *testing.T) {
	rec := &fakeRecorder{}
	ord := &fakeOrders{order: goodOrder()}
	svc := setup(&fakeChannel{event: goodEvent()}, rec, ord)

	res := svc.HandleCallback(context.Background(), "fake", http.Header{}, []byte(`{}`))
	if res.Stage != StageFulfilled || res.Status != http.StatusOK || res.Retryable {
		t.Fatalf("正常路径应履约并回成功：%+v", res)
	}
	if ord.fulfilCalls != 1 {
		t.Fatalf("应履约一次，实际 %d 次", ord.fulfilCalls)
	}
	if string(res.Body) != `{"code":"SUCCESS"}` {
		t.Fatalf("应回渠道要的应答体：%s", res.Body)
	}
}

// ⚠️ 验签失败**也要落库留痕**：那正是排查攻击与配置错误的唯一线索。
// 但**不让重试**（签名不会因为重发而变对），所以回 400 不回 500 ——
// 500 会让渠道按故障重试一整天。
func TestHandleCallback_验签失败留痕且不让重试(t *testing.T) {
	rec := &fakeRecorder{}
	ord := &fakeOrders{order: goodOrder()}
	svc := setup(&fakeChannel{err: domain.ErrSignatureInvalid}, rec, ord)

	res := svc.HandleCallback(context.Background(), "fake", http.Header{}, []byte(`{}`))
	if res.Stage != StageSignatureFail {
		t.Fatalf("应记验签失败：%+v", res)
	}
	if res.Retryable || res.Status != http.StatusBadRequest {
		t.Fatalf("验签失败不该让渠道重试：%+v", res)
	}
	if len(rec.records) != 1 || rec.records[0].SignatureOK {
		t.Fatalf("应留一条验签失败的痕：%+v", rec.records)
	}
	if ord.fulfilCalls != 0 {
		t.Fatal("验签失败绝不能履约")
	}
	// 事件 ID 取不到时也要能落下去 —— 否则连留痕都没有。
	if rec.records[0].EventID == "" {
		t.Fatal("取不到事件 ID 时要用兜底键，保证这条记录能落库")
	}
}

// ⚠️ **重复回调返回成功**：它是 at-least-once 投递的正常结局，不是错误
// （与 MQ 的重复消息走 ack 不走 nack 同一条）。回错误的话渠道会一直重试。
func TestHandleCallback_重复回调回成功且不重复履约(t *testing.T) {
	rec := &fakeRecorder{dup: true}
	ord := &fakeOrders{order: goodOrder()}
	svc := setup(&fakeChannel{event: goodEvent()}, rec, ord)

	res := svc.HandleCallback(context.Background(), "fake", http.Header{}, []byte(`{}`))
	if res.Stage != StageDuplicated || res.Status != http.StatusOK {
		t.Fatalf("重复回调应回成功：%+v", res)
	}
	if ord.fulfilCalls != 0 {
		t.Fatal("重复回调不该再履约")
	}
}

// ⚠️ 少付一分都不履约。这是"付 1 分拿一年会员"那条攻击的最后一道防线 ——
// 前面的验签只证明"这是渠道发的"，证明不了"付够了"。
func TestHandleCallback_金额不符不履约(t *testing.T) {
	rec := &fakeRecorder{}
	ord := &fakeOrders{order: goodOrder()}
	ev := goodEvent()
	ev.AmountCents = 1
	svc := setup(&fakeChannel{event: ev}, rec, ord)

	res := svc.HandleCallback(context.Background(), "fake", http.Header{}, []byte(`{}`))
	if res.Stage != StageAmountBad || res.Retryable {
		t.Fatalf("金额不符应拒且不重试：%+v", res)
	}
	if ord.fulfilCalls != 0 {
		t.Fatal("金额不符绝不能履约")
	}
	if rec.lastStage() != StageAmountBad {
		t.Fatalf("阶段应记 amount_mismatch，得到 %q", rec.lastStage())
	}
}

// 多付不拦（渠道优惠、重复支付都可能），但要履约 —— 用户确实付够了。
func TestHandleCallback_多付照常履约(t *testing.T) {
	rec := &fakeRecorder{}
	ord := &fakeOrders{order: goodOrder()}
	ev := goodEvent()
	ev.AmountCents = 3000
	svc := setup(&fakeChannel{event: ev}, rec, ord)

	res := svc.HandleCallback(context.Background(), "fake", http.Header{}, []byte(`{}`))
	if res.Stage != StageFulfilled || ord.fulfilCalls != 1 {
		t.Fatalf("多付应照常履约：%+v", res)
	}
}

// ⚠️ 订单找不到**不让重试**：重试也不会让它出现。这多半是串了环境
// （沙箱的回调打到了生产），要人去看。
func TestHandleCallback_订单不存在不让重试(t *testing.T) {
	rec := &fakeRecorder{}
	ord := &fakeOrders{findErr: ErrOrderNotFound}
	svc := setup(&fakeChannel{event: goodEvent()}, rec, ord)

	res := svc.HandleCallback(context.Background(), "fake", http.Header{}, []byte(`{}`))
	if res.Stage != StageOrderMissing || res.Retryable {
		t.Fatalf("订单不存在应拒且不重试：%+v", res)
	}
	if rec.lastStage() != StageOrderMissing {
		t.Fatalf("阶段应记 order_not_found，得到 %q", rec.lastStage())
	}
}

// ⚠️ **外部成功、本地失败**是最要紧的一种：钱已经收了。
// 必须让渠道重试（5xx）—— 回成功的话那笔钱收了而权益永远发不出去。
func TestHandleCallback_履约失败要让渠道重试(t *testing.T) {
	rec := &fakeRecorder{}
	ord := &fakeOrders{order: goodOrder(), fulfilErr: errors.New("db down")}
	svc := setup(&fakeChannel{event: goodEvent()}, rec, ord)

	res := svc.HandleCallback(context.Background(), "fake", http.Header{}, []byte(`{}`))
	if !res.Retryable || res.Status != http.StatusInternalServerError {
		t.Fatalf("履约失败必须让渠道重试：%+v", res)
	}
	if rec.lastStage() != StageFulfilFailed {
		t.Fatalf("阶段应记 fulfil_failed，得到 %q", rec.lastStage())
	}
}

// 落库失败时也要让渠道重试 —— 这时我们还不知道这笔钱处理过没有。
func TestHandleCallback_落库失败要让渠道重试(t *testing.T) {
	rec := &fakeRecorder{recErr: errors.New("db down")}
	ord := &fakeOrders{order: goodOrder()}
	svc := setup(&fakeChannel{event: goodEvent()}, rec, ord)

	res := svc.HandleCallback(context.Background(), "fake", http.Header{}, []byte(`{}`))
	if !res.Retryable {
		t.Fatalf("落库失败应让渠道重试：%+v", res)
	}
	if ord.fulfilCalls != 0 {
		t.Fatal("还没留痕就履约的话，重放时会看不出处理过没有")
	}
}

// ⚠️ 回调**不只有成功**（关闭、撤销都会回调）。不是成功就什么都不做，
// 但要如实记下来 —— 对账时要能解释"为什么这笔有回调却没发权益"。
func TestHandleCallback_非成功状态不履约但回成功(t *testing.T) {
	rec := &fakeRecorder{}
	ord := &fakeOrders{order: goodOrder()}
	ev := goodEvent()
	ev.Succeeded = false
	ev.RawStatus = "CLOSED"
	svc := setup(&fakeChannel{event: ev}, rec, ord)

	res := svc.HandleCallback(context.Background(), "fake", http.Header{}, []byte(`{}`))
	if res.Stage != StageNotSucceeded {
		t.Fatalf("应记 not_succeeded：%+v", res)
	}
	// 回成功 —— 这条回调我们**确实收到并处理了**，让渠道重试没有意义。
	if res.Status != http.StatusOK || res.Retryable {
		t.Fatalf("已处理的非成功回调应回成功：%+v", res)
	}
	if ord.fulfilCalls != 0 {
		t.Fatal("非成功状态绝不能履约")
	}
}

// 渠道没配：不让重试（重试一万次也还是没配）。
func TestHandleCallback_渠道没配不让重试(t *testing.T) {
	rec := &fakeRecorder{}
	ord := &fakeOrders{order: goodOrder()}
	svc := setup(&fakeChannel{event: goodEvent()}, rec, ord)

	res := svc.HandleCallback(context.Background(), "别的渠道", http.Header{}, []byte(`{}`))
	if res.Retryable || res.Status != http.StatusNotFound {
		t.Fatalf("未配置的渠道不该让重试：%+v", res)
	}
}

// ⚠️ 回调体可能不是 JSON（支付宝是 form 表单、被网关拦截的是 HTML）。
// `raw_payload` 是 jsonb —— 直接塞的话整条记录落不下，**连留痕都没有了**。
func TestSafeJSON_非JSON也要能落库(t *testing.T) {
	form := []byte("out_trade_no=ORD-1&sign=abc")
	got := safeJSON(form)
	if !json.Valid(got) {
		t.Fatalf("表单体应被包成合法 JSON：%s", got)
	}
	var wrapped map[string]string
	if err := json.Unmarshal(got, &wrapped); err != nil || wrapped["raw"] != string(form) {
		t.Fatalf("原文应保留在 raw 里：%s", got)
	}
	// 本来就是 JSON 的原样保留 —— 包一层会让排查时多剥一层。
	j := []byte(`{"id":"x"}`)
	if string(safeJSON(j)) != string(j) {
		t.Fatalf("合法 JSON 应原样保留：%s", safeJSON(j))
	}
}

// ---------------------------------------------------------------- 查单

// ⚠️ 查单是「用户说付了但回调没来」的出口，走**同一段履约代码**：
// 另写一份的话，回调路径与查单路径会对同一笔钱做出不同的事。
func TestSyncOrder_查到已付就补履约(t *testing.T) {
	ord := &fakeOrders{order: goodOrder()}
	ev := goodEvent()
	svc := setup(&fakeChannel{query: ev}, &fakeRecorder{}, ord)

	paid, err := svc.SyncOrder(context.Background(), "fake", goodOrder(), "ORD-1")
	if err != nil || !paid {
		t.Fatalf("查到已付应补履约：%v %v", paid, err)
	}
	if ord.fulfilCalls != 1 {
		t.Fatalf("应履约一次，实际 %d 次", ord.fulfilCalls)
	}
	if len(ord.syncNotes) == 0 {
		t.Fatal("查单结果要记下来（对账用）")
	}
}

// ⚠️ 查不到**不是错误**：用户可能根本没去付。
// 判成错误的话对账页上全是红字，而绝大多数只是"还没付"。
func TestSyncOrder_未付不算错误也不履约(t *testing.T) {
	ord := &fakeOrders{order: goodOrder()}
	svc := setup(&fakeChannel{query: domain.CallbackEvent{
		OutTradeNo: "ORD-1", Succeeded: false, RawStatus: "NOTPAY",
	}}, &fakeRecorder{}, ord)

	paid, err := svc.SyncOrder(context.Background(), "fake", goodOrder(), "ORD-1")
	if err != nil {
		t.Fatalf("未付不该报错：%v", err)
	}
	if paid || ord.fulfilCalls != 0 {
		t.Fatal("未付不该履约")
	}
	if len(ord.syncNotes) == 0 || ord.syncNotes[0] == "" {
		t.Fatal("未付也要记一次查单结果 —— 用户问起时要能说清")
	}
}

// 查单也要核金额 —— 和回调走同一条规则。
func TestSyncOrder_金额不符不履约(t *testing.T) {
	ord := &fakeOrders{order: goodOrder()}
	ev := goodEvent()
	ev.AmountCents = 1
	svc := setup(&fakeChannel{query: ev}, &fakeRecorder{}, ord)

	paid, err := svc.SyncOrder(context.Background(), "fake", goodOrder(), "ORD-1")
	if paid || err == nil {
		t.Fatalf("金额不符应拒：%v %v", paid, err)
	}
	if ord.fulfilCalls != 0 {
		t.Fatal("金额不符绝不能履约")
	}
}

// ⚠️ **"查不动"与"找不到"是两件事**：前者（数据库抖动、SQL 写错）
// 要让渠道**重试**，后者不能重试。混为一谈的话，一次查询失败会让一条
// 本该履约的回调被永久丢掉 —— 而日志里只有一句"订单不存在"，
// 看着像对方发错了。P11-02 联调里实测撞到过（别名写错，PG 报
// missing FROM-clause，被当成了"不存在"）。
func TestHandleCallback_查订单失败要让渠道重试(t *testing.T) {
	rec := &fakeRecorder{}
	ord := &fakeOrders{findErr: errors.New("missing FROM-clause entry for table \"r\"")}
	svc := setup(&fakeChannel{event: goodEvent()}, rec, ord)

	res := svc.HandleCallback(context.Background(), "fake", http.Header{}, []byte(`{}`))
	if !res.Retryable || res.Status != http.StatusInternalServerError {
		t.Fatalf("查订单失败应让渠道重试：%+v", res)
	}
	if rec.lastStage() != StageFulfilFailed {
		t.Fatalf("阶段应记 fulfil_failed（不是 order_not_found）：%q", rec.lastStage())
	}
	if ord.fulfilCalls != 0 {
		t.Fatal("查不到订单时不能履约")
	}
}
