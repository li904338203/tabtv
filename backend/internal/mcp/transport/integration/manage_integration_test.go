//go:build integration

// 三级连接管理与两级开关（MCPT-14）的真库 + httptest 测试。
//
// ⚠️ 走真实路由与真实的协作域工作区中间件（RequireWorkspace + RequireAction(manage)）：
// 「editor 调工作区连接接口 403」「跨工作区撤销 404」都必须由路由闸与 SQL 里的范围条件给出，
// 绕开路由直接调服务的测试把闸删掉照样绿。
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	billingapp "github.com/li904338203/tabtv/backend/internal/billing/app"
	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	collabapp "github.com/li904338203/tabtv/backend/internal/collaboration/app"
	collabrepo "github.com/li904338203/tabtv/backend/internal/collaboration/repo"
	collabhttp "github.com/li904338203/tabtv/backend/internal/collaboration/transport"
	identityapp "github.com/li904338203/tabtv/backend/internal/identity/app"
	identityrepo "github.com/li904338203/tabtv/backend/internal/identity/repo"
	mcpapp "github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	mcprepo "github.com/li904338203/tabtv/backend/internal/mcp/repo"
	mcphttp "github.com/li904338203/tabtv/backend/internal/mcp/transport"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

const (
	adminID       = int64(7001)
	headerAdmin   = "X-Test-Admin"
	headerPerms   = "X-Test-Perms"
	oauthClientID = "https://client.example/oauth/meta.json"
)

// menv 复用 env 的密钥与 /mcp 装配，另挂工作区与管理端接口。
type menv struct {
	*env
	manage *mcpapp.Manage
	// ws2 是外人（outsider）自己的团队，用来验跨工作区。
	ws2 int64
}

type adminKey struct{}

func newManageEnv(t *testing.T) *menv {
	t.Helper()
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	next := gen.Next
	ctx := context.Background()
	e := &env{t: t, pool: pool, next: next}
	e.owner = mkUser(t, pool, next, "负责人", "13900000031")
	e.mate = mkUser(t, pool, next, "同事", "13900000032")
	e.outsider = mkUser(t, pool, next, "外人", "13900000033")
	cr := collabrepo.New(pool)
	w, err := cr.CreateTeam(ctx, [3]int64{next(), next(), next()}, "制作组", e.owner)
	if err != nil {
		t.Fatal(err)
	}
	e.ws, e.payer = w.ID, w.PayerAccountID
	if _, err := pool.Exec(ctx, `insert into workspace_member (id, workspace_id, user_id, role)
		values ($1,$2,$3,'editor')`, next(), e.ws, e.mate); err != nil {
		t.Fatal(err)
	}
	w2, err := cr.CreateTeam(ctx, [3]int64{next(), next(), next()}, "隔壁组", e.outsider)
	if err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	e.collab = collabapp.New(cr, next)
	identity := identityapp.New(identityrepo.New(pool), nil, nil, nil, next)
	billing := billingapp.New(billingrepo.New(pool, next))
	store := mcprepo.New(pool)
	e.store = store
	members := wiring.MCPMembers{Svc: e.collab}
	e.resolver = mcpapp.NewResolver(store, wiring.MCPUsers{Svc: identity}, members, billing, logger)
	auditLog := audit.New(pool, next, logger)
	keys := mcpapp.NewKeys(store, members, auditLog, next)
	manage := mcpapp.NewManage(store, wiring.MCPWorkspaces{Svc: e.collab}, auditLog, true, logger)

	requireUser := func(nx http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			uid, err := strconv.ParseInt(r.Header.Get("X-Test-User"), 10, 64)
			if err != nil {
				http.Error(w, "no user", http.StatusUnauthorized)
				return
			}
			nx.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, uid)))
		})
	}
	userIDs := func(ctx context.Context) (int64, bool) {
		uid, ok := ctx.Value(userKey{}).(int64)
		return uid, ok
	}
	// 测试用的管理端鉴权：X-Test-Admin 即管理员、X-Test-Perms 是逗号分隔的权限码
	// （真实的 RequirePermission 在 identity 域有自己的测试；这里要验的是**哪条路由挂在哪个权限位下**）。
	requireAdmin := func(nx http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get(headerAdmin) == "" {
				http.Error(w, "no admin", http.StatusUnauthorized)
				return
			}
			nx.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), adminKey{}, adminID)))
		})
	}
	requirePerm := func(code string) func(http.Handler) http.Handler {
		return func(nx http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for _, p := range strings.Split(r.Header.Get(headerPerms), ",") {
					if p == code {
						nx.ServeHTTP(w, r)
						return
					}
				}
				httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
			})
		}
	}
	collabH := collabhttp.NewHandler(e.collab, userIDs)
	keysH := mcphttp.NewKeysHandler(keys, userIDs)
	keysH.SetManage(manage)
	sw := manage.Switch()

	r := chi.NewRouter()
	r.Use(httpx.TraceID)
	r.Route("/api/v1", func(r chi.Router) {
		keysH.Mount(r, requireUser)
		mcphttp.NewWorkspaceHandler(manage, userIDs).Mount(r, requireUser, collabH.RequireWorkspace)
		mcphttp.NewAdminHandler(manage, func(r *http.Request) int64 {
			v, _ := r.Context().Value(adminKey{}).(int64)
			return v
		}).MountAdmin(r, requireAdmin, requirePerm)
	})
	mcphttp.NewHandler(mcphttp.Options{Registry: registry(), Auth: mcphttp.KeyAuth{Resolver: e.resolver},
		Switch: sw, Logger: logger}).Mount(r)
	// OAuth 端点的平台闸（cmd/api 里同样的挂法）：/oauth/revoke 例外。
	r.Group(func(r chi.Router) {
		r.Use(mcphttp.PlatformGate(sw, func(req *http.Request) bool { return req.URL.Path == "/oauth/revoke" }))
		ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
		r.Get("/.well-known/oauth-authorization-server", ok)
		r.Get("/oauth/authorize", ok)
		r.Post("/oauth/revoke", ok)
	})
	e.srv = httptest.NewServer(r)
	t.Cleanup(e.srv.Close)
	t.Cleanup(e.resolver.Flush)
	return &menv{env: e, manage: manage, ws2: w2.ID}
}

