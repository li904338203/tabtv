// Package oauth 是对外 MCP 的 OAuth 2.1 授权服务器（MCPT-12，开发 01 §7.2 / §12 R5、MT-9）。
//
// 按 MCP 2026-07-28 授权规范：受保护资源元数据（RFC 9728）、授权服务器元数据（RFC 8414）、
// 授权码 + PKCE（S256 必选）、资源指示器与受众绑定（RFC 8707）、刷新令牌轮换、撤销（RFC 7009）；
// 客户端注册**优先 CIMD**（客户端元数据文档），DCR（RFC 7591）只作兼容。
//
// 签发的令牌落到 mcp_grant（source='oauth'）上，之后的权限（scope ∩ 当前角色）、限流、审计与访问密钥
// 走同一段代码（mcp/app.Resolver）。
//
// ⚠️ 授权端点**不读 cookie、不认用户 JWT**（开发 01 §12 R5）：用户端登录态是前端以 Bearer 头发送的，
// 浏览器跳到 /oauth/authorize 带不出身份。所以这里只校验客户端参数，把请求打包成签名句柄跳到用户端
// 同意页（/mcp/authorize?req=…，MCPT-13）；同意页以用户身份调 POST /api/v1/mcp/consent 拿回跳地址。
// 让 API 认 cookie 里的 JWT 的话就要另做 CSRF 防护，而且两种主体空间开始混用（MCP-F-06）。
//
// 端点由规范定义，**不进 OpenAPI**（开发 02 §4.1），说明见 api/mcp/README.md；
// 只有 /api/v1/mcp/consent 两个用户端接口在 OpenAPI 里。
package oauth

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/repo"
)

// Store 是 OAuth 用到的 MCP 自有表（mcp/repo.Repo 实现）。
type Store interface {
	GetClient(ctx context.Context, id int64) (repo.Client, error)
	GetClientByKey(ctx context.Context, key string) (repo.Client, error)
	UpsertCIMDClient(ctx context.Context, c repo.Client) (repo.Client, error)
	CreateDCRClient(ctx context.Context, c repo.Client) (repo.Client, error)

	UpsertOAuthGrant(ctx context.Context, g repo.Grant, clientID int64) (repo.Grant, bool, error)
	GetGrantByID(ctx context.Context, grantID int64) (repo.Grant, error)

	CreateAuthCode(ctx context.Context, c repo.AuthCode) error
	GetAuthCode(ctx context.Context, hash []byte) (repo.AuthCode, error)
	ConsumeAuthCode(ctx context.Context, hash []byte) (bool, error)

	FindToken(ctx context.Context, hash []byte) (repo.Token, error)
	IssueTokens(ctx context.Context, grantID, familyID int64, toks []repo.NewToken) error
	RotateRefresh(ctx context.Context, oldID, grantID, familyID, newRefreshID int64, toks []repo.NewToken) (bool, error)
	RevokeFamily(ctx context.Context, familyID int64) (int64, error)
	RevokeGrantTokens(ctx context.Context, grantID int64) (int64, error)
	RevokeToken(ctx context.Context, id int64) error
}

// RateLimiter 是 DCR 注册的按 IP 限流（mcp/app.Limiter.AllowFixed 满足）。
type RateLimiter interface {
	AllowFixed(ctx context.Context, parts []string, limit int, window time.Duration) app.RateDecision
}

// Options 装配 Server。
type Options struct {
	Store   Store
	Users   app.Users   // 账号状态（封禁的用户换不出令牌）
	Members app.Members // 同意时校验是所选工作区的 active 成员
	Audit   app.Auditor // 可为 nil
	NextID  func() int64
	// Fetcher 拉取 CIMD 文档（cmd/api 传 TransportFetcher：共享出网策略）。nil 时 CIMD 客户端一律拒。
	Fetcher Fetcher
	// Limiter 为 nil 时 DCR 不限流。
	Limiter RateLimiter
	// DCRPerHour 是同一 IP 每小时可注册的客户端数（设计建议值 20；0 = 不限）。
	DCRPerHour int
	// PublicURL 是 SEEAITV_MCP_PUBLIC_URL（/mcp 端点地址或其根）；为空时按请求推导（仅非 production）。
	PublicURL string
	// WebPublicURL 是用户端地址（SEEAITV_WEB_PUBLIC_URL）；为空时按请求主机名 + :3000 推导（仅非 production）。
	WebPublicURL string
	// RequestKey 是 req 句柄的 HMAC 子密钥（DeriveRequestKey）。必填。
	RequestKey []byte
	// UserIDs 从 ctx 取当前登录用户（身份域的 RequireUser 放进去的）。
	UserIDs func(ctx context.Context) (int64, bool)
	Logger  *slog.Logger
}

// Server 是授权服务器。
type Server struct {
	o    Options
	urls Endpoints
	web  string
	rand io.Reader
	now  func() time.Time
	log  *slog.Logger
}

// New 构造授权服务器。
func New(o Options) *Server {
	if o.Store == nil || o.NextID == nil || len(o.RequestKey) == 0 {
		panic("mcp oauth: Store / NextID / RequestKey 必填")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Server{o: o, urls: NewEndpoints(o.PublicURL), web: trimSlash(o.WebPublicURL),
		rand: rand.Reader, now: time.Now, log: o.Logger}
}

// SetClock 替换时钟（测试用）。
func (s *Server) SetClock(now func() time.Time) { s.now = now }

// Endpoints 返回地址计算器（MCP 端点的 WWW-Authenticate 与受众校验要用同一份）。
func (s *Server) Endpoints() Endpoints { return s.urls }

// MountRoot 挂根上的元数据与 /oauth/* 端点（不在 /api/v1 下）。
//
// ⚠️ 这些端点都给**非浏览器**的 MCP 客户端调（也有在浏览器里跑的，如 Inspector），所以带
// `Access-Control-Allow-Origin: *`：它们不读 cookie、不靠环境身份，放开来源没有 CSRF 面。
// 授权端点是浏览器整页跳转，不需要 CORS。
func (s *Server) MountRoot(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(cors)
		r.Get(wellKnownPR, s.protectedResource)
		r.Get(wellKnownPR+"/*", s.protectedResource)
		r.Get(wellKnownAS, s.authorizationServer)
		r.Get(wellKnownAS+"/*", s.authorizationServer)
		r.Post("/oauth/token", s.token)
		r.Post("/oauth/revoke", s.revoke)
		r.Post("/oauth/register", s.register)
		for _, p := range []string{wellKnownPR, wellKnownPR + "/*", wellKnownAS, wellKnownAS + "/*",
			"/oauth/token", "/oauth/revoke", "/oauth/register"} {
			r.Options(p, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
		}
	})
	r.Get("/oauth/authorize", s.authorize)
}

// MountConsent 挂 /mcp/consent（在 /api/v1 内，RequireUser；供用户端同意页 MCPT-13 调用）。
func (s *Server) MountConsent(r chi.Router, requireUser func(http.Handler) http.Handler) {
	r.With(requireUser).Get("/mcp/consent", s.consentInfo)
	r.With(requireUser).Post("/mcp/consent", s.consentSubmit)
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, MCP-Protocol-Version")
		h.Set("Access-Control-Max-Age", "600")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
