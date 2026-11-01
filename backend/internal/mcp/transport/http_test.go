package transport

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 协议测试用官方 SDK 客户端打真实 HTTP（httptest），覆盖两个协议版本（MCP-F-02）。
// 注册表注入两个假工具：canvas 工具集的只读工具、generation 工具集的花钱工具。

const (
	v2026 = "2026-07-28"
	v2025 = "2025-11-25"
)

type outlineIn struct {
	ProjectID string `json:"projectId" jsonschema:"项目 ID"`
	Count     int    `json:"count,omitempty" jsonschema:"返回多少个节点"`
	SleepMs   int    `json:"sleepMs,omitempty" jsonschema:"测试用：处理前等待毫秒数"`
}
type outlineNode struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}
type outlineOut struct {
	ProjectID string        `json:"projectId"`
	Nodes     []outlineNode `json:"nodes"`
	CalledBy  string        `json:"calledBy"`
}

type genIn struct {
	NodeID string `json:"nodeId"`
}
type genOut struct {
	GenerationID string `json:"generationId"`
}

func testRegistry() *app.Registry {
	r := app.NewRegistry()
	r.Register(app.Define(app.Tool{Name: "canvas_read", Toolset: domain.ToolsetCanvas, Description: "读画布大纲（测试）",
		Methods: []app.Method{{Action: collab.ActionView}}},
		func(ctx context.Context, _ string, in outlineIn) (outlineOut, string, error) {
			if in.SleepMs > 0 {
				select {
				case <-ctx.Done():
					return outlineOut{}, "", ctx.Err()
				case <-time.After(time.Duration(in.SleepMs) * time.Millisecond):
				}
			}
			p, _ := app.PrincipalFrom(ctx)
			out := outlineOut{ProjectID: in.ProjectID, CalledBy: p.ClientName, Nodes: []outlineNode{}}
			for i := 0; i < in.Count; i++ {
				out.Nodes = append(out.Nodes, outlineNode{ID: "n" + strings.Repeat("0", 18), Title: strings.Repeat("节点标题", 10)})
			}
			return out, "画布有 " + itoa(len(out.Nodes)) + " 个节点", nil
		}))
	r.Register(app.Define(app.Tool{Name: "generate", Toolset: domain.ToolsetGeneration, Description: "生成（测试，花钱）",
		Methods: []app.Method{{Action: collab.ActionGenerate, Paid: true, Write: true}}},
		func(context.Context, string, genIn) (genOut, string, error) {
			return genOut{GenerationID: "123456789012345678"}, "已受理", nil
		}))
	return r
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// fakeAuth：token → Principal。真实实现（KeyAuth + app.Resolver）的真库测试在 transport/integration。
type fakeAuth map[string]app.Principal

func (f fakeAuth) Authenticate(r *http.Request) (app.Principal, error) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if p, known := f[tok]; ok && known {
		return p, nil
	}
	return app.Principal{}, ErrNoCredential
}

var auth = fakeAuth{
	"all": {UserID: 1, WorkspaceID: 2, GrantID: 10, ClientName: "all", Toolsets: domain.Toolsets(),
		Scopes: domain.Scopes(), Role: collab.RoleOwner},
	// 同一个授权的另一把令牌（OAuth 刷新轮换后就是这样）：可以接着用同一个会话。
	"all-rotated": {UserID: 1, WorkspaceID: 2, GrantID: 10, ClientName: "all", Toolsets: domain.Toolsets(),
		Scopes: domain.Scopes(), Role: collab.RoleOwner},
	"canvas": {UserID: 1, WorkspaceID: 2, GrantID: 11, ClientName: "canvas-only", Toolsets: []string{domain.ToolsetCanvas},
		Scopes: domain.Scopes(), Role: collab.RoleOwner},
	// scope 只有 view：花钱工具不可见。
	"view-scope": {UserID: 1, WorkspaceID: 2, GrantID: 12, ClientName: "view", Toolsets: domain.Toolsets(),
		Scopes: []string{domain.ScopeView}, Role: collab.RoleOwner},
	// scope 全给、角色是 viewer：同样只剩只读工具（有效权限 = scope ∩ 当前角色）。
	"viewer-role": {UserID: 3, WorkspaceID: 2, GrantID: 13, ClientName: "viewer", Toolsets: domain.Toolsets(),
		Scopes: domain.Scopes(), Role: collab.RoleViewer},
}