// wsAPI 以 uid 身份、带 X-Workspace-Id 调 /api/v1。
func (e *menv) wsAPI(method, path string, uid, ws int64, body any) (int, []byte) {
	e.t.Helper()
	return e.request(method, path, body, map[string]string{
		"X-Test-User": strconv.FormatInt(uid, 10), collabhttp.HeaderWorkspaceID: strconv.FormatInt(ws, 10)})
}

// adminAPI 以带 perms 的管理员身份调 /api/v1。
func (e *menv) adminAPI(method, path, perms string, body any) (int, []byte) {
	e.t.Helper()
	return e.request(method, path, body, map[string]string{headerAdmin: "1", headerPerms: perms})
}

func (e *menv) request(method, path string, body any, headers map[string]string) (int, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+"/api/v1"+path, rd)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (e *menv) rawGet(path string) int {
	e.t.Helper()
	resp, err := http.Get(e.srv.URL + path)
	if err != nil {
		e.t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// oauthGrant 建一个 CIMD 客户端 + 本工作区的 OAuth 授权 + 一对令牌，返回授权 ID。
func (e *menv) oauthGrant(uid int64) int64 {
	e.t.Helper()
	ctx := context.Background()
	cid := e.next()
	if _, err := e.pool.Exec(ctx, `insert into mcp_client (id, client_key, name, redirect_uris, registration)
		values ($1, $2, 'Claude Desktop', array['http://127.0.0.1:3/cb'], 'cimd')`, cid, oauthClientID); err != nil {
		e.t.Fatal(err)
	}
	g, _, err := e.store.UpsertOAuthGrant(ctx, mcprepo.Grant{ID: e.next(), UserID: uid, WorkspaceID: e.ws,
		ClientName: "Claude Desktop", Scopes: []string{"seeaitv:view"}, Toolsets: []string{"context"}}, cid)
	if err != nil {
		e.t.Fatal(err)
	}
	exp := time.Now().Add(time.Hour)
	toks := []mcprepo.NewToken{
		{ID: e.next(), Kind: mcprepo.TokenAccess, Hash: []byte("h-access-" + strconv.FormatInt(g.ID, 10)), Audience: "x", ExpiresAt: exp},
		{ID: e.next(), Kind: mcprepo.TokenRefresh, Hash: []byte("h-refresh-" + strconv.FormatInt(g.ID, 10)), Audience: "x", ExpiresAt: exp},
	}
	if err := e.store.IssueTokens(ctx, g.ID, e.next(), toks); err != nil {
		e.t.Fatal(err)
	}
	return g.ID
}

func (e *menv) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

type connPage struct {
	Items []struct {
		ID                 string  `json:"id"`
		UserID             string  `json:"userId"`
		UserName           *string `json:"userName"`
		Source             string  `json:"source"`
		ClientRegistration *string `json:"clientRegistration"`
		ClientHost         string  `json:"clientHost"`
		Status             string  `json:"status"`
		Calls              int64   `json:"calls"`
	} `json:"items"`
	NextCursor *string `json:"nextCursor"`
}

func decode[T any](t *testing.T, b []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("解析 %s：%v", b, err)
	}
	return v
}

// ---------------------------------------------------------------- 撤销

func TestRevokeGrant_RevokesAllTokens(t *testing.T) {
	e := newManageEnv(t)
	gid := e.oauthGrant(e.mate)
	if n := e.count(`select count(*) from mcp_token where grant_id = $1 and revoked_at is null`, gid); n != 2 {
		t.Fatalf("撤销前应有 2 枚有效令牌：%d", n)
	}
	// 工作区列表里看得到它：OAuth 来源、CIMD、域名、昵称。
	code, b := e.wsAPI(http.MethodGet, "/workspaces/current/mcp/grants", e.owner, e.ws, nil)
	if code != 200 {
		t.Fatalf("工作区列表：%d %s", code, b)
	}
	p := decode[connPage](t, b)
	if len(p.Items) != 1 || p.Items[0].Source != "oauth" || p.Items[0].ClientRegistration == nil ||
		*p.Items[0].ClientRegistration != "cimd" || p.Items[0].ClientHost != "client.example" ||
		p.Items[0].UserName == nil || *p.Items[0].UserName != "同事" {
		t.Fatalf("工作区列表内容：%s", b)
	}
	if code, b := e.wsAPI(http.MethodDelete, "/workspaces/current/mcp/grants/"+strconv.FormatInt(gid, 10),
		e.owner, e.ws, map[string]any{"reason": "离职"}); code != 204 {
		t.Fatalf("撤销：%d %s", code, b)
	}
	if n := e.count(`select count(*) from mcp_token where grant_id = $1 and revoked_at is null`, gid); n != 0 {
		t.Fatalf("撤销后令牌应全部作废，还剩 %d", n)
	}
	if n := e.count(`select count(*) from mcp_grant where id = $1 and status = 'revoked' and revoked_at is not null`, gid); n != 1 {
		t.Fatal("授权应为 revoked")
	}
	// 审计：发起方是工作区管理员，记下作废了几枚令牌与理由。
	var after, reason string
	if err := e.pool.QueryRow(context.Background(), `select after_value::text, coalesce(reason,'') from audit_log
		where action = 'mcp.grant.revoke' and object_id = $1`, gid).Scan(&after, &reason); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(after, `"workspace_admin"`) || !strings.Contains(after, `"tokensRevoked": 2`) || reason != "离职" {
		t.Fatalf("审计：%s / %s", after, reason)
	}
	// 用户自己的列表也补上了 OAuth 字段。
	_, ub := e.api(http.MethodGet, "/me/mcp/grants", e.mate, nil)
	if !strings.Contains(string(ub), `"clientRegistration":"cimd"`) || !strings.Contains(string(ub), `"clientHost":"client.example"`) {
		t.Fatalf("用户列表缺 OAuth 字段：%s", ub)
	}
}

func TestRevokeGrant_NextCall401(t *testing.T) {
	e := newManageEnv(t)
	// 工作区管理员撤销同事的密钥。
	c := e.createKey(e.mate, nil)
	if code, b := e.mcpCall(c.Plaintext); code != 200 {
		t.Fatalf("撤销前：%d %s", code, b)
	}
	if code, b := e.wsAPI(http.MethodDelete, "/workspaces/current/mcp/grants/"+c.GrantID, e.owner, e.ws, nil); code != 204 {
		t.Fatalf("工作区撤销：%d %s", code, b)
	}
	if code, b := e.mcpCall(c.Plaintext); code != 401 || !strings.Contains(b, "TOKEN_REVOKED") {
		t.Fatalf("工作区撤销后：%d %s", code, b)
	}
	// 平台管理员撤销：理由必填。
	c2 := e.createKey(e.mate, nil)
	path := "/admin/mcp/grants/" + c2.GrantID
	if code, b := e.adminAPI(http.MethodDelete, path, "mcp:read,mcp:write", nil); code != 400 {
		t.Fatalf("无理由撤销应 400：%d %s", code, b)
	}
	if code, b := e.adminAPI(http.MethodDelete, path, "mcp:read,mcp:write", map[string]any{"reason": "滥用"}); code != 204 {
		t.Fatalf("平台撤销：%d %s", code, b)
	}
	if code, _ := e.mcpCall(c2.Plaintext); code != 401 {
		t.Fatalf("平台撤销后：%d", code)
	}
	// 重复撤销 409，不静默成功。
	if code, _ := e.adminAPI(http.MethodDelete, path, "mcp:write", map[string]any{"reason": "再来"}); code != 409 {
		t.Fatalf("重复撤销：%d", code)
	}
	if n := e.count(`select count(*) from audit_log where action = 'mcp.grant.revoke' and actor_type = 'admin' and actor_id = $1`, adminID); n != 1 {
		t.Fatalf("平台撤销审计应 1 条：%d", n)
	}
}

func TestWorkspaceGrants_EditorForbidden(t *testing.T) {
	e := newManageEnv(t)
	c := e.createKey(e.mate, nil)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/workspaces/current/mcp/grants"},
		{http.MethodDelete, "/workspaces/current/mcp/grants/" + c.GrantID},
		{http.MethodGet, "/workspaces/current/mcp/settings"},
		{http.MethodPut, "/workspaces/current/mcp/settings"},
	} {
		code, b := e.wsAPI(tc.method, tc.path, e.mate, e.ws, map[string]any{"enabled": false})
		if code != 403 || !strings.Contains(string(b), "INSUFFICIENT_ROLE") {
			t.Fatalf("editor %s %s：%d %s", tc.method, tc.path, code, b)
		}
	}
	// 非成员拿着别人的工作区 ID：403 NOT_WORKSPACE_MEMBER。
	if code, _ := e.wsAPI(http.MethodGet, "/workspaces/current/mcp/grants", e.outsider, e.ws, nil); code != 403 {
		t.Fatalf("非成员：%d", code)
	}
	// 被拒之后连接还在。
	if code, _ := e.mcpCall(c.Plaintext); code != 200 {
		t.Fatalf("连接不该受影响：%d", code)
	}
}

