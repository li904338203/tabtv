// Package transport 是 MCP 的 HTTP 入口：挂载 /mcp、/mcp/readonly、/mcp/x/{toolset}，
// 在 go-sdk 外面做 Origin 校验、凭据、工具集解析，并按协议版本分发到两个 SDK handler。
//
// 端点由 MCP 规范定义，**不进 OpenAPI**（开发 02 §4.1），说明见 api/mcp/README.md。
package transport

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/mcp/oauth"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

const (
	// ProtocolStateless 是走无状态 handler 的最低协议版本（MCPT-01 Q1：有状态 handler 拒绝它）。
	ProtocolStateless = "2026-07-28"
	// HeaderProtocolVersion 是 2025-06-18 起每个后续请求都带的协议版本头。
	HeaderProtocolVersion = "Mcp-Protocol-Version"
	// MaxRequestBodyBytes：SDK 默认 4 MiB，5 MB 文件 base64 后约 6.7 MB 会 413（MCPT-01 Q7）。
	MaxRequestBodyBytes = 8 << 20
	// DefaultSessionTimeout 是 2025 会话的空闲回收时间。
	DefaultSessionTimeout = 30 * time.Minute
	// DefaultResultMaxTokens 是结果截断阈值的默认值（MCP-I-07）。
	DefaultResultMaxTokens = 6000
)

// Options 装配 Handler。
type Options struct {
	// Registry 是工具注册表（与快照同源：internal/mcp/tools.NewRegistry）。必填。
	Registry *app.Registry
	// Auth 为 nil 时一律 401（兜底：漏装鉴权的端点不能对外敞开）。cmd/api 传 KeyAuth。
	Auth Authenticator
	// PublicURL 是 MCP 对外地址（SEEAITV_MCP_PUBLIC_URL）；其 Origin 被视为同源。
	PublicURL string
	// ResultMaxTokens <= 0 时取 DefaultResultMaxTokens。
	ResultMaxTokens int
	// CallTimeout <= 0 时取 DefaultCallTimeout。
	CallTimeout time.Duration
	// SessionTimeout <= 0 时取 DefaultSessionTimeout。
	SessionTimeout time.Duration
	// Limiter 为 nil 时不限流（两级阈值见 app.Limiter，MCPT-06）。
	Limiter RateLimiter
	// Calls 为 nil 时不记调用审计（cmd/api 传 app.CallLogger，MCPT-06）。
	Calls CallRecorder
	// Switch 为 nil 时不做运行时开关判定（只有环境变量决定挂不挂）。cmd/api 传 app.Manage.Switch()（MCPT-14）。
	Switch SwitchChecker
	Logger *slog.Logger
}

// SwitchChecker 是两级运行时开关（MCPT-14）：平台总开关（mcp_setting）与工作区开关（workspace.mcp_disabled）。
// ⚠️ 每次请求都判、不缓存：关掉之后的下一次调用即生效（app.Switch 的注释）。
type SwitchChecker interface {
	// PlatformOpen 为 false 时整个端点 404（凭据之前判：关掉就是不存在，不必先验凭据）。
	PlatformOpen(ctx context.Context) bool
	// Check 在凭据之后判平台 + 工作区：nil 放行；否则 404 或 403 MCP_DISABLED。
	Check(ctx context.Context, workspaceID int64) error
}

// Handler 是 MCP 的 HTTP 入口。nil 表示总开关关闭：Mount 什么都不挂（三个端点 404）。
type Handler struct {
	auth         Authenticator
	publicOrigin string
	// urls 计算端点的规范 URI（OAuth 令牌受众）与受保护资源元数据地址（与授权服务器同一份，MCPT-12）。
	urls    oauth.Endpoints
	limiter RateLimiter
	calls   CallRecorder
	sw      SwitchChecker
	// stateless 服务 2026-07-28（无会话、可多实例）；stateful 服务 2025 系列（会话在进程内存）。
	stateless http.Handler
	stateful  http.Handler
}

