package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/mcp/repo"
)

type fakeCallStore struct {
	mu    sync.Mutex
	rows  []repo.CallLog
	err   error
	block chan struct{} // 非 nil 时每次写都等它
}

func (f *fakeCallStore) InsertCallLog(ctx context.Context, c repo.CallLog) error {
	if f.block != nil {
		<-f.block
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.rows = append(f.rows, c)
	return nil
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func seq() func() int64 {
	var mu sync.Mutex
	var n int64
	return func() int64 { mu.Lock(); defer mu.Unlock(); n++; return n }
}

func TestCallLogger_缓冲满时丢弃不阻塞(t *testing.T) {
	store := &fakeCallStore{block: make(chan struct{})}
	var logs syncBuf
	l := NewCallLogger(store, seq(), slog.New(slog.NewTextHandler(&logs, nil)), 2)
	go l.Run(context.Background())

	// 写协程卡在第一条上：缓冲 2 条，其余必须立刻丢弃而不是让 Record 等。
	start := time.Now()
	for i := 0; i < 10; i++ {
		l.Record(CallEntry{Tool: "t", Result: domain.CallOK})
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Record 被阻塞了 %s", d)
	}
	// 第一条可能已被写协程取走（卡在 store 里），因此丢弃数在 7~8 之间。
	if n := l.Dropped(); n < 7 || n > 8 {
		t.Fatalf("丢弃 %d 条，期望 7~8", n)
	}
	if !strings.Contains(logs.String(), "缓冲已满") {
		t.Fatalf("缓冲满要告警，日志：%s", logs.String())
	}
	if c := strings.Count(logs.String(), "缓冲已满"); c != 1 {
		t.Fatalf("告警应节流成 1 条，实际 %d", c)
	}
	close(store.block)
	l.Close()
	if got := int64(len(store.rows)) + l.Dropped(); got != 10 {
		t.Fatalf("落库 %d + 丢弃 %d ≠ 10", len(store.rows), l.Dropped())
	}
	// 关闭之后再记：丢弃，不 panic。
	l.Record(CallEntry{Tool: "t"})
}

func TestCallLogger_写失败只告警(t *testing.T) {
	store := &fakeCallStore{err: errors.New("db down")}
	var logs syncBuf
	l := NewCallLogger(store, seq(), slog.New(slog.NewTextHandler(&logs, nil)), 0)
	go l.Run(context.Background())
	l.Record(CallEntry{GrantID: 7, Tool: "canvas_read", Result: domain.CallOK})
	l.Close()
	out := logs.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "写 MCP 调用审计失败") ||
		!strings.Contains(out, "db down") {
		t.Fatalf("写失败应记 Warn，日志：%s", out)
	}
	if strings.Contains(out, "level=ERROR") {
		t.Fatalf("写失败不应记 Error：%s", out)
	}
}

func TestCallLogger_独立context与字段(t *testing.T) {
	store := &fakeCallStore{}
	l := NewCallLogger(store, seq(), nil, 0)
	go l.Run(context.Background())
	args := json.RawMessage(`{"projectId":"1","token":"abc","note":"sk_mcp_0123456789abcdefABCDEF"}`)
	at := time.Date(2026, 10, 17, 1, 2, 3, 0, time.UTC)
	l.Record(CallEntry{GrantID: 1, UserID: 2, WorkspaceID: 3, ClientName: strings.Repeat("客", 120),
		Tool: "canvas_read", Method: "outline", Args: args, Result: domain.CallDenied, ErrorCode: 40307,
		Duration: 1500 * time.Millisecond, TraceID: "tid", At: at})
	l.Close()
	if len(store.rows) != 1 {
		t.Fatalf("落库 %d 条", len(store.rows))
	}
	r := store.rows[0]
	if r.ID == 0 || r.GrantID != 1 || r.UserID != 2 || r.WorkspaceID != 3 || r.Method != "outline" ||
		r.Result != domain.CallDenied || r.ErrorCode != 40307 || r.DurationMS != 1500 || !r.CreatedAt.Equal(at) {
		t.Fatalf("字段不对：%+v", r)
	}
	if n := len([]rune(r.ClientName)); n != maxClientLen {
		t.Fatalf("客户端名应截到 %d 字符，实际 %d", maxClientLen, n)
	}
	if !bytes.Equal(r.ArgsFingerprint, domain.Fingerprint("canvas_read", args)) {
		t.Fatal("指纹应复用 domain.Fingerprint")
	}
	if strings.Contains(r.ArgsSummary, "abc") || strings.Contains(r.ArgsSummary, "0123456789abcdef") {
		t.Fatalf("摘要未脱敏：%s", r.ArgsSummary)
	}
}