func TestWorkspaceRevoke_OtherWorkspace404(t *testing.T) {
	e := newManageEnv(t)
	// 外人在自己的工作区建一把密钥。
	other := e.createKey(e.outsider, map[string]any{"workspaceId": strconv.FormatInt(e.ws2, 10)})
	// 本工作区 owner 带着本工作区的头去撤销它：404（与不存在同一个码），且它仍可用。
	code, b := e.wsAPI(http.MethodDelete, "/workspaces/current/mcp/grants/"+other.GrantID, e.owner, e.ws, nil)
	if code != 404 {
		t.Fatalf("跨工作区撤销应 404：%d %s", code, b)
	}
	if code, _ := e.wsAPI(http.MethodDelete, "/workspaces/current/mcp/grants/9999999", e.owner, e.ws, nil); code != 404 {
		t.Fatalf("不存在应 404：%d", code)
	}
	if code, _ := e.mcpCall(other.Plaintext); code != 200 {
		t.Fatalf("别人的连接不该被撤销：%d", code)
	}
	// 列表也只有本工作区的。
	e.createKey(e.mate, nil)
	_, lb := e.wsAPI(http.MethodGet, "/workspaces/current/mcp/grants", e.owner, e.ws, nil)
	if p := decode[connPage](t, lb); len(p.Items) != 1 || strings.Contains(string(lb), other.GrantID) {
		t.Fatalf("工作区列表越界：%s", lb)
	}
}