func serve(t *testing.T, o Options) *httptest.Server {
	t.Helper()
	if o.Registry == nil {
		o.Registry = testRegistry()
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	r := chi.NewRouter()
	r.Use(httpx.TraceID)
	NewHandler(o).Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

type headerRT struct {
	h    http.Header
	sids []string
}

func (h *headerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range h.h {
		r.Header[k] = v
	}
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err == nil && resp.Header.Get("Mcp-Session-Id") != "" {
		h.sids = append(h.sids, resp.Header.Get("Mcp-Session-Id"))
	}
	return resp, err
}

func connect(t *testing.T, url, version, token string, extra http.Header) (*mcp.ClientSession, *headerRT, error) {
	t.Helper()
	h := http.Header{}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	for k, v := range extra {
		h[k] = v
	}
	rt := &headerRT{h: h}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "mcpt02-test", Version: "0"}, nil).Connect(ctx,
		&mcp.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: rt}, MaxRetries: -1},
		&mcp.ClientSessionOptions{ProtocolVersion: version})
	if cs != nil {
		t.Cleanup(func() { _ = cs.Close() })
	}
	return cs, rt, err
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

func call(t *testing.T, cs *mcp.ClientSession, name string, args any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("调用 %s：%v", name, err)
	}
	return res
}

func text(r *mcp.CallToolResult) string {
	var parts []string
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func rawPost(t *testing.T, url string, header http.Header) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"raw","version":"0"}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestMCPHandler_无凭据401(t *testing.T) {
	// 不注入 Auth：兜底实现一律拒绝。
	srv := serve(t, Options{})
	for _, p := range []string{"/mcp", "/mcp/readonly", "/mcp/x/canvas"} {
		for _, tok := range []string{"", "Bearer sk_mcp_whatever"} {
			h := http.Header{}
			if tok != "" {
				h.Set("Authorization", tok)
			}
			resp := rawPost(t, srv.URL+p, h)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s（%q）：%d，期望 401", p, tok, resp.StatusCode)
			}
			if !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Bearer ") {
				t.Fatalf("%s 缺 WWW-Authenticate", p)
			}
			b, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(b), "UNAUTHENTICATED") {
				t.Fatalf("%s 错误体：%s", p, b)
			}
		}
	}
	// SDK 客户端两个版本都拿到 Unauthorized。
	for _, v := range []string{v2026, v2025} {
		if _, _, err := connect(t, srv.URL+"/mcp", v, "", nil); err == nil || !strings.Contains(err.Error(), "Unauthorized") {
			t.Fatalf("%s 无凭据应 Unauthorized：%v", v, err)
		}
	}
	// 注入了 Auth 时，错误的 token 同样 401。
	srv2 := serve(t, Options{Auth: auth})
	if resp := rawPost(t, srv2.URL+"/mcp", http.Header{"Authorization": {"Bearer nope"}}); resp.StatusCode != 401 {
		t.Fatalf("错误 token：%d", resp.StatusCode)
	}
}

