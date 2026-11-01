package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

type fakeIn struct {
	ProjectID string `json:"projectId,omitempty" jsonschema:"项目 ID"`
}
type fakeOut struct {
	ID    string   `json:"id"`
	Items []string `json:"items"`
}

func fakeTool(name string, methods ...Method) Tool {
	return Define(Tool{Name: name, Toolset: domain.ToolsetProjects, Description: "测试工具", Methods: methods},
		func(_ context.Context, method string, in fakeIn) (fakeOut, string, error) {
			return fakeOut{ID: method + ":" + in.ProjectID}, "ok", nil
		})
}

var view = Method{Action: collab.ActionView}

func mustPanic(t *testing.T, want string, f func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("期望 panic（含 %q）", want)
		}
		if !strings.Contains(r.(string), want) {
			t.Fatalf("panic 文案 %q 不含 %q", r, want)
		}
	}()
	f()
}

func TestRegistry_重名注册panic(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeTool("dup", view))
	mustPanic(t, "重名", func() { r.Register(fakeTool("dup", view)) })
	// method 重名同样拒绝。
	mustPanic(t, "重名", func() {
		NewRegistry().Register(fakeTool("m", Method{Name: "get", Action: collab.ActionView},
			Method{Name: "get", Action: collab.ActionView}))
	})
}

func TestRegistry_method必须声明动作(t *testing.T) {
	cases := []struct {
		want string
		ms   []Method
	}{
		{"必须声明动作", []Method{{}}},
		{"没有对应 scope", []Method{{Action: collab.ActionManage}}},
		{"会写数据却只要 view", []Method{{Action: collab.ActionView, Write: true}}},
		{"花钱却不要 generate", []Method{{Action: collab.ActionEdit, Paid: true}}},
		{"都要有名字", []Method{{Name: "list", Action: collab.ActionView}, {Action: collab.ActionEdit, Write: true}}},
		{"至少要有一个 method", nil},
	}
	for _, c := range cases {
		mustPanic(t, c.want, func() { NewRegistry().Register(fakeTool("t", c.ms...)) })
	}
	mustPanic(t, "未知工具集", func() {
		tl := fakeTool("t", view)
		tl.Toolset = "writing"
		NewRegistry().Register(tl)
	})
	mustPanic(t, "Out 必须是结构体", func() {
		NewRegistry().Register(Define(Tool{Name: "t", Toolset: domain.ToolsetContext, Description: "x", Methods: []Method{view}},
			func(context.Context, string, fakeIn) ([]string, string, error) { return nil, "", nil }))
	})
}

func TestRegistry_注解由method推导(t *testing.T) {
	cases := []struct {
		name string
		ms   []Method
		want Annotations
	}{
		{"全只读", []Method{{Name: "list", Action: collab.ActionView}, {Name: "get", Action: collab.ActionView}},
			Annotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)}},
		{"含写不花钱", []Method{{Name: "list", Action: collab.ActionView}, {Name: "create", Action: collab.ActionEdit, Write: true}},
			Annotations{DestructiveHint: ptr(false), IdempotentHint: false, OpenWorldHint: ptr(false)}},
		{"含花钱", []Method{{Action: collab.ActionGenerate, Paid: true, Write: true}},
			Annotations{DestructiveHint: ptr(false), IdempotentHint: true, OpenWorldHint: ptr(true)}},
		{"花钱与普通写混合", []Method{{Name: "run", Action: collab.ActionGenerate, Paid: true}, {Name: "cancel", Action: collab.ActionEdit, Write: true}},
			Annotations{DestructiveHint: ptr(false), IdempotentHint: false, OpenWorldHint: ptr(true)}},
		{"破坏性", []Method{{Action: collab.ActionEdit, Write: true, Destructive: true}},
			Annotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(false)}},
		// AI 搭景（DRT-29）：不花钱、不破坏，但调用外部模型。
		{"不花钱但调用外部模型", []Method{{Action: collab.ActionEdit, Write: true, OpenWorld: true}},
			Annotations{DestructiveHint: ptr(false), IdempotentHint: false, OpenWorldHint: ptr(true)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := NewRegistry()
			r.Register(fakeTool("t", c.ms...))
			tl, _ := r.Lookup("t")
			got, _ := json.Marshal(tl.Annotations())
			want, _ := json.Marshal(c.want)
			if string(got) != string(want) {
				t.Fatalf("得到 %s，期望 %s", got, want)
			}
		})
	}
}

