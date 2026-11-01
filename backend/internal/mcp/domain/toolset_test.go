package domain

import (
	"testing"

	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
)

// 可见性判定的表驱动测试（MCPT-07）：端点 × 工具集 × scope × 角色 × method 读写。

var (
	specRead   = MethodSpec{Toolset: ToolsetProjects, Action: collab.ActionView}
	specWrite  = MethodSpec{Toolset: ToolsetProjects, Action: collab.ActionEdit, Write: true}
	specPaid   = MethodSpec{Toolset: ToolsetGeneration, Action: collab.ActionGenerate, Paid: true, Write: true}
	specExport = MethodSpec{Toolset: ToolsetGeneration, Action: collab.ActionExport, Paid: true}
	// 声明了 edit 却忘了标 Write：仍按写处理（往不可见那一侧倒）。
	specSloppy = MethodSpec{Toolset: ToolsetProjects, Action: collab.ActionEdit}
)

func TestVisible_只读端点优先于scope(t *testing.T) {
	// 持有全部 scope 的 owner、工具集全开：只读端点下写与花钱 method 一律 readonly_endpoint。
	for _, m := range []MethodSpec{specWrite, specPaid, specExport, specSloppy} {
		ok, why := Visible(m, EndpointReadOnly, Toolsets(), Scopes(), collab.RoleOwner)
		if ok || why != DenyReadonlyEndpoint {
			t.Fatalf("%+v：%v %q，期望 readonly_endpoint", m, ok, why)
		}
	}
	if ok, why := Visible(specRead, EndpointReadOnly, Toolsets(), Scopes(), collab.RoleOwner); !ok || why != DenyNone {
		t.Fatalf("只读 method 在只读端点应可见：%v %q", ok, why)
	}
	// 同一组输入换到 /mcp 就可见：拒绝确实来自端点，不是别的条件。
	for _, m := range []MethodSpec{specWrite, specPaid, specExport} {
		if ok, why := Visible(m, EndpointFull, Toolsets(), Scopes(), collab.RoleOwner); !ok {
			t.Fatalf("%+v 在 /mcp 应可见：%q", m, why)
		}
	}
}

func TestVisible_只读端点优先于请求头工具集(t *testing.T) {
	// 请求头收窄成 generation（= ResolveToolsets 的结果）：只读端点下花钱 method 仍拒，
	// 且原因是只读端点而不是工具集 —— 优先级在工具集之前。
	sets, err := ResolveToolsets(Toolsets(), ToolsetGeneration, "")
	if err != nil {
		t.Fatal(err)
	}
	if ok, why := Visible(specPaid, EndpointReadOnly, sets, Scopes(), collab.RoleOwner); ok || why != DenyReadonlyEndpoint {
		t.Fatalf("请求头点名 generation 也不能放开花钱 method：%v %q", ok, why)
	}
	// 写 method 不在请求头的工具集里：仍然先报只读端点（scope 与工具集都救不了它）。
	if _, why := Visible(specWrite, EndpointReadOnly, sets, Scopes(), collab.RoleOwner); why != DenyReadonlyEndpoint {
		t.Fatalf("只读端点应先于工具集：%q", why)
	}
}

func TestVisible_工具集关闭时不可见(t *testing.T) {
	for _, ep := range []Endpoint{EndpointFull, EndpointToolset, EndpointReadOnly} {
		ok, why := Visible(specRead, ep, []string{ToolsetCanvas}, Scopes(), collab.RoleOwner)
		if ok || why != DenyToolsetDisabled {
			t.Fatalf("%s：%v %q，期望 toolset_disabled", ep, ok, why)
		}
	}
	// 空工具集（ctx 里取不到范围）：什么都不可见。
	if ok, _ := Visible(specRead, EndpointFull, nil, Scopes(), collab.RoleOwner); ok {
		t.Fatal("空工具集应不可见")
	}
}

func TestVisible_scope与角色取交集(t *testing.T) {
	cases := []struct {
		name   string
		m      MethodSpec
		scopes []string
		role   collab.Role
		want   bool
	}{
		{"全 scope + owner 写", specWrite, Scopes(), collab.RoleOwner, true},
		{"全 scope + viewer 写", specWrite, Scopes(), collab.RoleViewer, false},
		{"只有 view + owner 写", specWrite, []string{ScopeView}, collab.RoleOwner, false},
		{"只有 view + viewer 读", specRead, []string{ScopeView}, collab.RoleViewer, true},
		{"全 scope + editor 花钱", specPaid, Scopes(), collab.RoleEditor, collab.RoleEditor.Can(collab.ActionGenerate)},
		{"缺 generate + owner 花钱", specPaid, []string{ScopeView, ScopeEdit}, collab.RoleOwner, false},
		{"未知角色读", specRead, Scopes(), collab.Role(""), false},
	}
	for _, c := range cases {
		for _, ep := range []Endpoint{EndpointFull, EndpointToolset} {
			ok, why := Visible(c.m, ep, Toolsets(), c.scopes, c.role)
			if ok != c.want {
				t.Errorf("%s @%s：%v %q，期望 %v", c.name, ep, ok, why, c.want)
			}
			if !ok && why != DenyInsufficientScope {
				t.Errorf("%s @%s：原因 %q，期望 insufficient_scope", c.name, ep, why)
			}
			// 与 Allowed 一致：Visible 在工具集与端点都放行时就是 Allowed。
			if ok != Allowed(c.scopes, c.role, c.m.Action) {
				t.Errorf("%s：Visible 与 Allowed 不一致", c.name)
			}
		}
	}
}

func TestVisible_拒绝原因分类(t *testing.T) {
	// 三个条件同时不满足时，按固定顺序只报第一个。
	viewOnly := []string{ScopeView}
	cases := []struct {
		name string
		m    MethodSpec
		ep   Endpoint
		sets []string
		want DenyReason
	}{
		{"三者都不满足 → 只读端点", specPaid, EndpointReadOnly, []string{ToolsetCanvas}, DenyReadonlyEndpoint},
		{"工具集与 scope 都不满足 → 工具集", specPaid, EndpointFull, []string{ToolsetCanvas}, DenyToolsetDisabled},
		{"只有 scope 不满足", specPaid, EndpointFull, Toolsets(), DenyInsufficientScope},
		{"只读 method 在只读端点缺工具集", specRead, EndpointReadOnly, []string{ToolsetCanvas}, DenyToolsetDisabled},
		{"全部满足", specRead, EndpointReadOnly, Toolsets(), DenyNone},
	}
	for _, c := range cases {
		ok, why := Visible(c.m, c.ep, c.sets, viewOnly, collab.RoleViewer)
		if why != c.want || ok != (c.want == DenyNone) {
			t.Errorf("%s：%v %q，期望 %q", c.name, ok, why, c.want)
		}
	}
	// 取值与审计 / 错误 details 里的字符串逐字一致。
	for r, s := range map[DenyReason]string{DenyReadonlyEndpoint: "readonly_endpoint",
		DenyToolsetDisabled: "toolset_disabled", DenyInsufficientScope: "insufficient_scope"} {
		if string(r) != s {
			t.Errorf("%q != %q", r, s)
		}
	}
}