func TestWorkspaceSwitch_Off403_OnRestores(t *testing.T) {
	e := newManageEnv(t)
	c := e.createKey(e.mate, nil)
	if code, b := e.wsAPI(http.MethodPut, "/workspaces/current/mcp/settings", e.owner, e.ws, map[string]any{}); code != 400 {
		t.Fatalf("缺 enabled 应 400（不能把 {} 读成关闭）：%d %s", code, b)
	}
	code, b := e.wsAPI(http.MethodPut, "/workspaces/current/mcp/settings", e.owner, e.ws, map[string]any{"enabled": false})
	if code != 200 || !strings.Contains(string(b), `"enabled":false`) || !strings.Contains(string(b), `"platformEnabled":true`) {
		t.Fatalf("关闭：%d %s", code, b)
	}
	code, body := e.mcpCall(c.Plaintext)
	if code != 403 || !strings.Contains(body, "MCP_DISABLED") || !strings.Contains(body, "workspace_disabled") {
		t.Fatalf("工作区关闭后应 403 MCP_DISABLED：%d %s", code, body)
	}
	// 关闭不撤销授权；别的工作区不受影响。
	if n := e.count(`select count(*) from mcp_grant where id = $1 and status = 'active'`, mustID(t, c.GrantID)); n != 1 {
		t.Fatal("关闭不该撤销授权")
	}
	other := e.createKey(e.outsider, map[string]any{"workspaceId": strconv.FormatInt(e.ws2, 10)})
	if code, _ := e.mcpCall(other.Plaintext); code != 200 {
		t.Fatalf("别的工作区不受影响：%d", code)
	}
	_, gb := e.wsAPI(http.MethodGet, "/workspaces/current/mcp/settings", e.owner, e.ws, nil)
	if !strings.Contains(string(gb), `"enabled":false`) {
		t.Fatalf("读回：%s", gb)
	}
	// 重新打开：下一次调用即恢复。
	if code, _ := e.wsAPI(http.MethodPut, "/workspaces/current/mcp/settings", e.owner, e.ws, map[string]any{"enabled": true}); code != 200 {
		t.Fatalf("打开：%d", code)
	}
	if code, b := e.mcpCall(c.Plaintext); code != 200 {
		t.Fatalf("重新打开后应恢复：%d %s", code, b)
	}
	if n := e.count(`select count(*) from audit_log where action = 'mcp.workspace.switch' and workspace_id = $1`, e.ws); n != 2 {
		t.Fatalf("工作区开关审计应 2 条：%d", n)
	}
}

