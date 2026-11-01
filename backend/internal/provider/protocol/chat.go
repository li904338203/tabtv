package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
)

// 多轮对话 + 工具调用的协议扩展（AGT-03，开发 01 §7.1）。
//
// ⚠️ 与 BuildSubmit/ParseSubmit **并存而不是替换**：脚本线的分镜/提示词合成走的是
// 单轮 Job，那条路的行为一个字都不能变。Chat 是另一组入口，只有能说「多轮 + 工具」的
// 协议才实现它（目前 openai_chat 与 anthropic_messages）。
//
// 这里同样是纯函数：不发网络、不碰数据库，重试与超时是传输层与 Provider 的事（AGT-04）。

// 消息角色。
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	// RoleTool 是工具结果。OpenAI 发成 role=tool 的消息；Anthropic 没有这个角色，
	// 要翻成 user 消息里的 tool_result 块。
	RoleTool = "tool"
)

// 结束原因的归一值。Agent 循环只按这四个判断「还要不要继续」。
const (
	FinishStop      = "stop"
	FinishToolCalls = "tool_calls"
	FinishLength    = "length"
	FinishError     = "error"
)

// ChatMessage 是一条对话消息。
type ChatMessage struct {
	Role    string
	Content string
	// ToolCalls 只在 assistant 消息上有意义：模型上一轮发起的工具调用，回填历史时原样带回。
	ToolCalls []ToolCall
	// ToolCallID 只在 tool 消息上有意义：这条结果回答的是哪一次调用。
	ToolCallID string
	// Images 只在 user 消息上有意义（URL 或 data: 形态）。
	Images []string
}

// ToolDef 是交给模型的工具定义。Parameters 是 JSON Schema。
type ToolDef struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// ToolCall 是模型发起的一次工具调用。
//
// ⚠️ Arguments 只在参数是**合法 JSON** 时才有值；不合法时为 nil、原文进 RawArguments。
// 协议层不因此报错 —— 「参数格式错」要交给 Agent 层回给模型让它改（开发 04 §4），
// 在这里失败的话整轮作废，模型连改正的机会都没有。
type ToolCall struct {
	ID           string
	Name         string
	Arguments    json.RawMessage
	RawArguments string
}

// ChatJob 是一次多轮对话调用。
type ChatJob struct {
	ModelCode     string
	UpstreamModel string
	// System 与 Messages 分开：两家放的位置不同（见 Job.System 的说明）。
	System   string
	Messages []ChatMessage
	Tools    []ToolDef
	// Options 透传（temperature / top_p / max_tokens …）。
	Options map[string]any
	// MaxTokensDefault 调用方建议的输出上限缺省值：只有**必须带**上限的协议（Anthropic 的 max_tokens）
	// 在 Options 没给 max_tokens 时用它；不必带的协议（OpenAI 系）忽略 ——
	// 往那边多塞一个 max_tokens 会改变调用方的行为，某些型号还会直接拒收这个参数。0 = 协议缺省。
	MaxTokensDefault int
	Stream           bool
}

// Usage 是本次调用的用量（上游报多少记多少，不估算）。
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// ChatResult 是一次对话调用的结果。
type ChatResult struct {
	Text      string
	ToolCalls []ToolCall
	// FinishReason 是归一值（stop / tool_calls / length / error）。
	FinishReason string
	// RawFinishReason 是上游原文：未知值归一成 stop，原文留给调用方写日志。
	RawFinishReason string
	Usage           Usage
}

// ChatProtocol 由能说「多轮 + 工具」的协议实现。
type ChatProtocol interface {
	BuildChat(job ChatJob) (Request, error)
	ParseChat(status int, body []byte) (ChatResult, error)
	// NewChatStream 返回一个新的流式解析器（每次调用一个，不可复用）。
	NewChatStream() ChatStreamParser
}

// ChatStreamParser 逐块解析流式响应。
//
// Feed 处理半包与 `\r\n`；文本增量回调 onDelta，工具调用参数在内部累积。
// done 为真表示流已结束（收到结束标记或错误事件），之后再 Feed 也只会原样返回。
// ⚠️ 流中的 error 事件**一定**以 err 返回 —— 当成空回复的话，用户看到的是模型「什么都没说」。
type ChatStreamParser interface {
	Feed(chunk []byte, onDelta func(string)) (done bool, err error)
	Result() ChatResult
}

