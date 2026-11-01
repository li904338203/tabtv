//go:build integration

// 两级限流（真 Redis）与调用审计（真库）的集成测试（MCPT-06）。
//
// ⚠️ 审计走真实的 /mcp 路由与真实的 CallLogger → mcp_call_log：
// 「每次调用都有一条」必须由统一拦截点给出，绕开路由直接调 Record 的测试把拦截点整条删掉照样绿。
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	collabrepo "github.com/li904338203/tabtv/backend/internal/collaboration/repo"
	mcpapp "github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	mcphttp "github.com/li904338203/tabtv/backend/internal/mcp/transport"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
)

// ---------------------------------------------------------------- 限流（真 Redis）

var nsSeq struct {
	sync.Mutex
	n int
}

// newRedis 给每个用例一个独立命名空间（同一个 Redis 容器在用例间共用）。
func newRedis(t *testing.T) (*redis.Client, func(...string) string) {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: testsupport.RedisAddr(t)})
	t.Cleanup(func() { _ = rdb.Close() })
	nsSeq.Lock()
	nsSeq.n++
	ns := t.Name() + ":" + strconv.Itoa(nsSeq.n) + ":" + strconv.FormatInt(time.Now().UnixNano(), 36)
	nsSeq.Unlock()
	return rdb, func(parts ...string) string { return ns + ":" + strings.Join(parts, ":") }
}

// fixedClock 停在某分钟的第 10 秒：窗口切换靠拨表，不靠 sleep 卡边界。
func fixedClock() (*time.Time, func() time.Time) {
	now := time.Date(2026, 10, 17, 8, 30, 10, 0, time.UTC)
	return &now, func() time.Time { return now }
}

// 同一 grant 一分钟内第 61 次调用被拒：HTTP 429 + Retry-After + retry_after_seconds（1~60），
// 另一个工作区的授权不受影响（MCP-F-75）。走真实路由与真实 Redis。
func TestMCPRateLimit_用户客户端级超限返回retry_after(t *testing.T) {
	rdb, key := newRedis(t)
	lim := mcpapp.NewLimiter(rdb, key, 60, 300, nil)
	e := newEnv(t, func(_ *env, o *mcphttp.Options) { o.Limiter = lim })
	c := e.createKey(e.owner, nil)

	for i := 1; i <= 60; i++ {
		if code, b := e.toolCall(c.Plaintext, "canvas_read", `{"projectId":"1"}`); code != http.StatusOK {
			t.Fatalf("第 %d 次应放行：%d %s", i, code, b)
		}
	}
	resp, b := e.toolCallResp(c.Plaintext, "canvas_read", `{"projectId":"1"}`)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("第 61 次应 429：%d %s", resp.StatusCode, b)
	}
	ra, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || ra < 1 || ra > 60 {
		t.Fatalf("Retry-After = %q", resp.Header.Get("Retry-After"))
	}
	var body struct {
		Code    int            `json:"code"`
		Details map[string]any `json:"details"`
	}
	_ = json.Unmarshal([]byte(b), &body)
	if body.Code != int(errcode.RateLimited) || body.Details["retry_after_seconds"] != float64(ra) ||
		body.Details["scope"] != mcpapp.RateScopeUserClient {
		t.Fatalf("限流响应：%s", b)
	}

	// 另一个工作区（同事自己的团队）的授权照常。
	ws2, err := collabrepo.New(e.pool).CreateTeam(context.Background(), [3]int64{e.next(), e.next(), e.next()}, "二组", e.mate)
	if err != nil {
		t.Fatal(err)
	}
	other := e.createKey(e.mate, map[string]any{"workspaceId": strconv.FormatInt(ws2.ID, 10)})
	if code, b := e.toolCall(other.Plaintext, "canvas_read", `{"projectId":"1"}`); code != http.StatusOK {
		t.Fatalf("另一工作区不应受影响：%d %s", code, b)
	}
}

