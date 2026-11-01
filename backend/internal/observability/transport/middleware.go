// Package transport 提供请求日志中间件与管理端接口（P13-04；AD-07）。
package transport

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/li904338203/tabtv/backend/internal/observability/app"
	"github.com/li904338203/tabtv/backend/internal/observability/domain"
	"github.com/li904338203/tabtv/backend/internal/observability/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/logredact"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// SubjectIDs 由身份域提供：从上下文取当前用户/管理员/工作区。
//
// ⚠️ 三个都可能是 0（未登录的公开接口、没有工作区上下文的后台接口），
// 那是**正常结果**不是缺陷 —— 日志里空着恰恰说明"这次是匿名访问"。
type SubjectIDs func(ctx context.Context) (userID, adminID, workspaceID int64)

// Logger 返回请求日志中间件（Handler 方法版，供路由组装处直接用 ——
// 那里拿得到 Handler 但拿不到 Service）。
func (h *Handler) Logger(subjects SubjectIDs) func(http.Handler) http.Handler {
	return Logger(h.svc, subjects)
}

// Logger 返回请求日志中间件。
//
// ⚠️ 它必须挂在 `httpx.TraceID` **之后**：日志的第一价值就是按 traceId 把
// 「API 请求 → 生成任务 → Provider 调用」串起来（AC-26）。挂在前面的话
// 每条日志的 traceId 都是空的，而那时这张表基本没有用。
//
// ⚠️ 写日志**在响应之后、异步之前**：同步写会给每个请求加一次数据库往返，
// 而它对用户毫无价值。这里用 goroutine + 独立超时 —— 请求的 ctx 在响应
// 返回后会被取消，拿它去写库的话**每一条日志都会因为 context canceled 失败**，
// 而失败只在日志里留一行 warn（于是"日志表一直是空的"要查很久）。
func Logger(svc *app.Service, subjects SubjectIDs) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !domain.ShouldLog(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			start := time.Now()

			var body []byte
			if domain.ShouldCaptureBody(r.Method, r.Header.Get("Content-Type")) {
				// ⚠️ 读完必须**把 body 放回去**，否则后面的 handler 读到空 ——
				// 表现为"所有 POST 都报参数非法"，而看起来像校验代码写错了。
				// ⚠️ 上限是**内存**上限（与入库截断是两件事）：一张内联 base64
				// 参考图就是几 MB，不设限并发几路就把进程撑爆。
				limited := io.LimitReader(r.Body, domain.MaxCapturedBody)
				body, _ = io.ReadAll(limited)
				rest, _ := io.ReadAll(r.Body)
				_ = r.Body.Close()
				r.Body = io.NopCloser(bytes.NewReader(append(body, rest...)))
			}

			r2, bizSlot := httpx.WithBizCodeSlot(r)
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r2)

			userID, adminID, wsID := int64(0), int64(0), int64(0)
			if subjects != nil {
				userID, adminID, wsID = subjects(r2.Context())
			}
			entry := repo.APILog{
				TraceID: httpx.TraceIDFrom(r2.Context()),
				Method:  r.Method, Path: r.URL.Path,
				HTTPStatus: rec.status, BizCode: *bizSlot,
				DurationMs: int(time.Since(start).Milliseconds()),
				UserID:     userID, AdminID: adminID, WorkspaceID: wsID,
				IP: clientIP(r),
			}
			if len(body) > 0 {
				// ⚠️ 脱敏走 `logredact`（与 Provider 日志**同一份规则**）：
				// 各抄一份的话某一类密钥会在一条日志里被打码、在另一条里
				// 原样躺着，而两边看起来都在工作。
				entry.RequestBody = logredact.RedactJSON(body)
			}
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				svc.WriteAPILog(ctx, entry)
			}()
		})
	}
}

// statusRecorder 只记状态码，**不缓存响应体**。
//
// ⚠️ 缓存响应体等于在内存里再复制一份产出（图片/音视频直接吐字节），
// 一次几百 MB 的成片下载就能把 API 打爆。要看响应内容去看 Provider 日志
// （那一侧体积可控且必须留）。
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.written {
		s.status = code
		s.written = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.written = true
	return s.ResponseWriter.Write(b)
}

// Flush 透传给底层 —— ⚠️ 不透传的话 SSE / 流式下载会被缓冲住，
// 表现为"进度条一直不动然后一次性跳完"。
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// clientIP 取客户端 IP。
//
// ⚠️ `X-Forwarded-For` 取**第一个**（最初的客户端），后面的都是代理链。
// 取最后一个的话所有请求的 IP 都是网关地址 —— 而那时这一列毫无意义。
// ⚠️ 它是**客户端可伪造**的头：这一列只用于排障参考，
// **不能拿来做任何授权判断**。
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return strings.TrimSpace(ip)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
