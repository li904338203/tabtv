package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

type memRecorder struct {
	mu sync.Mutex
	es []app.CallEntry
}

func (m *memRecorder) Record(e app.CallEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.es = append(m.es, e)
}

func (m *memRecorder) all() []app.CallEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]app.CallEntry(nil), m.es...)
}

// 每次 tools/call 都由统一拦截点记一条：成功、越权被拒、工具不存在 —— 工具自己一行审计都不写。
// 2025 会话里每次调用的 traceId 必须是**本次请求**的，不是 initialize 那一次的。
func TestMCPHandler_每次调用都记审计(t *testing.T) {
	for _, v := range []string{v2026, v2025} {
		t.Run(v, func(t *testing.T) {
			rec := &memRecorder{}
			srv := serve(t, Options{Auth: auth, Calls: rec})
			cs, _, err := connect(t, srv.URL+"/mcp", v, "all", nil)
			if err != nil {
				t.Fatal(err)
			}
			call(t, cs, "canvas_read", map[string]any{"projectId": "1"})
			call(t, cs, "canvas_read", map[string]any{"projectId": "2"})
			// 工具不存在：SDK 回协议错误，照样要记。
			_, _ = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "no_such_tool"})
			// tools/list 不记。
			toolNames(t, cs)

			viewer, _, err := connect(t, srv.URL+"/mcp", v, "viewer-role", nil)
			if err != nil {
				t.Fatal(err)
			}
			// 花钱工具对 viewer 不可见，但被点名调用时要被拒并记成 denied。
			if r := call(t, viewer, "generate", map[string]any{"nodeId": "n1"}); !r.IsError {
				t.Fatal("viewer 调花钱工具应被拒")
			}

			es := rec.all()
			if len(es) != 4 {
				t.Fatalf("记了 %d 条，期望 4：%+v", len(es), es)
			}
			want := []struct {
				tool, result string
				grant        int64
				code         int
			}{
				{"canvas_read", domain.CallOK, 10, 0},
				{"canvas_read", domain.CallOK, 10, 0},
				{"no_such_tool", domain.CallError, 10, 0},
				{"generate", domain.CallDenied, 13, int(errcode.McpInsufficientScope)},
			}
			for i, w := range want {
				e := es[i]
				if e.Tool != w.tool || e.Result != w.result || e.GrantID != w.grant || e.ErrorCode != w.code {
					t.Errorf("第 %d 条：%+v，期望 %+v", i, e, w)
				}
				if e.TraceID == "" || e.At.IsZero() || e.Duration < 0 {
					t.Errorf("第 %d 条缺 traceId / 时间：%+v", i, e)
				}
			}
			if es[0].TraceID == es[1].TraceID {
				t.Errorf("两次调用的 traceId 相同（%s）：会话里沿用了 initialize 的 traceId", es[0].TraceID)
			}
			if !strings.Contains(string(es[0].Args), `"projectId":"1"`) {
				t.Errorf("参数没有交给审计：%s", es[0].Args)
			}
		})
	}
}

// classify 的分类表（denied 只认 403 类业务码；限流码单列）。
func TestClassify_调用结果分类(t *testing.T) {
	isErr := &mcp.CallToolResult{IsError: true}
	ok := &mcp.CallToolResult{}
	cases := []struct {
		name    string
		res     *mcp.CallToolResult
		err     error
		confirm bool
		code    int
		want    string
		wantErr int
	}{
		{"成功", ok, nil, false, 0, domain.CallOK, 0},
		{"需确认", ok, nil, true, 0, domain.CallNeedsConfirm, 0},
		{"越权", isErr, nil, false, int(errcode.McpInsufficientScope), domain.CallDenied, int(errcode.McpInsufficientScope)},
		{"工具集未开", isErr, nil, false, int(errcode.PermissionDenied), domain.CallDenied, int(errcode.PermissionDenied)},
		{"限流", isErr, nil, false, int(errcode.RateLimited), domain.CallRateLimited, int(errcode.RateLimited)},
		{"参数错", isErr, nil, false, int(errcode.InvalidArgument), domain.CallError, int(errcode.InvalidArgument)},
		{"没回报码", isErr, nil, false, 0, domain.CallError, 0},
		{"协议错误带业务码", nil, httpx.Err(errcode.NotFound), false, 0, domain.CallError, int(errcode.NotFound)},
	}
	for _, c := range cases {
		code := c.code
		var res mcp.Result
		if c.res != nil {
			res = c.res
		}
		got := classify(res, c.err, c.confirm, &code)
		if got != c.want || code != c.wantErr {
			t.Errorf("%s：得到 %s/%d，期望 %s/%d", c.name, got, code, c.want, c.wantErr)
		}
	}
}

