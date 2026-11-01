package protocol

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture 照两家公开文档的真实响应形状写（字段名、index、partial_json 的切分方式），
// 不照实现反推 —— 那样测试与实现出自同一个错误前提，只会把错误钉住（CLAUDE.md §5.26）。

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "chat", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustChat(t *testing.T, code string) ChatProtocol {
	t.Helper()
	c, ok := GetChat(code)
	if !ok {
		t.Fatalf("%s 应实现 ChatProtocol", code)
	}
	return c
}

func decodeBody(t *testing.T, req Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatalf("请求体不是合法 JSON: %v", err)
	}
	return body
}

// feedAll 把整段流按 size 字节一块喂进去（size<=0 表示整段一次喂完），返回拼出的增量文本。
func feedAll(t *testing.T, p ChatStreamParser, data []byte, size int) (deltas string, done bool, err error) {
	t.Helper()
	var sb strings.Builder
	onDelta := func(s string) { sb.WriteString(s) }
	if size <= 0 {
		size = len(data)
	}
	for i := 0; i < len(data); i += size {
		end := i + size
		if end > len(data) {
			end = len(data)
		}
		done, err = p.Feed(data[i:end], onDelta)
		if done {
			break
		}
	}
	return sb.String(), done, err
}

// ---------------------------------------------------------------- 注册表

func TestIsChatCapable_由注册表派生(t *testing.T) {
	want := map[string]bool{"openai_chat": true, "anthropic_messages": true}
	for _, d := range All() {
		if got := IsChatCapable(d.Code); got != want[d.Code] {
			t.Errorf("IsChatCapable(%q) = %v，期望 %v", d.Code, got, want[d.Code])
		}
		_, ok := GetChat(d.Code)
		if ok != want[d.Code] {
			t.Errorf("GetChat(%q) 与 IsChatCapable 不一致", d.Code)
		}
	}
	if IsChatCapable("no_such_protocol") {
		t.Error("未注册的协议不能算 Chat 可用")
	}
}

// ---------------------------------------------------------------- OpenAI 构造