func TestMCPRateLimit_工作区级超限(t *testing.T) {
	rdb, key := newRedis(t)
	lim := mcpapp.NewLimiter(rdb, key, 10, 2, nil)
	_, clock := fixedClock()
	lim.SetClock(clock)
	ctx := context.Background()
	a := mcpapp.Principal{UserID: 1, WorkspaceID: 100, GrantID: 1}
	b := mcpapp.Principal{UserID: 2, WorkspaceID: 100, GrantID: 2}
	other := mcpapp.Principal{UserID: 3, WorkspaceID: 200, GrantID: 3}
	if !lim.Allow(ctx, a).Allowed || !lim.Allow(ctx, a).Allowed {
		t.Fatal("前两次应放行")
	}
	d := lim.Allow(ctx, b) // 另一个成员、另一个客户端，但同一个工作区
	if d.Allowed || d.Scope != mcpapp.RateScopeWorkspace || d.Limit != 2 || d.RetryAfter != 50 {
		t.Fatalf("工作区超限应拒：%+v", d)
	}
	// 被拒的那次不计数：b 的「用户 × 客户端」计数仍是 0（不存在）。
	n, _ := rdb.Get(ctx, key("mcp", "rl", "u", "2", "2", strconv.FormatInt(clock().Unix()/60, 10))).Int()
	if n != 0 {
		t.Fatalf("被拒的调用不应计数，实际 %d", n)
	}
	if !lim.Allow(ctx, other).Allowed {
		t.Fatal("另一工作区不受影响")
	}
}

func TestMCPRateLimit_窗口过后恢复(t *testing.T) {
	rdb, key := newRedis(t)
	lim := mcpapp.NewLimiter(rdb, key, 1, 0, nil)
	now, clock := fixedClock()
	lim.SetClock(clock)
	ctx := context.Background()
	p := mcpapp.Principal{UserID: 1, WorkspaceID: 100, GrantID: 1}
	if !lim.Allow(ctx, p).Allowed {
		t.Fatal("第一次应放行")
	}
	d := lim.Allow(ctx, p)
	if d.Allowed || d.RetryAfter != 50 || d.Scope != mcpapp.RateScopeUserClient {
		t.Fatalf("应被拒且 50 秒后重试：%+v", d)
	}
	// 键自带过期（窗口内首次 INCR 时设），不会在 Redis 里永久留下。
	ttl, err := rdb.TTL(ctx, key("mcp", "rl", "u", "1", "1", strconv.FormatInt(now.Unix()/60, 10))).Result()
	if err != nil || ttl <= 0 || ttl > time.Minute {
		t.Fatalf("计数键 TTL = %s（%v）", ttl, err)
	}
	// 0 = 不限：工作区那一级不建键。
	if n, _ := rdb.Exists(ctx, key("mcp", "rl", "w", "100", strconv.FormatInt(now.Unix()/60, 10))).Result(); n != 0 {
		t.Fatal("阈值为 0 的那一级不应计数")
	}
	*now = now.Add(time.Duration(d.RetryAfter) * time.Second) // 拨到下一分钟的第 0 秒
	if d := lim.Allow(ctx, p); !d.Allowed {
		t.Fatalf("下一个窗口应恢复：%+v", d)
	}
}

func TestMCPRateLimit_Redis不可用放行(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	var logs bytes.Buffer
	lim := mcpapp.NewLimiter(rdb, func(p ...string) string { return strings.Join(p, ":") }, 1, 1,
		slog.New(slog.NewTextHandler(&logs, nil)))
	p := mcpapp.Principal{UserID: 1, WorkspaceID: 100, GrantID: 1}
	for i := 0; i < 3; i++ {
		if d := lim.Allow(context.Background(), p); !d.Allowed {
			t.Fatalf("Redis 不可用时应放行：%+v", d)
		}
	}
	if c := strings.Count(logs.String(), "level=WARN"); c != 1 {
		t.Fatalf("应有且只有一条 Warn（每分钟节流），实际 %d：%s", c, logs.String())
	}

	// 走真实路由：调用照常成功。
	e := newEnv(t, func(_ *env, o *mcphttp.Options) { o.Limiter = lim })
	c := e.createKey(e.owner, nil)
	if code, b := e.toolCall(c.Plaintext, "canvas_read", `{"projectId":"1"}`); code != http.StatusOK {
		t.Fatalf("Redis 不可用时调用应成功：%d %s", code, b)
	}
}

// ---------------------------------------------------------------- 调用审计（真库）

// withCalls 装上真实的 CallLogger（写真库），写协程随用例启停。
func withCalls(e *env, o *mcphttp.Options) {
	l := mcpapp.NewCallLogger(e.store, e.next, nil, 0)
	go l.Run(context.Background())
	e.calls = l
	o.Calls = l
	e.t.Cleanup(l.Close)
}

type callRow struct {
	GrantID, UserID, WorkspaceID int64
	ClientName, Tool             string
	Method                       *string
	Fingerprint                  []byte
	Summary                      *string
	Credits                      int
	Result                       string
	ErrorCode                    *int
	DurationMS                   int
	TraceID                      *string
}

