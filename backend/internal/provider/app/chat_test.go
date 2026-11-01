package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/li904338203/tabtv/backend/internal/provider/protocol"
	"github.com/li904338203/tabtv/backend/internal/provider/transport"
)

// ---------------------------------------------------------------- 脚手架

// chatFixture 读协议层的流式 fixture（AGT-03 的 testdata，同一份样本两层共用）。
func chatFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../protocol/testdata/chat/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// sseServer 把 body 按 SSE 事件（空行分隔）逐个写出并 flush，模拟真实上游的分块到达。
// 返回服务器与「收到几次请求」的计数。
func sseServer(t *testing.T, check func(r *http.Request, body []byte), body string) (
	*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		raw, _ := io.ReadAll(r.Body)
		if check != nil {
			check(r, raw)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, ev := range strings.SplitAfter(body, "\n\n") {
			if ev == "" {
				continue
			}
			_, _ = io.WriteString(w, ev)
			fl.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func chatCfg(baseURL, proto string) RunConfig {
	return RunConfig{
		ModelCode: "m-chat", UpstreamModel: "up-chat", ProtocolCode: proto,
		ProviderCode: "p-1", BaseURL: baseURL, AuthType: "bearer", APIKey: "sk-test-secret-0001",
		RequestTimeout: 5 * time.Second, IdleTimeout: 2 * time.Second,
	}
}

func userMsg(s string) []protocol.ChatMessage {
	return []protocol.ChatMessage{{Role: protocol.RoleUser, Content: s}}
}

var addNodeTool = []protocol.ToolDef{{
	Name: "add_node", Description: "加一个节点",
	Parameters: json.RawMessage(`{"type":"object","properties":{"type":{"type":"string"}}}`),
}}

// ---------------------------------------------------------------- 用例

func TestChat_流式文本逐块回调(t *testing.T) {
	var gotBody map[string]any
	var gotAuth, gotPath string
	srv, _ := sseServer(t, func(r *http.Request, b []byte) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		_ = json.Unmarshal(b, &gotBody)
	}, chatFixture(t, "openai_stream_text.sse"))
	p, _ := newProvider(t, chatCfg(srv.URL+"/v1", "openai_chat"))

	var deltas []string
	out, err := p.Chat(context.Background(), ChatInput{
		ModelCode: "m-chat", System: "你是助手", Messages: userMsg("你好"),
	}, func(s string) { deltas = append(deltas, s) })
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/chat/completions" || gotAuth != "Bearer sk-test-secret-0001" {
		t.Fatalf("地址或认证头不对：%s / %s", gotPath, gotAuth)
	}
	if gotBody["stream"] != true || gotBody["model"] != "up-chat" {
		t.Fatalf("请求体应带 stream=true 与上游型号，得到 %v", gotBody)
	}
	if len(deltas) < 2 {
		t.Fatalf("应逐块回调，实际只回调了 %d 次", len(deltas))
	}
	if strings.Join(deltas, "") != out.Text || out.Text == "" {
		t.Fatalf("增量拼起来应等于最终文本：%q vs %q", strings.Join(deltas, ""), out.Text)
	}
	if out.FinishReason != protocol.FinishStop {
		t.Fatalf("结束原因应为 stop，得到 %s", out.FinishReason)
	}
}

func TestChat_工具调用参数分片拼接(t *testing.T) {
	var gotBody map[string]any
	srv, _ := sseServer(t, func(_ *http.Request, b []byte) { _ = json.Unmarshal(b, &gotBody) },
		chatFixture(t, "openai_stream_tools.sse"))
	p, _ := newProvider(t, chatCfg(srv.URL, "openai_chat"))

	out, err := p.Chat(context.Background(), ChatInput{
		ModelCode: "m-chat", Messages: userMsg("加一张图"), Tools: addNodeTool,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tools, _ := gotBody["tools"].([]any); len(tools) != 1 {
		t.Fatalf("请求应带上工具定义，得到 %v", gotBody["tools"])
	}
	if out.FinishReason != protocol.FinishToolCalls || len(out.ToolCalls) != 2 {
		t.Fatalf("应得到两次工具调用，得到 %s / %+v", out.FinishReason, out.ToolCalls)
	}
	if out.ToolCalls[0].Name != "add_node" || string(out.ToolCalls[0].Arguments) != `{"type":"image"}` {
		t.Fatalf("分片参数没拼对：%+v", out.ToolCalls[0])
	}
	if string(out.ToolCalls[1].Arguments) != `{"from": "n1", "to": "n2"}` {
		t.Fatalf("第二个调用参数没拼对：%s", out.ToolCalls[1].Arguments)
	}
}

func TestChat_Anthropic流式工具调用(t *testing.T) {
	var gotVersion, gotPath string
	var gotBody map[string]any
	srv, _ := sseServer(t, func(r *http.Request, b []byte) {
		gotVersion, gotPath = r.Header.Get("anthropic-version"), r.URL.Path
		_ = json.Unmarshal(b, &gotBody)
	}, chatFixture(t, "anthropic_stream_tools.sse"))
	p, _ := newProvider(t, chatCfg(srv.URL+"/v1", "anthropic_messages"))

	var deltas []string
	out, err := p.Chat(context.Background(), ChatInput{
		ModelCode: "m-chat", System: "你是画布助手", Messages: userMsg("加一张橘猫"), Tools: addNodeTool,
	}, func(s string) { deltas = append(deltas, s) })
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/messages" || gotVersion == "" || gotBody["system"] != "你是画布助手" {
		t.Fatalf("Anthropic 请求形状不对：path=%s version=%q system=%v", gotPath, gotVersion, gotBody["system"])
	}
	if strings.Join(deltas, "") != "好的，我来加节点。" {
		t.Fatalf("文本增量不对：%v", deltas)
	}
	if out.FinishReason != protocol.FinishToolCalls || len(out.ToolCalls) != 2 {
		t.Fatalf("应得到两次工具调用：%s / %+v", out.FinishReason, out.ToolCalls)
	}
	var args map[string]string
	if err := json.Unmarshal(out.ToolCalls[0].Arguments, &args); err != nil || args["prompt"] != "一只橘猫" {
		t.Fatalf("input_json_delta 没拼对：%s", out.ToolCalls[0].RawArguments)
	}
	if out.ToolCalls[1].Name != "list_nodes" || string(out.ToolCalls[1].Arguments) != `{}` {
		t.Fatalf("无参工具应得到 {}：%+v", out.ToolCalls[1])
	}
	if out.Usage.InputTokens != 472 || out.Usage.OutputTokens != 89 {
		t.Fatalf("用量不对：%+v", out.Usage)
	}
}

// ⚠️ 空闲超时**不重发**：上游可能已经在算、已经计费。
func TestChat_空闲超时不重发(t *testing.T) {
	var hits atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"先说一句"}}]}`+"\n\n")
		w.(http.Flusher).Flush()
		// 之后挂起不关连接 —— 正是空闲看门狗要防的那种上游。
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })

	cfg := chatCfg(srv.URL, "openai_chat")
	cfg.IdleTimeout = 300 * time.Millisecond
	cfg.MaxRetries = 3
	p, logs := newProvider(t, cfg)

	var deltas []string
	started := time.Now()
	_, err := p.Chat(context.Background(), ChatInput{ModelCode: "m-chat", Messages: userMsg("hi")},
		func(s string) { deltas = append(deltas, s) })
	if !errors.Is(err, transport.ErrIdleTimeout) {
		t.Fatalf("应返回 ErrIdleTimeout，得到 %v", err)
	}
	var ce *ChatError
	if !errors.As(err, &ce) || ce.Kind != transport.ErrorStream {
		t.Fatalf("应是 stream 类的 ChatError，得到 %#v", err)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("空闲超时不得重发，上游收到 %d 次", n)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatalf("应在空闲超时后尽快返回，实际耗时 %s", time.Since(started))
	}
	if len(deltas) != 1 {
		t.Fatalf("超时前收到的增量应已回调：%v", deltas)
	}
	if l := logs.all(); len(l) != 1 || l[0].ErrorType != string(transport.ErrorStream) {
		t.Fatalf("应落一条 stream 类日志：%+v", l)
	}
}

// 连不上 = 请求没到上游，按 MaxRetries 重试；每次尝试各落一条日志。
func TestChat_连接失败按配置重试(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // 端口空出来：连接一律被拒

	cfg := chatCfg("http://"+addr, "openai_chat")
	cfg.MaxRetries = 2
	p, logs := newProvider(t, cfg)

	_, err = p.Chat(context.Background(), ChatInput{ModelCode: "m-chat", Messages: userMsg("hi")}, nil)
	var ce *ChatError
	if !errors.As(err, &ce) || ce.Kind != transport.ErrorConnect {
		t.Fatalf("应是 connect 类的 ChatError，得到 %v", err)
	}
	if n := len(logs.all()); n != 3 {
		t.Fatalf("MaxRetries=2 应共尝试 3 次（3 条日志），实际 %d", n)
	}

	// 连上了才失败（HTTP 5xx）不重试。
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":{"type":"server_error","message":"upstream down"}}`)
	}))
	t.Cleanup(srv.Close)
	cfg = chatCfg(srv.URL, "openai_chat")
	cfg.MaxRetries = 2
	p, _ = newProvider(t, cfg)
	_, err = p.Chat(context.Background(), ChatInput{ModelCode: "m-chat", Messages: userMsg("hi")}, nil)
	if !errors.As(err, &ce) || ce.Kind != transport.ErrorHTTP || ce.HTTPStatus != 502 {
		t.Fatalf("应是 HTTP 502 的 ChatError，得到 %v", err)
	}
	if ce.Upstream == nil || ce.Upstream.Message != "upstream down" {
		t.Fatalf("应认出上游的错误信息：%+v", ce.Upstream)
	}
	if hits.Load() != 1 {
		t.Fatalf("HTTP 错误不得重发，上游收到 %d 次", hits.Load())
	}
}