func TestRegistry_schema与解码(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeTool("projects", Method{Name: "list", Action: collab.ActionView},
		Method{Name: "create", Action: collab.ActionEdit, Write: true}))
	r.Register(fakeTool("whoami", view))
	multi, _ := r.Lookup("projects")
	single, _ := r.Lookup("whoami")

	in, _ := json.Marshal(multi.InputSchema())
	if !strings.Contains(string(in), `"method":{"type":"string","description":"操作：list / create","enum":["list","create"]}`) ||
		!strings.Contains(string(in), `"required":["method"]`) {
		t.Fatalf("多 method 工具要注入 method 枚举并必填：%s", in)
	}
	if strings.HasPrefix(string(in), `{"type":"object","properties":{"projectId"`) {
		t.Fatalf("method 应排在属性首位：%s", in)
	}
	sin, _ := json.Marshal(single.InputSchema())
	if strings.Contains(string(sin), `"method"`) {
		t.Fatalf("单 method 工具不该有 method 参数：%s", sin)
	}
	out, _ := json.Marshal(multi.OutputSchema())
	if !strings.Contains(string(out), `"truncated":{"type":"boolean"`) || strings.Contains(string(out), `"required":["id","items","truncated"`) {
		t.Fatalf("出参要有可选的截断字段：%s", out)
	}

	call, err := multi.Decode(json.RawMessage(`{"method":"create","projectId":"42"}`))
	if err != nil || call.Method != "create" || call.Input.(*fakeIn).ProjectID != "42" {
		t.Fatalf("解码：%+v %v", call, err)
	}
	for _, bad := range []string{`{"method":"delete"}`, `{}`, `{"method":"list","extra":1}`, `{"method":"list","projectId":42}`, `[`} {
		_, err := multi.Decode(json.RawMessage(bad))
		var be *httpx.BizError
		if !errors.As(err, &be) || be.Code != errcode.InvalidArgument {
			t.Fatalf("%s 应为参数错误：%v", bad, err)
		}
	}
	if call, err := single.Decode(nil); err != nil || call.Method != "" {
		t.Fatalf("空入参：%+v %v", call, err)
	}
	if err := multi.ValidateOutput(json.RawMessage(`{"id":"1","items":[],"truncated":true,"truncationHint":"x"}`)); err != nil {
		t.Fatalf("带截断字段的结果应通过：%v", err)
	}
	if err := multi.ValidateOutput(json.RawMessage(`{"id":1,"items":[]}`)); err == nil {
		t.Fatal("类型不符应失败")
	}
}

func TestAccess_工具集与只读端点(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeTool("projects", Method{Name: "list", Action: collab.ActionView},
		Method{Name: "create", Action: collab.ActionEdit, Write: true}))
	tl, _ := r.Lookup("projects")
	list, _ := tl.Method("list")
	create, _ := tl.Method("create")

	if (Access{}).Visible(tl) {
		t.Fatal("空范围（ctx 里取不到）必须什么都不可见")
	}
	all := domain.Scopes()
	full := Access{Toolsets: []string{domain.ToolsetProjects}, Scopes: all, Role: collab.RoleOwner}
	if !full.Visible(tl) || full.Check(tl, create) != nil {
		t.Fatal("开启工具集后应可见可调")
	}
	ro := Access{Toolsets: []string{domain.ToolsetProjects}, ReadOnly: true, Scopes: all, Role: collab.RoleOwner}
	if !ro.Visible(tl) || ro.Check(tl, list) != nil {
		t.Fatal("只读端点下只读 method 可调")
	}
	if err := ro.Check(tl, create); err == nil || !strings.Contains(err.Error(), "projects.create") {
		t.Fatalf("只读端点不允许写：%v", err)
	}
	other := Access{Toolsets: []string{domain.ToolsetCanvas}, Scopes: all, Role: collab.RoleOwner}
	if err := other.Check(tl, list); err == nil || !strings.Contains(err.Error(), "/mcp/x/projects") {
		t.Fatalf("未开启工具集要说清怎么开：%v", err)
	}
}

