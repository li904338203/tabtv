package transport

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// Authenticator 把请求里的凭据解析成 Principal（访问密钥 MCPT-03 / OAuth 令牌 MCPT-12）。
//
// ⚠️ 与用户端的 RequireUser **互不复用**：MCP 凭据与用户登录 JWT 是两个主体空间（MCP-F-06），
// 用户端 token 拿来调 MCP 必须是 401。
//
// 返回的错误若是 *httpx.BizError，按它的业务码回（401 / 403 各有含义）；其他错误一律 401。
type Authenticator interface {
	Authenticate(r *http.Request) (app.Principal, error)
}

// ErrNoCredential 是请求没带可识别的凭据。
var ErrNoCredential = errors.New("mcp: 缺少或无法识别的凭据")

// denyAll 是没有注入 Authenticator 时的兜底：**一律拒绝**。
// 同 collabhttp 的「拿不到上下文一律拒绝」：默认放行的话，总开关默认开着，
// 一个漏装了鉴权的端点就是对公网敞开的。
type denyAll struct{}

func (denyAll) Authenticate(*http.Request) (app.Principal, error) {
	return app.Principal{}, ErrNoCredential
}

// Resolver 是凭据解析（app.Resolver 满足）。
type Resolver interface {
	Resolve(ctx context.Context, bearer string) (app.Principal, error)
}

// KeyAuth 是真实的凭据中间件实现：取 `Authorization: Bearer …` 交给 Resolver。
// 每个请求都解析一次、不缓存（MT-4）。
type KeyAuth struct{ Resolver Resolver }

func (k KeyAuth) Authenticate(r *http.Request) (app.Principal, error) {
	raw := bearer(r)
	if raw == "" {
		return app.Principal{}, ErrNoCredential
	}
	return k.Resolver.Resolve(r.Context(), raw)
}

func bearer(r *http.Request) string {
	v := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(v) > len(p) && strings.EqualFold(v[:len(p)], p) {
		return strings.TrimSpace(v[len(p):])
	}
	return ""
}

// PrincipalFrom 取本次请求的调用主体（凭据中间件放进 ctx 的）。
func PrincipalFrom(ctx context.Context) (app.Principal, bool) { return app.PrincipalFrom(ctx) }

// challenge 生成 401 的质询头（RFC 6750 §3 + RFC 9728 §5.1）：带上**本端点**的受保护资源元数据地址，
// 支持 OAuth 的客户端据此找到授权服务器并发起授权。没带凭据时不写 error（RFC 6750：缺凭据不是
// invalid_token），带了但无效时写 error="invalid_token"。
type challenge func(r *http.Request, hadCredential bool) string

func defaultChallenge(*http.Request, bool) string {
	return `Bearer realm="seeaitv-mcp", error="invalid_token"`
}

// requirePrincipal 解析凭据并放进 ctx；失败直接回 HTTP 错误，不进 SDK
// （MCPT-01 Q7：SDK 客户端把 401 报成 Unauthorized，客户端据此走授权流程）。
//
// 401（凭据本身不行：缺失 / 不认识 / 撤销 / 过期 / 受众不对）带 WWW-Authenticate；
// 403（凭据没问题但人不行：账号封禁、已不是工作区成员）不带 —— 重新授权解决不了它。
func requirePrincipal(auth Authenticator, ch challenge, next http.Handler) http.Handler {
	if ch == nil {
		ch = defaultChallenge
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := auth.Authenticate(r)
		if err != nil {
			if clientGone(r) {
				return
			}
			var be *httpx.BizError
			if !errors.As(err, &be) {
				be = httpx.Errf(errcode.Unauthenticated, "MCP 凭据缺失或无效：请在请求头 Authorization: Bearer 中提供访问密钥或 OAuth 访问令牌")
			}
			if be.Code.HTTPStatus() == http.StatusUnauthorized {
				w.Header().Set("WWW-Authenticate", ch(r, bearer(r) != ""))
			}
			httpx.Fail(w, r, be)
			return
		}
		next.ServeHTTP(w, r.WithContext(app.WithPrincipal(r.Context(), p)))
	})
}

// clientGone 判断客户端是否已经断开（请求 ctx 已取消）。这时验凭据 / 查开关的查询会以 context canceled 失败，
// 按内部错误回 500 并记 ERROR 是误报：没有人在等这个响应，也没有任何东西坏了。
//
// MCPT-15 实测：Codex 0.162.0 结束 2025 会话时发 `DELETE /mcp` 后立刻断开，每个会话都在服务端留下一条
// 「请求处理失败 … cause="context canceled"」的 ERROR 与一行 500 请求日志 —— 生产上就是每个 Codex 会话一次假告警。
func clientGone(r *http.Request) bool {
	return errors.Is(r.Context().Err(), context.Canceled)
}
