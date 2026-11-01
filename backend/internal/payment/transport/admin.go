package transport

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	paymentapp "github.com/li904338203/tabtv/backend/internal/payment/app"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 支付渠道配置（P11-02）。
//
// ⚠️ 凭据一律**加密存库 + 响应里只回掩码**。三态（保留/替换/清除）
// 不能省：只用"有值就写"的话，管理端回显的掩码会被当成新密钥存回去
// —— 而那一刻起这个渠道就再也用不了了，症状是"什么都没改却开始验签失败"。

// AdminHandler 渠道配置接口。
type AdminHandler struct {
	svc     *paymentapp.ChannelAdmin
	adminID func(*http.Request) int64
}

func NewAdminHandler(svc *paymentapp.ChannelAdmin,
	adminID func(*http.Request) int64) *AdminHandler {
	return &AdminHandler{svc: svc, adminID: adminID}
}

// MountAdmin 挂载渠道配置。
//
// ⚠️ 走 `system:read` / `system:write` 而不是 `member:*`：
// 改支付凭据是**运维**动作（凭据来自商户平台、配错了钱会打到别人账上），
// 而改商品价格是运营的日常。两者合在一起的话，运营会拿到改支付凭据的权限。
func (h *AdminHandler) MountAdmin(r chi.Router, requireAdmin func(http.Handler) http.Handler,
	requirePerm func(string) func(http.Handler) http.Handler) {
	r.Route("/admin/payments", func(r chi.Router) {
		r.Use(requireAdmin)
		r.With(requirePerm("system:read")).Get("/channels", h.list)
		r.With(requirePerm("system:write")).Put("/channels", h.save)
		// 回调留痕。⚠️ 走 `order:read`：这是**对账与排障**用的
		// （"这笔为什么没到账"），不是改配置。
		r.With(requirePerm("order:read")).Get("/callbacks", h.callbacks)
	})
}

func (h *AdminHandler) list(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.List(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, c := range items {
		out = append(out, map[string]any{
			"code": c.Code, "name": c.Name, "merchantId": c.MerchantID,
			"appId": c.AppID, "baseUrl": c.BaseURL, "notifyUrl": c.NotifyURL,
			"isSandbox": c.IsSandbox, "status": c.Status, "keyVersion": c.KeyVersion,
			"hasPrivateKey": c.HasPrivateKey, "hasPlatformKey": c.HasPlatformKey,
			"hasApiV3Key": c.HasAPIv3Key, "ready": c.Ready, "blocked": c.Blocked,
		})
	}
	httpx.JSON(w, http.StatusOK, out)
}

// secretField 是密钥字段的三态表达。
type secretField struct {
	// Action：`keep` 保留 · `replace` 替换 · `clear` 清除。
	Action string `json:"action"`
	Value  string `json:"value"`
}

// resolve 把三态翻成 `*string`（nil = 保留）。
//
// ⚠️ 未知的 action 按**保留**处理，不按替换：按替换的话一个拼错的
// action 会把密钥清成空串，而表单上看不出任何异常。
func (s *secretField) resolve() *string {
	if s == nil {
		return nil
	}
	switch s.Action {
	case "replace":
		v := s.Value
		return &v
	case "clear":
		empty := ""
		return &empty
	}
	return nil
}

func (h *AdminHandler) save(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code        string       `json:"code"`
		Name        string       `json:"name"`
		MerchantID  string       `json:"merchantId"`
		AppID       string       `json:"appId"`
		BaseURL     string       `json:"baseUrl"`
		NotifyURL   string       `json:"notifyUrl"`
		IsSandbox   *bool        `json:"isSandbox"`
		Status      string       `json:"status"`
		PrivateKey  *secretField `json:"privateKey"`
		PlatformKey *secretField `json:"platformKey"`
		APIv3Key    *secretField `json:"apiV3Key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Fail(w, r, httpx.Err(errcode.InvalidJSON))
		return
	}
	if err := h.svc.Save(r.Context(), h.adminID(r), paymentapp.SaveChannel{
		Code: req.Code, Name: req.Name, MerchantID: req.MerchantID,
		AppID: req.AppID, BaseURL: req.BaseURL, NotifyURL: req.NotifyURL,
		// ⚠️ 默认**沙箱**：漏传的话按沙箱处理而不是按生产 ——
		// 反过来的话一次漏传就会拿真钱去试。
		IsSandbox:  req.IsSandbox == nil || *req.IsSandbox,
		Status:     req.Status,
		PrivateKey: req.PrivateKey.resolve(), PlatformKey: req.PlatformKey.resolve(),
		APIv3Key: req.APIv3Key.resolve(),
	}); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	h.list(w, r)
}

func (h *AdminHandler) callbacks(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := h.svc.Callbacks(r.Context(), r.URL.Query().Get("outTradeNo"), limit)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, c := range rows {
		item := map[string]any{
			"id": strconv.FormatInt(c.ID, 10), "channel": c.Channel,
			"eventId": c.EventID, "stage": c.Stage,
			"signatureOk": c.SignatureOK, "outTradeNo": c.OutTradeNo,
			"amountCents": c.AmountCents, "errorMessage": c.ErrorMessage,
		}
		if c.OrderID != 0 {
			item["orderId"] = strconv.FormatInt(c.OrderID, 10)
		}
		out = append(out, item)
	}
	// ⚠️ **不下发 `raw_payload`**：它是渠道原文，里面有交易号、
	// 用户在渠道侧的标识，属于敏感面。要看原文只能直接查库
	// （与请求日志的 `system:log` 单开权限是同一条考量）。
	httpx.JSON(w, http.StatusOK, out)
}
