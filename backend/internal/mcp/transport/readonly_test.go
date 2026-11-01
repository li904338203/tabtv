package transport

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
)

// 只读端点的协议测试（MCPT-07，MCP-F-14、MCP-AC-02）。
//
// M1 的真实注册表里还没有写 method，所以注入一个混合工具 projects（list 只读 + create 写）：
// 只读端点的保证不能是「碰巧没有写工具」。
// ⚠️ 测试一律绕过注解直接点名调用（MCP-N-04：注解只是提示，客户端可以无视）。

type projIn struct {
	Name string `json:"name,omitempty" jsonschema:"项目名（create 用）"`
}
type projOut struct {
	Method string `json:"method"`
}

func readonlyRegistry() *app.Registry {
	r := testRegistry()
	r.Register(app.Define(app.Tool{Name: "projects", Toolset: domain.ToolsetProjects, Description: "项目（测试，混合工具）",
		Methods: []app.Method{{Name: "list", Action: collab.ActionView}, {Name: "create", Action: collab.ActionEdit, Write: true}}},
		func(_ context.Context, method string, _ projIn) (projOut, string, error) {
			return projOut{Method: method}, "ok", nil
		}))
	return r
}

// methodEnum 取工具 inputSchema 里 method 的枚举（没有 method 参数返回 nil）。
func methodEnum(t *testing.T, tl *mcp.Tool) []string {
	t.Helper()
	b, err := json.Marshal(tl.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if p, ok := s.Properties["method"]; ok {
		return p.Enum
	}
	return nil
}

func listTools(t *testing.T, cs *mcp.ClientSession) map[string]*mcp.Tool {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*mcp.Tool{}
	for _, tl := range res.Tools {
		out[tl.Name] = tl
	}
	return out
}

// argsFor 给测试注册表里每个 (工具, method) 一组合法参数。
func argsFor(tool, method string) map[string]any {
	switch tool {
	case "canvas_read":
		return map[string]any{"projectId": "1"}
	case "generate":
		return map[string]any{"nodeId": "1"}
	default:
		return map[string]any{"method": method}
	}
}

func TestMCPReadonly_列表不含写与花钱method(t *testing.T) {
	srv := serve(t, Options{Auth: auth, Registry: readonlyRegistry()})
	for _, v := range []string{v2026, v2025} {
		// 持有全部 scope 的 owner、工具集全开。
		cs, _, err := connect(t, srv.URL+"/mcp/readonly", v, "all", nil)
		if err != nil {
			t.Fatal(err)
		}
		tools := listTools(t, cs)
		if _, ok := tools["generate"]; ok {
			t.Fatalf("%s 只读端点列出了花钱工具", v)
		}
		if got := slices.Sorted(maps.Keys(tools)); !slices.Equal(got, []string{"canvas_read", "projects"}) {
			t.Fatalf("%s 只读端点列出 %v", v, got)
		}
		reg := readonlyRegistry()
		for name, tl := range tools {
			rt, _ := reg.Lookup(name)
			enum := methodEnum(t, tl)
			for _, m := range rt.Methods {
				if m.ReadOnly() {
					continue
				}
				if m.Name == "" || slices.Contains(enum, m.Name) {
					t.Fatalf("%s 只读端点暴露了写 / 花钱 method %s.%s（枚举 %v）", v, name, m.Name, enum)
				}
			}
			if !tl.Annotations.ReadOnlyHint {
				t.Fatalf("%s 只读端点上 %s 的注解不是只读", v, name)
			}
		}

		// 请求头点名 generation：仍看不到花钱 method（只读优先于请求头工具集）。
		cs, _, err = connect(t, srv.URL+"/mcp/readonly", v, "all", http.Header{domain.HeaderToolsets: {"generation"}})
		if err != nil {
			t.Fatal(err)
		}
		if got := toolNames(t, cs); len(got) != 0 {
			t.Fatalf("%s 只读端点 + X-MCP-Toolsets: generation 列出 %v", v, got)
		}
	}
}

func TestMCPReadonly_点名调用写method被拒(t *testing.T) {
	for _, v := range []string{v2026, v2025} {
		rec := &memRecorder{}
		srv := serve(t, Options{Auth: auth, Registry: readonlyRegistry(), Calls: rec})
		cs, _, err := connect(t, srv.URL+"/mcp/readonly", v, "all", nil)
		if err != nil {
			t.Fatal(err)
		}
		hdr, _, err := connect(t, srv.URL+"/mcp/readonly", v, "all", http.Header{domain.HeaderToolsets: {"generation"}})
		if err != nil {
			t.Fatal(err)
		}
		attempts := []struct {
			cs   *mcp.ClientSession
			tool string
			args map[string]any
		}{
			{cs, "projects", map[string]any{"method": "create", "name": "x"}},
			{cs, "generate", map[string]any{"nodeId": "1"}},
			// 参数不合法也先被闸门拒：改参数改成什么样都不会放行，先报参数错误只会误导。
			{cs, "generate", map[string]any{"nope": true}},
			{cs, "projects", map[string]any{"method": "create", "nope": true}},
			// 请求头开了 generation：花钱工具仍被拒，原因仍是只读端点。
			{hdr, "generate", map[string]any{"nodeId": "1"}},
		}
		for i, a := range attempts {
			res := call(t, a.cs, a.tool, a.args)
			txt := text(res)
			if !res.IsError || !strings.Contains(txt, "只读端点") || !strings.Contains(txt, "请改用 /mcp") {
				t.Fatalf("%s 第 %d 次（%s）：%s", v, i, a.tool, txt)
			}
			// 不提示 scope：scope 再多也没用。
			if strings.Contains(txt, "seeaitv:") || strings.Contains(txt, "requiredScopes") {
				t.Fatalf("%s 第 %d 次：只读端点的拒绝不该提示 scope：%s", v, i, txt)
			}
			if !strings.Contains(txt, `"reason":"readonly_endpoint"`) {
				t.Fatalf("%s 第 %d 次：缺拒绝原因：%s", v, i, txt)
			}
		}
		// 读 method 照常可用。
		if res := call(t, cs, "projects", map[string]any{"method": "list"}); res.IsError {
			t.Fatalf("%s 只读端点的读 method：%s", v, text(res))
		}
		es := rec.all()
		if len(es) != len(attempts)+1 {
			t.Fatalf("%s 审计 %d 条", v, len(es))
		}
		for i := range attempts {
			if es[i].Result != domain.CallDenied || es[i].ErrorCode != int(errcode.PermissionDenied) {
				t.Fatalf("%s 第 %d 条审计：%+v，期望 denied / 40301", v, i, es[i])
			}
		}
		if es[len(attempts)].Result != domain.CallOK {
			t.Fatalf("%s 读调用审计：%+v", v, es[len(attempts)])
		}
	}
}

func TestMCPReadonly_混合工具method枚举被收窄(t *testing.T) {
	srv := serve(t, Options{Auth: auth, Registry: readonlyRegistry()})
	enumAt := func(path, token string) ([]string, *mcp.Tool) {
		t.Helper()
		cs, _, err := connect(t, srv.URL+path, v2026, token, nil)
		if err != nil {
			t.Fatal(err)
		}
		tl, ok := listTools(t, cs)["projects"]
		if !ok {
			t.Fatalf("%s %s 没列出 projects", path, token)
		}
		return methodEnum(t, tl), tl
	}
	full, fullTool := enumAt("/mcp", "all")
	if !slices.Equal(full, []string{"list", "create"}) || fullTool.Annotations.ReadOnlyHint {
		t.Fatalf("/mcp 应暴露全部 method 且非只读：%v %+v", full, fullTool.Annotations)
	}
	ro, roTool := enumAt("/mcp/readonly", "all")
	if !slices.Equal(ro, []string{"list"}) || !roTool.Annotations.ReadOnlyHint {
		t.Fatalf("/mcp/readonly 应只剩 list 且注解只读：%v %+v", ro, roTool.Annotations)
	}
	// 收窄的是副本：之后 /mcp 上的定义不受影响。
	if again, _ := enumAt("/mcp", "all"); !slices.Equal(again, []string{"list", "create"}) {
		t.Fatalf("只读端点的收窄改到了共享的工具定义：%v", again)
	}
	// 同一个函数：/mcp 上 viewer 调不动 create，枚举里也没有它。
	if viewer, _ := enumAt("/mcp", "viewer-role"); !slices.Equal(viewer, []string{"list"}) {
		t.Fatalf("viewer 的枚举：%v", viewer)
	}
}

// tools/list 与 tools/call 对同一组输入的结论一致：列表里（含 method 枚举）出现的才调得动，
// 没出现的一律被拒（且拒绝是权限类 isError，不是参数错误）。
func TestMCPReadonly_列表与调用结论一致(t *testing.T) {
	reg := readonlyRegistry()
	srv := serve(t, Options{Auth: auth, Registry: reg})
	for _, path := range []string{"/mcp", "/mcp/readonly", "/mcp/x/projects", "/mcp/x/generation"} {
		for _, tok := range []string{"all", "canvas", "view-scope", "viewer-role"} {
			for _, hdr := range []string{"", "generation", "projects,canvas"} {
				h := http.Header{}
				if hdr != "" {
					h.Set(domain.HeaderToolsets, hdr)
				}
				cs, _, err := connect(t, srv.URL+path, v2026, tok, h)
				if err != nil {
					t.Fatal(err)
				}
				listed := listTools(t, cs)
				for _, rt := range reg.List() {
					for _, m := range rt.Methods {
						tl, ok := listed[rt.Name]
						visible := ok && (m.Name == "" || slices.Contains(methodEnum(t, tl), m.Name))
						res := call(t, cs, rt.Name, argsFor(rt.Name, m.Name))
						if visible == res.IsError {
							t.Fatalf("%s %s 头=%q：%s.%s 列表可见=%v 而调用 isError=%v：%s",
								path, tok, hdr, rt.Name, m.Name, visible, res.IsError, text(res))
						}
						if res.IsError && !strings.Contains(text(res), "（403") {
							t.Fatalf("%s %s：%s.%s 的拒绝应是权限类：%s", path, tok, rt.Name, m.Name, text(res))
						}
					}
				}
			}
		}
	}
}

func TestMCPReadonly_x组合路径404(t *testing.T) {
	srv := serve(t, Options{Auth: auth, Registry: readonlyRegistry()})
	for _, p := range []string{"/mcp/readonly/x/canvas", "/mcp/readonly/x/generation", "/mcp/readonly/x/nope"} {
		for _, h := range []http.Header{nil, {"Authorization": {"Bearer all"}}} {
			resp := rawPost(t, srv.URL+p, h)
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(b), domain.HeaderToolsets) {
				t.Fatalf("%s：%d %s，期望 404 并说明出路", p, resp.StatusCode, b)
			}
		}
	}
}

func TestMCPReadonly_服务端不声明listChanged(t *testing.T) {
	srv := serve(t, Options{Auth: auth, Registry: readonlyRegistry()})
	for _, p := range []string{"/mcp", "/mcp/readonly", "/mcp/x/canvas"} {
		for _, v := range []string{v2026, v2025} {
			cs, _, err := connect(t, srv.URL+p, v, "all", nil)
			if err != nil {
				t.Fatal(err)
			}
			caps := cs.InitializeResult().Capabilities
			if caps == nil || caps.Tools == nil {
				t.Fatalf("%s %s：没有声明 tools 能力", p, v)
			}
			// 工具列表按请求过滤、会话中途不会变；声明 listChanged 等于邀请客户端期待动态工具（MCP-F-91）。
			if caps.Tools.ListChanged {
				t.Fatalf("%s %s：声明了 tools.listChanged", p, v)
			}
		}
	}
}