func TestMCPHandler_列工具(t *testing.T) {
	srv := serve(t, Options{Auth: auth})
	cs, _, err := connect(t, srv.URL+"/mcp", v2026, "all", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 2 {
		t.Fatalf("工具：%v", res.Tools)
	}
	byName := map[string]*mcp.Tool{}
	for _, tl := range res.Tools {
		byName[tl.Name] = tl
	}
	cr, gen := byName["canvas_read"], byName["generate"]
	if cr == nil || gen == nil {
		t.Fatalf("缺工具：%v", byName)
	}
	if !cr.Annotations.ReadOnlyHint || gen.Annotations.ReadOnlyHint || gen.Annotations.OpenWorldHint == nil || !*gen.Annotations.OpenWorldHint {
		t.Fatalf("注解：%+v / %+v", cr.Annotations, gen.Annotations)
	}
	var in, out struct {
		Properties map[string]struct {
			Type any `json:"type"`
		} `json:"properties"`
	}
	ib, _ := json.Marshal(cr.InputSchema)
	ob, _ := json.Marshal(cr.OutputSchema)
	_ = json.Unmarshal(ib, &in)
	_ = json.Unmarshal(ob, &out)
	if in.Properties["projectId"].Type != "string" || out.Properties["truncated"].Type != "boolean" {
		t.Fatalf("schema：in=%s out=%s", ib, ob)
	}
	// 默认开启集不含 script 等：凭据全开时只按默认集。
	if _, ok := byName["script"]; ok {
		t.Fatal("默认集外的工具不该出现")
	}
}

func TestMCPHandler_两个协议版本(t *testing.T) {
	srv := serve(t, Options{Auth: auth})
	for _, c := range []struct {
		ask, want   string
		wantSession bool
	}{
		{v2026, v2026, false}, // 无状态分支：不发 Mcp-Session-Id
		{v2025, v2025, true},  // 会话分支：有会话（elicitation 要靠它）
		{"2025-06-18", "2025-06-18", true},
	} {
		t.Run(c.ask, func(t *testing.T) {
			cs, rt, err := connect(t, srv.URL+"/mcp", c.ask, "all", nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := cs.InitializeResult().ProtocolVersion; got != c.want {
				t.Fatalf("协商出 %s，期望 %s", got, c.want)
			}
			if (len(rt.sids) > 0) != c.wantSession {
				t.Fatalf("会话 ID：%v，期望有=%v", rt.sids, c.wantSession)
			}
			if len(toolNames(t, cs)) != 2 {
				t.Fatal("列工具失败")
			}
			res := call(t, cs, "canvas_read", map[string]any{"projectId": "42", "count": 2})
			if res.IsError {
				t.Fatalf("调用失败：%s", text(res))
			}
			var out outlineOut
			b, _ := json.Marshal(res.StructuredContent)
			if err := json.Unmarshal(b, &out); err != nil || out.ProjectID != "42" || len(out.Nodes) != 2 || out.CalledBy != "all" {
				t.Fatalf("structuredContent：%s %v", b, err)
			}
			// 文本里同时有摘要与完整 JSON（opencode 不读 structuredContent）。
			if len(res.Content) != 2 || !strings.Contains(text(res), "画布有 2 个节点") || !strings.Contains(text(res), `"projectId":"42"`) {
				t.Fatalf("文本：%s", text(res))
			}
			// 参数错误是 isError + 业务码，不是协议错误。
			bad := call(t, cs, "canvas_read", map[string]any{"projectId": 42})
			if !bad.IsError || !strings.Contains(text(bad), "40001") || !strings.Contains(text(bad), "INVALID_ARGUMENT") {
				t.Fatalf("参数错误：%s", text(bad))
			}
		})
	}
}

func TestMCPHandler_关闭总开关404(t *testing.T) {
	var h *Handler // SEEAITV_MCP_ENABLED=false 时 cmd/api 不构造 Handler
	r := chi.NewRouter()
	h.Mount(r)
	srv := httptest.NewServer(r)
	defer srv.Close()
	for _, p := range []string{"/mcp", "/mcp/readonly", "/mcp/x/canvas"} {
		if resp := rawPost(t, srv.URL+p, http.Header{"Authorization": {"Bearer all"}}); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s：%d，期望 404", p, resp.StatusCode)
		}
	}
}

func TestMCPHandler_x路径按工具集过滤(t *testing.T) {
	srv := serve(t, Options{Auth: auth})
	for _, v := range []string{v2026, v2025} {
		cs, _, err := connect(t, srv.URL+"/mcp/x/canvas", v, "all", nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := toolNames(t, cs); !slices.Equal(got, []string{"canvas_read"}) {
			t.Fatalf("%s /mcp/x/canvas 列出 %v", v, got)
		}
		// 隐藏的工具被点名调用：isError 并说清要开哪个工具集。
		res := call(t, cs, "generate", map[string]any{"nodeId": "1"})
		if !res.IsError || !strings.Contains(text(res), "generation") {
			t.Fatalf("%s 调隐藏工具：%s", v, text(res))
		}
	}
	// 未知工具集名：参数错误（先于凭据校验）。
	resp := rawPost(t, srv.URL+"/mcp/x/writing", nil)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(b), "INVALID_ARGUMENT") || !strings.Contains(string(b), "writing") {
		t.Fatalf("未知工具集：%d %s", resp.StatusCode, b)
	}
	// 请求头不能放大凭据：凭据只有 canvas，头里点名 generation 也拿不到。
	cs, _, err := connect(t, srv.URL+"/mcp", v2026, "canvas", http.Header{domain.HeaderToolsets: {"canvas,generation"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := toolNames(t, cs); !slices.Equal(got, []string{"canvas_read"}) {
		t.Fatalf("请求头放大了凭据：%v", got)
	}
	// 请求头收窄：凭据全开、头里只要 generation。
	cs, _, err = connect(t, srv.URL+"/mcp", v2025, "all", http.Header{domain.HeaderToolsets: {"generation"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := toolNames(t, cs); !slices.Equal(got, []string{"generate"}) {
		t.Fatalf("请求头收窄：%v", got)
	}
	// 请求头里的未知名：400。
	if resp := rawPost(t, srv.URL+"/mcp", http.Header{"Authorization": {"Bearer all"}, domain.HeaderToolsets: {"canvas,nope"}}); resp.StatusCode != 400 {
		t.Fatalf("头里未知工具集：%d", resp.StatusCode)
	}
}

func TestMCPHandler_只读端点(t *testing.T) {
	srv := serve(t, Options{Auth: auth})
	cs, _, err := connect(t, srv.URL+"/mcp/readonly", v2026, "all", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := toolNames(t, cs); !slices.Equal(got, []string{"canvas_read"}) {
		t.Fatalf("只读端点列出 %v", got)
	}
	res := call(t, cs, "generate", map[string]any{"nodeId": "1"})
	if !res.IsError || !strings.Contains(text(res), "只读端点") {
		t.Fatalf("只读端点调花钱工具：%s", text(res))
	}
}

func TestMCPHandler_结果截断(t *testing.T) {
	srv := serve(t, Options{Auth: auth, ResultMaxTokens: 400})
	cs, _, err := connect(t, srv.URL+"/mcp", v2025, "all", nil)
	if err != nil {
		t.Fatal(err)
	}
	res := call(t, cs, "canvas_read", map[string]any{"projectId": "1", "count": 200})
	if res.IsError {
		t.Fatalf("截断后应通过 outputSchema 校验：%s", text(res))
	}
	b, _ := json.Marshal(res.StructuredContent)
	if !json.Valid(b) {
		t.Fatal("截断后不是合法 JSON")
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m["truncated"] != true || !strings.Contains(m["truncationHint"].(string), "nodes 只保留前") {
		t.Fatalf("截断标记：%s", b)
	}
	if !strings.Contains(text(res), "已截断") {
		t.Fatalf("摘要要提示截断：%s", text(res))
	}
}

func TestMCPHandler_单次调用截止时间(t *testing.T) {
	srv := serve(t, Options{Auth: auth, CallTimeout: 200 * time.Millisecond})
	for _, v := range []string{v2026, v2025} {
		cs, _, err := connect(t, srv.URL+"/mcp", v, "all", nil)
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		res := call(t, cs, "canvas_read", map[string]any{"projectId": "1", "sleepMs": 5000})
		if !res.IsError || !strings.Contains(text(res), "上限") || time.Since(start) > 3*time.Second {
			t.Fatalf("%s 应在截止时间处失败：%s（%s）", v, text(res), time.Since(start))
		}
	}
}

func TestMCPHandler_Origin校验(t *testing.T) {
	srv := serve(t, Options{Auth: auth, PublicURL: "https://mcp.example.com/mcp"})
	cases := []struct {
		origin string
		ok     bool
	}{
		{"", true},
		{"https://mcp.example.com", true},
		{srv.URL, true}, // 同源
		{"https://evil.example.com", false},
	}
	for _, c := range cases {
		h := http.Header{"Authorization": {"Bearer all"}}
		if c.origin != "" {
			h.Set("Origin", c.origin)
		}
		resp := rawPost(t, srv.URL+"/mcp", h)
		if (resp.StatusCode == http.StatusOK) != c.ok {
			t.Fatalf("Origin %q：%d", c.origin, resp.StatusCode)
		}
	}
}