// UpstreamError 是上游在 HTTP 200 或流里报的业务错误（中转常见）。
type UpstreamError struct {
	// Type 是上游给的错误类别（overloaded_error / server_error …），可能为空。
	Type    string
	Message string
}

func (e *UpstreamError) Error() string {
	if e.Type != "" {
		return fmt.Sprintf("上游返回错误 %s: %s", e.Type, e.Message)
	}
	return "上游返回错误: " + e.Message
}

// GetChat 按协议码取 Chat 实现。
func GetChat(code string) (ChatProtocol, bool) {
	p, ok := registry[code]
	if !ok {
		return nil, false
	}
	c, ok := p.(ChatProtocol)
	return c, ok
}

// IsChatCapable 判断协议能否用于多轮对话 + 工具。
//
// ⚠️ 由注册表派生（类型断言），**不另维护名单**（GAP-09）：目录域直接复用它。
func IsChatCapable(code string) bool {
	_, ok := GetChat(code)
	return ok
}

// ParseUpstreamError 从 HTTP 错误体里认出上游的业务错误（AGT-04：Provider 把它带给
// Agent 侧按 Type 分类，如 overloaded_error）。认不出返回 nil。
func ParseUpstreamError(raw []byte) *UpstreamError { return parseUpstreamError(raw) }

// parseUpstreamError 从一段 JSON 里认出上游错误。认不出返回 nil。
//
// 兼容两种常见形状：`{"error":{"type","message","code"}}`（OpenAI 与多数中转）
// 与 `{"type":"error","error":{...}}`（Anthropic）；也兼容 error 直接是字符串。
func parseUpstreamError(raw []byte) *UpstreamError {
	var env struct {
		Type  string          `json:"type"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil
	}
	e := strings.TrimSpace(string(env.Error))
	if e == "" || e == "null" {
		return nil
	}
	var s string
	if json.Unmarshal(env.Error, &s) == nil {
		return &UpstreamError{Message: s}
	}
	var obj struct {
		Type    string          `json:"type"`
		Message string          `json:"message"`
		Code    json.RawMessage `json:"code"`
	}
	if json.Unmarshal(env.Error, &obj) != nil {
		return &UpstreamError{Message: e}
	}
	if obj.Type == "" && len(obj.Code) > 0 && string(obj.Code) != "null" {
		obj.Type = strings.Trim(string(obj.Code), `"`)
	}
	if obj.Message == "" {
		obj.Message = e
	}
	return &UpstreamError{Type: obj.Type, Message: obj.Message}
}

// errorEventError 把一个 `event: error` 的 data 翻成错误。
// ⚠️ 即使 data 解不开也必须报错 —— error 事件不能像普通坏事件那样被跳过。
func errorEventError(data string) error {
	if ue := parseUpstreamError([]byte(data)); ue != nil {
		return ue
	}
	return &UpstreamError{Message: truncateBody(data)}
}

// truncateBody 截断写进错误信息的上游原文（同 httpError 的 500 字上限）。
func truncateBody(s string) string {
	if len(s) > 500 {
		return s[:500] + "…"
	}
	return s
}

// toolArguments 按「合法 JSON 才放进 Arguments」的规则整理一次工具调用的参数。
//
// 空串当成 `{}`：无参工具的调用是合法的，流式下 Anthropic 对它只发一个空的
// partial_json，OpenAI 兼容的中转也有发空串的。
func toolArguments(raw string) (json.RawMessage, string) {
	t := strings.TrimSpace(raw)
	if t == "" {
		return json.RawMessage(`{}`), raw
	}
	if json.Valid([]byte(t)) {
		return json.RawMessage(t), raw
	}
	return nil, raw
}

// argumentsForRequest 取回填历史时要发给上游的参数（对象形态的 JSON）。
// 参数原本不合法时只能退回 `{}`：上游要求这里是对象，原样发一段坏 JSON 会让整轮 400。
func argumentsForRequest(tc ToolCall) json.RawMessage {
	if len(tc.Arguments) > 0 {
		return tc.Arguments
	}
	if t := strings.TrimSpace(tc.RawArguments); t != "" && json.Valid([]byte(t)) {
		return json.RawMessage(t)
	}
	return json.RawMessage(`{}`)
}