// NewHandler 构造 Handler。两个 SDK handler 共用同一个 *mcp.Server（开发 01 §12 R20 ①）。
func NewHandler(o Options) *Handler {
	if o.Registry == nil {
		panic("mcp transport: Registry 必填")
	}
	if o.Auth == nil {
		o.Auth = denyAll{}
	}
	if o.ResultMaxTokens <= 0 {
		o.ResultMaxTokens = DefaultResultMaxTokens
	}
	if o.CallTimeout <= 0 {
		o.CallTimeout = DefaultCallTimeout
	}
	if o.SessionTimeout <= 0 {
		o.SessionTimeout = DefaultSessionTimeout
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	srv := newServer(o.Registry, o.ResultMaxTokens, o.CallTimeout, o.Calls, o.Logger)
	get := func(*http.Request) *mcp.Server { return srv }
	// ⚠️ 三个默认值是实测后改的（MCPT-01）：
	//   - DisableLocalhostProtection：SDK 默认在「回环地址收到非回环 Host」时 403，
	//     反向代理转发到 127.0.0.1 时会整片 403。DNS rebinding 防护改由 checkOrigin 做。
	//   - MaxRequestBodyBytes：见常量。
	//   - **不开 JSONResponse**：开了 2026 的进度通知会全部丢失。
	common := mcp.StreamableHTTPOptions{
		Logger:                     sdkLogger(o.Logger),
		DisableLocalhostProtection: true,
		MaxRequestBodyBytes:        MaxRequestBodyBytes,
	}
	sl := common
	sl.Stateless = true
	// 2026 的 POST 就是整个请求生命周期：客户端断开就取消 handler，别让它空跑（Q4）。
	sl.PropagateRequestCancellation = true
	sf := common
	sf.SessionTimeout = o.SessionTimeout
	return &Handler{
		auth:         o.Auth,
		publicOrigin: originOf(o.PublicURL),
		urls:         oauth.NewEndpoints(o.PublicURL),
		limiter:      o.Limiter,
		calls:        o.Calls,
		sw:           o.Switch,
		stateless:    mcp.NewStreamableHTTPHandler(get, &sl),
		// 2025 会话绑定建立它的授权：换一个凭据拿着同一个 Mcp-Session-Id 来一律 403（见 sessionBinder）。
		stateful: newSessionBinder(o.SessionTimeout).wrap(mcp.NewStreamableHTTPHandler(get, &sf)),
	}
}

// Mount 挂三个端点。h 为 nil（总开关关闭）时不挂 —— 404 而不是 403：关掉就是不存在。
//
// ⚠️ 这三个路由**整体跳过**全局 requestTimeout（cmd/api 的 noTimeoutRoutes），
// 单次调用的上限由 callDeadline 给；也不进 longRunningRoutes（那只放宽 POST、上限 15 分钟）。
func (h *Handler) Mount(r chi.Router) {
	if h == nil {
		return
	}
	r.Handle("/mcp", h.endpoint(func(*http.Request) app.Endpoint { return app.Endpoint{Path: "/mcp"} }))
	r.Handle("/mcp/readonly", h.endpoint(func(*http.Request) app.Endpoint {
		return app.Endpoint{Path: "/mcp/readonly", ReadOnly: true}
	}))
	// ⚠️ /mcp/readonly 不与 /x/{toolset} 组合（MCPT-07）：多一种组合就多一套要解释、要测的语义，
	// 而「只读 + 收窄工具集」用 /mcp/readonly 加请求头 X-MCP-Toolsets 已经能表达。
	// 显式挂一个 404（不交给路由器的默认 404）是为了把出路说清楚；不经过凭据 —— 不存在就是不存在。
	r.Handle("/mcp/readonly/*", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.Fail(w, r, httpx.Errf(errcode.NotFound, "不存在 "+r.URL.Path+"：只读端点不与工具集路径组合，"+
			"请用 /mcp/readonly 并以请求头 "+domain.HeaderToolsets+" 收窄工具集"))
	}))
	r.Handle("/mcp/x/{toolset}", h.endpoint(func(r *http.Request) app.Endpoint {
		ts := chi.URLParam(r, "toolset")
		return app.Endpoint{Path: "/mcp/x/" + ts, Toolset: ts}
	}))
}