func TestPlatformSwitch_Off404(t *testing.T) {
	e := newManageEnv(t)
	c := e.createKey(e.mate, nil)
	code, b := e.adminAPI(http.MethodPut, "/admin/mcp/settings", "mcp:read,mcp:write",
		map[string]any{"enabled": false, "reason": "上游事故止损"})
	if code != 200 || !strings.Contains(string(b), `"enabled":false`) || !strings.Contains(string(b), `"envEnabled":true`) {
		t.Fatalf("关闭平台：%d %s", code, b)
	}
	// /mcp 404（关掉就是不存在），且与工作区关闭的原因不同。
	code, body := e.mcpCall(c.Plaintext)
	if code != 404 || strings.Contains(body, "MCP_DISABLED") {
		t.Fatalf("平台关闭后 /mcp 应 404：%d %s", code, body)
	}
	// 平台关着时工作区页如实显示。
	_, wb := e.wsAPI(http.MethodGet, "/workspaces/current/mcp/settings", e.owner, e.ws, nil)
	if !strings.Contains(string(wb), `"platformEnabled":false`) || !strings.Contains(string(wb), `"enabled":true`) {
		t.Fatalf("工作区页：%s", wb)
	}
	// OAuth 元数据与授权端点拒绝新授权；撤销端点照常可用。
	if st := e.rawGet("/.well-known/oauth-authorization-server"); st != 404 {
		t.Fatalf("元数据应 404：%d", st)
	}
	if st := e.rawGet("/oauth/authorize"); st != 404 {
		t.Fatalf("授权端点应 404：%d", st)
	}
	resp, err := http.Post(e.srv.URL+"/oauth/revoke", "application/x-www-form-urlencoded", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("撤销端点应照常可用：%d", resp.StatusCode)
	}
	// 关闭不撤销授权；重新打开即恢复。
	if n := e.count(`select count(*) from mcp_grant where status = 'active'`); n != 1 {
		t.Fatal("平台关闭不该撤销授权")
	}
	if code, _ := e.adminAPI(http.MethodPut, "/admin/mcp/settings", "mcp:write", map[string]any{"enabled": true}); code != 200 {
		t.Fatalf("打开平台：%d", code)
	}
	if code, b := e.mcpCall(c.Plaintext); code != 200 {
		t.Fatalf("重新打开后应恢复：%d %s", code, b)
	}
	if st := e.rawGet("/.well-known/oauth-authorization-server"); st != 200 {
		t.Fatalf("元数据应恢复：%d", st)
	}
	var reason string
	if err := e.pool.QueryRow(context.Background(), `select coalesce(reason,'') from audit_log
		where action = 'mcp.platform.switch' order by created_at, id limit 1`).Scan(&reason); err != nil || reason != "上游事故止损" {
		t.Fatalf("平台开关审计：%q %v", reason, err)
	}
}

