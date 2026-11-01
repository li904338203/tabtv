package transport

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// MCP-F-12/13：工具列表按 scope ∩ 当前角色过滤；被点名调用时 MCP_INSUFFICIENT_SCOPE 并列出所需 scope。
func TestMCPHandler_按scope过滤工具列表(t *testing.T) {
	srv := serve(t, Options{Auth: auth})
	for _, v := range []string{v2026, v2025} {
		for _, tok := range []string{"view-scope", "viewer-role"} {
			cs, _, err := connect(t, srv.URL+"/mcp", v, tok, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := toolNames(t, cs); !slices.Equal(got, []string{"canvas_read"}) {
				t.Fatalf("%s/%s 列出 %v，期望只有只读工具", v, tok, got)
			}
			res := call(t, cs, "generate", map[string]any{"nodeId": "1"})
			txt := text(res)
			if !res.IsError || !strings.Contains(txt, "40307") || !strings.Contains(txt, "MCP_INSUFFICIENT_SCOPE") ||
				!strings.Contains(txt, `"requiredScopes":["seeaitv:view","seeaitv:generate"]`) {
				t.Fatalf("%s/%s 越权调用：%s", v, tok, txt)
			}
			// 两种原因分开说：缺 scope → 重新授权；角色不够 → 找管理员。
			if tok == "view-scope" && (!strings.Contains(txt, `"missingScopes":["seeaitv:generate"]`) || !strings.Contains(txt, `"roleInsufficient":false`)) {
				t.Fatalf("缺 scope：%s", txt)
			}
			if tok == "viewer-role" && (!strings.Contains(txt, `"missingScopes":[]`) || !strings.Contains(txt, `"roleInsufficient":true`) ||
				!strings.Contains(txt, "管理员")) {
				t.Fatalf("角色不够：%s", txt)
			}
		}
	}
	cs, _, err := connect(t, srv.URL+"/mcp", v2026, "all", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := toolNames(t, cs); len(got) != 2 {
		t.Fatalf("全权限应列出两个工具：%v", got)
	}
}

// 凭据解析返回的 403（封禁 / 已不是成员）原样回给客户端，不压成 401；
// 401 才带 WWW-Authenticate（403 重新授权也没用）。
type errAuth struct{ err error }

func (e errAuth) Authenticate(*http.Request) (app.Principal, error) { return app.Principal{}, e.err }

func TestMCPHandler_凭据错误码原样返回(t *testing.T) {
	cases := []struct {
		err    error
		status int
		name   string
		www    bool
	}{
		{httpx.Err(errcode.TokenRevoked), 401, "TOKEN_REVOKED", true},
		{httpx.Err(errcode.TokenExpired), 401, "TOKEN_EXPIRED", true},
		{httpx.Err(errcode.NotWorkspaceMember), 403, "NOT_WORKSPACE_MEMBER", false},
		{httpx.Err(errcode.AccountBanned), 403, "ACCOUNT_BANNED", false},
		{ErrNoCredential, 401, "UNAUTHENTICATED", true},
	}
	for _, c := range cases {
		srv := serve(t, Options{Auth: errAuth{c.err}})
		resp := rawPost(t, srv.URL+"/mcp", http.Header{"Authorization": {"Bearer x"}})
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != c.status || !strings.Contains(string(b), c.name) ||
			(resp.Header.Get("WWW-Authenticate") != "") != c.www {
			t.Fatalf("%s：%d %s www=%q", c.name, resp.StatusCode, b, resp.Header.Get("WWW-Authenticate"))
		}
	}
}

func rawRPC(t *testing.T, method, url string, header http.Header, body string) (*http.Response, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, url, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range header {
		req.Header[k] = v
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode >= 300 || method == http.MethodPost {
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	return resp, "" // GET SSE：不读到底（那是长连接）
}

// 2025 会话（Mcp-Session-Id）绑定建立它的授权：换一个凭据拿着同一个会话 ID 来一律 403，
// 同一授权的另一把令牌（刷新轮换）照常；不认识的会话 ID 404（客户端会重新 initialize）。
func TestMCPHandler_会话绑定建立时的凭据(t *testing.T) {
	srv := serve(t, Options{Auth: auth})
	cs, rt, err := connect(t, srv.URL+"/mcp", v2025, "all", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.sids) == 0 {
		t.Fatal("2025 应建立会话")
	}
	sid := rt.sids[0]
	list := `{"jsonrpc":"2.0","id":7,"method":"tools/list","params":{}}`
	hdr := func(tok, session string) http.Header {
		return http.Header{"Authorization": {"Bearer " + tok}, HeaderSessionID: {session},
			HeaderProtocolVersion: {v2025}}
	}

	// 另一个授权（哪怕同一个用户、权限更小）拿着这个会话 ID：POST 与 GET SSE 都 403，并说清原因。
	for _, m := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		body := ""
		if m == http.MethodPost {
			body = list
		}
		resp, b := rawRPC(t, m, srv.URL+"/mcp", hdr("canvas", sid), body)
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(b, "PERMISSION_DENIED") || !strings.Contains(b, "另一个凭据") {
			t.Fatalf("%s 换凭据复用会话：%d %s", m, resp.StatusCode, b)
		}
	}
	// 被拒之后原会话不受影响（DELETE 没有真的关掉它）。
	if got := toolNames(t, cs); len(got) != 2 {
		t.Fatalf("原会话应仍可用：%v", got)
	}
	// 同一授权的另一把令牌：照常。
	resp, b := rawRPC(t, http.MethodPost, srv.URL+"/mcp", hdr("all-rotated", sid), list)
	if resp.StatusCode != http.StatusOK || !strings.Contains(b, "canvas_read") {
		t.Fatalf("同一授权轮换后的令牌：%d %s", resp.StatusCode, b)
	}
	// 不认识的会话 ID：404，不放行给 SDK。
	resp, b = rawRPC(t, http.MethodPost, srv.URL+"/mcp", hdr("all", "forged-session"), list)
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(b, "重新 initialize") {
		t.Fatalf("伪造会话：%d %s", resp.StatusCode, b)
	}
	// 另一个凭据自己 initialize：得到自己的会话，与原会话互不相干。
	cs2, rt2, err := connect(t, srv.URL+"/mcp", v2025, "canvas", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rt2.sids) == 0 || rt2.sids[0] == sid {
		t.Fatalf("新会话：%v", rt2.sids)
	}
	if got := toolNames(t, cs2); !slices.Equal(got, []string{"canvas_read"}) {
		t.Fatalf("新会话按自己的凭据过滤：%v", got)
	}
	// 原凭据拿新会话的 ID 同样被拒（绑定是双向独立的）。
	resp, _ = rawRPC(t, http.MethodPost, srv.URL+"/mcp", hdr("all", rt2.sids[0]), list)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("原凭据复用新会话：%d", resp.StatusCode)
	}
	// 关闭会话（DELETE）之后绑定随之删除：再拿这个 ID 来是 404。
	if err := cs.Close(); err != nil {
		t.Fatal(err)
	}
	resp, _ = rawRPC(t, http.MethodPost, srv.URL+"/mcp", hdr("all", sid), list)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("关闭后的会话：%d", resp.StatusCode)
	}
}

// mutableAuth 的权限可以在两次调用之间改（模拟成员被降级）。
type mutableAuth struct {
	mu sync.Mutex
	p  app.Principal
}

func (m *mutableAuth) Authenticate(*http.Request) (app.Principal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.p, nil
}

func (m *mutableAuth) set(role collab.Role) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.p.Role = role
}