// endpoint 组装单个端点：Origin → 路径工具集 → 凭据 → 限流 → 有效工具集 → 按版本分发。
func (h *Handler) endpoint(ep func(*http.Request) app.Endpoint) http.Handler {
	dispatch := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(HeaderProtocolVersion) >= ProtocolStateless {
			h.stateless.ServeHTTP(w, r)
			return
		}
		// 其余：无协议头的 initialize、2025-11-25 / 06-18、带 Mcp-Session-Id 的后续请求与 GET SSE。
		h.stateful.ServeHTTP(w, r)
	})
	// 本次请求的 Principal / Access 按消息透传给 SDK（2025 会话里 SDK 用的是会话初始 ctx，见 carry.go）。
	carried := carryToSDK(dispatch)
	withAccess := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e, _ := app.EndpointFrom(r.Context())
		p, _ := app.PrincipalFrom(r.Context())
		sets, err := domain.ResolveToolsets(p.Toolsets, r.Header.Get(domain.HeaderToolsets), e.Toolset)
		if err != nil {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, err.Error()))
			return
		}
		p.ReadOnly = e.ReadOnly
		ctx := app.WithPrincipal(r.Context(), p)
		ctx = app.WithAccess(ctx, app.Access{Toolsets: sets, ReadOnly: e.ReadOnly, Scopes: p.Scopes, Role: p.Role})
		carried.ServeHTTP(w, r.WithContext(ctx))
	})
	// 限流在凭据之后（要知道是谁）、进 SDK 之前（被限流的调用不该占用 SDK 与工具，开发 01 §7.3）。
	// 工作区开关在凭据之后、限流之前（dev/01 §3 ②）：被关掉的工作区的调用不该消耗限流额度。
	authed := requirePrincipal(h.auth, h.challenge, h.switchGate(h.rateLimit(withAccess)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 平台总开关（MCPT-14）：关着时 404，先于凭据 —— 关掉就是不存在。
		if h.sw != nil && !h.sw.PlatformOpen(r.Context()) {
			httpx.Fail(w, r, app.PlatformDisabled())
			return
		}
		if !h.originAllowed(r) {
			httpx.Fail(w, r, httpx.Errf(errcode.PermissionDenied, "不允许的 Origin："+r.Header.Get("Origin")))
			return
		}
		e := ep(r)
		// 路径里的工具集名先于凭据校验：它是公开信息（工具集清单本就在文档里），
		// 拼错时直接告诉调用方「没有这个工具集」比先让他去折腾凭据有用。
		if e.Toolset != "" && !domain.IsToolset(e.Toolset) {
			ue := &domain.UnknownToolsetError{Source: "path", Names: []string{e.Toolset}}
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, ue.Error()))
			return
		}
		// 受众（RFC 8707）：本端点的规范 URI。OAuth 访问令牌必须签发给它，访问密钥不看。
		ctx := app.WithAudience(app.WithEndpoint(r.Context(), e), h.urls.Resource(r, e.Path))
		authed.ServeHTTP(w, r.WithContext(ctx))
	})
}

// switchGate 在凭据之后判两级开关（平台关 → 404，工作区关 → 403 MCP_DISABLED）。
// 平台那一条在凭据之前已经判过一次，这里再判一次是为了覆盖「验凭据期间刚好被关掉」——
// 两次都是一条主键查询，代价可以忽略。关闭**不撤销** grant，重新打开即恢复。
func (h *Handler) switchGate(next http.Handler) http.Handler {
	if h.sw == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := app.PrincipalFrom(r.Context())
		if err := h.sw.Check(r.Context(), p.WorkspaceID); err != nil {
			if clientGone(r) {
				return
			}
			httpx.Fail(w, r, err)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// challenge 是 401 的 WWW-Authenticate：resource_metadata 指向**本次命中的端点**的元数据
// （/mcp/readonly 的 401 指向 …/oauth-protected-resource/mcp/readonly，其 resource 与该端点逐字一致）。
func (h *Handler) challenge(r *http.Request, hadCredential bool) string {
	path := r.URL.Path
	if e, ok := app.EndpointFrom(r.Context()); ok {
		path = e.Path
	}
	v := `Bearer resource_metadata="` + h.urls.ResourceMetadataURL(r, path) + `"`
	if hadCredential {
		v += `, error="invalid_token"`
	}
	return v
}

// originAllowed 是 DNS rebinding 防护（SDK 自带的已关闭，见 NewHandler）。
// 没有 Origin 的请求放行：非浏览器客户端（CLI、IDE 插件）本来就不带。
// 带了 Origin 的，只认同源（Origin 的 host 与请求 Host 一致）或配置的公网地址。
func (h *Handler) originAllowed(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	if h.publicOrigin != "" && strings.EqualFold(originOf(o), h.publicOrigin) {
		return true
	}
	u, err := url.Parse(o)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host)
}

// originOf 取 URL 的 scheme://host 部分；解析失败返回空串。
func originOf(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}
