package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	s := &server{addr: "http://fake"}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", s.chat)
	mux.HandleFunc("/v1/messages", s.anthropicMessages)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// sseData 读出全部 (event, data) 对。
func sseData(t *testing.T, resp *http.Response) [][2]string {
	t.Helper()
	defer resp.Body.Close()
	var out [][2]string
	event := ""
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			out = append(out, [2]string{event, strings.TrimPrefix(line, "data: ")})
		case line == "":
			event = ""
		}
	}
	return out
}

func post(t *testing.T, url string, header map[string]string, body any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestOpenAI_流式工具调用参数分三片(t *testing.T) {
	srv := newTestServer(t)
	resp := post(t, srv.URL+"/v1/chat/completions", nil, map[string]any{
		"model": "m", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": `加个节点 [[tool:add_node|{"type":"image"}]]`}},
		"tools":    []any{map[string]any{"type": "function", "function": map[string]any{"name": "add_node"}}},
	})
	var args strings.Builder
	pieces, name, finish, done := 0, "", "", false
	for _, ev := range sseData(t, resp) {
		if ev[1] == "[DONE]" {
			done = true
			continue
		}
		var c struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Function struct{ Name, Arguments string } `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(ev[1]), &c); err != nil {
			t.Fatal(err)
		}
		for _, ch := range c.Choices {
			for _, tc := range ch.Delta.ToolCalls {
				if tc.Function.Name != "" {
					name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					pieces++
					args.WriteString(tc.Function.Arguments)
				}
			}
			if ch.FinishReason != nil {
				finish = *ch.FinishReason
			}
		}
	}
	if !done || name != "add_node" || finish != "tool_calls" || pieces != 3 || args.String() != `{"type":"image"}` {
		t.Fatalf("done=%v name=%q finish=%q pieces=%d args=%q", done, name, finish, pieces, args.String())
	}
}

func TestOpenAI_工具结果回填后文本收尾(t *testing.T) {
	srv := newTestServer(t)
	resp := post(t, srv.URL+"/v1/chat/completions", nil, map[string]any{
		"model": "m", "stream": true,
		"messages": []any{
			map[string]any{"role": "user", "content": "[[tool:add_node]]"},
			map[string]any{"role": "tool", "tool_call_id": "c1", "content": `{"nodeId":"n1"}`},
		},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "add_node"}}},
	})
	all := ""
	for _, ev := range sseData(t, resp) {
		all += ev[1]
	}
	if !strings.Contains(all, `"finish_reason":"stop"`) || strings.Contains(all, "tool_calls") {
		t.Fatalf("工具结果之后应以文本收尾：%s", all)
	}
}

func TestOpenAI_非流式不带工具时行为不变(t *testing.T) {
	srv := newTestServer(t)
	resp := post(t, srv.URL+"/v1/chat/completions", nil, map[string]any{
		"model": "m", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	defer resp.Body.Close()
	var body struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if len(body.Choices) != 1 || !strings.Contains(body.Choices[0].Message.Content, "假上游回应") {
		t.Fatalf("单轮同步调用的回应变了：%+v", body)
	}
}

func TestAnthropic_流式tool_use(t *testing.T) {
	srv := newTestServer(t)
	resp := post(t, srv.URL+"/v1/messages", map[string]string{"anthropic-version": "2023-06-01"}, map[string]any{
		"model": "m", "max_tokens": 1024, "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "[[tool:list_nodes]]"}}}},
		"tools": []any{map[string]any{"name": "list_nodes", "input_schema": map[string]any{"type": "object"}}},
	})
	var args strings.Builder
	pieces, stop, sawStop, toolName := 0, "", false, ""
	for _, ev := range sseData(t, resp) {
		var e struct {
			Type         string                      `json:"type"`
			ContentBlock struct{ Type, Name string } `json:"content_block"`
			Delta        struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(ev[1]), &e); err != nil {
			t.Fatal(err)
		}
		if ev[0] != e.Type {
			t.Fatalf("event 名与 data.type 应一致：%s vs %s", ev[0], e.Type)
		}
		switch e.Type {
		case "content_block_start":
			if e.ContentBlock.Type == "tool_use" {
				toolName = e.ContentBlock.Name
			}
		case "content_block_delta":
			if e.Delta.Type == "input_json_delta" {
				pieces++
				args.WriteString(e.Delta.PartialJSON)
			}
		case "message_delta":
			stop = e.Delta.StopReason
		case "message_stop":
			sawStop = true
		}
	}
	if toolName != "list_nodes" || stop != "tool_use" || !sawStop || pieces != 3 || !json.Valid([]byte(args.String())) {
		t.Fatalf("tool=%q stop=%q sawStop=%v pieces=%d args=%q", toolName, stop, sawStop, pieces, args.String())
	}
}

func TestAnthropic_缺版本头或max_tokens时400(t *testing.T) {
	srv := newTestServer(t)
	body := map[string]any{"model": "m", "max_tokens": 10, "messages": []any{}}
	if resp := post(t, srv.URL+"/v1/messages", nil, body); resp.StatusCode != 400 {
		t.Fatalf("缺版本头应 400，得到 %d", resp.StatusCode)
	}
	delete(body, "max_tokens")
	if resp := post(t, srv.URL+"/v1/messages", map[string]string{"anthropic-version": "2023-06-01"}, body); resp.StatusCode != 400 {
		t.Fatalf("缺 max_tokens 应 400，得到 %d", resp.StatusCode)
	}
}
