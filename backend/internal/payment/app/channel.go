// Package app 是支付渠道的编排层：取配置、构造下单参数、验回调、查单。
//
// ⚠️ 分层与 `internal/provider` 同构：`domain`（签名/解析，不碰库不发网络）·
// `app`（取配置、发请求）· `transport`（**公开**的回调端点）。
// ⚠️ 端口定义在**支付域**（`OrderGateway`），由账本域实现 ——
// 所以依赖方向是 `billing/app → payment/app`（实现方 import 定义方，
// 与 `provider/app → generation/app` 同一条：**谁定义端口谁拥有契约**）。
// 「收到钱之后做什么」是账本域的事，支付域只负责「怎么确认收到了钱」，
// 两边的边界就是这个接口。
package app

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/li904338203/tabtv/backend/internal/payment/domain"
)

// PayParams 是给前端去拉起支付的参数。
//
// ⚠️ 形状**逐渠道不同**（微信是二维码链接或 JSAPI 参数、支付宝是一段表单），
// 所以这里是一个开放的 map 而不是固定字段。硬套一个统一结构的话，
// 接第二家时会发现字段对不上，然后开始在里面塞 `extra1/extra2`。
type PayParams struct {
	Channel string
	// Kind 告诉前端怎么用这些参数：`qrcode` 扫码 · `redirect` 跳转 ·
	// `sdk` 交给客户端 SDK · `none` 无需跳转（开发支付）。
	Kind string
	// Values 渠道特定的键值。
	Values map[string]string
	// ExpireAt 支付参数的失效时间（与订单的超时对齐）。
	ExpireAt time.Time
}

// OrderRef 是发起支付时要传给渠道的订单信息。
//
// ⚠️ 只带**渠道需要知道的**：付费主体、用户昵称这些一律不传 ——
// 支付渠道不需要知道我们的用户是谁，传了就是多一份个人信息出境。
type OrderRef struct {
	// OutTradeNo 我们的订单号。⚠️ 直接用订单 ID，不另造一套编号：
	// 另造的话对账时要在两套号之间来回映射，而映射表本身会丢。
	OutTradeNo  string
	AmountCents int64
	Subject     string
	ExpireAt    time.Time
}

// Channel 是一家支付渠道的适配器。
//
// ⚠️ 接口里**没有"标记订单已支付"这类方法**：渠道只回答
// 「这笔钱收到没有」，「收到之后发什么权益」是账本域的事
// （`billing/app.FulfilOrder`）。混在一起的话，接第二家渠道时
// 履约逻辑会被抄第二份，而两份早晚不一致。
type Channel interface {
	// Code 渠道码。
	Code() string
	// CreatePayment 向渠道下单，拿回拉起支付所需的参数。
	CreatePayment(ctx context.Context, ref OrderRef) (PayParams, error)
	// VerifyAndParse 验签并解析回调。
	//
	// ⚠️ **验签在解析业务字段之前**：反过来的话，攻击者就能控制
	// 你解析什么 —— 解析器本身就是攻击面。
	VerifyAndParse(header http.Header, body []byte, now time.Time) (
		domain.CallbackEvent, error)
	// QueryOrder 主动查单。⚠️ 这是对账入口，不是可选项：
	// 「用户说付了但回调没来」是支付里最常见的工单。
	QueryOrder(ctx context.Context, outTradeNo string) (domain.CallbackEvent, error)
	// Refund 向渠道发起退款。
	//
	// ⚠️ `outRefundNo` 是**我们的**退款单号，幂等就靠它 —— 渠道按这个号
	// 去重。不给的话重试会在渠道那边退第二笔，而我们只认得一笔。
	Refund(ctx context.Context, in RefundRequest) (RefundResult, error)
	// CallbackResponse 渠道期望的回调应答（成功时）。
	//
	// ⚠️ 逐家不同且**必须答对**：微信要 200 + `{"code":"SUCCESS"}`，
	// 支付宝要纯文本 `success`。答错的话渠道会认为我们没收到，
	// 于是不停重试 —— 而我们每一次都在正确地处理并回一个它不认识的东西。
	CallbackResponse() (status int, contentType string, body []byte)
}

// RefundRequest 是一次渠道退款。
type RefundRequest struct {
	OutTradeNo  string
	OutRefundNo string
	// TotalCents 原订单金额；RefundCents 本次退多少。
	//
	// ⚠️ 两个都要传：渠道要用原单金额校验"退的不能比收的多"。
	// 只传退款额的话上游一律报参数错误，而错误信息指不出少了哪个。
	TotalCents  int64
	RefundCents int64
	Reason      string
}

// RefundResult 渠道的退款结果。
type RefundResult struct {
	// ChannelRefundID 渠道侧的退款单号，对账要用。
	ChannelRefundID string
	// Succeeded 渠道确认受理/成功。
	//
	// ⚠️ 「受理」与「到账」是两件事：微信的退款是异步的，
	// 这里的 true 只表示**渠道收下了这笔退款请求**。真正到账要等退款回调 ——
	// 当前把受理即当成功是**设计取舍**（PARAM-03 定稿时要复核）：
	// 退款受理后极少失败，而多等一道回调会让用户看不到任何反馈。
	Succeeded bool
	// RawStatus 渠道原文状态。
	RawStatus string
	// Detail 失败时的原因（脱敏后），排障用。
	Detail string
}

// ErrChannelNotConfigured 渠道没配或没启用。
var ErrChannelNotConfigured = errors.New("支付渠道未配置或未启用")

// Registry 按渠道码找适配器。
//
// ⚠️ **不是全局开关**：开发支付与真实渠道**按渠道码共存**
// （与 `provider/app.Routing` 同一条）。一刀切的话，
// 打开开发支付就等于所有渠道都变假，真实联调没法做。
type Registry struct {
	channels map[string]Channel
}

func NewRegistry() *Registry {
	return &Registry{channels: map[string]Channel{}}
}

func (r *Registry) Register(c Channel) {
	if c != nil {
		r.channels[c.Code()] = c
	}
}

func (r *Registry) Get(code string) (Channel, error) {
	c, ok := r.channels[code]
	if !ok || c == nil {
		return nil, ErrChannelNotConfigured
	}
	return c, nil
}

// Codes 已注册的渠道码（供前端列可选渠道）。
func (r *Registry) Codes() []string {
	out := make([]string, 0, len(r.channels))
	for k := range r.channels {
		out = append(out, k)
	}
	return out
}
