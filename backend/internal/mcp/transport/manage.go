package transport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	collabdomain "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	collabtransport "github.com/li904338203/tabtv/backend/internal/collaboration/transport"
	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 三级连接管理与开关的 HTTP 入口（MCPT-14）。
//
//	用户       GET/DELETE /me/mcp/grants…                       —— KeysHandler（keys.go），撤销走 app.Manage
//	工作区管理员 GET/DELETE /workspaces/current/mcp/grants…、GET/PUT …/mcp/settings —— WorkspaceHandler
//	平台管理员   GET/DELETE /admin/mcp/grants…、GET/PUT /admin/mcp/settings、GET /admin/mcp/stats —— AdminHandler

// connResp 对应 OpenAPI 的 McpConnection（McpGrant + 是谁连的 + 调用次数）。
type connResp struct {
	grantResp
	UserID   string  `json:"userId"`
	UserName *string `json:"userName"`
	Calls    int64   `json:"calls"`
}

func toConn(row repo.GrantRow, names map[int64]string) connResp {
	c := connResp{grantResp: toGrantRow(row), UserID: idStr(row.UserID), Calls: row.Calls}
	if n, ok := names[row.UserID]; ok {
		c.UserName = &n
	}
	return c
}

// toGrantRow 是带客户端登记信息的 McpGrant。
func toGrantRow(row repo.GrantRow) grantResp {
	g := toGrant(row.Grant)
	if row.ClientRegistration != "" {
		reg := row.ClientRegistration
		g.ClientRegistration = &reg
	}
	g.ClientHost = clientHost(row.ClientRegistration, row.ClientKey)
	return g
}

// clientHost：CIMD 客户端的身份由元数据文档所在域名担保，显示它；DCR 是自报的、访问密钥没有客户端，给空串。
func clientHost(registration, key string) string {
	if registration != "cimd" {
		return ""
	}
	u, err := url.Parse(key)
	if err != nil {
		return ""
	}
	return u.Host
}

type connPage struct {
	Items      []connResp `json:"items"`
	NextCursor *string    `json:"nextCursor"`
}

func page(rows []repo.GrantRow, limit int, names map[int64]string) connPage {
	out := connPage{Items: make([]connResp, 0, len(rows))}
	for _, row := range rows {
		out.Items = append(out.Items, toConn(row, names))
	}
	if len(rows) == limit && len(rows) > 0 {
		next := idStr(rows[len(rows)-1].ID)
		out.NextCursor = &next
	}
	return out
}

// filterFrom 解析筛选参数。⚠️ 空串当成没传（`?status=` 的意思是「不筛」，CD-65）；
// 枚举值不合法回 400 —— 静默当成没传的话，拼错一个字母列表就变成「全部」，而运营以为筛过了。
func filterFrom(q url.Values, limitDefault int) (repo.GrantFilter, error) {
	f := repo.GrantFilter{ClientName: q.Get("client"), Source: q.Get("source"), Status: q.Get("status"),
		Registration: q.Get("registration"), Limit: limitDefault}
	if f.Source != "" && f.Source != repo.SourceAccessKey && f.Source != repo.SourceOAuth {
		return f, httpx.Errf(errcode.InvalidArgument, "source 只能是 access_key 或 oauth")
	}
	if f.Status != "" && f.Status != repo.GrantActive && f.Status != repo.GrantRevoked {
		return f, httpx.Errf(errcode.InvalidArgument, "status 只能是 active 或 revoked")
	}
	if f.Registration != "" && f.Registration != "cimd" && f.Registration != "dcr" {
		return f, httpx.Errf(errcode.InvalidArgument, "registration 只能是 cimd 或 dcr")
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			f.Limit = n
		}
	}
	if v := q.Get("cursor"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			f.CursorID = n
		}
	}
	return f, nil
}

func optionalID(q url.Values, name string) (int64, error) {
	v := q.Get(name)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, httpx.Errf(errcode.InvalidArgument, name+" 须为合法 ID")
	}
	return n, nil
}

// decodeOptional 解析可空的请求体（DELETE 的理由）：没有 body 不是错误。
func decodeOptional(w http.ResponseWriter, r *http.Request, v any) error {
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(v)
	if err == nil || errors.Is(err, io.EOF) {
		return nil
	}
	return httpx.Err(errcode.InvalidJSON)
}

// ---------------------------------------------------------------- 工作区

// WorkspaceHandler 是工作区管理员的「MCP 连接」（/workspaces/current/mcp/*）。
type WorkspaceHandler struct {
	svc     *app.Manage
	userIDs func(ctx context.Context) (int64, bool)
}

func NewWorkspaceHandler(svc *app.Manage, userIDs func(ctx context.Context) (int64, bool)) *WorkspaceHandler {
	return &WorkspaceHandler{svc: svc, userIDs: userIDs}
}

