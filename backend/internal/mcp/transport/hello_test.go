package transport

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// 兼容矩阵的「声明值」列靠这条日志（MCPT-15）：两个协议版本都要记下客户端名、版本、协议版本与 elicitation 声明；
// tools/call 在 Debug 级额外记是否带 progressToken。
func TestClientHello_两个协议版本都记下声明值(t *testing.T) {
	for _, v := range []string{v2026, v2025} {
		t.Run(v, func(t *testing.T) {
			buf := &syncBuf{}
			srv := serve(t, Options{Auth: auth, Logger: slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))})
			cs, _, err := connect(t, srv.URL+"/mcp", v, "all", nil)
			if err != nil {
				t.Fatal(err)
			}
			p := &mcp.CallToolParams{Name: "canvas_read", Arguments: map[string]any{"projectId": "1"}}
			p.SetProgressToken("tok-1")
			if _, err := cs.CallTool(context.Background(), p); err != nil {
				t.Fatal(err)
			}
			out := buf.String()
			var hello, callLine string
			for _, l := range strings.Split(out, "\n") {
				if !strings.Contains(l, "msg=mcp.client_hello") {
					continue
				}
				if strings.Contains(l, "level=INFO") && hello == "" {
					hello = l
				}
				if strings.Contains(l, "method=tools/call") {
					callLine = l
				}
			}
			if hello == "" || !strings.Contains(hello, "client=mcpt02-test") || !strings.Contains(hello, "protocolVersion="+v) ||
				!strings.Contains(hello, "elicitationForm=false") {
				t.Fatalf("握手日志：%q\n全部：%s", hello, out)
			}
			if !strings.Contains(callLine, "level=DEBUG") || !strings.Contains(callLine, "wantsProgress=true") ||
				!strings.Contains(callLine, "tool=canvas_read") || !strings.Contains(callLine, "client=mcpt02-test") {
				t.Fatalf("调用日志：%q", callLine)
			}
			if strings.Contains(out, "Bearer") || strings.Contains(out, "Authorization") {
				t.Fatal("日志里出现了凭据")
			}
		})
	}
}