// MCP-F-11：**同一个会话里**下一次调用就按新的角色判定。
// 2025 会话模式下 SDK 用的是会话初始 ctx —— 只把 Principal 放进 HTTP ctx 的话，
// 这里降级之后花钱工具照样列出、照样受理（MCPT-03 实测踩到，见 carry.go）。
func TestMCPHandler_同一会话内降级下一次调用生效(t *testing.T) {
	for _, v := range []string{v2025, "2025-06-18", v2026} {
		ma := &mutableAuth{p: app.Principal{UserID: 1, WorkspaceID: 2, GrantID: 77, ClientName: "m",
			Toolsets: domain.Toolsets(), Scopes: domain.Scopes(), Role: collab.RoleOwner}}
		srv := serve(t, Options{Auth: ma})
		cs, _, err := connect(t, srv.URL+"/mcp", v, "m", nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := toolNames(t, cs); len(got) != 2 {
			t.Fatalf("%s owner：%v", v, got)
		}
		ma.set(collab.RoleViewer)
		if got := toolNames(t, cs); !slices.Equal(got, []string{"canvas_read"}) {
			t.Fatalf("%s 降级后同一会话仍列出 %v", v, got)
		}
		if res := call(t, cs, "generate", map[string]any{"nodeId": "1"}); !res.IsError || !strings.Contains(text(res), "MCP_INSUFFICIENT_SCOPE") {
			t.Fatalf("%s 降级后同一会话仍能调花钱工具：%s", v, text(res))
		}
		ma.set(collab.RoleEditor)
		if res := call(t, cs, "generate", map[string]any{"nodeId": "1"}); res.IsError {
			t.Fatalf("%s 升回 editor 后应可调：%s", v, text(res))
		}
	}
}

// 401 的 WWW-Authenticate 带**本端点**的受保护资源元数据地址（RFC 9728 §5.1，MCPT-12）；
// 没带凭据时不写 error="invalid_token"（RFC 6750 §3.1），带了无效凭据才写。
func TestMCPHandler_401带resource_metadata(t *testing.T) {
	srv := serve(t, Options{Auth: errAuth{ErrNoCredential}, PublicURL: "https://example.com/mcp"})
	for path, want := range map[string]string{
		"/mcp":          "https://example.com/.well-known/oauth-protected-resource/mcp",
		"/mcp/readonly": "https://example.com/.well-known/oauth-protected-resource/mcp/readonly",
		"/mcp/x/script": "https://example.com/.well-known/oauth-protected-resource/mcp/x/script",
	} {
		resp := rawPost(t, srv.URL+path, nil)
		got := resp.Header.Get("WWW-Authenticate")
		if resp.StatusCode != 401 || got != `Bearer resource_metadata="`+want+`"` {
			t.Fatalf("%s 无凭据：%d %q", path, resp.StatusCode, got)
		}
		resp = rawPost(t, srv.URL+path, http.Header{"Authorization": {"Bearer x"}})
		if got := resp.Header.Get("WWW-Authenticate"); !strings.Contains(got, `resource_metadata="`+want+`"`) ||
			!strings.Contains(got, `error="invalid_token"`) {
			t.Fatalf("%s 无效凭据：%q", path, got)
		}
	}
}

// ctx 里的受众就是本端点的规范 URI（OAuth 访问令牌按它精确比对，MCPT-12）。
func TestMCPHandler_受众为端点规范URI(t *testing.T) {
	var got []string
	var mu sync.Mutex
	a := authFunc(func(r *http.Request) (app.Principal, error) {
		aud, _ := app.AudienceFrom(r.Context())
		mu.Lock()
		got = append(got, aud)
		mu.Unlock()
		return app.Principal{}, ErrNoCredential
	})
	srv := serve(t, Options{Auth: a, PublicURL: "https://Example.com/mcp"})
	for _, p := range []string{"/mcp", "/mcp/readonly", "/mcp/x/script"} {
		rawPost(t, srv.URL+p, nil)
	}
	want := []string{"https://example.com/mcp", "https://example.com/mcp/readonly", "https://example.com/mcp/x/script"}
	if !slices.Equal(got, want) {
		t.Fatalf("受众 %v，期望 %v", got, want)
	}
}

type authFunc func(r *http.Request) (app.Principal, error)

func (f authFunc) Authenticate(r *http.Request) (app.Principal, error) { return f(r) }

// ctxAuth 模拟验凭据时的数据库查询：请求 ctx 已取消就以 ctx 的错误失败。
type ctxAuth struct{}

func (ctxAuth) Authenticate(r *http.Request) (app.Principal, error) {
	if err := r.Context().Err(); err != nil {
		return app.Principal{}, err
	}
	return app.Principal{}, ErrNoCredential
}

// MCPT-15 实测：Codex 结束会话时发 DELETE /mcp 后立刻断开。客户端已经走了的请求不该按内部错误回 500、记 ERROR ——
// 那是每个会话一次的假告警。只是凭据确实不对时，照常 401。
func TestRequirePrincipal_客户端已断开不记内部错误(t *testing.T) {
	var buf syncBuf
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	h := requirePrincipal(ctxAuth{}, nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("凭据失败时不该进到下一层")
	}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/mcp", nil).WithContext(ctx))
	if rec.Code >= 500 || strings.Contains(buf.String(), "请求处理失败") {
		t.Fatalf("客户端已断开：HTTP %d，日志 %q", rec.Code, buf.String())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("没有人在等响应，不该写响应体：%q", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("凭据缺失应照常 401 + WWW-Authenticate：%d %v", rec.Code, rec.Header())
	}
}