func TestOpenAIBuildChat_系统提示词是首条消息(t *testing.T) {
	req, err := mustChat(t, "openai_chat").BuildChat(ChatJob{
		UpstreamModel: "gpt-4o", System: "你是画布助手",
		Messages: []ChatMessage{{Role: RoleUser, Content: "你好"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.Path != "/chat/completions" || req.Method != "POST" {
		t.Fatalf("地址不对: %s %s", req.Method, req.Path)
	}
	body := decodeBody(t, req)
	msgs := body["messages"].([]any)
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "你是画布助手" {
		t.Fatalf("系统提示词应是首条 role=system 消息，实际 %v", first)
	}
	if body["system"] != nil {
		t.Fatal("OpenAI 没有顶层 system 字段")
	}
	if _, ok := body["stream"]; ok || req.Stream {
		t.Fatal("非流式不该带 stream 标记")
	}
}

func TestOpenAIBuildChat_工具调用与工具结果回填形状(t *testing.T) {
	req, err := mustChat(t, "openai_chat").BuildChat(ChatJob{
		UpstreamModel: "gpt-4o",
		Tools: []ToolDef{{
			Name: "add_node", Description: "加节点",
			Parameters: json.RawMessage(`{"type":"object","properties":{"type":{"type":"string"}}}`),
		}},
		Messages: []ChatMessage{
			{Role: RoleUser, Content: "加两个节点", Images: []string{"https://x/a.png"}},
			{Role: RoleAssistant, ToolCalls: []ToolCall{
				{ID: "call_1", Name: "add_node", Arguments: json.RawMessage(`{"type":"image"}`)},
				{ID: "call_2", Name: "add_node", RawArguments: `{"type":`},
			}},
			{Role: RoleTool, ToolCallID: "call_1", Content: `{"nodeId":"n1"}`},
			{Role: RoleTool, ToolCallID: "call_2", Content: "参数格式错"},
		},
		Options: map[string]any{"temperature": 0.3},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := decodeBody(t, req)
	if body["temperature"] != 0.3 {
		t.Error("options 应原样透传")
	}

	tools := body["tools"].([]any)
	tool := tools[0].(map[string]any)
	fn := tool["function"].(map[string]any)
	if tool["type"] != "function" || fn["name"] != "add_node" || fn["parameters"] == nil {
		t.Fatalf("工具定义形状不对: %v", tool)
	}

	msgs := body["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("应有 4 条消息（OpenAI 的工具结果各自一条），实际 %d", len(msgs))
	}
	user := msgs[0].(map[string]any)
	if _, ok := user["content"].([]any); !ok {
		t.Fatal("带图片的 user 消息应走 content 数组")
	}

	asst := msgs[1].(map[string]any)
	if v, ok := asst["content"]; !ok || v != nil {
		t.Fatalf("只有工具调用时 content 应为 null，实际 %v", v)
	}
	calls := asst["tool_calls"].([]any)
	c0 := calls[0].(map[string]any)
	f0 := c0["function"].(map[string]any)
	if c0["id"] != "call_1" || c0["type"] != "function" || f0["name"] != "add_node" {
		t.Fatalf("工具调用形状不对: %v", c0)
	}
	if args, ok := f0["arguments"].(string); !ok || args != `{"type":"image"}` {
		t.Fatalf("arguments 必须是 JSON 字符串，实际 %#v", f0["arguments"])
	}
	f1 := calls[1].(map[string]any)["function"].(map[string]any)
	if f1["arguments"] != `{"type":` {
		t.Fatalf("不合法的参数应原样带回让模型看到，实际 %#v", f1["arguments"])
	}

	for i, id := range []string{"call_1", "call_2"} {
		m := msgs[2+i].(map[string]any)
		if m["role"] != "tool" || m["tool_call_id"] != id {
			t.Fatalf("工具结果形状不对: %v", m)
		}
		if _, ok := m["content"].(string); !ok {
			t.Fatal("工具结果 content 应是字符串")
		}
	}
}

func TestOpenAIBuildChat_流式加stream标记(t *testing.T) {
	req, err := mustChat(t, "openai_chat").BuildChat(ChatJob{
		UpstreamModel: "gpt-4o", Stream: true,
		Messages: []ChatMessage{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !req.Stream || decodeBody(t, req)["stream"] != true {
		t.Fatal("流式应同时设置 body.stream 与 Request.Stream")
	}
}

func TestOpenAIBuildChat_非法消息报错(t *testing.T) {
	c := mustChat(t, "openai_chat")
	for name, m := range map[string]ChatMessage{
		"未知角色":            {Role: "robot", Content: "x"},
		"工具结果缺ToolCallID": {Role: RoleTool, Content: "x"},
	} {
		if _, err := c.BuildChat(ChatJob{Messages: []ChatMessage{m}}); err == nil {
			t.Errorf("%s 应报错", name)
		}
	}
}

// ---------------------------------------------------------------- OpenAI 非流式解析

func TestOpenAIParseChat_纯文本(t *testing.T) {
	res, err := mustChat(t, "openai_chat").ParseChat(200, fixture(t, "openai_text.json"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "好的，我来帮你把分镜整理成三段。" || res.FinishReason != FinishStop || len(res.ToolCalls) != 0 {
		t.Fatalf("解析不对: %+v", res)
	}
	if res.Usage != (Usage{InputTokens: 19, OutputTokens: 10}) {
		t.Fatalf("用量不对: %+v", res.Usage)
	}
}

func TestOpenAIParseChat_多工具调用(t *testing.T) {
	res, err := mustChat(t, "openai_chat").ParseChat(200, fixture(t, "openai_tool_calls.json"))
	if err != nil {
		t.Fatal(err)
	}
	if res.FinishReason != FinishToolCalls || res.RawFinishReason != "tool_calls" {
		t.Fatalf("结束原因不对: %q/%q", res.FinishReason, res.RawFinishReason)
	}
	if res.Text != "" {
		t.Fatalf("content 为 null 时文本应为空，实际 %q", res.Text)
	}
	if len(res.ToolCalls) != 2 {
		t.Fatalf("应解析出 2 个工具调用，实际 %d", len(res.ToolCalls))
	}
	assertArgs(t, res.ToolCalls[0], "call_abc123", "add_node", map[string]any{"type": "image", "prompt": "一只橘猫"})
	assertArgs(t, res.ToolCalls[1], "call_def456", "connect_nodes", map[string]any{"from": "n1", "to": "n2"})
	if res.Usage != (Usage{InputTokens: 82, OutputTokens: 17}) {
		t.Fatalf("用量不对: %+v", res.Usage)
	}
}

func TestOpenAIParseChat_参数非法JSON保留原文(t *testing.T) {
	res, err := mustChat(t, "openai_chat").ParseChat(200, fixture(t, "openai_bad_arguments.json"))
	if err != nil {
		t.Fatalf("参数格式错不该让解析失败（要交给 Agent 层回给模型），实际 %v", err)
	}
	tc := res.ToolCalls[0]
	if tc.Arguments != nil {
		t.Fatalf("非法 JSON 时 Arguments 应为 nil，实际 %s", tc.Arguments)
	}
	if tc.RawArguments != `{"type": "image", "prompt": "一只` {
		t.Fatalf("原文应保留，实际 %q", tc.RawArguments)
	}
}

func TestOpenAIParseChat_上游错误(t *testing.T) {
	c := mustChat(t, "openai_chat")
	cases := []struct {
		name   string
		status int
		body   []byte
		want   string
	}{
		{"HTTP 4xx 带回原文", 429, []byte(`{"error":{"message":"rate limit"}}`), "rate limit"},
		{"HTTP 200 里夹业务错误", 200, fixture(t, "openai_http200_error.json"), "负载已饱和"},
		{"没有 choices 也没有 error", 200, []byte(`{"id":"x"}`), "choices"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.ParseChat(tc.status, tc.body)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("应报错且含 %q，实际 %v", tc.want, err)
			}
		})
	}
}

// ---------------------------------------------------------------- OpenAI 流式

func TestOpenAIChatStream_文本增量回调(t *testing.T) {
	s := mustChat(t, "openai_chat").NewChatStream()
	deltas, done, err := feedAll(t, s, fixture(t, "openai_stream_text.sse"), 0)
	if err != nil || !done {
		t.Fatalf("应正常结束: done=%v err=%v", done, err)
	}
	if deltas != "你好，世界" {
		t.Fatalf("增量回调拼起来不对: %q", deltas)
	}
	res := s.Result()
	if res.Text != "你好，世界" || res.FinishReason != FinishStop || len(res.ToolCalls) != 0 {
		t.Fatalf("结果不对: %+v", res)
	}
	if res.Usage != (Usage{InputTokens: 12, OutputTokens: 4}) {
		t.Fatalf("最后一块的 usage 应被收下: %+v", res.Usage)
	}
}

func TestOpenAIChatStream_工具参数分片拼接(t *testing.T) {
	s := mustChat(t, "openai_chat").NewChatStream()
	deltas, done, err := feedAll(t, s, fixture(t, "openai_stream_tools.sse"), 0)
	if err != nil || !done {
		t.Fatalf("应正常结束: done=%v err=%v", done, err)
	}
	if deltas != "" {
		t.Fatalf("只有工具调用时不该回调文本，实际 %q", deltas)
	}
	res := s.Result()
	if res.FinishReason != FinishToolCalls {
		t.Fatalf("结束原因应为 tool_calls，实际 %q", res.FinishReason)
	}
	if len(res.ToolCalls) != 2 {
		t.Fatalf("应有 2 个工具调用，实际 %+v", res.ToolCalls)
	}
	assertArgs(t, res.ToolCalls[0], "call_DdmO9pD3xa9XTPNJ32zg2hcA", "add_node", map[string]any{"type": "image"})
	assertArgs(t, res.ToolCalls[1], "call_8XQLhmS2Lj2rVzn3H2jJk7cQ", "connect_nodes", map[string]any{"from": "n1", "to": "n2"})
}

func TestOpenAIChatStream_流中error事件返回错误(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"data 里的 error 对象", fixture(t, "openai_stream_error.sse"), "server_error"},
		{"event: error 且 data 解不开", []byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\nevent: error\ndata: upstream gone\n\n"), "upstream gone"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := mustChat(t, "openai_chat").NewChatStream()
			_, done, err := feedAll(t, s, tc.data, 0)
			if !done || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("流中错误必须返回错误（不能当空回复）: done=%v err=%v", done, err)
			}
			var ue *UpstreamError
			if !errors.As(err, &ue) {
				t.Fatalf("应是 *UpstreamError，实际 %T", err)
			}
			if s.Result().FinishReason != FinishError {
				t.Fatalf("结束原因应为 error，实际 %q", s.Result().FinishReason)
			}
			// 结束之后再喂也只原样返回。
			if d, e := s.Feed([]byte("data: [DONE]\n"), nil); !d || e == nil {
				t.Fatal("出错后再 Feed 应保持 done 与原错误")
			}
		})
	}
}

func TestOpenAIChatStream_半包(t *testing.T) {
	s := mustChat(t, "openai_chat").NewChatStream()
	var got strings.Builder
	on := func(x string) { got.WriteString(x) }
	// 一行 data 跨两次读取，且行尾是 \r\n。
	if done, err := s.Feed([]byte(`data: {"choices":[{"delta":{"content":"一`), on); done || err != nil || got.Len() != 0 {
		t.Fatal("半包不该产出内容")
	}
	if done, err := s.Feed([]byte("只\"}}]}\r\n\r\n"), on); done || err != nil || got.String() != "一只" {
		t.Fatalf("补齐后应产出「一只」，实际 %q", got.String())
	}
	s.Feed([]byte("data: {坏\r\n: keep-alive\r\n\r\n"), on) // 坏事件与注释被跳过
	s.Feed([]byte(`data: {"choices":[{"delta":{"content":"猫"},"finish_reason":"length"}]}`+"\r\n\r\ndata: [DO"), on)
	if done, err := s.Feed([]byte("NE]\r\n\r\n"), on); !done || err != nil {
		t.Fatalf("应识别跨块的结束标记: done=%v err=%v", done, err)
	}
	if res := s.Result(); res.Text != "一只猫" || res.FinishReason != FinishLength {
		t.Fatalf("结果不对: %+v", res)
	}
}

// ---------------------------------------------------------------- Anthropic 构造

func TestAnthropicBuildChat_system为顶层字段且max_tokens必填(t *testing.T) {
	req, err := mustChat(t, "anthropic_messages").BuildChat(ChatJob{
		UpstreamModel: "claude-x", System: "你是画布助手",
		Messages: []ChatMessage{{Role: RoleUser, Content: "你好"}},
		Tools: []ToolDef{
			{Name: "add_node", Description: "加节点", Parameters: json.RawMessage(`{"type":"object","properties":{"type":{"type":"string"}},"required":["type"]}`)},
			{Name: "list_nodes"},
		},
		Options: map[string]any{"temperature": 0.5},
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.Path != "/messages" || req.Header["anthropic-version"] == "" {
		t.Fatalf("地址或版本头不对: %s %v", req.Path, req.Header)
	}
	body := decodeBody(t, req)
	if body["system"] != "你是画布助手" {
		t.Fatalf("system 应是顶层字段，实际 %v", body["system"])
	}
	for _, m := range body["messages"].([]any) {
		if m.(map[string]any)["role"] == "system" {
			t.Fatal("Anthropic 不接受 role=system 的消息")
		}
	}
	if body["max_tokens"] != float64(4096) {
		t.Fatalf("max_tokens 必填且默认 4096，实际 %v", body["max_tokens"])
	}
	if body["temperature"] != 0.5 {
		t.Error("options 应透传")
	}
	tools := body["tools"].([]any)
	t0 := tools[0].(map[string]any)
	if t0["name"] != "add_node" || t0["input_schema"] == nil || t0["function"] != nil || t0["type"] != nil {
		t.Fatalf("工具定义应为 {name,description,input_schema}，实际 %v", t0)
	}
	if tools[1].(map[string]any)["input_schema"] == nil {
		t.Fatal("无参工具也要有 input_schema（必填）")
	}

	// 显式给 max_tokens 时用给的值，且不重复透传。
	req, _ = mustChat(t, "anthropic_messages").BuildChat(ChatJob{
		Messages: []ChatMessage{{Role: RoleUser, Content: "x"}},
		Options:  map[string]any{"max_tokens": float64(256)}, Stream: true,
	})
	body = decodeBody(t, req)
	if body["max_tokens"] != float64(256) || body["stream"] != true || !req.Stream {
		t.Fatalf("max_tokens/stream 不对: %v", body)
	}
}

// 04 文档 P-09：Agent 要的输出上限只在「必须带上限」的协议上生效，显式的 max_tokens 仍然优先；
// OpenAI 系不带上限也能跑，多塞一个 max_tokens 会改变调用方行为（有的型号直接拒收），所以忽略它。
func TestBuildChat_输出上限缺省值只给必须带上限的协议(t *testing.T) {
	job := ChatJob{Messages: []ChatMessage{{Role: RoleUser, Content: "x"}}, MaxTokensDefault: 8192}
	req, err := mustChat(t, "anthropic_messages").BuildChat(job)
	if err != nil {
		t.Fatal(err)
	}
	if body := decodeBody(t, req); body["max_tokens"] != float64(8192) {
		t.Fatalf("Anthropic 应用调用方给的缺省上限 8192，实际 %v", body["max_tokens"])
	}

	job.Options = map[string]any{"max_tokens": float64(300)}
	req, _ = mustChat(t, "anthropic_messages").BuildChat(job)
	if body := decodeBody(t, req); body["max_tokens"] != float64(300) {
		t.Fatalf("显式的 max_tokens 应优先，实际 %v", body["max_tokens"])
	}

	req, err = mustChat(t, "openai_chat").BuildChat(ChatJob{
		Messages: []ChatMessage{{Role: RoleUser, Content: "x"}}, MaxTokensDefault: 8192})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := decodeBody(t, req)["max_tokens"]; ok {
		t.Fatal("OpenAI 系不该被塞进 max_tokens")
	}
}

func TestAnthropicBuildChat_连续工具结果合并为一条user消息(t *testing.T) {
	req, err := mustChat(t, "anthropic_messages").BuildChat(ChatJob{
		UpstreamModel: "claude-x",
		Messages: []ChatMessage{
			{Role: RoleUser, Content: "加两个节点"},
			{Role: RoleAssistant, Content: "好的", ToolCalls: []ToolCall{
				{ID: "toolu_1", Name: "add_node", Arguments: json.RawMessage(`{"type":"image"}`)},
				{ID: "toolu_2", Name: "add_node", RawArguments: `{"type":`},
			}},
			{Role: RoleTool, ToolCallID: "toolu_1", Content: `{"nodeId":"n1"}`},
			{Role: RoleTool, ToolCallID: "toolu_2", Content: "参数格式错"},
			{Role: RoleUser, Content: "继续"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	msgs := decodeBody(t, req)["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("应为 user / assistant / user 三条（角色必须交替），实际 %d 条: %v", len(msgs), msgs)
	}
	roles := []string{}
	for _, m := range msgs {
		roles = append(roles, m.(map[string]any)["role"].(string))
	}
	if strings.Join(roles, ",") != "user,assistant,user" {
		t.Fatalf("角色序列不对: %v", roles)
	}

	asst := msgs[1].(map[string]any)["content"].([]any)
	if asst[0].(map[string]any)["type"] != "text" {
		t.Fatal("assistant 的文字块应在工具调用前")
	}
	tu := asst[1].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "toolu_1" || tu["name"] != "add_node" {
		t.Fatalf("tool_use 块形状不对: %v", tu)
	}
	if in, ok := tu["input"].(map[string]any); !ok || in["type"] != "image" {
		t.Fatalf("input 必须是对象，实际 %#v", tu["input"])
	}
	if in, ok := asst[2].(map[string]any)["input"].(map[string]any); !ok || len(in) != 0 {
		t.Fatalf("原本不合法的参数只能退回 {}，实际 %#v", asst[2].(map[string]any)["input"])
	}

	last := msgs[2].(map[string]any)["content"].([]any)
	if len(last) != 3 {
		t.Fatalf("两条工具结果 + 一段文字应合并进同一条 user 消息，实际 %v", last)
	}
	for i, id := range []string{"toolu_1", "toolu_2"} {
		b := last[i].(map[string]any)
		if b["type"] != "tool_result" || b["tool_use_id"] != id {
			t.Fatalf("第 %d 块应为 %s 的 tool_result，实际 %v", i, id, b)
		}
	}
	if b := last[2].(map[string]any); b["type"] != "text" || b["text"] != "继续" {
		t.Fatalf("tool_result 必须排在文字前，实际 %v", b)
	}
}

func TestAnthropicBuildChat_图片块在文本前(t *testing.T) {
	req, err := mustChat(t, "anthropic_messages").BuildChat(ChatJob{
		Messages: []ChatMessage{{
			Role: RoleUser, Content: "看看这两张",
			Images: []string{"data:image/png;base64,AAAA", "https://x/b.jpg"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	content := decodeBody(t, req)["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("应为 2 图 + 1 文，实际 %v", content)
	}
	for i := 0; i < 2; i++ {
		if content[i].(map[string]any)["type"] != "image" {
			t.Fatalf("第 %d 块应是图片（图片在文本前），实际 %v", i, content[i])
		}
	}
	src0 := content[0].(map[string]any)["source"].(map[string]any)
	src1 := content[1].(map[string]any)["source"].(map[string]any)
	if src0["type"] != "base64" || src0["media_type"] != "image/png" || src1["type"] != "url" {
		t.Fatalf("应使用 source 结构: %v / %v", src0, src1)
	}
	if content[2].(map[string]any)["type"] != "text" {
		t.Fatal("文本应在最后")
	}
}

// ---------------------------------------------------------------- Anthropic 非流式解析

func TestAnthropicParseChat_纯文本(t *testing.T) {
	res, err := mustChat(t, "anthropic_messages").ParseChat(200, fixture(t, "anthropic_text.json"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "好的，这是第二段。" || res.FinishReason != FinishStop || res.RawFinishReason != "end_turn" {
		t.Fatalf("解析不对: %+v", res)
	}
	if res.Usage != (Usage{InputTokens: 2095, OutputTokens: 503}) {
		t.Fatalf("用量不对: %+v", res.Usage)
	}
}

func TestAnthropicParseChat_tool_use转ToolCall(t *testing.T) {
	res, err := mustChat(t, "anthropic_messages").ParseChat(200, fixture(t, "anthropic_tool_use.json"))
	if err != nil {
		t.Fatal(err)
	}
	if res.FinishReason != FinishToolCalls {
		t.Fatalf("stop_reason=tool_use 应归一为 tool_calls，实际 %q", res.FinishReason)
	}
	if res.Text != "我先加一个图片节点，再把它连到视频节点。" {
		t.Fatalf("文本不对: %q", res.Text)
	}
	if len(res.ToolCalls) != 2 {
		t.Fatalf("应有 2 个工具调用，实际 %+v", res.ToolCalls)
	}
	assertArgs(t, res.ToolCalls[0], "toolu_01A09q90qw90lq917835lq9", "add_node", map[string]any{"type": "image", "prompt": "一只橘猫"})
	assertArgs(t, res.ToolCalls[1], "toolu_01B11r22st33uv44wx55yz6", "connect_nodes", map[string]any{"from": "n1", "to": "n2"})
}

func TestAnthropicParseChat_上游错误(t *testing.T) {
	c := mustChat(t, "anthropic_messages")
	cases := []struct {
		name   string
		status int
		body   []byte
		want   string
	}{
		{"HTTP 529 带回原文", 529, fixture(t, "anthropic_http200_error.json"), "Overloaded"},
		{"HTTP 200 里夹 type=error", 200, fixture(t, "anthropic_http200_error.json"), "overloaded_error"},
		{"HTTP 200 里只有 error 对象", 200, []byte(`{"error":{"message":"余额不足"}}`), "余额不足"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.ParseChat(tc.status, tc.body)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("应报错且含 %q，实际 %v", tc.want, err)
			}
		})
	}
}

// ---------------------------------------------------------------- Anthropic 流式

func TestAnthropicChatStream_文本增量回调(t *testing.T) {
	s := mustChat(t, "anthropic_messages").NewChatStream()
	deltas, done, err := feedAll(t, s, fixture(t, "anthropic_stream_text.sse"), 0)
	if err != nil || !done {
		t.Fatalf("应在 message_stop 结束: done=%v err=%v", done, err)
	}
	if deltas != "你好，世界" {
		t.Fatalf("增量回调不对: %q", deltas)
	}
	res := s.Result()
	if res.Text != "你好，世界" || res.FinishReason != FinishStop {
		t.Fatalf("结果不对: %+v", res)
	}
	if res.Usage != (Usage{InputTokens: 25, OutputTokens: 15}) {
		t.Fatalf("用量应取 message_start 的输入与 message_delta 的累计输出: %+v", res.Usage)
	}
}

func TestAnthropicChatStream_input_json_delta拼接(t *testing.T) {
	s := mustChat(t, "anthropic_messages").NewChatStream()
	deltas, done, err := feedAll(t, s, fixture(t, "anthropic_stream_tools.sse"), 0)
	if err != nil || !done {
		t.Fatalf("应正常结束: done=%v err=%v", done, err)
	}
	if deltas != "好的，我来加节点。" {
		t.Fatalf("文本增量不对: %q", deltas)
	}
	res := s.Result()
	if res.FinishReason != FinishToolCalls {
		t.Fatalf("结束原因应为 tool_calls，实际 %q", res.FinishReason)
	}
	if len(res.ToolCalls) != 2 {
		t.Fatalf("应有 2 个工具调用（文本块不算），实际 %+v", res.ToolCalls)
	}
	assertArgs(t, res.ToolCalls[0], "toolu_01T1x1fJ34qAmk2tNTrN7Up6", "add_node", map[string]any{"type": "image", "prompt": "一只橘猫"})
	// 无参工具只发了一个空的 partial_json：按 {} 处理，而不是当成参数非法。
	assertArgs(t, res.ToolCalls[1], "toolu_01CnXAVTbUu5aWTrQZCk1Kmf", "list_nodes", map[string]any{})
	if res.Usage != (Usage{InputTokens: 472, OutputTokens: 89}) {
		t.Fatalf("用量不对: %+v", res.Usage)
	}
}

func TestAnthropicChatStream_error事件(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"官方 error 事件", fixture(t, "anthropic_stream_error.sse"), "overloaded_error"},
		{"event: error 且 data 解不开", []byte("event: error\ndata: <html>502</html>\n\n"), "502"},
		{"没有 event 行、只推 error 对象（中转）", []byte(`data: {"error":{"type":"api_error","message":"boom"}}` + "\n\n"), "boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := mustChat(t, "anthropic_messages").NewChatStream()
			_, done, err := feedAll(t, s, tc.data, 0)
			if !done || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("流中错误必须返回错误: done=%v err=%v", done, err)
			}
			if s.Result().FinishReason != FinishError {
				t.Fatalf("结束原因应为 error，实际 %q", s.Result().FinishReason)
			}
		})
	}
}

func TestAnthropicChatStream_ping忽略(t *testing.T) {
	s := mustChat(t, "anthropic_messages").NewChatStream()
	stream := "event: ping\ndata: {\"type\": \"ping\"}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: ping\ndata: {\"type\": \"ping\"}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n" +
		"event: some_future_event\ndata: {\"type\":\"some_future_event\"}\n\n"
	deltas, done, err := feedAll(t, s, []byte(stream), 0)
	if done || err != nil {
		t.Fatalf("ping 与未知事件都不该结束或报错: done=%v err=%v", done, err)
	}
	if deltas != "ok" || s.Result().Text != "ok" {
		t.Fatalf("文本不对: %q", deltas)
	}
}

func TestAnthropicChatStream_半包与CRLF(t *testing.T) {
	s := mustChat(t, "anthropic_messages").NewChatStream()
	var got strings.Builder
	on := func(x string) { got.WriteString(x) }
	s.Feed([]byte("event: content_block_start\r\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_x\",\"name\":\"add_node\",\"input\":{}}}\r\n\r\n"), on)
	s.Feed([]byte("event: content_block_delta\r\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"ty"), on)
	s.Feed([]byte("pe\\\":\\\"image\\\"}\"}}\r\n\r\nevent: message_delta\r\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":7}}\r\n\r\nevent: message_st"), on)
	done, err := s.Feed([]byte("op\r\ndata: {\"type\":\"message_stop\"}\r\n\r\n"), on)
	if !done || err != nil {
		t.Fatalf("应在跨块的 message_stop 处结束: done=%v err=%v", done, err)
	}
	res := s.Result()
	if len(res.ToolCalls) != 1 {
		t.Fatalf("工具调用不对: %+v", res)
	}
	assertArgs(t, res.ToolCalls[0], "toolu_x", "add_node", map[string]any{"type": "image"})
}

// ---------------------------------------------------------------- 两家共用

// 同一段流按任意字节数切块喂进去，结果必须与整段一次喂完相同 —— 半包的通用形式。
func TestChatStream_任意切块结果一致(t *testing.T) {
	cases := []struct {
		code, file string
	}{
		{"openai_chat", "openai_stream_text.sse"},
		{"openai_chat", "openai_stream_tools.sse"},
		{"anthropic_messages", "anthropic_stream_text.sse"},
		{"anthropic_messages", "anthropic_stream_tools.sse"},
	}
	for _, tc := range cases {
		data := fixture(t, tc.file)
		whole := mustChat(t, tc.code).NewChatStream()
		wantDeltas, _, _ := feedAll(t, whole, data, 0)
		want := mustJSON(whole.Result())
		for _, size := range []int{1, 3, 7, 64} {
			s := mustChat(t, tc.code).NewChatStream()
			deltas, done, err := feedAll(t, s, data, size)
			if err != nil || !done {
				t.Fatalf("%s 切 %d 字节: done=%v err=%v", tc.file, size, done, err)
			}
			if deltas != wantDeltas || string(mustJSON(s.Result())) != string(want) {
				t.Fatalf("%s 切 %d 字节结果与整段不同:\n%s\n%s", tc.file, size, mustJSON(s.Result()), want)
			}
		}
	}
}

func TestChatFinishReason_归一(t *testing.T) {
	cases := []struct {
		fn       func(string) string
		raw, out string
	}{
		{normalizeOpenAIFinish, "stop", FinishStop},
		{normalizeOpenAIFinish, "tool_calls", FinishToolCalls},
		{normalizeOpenAIFinish, "function_call", FinishToolCalls},
		{normalizeOpenAIFinish, "length", FinishLength},
		{normalizeOpenAIFinish, "content_filter", FinishStop},
		{normalizeOpenAIFinish, "", FinishStop},
		{normalizeAnthropicStop, "end_turn", FinishStop},
		{normalizeAnthropicStop, "stop_sequence", FinishStop},
		{normalizeAnthropicStop, "tool_use", FinishToolCalls},
		{normalizeAnthropicStop, "max_tokens", FinishLength},
		{normalizeAnthropicStop, "refusal", FinishStop},
	}
	for _, tc := range cases {
		if got := tc.fn(tc.raw); got != tc.out {
			t.Errorf("%q 应归一为 %q，实际 %q", tc.raw, tc.out, got)
		}
	}
}

func assertArgs(t *testing.T, tc ToolCall, id, name string, want map[string]any) {
	t.Helper()
	if tc.ID != id || tc.Name != name {
		t.Fatalf("工具调用应为 %s/%s，实际 %s/%s", id, name, tc.ID, tc.Name)
	}
	if tc.Arguments == nil {
		t.Fatalf("%s 的参数应是合法 JSON，原文 %q", name, tc.RawArguments)
	}
	var got map[string]any
	if err := json.Unmarshal(tc.Arguments, &got); err != nil {
		t.Fatalf("参数解不开: %v", err)
	}
	if string(mustJSON(got)) != string(mustJSON(want)) {
		t.Fatalf("%s 参数不对: %s，期望 %s", name, mustJSON(got), mustJSON(want))
	}
}

// ParseSSE 改为复用 sse.go 的切行逻辑后行为不变（脚本线在用）：
// CRLF、event 行、注释行、空 data 与结束标记之后的内容都按原规则处理。
func TestParseSSE_抽出切行逻辑后行为不变(t *testing.T) {
	cases := []struct {
		name     string
		chunks   []string
		want     string
		wantDone bool
	}{
		{"CRLF 行尾", []string{"data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\r\n\r\n"}, "a", false},
		{"event 行与注释行被忽略", []string{"event: message\n: c\ndata: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n"}, "b", false},
		{"空 data 被跳过", []string{"data:\ndata: {\"choices\":[{\"delta\":{\"content\":\"c\"}}]}\n"}, "c", false},
		{"结束标记之后同块的内容被丢弃", []string{"data: [DONE]\ndata: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n"}, "", true},
		{"没有换行的尾巴留到下一块", []string{"data: {\"choices\":[{\"delta\":{\"content\":\"d\"}}]}", "\n"}, "d", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf, got strings.Builder
			var done bool
			for _, c := range tc.chunks {
				done = ParseSSE(&buf, []byte(c), func(s string) { got.WriteString(s) })
			}
			if got.String() != tc.want || done != tc.wantDone {
				t.Fatalf("got %q done=%v，期望 %q done=%v", got.String(), done, tc.want, tc.wantDone)
			}
		})
	}
}
