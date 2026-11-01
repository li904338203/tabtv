package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/li904338203/tabtv/backend/internal/payment/domain"
)

// 回调编排：落库 → 验签 → 找单 → 核金额 → 履约。
//
// ⚠️ 每一步失败都要**落到 `payment_callback.stage` 上**。支付排障要回答的
// 恰恰是"到哪一步断的"：验签就没过（配置错或有人在打我们）· 验签过了但
// 订单找不到（下错单或串了环境）· 订单找到了但金额不对（被改过）·
// 履约本身炸了（我们的 bug）。只记一个"失败"的话这四种分不出来，
// 而它们的处置完全不同。

// Recorder 由账本域/平台侧实现：把回调原文与处理阶段落库。
type Recorder interface {
	// Record 写一条回调记录。返回 `duplicated=true` 表示这个事件已经处理过
	// （靠 `uk_callback_event(channel, channel_event_id)` 判，**不是先查再写**）。
	Record(ctx context.Context, in domain.CallbackRecord) (duplicated bool, err error)
	// UpdateStage 更新处理阶段。
	UpdateStage(ctx context.Context, channel, eventID, stage, errMsg string,
		orderID int64) error
}

// OrderGateway 由账本域实现：找单、核金额、履约。
//
// ⚠️ 支付域**不直接读写订单表**：那是账本域的事。
// 直接写的话，"收到钱之后发什么权益"会在两个域各写一遍。
type OrderGateway interface {
	// FindByOutTradeNo 按商户订单号找单。
	FindByOutTradeNo(ctx context.Context, outTradeNo string) (OrderInfo, error)
	// Fulfil 履约（幂等，见 billing 的 CAS）。
	Fulfil(ctx context.Context, payerID, orderID int64, channel, txnID string) (
		alreadyDone bool, err error)
	// MarkSynced 记一次查单结果（对账用）。
	MarkSynced(ctx context.Context, orderID int64, note string) error
}

// OrderInfo 是支付域需要知道的订单事实。
type OrderInfo struct {
	ID          int64
	PayerID     int64
	AmountCents int64
	PayStatus   string
	Channel     string
}

// ErrOrderNotFound 订单不存在。
var ErrOrderNotFound = errors.New("订单不存在")

// 回调处理阶段。与迁移里的 `ck_callback_stage` 是同一份取值。
const (
	StageReceived      = "received"
	StageSignatureFail = "signature_failed"
	StageOrderMissing  = "order_not_found"
	StageAmountBad     = "amount_mismatch"
	StageNotSucceeded  = "not_succeeded"
	StageDuplicated    = "duplicated"
	StageFulfilled     = "fulfilled"
	StageFulfilFailed  = "fulfil_failed"
)

// Service 编排回调与查单。
type Service struct {
	registry *Registry
	recorder Recorder
	orders   OrderGateway
	log      *slog.Logger
}

func New(reg *Registry, rec Recorder, orders OrderGateway, log *slog.Logger) *Service {
	return &Service{registry: reg, recorder: rec, orders: orders, log: log}
}

// CallbackResult 告诉 transport 该怎么应答。
type CallbackResult struct {
	// Retryable 让渠道**重试**（我们这边出了问题，钱是真的收到了）。
	//
	// ⚠️ 这一位极其要紧：履约失败时必须让渠道重试，否则那笔钱收了
	// 而权益永远发不出去 —— 只能人工补。反过来，验签失败、订单不存在
	// 这类**重试也没用**的情况绝不能让它重试（渠道会按小时级重试一整天）。
	Retryable bool
	Stage     string
	Status    int
	BodyType  string
	Body      []byte
}

