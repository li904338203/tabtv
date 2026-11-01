package domain

import (
	"bytes"
	"crypto/rand"
	"reflect"
	"strings"
	"testing"

	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
)

// 4 scope × 4 角色 × 4 动作：有效权限 = scope ∩ 当前角色（MCP-F-11）。
// 期望值**不从 Role.Can 推导**：两边都调同一个函数的话，矩阵写错了测试照样绿。
func TestAllowed_scope与角色取交集(t *testing.T) {
	roleCan := map[collab.Role][]collab.Action{
		collab.RoleOwner:  {collab.ActionView, collab.ActionEdit, collab.ActionGenerate, collab.ActionExport},
		collab.RoleAdmin:  {collab.ActionView, collab.ActionEdit, collab.ActionGenerate, collab.ActionExport},
		collab.RoleEditor: {collab.ActionView, collab.ActionEdit, collab.ActionGenerate, collab.ActionExport},
		collab.RoleViewer: {collab.ActionView},
	}
	actionScope := map[collab.Action]string{
		collab.ActionView: ScopeView, collab.ActionEdit: ScopeEdit,
		collab.ActionGenerate: ScopeGenerate, collab.ActionExport: ScopeExport,
	}
	for _, granted := range Scopes() { // 凭据只有一个 scope
		for role, can := range roleCan {
			for action, need := range actionScope {
				want := granted == need && contains(can, action)
				if got := Allowed([]string{granted}, role, action); got != want {
					t.Errorf("scope=%s role=%s action=%s：%v，期望 %v", granted, role, action, got, want)
				}
			}
		}
	}
	// 全部 scope + viewer：只有 view。
	for action := range actionScope {
		if got := Allowed(Scopes(), collab.RoleViewer, action); got != (action == collab.ActionView) {
			t.Errorf("viewer 全 scope %s：%v", action, got)
		}
	}
	// manage / transfer 没有 scope：owner 全 scope 也不行；未知角色一律不行。
	for _, a := range []collab.Action{collab.ActionManage, collab.ActionTransfer} {
		if Allowed(Scopes(), collab.RoleOwner, a) {
			t.Errorf("%s 不该能通过 MCP 执行", a)
		}
	}
	if Allowed(Scopes(), "", collab.ActionView) || Allowed(Scopes(), "ghost", collab.ActionView) {
		t.Error("未知角色必须拒绝")
	}
	if Allowed(nil, collab.RoleOwner, collab.ActionView) {
		t.Error("没有 scope 必须拒绝")
	}
}

func contains(as []collab.Action, a collab.Action) bool {
	for _, x := range as {
		if x == a {
			return true
		}
	}
	return false
}

func TestMissingScopes_一次列全(t *testing.T) {
	// 凭据只有 edit（未经 ParseScopes 规范化的历史值），调 generate：两个都缺、一次列全。
	d, denied := MissingScopes([]string{ScopeEdit}, collab.RoleEditor, collab.ActionGenerate)
	if !denied {
		t.Fatal("应被拒")
	}
	if !reflect.DeepEqual(d.RequiredScopes, []string{ScopeView, ScopeGenerate}) ||
		!reflect.DeepEqual(d.MissingScopes, []string{ScopeView, ScopeGenerate}) || d.RoleInsufficient {
		t.Fatalf("缺 scope：%+v", d)
	}
	if !strings.Contains(d.Reason(), ScopeGenerate) || strings.Contains(d.Reason(), "管理员") {
		t.Fatalf("原因：%s", d.Reason())
	}

	// scope 够、角色不够：MissingScopes 为空，RoleInsufficient，原因指向管理员而不是重新授权。
	d, denied = MissingScopes(Scopes(), collab.RoleViewer, collab.ActionEdit)
	if !denied || len(d.MissingScopes) != 0 || !d.RoleInsufficient ||
		!reflect.DeepEqual(d.RequiredScopes, []string{ScopeView, ScopeEdit}) {
		t.Fatalf("角色不够：%+v", d)
	}
	if !strings.Contains(d.Reason(), "管理员") || !strings.Contains(d.Reason(), "viewer") {
		t.Fatalf("原因：%s", d.Reason())
	}

	// 两样都不够：两句都要说。
	d, _ = MissingScopes([]string{ScopeView}, collab.RoleViewer, collab.ActionExport)
	if !reflect.DeepEqual(d.MissingScopes, []string{ScopeExport}) || !d.RoleInsufficient {
		t.Fatalf("两样都不够：%+v", d)
	}
	if r := d.Reason(); !strings.Contains(r, ScopeExport) || !strings.Contains(r, "管理员") {
		t.Fatalf("原因：%s", r)
	}

	// 允许时不报。
	if _, denied := MissingScopes(Scopes(), collab.RoleEditor, collab.ActionGenerate); denied {
		t.Fatal("editor 全 scope 可以 generate")
	}
}

