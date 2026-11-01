package transport

import (
	"context"
	"net/http"
	"strconv"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 每个请求的 Principal / Access 怎么送到工具 handler 里。
//
// ⚠️ 2025 会话模式下，SDK 处理消息用的 context **来自建立会话的那个请求**，不是本次 POST 的。
// 只把 Principal 放进 HTTP 请求的 ctx 的话，会话里后续每一次 tools/list、tools/call 看到的都是
// initialize 那一刻的身份与权限 —— 成员被降级、凭据被换，**同一个会话里毫无变化**
// （MCPT-03 实测：初始化时是 owner，之后改成 viewer，花钱工具照样列出、照样受理）。
// 这直接违反 MCP-F-11「下一次调用即失效」。
//
// SDK 唯一按请求透传到消息上的是 RequestExtra（TokenInfo + Header）。TokenInfo 只能由
// sdkauth.RequireBearerToken 放进请求 ctx（键未导出），所以在凭据中间件已经解析好 Principal 之后，
// 再套一层 RequireBearerToken：它的 verifier 不再校验任何东西，只把本次请求的 Principal / Access
// 装进 TokenInfo.Extra；接收中间件 requestScope 再从消息的 Extra 里取出来覆盖 ctx。
//
// 附带收获：TokenInfo.UserID 填 grant ID，SDK 自带的「会话主体一致」检查（session user mismatch）
// 也按授权生效，是 sessionBinder 之外的第二道闸。
const (
	extraPrincipal = "seeaitv.mcp.principal"
	extraAccess    = "seeaitv.mcp.access"
	extraEndpoint  = "seeaitv.mcp.endpoint"
	// extraTraceID：本次 HTTP 请求的 traceId。会话里不覆盖的话，错误里回给客户端的 traceId 与
	// 调用审计的 trace_id 都是 initialize 那一次的（MCPT-06）。
	extraTraceID = "seeaitv.mcp.traceId"
)

// carryToSDK 把本次请求已解析好的 Principal / Access / Endpoint 交给 SDK 按消息透传。
func carryToSDK(next http.Handler) http.Handler {
	verifier := func(_ context.Context, _ string, r *http.Request) (*sdkauth.TokenInfo, error) {
		ctx := r.Context()
		p, ok := app.PrincipalFrom(ctx)
		if !ok {
			return nil, sdkauth.ErrInvalidToken // 凭据中间件之后不会发生；发生了就拒绝
		}
		e, _ := app.EndpointFrom(ctx)
		return &sdkauth.TokenInfo{
			Scopes: p.Scopes,
			UserID: "grant:" + strconv.FormatInt(p.GrantID, 10),
			Extra: map[string]any{extraPrincipal: p, extraAccess: app.AccessFrom(ctx), extraEndpoint: e,
				extraTraceID: httpx.TraceIDFrom(ctx)},
		}, nil
	}
	// 凭据的过期已由 Resolver 判过（且永久密钥本来就没有过期时间），这里不再要求 Expiration。
	return sdkauth.RequireBearerToken(verifier, &sdkauth.RequireBearerTokenOptions{AllowMissingExpiration: true})(next)
}

// requestScope 是第一个接收中间件：用**本条消息所属请求**的 Principal / Access 覆盖 ctx。
//
// ⚠️ 取不到时**清空**（空 Access = 什么都不可见、什么都调不了），不沿用 ctx 里原有的 ——
// 原有的正是那份可能过期的会话初始身份。
func requestScope(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		var (
			p   app.Principal
			a   app.Access
			e   app.Endpoint
			tid string
			ok  bool
		)
		if ex := req.GetExtra(); ex != nil && ex.TokenInfo != nil {
			p, ok = ex.TokenInfo.Extra[extraPrincipal].(app.Principal)
			a, _ = ex.TokenInfo.Extra[extraAccess].(app.Access)
			e, _ = ex.TokenInfo.Extra[extraEndpoint].(app.Endpoint)
			tid, _ = ex.TokenInfo.Extra[extraTraceID].(string)
		}
		if !ok {
			p, a, e = app.Principal{}, app.Access{}, app.Endpoint{}
		}
		ctx = app.WithEndpoint(app.WithAccess(app.WithPrincipal(ctx, p), a), e)
		if tid != "" {
			ctx = httpx.WithTraceID(ctx, tid)
		}
		return next(ctx, method, req)
	}
}
