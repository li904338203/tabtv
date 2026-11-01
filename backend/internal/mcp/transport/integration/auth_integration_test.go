//go:build integration

// 对外 MCP 的访问密钥、凭据与 Principal（MCPT-03）的真库 + httptest 测试。
//
// ⚠️ 走真实的路由（/api/v1/me/mcp/* 与 /mcp）、真实的成员表与用户表：
// 「移除成员 / 降级 / 封禁后下一次调用即失效」必须由每次请求的实时查询给出，
// 绕开路由直接调 Resolver 的测试把凭据中间件整条删掉照样绿。
package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	billingapp "github.com/li904338203/tabtv/backend/internal/billing/app"
	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	collabapp "github.com/li904338203/tabtv/backend/internal/collaboration/app"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	collabrepo "github.com/li904338203/tabtv/backend/internal/collaboration/repo"
	identityapp "github.com/li904338203/tabtv/backend/internal/identity/app"
	identityrepo "github.com/li904338203/tabtv/backend/internal/identity/repo"
	"github.com/li904338203/tabtv/backend/internal/identity/token"
	mcpapp "github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	mcprepo "github.com/li904338203/tabtv/backend/internal/mcp/repo"
	mcphttp "github.com/li904338203/tabtv/backend/internal/mcp/transport"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

// ---------------------------------------------------------------- 测试注册表

type in struct {
	ProjectID string `json:"projectId"`
}
type out struct {
	OK string `json:"ok"`
}

// registry：view / edit / generate 三类假工具（只验权限，不碰业务）。
func registry() *mcpapp.Registry {
	r := mcpapp.NewRegistry()
	h := func(context.Context, string, in) (out, string, error) { return out{OK: "1"}, "ok", nil }
	r.Register(mcpapp.Define(mcpapp.Tool{Name: "canvas_read", Toolset: domain.ToolsetCanvas, Description: "读（测试）",
		Methods: []mcpapp.Method{{Action: collab.ActionView}}}, h))
	r.Register(mcpapp.Define(mcpapp.Tool{Name: "canvas_apply", Toolset: domain.ToolsetCanvas, Description: "写（测试）",
		Methods: []mcpapp.Method{{Action: collab.ActionEdit, Write: true}}}, h))
	r.Register(mcpapp.Define(mcpapp.Tool{Name: "generate", Toolset: domain.ToolsetGeneration, Description: "花钱（测试）",
		Methods: []mcpapp.Method{{Action: collab.ActionGenerate, Paid: true, Write: true}}}, h))
	return r
}

// ---------------------------------------------------------------- 环境

type userKey struct{}

type env struct {
	t        *testing.T
	pool     *pgxpool.Pool
	next     func() int64
	collab   *collabapp.Service
	resolver *mcpapp.Resolver
	store    *mcprepo.Repo
	calls    *mcpapp.CallLogger // 只在 withCalls 时有（MCPT-06）
	srv      *httptest.Server
	ws       int64
	payer    int64
	owner    int64
	mate     int64
	outsider int64
}

func mkUser(t *testing.T, pool *pgxpool.Pool, next func() int64, nickname, phone string) int64 {
	t.Helper()
	uid := next()
	if _, err := pool.Exec(context.Background(), `
		insert into app_user (id, phone, nickname, password_hash) values ($1,$2,$3,'x')`,
		uid, phone, nickname); err != nil {
		t.Fatal(err)
	}
	return uid
}

