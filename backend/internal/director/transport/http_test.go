package transport

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 限流时除了业务码，还要有 Retry-After 头（同 MCP 的限流）：客户端与网关认的是这个头。
func TestFail_限流设RetryAfter头(t *testing.T) {
	for _, c := range []struct {
		code errcode.Code
		want string
	}{
		{errcode.RateLimited, "125"},
		{errcode.ConcurrencyLimited, "125"},
		{errcode.InvalidArgument, ""},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/x", nil)
		fail(w, r, httpx.Errf(c.code, "x").WithDetails(map[string]any{"retry_after_seconds": 125}))
		if got := w.Header().Get("Retry-After"); got != c.want {
			t.Errorf("%d：Retry-After 应为 %q，得到 %q", c.code, c.want, got)
		}
	}
}