func TestAdminGrants_ReadOnlyAdminCannotRevoke(t *testing.T) {
	e := newManageEnv(t)
	c := e.createKey(e.mate, nil)
	e.oauthGrant(e.owner)
	// 只读：看得到列表、开关与统计。
	code, b := e.adminAPI(http.MethodGet, "/admin/mcp/grants", "mcp:read", nil)
	if code != 200 {
		t.Fatalf("只读看列表：%d %s", code, b)
	}
	if p := decode[connPage](t, b); len(p.Items) != 2 || p.Items[0].UserName != nil {
		t.Fatalf("全站列表：%s", b)
	}
	for _, path := range []string{"/admin/mcp/settings", "/admin/mcp/stats"} {
		if code, b := e.adminAPI(http.MethodGet, path, "mcp:read", nil); code != 200 {
			t.Fatalf("只读 %s：%d %s", path, code, b)
		}
	}
	// 只读直打写接口：403，且什么都没改。
	if code, _ := e.adminAPI(http.MethodDelete, "/admin/mcp/grants/"+c.GrantID, "mcp:read", map[string]any{"reason": "x"}); code != 403 {
		t.Fatalf("只读撤销应 403：%d", code)
	}
	if code, _ := e.adminAPI(http.MethodPut, "/admin/mcp/settings", "mcp:read", map[string]any{"enabled": false}); code != 403 {
		t.Fatalf("只读改开关应 403：%d", code)
	}
	if code, _ := e.mcpCall(c.Plaintext); code != 200 {
		t.Fatalf("只读管理员什么都不该改动：%d", code)
	}
	// 没有 mcp:read 连看都不行。
	if code, _ := e.adminAPI(http.MethodGet, "/admin/mcp/grants", "system:read", nil); code != 403 {
		t.Fatalf("无 mcp:read：%d", code)
	}
	// 筛选：来源 + 空串丢弃语义（空串 = 不筛）+ 非法枚举 400。
	_, ob := e.adminAPI(http.MethodGet, "/admin/mcp/grants?source=oauth&status=", "mcp:read", nil)
	if p := decode[connPage](t, ob); len(p.Items) != 1 || p.Items[0].Source != "oauth" {
		t.Fatalf("按来源筛：%s", ob)
	}
	if code, _ := e.adminAPI(http.MethodGet, "/admin/mcp/grants?status=gone", "mcp:read", nil); code != 400 {
		t.Fatalf("非法状态应 400：%d", code)
	}
	_, kb := e.adminAPI(http.MethodGet, "/admin/mcp/grants?client=claude&userId="+strconv.FormatInt(e.mate, 10), "mcp:read", nil)
	if p := decode[connPage](t, kb); len(p.Items) != 1 || p.Items[0].ID != c.GrantID {
		t.Fatalf("按用户 + 客户端筛：%s", kb)
	}
	// 游标分页：limit=1 → 第二页拿到另一条，第三页没有。
	_, p1 := e.adminAPI(http.MethodGet, "/admin/mcp/grants?limit=1", "mcp:read", nil)
	pg1 := decode[connPage](t, p1)
	if len(pg1.Items) != 1 || pg1.NextCursor == nil {
		t.Fatalf("第一页：%s", p1)
	}
	_, p2 := e.adminAPI(http.MethodGet, "/admin/mcp/grants?limit=1&cursor="+*pg1.NextCursor, "mcp:read", nil)
	pg2 := decode[connPage](t, p2)
	if len(pg2.Items) != 1 || pg2.Items[0].ID == pg1.Items[0].ID {
		t.Fatalf("第二页：%s", p2)
	}
}

