// Package domain 是 MCP 域的纯计算部分：工具集选择、scope 映射、结果截断。
// 不碰数据库、不发网络、不认识 HTTP —— 每条规则都能单独做表驱动测试。
package domain

import (
	"fmt"
	"slices"
	"strings"

	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
)

// 工具集（MCP-F-20）。顺序即对外展示与快照里的顺序，**不按字典序**。
const (
	ToolsetContext    = "context"
	ToolsetProjects   = "projects"
	ToolsetCanvas     = "canvas"
	ToolsetCatalog    = "catalog"
	ToolsetGeneration = "generation"
	ToolsetScript     = "script"
	ToolsetWorkflow   = "workflow"
	ToolsetTimeline   = "timeline"
	ToolsetAgent      = "agent"
)

// toolsetDefs 是工具集的唯一定义：名字 + 是否默认开启。
var toolsetDefs = []struct {
	name        string
	defaultOn   bool
	description string
}{
	{ToolsetContext, true, "当前身份与凭据"},
	{ToolsetProjects, true, "项目"},
	{ToolsetCanvas, true, "画布与项目文档"},
	{ToolsetCatalog, true, "模型与图片工具目录"},
	{ToolsetGeneration, true, "报价、生成、任务与媒体"},
	{ToolsetScript, false, "脚本线"},
	{ToolsetWorkflow, false, "整图运行"},
	{ToolsetTimeline, false, "时间线与成片"},
	{ToolsetAgent, false, "委托站内 Agent"},
}

// HeaderToolsets 是收窄工具集的请求头（MCP-F-20）。
const HeaderToolsets = "X-MCP-Toolsets"

// Toolsets 返回全部工具集名（定义顺序）。
func Toolsets() []string {
	out := make([]string, 0, len(toolsetDefs))
	for _, d := range toolsetDefs {
		out = append(out, d.name)
	}
	return out
}

// DefaultToolsets 返回默认开启的工具集（定义顺序）。
func DefaultToolsets() []string {
	var out []string
	for _, d := range toolsetDefs {
		if d.defaultOn {
			out = append(out, d.name)
		}
	}
	return out
}

// IsToolset 判断是否为已知工具集名。
func IsToolset(name string) bool {
	for _, d := range toolsetDefs {
		if d.name == name {
			return true
		}
	}
	return false
}

// UnknownToolsetError 是请求里点名了不存在的工具集。对外是参数错误：
// 静默忽略的话，用户拼错一个字母就会拿到一个比预期更小的工具列表，而他不知道为什么。
type UnknownToolsetError struct {
	Source string // "path" | "header"
	Names  []string
}

func (e *UnknownToolsetError) Error() string {
	where := "请求头 " + HeaderToolsets
	if e.Source == "path" {
		where = "路径 /mcp/x/{toolset}"
	}
	return fmt.Sprintf("%s 里有未知的工具集：%s（可选：%s）",
		where, strings.Join(e.Names, ", "), strings.Join(Toolsets(), ", "))
}

// ResolveToolsets 计算本次请求的有效工具集：
//
//	有效 = 凭据工具集 ∩（路径 ＞ 请求头 ＞ 默认开启集）
//
// ⚠️ 请求头与路径**只能收窄、不能放大**凭据：用户在同意页 / 密钥上没勾的工具集，
// 客户端改一个请求头就拿到的话，那次勾选就形同虚设。
//
// grant 里的未知名**丢弃不报错**（那是库里存的历史值，工具集下线后仍可能留着，
// 不该让整个凭据失效）；header / path 里的未知名是本次请求的输入，报参数错误。
// 返回值按定义顺序排列、去重。
func ResolveToolsets(grant []string, header, path string) ([]string, error) {
	var asked []string
	switch {
	case strings.TrimSpace(path) != "":
		p := strings.TrimSpace(path)
		if !IsToolset(p) {
			return nil, &UnknownToolsetError{Source: "path", Names: []string{p}}
		}
		asked = []string{p}
	case strings.TrimSpace(header) != "":
		var unknown []string
		for _, part := range strings.Split(header, ",") {
			n := strings.TrimSpace(part)
			if n == "" {
				continue
			}
			if !IsToolset(n) {
				unknown = append(unknown, n)
				continue
			}
			asked = append(asked, n)
		}
		if len(unknown) > 0 {
			return nil, &UnknownToolsetError{Source: "header", Names: unknown}
		}
		if len(asked) == 0 { // 只有逗号与空白：等于没写
			asked = DefaultToolsets()
		}
	default:
		asked = DefaultToolsets()
	}
	granted := map[string]bool{}
	for _, g := range grant {
		granted[strings.TrimSpace(g)] = true
	}
	want := map[string]bool{}
	for _, a := range asked {
		want[a] = true
	}
	out := []string{}
	for _, d := range toolsetDefs {
		if granted[d.name] && want[d.name] {
			out = append(out, d.name)
		}
	}
	return out, nil
}