// 限流在进 SDK 之前：超限的 tools/call 回 429 + Retry-After + retry_after_seconds，并记 rate_limited；
// 其他方法不计数。
func TestMCPHandler_限流返回429并记审计(t *testing.T) {
	lim := &stubLimiter{allow: 1}
	rec := &memRecorder{}
	srv := serve(t, Options{Auth: auth, Calls: rec, Limiter: lim})
	h := http.Header{"Authorization": {"Bearer all"}, HeaderProtocolVersion: {v2026}}
	body := func(id int) string {
		return `{"jsonrpc":"2.0","id":` + itoa(id) + `,"method":"tools/call","params":{` + meta2026 + `,"name":"canvas_read","arguments":{"projectId":"9"}}}`
	}
	// tools/list 不计数。
	hl := h.Clone()
	hl.Set("Mcp-Method", "tools/list")
	h.Set("Mcp-Method", "tools/call")
	h.Set("Mcp-Name", "canvas_read")
	resp, lb := rawRPC(t, http.MethodPost, srv.URL+"/mcp", hl, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{`+meta2026+`}}`)
	if resp.StatusCode != http.StatusOK || lim.calls() != 0 {
		t.Fatalf("tools/list：%d %s，计数 %d", resp.StatusCode, lb, lim.calls())
	}
	// 第一次放行（进了 SDK，由 auditCalls 记），第二次被限流。
	if resp, b := rawRPC(t, http.MethodPost, srv.URL+"/mcp", h, body(2)); resp.StatusCode != http.StatusOK {
		t.Fatalf("第一次应放行：%d %s", resp.StatusCode, b)
	}
	resp, b := rawRPC(t, http.MethodPost, srv.URL+"/mcp", h, body(3))
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("第二次应 429：%d %s", resp.StatusCode, b)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "17" {
		t.Fatalf("Retry-After = %q", ra)
	}
	var out struct {
		Code    int            `json:"code"`
		Name    string         `json:"name"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	}
	if err := json.Unmarshal([]byte(b), &out); err != nil {
		t.Fatal(err, b)
	}
	if out.Code != int(errcode.RateLimited) || out.Name != "RATE_LIMITED" || out.Details["retry_after_seconds"] != float64(17) ||
		out.Details["scope"] != app.RateScopeWorkspace || !strings.Contains(out.Message, "17 秒后重试") {
		t.Fatalf("限流响应：%s", b)
	}
	es := rec.all()
	if len(es) != 2 || es[0].Result != domain.CallOK || es[1].Result != domain.CallRateLimited ||
		es[1].Tool != "canvas_read" || es[1].ErrorCode != int(errcode.RateLimited) || es[1].GrantID != 10 {
		t.Fatalf("审计：%+v", es)
	}
	// 被限流之后请求体仍原样交给 SDK 的那条路不受影响：放开限流再调一次照常成功。
	lim.set(100)
	if resp, b := rawRPC(t, http.MethodPost, srv.URL+"/mcp", h, body(4)); resp.StatusCode != http.StatusOK ||
		!strings.Contains(b, `projectId`) {
		t.Fatalf("放开后应成功：%d %s", resp.StatusCode, b)
	}
}

// meta2026 是 2026-07-28 无状态请求必带的 _meta（没有 initialize，协议版本跟着每条消息走）。
const meta2026 = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"raw","version":"0"}}`

type stubLimiter struct {
	mu    sync.Mutex
	allow int
	n     int
}

func (s *stubLimiter) Allow(context.Context, app.Principal) app.RateDecision {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	if s.n <= s.allow {
		return app.RateDecision{Allowed: true}
	}
	return app.RateDecision{RetryAfter: 17, Scope: app.RateScopeWorkspace, Limit: s.allow}
}

func (s *stubLimiter) calls() int { s.mu.Lock(); defer s.mu.Unlock(); return s.n }
func (s *stubLimiter) set(n int)  { s.mu.Lock(); defer s.mu.Unlock(); s.allow = n }