// ---------------------------------------------------------------- 统计

type statsBody struct {
	Calls        int64   `json:"calls"`
	Errors       int64   `json:"errors"`
	ErrorRate    float64 `json:"errorRate"`
	QuotesIssued int64   `json:"quotesIssued"`
	QuotesUsed   int64   `json:"quotesUsed"`
	ConfirmRate  float64 `json:"confirmRate"`
	SpanAdjusted bool    `json:"spanAdjusted"`
	ByClient     []struct {
		ClientName string `json:"clientName"`
		Kind       string `json:"kind"`
		Calls      int64  `json:"calls"`
		Errors     int64  `json:"errors"`
	} `json:"byClient"`
}

func (e *menv) logCall(grantID, uid int64, client, result string, at time.Time) {
	e.t.Helper()
	if err := e.store.InsertCallLog(context.Background(), mcprepo.CallLog{ID: e.next(), GrantID: grantID, UserID: uid,
		WorkspaceID: e.ws, ClientName: client, Tool: "canvas_read", Result: result, DurationMS: 3, CreatedAt: at}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *menv) quote(grantID int64, used bool, voided bool) {
	e.t.Helper()
	detail := `{}`
	if voided {
		detail = `{"voided":"price_changed"}`
	}
	if _, err := e.pool.Exec(context.Background(), `
		insert into mcp_quote (id, grant_id, tool, fingerprint, credits, payer_account_id, detail, expires_at, used_at)
		values ($1, $2, 'generate', decode(repeat('ab', 32), 'hex'), 10, $3, $4::jsonb, now() + interval '5 minutes',
		        case when $5 then now() end)`, e.next(), grantID, e.payer, detail, used); err != nil {
		e.t.Fatal(err)
	}
}

func TestMcpStats_ClientSumEqualsTotal(t *testing.T) {
	e := newManageEnv(t)
	a := mustID(t, e.createKey(e.mate, nil).GrantID)
	b := mustID(t, e.createKey(e.owner, map[string]any{"clientName": "Cursor"}).GrantID)
	now := time.Now()
	for i := 0; i < 5; i++ {
		e.logCall(a, e.mate, "Claude Desktop", "ok", now.Add(-time.Hour))
	}
	e.logCall(a, e.mate, "Claude Desktop", "error", now.Add(-time.Hour))
	e.logCall(b, e.owner, "Cursor", "denied", now.Add(-time.Hour))
	e.logCall(b, e.owner, "Cursor", "error", now.Add(-time.Hour))
	// 记录里没有客户端名（历史数据）：单列「未知」，不丢。
	if _, err := e.pool.Exec(context.Background(), `update mcp_call_log set client_name = '' where id in
		(select id from mcp_call_log where grant_id = $1 order by id limit 1)`, b); err != nil {
		t.Fatal(err)
	}
	// 范围之外的不算（审计保留 30 天，范围最多 30 天）。
	e.logCall(a, e.mate, "Claude Desktop", "ok", now.Add(-40*24*time.Hour))
	e.quote(a, true, false)
	e.quote(a, false, false)
	e.quote(a, true, true) // 作废的不算通过

	// ⚠️ to 显式给到一小时后：报价的 created_at 是容器里的 now()，colima 下与宿主机时钟差几秒很常见，
	// 缺省的 to=宿主机 now 会把刚写的报价排除在外。
	to := now.Add(time.Hour)
	code, body := e.adminAPI(http.MethodGet, "/admin/mcp/stats?from="+now.Add(-60*24*time.Hour).UTC().Format(time.RFC3339)+
		"&to="+to.UTC().Format(time.RFC3339), "mcp:read", nil)
	if code != 200 {
		t.Fatalf("统计：%d %s", code, body)
	}
	s := decode[statsBody](t, body)
	if s.Calls != 8 || s.Errors != 2 || s.ErrorRate != 0.25 || !s.SpanAdjusted {
		t.Fatalf("调用 / 错误 / 错误率 / 范围被夹：%s", body)
	}
	if s.QuotesIssued != 3 || s.QuotesUsed != 1 || s.ConfirmRate != 0.3333 {
		t.Fatalf("确认通过率：%s", body)
	}
	var sum, errSum int64
	unknown := false
	for _, c := range s.ByClient {
		sum += c.Calls
		errSum += c.Errors
		if c.Kind == domain.ClientKindUnknown {
			unknown = c.Calls == 1
		}
	}
	if sum != s.Calls || errSum != s.Errors || !unknown {
		t.Fatalf("各客户端之和 %d/%d 应等于总数 %d/%d，未知单列：%s", sum, errSum, s.Calls, s.Errors, body)
	}
	// 与列表同一套筛选：只看 Cursor 那个用户。
	_, ub := e.adminAPI(http.MethodGet, "/admin/mcp/stats?userId="+strconv.FormatInt(e.owner, 10), "mcp:read", nil)
	if us := decode[statsBody](t, ub); us.Calls != 2 || us.QuotesIssued != 0 || us.ConfirmRate != -1 {
		t.Fatalf("按用户筛的统计：%s", ub)
	}
	// 列表上的调用次数与统计同源。
	_, lb := e.adminAPI(http.MethodGet, "/admin/mcp/grants?userId="+strconv.FormatInt(e.mate, 10), "mcp:read", nil)
	if p := decode[connPage](t, lb); len(p.Items) != 1 || p.Items[0].Calls != 7 {
		t.Fatalf("列表调用次数（含 40 天前那条，它尚未被清理）：%s", lb)
	}
}

func TestMcpStats_NoData_MinusOne(t *testing.T) {
	e := newManageEnv(t)
	code, body := e.adminAPI(http.MethodGet, "/admin/mcp/stats", "mcp:read", nil)
	if code != 200 {
		t.Fatalf("统计：%d %s", code, body)
	}
	s := decode[statsBody](t, body)
	if s.Calls != 0 || s.ErrorRate != -1 || s.ConfirmRate != -1 || len(s.ByClient) != 0 || s.ByClient == nil {
		t.Fatalf("无数据应 -1 且分布为空数组：%s", body)
	}
}

func TestMcpSetting_SingleRow(t *testing.T) {
	e := newManageEnv(t)
	// 从没改过：按开启，version 0。
	_, b := e.adminAPI(http.MethodGet, "/admin/mcp/settings", "mcp:read", nil)
	if !strings.Contains(string(b), `"enabled":true`) || !strings.Contains(string(b), `"version":0`) {
		t.Fatalf("初始：%s", b)
	}
	for _, en := range []bool{false, true, false} {
		if code, b := e.adminAPI(http.MethodPut, "/admin/mcp/settings", "mcp:write", map[string]any{"enabled": en}); code != 200 {
			t.Fatalf("保存：%d %s", code, b)
		}
	}
	if n := e.count(`select count(*) from mcp_setting`); n != 1 {
		t.Fatalf("保存三次应仍是一行：%d", n)
	}
	_, b = e.adminAPI(http.MethodGet, "/admin/mcp/settings", "mcp:read", nil)
	if !strings.Contains(string(b), `"enabled":false`) || !strings.Contains(string(b), `"version":3`) ||
		!strings.Contains(string(b), `"updatedByAdminId":"7001"`) {
		t.Fatalf("读回最后一次：%s", b)
	}
}
