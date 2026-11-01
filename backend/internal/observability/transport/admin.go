package transport

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/li904338203/tabtv/backend/internal/observability/app"
	"github.com/li904338203/tabtv/backend/internal/observability/domain"
	"github.com/li904338203/tabtv/backend/internal/observability/repo"
	"github.com/li904338203/tabtv/backend/internal/provider/runcfg"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

type Handler struct{ svc *app.Service }

func NewHandler(svc *app.Service) *Handler { return &Handler{svc: svc} }

// Mount 挂载请求日志与统计。
//
// ⚠️ 请求日志走 **`system:log`**（seed 里早就单开的位），**不随 `system:read`
// 放开**：日志含脱敏后的请求体 —— 即便脱敏过，提示词、项目名、文件名这些
// 仍然是用户的内容，属于敏感面。
//
// ⚠️ 统计走 `system:read`，**不新造权限位**：新造的话已有角色不会自动长出来，
// 症状是运营 403 而看着像鉴权代码写错（CD-53 记过一次）。
func (h *Handler) Mount(r chi.Router, requireAdmin func(http.Handler) http.Handler,
	requirePerm func(string) func(http.Handler) http.Handler) {
	r.Route("/admin/request-logs", func(r chi.Router) {
		r.Use(requireAdmin, requirePerm("system:log"))
		r.Get("/api", h.apiLogs)
		r.Get("/provider", h.providerLogs)
	})
	r.Route("/admin/stats", func(r chi.Router) {
		r.Use(requireAdmin, requirePerm("system:read"))
		r.Get("/overview", h.overview)
		r.Get("/series", h.series)
	})
}

// ---------------------------------------------------------------- 请求日志

func (h *Handler) apiLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := repo.APILogFilter{
		TraceID: q.Get("traceId"), Method: q.Get("method"), Path: q.Get("path"),
		FailedOnly: q.Get("failed") == "true", Keyword: q.Get("keyword"),
		Limit: 20,
	}
	f.From, f.To = timeRange(q.Get("from"), q.Get("to"))
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			f.Limit = n
		}
	}
	if v := q.Get("cursor"); v != "" {
		f.CursorID, _ = strconv.ParseInt(v, 10, 64)
	}
	list, err := h.svc.ListAPILogs(r.Context(), f)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(list))
	for _, l := range list {
		item := map[string]any{
			"id": itoa(l.ID), "traceId": l.TraceID, "method": l.Method,
			"path": l.Path, "httpStatus": l.HTTPStatus, "bizCode": l.BizCode,
			"durationMs": l.DurationMs, "ip": l.IP,
			"requestBody": l.RequestBody,
			"createdAt":   l.CreatedAt.UTC().Format(time.RFC3339),
			// 服务端算，前端不用再按状态码判一遍（两处判不一致时没人说得清）。
			"failed": domain.Failed(l.HTTPStatus),
		}
		// ⚠️ 0 表示"这次是匿名/没有工作区上下文"，是**正常结果**。
		// 写成 "0" 的话前端会拿着它去查一个不存在的用户。
		if l.UserID != 0 {
			item["userId"] = itoa(l.UserID)
		}
		if l.AdminID != 0 {
			item["adminId"] = itoa(l.AdminID)
		}
		if l.WorkspaceID != 0 {
			item["workspaceId"] = itoa(l.WorkspaceID)
		}
		items = append(items, item)
	}
	next := ""
	if len(list) == f.Limit && len(list) > 0 {
		next = itoa(list[len(list)-1].ID)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": next})
}

func (h *Handler) providerLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := runcfg.LogFilter{
		TraceID: q.Get("traceId"), ProviderCode: q.Get("provider"),
		ModelCode: q.Get("model"), FailedOnly: q.Get("failed") == "true",
		Limit: 20,
	}
	if v := q.Get("generationId"); v != "" {
		f.GenerationID, _ = strconv.ParseInt(v, 10, 64)
	}
	f.From, f.To = timeRange(q.Get("from"), q.Get("to"))
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			f.Limit = n
		}
	}
	if v := q.Get("cursor"); v != "" {
		f.CursorID, _ = strconv.ParseInt(v, 10, 64)
	}
	list, err := h.svc.ListProviderLogs(r.Context(), f)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(list))
	for _, l := range list {
		item := map[string]any{
			"id": itoa(l.ID), "traceId": l.TraceID,
			"provider": l.ProviderCode, "model": l.ModelCode,
			"operation": l.OperationCode, "url": l.URL,
			"httpStatus": l.HTTPStatus,
			// ⚠️ `errorType` 才是"这次调用成没成"的依据：上游的业务错误常藏在
			// HTTP 200 里（可灵在 `code`、方舟在 `error.message`），
			// 而超时/断连根本没有状态码。
			"errorType":    l.ErrorType,
			"durationMs":   int(l.Duration.Milliseconds()),
			"requestBody":  l.RequestBody,
			"responseBody": l.ResponseBody,
			"createdAt":    l.CreatedAt.UTC().Format(time.RFC3339),
		}
		if l.GenerationID != 0 {
			item["generationId"] = itoa(l.GenerationID)
		}
		items = append(items, item)
	}
	next := ""
	if len(list) == f.Limit && len(list) > 0 {
		next = itoa(list[len(list)-1].ID)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": next})
}

