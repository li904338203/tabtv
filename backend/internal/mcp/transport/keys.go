package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// KeysHandler 是 /me/mcp/*（访问密钥与已连接的客户端，MCP-F-03）。挂在 /api/v1 内，
// **只要登录**（RequireUser）：密钥是个人的东西，工作区写在密钥自己身上，不靠 X-Workspace-Id。
type KeysHandler struct {
	svc *app.Keys
	// manage 是三级连接管理（MCPT-14）：已连接客户端的列表与撤销走它（撤销只有一个入口）。
	// 为 nil 时退回 Keys 的旧路径（测试装配）。
	manage  *app.Manage
	userIDs func(ctx context.Context) (int64, bool)
	// snippets 是客户端配置片段（MCPT-11F）。⚠️ 进程启动时渲染一次：入参只有公网地址与注册表，
	// 两者运行期都不变；片段里没有任何用户相关的内容（密钥是占位符），所以对所有人是同一份。
	snippets snippetsResp
}

// NewKeysHandler 需要一个从 context 取当前用户 ID 的函数（身份域提供；MCP 不认识 JWT 的结构）。
func NewKeysHandler(svc *app.Keys, userIDs func(ctx context.Context) (int64, bool)) *KeysHandler {
	return &KeysHandler{svc: svc, userIDs: userIDs}
}

// Mount 挂 /me/mcp/*。
func (h *KeysHandler) Mount(r chi.Router, requireUser func(http.Handler) http.Handler) {
	r.Route("/me/mcp", func(r chi.Router) {
		r.Use(requireUser)
		r.Get("/keys", h.listKeys)
		r.Post("/keys", h.createKey)
		r.Delete("/keys/{keyID}", h.revokeKey)
		r.Get("/grants", h.listGrants)
		r.Delete("/grants/{grantID}", h.revokeGrant)
		r.Patch("/grants/{grantID}", h.updateGrant) // MCPT-13：切换「花钱需在 seeAitv 页面确认」
		r.Get("/client-snippets", h.clientSnippets)
	})
}

// SetManage 接上三级连接管理（MCPT-14）。
func (h *KeysHandler) SetManage(m *app.Manage) { h.manage = m }

// SetClientSnippets 设定配置片段的入参（公网地址 + 注册表里实际有的工具集）。
// 注册表在密钥服务之后才建出来，所以不放进构造函数。
func (h *KeysHandler) SetClientSnippets(publicURL string, toolsets []app.ToolsetInfo) {
	h.snippets = toSnippets(app.ClientSnippets(publicURL, toolsets))
}

// snippetsResp 对应 OpenAPI 的 McpClientSnippets。
type snippetsResp struct {
	MCPURL         string            `json:"mcpUrl"`
	URLConfigured  bool              `json:"mcpUrlConfigured"`
	KeyPlaceholder string            `json:"keyPlaceholder"`
	Toolsets       []toolsetInfoResp `json:"toolsets"`
	Items          []snippetResp     `json:"items"`
}

type toolsetInfoResp struct {
	Name      string `json:"name"`
	DefaultOn bool   `json:"defaultOn"`
	Available bool   `json:"available"`
}

type snippetResp struct {
	Client   string           `json:"client"`
	Label    string           `json:"label"`
	Format   string           `json:"format"`
	Location string           `json:"location"`
	Content  string           `json:"content"`
	Note     string           `json:"note"`
	Modes    []clientModeResp `json:"modes"`
}

// clientModeResp 对应 OpenAPI 的 McpClientMode。
type clientModeResp struct {
	Mode        string `json:"mode"`
	Supported   bool   `json:"supported"`
	Format      string `json:"format,omitempty"`
	Location    string `json:"location"`
	Content     string `json:"content"`
	AgentPrompt string `json:"agentPrompt"`
	Note        string `json:"note"`
}

