package app

import "context"

// elicitation 的协议无关表示（MCPT-10，开发 01 §12 R20 ②）。
//
// 工具 handler 只认这里的类型，不认识 go-sdk：transport 在调用前把「客户端声明了 elicitation 没有、
// 上一轮的 requestState、用户的回答」放进 ctx，工具返回 ElicitRequest 时由 transport 翻成
// InputRequests + RequestState —— SDK 对 2026-07-28 客户端原样返回 input_required，对 2025 会话
// 由它自己代发 elicitation/create 并重入同一个 handler。**禁止在 handler 里直接 Session.Elicit**
// （2026 下报错，MCPT-01 实测）。

// ElicitInputID 是确认表单在 InputRequests / InputResponses 里的键。
const ElicitInputID = "confirm"

// ElicitConfirmField 是确认表单唯一的字段名（布尔，用户勾选「确认」）。
//
// ⚠️ 这是**给用户看的表单**里的字段，不是工具入参：工具入参里没有任何布尔确认参数（MCP-F-45）。
const ElicitConfirmField = "confirm"

// ElicitAnswer 是用户对确认表单的回答。
type ElicitAnswer struct {
	// Action：accept | decline | cancel。
	Action  string
	Content map[string]any
}

// Accepted 用户点了确认（accept 且表单里勾了 confirm）。
func (a ElicitAnswer) Accepted() bool {
	v, _ := a.Content[ElicitConfirmField].(bool)
	return a.Action == "accept" && v
}

// ElicitContext 是本次 tools/call 的 elicitation 上下文（transport 填）。
type ElicitContext struct {
	// Supported 客户端在 initialize（2025）/ 本次请求 _meta（2026）里声明了 form 模式的 elicitation。
	// 没声明时**不能**返回 InputRequests（协议错误），回落到基线 quoteId。
	Supported bool
	// URLSupported 客户端声明了 url 模式的 elicitation（MCPT-13 页面审批用）。只有声明了才能发 url 模式，
	// 否则审批链接放在基线 needsConfirmation.approvalUrl 里。
	URLSupported bool
	// State 是上一轮 input_required 的 requestState（客户端原样回传；未签名校验过，不可信）。
	State string
	// Answer 是用户的回答；nil 表示这不是一次「带回答的重试」。
	Answer *ElicitAnswer
}

// ElicitRequest 是工具要求用户确认时的表单（form 模式），或要用户打开的审批页（url 模式，MCPT-13）。
type ElicitRequest struct {
	Message string
	// Schema 是表单的 JSON Schema（只允许顶层字段）。url 模式为空。
	Schema any
	// URL 非空表示 url 模式：让用户在**我们自己的域名**上打开审批页（页面审批，MCP-F-44）。
	URL string
	// ElicitationID 是 url 模式的标识（取 quoteId）。
	ElicitationID string
	// State 是签了名的 requestState（绑定 quoteId 与过期时间，见 confirm_state.go）。
	State string
}

// ElicitCarrier 由需要发起 elicitation 的出参类型实现（Define 据此填 Output.Elicit）。
type ElicitCarrier interface {
	ElicitRequest() *ElicitRequest
}

type elicitKey struct{}

// WithElicitation / ElicitationFrom 在 ctx 上存取 elicitation 上下文。
func WithElicitation(ctx context.Context, e ElicitContext) context.Context {
	return context.WithValue(ctx, elicitKey{}, e)
}

func ElicitationFrom(ctx context.Context) ElicitContext {
	e, _ := ctx.Value(elicitKey{}).(ElicitContext)
	return e
}