// newEnv 建环境；opts 可改 /mcp 的装配（MCPT-06 的限流与审计用例注入 Limiter / Calls）。
func newEnv(t *testing.T, opts ...func(*env, *mcphttp.Options)) *env {
	t.Helper()
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	next := gen.Next
	ctx := context.Background()
	e := &env{t: t, pool: pool, next: next}
	e.owner = mkUser(t, pool, next, "负责人", "13900000011")
	e.mate = mkUser(t, pool, next, "同事", "13900000012")
	e.outsider = mkUser(t, pool, next, "外人", "13900000013")
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

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	e.collab = collabapp.New(cr, next)
	identity := identityapp.New(identityrepo.New(pool), nil, nil, nil, next)
	billing := billingapp.New(billingrepo.New(pool, next))
	store := mcprepo.New(pool)
	e.store = store
	members := wiring.MCPMembers{Svc: e.collab}
	e.resolver = mcpapp.NewResolver(store, wiring.MCPUsers{Svc: identity}, members, billing, logger)
	keys := mcpapp.NewKeys(store, members, audit.New(pool, next, logger), next)

	r := chi.NewRouter()
	r.Use(httpx.TraceID)
	// 测试用的登录中间件：X-Test-User 即当前用户（真实的 RequireUser 在 identity 域有自己的测试）。
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
	r.Route("/api/v1", func(r chi.Router) {
		mcphttp.NewKeysHandler(keys, userIDs).Mount(r, requireUser)
	})
	o := mcphttp.Options{Registry: registry(), Auth: mcphttp.KeyAuth{Resolver: e.resolver}, Logger: logger}
	for _, f := range opts {
		f(e, &o)
	}
	mcphttp.NewHandler(o).Mount(r)
	e.srv = httptest.NewServer(r)
	t.Cleanup(e.srv.Close)
	t.Cleanup(e.resolver.Flush)
	return e
}

// api 以 uid 身份调 /api/v1。
func (e *env) api(method, path string, uid int64, body any) (int, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+"/api/v1"+path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-User", strconv.FormatInt(uid, 10))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

type created struct {
	ID        string   `json:"id"`
	GrantID   string   `json:"grantId"`
	Prefix    string   `json:"prefix"`
	Plaintext string   `json:"plaintext"`
	Scopes    []string `json:"scopes"`
	Toolsets  []string `json:"toolsets"`
	Status    string   `json:"status"`
	ExpiresAt *string  `json:"expiresAt"`
}

func (e *env) createKey(uid int64, extra map[string]any) created {
	e.t.Helper()
	body := map[string]any{"workspaceId": strconv.FormatInt(e.ws, 10), "clientName": "Claude Desktop",
		"scopes": []string{"seeaitv:edit", "seeaitv:generate"}}
	for k, v := range extra {
		body[k] = v
	}
	code, b := e.api(http.MethodPost, "/me/mcp/keys", uid, body)
	if code != http.StatusCreated {
		e.t.Fatalf("建密钥：%d %s", code, b)
	}
	var c created
	if err := json.Unmarshal(b, &c); err != nil {
		e.t.Fatal(err)
	}
	return c
}

// mcpCall 用 bearer 发一次 MCP initialize，返回状态码与响应体（200 = 凭据通过）。
func (e *env) mcpCall(bearer string) (int, string) {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"raw","version":"0"}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func connect(t *testing.T, url, version, key string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rt := bearerRT{key}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "mcpt03-it", Version: "0"}, nil).Connect(ctx,
		&mcp.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: rt}, MaxRetries: -1},
		&mcp.ClientSessionOptions{ProtocolVersion: version})
	if err != nil {
		t.Fatalf("%s 连接：%v", version, err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

type bearerRT struct{ key string }

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.key)
	return http.DefaultTransport.RoundTrip(r)
}

func toolNames(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	return names
}

// ---------------------------------------------------------------- 用例