// HandleCallback 处理一次回调。
//
// 顺序是 **验签 → 落库 → 找单 → 核金额 → 履约**，每一步的失败都单独记阶段。
//
// ⚠️ **验签在落库之前**：把未验签的报文当业务数据存下来没问题（那正是留痕），
// 但绝不能据此做任何判断。这里的写法是：验签失败也落一条（stage=signature_failed），
// 因为**那正是排查攻击与配置错误的唯一线索**。
func (s *Service) HandleCallback(ctx context.Context, channelCode string,
	header http.Header, body []byte) CallbackResult {
	ch, err := s.registry.Get(channelCode)
	if err != nil {
		// 渠道没配：**不让重试**（重试一万次也还是没配）。
		s.warn("回调渠道未配置", "channel", channelCode)
		return CallbackResult{
			Stage: StageSignatureFail, Status: http.StatusNotFound,
			BodyType: "text/plain", Body: []byte("channel not configured"),
		}
	}

	event, verr := ch.VerifyAndParse(header, body, time.Now())
	if verr != nil {
		// ⚠️ 验签失败也要**落库留痕**：那正是排查攻击与配置错误的唯一线索。
		// 事件 ID 取不到时用一个时间戳键，保证这条记录能落下去。
		id := event.EventID
		if id == "" {
			id = "unverified:" + time.Now().UTC().Format("20060102150405.000000000")
		}
		s.record(ctx, domain.CallbackRecord{
			Channel: channelCode, EventID: id, RawPayload: safeJSON(body),
			SignatureOK: false, Stage: StageSignatureFail,
			ErrorMessage: verr.Error(),
		})
		s.warn("回调验签失败", "channel", channelCode, "error", verr)
		// ⚠️ 验签失败**不让重试**：签名不会因为重发而变对。
		// 回 400 而不是 500 —— 500 会让渠道按故障重试一整天。
		return CallbackResult{
			Stage: StageSignatureFail, Status: http.StatusBadRequest,
			BodyType: "text/plain", Body: []byte("signature invalid"),
		}
	}

	dup, err := s.recorder.Record(ctx, domain.CallbackRecord{
		Channel: channelCode, EventID: event.EventID, RawPayload: safeJSON(body),
		SignatureOK: true, Stage: StageReceived,
		OutTradeNo: event.OutTradeNo, AmountCents: event.AmountCents,
	})
	if err != nil {
		// 落库失败 —— 让渠道重试：这时我们还不知道这笔钱处理过没有。
		s.warn("回调落库失败", "channel", channelCode, "error", err)
		return s.retry(ch, StageReceived)
	}
	if dup {
		// ⚠️ **重复回调返回成功**：它是 at-least-once 投递的正常结局，
		// 不是错误（与 MQ 的重复消息走 ack 不走 nack 是同一条）。
		// 回错误的话渠道会一直重试，而我们每次都"正确地"拒绝它。
		s.info("回调重复，已忽略", "channel", channelCode, "eventId", event.EventID)
		return s.success(ch, StageDuplicated)
	}

	if !event.Succeeded {
		// ⚠️ 回调**不只有成功**（关闭、撤销都会回调）。
		// 不是成功就什么都不做，但要如实记下来 —— 对账时要能解释
		// "为什么这笔有回调却没发权益"。
		_ = s.recorder.UpdateStage(ctx, channelCode, event.EventID,
			StageNotSucceeded, "渠道状态："+event.RawStatus, 0)
		return s.success(ch, StageNotSucceeded)
	}

	order, err := s.orders.FindByOutTradeNo(ctx, event.OutTradeNo)
	if err != nil && !errors.Is(err, ErrOrderNotFound) {
		// ⚠️ **"查不动"与"找不到"是两件事**：前者（数据库抖动、SQL 写错）
		// 要让渠道**重试**，后者不能重试。混为一谈的话，一次查询失败会
		// 让一条本该履约的回调被永久丢掉 —— 而日志里只有一句
		// "回调的订单不存在"，看着像对方发错了。实测在联调里撞到过
		// （别名写错，PG 报 missing FROM-clause，被当成了"不存在"）。
		_ = s.recorder.UpdateStage(ctx, channelCode, event.EventID,
			StageFulfilFailed, "查订单失败："+err.Error(), 0)
		s.warn("回调查订单失败（可重试）", "channel", channelCode,
			"outTradeNo", event.OutTradeNo, "error", err)
		return s.retry(ch, StageFulfilFailed)
	}
	if err != nil {
		msg := "订单不存在：" + event.OutTradeNo
		_ = s.recorder.UpdateStage(ctx, channelCode, event.EventID,
			StageOrderMissing, msg, 0)
		// ⚠️ 订单找不到**不让重试**：重试也不会让它出现。
		// 这多半是串了环境（沙箱的回调打到了生产），要人去看。
		s.warn("回调的订单不存在", "channel", channelCode,
			"outTradeNo", event.OutTradeNo)
		return CallbackResult{
			Stage: StageOrderMissing, Status: http.StatusBadRequest,
			BodyType: "text/plain", Body: []byte("order not found"),
		}
	}

	if err := domain.VerifyAmount(event, order.AmountCents); err != nil {
		// ⚠️ 金额不符**绝不履约**，且不让重试 —— 这是"付 1 分拿一年会员"
		// 那条攻击的最后一道防线。钱确实收到了，所以要人工处理。
		_ = s.recorder.UpdateStage(ctx, channelCode, event.EventID,
			StageAmountBad, err.Error(), order.ID)
		s.warn("回调金额与订单不符", "orderId", order.ID,
			"orderCents", order.AmountCents, "paidCents", event.AmountCents)
		return CallbackResult{
			Stage: StageAmountBad, Status: http.StatusBadRequest,
			BodyType: "text/plain", Body: []byte("amount mismatch"),
		}
	}
	if event.AmountCents > order.AmountCents {
		// 多付不拦（渠道优惠、重复支付都可能），但要留一条告警让人去看。
		s.warn("回调金额多于订单，已按足额履约", "orderId", order.ID,
			"orderCents", order.AmountCents, "paidCents", event.AmountCents)
	}

	already, err := s.orders.Fulfil(ctx, order.PayerID, order.ID,
		channelCode, event.ChannelTxnID)
	if err != nil {
		// ⚠️ **外部成功、本地失败** —— 这是最要紧的一种：钱已经收了。
		// 必须让渠道重试（回 5xx），同时把阶段记清楚让人能对账。
		// 回成功的话那笔钱收了而权益永远发不出去，只能人工补。
		_ = s.recorder.UpdateStage(ctx, channelCode, event.EventID,
			StageFulfilFailed, err.Error(), order.ID)
		s.warn("回调履约失败（已收款）", "orderId", order.ID, "error", err)
		return s.retry(ch, StageFulfilFailed)
	}
	stage := StageFulfilled
	if already {
		stage = StageDuplicated
	}
	_ = s.recorder.UpdateStage(ctx, channelCode, event.EventID, stage, "", order.ID)
	return s.success(ch, stage)
}

