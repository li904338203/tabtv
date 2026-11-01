// Package transport 暴露支付回调端点。
//
// ⚠️ 这是**外网可达的公开端点**：没有 Authorization 头、没有工作区上下文、
// 没有用户会话。订单只能从渠道给的数据里解析出来（`out_trade_no`），
// 绝不能从请求里读任何"当前用户"—— 那里根本没有当前用户。
package transport

import (
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	paymentapp "github.com/li904338203/tabtv/backend/internal/payment/app"
)

// MaxCallbackBody 回调体上限。
//
// ⚠️ 必须有：这是公开端点，不限长度的话任何人都能用一个几 GB 的 body
// 把内存打满。1 MiB 对支付回调绰绰有余（微信的加密体也就几 KB）。
const MaxCallbackBody = 1 << 20

type Handler struct {
	svc *paymentapp.Service
}

func NewHandler(svc *paymentapp.Service) *Handler { return &Handler{svc: svc} }

// Mount 挂载回调端点。
//
// ⚠️ **不挂任何鉴权中间件**：渠道不会带我们的 token。
// 安全性完全靠**验签** —— 这也是为什么验签必须在做任何判断之前。
//
// ⚠️ 路径里带渠道码而不是靠内容猜：靠猜的话一个精心构造的
// 支付宝格式报文会被送去微信适配器验签，而两边的失败信息会互相污染。
func (h *Handler) Mount(r chi.Router) {
	r.Post("/payments/callback/{channel}", h.callback)
}

func (h *Handler) callback(w http.ResponseWriter, r *http.Request) {
	channel := chi.URLParam(r, "channel")
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxCallbackBody))
	if err != nil {
		// 读不完 —— 让渠道重试（也许是网络抖动）。
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("read error"))
		return
	}
	res := h.svc.HandleCallback(r.Context(), channel, r.Header, body)
	// ⚠️ 应答体**由渠道适配器给**：微信要 `{"code":"SUCCESS"}`，
	// 支付宝要纯文本 `success`。统一回一个 JSON 的话支付宝会认为我们没收到，
	// 于是按 25 小时的节奏一直重试 —— 而我们每次都在正确地处理。
	w.Header().Set("Content-Type", res.BodyType)
	w.WriteHeader(res.Status)
	_, _ = w.Write(res.Body)
}
