// Package transport 暴露画布配置的用户端与管理端接口。
package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/li904338203/tabtv/backend/internal/catalog/app"
	"github.com/li904338203/tabtv/backend/internal/catalog/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// ProviderProber 测试一个 Provider 的连通性。
type ProviderProber interface {
	Probe(ctx context.Context, providerCode string) (any, error)
}

type Handler struct {
	svc     *app.Service
	adminID func(r *http.Request) int64
	// memberLevel 取当前用户的会员等级。P11 接入会员后由身份域提供；
	// 在那之前一律返回 0（无会员），模型的会员门槛因此暂时都拦不住 ——
	// 这是**已知缺口**，不是忘了。
	memberLevel func(r *http.Request) int64
	// userID 取当前用户；未登录场景返回 0。
	userID func(r *http.Request) int64
	// prober 由组装根注入：连接测试要用共享的 HTTP 传输层，
	// 而那个包属于 provider 域，目录域不该直接依赖它的实现。
	prober ProviderProber
}

func NewHandler(svc *app.Service, adminID func(r *http.Request) int64) *Handler {
	return &Handler{
		svc: svc, adminID: adminID,
		memberLevel: func(*http.Request) int64 { return 0 },
		userID:      func(*http.Request) int64 { return 0 },
	}
}

// SetUserIDResolver 注入当前用户解析器。
func (h *Handler) SetUserIDResolver(f func(r *http.Request) int64) { h.userID = f }

// SetMemberLevelResolver 注入会员等级解析器（P11 接入）。
func (h *Handler) SetMemberLevelResolver(f func(r *http.Request) int64) {
	h.memberLevel = f
}

// MountUser 挂载用户端接口。
func (h *Handler) MountUser(r chi.Router, requireUser func(http.Handler) http.Handler) {
	r.Group(func(r chi.Router) {
		r.Use(requireUser)
		r.Get("/canvas/config", h.config)
		r.Post("/canvas/connections/validate", h.validateConnection)
	})
}

// MountAdmin 挂载管理端接口。配置读写分权限位。
func (h *Handler) MountAdmin(r chi.Router, requireAdmin func(http.Handler) http.Handler,
	requirePerm func(string) func(http.Handler) http.Handler) {
	r.Route("/admin/canvas", func(r chi.Router) {
		r.Use(requireAdmin)
		// 管理端看配置快照（节点 / 参数 / 连线规则 / 图片预设）。
		// ⚠️ 不能让管理端去调用户端的 `/canvas/config`：admin token 与 user token 类型隔离，
		// 那条路由会回 401 —— 而管理端收到 401 会清会话，表现为「一点画布配置就被踢回登录页」。
		// 复用同一个 handler：快照只有一种合成方式，两边看到的永远是同一份。
		r.With(requirePerm("canvas:read")).Get("/config", h.config)
		r.With(requirePerm("canvas:read")).Get("/param-definitions", h.listParamDefs)
		r.With(requirePerm("canvas:write")).Put("/param-definitions", h.saveParamDef)
		r.With(requirePerm("canvas:write")).Delete("/param-definitions/{code}", h.disableParamDef)
		r.With(requirePerm("canvas:write")).Put("/connection-rules", h.saveConnectionRule)
		r.With(requirePerm("canvas:read")).Get("/revision", h.revision)
	})
}

// config 下发配置快照，支持按 revision 返回 304。
//
// ⚠️ 前端缓存键必须含 revision（CD-14）。写配置即 bump，
// 漏删缓存不再可能产生脏读 —— 这是旧系统"最长 30 分钟拿旧配置"的修复。
func (h *Handler) config(w http.ResponseWriter, r *http.Request) {
	if raw := r.URL.Query().Get("revision"); raw != "" {
		if client, err := strconv.ParseInt(raw, 10, 64); err == nil {
			current, err := h.svc.Revision(r.Context())
			if err != nil {
				httpx.Fail(w, r, err)
				return
			}
			if client == current {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
	}
	snap, err := h.svc.Config(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, snap)
}

func (h *Handler) validateConnection(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TargetType string `json:"targetType"`
		Mode       string `json:"mode"`
		Inputs     []struct {
			Order      int    `json:"order"`
			SourceType string `json:"sourceType"`
		} `json:"inputs"`
	}
	if !decode(w, r, &req) {
		return
	}
	inputs := make([]domain.Input, 0, len(req.Inputs))
	for _, i := range req.Inputs {
		inputs = append(inputs, domain.Input{Order: i.Order, SourceType: i.SourceType})
	}
	res, err := h.svc.ValidateConnection(r.Context(), app.ValidateConnectionInput{
		TargetType: req.TargetType, Mode: req.Mode, Inputs: inputs,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	// 校验不通过返回 200 + valid:false，**不是**错误码：
	// 这是一次"询问"，不是一次失败的操作。前端据此禁用连线即可。
	httpx.JSON(w, http.StatusOK, res)
}

func (h *Handler) revision(w http.ResponseWriter, r *http.Request) {
	rev, err := h.svc.Revision(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]int64{"revision": rev})
}

func (h *Handler) listParamDefs(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.ListParamDefs(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	type resp struct {
		Code      string          `json:"code"`
		Name      string          `json:"name"`
		Control   string          `json:"control"`
		Config    json.RawMessage `json:"config"`
		IsEnabled bool            `json:"isEnabled"`
	}
	out := make([]resp, 0, len(list))
	for _, p := range list {
		out = append(out, resp{p.Code, p.Name, p.Control, p.Config, p.IsEnabled})
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) saveParamDef(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code    string          `json:"code"`
		Name    string          `json:"name"`
		Control string          `json:"control"`
		Config  json.RawMessage `json:"config"`
		// 缺省 = 不改启用状态（不带这个字段的老调用方不会顺手把停用的参数启用了）。
		IsEnabled *bool `json:"isEnabled"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := h.svc.SaveParamDef(r.Context(), h.adminID(r), app.ParamDefInput{
		Code: req.Code, Name: req.Name, Control: req.Control, Config: req.Config, IsEnabled: req.IsEnabled,
	}); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) disableParamDef(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.DisableParamDef(r.Context(), h.adminID(r), chi.URLParam(r, "code")); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) saveConnectionRule(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SourceType       string `json:"sourceType"`
		TargetType       string `json:"targetType"`
		IsAllowed        bool   `json:"isAllowed"`
		DefaultOperation string `json:"defaultOperation"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := h.svc.SaveConnectionRule(r.Context(), h.adminID(r), app.ConnectionRuleInput{
		SourceType: req.SourceType, TargetType: req.TargetType,
		IsAllowed: req.IsAllowed, DefaultOperation: req.DefaultOperation,
	}); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(dst); err != nil {
		httpx.Fail(w, r, httpx.Err(errcode.InvalidJSON))
		return false
	}
	return true
}

func itoa64(v int64) string { return strconv.FormatInt(v, 10) }

// SetProber 注入连接测试实现。
func (h *Handler) SetProber(p ProviderProber) { h.prober = p }