// Mount 挂工作区级接口。
//
// ⚠️ 整组显式 `RequireAction(manage)`：GET 的默认闸只给 view，而「同事连了哪些外部客户端」
// 与「把整个工作区的 MCP 关掉」都是管理动作 —— **更严的要求必须自己标出来**（CLAUDE.md §5.18）。
// 工作区来自 X-Workspace-Id 的成员资格校验（wsCtx），不从路径或查询参数收。
func (h *WorkspaceHandler) Mount(r chi.Router, requireUser, wsCtx func(http.Handler) http.Handler) {
	r.Group(func(r chi.Router) {
		r.Use(requireUser, wsCtx, collabtransport.RequireAction(collabdomain.ActionManage))
		r.Get("/workspaces/current/mcp/grants", h.list)
		r.Delete("/workspaces/current/mcp/grants/{grantID}", h.revoke)
		r.Get("/workspaces/current/mcp/settings", h.getSetting)
		r.Put("/workspaces/current/mcp/settings", h.putSetting)
	})
}

func workspaceID(r *http.Request) int64 {
	wc, _ := collabtransport.WorkspaceFrom(r.Context())
	return wc.WorkspaceID
}

func (h *WorkspaceHandler) list(w http.ResponseWriter, r *http.Request) {
	f, err := filterFrom(r.URL.Query(), 50)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	ws := workspaceID(r)
	f.GrantScope = repo.GrantScope{WorkspaceID: ws}
	rows, err := h.svc.ListGrants(r.Context(), f)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, page(rows, f.Limit, h.svc.MemberNames(r.Context(), ws)))
}

func (h *WorkspaceHandler) revoke(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "grantID")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if err := decodeOptional(w, r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	uid, _ := h.userIDs(r.Context())
	// ⚠️ 范围是本工作区：别的工作区的 grant 与不存在回同一个 404。
	if err := h.svc.RevokeGrant(r.Context(), app.Revoker{Kind: app.RevokerWorkspace, ActorID: uid,
		WorkspaceID: workspaceID(r)}, id, req.Reason); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type wsSettingResp struct {
	Enabled         bool `json:"enabled"`
	PlatformEnabled bool `json:"platformEnabled"`
}

func (h *WorkspaceHandler) getSetting(w http.ResponseWriter, r *http.Request) {
	s, err := h.svc.WorkspaceSetting(r.Context(), workspaceID(r))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, wsSettingResp{Enabled: s.Enabled, PlatformEnabled: s.PlatformEnabled})
}

func (h *WorkspaceHandler) putSetting(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		httpx.Fail(w, r, httpx.Err(errcode.InvalidJSON))
		return
	}
	// ⚠️ 指针：`{}` 不能被读成「关闭」（零值 false 会悄悄把工作区关掉）。
	if req.Enabled == nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "enabled 必填"))
		return
	}
	uid, _ := h.userIDs(r.Context())
	s, err := h.svc.SetWorkspace(r.Context(), uid, workspaceID(r), *req.Enabled)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, wsSettingResp{Enabled: s.Enabled, PlatformEnabled: s.PlatformEnabled})
}

// ---------------------------------------------------------------- 平台

// AdminHandler 是管理端「MCP 连接」（/admin/mcp/*）。
type AdminHandler struct {
	svc      *app.Manage
	adminIDs func(r *http.Request) int64
}

func NewAdminHandler(svc *app.Manage, adminIDs func(r *http.Request) int64) *AdminHandler {
	return &AdminHandler{svc: svc, adminIDs: adminIDs}
}

// MountAdmin 挂管理端接口：读 `mcp:read`、写 `mcp:write`（挂法同 observability/transport/admin.go）。
//
// ⚠️ 读写分开挂：只读管理员要能**看**（连接与统计），而撤销与总开关一律 403 —— 前端禁用按钮只是体验。
func (h *AdminHandler) MountAdmin(r chi.Router, requireAdmin func(http.Handler) http.Handler,
	requirePerm func(string) func(http.Handler) http.Handler) {
	r.Group(func(r chi.Router) {
		r.Use(requireAdmin, requirePerm("mcp:read"))
		r.Get("/admin/mcp/grants", h.list)
		r.Get("/admin/mcp/settings", h.getSetting)
		r.Get("/admin/mcp/stats", h.stats)
	})
	r.Group(func(r chi.Router) {
		r.Use(requireAdmin, requirePerm("mcp:write"))
		r.Delete("/admin/mcp/grants/{grantID}", h.revoke)
		r.Put("/admin/mcp/settings", h.putSetting)
	})
}

func adminFilter(q url.Values) (repo.GrantFilter, error) {
	f, err := filterFrom(q, 30)
	if err != nil {
		return f, err
	}
	if f.UserID, err = optionalID(q, "userId"); err != nil {
		return f, err
	}
	if f.WorkspaceID, err = optionalID(q, "workspaceId"); err != nil {
		return f, err
	}
	return f, nil
}

func (h *AdminHandler) list(w http.ResponseWriter, r *http.Request) {
	f, err := adminFilter(r.URL.Query())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	rows, err := h.svc.ListGrants(r.Context(), f)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	// 平台视角不取昵称：跨工作区按人查名字要一个新的身份域端口，而运营排障用的是 ID（能直接粘到用户页去查）。
	httpx.JSON(w, http.StatusOK, page(rows, f.Limit, nil))
}

