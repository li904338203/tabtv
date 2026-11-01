package app

import (
	"context"
	"fmt"
	"slices"

	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
)

// 资源（resources，MCP-F-36）与提示词（prompts，MCP-F-37）的注册（MCPT-16）。
//
// ⚠️ 与工具同一个注册表、同一个可见性判定：资源归属某个工具集、只要 view 动作，按
// domain.Visible（工具集 ∩ scope ∩ 当前角色 ∩ 端点）过滤 —— 另写一份判定的话，「工具看不见、资源却读得到」
// 会在某个凭据组合下悄悄成立。
// ⚠️ 资源列表**不枚举具体对象**（resources/list 为空，只给模板）：枚举等于把整个工作区的项目 / 媒体
// 一次性塞给客户端，且分页与权限要再做一遍。客户端从工具结果里的 resource_link 拿到具体 URI。
// ⚠️ 资源不是 transport 层的东西：本包只定义「有哪些、怎么读」，SDK 适配在 transport。

// ResourceContents 是读出的资源内容（文本；媒体给签名直链与元信息的 JSON，不给字节）。
type ResourceContents struct {
	URI      string
	MIMEType string
	Text     string
}

// ResourceTemplate 是一类资源（URI 模板，RFC 6570 的最简子集 `{var}`）。
type ResourceTemplate struct {
	// Name 同时是审计里的 method（≤32 字符）。
	Name        string
	Title       string
	Description string
	URITemplate string
	MIMEType    string
	// Toolset 是归属的工具集：未开启时不可见、不可读。
	Toolset string
	// Read 按解析出的变量读资源。返回的错误若是业务错误，原样交给客户端。
	Read func(ctx context.Context, vars map[string]string) (ResourceContents, error)
}

// Match 按模板解析 URI（只认 `{var}` 占位，每个变量是一段不含 `/` 的非空串）。
func (t ResourceTemplate) Match(uri string) (map[string]string, bool) {
	return domain.MatchURITemplate(t.URITemplate, uri)
}

// AddResource 登记一类资源。注册期出错一律 panic（同 Register）。
func (r *Registry) AddResource(t ResourceTemplate) {
	fail := func(msg string) { panic(fmt.Sprintf("mcp 注册资源 %q：%s", t.Name, msg)) }
	if !toolNameRe.MatchString(t.Name) || len(t.Name) > 32 {
		fail("名字必须是 snake_case 且 ≤32（同时是审计里的 method）")
	}
	if !domain.IsToolset(t.Toolset) {
		fail("未知工具集 " + t.Toolset)
	}
	if t.Read == nil || t.URITemplate == "" || t.Description == "" {
		fail("缺 Read / URITemplate / Description")
	}
	for _, x := range r.resources {
		if x.Name == t.Name || x.URITemplate == t.URITemplate {
			fail("重名")
		}
	}
	r.resources = append(r.resources, t)
}

// Resources 返回全部资源模板（注册顺序）。
func (r *Registry) Resources() []ResourceTemplate { return slices.Clone(r.resources) }

// ResourceFor 找能解析 uri 的资源模板。
func (r *Registry) ResourceFor(uri string) (ResourceTemplate, map[string]string, bool) {
	for _, t := range r.resources {
		if vars, ok := t.Match(uri); ok {
			return t, vars, true
		}
	}
	return ResourceTemplate{}, nil, false
}

// AllowsResource 资源在本次请求里是否可见 / 可读：与工具的只读 method 同一个判定（domain.Visible）。
func (a Access) AllowsResource(t ResourceTemplate) (bool, domain.DenyReason) {
	return domain.Visible(domain.MethodSpec{Toolset: t.Toolset, Action: collab.ActionView}, a.endpoint(),
		a.Toolsets, a.Scopes, a.Role)
}

// PromptInfo 是 prompts/list 的一项。
type PromptInfo struct {
	Name        string
	Title       string
	Description string
}

// PromptBody 是 prompts/get 的结果：一条 user 消息的正文。
type PromptBody struct {
	Description string
	Text        string
}

// PromptSource 给出提示词（实现在 tools：从站内技能派生）。列表是**每次现查**的（运营在技能后台改了，
// 下一次 prompts/list 就变），所以不在注册期固定。
type PromptSource interface {
	List(ctx context.Context) ([]PromptInfo, error)
	// Get 取不到（不存在或已停用）时返回 ErrPromptNotFound。
	Get(ctx context.Context, name string) (PromptBody, error)
}

// ErrPromptNotFound 是提示词不存在（或技能已停用）。
var ErrPromptNotFound = fmt.Errorf("提示词不存在或已停用")

// SetPrompts 登记提示词来源（只能一个）。
func (r *Registry) SetPrompts(ps PromptSource) {
	if r.prompts != nil {
		panic("mcp 注册提示词：重复登记")
	}
	r.prompts = ps
}

// Prompts 返回提示词来源（没有时 nil）。
func (r *Registry) Prompts() PromptSource { return r.prompts }