// Endpoint 是请求命中的 MCP 端点种类（MCP-F-14）。
type Endpoint string

const (
	// EndpointFull 是 /mcp。
	EndpointFull Endpoint = "full"
	// EndpointReadOnly 是 /mcp/readonly：写方法与花钱方法一律不可见、不可调用。
	EndpointReadOnly Endpoint = "readonly"
	// EndpointToolset 是 /mcp/x/{toolset}：工具集已由路径收窄（ResolveToolsets），其余规则同 /mcp。
	EndpointToolset Endpoint = "toolset"
)

// DenyReason 是可见性判定没过的原因，供错误文案与调用审计（result=denied）使用。
type DenyReason string

const (
	// DenyNone 表示可见。
	DenyNone DenyReason = ""
	// DenyReadonlyEndpoint：只读端点不允许写 / 花钱。⚠️ 重新授权**没用**（scope 再多也不行），
	// 出路只有换到 /mcp —— 所以这条原因的文案不提示所需 scope。
	DenyReadonlyEndpoint DenyReason = "readonly_endpoint"
	// DenyToolsetDisabled：工具所在工具集本次未开启（凭据没授予，或路径 / 请求头收窄掉了）。
	DenyToolsetDisabled DenyReason = "toolset_disabled"
	// DenyInsufficientScope：凭据 scope ∩ 当前角色做不了这个动作（细节见 MissingScopes）。
	DenyInsufficientScope DenyReason = "insufficient_scope"
)

// MethodSpec 是可见性判定需要的 method 事实（app.Method + 所属工具集）。
// domain 不认识注册表，调用方把这几个字段抄进来。
type MethodSpec struct {
	Toolset string
	Action  collab.Action
	Paid    bool
	Write   bool
}

// ReadOnly 是否只读：不写、不花钱、只要 view。
// ⚠️ 三个条件缺一不可：一个声明了 edit 却忘了标 Write 的 method 仍按写处理 —— 声明错了时
// 往「不可见」那一侧倒，而不是把只读端点开一个口子。
func (m MethodSpec) ReadOnly() bool { return !m.Write && !m.Paid && m.Action == collab.ActionView }

// Visible 是 method 在本次请求里可不可见 / 可不可调用的**唯一**判定（MCP-F-12、MCP-F-14）。
// tools/list（列表与 method 枚举）与 tools/call（闸门）都调它 —— 两处各写一份的话，
// 迟早出现「列表里没有但能调用」或反过来，而两边各自看起来都没错。
//
// 判定顺序固定：
//
//  1. **只读端点**：写或花钱 → readonly_endpoint。优先级最高，高于工具集、请求头与 scope ——
//     持有全部 scope 的 owner 连 /mcp/readonly 也一样只能读；
//  2. **工具集**：不在本次有效工具集里 → toolset_disabled；
//  3. **scope ∩ 当前角色**（Allowed）→ insufficient_scope。
func Visible(m MethodSpec, ep Endpoint, toolsets, scopes []string, role collab.Role) (bool, DenyReason) {
	if ep == EndpointReadOnly && !m.ReadOnly() {
		return false, DenyReadonlyEndpoint
	}
	if !slices.Contains(toolsets, m.Toolset) {
		return false, DenyToolsetDisabled
	}
	if !Allowed(scopes, role, m.Action) {
		return false, DenyInsufficientScope
	}
	return true, DenyNone
}
