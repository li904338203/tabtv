package transport

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/li904338203/tabtv/backend/internal/mcp/app"
)

// 建立会话的响应一发出头，绑定就必须已经生效：客户端读到会话 ID 后立刻发下一条，
// 而那时建立会话的 handler 可能还没返回（SSE 流还开着、或只是负载高）。
// 旧实现等 ServeHTTP 返回才绑定，下一条请求会拿到 404「会话不存在」。
func TestSessionBinder_发头即绑定不等handler返回(t *testing.T) {
	b := newSessionBinder(time.Hour)
	release := make(chan struct{})
	headerSent := make(chan struct{})
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(HeaderSessionID) == "" {
			w.Header().Set(HeaderSessionID, "s1")
			w.WriteHeader(http.StatusOK)
			close(headerSent)
			<-release // 模拟 handler 迟迟不返回
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	h := b.wrap(next)
	withGrant := func(r *http.Request) *http.Request {
		return r.WithContext(app.WithPrincipal(r.Context(), app.Principal{GrantID: 7}))
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), withGrant(httptest.NewRequest(http.MethodPost, "/mcp", nil)))
	}()
	<-headerSent

	req := withGrant(httptest.NewRequest(http.MethodPost, "/mcp", nil))
	req.Header.Set(HeaderSessionID, "s1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	close(release)
	<-done
	if rec.Code != http.StatusAccepted {
		t.Fatalf("建立会话的请求尚未返回时，下一条请求应已能通过绑定，实际 %d %s", rec.Code, rec.Body.String())
	}
}