func TestParseScopes_补view去重定序(t *testing.T) {
	got, err := ParseScopes([]string{" seeaitv:generate", ScopeEdit, ScopeEdit, ""})
	if err != nil || !reflect.DeepEqual(got, []string{ScopeView, ScopeEdit, ScopeGenerate}) {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := ParseScopes([]string{"seeaitv:admin"}); err == nil || !strings.Contains(err.Error(), "seeaitv:admin") {
		t.Fatalf("未知 scope：%v", err)
	}
	if _, err := ParseScopes(nil); err == nil {
		t.Fatal("空 scope 应报错")
	}
	ts, err := ParseToolsets(nil)
	if err != nil || !reflect.DeepEqual(ts, DefaultToolsets()) {
		t.Fatalf("空工具集取默认：%v %v", ts, err)
	}
	if ts, _ := ParseToolsets([]string{ToolsetAgent, ToolsetCanvas, ToolsetCanvas}); !reflect.DeepEqual(ts, []string{ToolsetCanvas, ToolsetAgent}) {
		t.Fatalf("工具集定序去重：%v", ts)
	}
	if _, err := ParseToolsets([]string{"writing"}); err == nil {
		t.Fatal("未知工具集应报错")
	}
}

func TestParseKey_前缀与长度(t *testing.T) {
	for i := 0; i < 200; i++ { // 随机数高位为 0 时左补 0，长度必须恒定
		k, err := GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParseKey(k); err != nil {
			t.Fatalf("生成的密钥解析失败：%q", k)
		}
	}
	zero, _ := GenerateKey(bytes.NewReader(make([]byte, 32)))
	if zero != KeyPrefix+strings.Repeat("0", keyBodyLen) {
		t.Fatalf("全零随机数：%q", zero)
	}
	max, _ := GenerateKey(bytes.NewReader(bytes.Repeat([]byte{0xff}, 32)))
	if len(max) != keyLen {
		t.Fatalf("最大随机数长度：%d", len(max))
	}
	good, _ := GenerateKey(rand.Reader)
	for _, bad := range []string{
		"",
		KeyPrefix,
		good[:len(good)-1],                       // 短一位
		good + "0",                               // 长一位
		"sk_live_" + good[len("sk_mcp_"):] + "0", // 别家前缀
		KeyPrefix + strings.Repeat("-", keyBodyLen),
		strings.Replace(good, "sk_mcp_", "SK_MCP_", 1),
	} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("应拒绝：%q", bad)
		}
	}
	if p := DisplayPrefix(good); len(p) != DisplayPrefixLen || !strings.HasPrefix(good, KeyPrefix+p) {
		t.Fatalf("前缀：%q / %q", p, good)
	}
}

func TestParseKey_JWT形态直接拒绝(t *testing.T) {
	// 用户端 / 管理端 access token 都是 JWT（MCP-F-06）。
	for _, jwt := range []string{
		"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJl",
		"sk_mcp_eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sig1234",
	} {
		if _, err := ParseKey(jwt); err == nil {
			t.Errorf("JWT 不能当访问密钥：%q", jwt)
		}
	}
}

func TestHashKey_同一明文同一哈希(t *testing.T) {
	k, _ := GenerateKey(rand.Reader)
	k2, _ := GenerateKey(rand.Reader)
	if !bytes.Equal(HashKey(k), HashKey(k)) || len(HashKey(k)) != 32 {
		t.Fatal("同一明文必须同一哈希（按哈希唯一索引直查依赖这一点）")
	}
	if bytes.Equal(HashKey(k), HashKey(k2)) {
		t.Fatal("不同明文不同哈希")
	}
	if bytes.Contains(HashKey(k), []byte(k)) {
		t.Fatal("哈希里不能含明文")
	}
}