func toSnippets(r app.ClientSnippetsResult) snippetsResp {
	out := snippetsResp{
		MCPURL: r.MCPURL, URLConfigured: r.URLConfigured, KeyPlaceholder: r.KeyPlaceholder,
		Toolsets: make([]toolsetInfoResp, 0, len(r.Toolsets)), Items: make([]snippetResp, 0, len(r.Items)),
	}
	for _, t := range r.Toolsets {
		out.Toolsets = append(out.Toolsets, toolsetInfoResp{Name: t.Name, DefaultOn: t.DefaultOn, Available: t.Available})
	}
	for _, it := range r.Items {
		modes := make([]clientModeResp, 0, len(it.Modes))
		for _, m := range it.Modes {
			modes = append(modes, clientModeResp{
				Mode: m.Mode, Supported: m.Supported, Format: m.Format, Location: m.Location,
				Content: m.Content, AgentPrompt: m.AgentPrompt, Note: m.Note,
			})
		}
		out.Items = append(out.Items, snippetResp{
			Client: it.Client, Label: it.Label, Format: it.Format, Location: it.Location, Content: it.Content, Note: it.Note,
			Modes: modes,
		})
	}
	return out
}

func (h *KeysHandler) clientSnippets(w http.ResponseWriter, _ *http.Request) {
	res := h.snippets
	if res.Items == nil { // 没调 SetClientSnippets（测试或装配遗漏）：按「地址未配置、工具集未知」给，不回 500
		res = toSnippets(app.ClientSnippets("", app.ToolsetAvailability(nil)))
	}
	httpx.JSON(w, http.StatusOK, res)
}

// keyResp 对应 OpenAPI 的 McpAccessKey。⚠️ 没有明文、没有哈希。
type keyResp struct {
	ID                 string   `json:"id"`
	GrantID            string   `json:"grantId"`
	WorkspaceID        string   `json:"workspaceId"`
	ClientName         string   `json:"clientName"`
	Prefix             string   `json:"prefix"`
	Scopes             []string `json:"scopes"`
	Toolsets           []string `json:"toolsets"`
	RequirePageConfirm bool     `json:"requirePageConfirm"`
	Status             string   `json:"status"`
	ExpiresAt          *string  `json:"expiresAt"`
	LastUsedAt         *string  `json:"lastUsedAt"`
	CreatedAt          string   `json:"createdAt"`
	RevokedAt          *string  `json:"revokedAt"`
}

type grantResp struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspaceId"`
	ClientName  string `json:"clientName"`
	Source      string `json:"source"`
	// ClientRegistration / ClientHost：OAuth 客户端的登记方式与（CIMD 的）来源域名，MCPT-14。
	ClientRegistration *string  `json:"clientRegistration"`
	ClientHost         string   `json:"clientHost"`
	KeyPrefix          *string  `json:"keyPrefix"`
	Scopes             []string `json:"scopes"`
	Toolsets           []string `json:"toolsets"`
	RequirePageConfirm bool     `json:"requirePageConfirm"`
	Status             string   `json:"status"`
	LastUsedAt         *string  `json:"lastUsedAt"`
	CreatedAt          string   `json:"createdAt"`
	RevokedAt          *string  `json:"revokedAt"`
}

func (h *KeysHandler) toKey(k repo.AccessKey) keyResp {
	return keyResp{
		ID: idStr(k.ID), GrantID: idStr(k.GrantID), WorkspaceID: idStr(k.Grant.WorkspaceID),
		ClientName: k.Grant.ClientName, Prefix: k.Prefix, Scopes: nonNil(k.Grant.Scopes),
		Toolsets: nonNil(k.Grant.Toolsets), RequirePageConfirm: k.Grant.RequirePageConfirm,
		Status: app.KeyStatus(k, h.svc.Now()), ExpiresAt: ts(k.ExpiresAt), LastUsedAt: ts(k.Grant.LastUsedAt),
		CreatedAt: k.CreatedAt.UTC().Format(time.RFC3339), RevokedAt: ts(k.Grant.RevokedAt),
	}
}