func (s *Service) success(ch Channel, stage string) CallbackResult {
	status, ct, body := ch.CallbackResponse()
	return CallbackResult{Stage: stage, Status: status, BodyType: ct, Body: body}
}

func (s *Service) retry(ch Channel, stage string) CallbackResult {
	_ = ch
	return CallbackResult{
		Retryable: true, Stage: stage, Status: http.StatusInternalServerError,
		BodyType: "text/plain", Body: []byte("retry later"),
	}
}

func (s *Service) record(ctx context.Context, in domain.CallbackRecord) {
	if _, err := s.recorder.Record(ctx, in); err != nil {
		s.warn("回调留痕失败", "channel", in.Channel, "error", err)
	}
}

// safeJSON 保证落库的是合法 JSON。
//
// ⚠️ 回调体可能是 **form 表单**（支付宝）甚至任意字节（被网关拦截的错误页），
// 而 `payment_callback.raw_payload` 是 jsonb —— 直接塞进去整条记录会落不下，
// 于是**连留痕都没有了**，而那正是出问题时唯一的线索。
func safeJSON(body []byte) []byte {
	if json.Valid(body) {
		return body
	}
	wrapped, _ := json.Marshal(map[string]string{"raw": string(body)})
	return wrapped
}

func (s *Service) warn(msg string, args ...any) {
	if s.log != nil {
		s.log.Warn(msg, args...)
	}
}

func (s *Service) info(msg string, args ...any) {
	if s.log != nil {
		s.log.Info(msg, args...)
	}
}

// CreatePayment 向渠道下单。
func (s *Service) CreatePayment(ctx context.Context, channelCode string,
	ref OrderRef) (PayParams, error) {
	ch, err := s.registry.Get(channelCode)
	if err != nil {
		return PayParams{}, err
	}
	return ch.CreatePayment(ctx, ref)
}

// SyncOrder 主动查单并按结果履约。
//
// ⚠️ 这是**「用户说付了但回调没来」的出口**，也是 GAP-14 的同类：
// 浏览器那边的"跳转回来了"证明不了任何事 —— 唯一可信的是
// 渠道自己说的话（回调或查单）。
//
// ⚠️ 查单走**同一段履约代码**：另写一份的话，回调路径与查单路径
// 会对同一笔钱做出不同的事，而差异只在生产上暴露。
func (s *Service) SyncOrder(ctx context.Context, channelCode string,
	order OrderInfo, outTradeNo string) (paid bool, err error) {
	ch, err := s.registry.Get(channelCode)
	if err != nil {
		return false, err
	}
	event, err := ch.QueryOrder(ctx, outTradeNo)
	if err != nil {
		return false, err
	}
	note := "查单：" + event.RawStatus
	if !event.Succeeded {
		_ = s.orders.MarkSynced(ctx, order.ID, note)
		return false, nil
	}
	if err := domain.VerifyAmount(event, order.AmountCents); err != nil {
		_ = s.orders.MarkSynced(ctx, order.ID, "查单金额不符："+err.Error())
		return false, err
	}
	if _, err := s.orders.Fulfil(ctx, order.PayerID, order.ID,
		channelCode, event.ChannelTxnID); err != nil {
		_ = s.orders.MarkSynced(ctx, order.ID, "查单已付但履约失败："+err.Error())
		return false, err
	}
	_ = s.orders.MarkSynced(ctx, order.ID, note+"，已补履约")
	return true, nil
}
