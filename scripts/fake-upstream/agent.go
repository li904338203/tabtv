package main

// 推理模型（画布 Agent）联调用的假上游：OpenAI Chat 与 Anthropic Messages 两种形状的
// **流式 + 工具调用**（AGT-04）。
//
// 按「脚本」返回工具调用：请求带 tools 且最后一条 user 消息含 `[[tool:NAME]]`
// （或 `[[tool:NAME|{"k":"v"}]]` 指定参数）时，流式返回一次 NAME 的工具调用，参数**分三片**发 ——
// 验的是我们的流式解析器能把分片拼回去，真实上游恰恰是这么切的。
// 最后一条是工具结果（OpenAI 的 role=tool / Anthropic 的 tool_result 块）时回一段文本收尾，
// 于是「提问 → 工具调用 → 回填结果 → 文本收尾」这一整轮能在两种协议上走完。
//
// ⚠️ 只校验**形状**（Anthropic 的版本头与 max_tokens 必填），不模拟模型的判断力。

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// toolMarker 匹配 `[[tool:NAME]]` 或 `[[tool:NAME|{json}]]`。
var toolMarker = regexp.MustCompile(`\[\[tool:([A-Za-z0-9_\-]+)(?:\|(\{.*?\}))?\]\]`)

// chatScript 是按请求推出来的「这一轮该回什么」。
type chatScript struct {
	// ToolName 非空时回一次工具调用。
	ToolName string
	ToolArgs string
	Text     string
}

// planReply 从最后一条消息推出回复。lastRole/lastText 由各协议的解析方给出。
func planReply(protocol string, toolNames []string, lastRole, lastText string, nMessages int) chatScript {
	if lastRole == "tool" {
		return chatScript{Text: "已收到工具结果：" + truncate(lastText, 60) + "。本轮完成。"}
	}
	if m := toolMarker.FindStringSubmatch(lastText); m != nil && len(toolNames) > 0 {
		name, args := m[1], m[2]
		if !contains(toolNames, name) {
			return chatScript{Text: fmt.Sprintf("请求里没有定义工具 %s（可用：%s）", name,
				strings.Join(toolNames, ","))}
		}
		if args == "" || !json.Valid([]byte(args)) {
			args = fmt.Sprintf(`{"from":"fake-upstream","protocol":%q}`, protocol)
		}
		return chatScript{Text: "好的，我来调用 " + name + "。", ToolName: name, ToolArgs: args}
	}
	return chatScript{Text: fmt.Sprintf("假上游流式回应（%s，收到 %d 条消息）。", protocol, nMessages)}
}

