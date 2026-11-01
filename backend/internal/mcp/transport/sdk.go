package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/platform/buildinfo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// DefaultCallTimeout 是单次 tools/call 的截止时间（开发 01 §12 R20 ③）。
//
// ⚠️ `/mcp` 整体跳过了 API 的全局 30 秒超时：那个外壳会在流中途写 504，客户端拿不到结果
// 而 handler 继续空跑（MCPT-01 Q4 实测）。上限改由这里按「一次工具调用」给 ——
// GET SSE 与 subscriptions/listen 是合法的长连接，不该被它截断。`tasks.wait` 自限 50 秒，留出余量。
const DefaultCallTimeout = 60 * time.Second

// newServer 把注册表登记进**一个**全量的 *mcp.Server。
//
// 按请求的差异（工具集、只读端点）用接收中间件过滤，而不是每请求建 Server：
// 后者约 2 ms/次且随工具数增长，前者 0.4 ms（MCPT-01 Q6）。两个 StreamableHTTPHandler
// （2026 无状态 / 2025 会话）共用这一个 Server —— 工具定义只有一份。
func newServer(reg *app.Registry, maxTokens int, callTimeout time.Duration, calls CallRecorder,
	logger *slog.Logger) *mcp.Server {
	caps := &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}}
	// 资源与提示词（MCPT-16）：由接收中间件 serveExtras 直接作答（列表按请求过滤、提示词每次现查），
	// 不登记进 SDK 的静态表 —— 能力要显式声明（SDK 只在登记过东西时才自动带上），listChanged=false。
	if len(reg.Resources()) > 0 {
		caps.Resources = &mcp.ResourceCapabilities{}
	}
	if reg.Prompts() != nil {
		caps.Prompts = &mcp.PromptCapabilities{}
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "seeaitv", Title: "seeAitv", Version: buildinfo.Get().Version},
		&mcp.ServerOptions{
			Logger: sdkLogger(logger),
			// 工具列表按请求过滤、不会在会话中途变化，listChanged=false。
			// 显式声明是因为工具为空时 SDK 不会自动带上 tools 能力（MCPT-02 起注册表本就是空的）。
			Capabilities: caps,
		})
	for _, t := range reg.List() {
		s.AddTool(sdkTool(t), toolHandler(t, maxTokens, logger))
	}
	// requestScope 必须排第一：后面的审计、过滤与工具 handler 都要看到**本次请求**的身份与权限。
	// clientHello 只记日志（MCPT-15 兼容矩阵的「声明值」）。auditCalls 紧随其后：每次 tools/call（含越权被拒、工具不存在、超时）都在这一处记审计。
	s.AddReceivingMiddleware(requestScope, clientHello(logger), auditCalls(calls), filterTools(reg), callDeadline(callTimeout, logger),
		serveExtras(reg, logger))
	return s
}

func sdkTool(t *app.Tool) *mcp.Tool {
	return &mcp.Tool{
		Name:         t.Name,
		Description:  t.Description,
		InputSchema:  t.InputSchema(),
		OutputSchema: t.OutputSchema(),
		Annotations:  sdkAnnotations(t.Annotations()),
	}
}

func sdkAnnotations(a app.Annotations) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{
		ReadOnlyHint:    a.ReadOnlyHint,
		DestructiveHint: a.DestructiveHint,
		IdempotentHint:  a.IdempotentHint,
		OpenWorldHint:   a.OpenWorldHint,
	}
}

