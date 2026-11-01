package protocol

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// anthropic_messages 的多轮对话 + 工具调用（AGT-03）。
//
// 形状要点（与 OpenAI 不同的地方）：
//   - 系统提示词是**顶层** `system` 字段，没有 system 角色的消息；
//   - `max_tokens` 必填、`anthropic-version` 头必填，漏任一个都是 400；
//   - 工具定义 `tools:[{name,description,input_schema}]`，input_schema 必填；
//   - assistant 的工具调用是 content 里的 `tool_use` 块，`input` 是**对象**不是字符串；
//   - 工具结果没有 tool 角色，是 **user** 消息里的 `tool_result` 块；
//   - ⚠️ 角色必须交替：连续几条工具结果（以及紧随其后的用户消息）要**合并进同一条
//     user 消息**，分开发直接 400；
//   - 图片块排在文本**之前**；
//   - 流式按 `event:` 分派，工具参数在 `content_block_delta` 的 `input_json_delta.partial_json`
//     里分片到达。

var _ ChatProtocol = anthropicMessages{}

// anthropicDefaultMaxTokens 是调用方没给上限时的 max_tokens（必填项）。
// 调用方要更长的输出用 ChatJob.MaxTokensDefault 或 Options 的 max_tokens 指定。
const anthropicDefaultMaxTokens = 4096

// anthropicTurn 是正在拼的一条 Anthropic 消息。
type anthropicTurn struct {
	role string
	// results 是 tool_result 块。⚠️ 它们必须排在同一条 user 消息里其它内容的**前面**，
	// 否则上游报「tool_use ids were found without tool_result blocks immediately after」。
	results []any
	blocks  []any
}

func (t *anthropicTurn) content() []any {
	out := make([]any, 0, len(t.results)+len(t.blocks))
	out = append(out, t.results...)
	return append(out, t.blocks...)
}

