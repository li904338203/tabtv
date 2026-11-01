package domain

import (
	"fmt"
	"slices"
	"strings"

	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
)

// MCP 凭据的 scope（设计方案 §3.2、MCP-F-10）。与工作区动作一一对应。
const (
	ScopeView     = "seeaitv:view"
	ScopeEdit     = "seeaitv:edit"
	ScopeGenerate = "seeaitv:generate"
	ScopeExport   = "seeaitv:export"
)

// Scopes 返回全部 scope（固定顺序）。
func Scopes() []string { return []string{ScopeView, ScopeEdit, ScopeGenerate, ScopeExport} }

// ScopeFor 返回某个工作区动作对应的 scope。
// ⚠️ manage / transfer **没有** scope：外部客户端不该能改成员、转让或归档工作区，
// 返回 false，注册表据此拒绝声明这类动作的工具。
func ScopeFor(a collab.Action) (string, bool) {
	switch a {
	case collab.ActionView:
		return ScopeView, true
	case collab.ActionEdit:
		return ScopeEdit, true
	case collab.ActionGenerate:
		return ScopeGenerate, true
	case collab.ActionExport:
		return ScopeExport, true
	}
	return "", false
}

// IsScope 判断是否为已知 scope。
func IsScope(s string) bool { return slices.Contains(Scopes(), s) }

// ParseScopes 校验并规范化凭据要授予的 scope：去空白、去重、按 Scopes() 的固定顺序排列，
// 并**自动补上 view**。
//
//   - 未知名报错（不静默丢弃：用户拼错一个字母就拿到一个比预期小的权限，而他不知道为什么）；
//   - 一个都没有报错（没有任何权限的凭据只会让人困惑）；
//   - 补 view：能写却不能读的凭据自相矛盾 —— 每个写工具都要先读到对象才能写（同共享权限集 CD-57）；
//   - 固定顺序：库里存的是 text[]，顺序不稳定的话同一组权限在审计 diff 里会显示成「变了」。
func ParseScopes(raw []string) ([]string, error) {
	want := map[string]bool{}
	var unknown []string
	for _, r := range raw {
		s := strings.TrimSpace(r)
		if s == "" {
			continue
		}
		if !IsScope(s) {
			unknown = append(unknown, s)
			continue
		}
		want[s] = true
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("未知的 scope：%s（可选：%s）", strings.Join(unknown, ", "), strings.Join(Scopes(), ", "))
	}
	if len(want) == 0 {
		return nil, fmt.Errorf("至少要授予一个 scope（可选：%s）", strings.Join(Scopes(), ", "))
	}
	want[ScopeView] = true
	out := make([]string, 0, len(want))
	for _, s := range Scopes() {
		if want[s] {
			out = append(out, s)
		}
	}
	return out, nil
}

// ParseToolsets 校验并规范化凭据要授予的工具集；为空时取默认开启集。未知名报错。
// 返回值按定义顺序排列、去重。
func ParseToolsets(raw []string) ([]string, error) {
	want := map[string]bool{}
	var unknown []string
	for _, r := range raw {
		n := strings.TrimSpace(r)
		if n == "" {
			continue
		}
		if !IsToolset(n) {
			unknown = append(unknown, n)
			continue
		}
		want[n] = true
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("未知的工具集：%s（可选：%s）", strings.Join(unknown, ", "), strings.Join(Toolsets(), ", "))
	}
	if len(want) == 0 {
		return DefaultToolsets(), nil
	}
	out := []string{}
	for _, n := range Toolsets() {
		if want[n] {
			out = append(out, n)
		}
	}
	return out, nil
}

// Allowed 是 MCP 的权限判定：**凭据 scope ∩ 当前角色**（MCP-F-11）。
//
// 角色判定直接用协作域的同一份矩阵 Role.Can（开发 01 §12 R3）：collaboration 的路由闸
// 绑定在 HTTP 请求的工作区上下文上、MCP 的工作区来自凭据，所以不复用中间件，但**矩阵只有一份**
// —— 抄一份的话两边迟早不一致，而那种不一致的表现是「网页上点不动的事，从 MCP 能做」。
// 未知角色、未知动作、manage/transfer（没有 scope）一律 false。
func Allowed(scopes []string, role collab.Role, a collab.Action) bool {
	s, ok := ScopeFor(a)
	if !ok {
		return false
	}
	return slices.Contains(scopes, s) && role.Can(a)
}

// RequiredScopes 是调用某个动作需要的**全部** scope：本动作的 scope + view。
// 一次列全（MCP-F-13）：只报「缺 generate」的话，客户端按它重新授权拿到一个只有 generate 的
// 凭据，下一次又因为缺 view 被拒 —— 用户被迫授权两次。ParseScopes 本就会补 view，这里与之一致。
func RequiredScopes(a collab.Action) []string {
	s, ok := ScopeFor(a)
	if !ok {
		return nil
	}
	if s == ScopeView {
		return []string{ScopeView}
	}
	return []string{ScopeView, s}
}

// Denial 说明一次权限判定为什么没过。两种原因要分开说（MCP-F-13）：
//   - 凭据缺 scope：用户可以重新授权 / 新建一把权限更大的密钥解决；
//   - 角色不够：重新授权**没用**，要找工作区管理员调整角色 —— 混为一谈的话，
//     用户会反复重新授权而每次都被拒。
type Denial struct {
	// RequiredScopes 是这个动作需要的全部 scope（一次列全）。
	RequiredScopes []string
	// MissingScopes 是凭据上缺的那几个；为空表示凭据够、是角色不够。
	MissingScopes []string
	// RoleInsufficient 表示用户在该工作区的当前角色做不了这个动作。
	RoleInsufficient bool
	Role             collab.Role
	Action           collab.Action
}

// MissingScopes 判定 scopes ∩ role 能否执行动作 a；能则返回 (Denial{}, false)。
func MissingScopes(scopes []string, role collab.Role, a collab.Action) (Denial, bool) {
	if Allowed(scopes, role, a) {
		return Denial{}, false
	}
	d := Denial{RequiredScopes: RequiredScopes(a), Role: role, Action: a}
	for _, s := range d.RequiredScopes {
		if !slices.Contains(scopes, s) {
			d.MissingScopes = append(d.MissingScopes, s)
		}
	}
	d.RoleInsufficient = !role.Can(a)
	if _, ok := ScopeFor(a); !ok {
		// manage / transfer：MCP 根本不提供（注册表已拒绝这类工具，这里只是兜底）。
		d.RoleInsufficient = false
	}
	return d, true
}

// Reason 是给模型 / 用户看的一句话：缺什么、该找谁。
func (d Denial) Reason() string {
	var parts []string
	if len(d.MissingScopes) > 0 {
		parts = append(parts, "凭据缺少 scope "+strings.Join(d.MissingScopes, ", ")+
			"：请在「账号设置 → MCP 访问」新建带这些权限的访问密钥（或重新授权）")
	}
	if d.RoleInsufficient {
		parts = append(parts, fmt.Sprintf("你在该工作区的当前角色（%s）不能执行 %s：重新授权无法解决，请联系工作区管理员调整角色",
			roleName(d.Role), d.Action))
	}
	if len(parts) == 0 {
		return "MCP 不提供该操作（" + string(d.Action) + "）"
	}
	return strings.Join(parts, "；")
}

func roleName(r collab.Role) string {
	if r == "" {
		return "无"
	}
	return string(r)
}
