package transport

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// serveExtras 作答资源（resources/list、resources/templates/list、resources/read）与提示词
// （prompts/list、prompts/get）（MCPT-16，MCP-F-36 / 37）。
//
// 不登记进 SDK 的静态表，而是在接收中间件里直接作答：
//   - 资源要按**本次请求**的可见性过滤（与工具同一个 domain.Visible，经 app.Access.AllowsResource）——
//     静态表做不到按请求过滤；
//   - 提示词来自站内技能，运营随时会改、会停用，静态表在进程启动那一刻就过期了。
//
// ⚠️ 读资源是一次真实的数据读取：权限不够 / 工具集没开 → 拒绝（JSON-RPC 错误带业务码，审计记 denied）；
// 别的工作区的对象与不存在的一样回 resource not found（不区分，否则就是探测接口）。
func serveExtras(reg *app.Registry, logger *slog.Logger) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			switch method {
			case "resources/list":
				return &mcp.ListResourcesResult{Resources: []*mcp.Resource{}}, nil
			case "resources/templates/list":
				return listTemplates(ctx, reg), nil
			case "resources/read":
				rr, _ := req.(*mcp.ReadResourceRequest)
				if rr == nil || rr.Params == nil {
					return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "缺少 uri"}
				}
				return readResource(ctx, reg, rr.Params.URI, logger)
			case "prompts/list":
				return listPrompts(ctx, reg, logger)
			case "prompts/get":
				gr, _ := req.(*mcp.GetPromptRequest)
				if gr == nil || gr.Params == nil {
					return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "缺少 name"}
				}
				return getPrompt(ctx, reg, gr.Params.Name, logger)
			}
			return next(ctx, method, req)
		}
	}
}

func listTemplates(ctx context.Context, reg *app.Registry) *mcp.ListResourceTemplatesResult {
	access := app.AccessFrom(ctx)
	out := &mcp.ListResourceTemplatesResult{ResourceTemplates: []*mcp.ResourceTemplate{}}
	for _, t := range reg.Resources() {
		if ok, _ := access.AllowsResource(t); !ok {
			continue
		}
		out.ResourceTemplates = append(out.ResourceTemplates, &mcp.ResourceTemplate{Name: t.Name, Title: t.Title,
			Description: t.Description, URITemplate: t.URITemplate, MIMEType: t.MIMEType})
	}
	return out
}

func readResource(ctx context.Context, reg *app.Registry, uri string, logger *slog.Logger) (mcp.Result, error) {
	t, vars, ok := reg.ResourceFor(uri)
	if !ok {
		return nil, mcp.ResourceNotFoundError(uri)
	}
	app.ReportResourceKind(ctx, t.Name)
	access := app.AccessFrom(ctx)
	if ok, why := access.AllowsResource(t); !ok {
		be := httpx.Errf(errcode.PermissionDenied, "无权读取资源 "+uri+"：它属于工具集 "+t.Toolset+
			"（需要该工具集已授予并开启，且有 view 权限）").WithDetails(map[string]any{"reason": string(why),
			"toolset": t.Toolset})
		return nil, rpcError(ctx, be)
	}
	c, err := t.Read(ctx, vars)
	if err != nil {
		var be *httpx.BizError
		if errors.As(err, &be) && be.Code == errcode.NotFound {
			app.ReportErrorCode(ctx, int(be.Code))
			return nil, mcp.ResourceNotFoundError(uri)
		}
		if !errors.As(err, &be) {
			logger.ErrorContext(ctx, "MCP 读资源失败", "uri", uri, "error", err, "traceId", httpx.TraceIDFrom(ctx))
			be = httpx.Wrap(errcode.Internal, err)
		}
		return nil, rpcError(ctx, be)
	}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: c.URI, MIMEType: c.MIMEType,
		Text: c.Text}}}, nil
}

// rpcError 把业务错误翻成 JSON-RPC 错误：message 是可读原因，data 里带业务码（与工具错误同形）。
// 非业务错误不回显原文（同 errorResult）。
func rpcError(ctx context.Context, be *httpx.BizError) error {
	app.ReportErrorCode(ctx, int(be.Code))
	code := int64(jsonrpc.CodeInvalidParams)
	if be.Code.HTTPStatus() >= 500 {
		code = jsonrpc.CodeInternalError
	}
	data, _ := json.Marshal(toolError{Code: int(be.Code), Name: be.Code.Name(), Message: be.Error(),
		TraceID: httpx.TraceIDFrom(ctx), Details: be.Details})
	return &jsonrpc.Error{Code: code, Message: be.Error(), Data: data}
}

func listPrompts(ctx context.Context, reg *app.Registry, logger *slog.Logger) (mcp.Result, error) {
	out := &mcp.ListPromptsResult{Prompts: []*mcp.Prompt{}}
	ps := reg.Prompts()
	if ps == nil {
		return out, nil
	}
	list, err := ps.List(ctx)
	if err != nil {
		logger.ErrorContext(ctx, "MCP 列提示词失败", "error", err, "traceId", httpx.TraceIDFrom(ctx))
		return nil, rpcError(ctx, httpx.Wrap(errcode.Internal, err))
	}
	for _, p := range list {
		out.Prompts = append(out.Prompts, &mcp.Prompt{Name: p.Name, Title: p.Title, Description: p.Description})
	}
	return out, nil
}

func getPrompt(ctx context.Context, reg *app.Registry, name string, logger *slog.Logger) (mcp.Result, error) {
	ps := reg.Prompts()
	if ps == nil {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "提示词不存在：" + name}
	}
	b, err := ps.Get(ctx, name)
	if errors.Is(err, app.ErrPromptNotFound) {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "提示词不存在或已停用：" + name}
	}
	if err != nil {
		logger.ErrorContext(ctx, "MCP 取提示词失败", "name", name, "error", err, "traceId", httpx.TraceIDFrom(ctx))
		return nil, rpcError(ctx, httpx.Wrap(errcode.Internal, err))
	}
	return &mcp.GetPromptResult{Description: b.Description, Messages: []*mcp.PromptMessage{{Role: "user",
		Content: &mcp.TextContent{Text: b.Text}}}}, nil
}