func TestMCPKey_明文只在创建时返回一次(t *testing.T) {
	e := newEnv(t)
	c := e.createKey(e.mate, nil)
	if _, err := domain.ParseKey(c.Plaintext); err != nil {
		t.Fatalf("明文形态：%q", c.Plaintext)
	}
	if !strings.HasPrefix(c.Plaintext, domain.KeyPrefix+c.Prefix) || len(c.Prefix) != 8 {
		t.Fatalf("前缀：%q / %q", c.Prefix, c.Plaintext)
	}
	// scope 自动补 view；工具集为空取默认；默认 90 天。
	if !slices.Equal(c.Scopes, []string{"seeaitv:view", "seeaitv:edit", "seeaitv:generate"}) ||
		!slices.Equal(c.Toolsets, domain.DefaultToolsets()) || c.Status != "active" || c.ExpiresAt == nil {
		t.Fatalf("创建结果：%+v", c)
	}
	exp, _ := time.Parse(time.RFC3339, *c.ExpiresAt)
	if d := time.Until(exp); d < 89*24*time.Hour || d > 91*24*time.Hour {
		t.Fatalf("默认有效期：%s", *c.ExpiresAt)
	}
	// 之后任何接口都取不到明文。
	body := c.Plaintext[len(domain.KeyPrefix):]
	for _, p := range []string{"/me/mcp/keys", "/me/mcp/grants"} {
		code, b := e.api(http.MethodGet, p, e.mate, nil)
		if code != 200 || strings.Contains(string(b), body) || strings.Contains(string(b), "plaintext") {
			t.Fatalf("%s 泄漏明文：%d %s", p, code, b)
		}
	}
	code, b := e.api(http.MethodGet, "/me/mcp/keys", e.mate, nil)
	var list []map[string]any
	_ = json.Unmarshal(b, &list)
	if code != 200 || len(list) != 1 || list[0]["prefix"] != c.Prefix || list[0]["id"] != c.ID {
		t.Fatalf("列表：%s", b)
	}
	// 别人的列表里看不到。
	if _, b := e.api(http.MethodGet, "/me/mcp/keys", e.owner, nil); string(bytes.TrimSpace(b)) != "[]" {
		t.Fatalf("别人的列表：%s", b)
	}
	// 永久密钥：expiresAt 为空。
	if p := e.createKey(e.mate, map[string]any{"neverExpires": true}); p.ExpiresAt != nil {
		t.Fatalf("永久密钥：%+v", p)
	}
	// 参数校验：clientName 必填、未知 scope、两种有效期同时给。
	for _, bad := range []map[string]any{
		{"workspaceId": strconv.FormatInt(e.ws, 10), "clientName": " ", "scopes": []string{"seeaitv:view"}},
		{"workspaceId": strconv.FormatInt(e.ws, 10), "clientName": "x", "scopes": []string{"seeaitv:root"}},
		{"workspaceId": strconv.FormatInt(e.ws, 10), "clientName": "x", "scopes": []string{"seeaitv:view"},
			"neverExpires": true, "expiresInDays": 3},
		{"workspaceId": strconv.FormatInt(e.ws, 10), "clientName": "x", "scopes": []string{"seeaitv:view"},
			"toolsets": []string{"writing"}},
	} {
		if code, b := e.api(http.MethodPost, "/me/mcp/keys", e.mate, bad); code != 400 {
			t.Fatalf("应 400：%v → %d %s", bad, code, b)
		}
	}
}

func TestMCPKey_库里只存哈希与前缀(t *testing.T) {
	e := newEnv(t)
	c := e.createKey(e.mate, nil)
	var hash []byte
	var prefix string
	if err := e.pool.QueryRow(context.Background(),
		`select key_hash, prefix from mcp_access_key where id = $1`, mustID(t, c.ID)).Scan(&hash, &prefix); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte(c.Plaintext))
	if !bytes.Equal(hash, want[:]) || prefix != c.Prefix {
		t.Fatalf("哈希 / 前缀不符：%x %q", hash, prefix)
	}
	// 两张表的任何一列（转成文本）都不含明文主体。
	body := c.Plaintext[len(domain.KeyPrefix):]
	for _, q := range []string{
		`select coalesce(string_agg(row_to_json(k)::text, ''), '') from mcp_access_key k`,
		`select coalesce(string_agg(row_to_json(g)::text, ''), '') from mcp_grant g`,
	} {
		var dump string
		if err := e.pool.QueryRow(context.Background(), q).Scan(&dump); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(dump, body) {
			t.Fatalf("库里出现明文：%s", q)
		}
	}
}