func TestChat_协议不支持Chat时明确报错(t *testing.T) {
	p, logs := newProvider(t, chatCfg("http://127.0.0.1:1", "generic_sync"))
	_, err := p.Chat(context.Background(), ChatInput{ModelCode: "m-chat", Messages: userMsg("hi")}, nil)
	if err == nil || !strings.Contains(err.Error(), "generic_sync") ||
		!strings.Contains(err.Error(), "不支持多轮对话") {
		t.Fatalf("应明确说出协议不支持对话，得到 %v", err)
	}
	if len(logs.all()) != 0 {
		t.Fatal("没发请求就不应落日志")
	}
}

func TestChat_请求日志记摘要不记整流(t *testing.T) {
	srv, _ := sseServer(t, nil, chatFixture(t, "openai_stream_tools.sse"))
	p, logs := newProvider(t, chatCfg(srv.URL, "openai_chat"))
	_, err := p.Chat(context.Background(), ChatInput{
		ModelCode: "m-chat", Messages: userMsg("加一张图"), Tools: addNodeTool,
		Options: map[string]any{"api_key": "sk-should-not-appear-123456"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	l := logs.all()
	if len(l) != 1 {
		t.Fatalf("每次 Chat 应落一条日志，得到 %d", len(l))
	}
	e := l[0]
	if e.OperationCode != OpAgentChat || e.GenerationID != 0 || e.ModelCode != "m-chat" ||
		e.ErrorType != string(transport.ErrorNone) || e.HTTPStatus != 200 {
		t.Fatalf("日志字段不对：%+v", e)
	}
	if strings.Contains(e.ResponseBody, "chat.completion.chunk") || strings.Contains(e.ResponseBody, "data:") {
		t.Fatalf("响应不应记整条流：%s", e.ResponseBody)
	}
	if !strings.Contains(e.ResponseBody, "finish=tool_calls") ||
		!strings.Contains(e.ResponseBody, "add_node,connect_nodes") {
		t.Fatalf("响应摘要应含结束原因与工具名：%s", e.ResponseBody)
	}
	if strings.Contains(e.RequestBody, "sk-should-not-appear") || !strings.Contains(e.RequestBody, "add_node") {
		t.Fatalf("请求体应脱敏且保留内容：%s", e.RequestBody)
	}
}

// 中转常在 HTTP 200 的流里推 error 事件：必须报错，不能当成空回复。
func TestChat_流里的错误事件返回错误(t *testing.T) {
	srv, hits := sseServer(t, nil, chatFixture(t, "openai_stream_error.sse"))
	cfg := chatCfg(srv.URL, "openai_chat")
	cfg.MaxRetries = 2
	p, logs := newProvider(t, cfg)
	_, err := p.Chat(context.Background(), ChatInput{ModelCode: "m-chat", Messages: userMsg("hi")}, nil)
	var ce *ChatError
	if !errors.As(err, &ce) || ce.Kind != transport.ErrorStream || ce.Upstream == nil {
		t.Fatalf("应是带上游错误的 stream 类 ChatError，得到 %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("流里报错不得重发，上游收到 %d 次", hits.Load())
	}
	if l := logs.all(); len(l) != 1 || l[0].ErrorType != "stream" {
		t.Fatalf("应落一条 stream 类日志：%+v", l)
	}
}

// 断在半路（没有结束标记、也没有 finish_reason）不能当成功：工具参数可能只拼了一半。
func TestChat_流中途断开判失败(t *testing.T) {
	body := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"add_node","arguments":"{\"ty"}}]}}]}` + "\n\n"
	srv, _ := sseServer(t, nil, body)
	p, _ := newProvider(t, chatCfg(srv.URL, "openai_chat"))
	_, err := p.Chat(context.Background(), ChatInput{ModelCode: "m-chat", Messages: userMsg("hi")}, nil)
	if !errors.Is(err, errStreamTruncated) {
		t.Fatalf("应判流中断，得到 %v", err)
	}
}

// 个别中转忽略 stream:true、直接回一整段 JSON：按非流式解析而不是判失败。
func TestChat_上游回整段JSON时按非流式解析(t *testing.T) {
	body := chatFixture(t, "openai_tool_calls.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	p, _ := newProvider(t, chatCfg(srv.URL, "openai_chat"))
	out, err := p.Chat(context.Background(), ChatInput{ModelCode: "m-chat", Messages: userMsg("hi")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.FinishReason != protocol.FinishToolCalls || len(out.ToolCalls) == 0 {
		t.Fatalf("应解析出工具调用：%+v", out)
	}
}