func (e *env) callRows() []callRow {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(), `
		select grant_id, user_id, workspace_id, client_name, tool, method, args_fingerprint, args_summary,
		       credits, result, error_code, duration_ms, trace_id
		from mcp_call_log order by created_at, id`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []callRow
	for rows.Next() {
		var r callRow
		if err := rows.Scan(&r.GrantID, &r.UserID, &r.WorkspaceID, &r.ClientName, &r.Tool, &r.Method,
			&r.Fingerprint, &r.Summary, &r.Credits, &r.Result, &r.ErrorCode, &r.DurationMS, &r.TraceID); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// waitRows 轮询到出现 n 行（写入是异步的；不用固定 sleep）。
func (e *env) waitRows(n int) []callRow {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		rows := e.callRows()
		if len(rows) >= n || time.Now().After(deadline) {
			if len(rows) != n {
				e.t.Fatalf("期望 %d 条审计，实际 %d：%+v", n, len(rows), rows)
			}
			return rows
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestMCPCallLog_每次调用落一条(t *testing.T) {
	e := newEnv(t, withCalls)
	c := e.createKey(e.owner, nil)
	for i := 0; i < 3; i++ {
		if code, b := e.toolCall(c.Plaintext, "canvas_read", `{"projectId":"`+strconv.Itoa(i)+`"}`); code != http.StatusOK {
			t.Fatalf("%d %s", code, b)
		}
	}
	// 非 tools/call 不记。
	if code, _ := e.mcpCall(c.Plaintext); code != http.StatusOK {
		t.Fatal(code)
	}
	e.calls.Close()
	rows := e.waitRows(3)
	gid, _ := strconv.ParseInt(c.GrantID, 10, 64)
	seen := map[string]bool{}
	for _, r := range rows {
		if r.GrantID != gid || r.UserID != e.owner || r.WorkspaceID != e.ws || r.ClientName != "Claude Desktop" ||
			r.Tool != "canvas_read" || r.Result != domain.CallOK || r.ErrorCode != nil || r.Credits != 0 ||
			r.TraceID == nil || *r.TraceID == "" || len(r.Fingerprint) != 32 {
			t.Fatalf("审计行：%+v", r)
		}
		seen[string(r.Fingerprint)] = true
	}
	if len(seen) != 3 {
		t.Fatalf("三次参数不同，指纹应各不相同：%d", len(seen))
	}
}

func TestMCPCallLog_被拒与限流也记录(t *testing.T) {
	rdb, key := newRedis(t)
	e := newEnv(t, withCalls, func(_ *env, o *mcphttp.Options) {
		o.Limiter = mcpapp.NewLimiter(rdb, key, 2, 0, nil)
	})
	view := e.createKey(e.owner, map[string]any{"scopes": []string{"seeaitv:view"}})
	// 只有 view 的凭据点名调花钱工具：被拒（工具没执行）。
	if code, b := e.toolCall(view.Plaintext, "generate", `{"projectId":"1"}`); code != http.StatusOK ||
		!strings.Contains(b, `"isError":true`) {
		t.Fatalf("越权调用应回工具错误：%d %s", code, b)
	}
	if code, _ := e.toolCall(view.Plaintext, "canvas_read", `{"projectId":"1"}`); code != http.StatusOK {
		t.Fatal(code)
	}
	if code, _ := e.toolCall(view.Plaintext, "canvas_read", `{"projectId":"1"}`); code != http.StatusTooManyRequests {
		t.Fatalf("第三次应被限流：%d", code)
	}
	e.calls.Close()
	rows := e.waitRows(3)
	want := []struct {
		tool, result string
		code         int
	}{
		{"generate", domain.CallDenied, int(errcode.McpInsufficientScope)},
		{"canvas_read", domain.CallOK, 0},
		{"canvas_read", domain.CallRateLimited, int(errcode.RateLimited)},
	}
	for i, w := range want {
		r := rows[i]
		got := 0
		if r.ErrorCode != nil {
			got = *r.ErrorCode
		}
		if r.Tool != w.tool || r.Result != w.result || got != w.code {
			t.Errorf("第 %d 条：%s/%s/%d，期望 %+v", i, r.Tool, r.Result, got, w)
		}
	}
}

func TestMCPCallLog_参数只存指纹与脱敏摘要(t *testing.T) {
	e := newEnv(t, withCalls)
	c := e.createKey(e.owner, nil)
	const leaked = "sk_mcp_0123456789ABCDEFGHIJabcdefghij0123456789x"
	// token 是未声明的字段：工具按入参校验拒绝（40001），但审计照样要记且要打码。
	args := `{"projectId":"请用 ` + leaked + ` 和 Bearer abcdefghijklmnop 调用","token":"tok-plain-secret-1"}`
	if code, b := e.toolCall(c.Plaintext, "canvas_read", args); code != http.StatusOK {
		t.Fatalf("%d %s", code, b)
	}
	e.calls.Close()
	r := e.waitRows(1)[0]
	if r.Result != domain.CallError || r.ErrorCode == nil || *r.ErrorCode != int(errcode.InvalidArgument) {
		t.Fatalf("入参校验失败应记 error/40001：%+v", r)
	}
	if !bytes.Equal(r.Fingerprint, domain.Fingerprint("canvas_read", json.RawMessage(args))) {
		t.Fatal("指纹应等于 domain.Fingerprint(工具, 参数)")
	}
	if r.Summary == nil {
		t.Fatal("应有脱敏摘要")
	}
	for _, secret := range []string{leaked[7:], "abcdefghijklmnop", "tok-plain-secret-1"} {
		if strings.Contains(*r.Summary, secret) {
			t.Fatalf("摘要里有明文 %q：%s", secret, *r.Summary)
		}
	}
	if !strings.Contains(*r.Summary, "sk_mcp_***") || !strings.Contains(*r.Summary, `"token":"***"`) {
		t.Fatalf("摘要应保留结构并打码：%s", *r.Summary)
	}
}

// 写日志用的是独立 context：请求结束（ctx 被取消）之后审计照样落库（CLAUDE.md §5.22）。
func TestMCPCallLog_请求ctx取消后仍写入(t *testing.T) {
	gate := make(chan struct{})
	e := newEnv(t, func(e *env, o *mcphttp.Options) {
		withCalls(e, o)
		// 写协程等到请求结束、请求 ctx 被取消之后才开始写。
		o.Calls = gatedRecorder{next: e.calls, gate: gate}
	})
	c := e.createKey(e.owner, nil)
	ctx, cancel := context.WithCancel(context.Background())
	if code, b := e.toolCallCtx(ctx, c.Plaintext, "canvas_read", `{"projectId":"1"}`); code != http.StatusOK {
		t.Fatalf("%d %s", code, b)
	}
	cancel()
	close(gate)
	e.waitRows(1)
}

// gatedRecorder 把审计攒到 gate 打开才交给真正的写入器。
type gatedRecorder struct {
	next mcphttp.CallRecorder
	gate chan struct{}
}

func (g gatedRecorder) Record(en mcpapp.CallEntry) {
	go func() { <-g.gate; g.next.Record(en) }()
}

func TestMCPCallLog_清理30天前分批删除(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	now := time.Now()
	ins := func(age time.Duration, n int) {
		for i := 0; i < n; i++ {
			if _, err := e.pool.Exec(ctx, `insert into mcp_call_log (id, grant_id, user_id, workspace_id,
				client_name, tool, result, duration_ms, created_at) values ($1,1,1,1,'c','t','ok',1,$2)`,
				e.next(), now.Add(-age)); err != nil {
				t.Fatal(err)
			}
		}
	}
	ins(31*24*time.Hour, 5)
	ins(29*24*time.Hour, 2)
	before := now.Add(-domain.CallLogRetention)
	for i, want := range []int64{3, 2, 0} {
		n, err := e.store.CleanupCallLogBefore(ctx, before, 3)
		if err != nil || n != want {
			t.Fatalf("第 %d 批删了 %d（%v），期望 %d（单批不超过上限）", i+1, n, err, want)
		}
	}
	var left int
	if err := e.pool.QueryRow(ctx, `select count(*) from mcp_call_log`).Scan(&left); err != nil || left != 2 {
		t.Fatalf("29 天前的应保留：剩 %d（%v）", left, err)
	}
}

// ---------------------------------------------------------------- 请求工具

// toolCall 以 2026-07-28 无状态形态发一次 tools/call，返回状态码与响应体。
func (e *env) toolCall(bearer, tool, args string) (int, string) {
	e.t.Helper()
	resp, b := e.toolCallResp(bearer, tool, args)
	return resp.StatusCode, b
}

func (e *env) toolCallCtx(ctx context.Context, bearer, tool, args string) (int, string) {
	e.t.Helper()
	resp, b := e.do(ctx, bearer, tool, args)
	return resp.StatusCode, b
}

func (e *env) toolCallResp(bearer, tool, args string) (*http.Response, string) {
	e.t.Helper()
	return e.do(context.Background(), bearer, tool, args)
}

func (e *env) do(ctx context.Context, bearer, tool, args string) (*http.Response, string) {
	e.t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"_meta":{` +
		`"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},` +
		`"io.modelcontextprotocol/clientInfo":{"name":"it","version":"0"}},"name":"` + tool + `","arguments":` + args + `}}`
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.srv.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set(mcphttp.HeaderProtocolVersion, "2026-07-28")
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", tool)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}