func TestMCPAuth_撤销密钥后下一次调用401(t *testing.T) {
	e := newEnv(t)
	c := e.createKey(e.mate, nil)
	if code, b := e.mcpCall(c.Plaintext); code != 200 {
		t.Fatalf("撤销前：%d %s", code, b)
	}
	// 别人撤销不了（404，不暴露是否存在）。
	if code, _ := e.api(http.MethodDelete, "/me/mcp/keys/"+c.ID, e.owner, nil); code != 404 {
		t.Fatalf("别人撤销：%d", code)
	}
	if code, b := e.api(http.MethodDelete, "/me/mcp/keys/"+c.ID, e.mate, nil); code != 204 {
		t.Fatalf("撤销：%d %s", code, b)
	}
	code, b := e.mcpCall(c.Plaintext)
	if code != 401 || !strings.Contains(b, "TOKEN_REVOKED") {
		t.Fatalf("撤销后：%d %s", code, b)
	}
	// 重复撤销：409 INVALID_STATE_TRANSITION，不静默成功。
	if code, b := e.api(http.MethodDelete, "/me/mcp/keys/"+c.ID, e.mate, nil); code != 409 || !strings.Contains(string(b), "INVALID_STATE_TRANSITION") {
		t.Fatalf("重复撤销：%d %s", code, b)
	}
	// 撤销授权（另一把）：同样下一次 401；状态在列表里可见。
	c2 := e.createKey(e.mate, nil)
	if code, b := e.api(http.MethodDelete, "/me/mcp/grants/"+c2.GrantID, e.mate, nil); code != 204 {
		t.Fatalf("撤销授权：%d %s", code, b)
	}
	if code, _ := e.mcpCall(c2.Plaintext); code != 401 {
		t.Fatalf("撤销授权后：%d", code)
	}
	if code, _ := e.api(http.MethodDelete, "/me/mcp/grants/"+c2.GrantID, e.mate, nil); code != 409 {
		t.Fatalf("重复撤销授权：%d", code)
	}
	_, lb := e.api(http.MethodGet, "/me/mcp/keys", e.mate, nil)
	if strings.Count(string(lb), `"status":"revoked"`) != 2 {
		t.Fatalf("列表状态：%s", lb)
	}
	_, gb := e.api(http.MethodGet, "/me/mcp/grants", e.mate, nil)
	if strings.Count(string(gb), `"status":"revoked"`) != 2 || !strings.Contains(string(gb), `"source":"access_key"`) ||
		!strings.Contains(string(gb), `"keyPrefix":"`+c.Prefix+`"`) {
		t.Fatalf("授权列表：%s", gb)
	}
}

func TestMCPAuth_过期密钥401(t *testing.T) {
	e := newEnv(t)
	c := e.createKey(e.mate, map[string]any{"expiresInDays": 1})
	if code, _ := e.mcpCall(c.Plaintext); code != 200 {
		t.Fatalf("过期前：%d", code)
	}
	// ⚠️ 留足余量：过期按 API 进程的时钟判，而这里的 now() 是容器里的时钟，colima 下两者差几秒很常见。
	if _, err := e.pool.Exec(context.Background(),
		`update mcp_access_key set expires_at = now() - interval '1 hour' where id = $1`, mustID(t, c.ID)); err != nil {
		t.Fatal(err)
	}
	code, b := e.mcpCall(c.Plaintext)
	if code != 401 || !strings.Contains(b, "TOKEN_EXPIRED") {
		t.Fatalf("过期后：%d %s", code, b)
	}
	// 过期是算出来的：列表里显示 expired（grant 本身仍是 active）。
	_, lb := e.api(http.MethodGet, "/me/mcp/keys", e.mate, nil)
	if !strings.Contains(string(lb), `"status":"expired"`) {
		t.Fatalf("列表：%s", lb)
	}
}

func TestMCPAuth_移除成员后下一次调用403(t *testing.T) {
	e := newEnv(t)
	c := e.createKey(e.mate, nil)
	if code, _ := e.mcpCall(c.Plaintext); code != 200 {
		t.Fatalf("移除前：%d", code)
	}
	if err := e.collab.RemoveMember(context.Background(), e.ws, e.mate, collab.RoleOwner, e.owner); err != nil {
		t.Fatal(err)
	}
	code, b := e.mcpCall(c.Plaintext)
	if code != 403 || !strings.Contains(b, "NOT_WORKSPACE_MEMBER") {
		t.Fatalf("移除后：%d %s", code, b)
	}
}