func (anthropicMessages) BuildChat(job ChatJob) (Request, error) {
	systems := []string{}
	if job.System != "" {
		systems = append(systems, job.System)
	}
	var turns []*anthropicTurn
	// at 取一条指定角色的消息来追加内容：与上一条同角色就并进去，否则新开一条。
	at := func(role string) *anthropicTurn {
		if n := len(turns); n > 0 && turns[n-1].role == role {
			return turns[n-1]
		}
		t := &anthropicTurn{role: role}
		turns = append(turns, t)
		return t
	}
	for i, m := range job.Messages {
		switch m.Role {
		case RoleSystem:
			// 中途的系统消息只能并进顶层 system：Anthropic 没有这个角色。
			if m.Content != "" {
				systems = append(systems, m.Content)
			}
		case RoleUser:
			t := at(RoleUser)
			// ⚠️ 图片在文本之前，且用 source 结构（同 BuildSubmit）。
			for _, img := range m.Images {
				if src, ok := anthropicImageSource(img); ok {
					t.blocks = append(t.blocks, src)
				}
			}
			// 空文本块会被上游拒收（text content blocks must be non-empty）。
			if m.Content != "" {
				t.blocks = append(t.blocks, map[string]any{"type": "text", "text": m.Content})
			}
		case RoleAssistant:
			t := at(RoleAssistant)
			if m.Content != "" {
				t.blocks = append(t.blocks, map[string]any{"type": "text", "text": m.Content})
			}
			for _, tc := range m.ToolCalls {
				t.blocks = append(t.blocks, map[string]any{
					"type": "tool_use", "id": tc.ID, "name": tc.Name,
					// input 必须是对象；原本不合法的参数只能退回 {}。
					"input": argumentsForRequest(tc),
				})
			}
		case RoleTool:
			if m.ToolCallID == "" {
				return Request{}, fmt.Errorf("第 %d 条消息: 工具结果缺少 ToolCallID", i+1)
			}
			t := at(RoleUser)
			t.results = append(t.results, map[string]any{
				"type": "tool_result", "tool_use_id": m.ToolCallID, "content": m.Content,
			})
		default:
			return Request{}, fmt.Errorf("第 %d 条消息: 不支持的消息角色 %q", i+1, m.Role)
		}
	}
	messages := make([]any, 0, len(turns))
	for _, t := range turns {
		c := t.content()
		if len(c) == 0 {
			// 一条空消息（例如只有空文本的 assistant）发出去是 400，丢掉它。
			continue
		}
		messages = append(messages, map[string]any{"role": t.role, "content": c})
	}

	maxTokens := anthropicDefaultMaxTokens
	if job.MaxTokensDefault > 0 {
		maxTokens = job.MaxTokensDefault
	}
	body := map[string]any{
		"model":    job.UpstreamModel,
		"messages": messages,
		// max_tokens 在 Anthropic 是**必填**，缺了直接 400。
		"max_tokens": paramInt(job.Options, "max_tokens", maxTokens),
	}
	if len(systems) > 0 {
		body["system"] = strings.Join(systems, "\n\n")
	}
	if len(job.Tools) > 0 {
		tools := make([]any, 0, len(job.Tools))
		for _, td := range job.Tools {
			schema := td.Parameters
			if len(schema) == 0 {
				// input_schema 必填；无参工具给一个空对象 schema。
				schema = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			tool := map[string]any{"name": td.Name, "input_schema": schema}
			if td.Description != "" {
				tool["description"] = td.Description
			}
			tools = append(tools, tool)
		}
		body["tools"] = tools
	}
	for k, v := range job.Options {
		if k == "max_tokens" {
			continue
		}
		body[k] = v
	}
	if job.Stream {
		body["stream"] = true
	}
	return Request{
		Method: "POST", Path: "/messages", Body: mustJSON(body), Stream: job.Stream,
		// 版本头必填，漏了回 400。
		Header: map[string]string{"anthropic-version": "2023-06-01"},
	}, nil
}

// normalizeAnthropicStop 把 stop_reason 归一。未知值（refusal / pause_turn …）归 stop，
// 原文由调用方记日志。
func normalizeAnthropicStop(raw string) string {
	switch raw {
	case "tool_use":
		return FinishToolCalls
	case "max_tokens", "model_context_window_exceeded":
		return FinishLength
	}
	return FinishStop
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func (anthropicMessages) ParseChat(status int, body []byte) (ChatResult, error) {
	if status >= 400 {
		return ChatResult{}, httpError(status, body)
	}
	var resp struct {
		Type    string `json:"type"`
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string          `json:"stop_reason"`
		Usage      *anthropicUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return ChatResult{}, err
	}
	// ⚠️ HTTP 200 里夹的错误（中转常见）：`{"type":"error","error":{...}}`。
	if resp.Type == "error" || (resp.Content == nil && resp.StopReason == "") {
		if ue := parseUpstreamError(body); ue != nil {
			return ChatResult{}, ue
		}
		if resp.Type == "error" {
			return ChatResult{}, &UpstreamError{Message: truncateBody(string(body))}
		}
	}
	out := ChatResult{
		RawFinishReason: resp.StopReason,
		FinishReason:    normalizeAnthropicStop(resp.StopReason),
	}
	var sb strings.Builder
	for _, c := range resp.Content {
		switch c.Type {
		case "text":
			sb.WriteString(c.Text)
		case "tool_use":
			args, raw := toolArguments(string(c.Input))
			out.ToolCalls = append(out.ToolCalls, ToolCall{
				ID: c.ID, Name: c.Name, Arguments: args, RawArguments: raw,
			})
		}
	}
	out.Text = sb.String()
	if resp.Usage != nil {
		out.Usage = Usage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens}
	}
	return out, nil
}

func (anthropicMessages) NewChatStream() ChatStreamParser { return &anthropicChatStream{} }

// anthropicChatStream 按 `event:` 分派累积 Anthropic 的流式响应。
type anthropicChatStream struct {
	r         sseEventReader
	text      strings.Builder
	blocks    map[int]*anthropicPartialBlock
	rawFinish string
	usage     Usage
	done      bool
	err       error
}

type anthropicPartialBlock struct {
	typ, id, name string
	json          strings.Builder
}

func (s *anthropicChatStream) Feed(chunk []byte, onDelta func(string)) (bool, error) {
	if s.done {
		return true, s.err
	}
	s.r.feed(chunk, func(event, data string) bool {
		var ev struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
				Text string `json:"text"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Message struct {
				Usage *anthropicUsage `json:"usage"`
			} `json:"message"`
			Usage *anthropicUsage `json:"usage"`
			Error json.RawMessage `json:"error"`
		}
		bad := json.Unmarshal([]byte(data), &ev) != nil
		// ⚠️ error 事件不能像坏事件那样被跳过：事件名或 data 里的 type 任一说是 error 就是 error。
		// 中转也有不带 type、只推 `{"error":{...}}` 的。
		if event == "error" || (!bad && (ev.Type == "error" ||
			(len(ev.Error) > 0 && string(ev.Error) != "null"))) {
			s.err = errorEventError(data)
			s.done = true
			return true
		}
		if bad {
			return false
		}
		kind := ev.Type
		if kind == "" {
			kind = event
		}
		switch kind {
		case "message_start":
			if u := ev.Message.Usage; u != nil {
				s.usage.InputTokens = u.InputTokens
				s.usage.OutputTokens = u.OutputTokens
			}
		case "content_block_start":
			b := s.block(ev.Index)
			b.typ, b.id, b.name = ev.ContentBlock.Type, ev.ContentBlock.ID, ev.ContentBlock.Name
			if ev.ContentBlock.Type == "text" && ev.ContentBlock.Text != "" {
				s.emit(ev.ContentBlock.Text, onDelta)
			}
		case "content_block_delta":
			switch ev.Delta.Type {
			case "text_delta":
				s.emit(ev.Delta.Text, onDelta)
			case "input_json_delta":
				s.block(ev.Index).json.WriteString(ev.Delta.PartialJSON)
			}
			// thinking_delta / signature_delta 等不是给用户的正文，忽略。
		case "message_delta":
			if ev.Delta.StopReason != "" {
				s.rawFinish = ev.Delta.StopReason
			}
			if u := ev.Usage; u != nil {
				// message_delta 里的 output_tokens 是**累计值**，直接覆盖。
				s.usage.OutputTokens = u.OutputTokens
				if u.InputTokens > 0 {
					s.usage.InputTokens = u.InputTokens
				}
			}
		case "message_stop":
			s.done = true
			return true
		}
		// ping / content_block_stop / 未知事件：忽略（上游会加新事件类型，不能因此中断）。
		return false
	})
	return s.done, s.err
}

func (s *anthropicChatStream) block(i int) *anthropicPartialBlock {
	if s.blocks == nil {
		s.blocks = map[int]*anthropicPartialBlock{}
	}
	b := s.blocks[i]
	if b == nil {
		b = &anthropicPartialBlock{}
		s.blocks[i] = b
	}
	return b
}

func (s *anthropicChatStream) emit(text string, onDelta func(string)) {
	if text == "" {
		return
	}
	s.text.WriteString(text)
	if onDelta != nil {
		onDelta(text)
	}
}

func (s *anthropicChatStream) Result() ChatResult {
	out := ChatResult{Text: s.text.String(), RawFinishReason: s.rawFinish, Usage: s.usage}
	idxs := make([]int, 0, len(s.blocks))
	for i := range s.blocks {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	for _, i := range idxs {
		b := s.blocks[i]
		if b.typ != "tool_use" {
			continue
		}
		args, raw := toolArguments(b.json.String())
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			ID: b.id, Name: b.name, Arguments: args, RawArguments: raw,
		})
	}
	if s.err != nil {
		out.FinishReason = FinishError
	} else {
		out.FinishReason = normalizeAnthropicStop(s.rawFinish)
	}
	return out
}