// toolHandler 是注册表工具到 SDK 的适配：校验入参 → 可用范围 → 执行 → 截断 → 校验出参 → 信封。
func toolHandler(t *app.Tool, maxTokens int, logger *slog.Logger) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		access := app.AccessFrom(ctx)
		// ⚠️ 闸门先于参数校验：只读端点 / 未开启的工具集 / 越权的调用，参数写成什么样都不会被放行，
		// 先报参数错误只会让模型去反复改参数。点名的 method 认不出来时，若整个工具都不可见也直接拒。
		if m, ok := t.Method(t.PeekMethod(req.Params.Arguments)); ok {
			if err := access.Check(t, m); err != nil {
				return errorResult(ctx, logger, t.Name, err), nil
			}
		} else if !access.Visible(t) {
			return errorResult(ctx, logger, t.Name, access.Check(t, t.Methods[0])), nil
		}
		call, err := t.Decode(req.Params.Arguments)
		if err != nil {
			return errorResult(ctx, logger, t.Name, err), nil
		}
		m, ok := t.Method(call.Method)
		if !ok { // inputSchema 的枚举已挡住，这里只是兜底
			return errorResult(ctx, logger, t.Name, httpx.Errf(errcode.InvalidArgument, "未知 method："+call.Method)), nil
		}
		// ⚠️ 隐藏的工具照样可能被点名调用（客户端缓存了旧列表、或者直接手写请求）：
		// 过滤 tools/list 只是体验，这里才是闸门。
		if err := access.Check(t, m); err != nil {
			return errorResult(ctx, logger, t.Name, err), nil
		}
		ctx = app.WithElicitation(ctx, elicitContext(req))
		if pf := progressFunc(req, logger); pf != nil { // MCPT-11：tasks.wait 的心跳
			ctx = app.WithProgress(ctx, pf)
		}
		out, err := t.Handler(ctx, call)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil {
				d, _ := ctx.Deadline()
				err = httpx.Errf(errcode.Internal, fmt.Sprintf("工具 %s 超过单次调用上限（截止 %s）被中止，可稍后重试或缩小范围",
					t.Name, d.UTC().Format(time.RFC3339)))
			}
			return errorResult(ctx, logger, t.Name, err), nil
		}
		if out.Elicit != nil {
			return elicitResult(ctx, logger, t.Name, out.Elicit), nil
		}
		raw, truncated := domain.Truncate(out.Data, maxTokens)
		if raw == nil {
			return errorResult(ctx, logger, t.Name, fmt.Errorf("序列化工具 %s 的结果失败", t.Name)), nil
		}
		// 出参与声明对不上是代码缺陷：按协议错误返回（与 SDK 泛型 AddTool 的行为一致），并记日志。
		if err := t.ValidateOutput(raw); err != nil {
			logger.ErrorContext(ctx, "MCP 工具结果不符合 outputSchema", "tool", t.Name, "error", err,
				"traceId", httpx.TraceIDFrom(ctx))
			return nil, fmt.Errorf("工具 %s 的结果不符合 outputSchema：%w", t.Name, err)
		}
		summary := out.Summary
		if summary == "" {
			summary = t.Name + " 完成"
		}
		if truncated {
			summary += "（结果过长已截断，见 truncationHint）"
		}
		// ⚠️ 文本里必须同时带完整 JSON：opencode 等客户端不读 structuredContent，
		// 只给一句摘要的话模型拿不到任何字段（MCPT-01 Q8）。
		content := []mcp.Content{&mcp.TextContent{Text: summary}, &mcp.TextContent{Text: string(raw)}}
		for _, img := range out.Images { // MCPT-04：只有小图（缩略图）会走这里，视频音频给直链
			content = append(content, &mcp.ImageContent{Data: img.Data, MIMEType: img.MIMEType})
		}
		for _, l := range out.Links { // MCPT-16：资源以 resource_link 返回，客户端用 resources/read 读
			content = append(content, &mcp.ResourceLink{URI: l.URI, Name: l.Name, Title: l.Title, MIMEType: l.MIMEType})
		}
		return &mcp.CallToolResult{StructuredContent: json.RawMessage(raw), Content: content}, nil
	}
}

// progressFunc 在客户端给了 progressToken 时返回发进度通知的函数（MCPT-11，MCP-F-52）；否则 nil ——
// 协议规定没有 token 不得发进度通知。发送失败只记 Debug：进度是提示不是结果，客户端断开由 ctx 收尾。
func progressFunc(req *mcp.CallToolRequest, logger *slog.Logger) app.ProgressFunc {
	if req.Session == nil || req.Params == nil {
		return nil
	}
	tok := req.Params.GetProgressToken()
	if tok == nil {
		return nil
	}
	ss := req.Session
	return func(ctx context.Context, progress, total float64, message string) {
		if err := ss.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: tok, Progress: progress,
			Total: total, Message: message}); err != nil {
			logger.DebugContext(ctx, "MCP 进度通知发送失败", "error", err)
		}
	}
}

// elicitContext 读出本次调用的 elicitation 上下文（MCPT-10）：客户端是否声明了 form 模式的 elicitation、
// 上一轮的 requestState、用户对确认表单的回答。
//
// 2025 会话的声明来自 initialize；2026 无状态请求来自每请求 _meta —— SDK 都放在 InitializeParams 里（MCPT-01）。
// elicitation 能力为 `{}`（没写 form / url）时按规范视同 form。
func elicitContext(req *mcp.CallToolRequest) app.ElicitContext {
	var e app.ElicitContext
	if req.Session != nil {
		if ip := req.Session.InitializeParams(); ip != nil && ip.Capabilities != nil && ip.Capabilities.Elicitation != nil {
			el := ip.Capabilities.Elicitation
			e.Supported = el.Form != nil || el.URL == nil
			e.URLSupported = el.URL != nil // MCPT-13：页面审批走 url 模式
		}
	}
	if req.Params == nil {
		return e
	}
	e.State = req.Params.RequestState
	if r, ok := req.Params.InputResponses[app.ElicitInputID].(*mcp.ElicitResult); ok && r != nil {
		e.Answer = &app.ElicitAnswer{Action: r.Action, Content: r.Content}
	}
	return e
}