// 有效权限 = scope ∩ 当前角色（MCP-F-11/12/13）：缺 scope 与角色不够分开报，requiredScopes 一次列全。
func TestAccess_scope与角色过滤(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeTool("projects", Method{Name: "list", Action: collab.ActionView},
		Method{Name: "create", Action: collab.ActionEdit, Write: true}))
	r.Register(fakeTool("render", Method{Action: collab.ActionExport, Write: true}))
	projects, _ := r.Lookup("projects")
	render, _ := r.Lookup("render")
	create, _ := projects.Method("create")
	exp, _ := render.Method("")
	sets := []string{domain.ToolsetProjects}

	viewer := Access{Toolsets: sets, Scopes: domain.Scopes(), Role: collab.RoleViewer}
	if !viewer.Visible(projects) {
		t.Fatal("viewer 仍能看到含只读 method 的工具")
	}
	assertDenied := func(err error, missing []string, roleBad bool) {
		t.Helper()
		var be *httpx.BizError
		if !errors.As(err, &be) || be.Code != errcode.McpInsufficientScope || be.Code.HTTPStatus() != 403 {
			t.Fatalf("应为 MCP_INSUFFICIENT_SCOPE：%v", err)
		}
		if got := be.Details["missingScopes"].([]string); !reflect.DeepEqual(got, missing) {
			t.Fatalf("missingScopes：%v，期望 %v", got, missing)
		}
		if be.Details["roleInsufficient"] != roleBad {
			t.Fatalf("roleInsufficient：%v", be.Details)
		}
		if len(be.Details["requiredScopes"].([]string)) == 0 {
			t.Fatal("requiredScopes 必须列出")
		}
	}
	assertDenied(viewer.Check(projects, create), []string{}, true)

	viewScope := Access{Toolsets: sets, Scopes: []string{domain.ScopeView}, Role: collab.RoleEditor}
	assertDenied(viewScope.Check(projects, create), []string{domain.ScopeEdit}, false)

	// 工具只有一个写 method 且角色不够：整个工具不可见。
	editorNoExport := Access{Toolsets: sets, Scopes: []string{domain.ScopeView}, Role: collab.RoleViewer}
	if editorNoExport.Visible(render) {
		t.Fatal("没有任何一个 method 调得动的工具不该出现在列表里")
	}
	assertDenied(editorNoExport.Check(render, exp), []string{domain.ScopeExport}, true)
}

func TestSnapshot_稳定且字段齐全(t *testing.T) {
	empty, err := Snapshot(NewRegistry())
	if err != nil || string(empty) != "[]\n" {
		t.Fatalf("空注册表应为 []：%q %v", empty, err)
	}
	build := func() []byte {
		r := NewRegistry()
		// 注册顺序不同，快照仍按名字排序。
		r.Register(fakeTool("whoami", view))
		r.Register(fakeTool("projects", Method{Name: "list", Action: collab.ActionView},
			Method{Name: "create", Action: collab.ActionEdit, Write: true}))
		b, err := Snapshot(r)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	a, b := build(), build()
	if string(a) != string(b) {
		t.Fatal("两次生成不一致")
	}
	s := string(a)
	if strings.Index(s, `"name": "projects"`) > strings.Index(s, `"name": "whoami"`) {
		t.Fatal("应按工具名排序")
	}
	for _, want := range []string{`"toolset": "projects"`, `"action": "edit"`, `"scope": "seeaitv:edit"`,
		`"write": true`, `"readOnly": true`, `"readOnly": false`, `"readOnlyHint": true`, `"inputSchema"`, `"outputSchema"`, `"truncationHint"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("快照缺 %s：\n%s", want, s)
		}
	}
	// 属性顺序跟结构体字段走，method 排第一。
	if strings.Index(s, `"method"`) > strings.Index(s, `"projectId"`) {
		t.Fatal("method 应排在 projectId 之前")
	}
	if !json.Valid(a) {
		t.Fatal("快照不是合法 JSON")
	}
}
