package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 只有「在 API 进程里同步调模型」的端点放宽超时，其余仍是 30 秒。
//
// ⚠️ 起因：全局 30 秒超时在模型写分镜写到一半时掐断请求（真实模型常常要一两分钟，假上游秒回所以测试里从没撞上）。
// 放宽要**精确**：把别的写接口也放宽到 15 分钟的话，一个卡住的请求会占着连接与数据库事务那么久。
func TestRequestTimeout_同步调模型的端点才放宽(t *testing.T) {
	var remaining time.Duration
	h := requestTimeout(30*time.Second, 15*time.Minute)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		deadline, ok := r.Context().Deadline()
		if !ok {
			t.Fatal("每个请求都应当有上限")
		}
		remaining = time.Until(deadline)
	}))
	cases := []struct {
		method, path string
		long         bool
	}{
		{http.MethodPost, "/api/v1/scripts/103561941909241856/storyboard", true},
		{http.MethodPost, "/api/v1/scripts/103561941909241856/prompts/compose", true},
		{http.MethodGet, "/api/v1/scripts/103561941909241856/storyboard", false},
		{http.MethodPost, "/api/v1/scripts", false},
		{http.MethodPost, "/api/v1/scripts/1/storyboard/extra", false},
		{http.MethodPost, "/api/v1/generations", false},
		{http.MethodPost, "/api/v1/agent/prompt-optimize", true},
		{http.MethodPost, "/api/v1/agent/sessions", false},
		// 3D 导演台：预演与泼溅上传放宽，截图、保存场景照常 30 秒。
		{http.MethodPost, "/api/v1/projects/1/director-scenes/2/previews", true},
		{http.MethodPost, "/api/v1/projects/1/director-scenes/2/assets", true},
		{http.MethodPost, "/api/v1/projects/1/director-scenes/2/captures", false},
		{http.MethodPut, "/api/v1/projects/1/director-scenes/2", false},
		{http.MethodPost, "/api/v1/projects/1/director-scenes/2/assets/x", false},
		// AI 搭景同步调模型：放宽；可用性查询是普通 GET。
		{http.MethodPost, "/api/v1/projects/1/director-scenes/2/build", true},
		// 智能剪辑的 AI 剪辑、方案切换、追加指令、翻译（SCT-27）：同步调模型；同步素材与导出不放宽。
		{http.MethodPost, "/api/v1/projects/1/smartcuts/2/plan", true},
		{http.MethodPost, "/api/v1/projects/1/smartcuts/2/plan/3/apply", true},
		{http.MethodPost, "/api/v1/projects/1/smartcuts/2/edit", true},
		{http.MethodPost, "/api/v1/projects/1/smartcuts/2/translate", true},
		{http.MethodPost, "/api/v1/projects/1/smartcuts/2/sync", false},
		{http.MethodGet, "/api/v1/projects/1/smartcuts/2/plan", false},
		{http.MethodGet, "/api/v1/director/build-config", false},
	}
	for _, c := range cases {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(c.method, c.path, nil))
		if got := remaining > time.Minute; got != c.long {
			t.Errorf("%s %s：剩余 %s，应%s放宽", c.method, c.path, remaining.Round(time.Second), map[bool]string{true: "", false: "不"}[c.long])
		}
	}
}

// /mcp 的**所有方法**整体跳过全局超时（开发 01 §12 R20 ③）：30 秒外壳会在流中途写 504，
// 而 GET SSE / subscriptions/listen 是合法长连接。单次调用的上限由 MCP 自己的中间件给。
// 只匹配根上的 /mcp，`/api/v1/...` 下同名前缀的接口照常有上限。
func TestRequestTimeout_MCP端点跳过全局超时(t *testing.T) {
	var has bool
	h := requestTimeout(30*time.Second, 15*time.Minute)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, has = r.Context().Deadline()
	}))
	cases := []struct {
		method, path string
		deadline     bool
	}{
		{http.MethodPost, "/mcp", false},
		{http.MethodGet, "/mcp", false},
		{http.MethodDelete, "/mcp", false},
		{http.MethodPost, "/mcp/readonly", false},
		{http.MethodGet, "/mcp/x/canvas", false},
		{http.MethodPost, "/mcpx", true},
		{http.MethodGet, "/api/v1/mcp/approvals/1", true},
		{http.MethodGet, "/healthz", true},
	}
	for _, c := range cases {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(c.method, c.path, nil))
		if has != c.deadline {
			t.Errorf("%s %s：有截止时间=%v，期望 %v", c.method, c.path, has, c.deadline)
		}
	}
}

// 慢速上传在全局超时下会失败（DRT-03 的实测）：请求体要读几秒，读完时 ctx 已经过期，
// 接下来写对象存储的调用拿到的是一个死掉的 ctx。放宽的路由读完之后 ctx 仍然有效。
//
// 用真的 HTTP 连接与一个慢慢写的请求体，而不是 httptest.NewRequest：后者的 body 是内存里现成的，
// 读多久都是零，测不出这件事。上限按比例缩小（300ms 对 30 秒），慢写的节奏也按比例缩小。
func TestRequestTimeout_慢速上传撞上全局超时(t *testing.T) {
	ctxErr := make(chan error, 1)
	h := requestTimeout(300*time.Millisecond, 15*time.Minute)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // 读完整个请求体（等同 ParseMultipartForm）
		ctxErr <- r.Context().Err()        // 然后才会去写对象存储
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	upload := func(path string) error {
		pr, pw := io.Pipe()
		go func() {
			for i := 0; i < 6; i++ { // 6 × 100ms ≈ 600ms，超过缩小后的 300ms 上限
				time.Sleep(100 * time.Millisecond)
				if _, err := pw.Write(make([]byte, 1024)); err != nil {
					return
				}
			}
			_ = pw.Close()
		}()
		resp, err := http.Post(srv.URL+path, "application/octet-stream", pr)
		if err == nil {
			_ = resp.Body.Close()
		}
		return <-ctxErr
	}
	if err := upload("/api/v1/media/references"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("普通路由上传 600ms 应撞上 300ms 上限，读完请求体时 ctx 已过期，得到 %v", err)
	}
	if err := upload("/api/v1/projects/1/director-scenes/2/assets"); err != nil {
		t.Fatalf("泼溅上传放宽之后，读完请求体时 ctx 应仍有效，得到 %v", err)
	}
}