// splitRunes 把 s 按字符切成 n 片（不在 UTF-8 中间切开）。
func splitRunes(s string, n int) []string {
	r := []rune(s)
	if n <= 1 || len(r) < n {
		return []string{s}
	}
	out := make([]string, 0, n)
	size := len(r) / n
	for i := 0; i < n; i++ {
		end := (i + 1) * size
		if i == n-1 {
			end = len(r)
		}
		out = append(out, string(r[i*size:end]))
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// textOf 取一条消息的文本：content 可能是字符串，也可能是内容块数组（取其中的 text / tool_result）。
func textOf(content any) (text string, hasToolResult bool) {
	switch c := content.(type) {
	case string:
		return c, false
	case []any:
		var sb strings.Builder
		for _, b := range c {
			blk, _ := b.(map[string]any)
			switch blk["type"] {
			case "text":
				s, _ := blk["text"].(string)
				sb.WriteString(s)
			case "tool_result":
				hasToolResult = true
				t, _ := textOf(blk["content"])
				sb.WriteString(t)
			}
		}
		return sb.String(), hasToolResult
	}
	return "", false
}

// imagePartsOf 数一条消息里的图片块（OpenAI 的 image_url）：总数与其中以 http(s) 外链发出的张数。
// 营销 E2E（MKT-T-30）据此断言「推理模型真的收到了商品图」且「图是内联的、不是外链」（MKT-AC-05、MKD-18 R-4）。
func imagePartsOf(content any) (images, httpImages int) {
	blocks, _ := content.([]any)
	for _, b := range blocks {
		blk, _ := b.(map[string]any)
		if blk["type"] != "image_url" {
			continue
		}
		images++
		u, _ := blk["image_url"].(map[string]any)
		if s, _ := u["url"].(string); strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
			httpImages++
		}
	}
	return images, httpImages
}

// audioPartsOf 数一条消息里的音频块（OpenAI 的 input_audio / audio_url / audio）。
// 营销 E2E（MKT-T-41）据此断言「角色的参考音频不发给推理模型」（MKT-AC-45、MKD-33）。
func audioPartsOf(content any) (audios int) {
	blocks, _ := content.([]any)
	for _, b := range blocks {
		blk, _ := b.(map[string]any)
		switch blk["type"] {
		case "input_audio", "audio_url", "audio":
			audios++
		}
	}
	return audios
}

// bodyAudioData 数请求体里内联音频（data:audio/…）出现的次数：不论放在哪种块里都算（MKT-T-41）。
func bodyAudioData(body []byte) int { return strings.Count(string(body), "data:audio/") }

var reBodyURL = regexp.MustCompile(`https?://[^\s"\\]+`)

// bodyURLs 取请求体里出现的全部 http(s) 地址（去重，最多 5 个）：营销请求体里不该有任何媒体地址（MKT-AC-05）。
func bodyURLs(body []byte) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, u := range reBodyURL.FindAllString(string(body), -1) {
		if !seen[u] && len(out) < 5 {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}

// sse 是一个逐事件 flush 的写出器（模拟真实上游的分块到达）。
type sse struct {
	w  http.ResponseWriter
	fl http.Flusher
}

func newSSE(w http.ResponseWriter) *sse {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fl, _ := w.(http.Flusher)
	return &sse{w: w, fl: fl}
}

func (s *sse) send(event string, v any) {
	if event != "" {
		fmt.Fprintf(s.w, "event: %s\n", event)
	}
	switch d := v.(type) {
	case string:
		fmt.Fprintf(s.w, "data: %s\n\n", d)
	default:
		b, _ := json.Marshal(v)
		fmt.Fprintf(s.w, "data: %s\n\n", b)
	}
	if s.fl != nil {
		s.fl.Flush()
	}
	// 让分块真的分开到达（否则多半会被合进一个 TCP 包，验不到半包处理）。
	time.Sleep(5 * time.Millisecond)
}

// ---------------------------------------------------------------- OpenAI Chat

type openAIChatReq struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role       string `json:"role"`
		Content    any    `json:"content"`
		ToolCallID string `json:"tool_call_id"`
		ToolCalls  []struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	} `json:"messages"`
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

// agentInput 把请求换成 Agent 脚本的输入；系统提示词不是 Agent 骨架时返回 false。
func (req openAIChatReq) agentInput(toolNames []string) (agentTurnInput, bool) {
	in := agentTurnInput{Tools: toolNames}
	for _, m := range req.Messages {
		text, _ := textOf(m.Content)
		if m.Role == "system" {
			in.System += text
			continue
		}
		am := agentMsg{Role: m.Role, Text: text, ToolCallID: m.ToolCallID}
		am.Images, am.HTTPImages = imagePartsOf(m.Content)
		am.Audios = audioPartsOf(m.Content)
		for _, c := range m.ToolCalls {
			am.ToolCalls = append(am.ToolCalls, agentCall{ID: c.ID, Name: c.Function.Name, Args: c.Function.Arguments})
		}
		in.Messages = append(in.Messages, am)
	}
	return in, isAgentPrompt(in.System)
}

// openAIAgent 处理「流式或带工具」的 OpenAI Chat 请求。返回 false 表示不归它管
// （非流式且不带工具：走原来的单轮文本 / 分镜逻辑，脚本线的联调行为一字不变）。
func (s *server) openAIAgent(w http.ResponseWriter, body []byte) bool {
	var req openAIChatReq
	if err := json.Unmarshal(body, &req); err != nil {
		return false
	}
	if !req.Stream && len(req.Tools) == 0 {
		return false
	}
	names := make([]string, 0, len(req.Tools))
	for _, t := range req.Tools {
		names = append(names, t.Function.Name)
	}
	lastRole, lastText := "", ""
	if n := len(req.Messages); n > 0 {
		last := req.Messages[n-1]
		lastRole = last.Role
		lastText, _ = textOf(last.Content)
	}
	plan := planReply("openai_chat", names, lastRole, lastText, len(req.Messages))
	// 画布 Agent（AGT-31）：骨架标记命中且不是 `[[tool:…]]` 手动脚本时，按单段流程的决策表走。
	if in, ok := req.agentInput(names); ok && len(names) > 0 && !toolMarker.MatchString(lastText) {
		in.BodyURLs = bodyURLs(body)
		in.BodyAudioData = bodyAudioData(body)
		reply := agentScript(in)
		s.agentProbe.record(in, reply)
		log.Printf("  Agent 脚本：state=%s tools=%v → %s", agentState(in.System), names, reply.Action)
		if reply.Fail {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"type":"server_error","message":"假上游按 #对话失败 回 500"}}`))
			return true
		}
		plan = reply.chatScript
	}
	callID := fmt.Sprintf("call_fake_%d", s.seq.Add(1))
	log.Printf("  OpenAI Chat：stream=%v tools=%v → tool=%q", req.Stream, names, plan.ToolName)

	if !req.Stream {
		msg := map[string]any{"role": "assistant", "content": plan.Text}
		finish := "stop"
		if plan.ToolName != "" {
			msg["tool_calls"] = []any{map[string]any{"id": callID, "type": "function",
				"function": map[string]any{"name": plan.ToolName, "arguments": plan.ToolArgs}}}
			finish = "tool_calls"
		}
		writeJSON(w, map[string]any{
			"choices": []any{map[string]any{"message": msg, "finish_reason": finish}},
			"usage":   map[string]any{"prompt_tokens": len(body) / 4, "completion_tokens": 20},
		})
		return true
	}

	out := newSSE(w)
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{"id": "chatcmpl-fake", "object": "chat.completion.chunk",
			"model": req.Model, "choices": []any{map[string]any{
				"index": 0, "delta": delta, "finish_reason": finish}}}
	}
	out.send("", chunk(map[string]any{"role": "assistant", "content": ""}, nil))
	for _, piece := range splitRunes(plan.Text, 3) {
		out.send("", chunk(map[string]any{"content": piece}, nil))
	}
	finish := "stop"
	if plan.ToolName != "" {
		// 首片带 id 与 name、参数为空；之后三片只带参数（真实 OpenAI 就是这个顺序）。
		out.send("", chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": 0, "id": callID, "type": "function",
			"function": map[string]any{"name": plan.ToolName, "arguments": ""}}}}, nil))
		for _, piece := range splitRunes(plan.ToolArgs, 3) {
			out.send("", chunk(map[string]any{"tool_calls": []any{map[string]any{
				"index": 0, "function": map[string]any{"arguments": piece}}}}, nil))
		}
		finish = "tool_calls"
	}
	out.send("", chunk(map[string]any{}, finish))
	out.send("", map[string]any{"id": "chatcmpl-fake", "object": "chat.completion.chunk",
		"choices": []any{}, "usage": map[string]any{"prompt_tokens": len(body) / 4, "completion_tokens": 20}})
	out.send("", "[DONE]")
	return true
}

// ---------------------------------------------------------------- Anthropic Messages

func (s *server) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Stream    bool   `json:"stream"`
		System    any    `json:"system"`
		Messages  []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
	}
	anthropicErr := func(status int, typ, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		b, _ := json.Marshal(map[string]any{"type": "error",
			"error": map[string]any{"type": typ, "message": msg}})
		_, _ = w.Write(b)
	}
	if err := json.Unmarshal(body, &req); err != nil {
		anthropicErr(400, "invalid_request_error", "请求体不是合法 JSON")
		return
	}
	// ⚠️ 真实 Anthropic 对这两项缺一即 400：假上游照样拦，才验得到「我们拼对了」。
	if r.Header.Get("anthropic-version") == "" {
		anthropicErr(400, "invalid_request_error", "缺少 anthropic-version 头")
		return
	}
	if req.MaxTokens <= 0 {
		anthropicErr(400, "invalid_request_error", "max_tokens 必填")
		return
	}
	// 导演台搭景（DRT-30）：同 OpenAI 入口，系统提示词里有搭景标记就回固定方案（Anthropic 协议的模型也能联调）。
	if system := anthropicSystemText(req.System); isDirectorBuild(system) {
		in := directorBuildInput{Protocol: "anthropic_messages", Model: req.Model, System: system, URLs: bodyURLs(body)}
		var users []string
		for _, m := range req.Messages {
			texts, images := contentParts(m.Content)
			for _, u := range images {
				in.countImage(u)
			}
			if m.Role == "user" {
				users = append(users, texts...)
			}
		}
		in.User = strings.Join(users, "\n")
		s.serveDirectorBuild(w, in, func(text string) { writeAnthropicText(w, req.Model, req.Stream, text, len(body)/4) })
		return
	}
	names := make([]string, 0, len(req.Tools))
	for _, t := range req.Tools {
		if len(t.InputSchema) == 0 {
			anthropicErr(400, "invalid_request_error", "工具 "+t.Name+" 缺少 input_schema")
			return
		}
		names = append(names, t.Name)
	}
	lastRole, lastText := "", ""
	if n := len(req.Messages); n > 0 {
		last := req.Messages[n-1]
		text, hasResult := textOf(last.Content)
		lastRole, lastText = last.Role, text
		if hasResult {
			lastRole = "tool"
		}
	}
	plan := planReply("anthropic_messages", names, lastRole, lastText, len(req.Messages))
	toolID := fmt.Sprintf("toolu_fake_%d", s.seq.Add(1))
	log.Printf("  Anthropic Messages：stream=%v tools=%v → tool=%q", req.Stream, names, plan.ToolName)
	inTokens := len(body) / 4

	if !req.Stream {
		content := []any{map[string]any{"type": "text", "text": plan.Text}}
		stop := "end_turn"
		if plan.ToolName != "" {
			content = append(content, map[string]any{"type": "tool_use", "id": toolID,
				"name": plan.ToolName, "input": json.RawMessage(plan.ToolArgs)})
			stop = "tool_use"
		}
		writeJSON(w, map[string]any{"id": "msg_fake", "type": "message", "role": "assistant",
			"model": req.Model, "content": content, "stop_reason": stop,
			"usage": map[string]any{"input_tokens": inTokens, "output_tokens": 20}})
		return
	}

	out := newSSE(w)
	out.send("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_fake", "type": "message", "role": "assistant", "model": req.Model, "content": []any{},
		"usage": map[string]any{"input_tokens": inTokens, "output_tokens": 1}}})
	out.send("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""}})
	out.send("ping", map[string]any{"type": "ping"})
	for _, piece := range splitRunes(plan.Text, 3) {
		out.send("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": piece}})
	}
	out.send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	stop := "end_turn"
	if plan.ToolName != "" {
		out.send("content_block_start", map[string]any{"type": "content_block_start", "index": 1,
			"content_block": map[string]any{"type": "tool_use", "id": toolID, "name": plan.ToolName,
				"input": map[string]any{}}})
		for _, piece := range splitRunes(plan.ToolArgs, 3) {
			out.send("content_block_delta", map[string]any{"type": "content_block_delta", "index": 1,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": piece}})
		}
		out.send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 1})
		stop = "tool_use"
	}
	out.send("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": stop}, "usage": map[string]any{"output_tokens": 20}})
	out.send("message_stop", map[string]any{"type": "message_stop"})
}

// anthropicSystemText 取 Anthropic 请求的系统提示词：字符串，或文本块数组。
func anthropicSystemText(system any) string {
	texts, _ := contentParts(system)
	return strings.Join(texts, "\n")
}

// writeAnthropicText 回一条只有文字的 Anthropic 消息（流式或整包，形状同上面的推理模型应答）。
func writeAnthropicText(w http.ResponseWriter, model string, stream bool, text string, inTokens int) {
	if !stream {
		writeJSON(w, map[string]any{"id": "msg_fake", "type": "message", "role": "assistant", "model": model,
			"content": []any{map[string]any{"type": "text", "text": text}}, "stop_reason": "end_turn",
			"usage": map[string]any{"input_tokens": inTokens, "output_tokens": 20}})
		return
	}
	out := newSSE(w)
	out.send("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_fake", "type": "message", "role": "assistant", "model": model, "content": []any{},
		"usage": map[string]any{"input_tokens": inTokens, "output_tokens": 1}}})
	out.send("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""}})
	for _, piece := range splitRunes(text, 3) {
		out.send("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": piece}})
	}
	out.send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	out.send("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]any{"output_tokens": 20}})
	out.send("message_stop", map[string]any{"type": "message_stop"})
}