func (h *AdminHandler) revoke(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "grantID")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if err := decodeOptional(w, r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if err := h.svc.RevokeGrant(r.Context(), app.Revoker{Kind: app.RevokerAdmin, ActorID: h.adminIDs(r)},
		id, req.Reason); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type platformSettingResp struct {
	Enabled bool `json:"enabled"`
	// EnvEnabled 是 SEEAITV_MCP_ENABLED：为 false 时 /mcp 根本没挂，这里开着也没用（如实说出来）。
	EnvEnabled       bool    `json:"envEnabled"`
	Version          int     `json:"version"`
	UpdatedAt        *string `json:"updatedAt"`
	UpdatedByAdminID *string `json:"updatedByAdminId"`
}

func (h *AdminHandler) toSetting(s repo.PlatformSetting) platformSettingResp {
	out := platformSettingResp{Enabled: s.Enabled, EnvEnabled: h.svc.EnvEnabled(), Version: s.Version,
		UpdatedAt: ts(s.UpdatedAt)}
	if s.UpdatedByAdminID != nil {
		v := idStr(*s.UpdatedByAdminID)
		out.UpdatedByAdminID = &v
	}
	return out
}

func (h *AdminHandler) getSetting(w http.ResponseWriter, r *http.Request) {
	s, err := h.svc.PlatformSetting(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, h.toSetting(s))
}

func (h *AdminHandler) putSetting(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled *bool  `json:"enabled"`
		Reason  string `json:"reason"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		httpx.Fail(w, r, httpx.Err(errcode.InvalidJSON))
		return
	}
	if req.Enabled == nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "enabled 必填"))
		return
	}
	s, err := h.svc.SetPlatform(r.Context(), h.adminIDs(r), *req.Enabled, req.Reason)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, h.toSetting(s))
}

type clientShareResp struct {
	ClientName string `json:"clientName"`
	Kind       string `json:"kind"`
	Calls      int64  `json:"calls"`
	Errors     int64  `json:"errors"`
}

type statsResp struct {
	From         string            `json:"from"`
	To           string            `json:"to"`
	MaxSpanDays  int               `json:"maxSpanDays"`
	SpanAdjusted bool              `json:"spanAdjusted"`
	Calls        int64             `json:"calls"`
	Errors       int64             `json:"errors"`
	Denied       int64             `json:"denied"`
	RateLimited  int64             `json:"rateLimited"`
	NeedsConfirm int64             `json:"needsConfirm"`
	ErrorRate    float64           `json:"errorRate"`
	QuotesIssued int64             `json:"quotesIssued"`
	QuotesUsed   int64             `json:"quotesUsed"`
	ConfirmRate  float64           `json:"confirmRate"`
	ByClient     []clientShareResp `json:"byClient"`
}

func parseTime(v string) *time.Time {
	if v == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil // 解析失败当成没传（为一个格式问题把整张统计挡掉不值，CD-64）
	}
	return &t
}

func (h *AdminHandler) stats(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f, err := adminFilter(q)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	res, err := h.svc.Stats(r.Context(), app.StatsInput{Filter: f, From: parseTime(q.Get("from")), To: parseTime(q.Get("to"))})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := statsResp{
		From: res.From.UTC().Format(time.RFC3339), To: res.To.UTC().Format(time.RFC3339),
		MaxSpanDays: res.MaxSpanDays, SpanAdjusted: res.SpanAdjusted,
		Calls: res.Raw.Calls, Errors: res.Raw.Errors, Denied: res.Raw.Denied, RateLimited: res.Raw.RateLimited,
		NeedsConfirm: res.Raw.NeedsConfirm, ErrorRate: res.ErrorRate,
		QuotesIssued: res.Raw.QuotesIssued, QuotesUsed: res.Raw.QuotesUsed, ConfirmRate: res.ConfirmRate,
		ByClient: make([]clientShareResp, 0, len(res.ByClient)),
	}
	for _, c := range res.ByClient {
		out.ByClient = append(out.ByClient, clientShareResp{ClientName: c.Name, Kind: c.Kind, Calls: c.Calls, Errors: c.Errors})
	}
	httpx.JSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------- 平台总开关挂在 OAuth 端点上

// PlatformGate 让平台总开关也覆盖 OAuth 元数据 / 授权 / 令牌 / 注册与同意接口（MCPT-14）：
// 关着时一律 404（拒绝新授权；关掉就是不存在）。⚠️ `allow` 放行的路径不受影响 ——
// 撤销端点（/oauth/revoke）要一直可用：平台暂停期间用户想收回授权，不该被拦。
func PlatformGate(sw SwitchChecker, allow func(*http.Request) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if sw == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if (allow == nil || !allow(r)) && !sw.PlatformOpen(r.Context()) {
				httpx.Fail(w, r, app.PlatformDisabled())
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// 编译期确认 app.Switch 满足 SwitchChecker。
var _ SwitchChecker = app.Switch{}