// ---------------------------------------------------------------- 统计

func (h *Handler) overview(w http.ResponseWriter, r *http.Request) {
	from, to := timeRange(r.URL.Query().Get("from"), r.URL.Query().Get("to"))
	s, err := h.svc.Overview(r.Context(), domain.Range{From: from, To: to}, time.Now())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	byProvider := make([]map[string]any, 0, len(s.Provider))
	for _, p := range s.Provider {
		byProvider = append(byProvider, map[string]any{
			"provider": p.ProviderCode, "calls": p.Calls, "errors": p.Errors,
			"avgDurationMs": p.AvgDurationMs,
		})
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		// 把**实际用的**时间范围回给调用方：传了非法值时它会被校正，
		// 不回的话前端画出来的图与标题上的日期对不上。
		"from": s.Range.From.UTC().Format(time.RFC3339),
		"to":   s.Range.To.UTC().Format(time.RFC3339),
		"totals": map[string]any{
			"activeUsers": s.Totals.ActiveUsers,
			// ⚠️ 消耗与退还**分开给，不合并成净值**（第五次）：
			// 把退回的悄悄减掉之后，看的人拿这个数去对账本流水会对不上。
			"creditsConsumed":     s.Totals.CreditsConsumed,
			"creditsRefunded":     s.Totals.CreditsRefunded,
			"generations":         s.Totals.Generations,
			"generationSuccess":   s.Totals.GenerationSuccess,
			"generationFailed":    s.Totals.GenerationFailed,
			"generationCancelled": s.Totals.GenerationCancelled,
			"renders":             s.Totals.Renders,
			"renderSuccess":       s.Totals.RenderSuccess,
			"paidOrders":          s.Totals.PaidOrders,
			"revenueCents":        s.Totals.RevenueCents,
			"refundedCents":       s.Totals.RefundedCents,
			// ⚠️ 这是**调用次数不是成本**：真实成本要各 Provider 的单价，
			// 而那是 PARAM-04 未定稿的内容。前端标题必须照这个名字写。
			"providerCalls":  s.Totals.ProviderCalls,
			"providerErrors": s.Totals.ProviderErrors,
		},
		// ⚠️ **-1 表示"还没有数据"，不是 0%**。两者在看板上长得一模一样，
		// 一个是"全挂了"，一个是"今天没人用" —— 前端据此显示「—」。
		"generationSuccessRate": s.GenerationSuccessRate,
		"renderSuccessRate":     s.RenderSuccessRate,
		"byProvider":            byProvider,
	})
}

func (h *Handler) series(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, to := timeRange(q.Get("from"), q.Get("to"))
	m := domain.Metric(q.Get("metric"))
	pts, rg, err := h.svc.Series(r.Context(), m, domain.Range{From: from, To: to}, time.Now())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(pts))
	for _, p := range pts {
		items = append(items, map[string]any{"date": p.Date, "value": p.Value})
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"metric": string(m),
		"from":   rg.From.UTC().Format(time.RFC3339),
		"to":     rg.To.UTC().Format(time.RFC3339),
		"points": items,
	})
}

// ---------------------------------------------------------------- 工具

// timeRange 解析时间参数。
//
// ⚠️ 解析失败**当成没传**而不是报错：为一个格式问题把整个看板挡掉不值
// （同后台订单检索里的时间参数，CD-56）。范围的校正（缺省补齐、倒置交换、
// 跨度上限）在 `domain.Range.Normalize` 里 —— 这里只负责解析。
func timeRange(fromStr, toStr string) (time.Time, time.Time) {
	var from, to time.Time
	if v, err := time.Parse(time.RFC3339, fromStr); err == nil {
		from = v
	}
	if v, err := time.Parse(time.RFC3339, toStr); err == nil {
		to = v
	}
	return from, to
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
