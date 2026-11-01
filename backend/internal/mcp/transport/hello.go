package transport

import (
	"context"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// clientHello 记录客户端**声明**了什么（MCPT-15 兼容矩阵的「声明值」列）：客户端名与版本、协议版本、
// 是否声明 elicitation（form / url）、tools/call 是否带 progressToken（日志字段 wantsProgress）。只写日志不入库。
//
// ⚠️ 字段名不能含 token：生产日志经 logredact 按字段名打码，叫 progressToken 的话值一律变成 [REDACTED]
// （MCPT-15 实测踩到）。
//
// ⚠️ 「声明」≠「实际」：声明了 elicitation 的客户端界面里未必弹表单，这条日志只回答前者 ——
// 矩阵里两列分开记，就是因为只有这一侧能从服务端看到。
//
// 握手（2025 的 initialize、2026 的 server/discover）记 Info；tools/call 记 Debug（2026 无状态下每次调用
// 都带一份 _meta，按 Info 记等于每次调用多一行）。
func clientHello(logger *slog.Logger) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if logger == nil {
				return next(ctx, method, req)
			}
			if method == "initialize" || method == "server/discover" {
				// 握手处理完之后再取：会话里的握手参数由 SDK 在处理这次请求时写入。
				res, err := next(ctx, method, req)
				logger.InfoContext(ctx, "mcp.client_hello", helloAttrs(ctx, method, req)...)
				return res, err
			}
			if method == "tools/call" && logger.Enabled(ctx, slog.LevelDebug) {
				attrs := helloAttrs(ctx, method, req)
				if cr, ok := req.(*mcp.CallToolRequest); ok && cr.Params != nil {
					attrs = append(attrs, "tool", cr.Params.Name, "wantsProgress", cr.Params.GetProgressToken() != nil)
				}
				logger.DebugContext(ctx, "mcp.client_hello", attrs...)
			}
			return next(ctx, method, req)
		}
	}
}

// helloAttrs 取本次请求的握手参数：优先取会话（2025 来自 initialize、2026 无状态来自每请求 _meta），
// 取不到再看请求本身是不是 initialize。
func helloAttrs(ctx context.Context, method string, req mcp.Request) []any {
	var ip *mcp.InitializeParams
	if ss, ok := req.GetSession().(*mcp.ServerSession); ok && ss != nil {
		ip = ss.InitializeParams()
	}
	if r, ok := req.(*mcp.InitializeRequest); ip == nil && ok && r.Params != nil {
		ip = r.Params
	}
	attrs := []any{"method", method, "traceId", httpx.TraceIDFrom(ctx)}
	if ip == nil {
		return append(attrs, "clientInfo", "unknown")
	}
	name, ver := "", ""
	if ip.ClientInfo != nil {
		name, ver = ip.ClientInfo.Name, ip.ClientInfo.Version
	}
	form, url := false, false
	if ip.Capabilities != nil && ip.Capabilities.Elicitation != nil {
		el := ip.Capabilities.Elicitation
		form = el.Form != nil || el.URL == nil // `{}` 按规范视同 form（与 elicitContext 同一判定）
		url = el.URL != nil
	}
	return append(attrs, "client", name, "clientVersion", ver, "protocolVersion", ip.ProtocolVersion,
		"elicitationForm", form, "elicitationURL", url)
}