// elicitResult 把工具的确认表单翻成 input_required 结果：InputRequests + 签名的 RequestState，**不带内容**
// （协议规定两者互斥，带了 SDK 判为 server bug）。2026 客户端拿到 input_required 后带回答重调；
// 2025 会话由 SDK 的 MRTR 中间件代发 elicitation/create 并重入本 handler（MCPT-01、R20 ②）。
// 工具只在 ElicitContext.Supported 时才会要表单；万一不支持还要了，是代码缺陷，按内部错误返回。
//
// url 模式（页面审批，MCPT-13）：只在客户端声明了 url 模式时发；elicitationId 是 quoteId。
func elicitResult(ctx context.Context, logger *slog.Logger, tool string, er *app.ElicitRequest) *mcp.CallToolResult {
	el := app.ElicitationFrom(ctx)
	if er.URL != "" {
		if !el.URLSupported {
			return errorResult(ctx, logger, tool, fmt.Errorf("工具 %s 在客户端未声明 url 模式 elicitation 时要求了审批页", tool))
		}
		return &mcp.CallToolResult{
			InputRequests: mcp.InputRequestMap{app.ElicitInputID: &mcp.ElicitParams{
				Mode: "url", Message: er.Message, URL: er.URL, ElicitationID: er.ElicitationID,
			}},
			RequestState: er.State,
		}
	}
	if !el.Supported {
		return errorResult(ctx, logger, tool, fmt.Errorf("工具 %s 在客户端未声明 elicitation 时要求了表单", tool))
	}
	return &mcp.CallToolResult{
		InputRequests: mcp.InputRequestMap{app.ElicitInputID: &mcp.ElicitParams{
			Mode: "form", Message: er.Message, RequestedSchema: er.Schema,
		}},
		RequestState: er.State,
	}
}

