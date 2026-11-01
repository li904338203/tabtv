package app

import (
	"context"
	"strings"

	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// Principal 是一次 MCP 调用的主体（开发 01 §4、MT-4）。
//
// ⚠️ **每个请求重新解析**（凭据中间件 → Resolver.Resolve），角色、付费主体、会员等级都是
// 那一刻实时查出来的，**不缓存**：缓存的话成员被移出或降级之后，他的密钥照样按原权限
// 用到缓存过期（MCP-F-11）。
type Principal struct {
	UserID      int64
	WorkspaceID int64
	// Role 是用户在该工作区的**当前**角色；有效权限 = Scopes ∩ Role（domain.Allowed）。
	Role           collab.Role
	PayerAccountID int64
	// MemberLevelID 只供工具里的模型门槛用，不限制 MCP 本身（MCP-F-15）。
	MemberLevelID int64
	// GrantID 是授权；撤销、审计、2025 会话绑定、画布节点归属都按它。
	GrantID    int64
	ClientName string
	// Scopes 是凭据授予的 scope（已规范化：含 view、固定顺序）。
	Scopes []string
	// Toolsets 是凭据授予的工具集；请求头 / 路径只能在它之内收窄。
	Toolsets []string
	// ReadOnly 表示本次请求命中只读端点（/mcp/readonly），由 transport 按端点填。
	ReadOnly bool
	// RequirePageConfirm 是授权上的「花钱一律走页面确认」开关（MCPT-10 起生效）。
	RequirePageConfirm bool
}

// Access 是本次请求实际可用的范围：有效工具集 + 是否只读端点 + scope 与当前角色。
// 由 transport 在鉴权之后算好放进 ctx。
type Access struct {
	Toolsets []string
	ReadOnly bool
	Scopes   []string
	Role     collab.Role
}

// Endpoint 是请求命中的端点：/mcp、/mcp/readonly 或 /mcp/x/{toolset}。
type Endpoint struct {
	Path     string
	ReadOnly bool
	Toolset  string // 仅 /mcp/x/{toolset}
}

type principalKey struct{}
type accessKey struct{}
type endpointKey struct{}

// WithEndpoint / EndpointFrom 在 ctx 上存取命中的端点（审计与只读规则要用）。
func WithEndpoint(ctx context.Context, e Endpoint) context.Context {
	return context.WithValue(ctx, endpointKey{}, e)
}

func EndpointFrom(ctx context.Context) (Endpoint, bool) {
	e, ok := ctx.Value(endpointKey{}).(Endpoint)
	return e, ok
}

// WithPrincipal / PrincipalFrom 在 ctx 上存取调用主体。
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// WithAccess / AccessFrom 在 ctx 上存取本次请求的可用范围。
func WithAccess(ctx context.Context, a Access) context.Context {
	return context.WithValue(ctx, accessKey{}, a)
}

// AccessFrom 取可用范围。⚠️ 取不到时返回**空范围**（什么都不可见），而不是全开 ——
// 中间件顺序哪天写反，结果应该是「工具列表是空的」而不是「所有工具都对外开放」。
func AccessFrom(ctx context.Context) Access {
	a, _ := ctx.Value(accessKey{}).(Access)
	return a
}

// endpoint 把 Access.ReadOnly 还原成端点种类（/mcp/x/{toolset} 的工具集已在 ResolveToolsets 里收窄，
// 对可见性而言与 /mcp 等价）。
func (a Access) endpoint() domain.Endpoint {
	if a.ReadOnly {
		return domain.EndpointReadOnly
	}
	return domain.EndpointFull
}

// Allows 是一个 method 在本次请求里的可见性结论：**直接调 domain.Visible**，
// tools/list 与 tools/call 都经过这里（MCPT-07：只写一处）。
func (a Access) Allows(t *Tool, m Method) (bool, domain.DenyReason) {
	return domain.Visible(m.Spec(t.Toolset), a.endpoint(), a.Toolsets, a.Scopes, a.Role)
}

// VisibleMethods 返回本次请求里可见的 method（保持注册顺序）。tools/list 据此收窄 method 枚举：
// 混合工具在只读端点下只剩读 method（MCP-F-14），viewer 看不到他调不动的写 method。
func (a Access) VisibleMethods(t *Tool) []Method {
	var out []Method
	for _, m := range t.Methods {
		if ok, _ := a.Allows(t, m); ok {
			out = append(out, m)
		}
	}
	return out
}

// Visible 判断工具在本次请求里是否可见（MCP-F-12）：至少有一个 method 可见。
func (a Access) Visible(t *Tool) bool { return len(a.VisibleMethods(t)) > 0 }

// Check 判断本次请求能否调用工具的某个 method；不能时给出**可操作**的原因
// （「需要开启 xx 工具集」比「无权限」有用：模型可以把它转述给用户）。
// 结论来自 Allows（与 tools/list 同一个函数），这里只负责把拒绝原因翻成错误。
func (a Access) Check(t *Tool, m Method) error {
	ok, why := a.Allows(t, m)
	if ok {
		return nil
	}
	name := t.Name
	if m.Name != "" {
		name += "." + m.Name
	}
	switch why {
	case domain.DenyReadonlyEndpoint:
		// ⚠️ 不提示所需 scope：只读端点上 scope 再多也没用，提示了模型会去引导用户重新授权，
		// 而那一步什么都改变不了。
		return httpx.Errf(errcode.PermissionDenied, "这是只读端点 /mcp/readonly，不允许调用 "+name+
			"（会写数据或花费积分）：请改用 /mcp").
			WithDetails(map[string]any{"reason": string(why), "tool": t.Name, "method": m.Name, "endpoint": "/mcp/readonly"})
	case domain.DenyToolsetDisabled:
		return httpx.Errf(errcode.PermissionDenied,
			"工具 "+t.Name+" 属于未开启的工具集 "+t.Toolset+
				"：需要凭据授予该工具集，并通过 /mcp/x/"+t.Toolset+" 或请求头 "+domain.HeaderToolsets+" 开启"+
				"（当前开启："+strings.Join(a.Toolsets, ", ")+"）").
			WithDetails(map[string]any{"reason": string(why), "toolset": t.Toolset})
	default:
		d, _ := domain.MissingScopes(a.Scopes, a.Role, m.Action)
		return InsufficientScope(name, d)
	}
}

// InsufficientScope 是越权的统一错误（MCP_INSUFFICIENT_SCOPE，403，MCP-F-13）：
// requiredScopes 一次列全，并分开「凭据缺 scope」与「角色不够」—— 两者的出路完全不同
// （前者重新授权，后者找管理员），混在一起用户会反复重新授权而每次都被拒。
func InsufficientScope(what string, d domain.Denial) error {
	missing := d.MissingScopes
	if missing == nil {
		missing = []string{}
	}
	return httpx.Errf(errcode.McpInsufficientScope, "无权调用 "+what+"："+d.Reason()).
		WithDetails(map[string]any{
			"requiredScopes":   d.RequiredScopes,
			"missingScopes":    missing,
			"roleInsufficient": d.RoleInsufficient,
			"role":             string(d.Role),
			"action":           string(d.Action),
			"reason":           string(domain.DenyInsufficientScope),
		})
}

type audienceKey struct{}

// WithAudience / AudienceFrom 在 ctx 上存取本次请求端点的**规范 URI**（OAuth 受众校验用，RFC 8707）。
// 由 MCP 端点在凭据中间件之前放进去；访问密钥不看它。
func WithAudience(ctx context.Context, aud string) context.Context {
	return context.WithValue(ctx, audienceKey{}, aud)
}

func AudienceFrom(ctx context.Context) (string, bool) {
	a, ok := ctx.Value(audienceKey{}).(string)
	return a, ok
}