func TestMCPAuth_降级为viewer后写方法不可见(t *testing.T) {
	e := newEnv(t)
	c := e.createKey(e.mate, map[string]any{"scopes": []string{"seeaitv:view", "seeaitv:edit", "seeaitv:generate"}})
	url := e.srv.URL + "/mcp"
	// 2025 会话在降级前建立：同一个会话里下一次列工具就要变（SDK 用的是会话初始 ctx，见 carry.go）。
	s25 := connect(t, url, "2025-11-25", c.Plaintext)
	s26 := connect(t, url, "2026-07-28", c.Plaintext)
	for _, cs := range []*mcp.ClientSession{s25, s26} {
		if got := toolNames(t, cs); !slices.Equal(got, []string{"canvas_apply", "canvas_read", "generate"}) &&
			!slices.Equal(sorted(got), []string{"canvas_apply", "canvas_read", "generate"}) {
			t.Fatalf("editor：%v", got)
		}
	}
	if err := e.collab.SetRole(context.Background(), e.ws, e.mate, collab.RoleOwner, e.owner, collab.RoleViewer); err != nil {
		t.Fatal(err)
	}
	for name, cs := range map[string]*mcp.ClientSession{"2025": s25, "2026": s26} {
		if got := toolNames(t, cs); !slices.Equal(got, []string{"canvas_read"}) {
			t.Fatalf("%s 降级后：%v", name, got)
		}
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "canvas_apply",
			Arguments: map[string]any{"projectId": "1"}})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(res.Content)
		if !res.IsError || !strings.Contains(string(b), "MCP_INSUFFICIENT_SCOPE") || !strings.Contains(string(b), "roleInsufficient") {
			t.Fatalf("%s 降级后调写工具：%s", name, b)
		}
	}
}

