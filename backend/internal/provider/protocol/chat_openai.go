package protocol

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// openai_chat 的多轮对话 + 工具调用（AGT-03）。
//
// 形状要点（与 Anthropic 不同的地方）：
//   - 系统提示词是首条 role=system 的消息；
//   - 工具定义 `tools:[{type:"function",function:{name,description,parameters}}]`；
//   - assistant 的工具调用在 `tool_calls`，`function.arguments` 是 **JSON 字符串**不是对象；
//   - 工具结果是一条 role=tool 的消息，带 `tool_call_id`；
//   - 流式的工具参数按 `delta.tool_calls[i].index` 分片到达，要自己拼。

var _ ChatProtocol = openAIChat{}

func (openAIChat) BuildChat(job ChatJob) (Request, error) {
	messages := make([]any, 0, len(job.Messages)+1)
	if job.System != "" {
		messages = append(messages, map[string]any{"role": RoleSystem, "content": job.System})
	}
	for i, m := range job.Messages {
		msg, err := openAIChatMessage(m)
		if err != nil {
			return Request{}, fmt.Errorf("第 %d 条消息: %w", i+1, err)
		}
		messages = append(messages, msg)
	}
	body := map[string]any{
		"model":    job.UpstreamModel,
		"messages": messages,
	}
	if len(job.Tools) > 0 {
		tools := make([]any, 0, len(job.Tools))
		for _, t := range job.Tools {
			fn := map[string]any{"name": t.Name}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if len(t.Parameters) > 0 {
				fn["parameters"] = t.Parameters
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		body["tools"] = tools
	}
	// options 原样透传（同 BuildSubmit）：协议不替调用方选择，也不替它兜底。
	for k, v := range job.Options {
		body[k] = v
	}
	if job.Stream {
		body["stream"] = true
	}
	return Request{
		Method: "POST", Path: "/chat/completions", Body: mustJSON(body), Stream: job.Stream,
	}, nil
}

func openAIChatMessage(m ChatMessage) (map[string]any, error) {
	switch m.Role {
	case RoleSystem:
		return map[string]any{"role": RoleSystem, "content": m.Content}, nil
	case RoleUser:
		// 有图片时走 content 数组（沿用 BuildSubmit 的写法），否则是简单字符串。
		var content any = m.Content
		if len(m.Images) > 0 {
			parts := make([]any, 0, len(m.Images)+1)
			parts = append(parts, map[string]any{"type": "text", "text": m.Content})
			for _, img := range m.Images {
				parts = append(parts, map[string]any{
					"type": "image_url", "image_url": map[string]any{"url": img},
				})
			}
			content = parts
		}
		return map[string]any{"role": RoleUser, "content": content}, nil
	case RoleAssistant:
		msg := map[string]any{"role": RoleAssistant}
		if len(m.ToolCalls) == 0 {
			msg["content"] = m.Content
			return msg, nil
		}
		// 只有工具调用、没有文字时 content 发 null（官方文档的形状）。
		if m.Content != "" {
			msg["content"] = m.Content
		} else {
			msg["content"] = nil
		}
		calls := make([]any, 0, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			calls = append(calls, map[string]any{
				"id":   tc.ID,
				"type": "function",
				"function": map[string]any{
					"name": tc.Name,
					// ⚠️ arguments 是字符串。原本不合法的参数原样带回：
					// 模型要看到自己写坏了什么，才知道怎么改。
					"arguments": openAIArgumentsString(tc),
				},
			})
		}
		msg["tool_calls"] = calls
		return msg, nil
	case RoleTool:
		if m.ToolCallID == "" {
			return nil, fmt.Errorf("工具结果缺少 ToolCallID")
		}
		return map[string]any{
			"role": RoleTool, "tool_call_id": m.ToolCallID, "content": m.Content,
		}, nil
	}
	return nil, fmt.Errorf("不支持的消息角色 %q", m.Role)
}

func openAIArgumentsString(tc ToolCall) string {
	if len(tc.Arguments) > 0 {
		return string(tc.Arguments)
	}
	if tc.RawArguments != "" {
		return tc.RawArguments
	}
	return "{}"
}

// normalizeOpenAIFinish 把 finish_reason 归一。未知值归 stop（原文由调用方记日志）。
func normalizeOpenAIFinish(raw string) string {
	switch raw {
	case "tool_calls", "function_call":
		return FinishToolCalls
	case "length":
		return FinishLength
	}
	return FinishStop
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type openAIToolCallWire struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func (openAIChat) ParseChat(status int, body []byte) (ChatResult, error) {
	if status >= 400 {
		return ChatResult{}, httpError(status, body)
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content   string               `json:"content"`
				ToolCalls []openAIToolCallWire `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *openAIUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return ChatResult{}, err
	}
	if len(resp.Choices) == 0 {
		// ⚠️ 中转常在 HTTP 200 里夹业务错误：当成空回复的话用户看到模型「什么都没说」。
		if ue := parseUpstreamError(body); ue != nil {
			return ChatResult{}, ue
		}
		return ChatResult{}, fmt.Errorf("上游响应里没有 choices: %s", truncateBody(string(body)))
	}
	c := resp.Choices[0]
	out := ChatResult{
		Text:            c.Message.Content,
		RawFinishReason: c.FinishReason,
		FinishReason:    normalizeOpenAIFinish(c.FinishReason),
	}
	for _, w := range c.Message.ToolCalls {
		args, raw := toolArguments(w.Function.Arguments)
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			ID: w.ID, Name: w.Function.Name, Arguments: args, RawArguments: raw,
		})
	}
	if resp.Usage != nil {
		out.Usage = Usage{InputTokens: resp.Usage.PromptTokens, OutputTokens: resp.Usage.CompletionTokens}
	}
	return out, nil
}

func (openAIChat) NewChatStream() ChatStreamParser { return &openAIChatStream{} }

// openAIChatStream 累积 OpenAI 的流式响应。
type openAIChatStream struct {
	r     sseEventReader
	text  strings.Builder
	calls map[int]*openAIPartialCall
	// last 是最近一个工具调用槽位：个别兼容接口不带 index，只能按「有 id 就开新槽、
	// 没 id 就续在上一个」来拼。
	last      int
	rawFinish string
	usage     Usage
	done      bool
	err       error
}

type openAIPartialCall struct {
	id, name string
	args     strings.Builder
}

func (s *openAIChatStream) Feed(chunk []byte, onDelta func(string)) (bool, error) {
	if s.done {
		return true, s.err
	}
	s.r.feed(chunk, func(event, data string) bool {
		if data == "[DONE]" {
			s.done = true
			return true
		}
		if event == "error" {
			s.fail(errorEventError(data))
			return true
		}
		var ev struct {
			Choices []struct {
				Delta struct {
					Content   string               `json:"content"`
					ToolCalls []openAIToolCallWire `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *openAIUsage    `json:"usage"`
			Error json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			// 单条事件坏了跳过（同 ParseSSE）：上游可能夹带心跳或注释。
			return false
		}
		// ⚠️ 中转常在流里推 `{"error":{...}}`：不认出来的话这一轮被当成空回复。
		if len(ev.Error) > 0 && string(ev.Error) != "null" {
			s.fail(errorEventError(data))
			return true
		}
		if ev.Usage != nil {
			s.usage = Usage{InputTokens: ev.Usage.PromptTokens, OutputTokens: ev.Usage.CompletionTokens}
		}
		if len(ev.Choices) == 0 {
			return false
		}
		c := ev.Choices[0]
		if c.Delta.Content != "" {
			s.text.WriteString(c.Delta.Content)
			if onDelta != nil {
				onDelta(c.Delta.Content)
			}
		}
		for _, w := range c.Delta.ToolCalls {
			s.addToolDelta(w)
		}
		if c.FinishReason != nil && *c.FinishReason != "" {
			s.rawFinish = *c.FinishReason
		}
		return false
	})
	return s.done, s.err
}

func (s *openAIChatStream) addToolDelta(w openAIToolCallWire) {
	if s.calls == nil {
		s.calls = map[int]*openAIPartialCall{}
	}
	idx := s.last
	switch {
	case w.Index != nil:
		idx = *w.Index
	case w.ID != "" && s.calls[s.last] != nil && s.calls[s.last].id != w.ID:
		idx = len(s.calls)
	}
	pc := s.calls[idx]
	if pc == nil {
		pc = &openAIPartialCall{}
		s.calls[idx] = pc
	}
	s.last = idx
	// 首块带 id 与 name；之后的分片只带 arguments。
	if w.ID != "" {
		pc.id = w.ID
	}
	if w.Function.Name != "" {
		pc.name = w.Function.Name
	}
	pc.args.WriteString(w.Function.Arguments)
}

func (s *openAIChatStream) fail(err error) {
	s.err = err
	s.done = true
}

func (s *openAIChatStream) Result() ChatResult {
	out := ChatResult{Text: s.text.String(), RawFinishReason: s.rawFinish, Usage: s.usage}
	idxs := make([]int, 0, len(s.calls))
	for i := range s.calls {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	for _, i := range idxs {
		pc := s.calls[i]
		args, raw := toolArguments(pc.args.String())
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			ID: pc.id, Name: pc.name, Arguments: args, RawArguments: raw,
		})
	}
	switch {
	case s.err != nil:
		out.FinishReason = FinishError
	default:
		out.FinishReason = normalizeOpenAIFinish(s.rawFinish)
	}
	return out
}
