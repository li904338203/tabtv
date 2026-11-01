package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// CallRecorder 收调用审计（app.CallLogger 满足）。Record 必须不阻塞。
type CallRecorder interface {
	Record(e app.CallEntry)
}

// auditCalls 是调用审计的**唯一**拦截点（另一个是限流中间件，记被限流、没进 SDK 的那些）：
// 每次 tools/call —— 成功、失败、越权被拒、停在「需确认」、工具不存在 —— 都在这里记一条。
// 工具自己不记：每个工具各记一行的话，漏掉的那一个没有编译错误（MCPT-06）。
//
// 必须排在 requestScope 之后（要本次请求的 Principal 与 traceId），在 filterTools / callDeadline
// 之前（耗时要含超时中止的那一段）。
func auditCalls(rec CallRecorder) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if rec == nil || (method != "tools/call" && method != "resources/read") {
				return next(ctx, method, req)
			}
			start := time.Now()
			ctx, out := app.WithCallOutcome(ctx)
			res, err := next(ctx, method, req)

			p, _ := app.PrincipalFrom(ctx)
			e := app.EntryFor(p)
			if cr, ok := req.(*mcp.CallToolRequest); ok && cr.Params != nil {
				e.Tool, e.Args = cr.Params.Name, cr.Params.Arguments
			}
			// 读资源同样进审计（MCPT-16）：tool 记 resources/read，method 记资源类别，参数指纹取 URI。
			if rr, ok := req.(*mcp.ReadResourceRequest); ok && rr.Params != nil {
				e.Tool, e.Method = "resources/read", out.ResourceKind()
				e.Args, _ = json.Marshal(map[string]string{"uri": rr.Params.URI})
			}
			e.Duration, e.TraceID, e.At = time.Since(start), httpx.TraceIDFrom(ctx), start
			credits, needsConfirm, code := out.Snapshot()
			e.Credits, e.ErrorCode = credits, code
			// 2025 会话里 elicitation 由 SDK 代发并在同一次调用里重入 handler：先回报了「需确认」、随后又受理扣了费，
			// 这一条按受理算（credits > 0 时不再是 needs_confirm）。
			e.Result = classify(res, err, needsConfirm && credits == 0, &e.ErrorCode)
			rec.Record(e)
			return res, err
		}
	}
}

// classify 把一次调用归成 ok | error | needs_confirm | denied | rate_limited。
func classify(res mcp.Result, err error, needsConfirm bool, code *int) string {
	if err != nil { // 协议错误：工具不存在、出参不符合 outputSchema、读资源失败……
		var be *httpx.BizError
		if errors.As(err, &be) {
			*code = int(be.Code)
		}
		// 读资源被权限拒绝（MCPT-16）以 JSON-RPC 错误返回，同样要记成 denied（越权尝试最需要留痕）。
		if *code != 0 && errcode.Code(*code).HTTPStatus() == http.StatusForbidden {
			return domain.CallDenied
		}
		return domain.CallError
	}
	if cr, ok := res.(*mcp.CallToolResult); ok && cr.IsError {
		c := errcode.Code(*code)
		switch {
		case c == errcode.RateLimited:
			return domain.CallRateLimited
		case *code != 0 && c.HTTPStatus() == http.StatusForbidden:
			return domain.CallDenied
		}
		return domain.CallError
	}
	if needsConfirm {
		return domain.CallNeedsConfirm
	}
	return domain.CallOK
}