func sorted(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

func TestMCPAuth_封禁账号后403(t *testing.T) {
	e := newEnv(t)
	c := e.createKey(e.mate, nil)
	if _, err := e.pool.Exec(context.Background(), `update app_user set status = 'banned' where id = $1`, e.mate); err != nil {
		t.Fatal(err)
	}
	code, b := e.mcpCall(c.Plaintext)
	if code != 403 || !strings.Contains(b, "ACCOUNT_BANNED") {
		t.Fatalf("封禁后：%d %s", code, b)
	}
}

func TestMCPAuth_用户端JWT不能当MCP凭据(t *testing.T) {
	e := newEnv(t)
	tokens, err := token.New(token.DefaultConfig([]byte(strings.Repeat("k", 32))))
	if err != nil {
		t.Fatal(err)
	}
	user, _ := tokens.IssueAccess(e.mate, token.KindUser, 0, "jti-u", "sid-u")
	admin, _ := tokens.IssueAccess(e.owner, token.KindAdmin, 0, "jti-a", "sid-a")
	for _, tok := range []string{user.Token, admin.Token, "sk_mcp_" + strings.Repeat("0", 43)} {
		code, b := e.mcpCall(tok)
		if code != 401 || !strings.Contains(b, "UNAUTHENTICATED") {
			t.Fatalf("%q：%d %s", tok[:12], code, b)
		}
	}
}

func TestMCPKey_非成员不能为该工作区建密钥(t *testing.T) {
	e := newEnv(t)
	code, b := e.api(http.MethodPost, "/me/mcp/keys", e.outsider, map[string]any{
		"workspaceId": strconv.FormatInt(e.ws, 10), "clientName": "x", "scopes": []string{"seeaitv:view"}})
	if code != 403 || !strings.Contains(string(b), "NOT_WORKSPACE_MEMBER") {
		t.Fatalf("非成员建密钥：%d %s", code, b)
	}
	var n int
	_ = e.pool.QueryRow(context.Background(), `select count(*) from mcp_grant`).Scan(&n)
	if n != 0 {
		t.Fatalf("不该落库：%d", n)
	}
}

func TestMCPKey_创建与撤销写审计且不含明文(t *testing.T) {
	e := newEnv(t)
	c := e.createKey(e.mate, nil)
	if code, _ := e.api(http.MethodDelete, "/me/mcp/keys/"+c.ID, e.mate, nil); code != 204 {
		t.Fatal(code)
	}
	c2 := e.createKey(e.mate, nil)
	if code, _ := e.api(http.MethodDelete, "/me/mcp/grants/"+c2.GrantID, e.mate, nil); code != 204 {
		t.Fatal(code)
	}
	rows, err := e.pool.Query(context.Background(), `
		select action, actor_type, actor_id, workspace_id, object_type, object_id,
		       coalesce(before_value::text, ''), coalesce(after_value::text, '')
		from audit_log where object_type = 'mcp_grant' order by id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var actions []string
	for rows.Next() {
		var action, actorType, objType, before, after string
		var actor, ws, obj int64
		if err := rows.Scan(&action, &actorType, &actor, &ws, &objType, &obj, &before, &after); err != nil {
			t.Fatal(err)
		}
		actions = append(actions, action)
		if actorType != "user" || actor != e.mate || ws != e.ws {
			t.Fatalf("审计主体：%s %s %d %d", action, actorType, actor, ws)
		}
		dump := before + after
		for _, k := range []created{c, c2} {
			h := sha256.Sum256([]byte(k.Plaintext))
			if strings.Contains(dump, k.Plaintext[len(domain.KeyPrefix):]) || strings.Contains(dump, hex.EncodeToString(h[:])) {
				t.Fatalf("审计含明文或哈希：%s %s", action, dump)
			}
		}
	}
	if !slices.Equal(actions, []string{"mcp.key.create", "mcp.key.revoke", "mcp.key.create", "mcp.grant.revoke"}) {
		t.Fatalf("审计动作：%v", actions)
	}
}

func TestMCPAuth_last_used_at一分钟内只写一次(t *testing.T) {
	e := newEnv(t)
	c := e.createKey(e.mate, nil)
	gid := mustID(t, c.GrantID)
	lastUsed := func() *time.Time {
		var ts *time.Time
		if err := e.pool.QueryRow(context.Background(), `select last_used_at from mcp_grant where id = $1`, gid).Scan(&ts); err != nil {
			t.Fatal(err)
		}
		return ts
	}
	if lastUsed() != nil {
		t.Fatal("还没用过")
	}
	if code, _ := e.mcpCall(c.Plaintext); code != 200 {
		t.Fatal(code)
	}
	e.resolver.Flush()
	first := lastUsed()
	if first == nil {
		t.Fatal("用过之后应写入")
	}
	for i := 0; i < 3; i++ {
		if code, _ := e.mcpCall(c.Plaintext); code != 200 {
			t.Fatal(code)
		}
	}
	e.resolver.Flush()
	if got := lastUsed(); !got.Equal(*first) {
		t.Fatalf("一分钟内又写了：%s → %s", first, got)
	}
	// SQL 里那一道（多实例）：换一个没有进程内节流记录的 Resolver，一分钟内仍不写。
	other := mcpapp.NewResolver(mcprepo.New(e.pool), wiring.MCPUsers{Svc: identityapp.New(identityrepo.New(e.pool), nil, nil, nil, e.next)},
		wiring.MCPMembers{Svc: e.collab}, nil, nil)
	p, err := other.Resolve(context.Background(), c.Plaintext)
	if err != nil {
		t.Fatal(err)
	}
	other.Flush()
	if got := lastUsed(); !got.Equal(*first) {
		t.Fatalf("另一个实例一分钟内又写了：%s → %s", first, got)
	}
	// 超过一分钟之后会再写。
	if _, err := e.pool.Exec(context.Background(), `update mcp_grant set last_used_at = now() - interval '2 minutes' where id = $1`, gid); err != nil {
		t.Fatal(err)
	}
	stale := lastUsed()
	// 再换一个 Resolver：other 的进程内节流刚记过这个授权，会直接跳过（那是它该做的事）。
	third := mcpapp.NewResolver(mcprepo.New(e.pool), wiring.MCPUsers{Svc: identityapp.New(identityrepo.New(e.pool), nil, nil, nil, e.next)},
		wiring.MCPMembers{Svc: e.collab}, nil, nil)
	if _, err := third.Resolve(context.Background(), c.Plaintext); err != nil {
		t.Fatal(err)
	}
	third.Flush()
	if got := lastUsed(); got == nil || !got.After(*stale) {
		t.Fatalf("超过一分钟应再写：%v → %v", stale, got)
	}
	// 顺带：Principal 的实时字段（MCP-F-15：没有会员也能用，等级 0）。
	if p.UserID != e.mate || p.WorkspaceID != e.ws || p.Role != collab.RoleEditor || p.PayerAccountID != e.payer ||
		p.MemberLevelID != 0 || p.GrantID != gid || p.ClientName != "Claude Desktop" ||
		!slices.Contains(p.Scopes, domain.ScopeGenerate) {
		t.Fatalf("Principal：%+v", p)
	}
}

func mustID(t *testing.T, s string) int64 {
	t.Helper()
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("ID %q：%v", s, err)
	}
	return v
}