func toGrant(g repo.Grant) grantResp {
	var prefix *string
	if g.KeyPrefix != "" {
		p := g.KeyPrefix
		prefix = &p
	}
	return grantResp{
		ID: idStr(g.ID), WorkspaceID: idStr(g.WorkspaceID), ClientName: g.ClientName, Source: g.Source,
		KeyPrefix: prefix, Scopes: nonNil(g.Scopes), Toolsets: nonNil(g.Toolsets),
		RequirePageConfirm: g.RequirePageConfirm, Status: g.Status, LastUsedAt: ts(g.LastUsedAt),
		CreatedAt: g.CreatedAt.UTC().Format(time.RFC3339), RevokedAt: ts(g.RevokedAt),
	}
}

func (h *KeysHandler) listKeys(w http.ResponseWriter, r *http.Request) {
	uid, _ := h.userIDs(r.Context())
	ks, err := h.svc.ListKeys(r.Context(), uid)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := make([]keyResp, 0, len(ks))
	for _, k := range ks {
		out = append(out, h.toKey(k))
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *KeysHandler) createKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WorkspaceID        string   `json:"workspaceId"`
		ClientName         string   `json:"clientName"`
		Scopes             []string `json:"scopes"`
		Toolsets           []string `json:"toolsets"`
		ExpiresInDays      *int     `json:"expiresInDays"`
		NeverExpires       bool     `json:"neverExpires"`
		RequirePageConfirm bool     `json:"requirePageConfirm"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		httpx.Fail(w, r, httpx.Err(errcode.InvalidJSON))
		return
	}
	ws, err := strconv.ParseInt(req.WorkspaceID, 10, 64)
	if err != nil || ws <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "workspaceId 必填且须为合法 ID"))
		return
	}
	uid, _ := h.userIDs(r.Context())
	res, err := h.svc.CreateKey(r.Context(), uid, app.CreateKeyInput{
		WorkspaceID: ws, ClientName: req.ClientName, Scopes: req.Scopes, Toolsets: req.Toolsets,
		ExpiresInDays: req.ExpiresInDays, NeverExpires: req.NeverExpires, RequirePageConfirm: req.RequirePageConfirm,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	// ⚠️ 明文只在这里返回一次（库里只有哈希与前缀）。
	httpx.JSON(w, http.StatusCreated, struct {
		keyResp
		Plaintext string `json:"plaintext"`
	}{h.toKey(res.Key), res.Plaintext})
}

func (h *KeysHandler) revokeKey(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "keyID")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	uid, _ := h.userIDs(r.Context())
	if err := h.svc.RevokeKey(r.Context(), uid, id); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *KeysHandler) listGrants(w http.ResponseWriter, r *http.Request) {
	uid, _ := h.userIDs(r.Context())
	if h.manage != nil {
		rows, err := h.manage.ListGrants(r.Context(), repo.GrantFilter{GrantScope: repo.GrantScope{UserID: uid}, Limit: 200})
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		out := make([]grantResp, 0, len(rows))
		for _, row := range rows {
			out = append(out, toGrantRow(row))
		}
		httpx.JSON(w, http.StatusOK, out)
		return
	}
	gs, err := h.svc.ListGrants(r.Context(), uid)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := make([]grantResp, 0, len(gs))
	for _, g := range gs {
		out = append(out, toGrant(g))
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *KeysHandler) revokeGrant(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "grantID")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	uid, _ := h.userIDs(r.Context())
	if h.manage != nil {
		if err := h.manage.RevokeGrant(r.Context(), app.Revoker{Kind: app.RevokerUser, ActorID: uid}, id, ""); err != nil {
			httpx.Fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := h.svc.RevokeGrant(r.Context(), uid, id); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func pathID(r *http.Request, name string) (int64, error) {
	id, err := strconv.ParseInt(chi.URLParam(r, name), 10, 64)
	if err != nil || id <= 0 {
		return 0, httpx.Errf(errcode.InvalidArgument, "路径参数 "+name+" 非法")
	}
	return id, nil
}

func idStr(v int64) string { return strconv.FormatInt(v, 10) }

func ts(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