// toolError 是工具失败时文本里给出的结构（MCP-I-06：业务码 + 可读原因）。
type toolError struct {
	Code    int            `json:"code"`
	Name    string         `json:"name"`
	Message string         `json:"message"`
	TraceID string         `json:"traceId,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

// errorResult 把错误包成 isError 结果（模型看得到原因，能转述给用户或换个参数重试）。
// 非业务错误按内部错误处理且**不回显原文**（同 httpx.Fail：底层错误常含 SQL 片段与连接串）。
// ⚠️ 不设 structuredContent：它要符合 outputSchema，而错误不是那个形状。
func errorResult(ctx context.Context, logger *slog.Logger, tool string, err error) *mcp.CallToolResult {
	var be *httpx.BizError
	if !errors.As(err, &be) {
		be = httpx.Wrap(errcode.Internal, err)
	}
	tid := httpx.TraceIDFrom(ctx)
	app.ReportErrorCode(ctx, int(be.Code)) // 调用审计按它分类（denied / error，见 auditCalls）
	if be.Code.HTTPStatus() >= 500 {
		attrs := []any{"tool", tool, "traceId", tid, "code", int(be.Code), "error", be.Error()}
		if cause := errors.Unwrap(be); cause != nil {
			attrs = append(attrs, "cause", cause.Error())
		}
		logger.ErrorContext(ctx, "MCP 工具调用失败", attrs...)
	}
	te := toolError{Code: int(be.Code), Name: be.Code.Name(), Message: be.Error(), TraceID: tid, Details: be.Details}
	b, _ := json.Marshal(map[string]any{"error": te})
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{
			&mcp.TextContent{Text: fmt.Sprintf("失败（%d %s）：%s", te.Code, te.Name, te.Message)},
			&mcp.TextContent{Text: string(b)},
		},
	}
}

// filterTools 按本次请求的可用范围过滤 tools/list（单 Server + 接收中间件，MCPT-01 Q6）。
func filterTools(reg *app.Registry) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if err != nil || method != "tools/list" {
				return res, err
			}
			lr, ok := res.(*mcp.ListToolsResult)
			if !ok {
				return res, err
			}
			access := app.AccessFrom(ctx)
			kept := make([]*mcp.Tool, 0, len(lr.Tools))
			for _, st := range lr.Tools {
				t, ok := reg.Lookup(st.Name)
				if !ok {
					continue
				}
				// 与 tools/call 的闸门（toolHandler → Access.Check）同一个判定：Access.Allows → domain.Visible。
				ms := access.VisibleMethods(t)
				if len(ms) == 0 {
					continue // 一个 method 都调不动：整条隐藏（只读端点下全是写 / 花钱 method 的工具）
				}
				if len(ms) < len(t.Methods) {
					st = narrowTool(st, t, ms)
				}
				kept = append(kept, st)
			}
			lr.Tools = kept
			return lr, nil
		}
	}
}

// narrowTool 返回只暴露 ms 这几个 method 的工具定义副本：method 枚举收窄、注解按剩下的 method 重新推导
// （混合工具在只读端点下就成了只读工具）。
//
// ⚠️ 必须是副本：lr.Tools 里的指针就是 Server 登记的那一份，原地改会把所有请求的工具定义一起改掉。
func narrowTool(st *mcp.Tool, t *app.Tool, ms []app.Method) *mcp.Tool {
	cp := *st
	cp.InputSchema = t.InputSchemaFor(ms)
	cp.Annotations = sdkAnnotations(app.AnnotationsFor(ms))
	return &cp
}

// callDeadline 给 tools/call 加截止时间；其他方法（initialize、listen……）不受影响。
//
// ⚠️ 它包在 SDK 的 MRTR 中间件**外面**（SDK 在 NewServer 时先登记，AddReceivingMiddleware 后登记的在外层），
// 所以 2025 会话里 SDK 代发 elicitation/create、等用户作答的时间**也算在这个上限里**。MCPT-15 实测
// （Claude Code 2.1.283 经 mcp-bridge 走 2025-11-25）：用户 70 秒才答，调用在 60 秒处以 JSON-RPC 错误
// 「multi-round-trip: fulfilling input request "confirm": context deadline exceeded」结束 —— 英文、
// 不说有没有扣费。这里把「截止时间到了的协议错误」翻成工具错误（isError + 中文原因），模型能转述、能决定重调。
// 上限本身是否要为 elicitation 放宽是另一个决定（见 MCPT-15 记录），这里只管说清楚。2026 不受影响：
// 那里的确认是两次独立请求（input_required → 带回答重调），作答时间不在任何一次调用里。
func callDeadline(d time.Duration, logger *slog.Logger) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" || d <= 0 {
				return next(ctx, method, req)
			}
			ctx, cancel := context.WithTimeout(ctx, d)
			defer cancel()
			res, err := next(ctx, method, req)
			if err == nil || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return res, err
			}
			name := ""
			if cr, ok := req.(*mcp.CallToolRequest); ok && cr.Params != nil {
				name = cr.Params.Name
			}
			return errorResult(ctx, logger, name, httpx.Errf(errcode.Internal, deadlineMessage(name, d, err))), nil
		}
	}
}

// deadlineMessage 是截止时间到了时给模型看的原因。等的是用户作答（SDK 代发确认表单）时，确认之前不会受理，
// 所以能明确说「没有扣费」；其余情形（极少见）不下这个结论。
//
// ⚠️ 按 SDK 错误文本里的 "input request" 认出确认表单 —— SDK 没有导出这个错误类型。
// TestCallDeadline_等确认表单超时给可读错误 用真实 SDK 钉住它：SDK 升级改了措辞，那条测试会红。
func deadlineMessage(tool string, d time.Duration, err error) string {
	if strings.Contains(err.Error(), "input request") {
		return fmt.Sprintf("等待用户回答确认表单超过单次调用上限（%s），本次调用已中止，没有扣费。"+
			"告诉用户这次确认已失效；用户仍想继续的话再调用一次 %s，会重新弹出确认表单", d, tool)
	}
	return fmt.Sprintf("工具 %s 超过单次调用上限（%s）被中止，可稍后重试或缩小范围；花钱的操作重试前先用 tasks 查一下是否已受理", tool, d)
}

// sdkLogger 只把 SDK 的 Warn 及以上交给业务日志：SDK 每个请求都打 Info（会话连上 / 断开），
// 2026 无状态下等于每次调用两行，会把真正的问题淹没。
func sdkLogger(l *slog.Logger) *slog.Logger { return slog.New(minLevel{l.Handler(), slog.LevelWarn}) }

type minLevel struct {
	slog.Handler
	min slog.Level
}

func (m minLevel) Enabled(ctx context.Context, lv slog.Level) bool {
	return lv >= m.min && m.Handler.Enabled(ctx, lv)
}

func (m minLevel) WithAttrs(as []slog.Attr) slog.Handler {
	return minLevel{m.Handler.WithAttrs(as), m.min}
}

func (m minLevel) WithGroup(n string) slog.Handler { return minLevel{m.Handler.WithGroup(n), m.min} }
