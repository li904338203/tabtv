package transport

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// HeaderSessionID 是 2025 系列 Streamable HTTP 的会话头。
const HeaderSessionID = "Mcp-Session-Id"

// sessionBinder 把 2025 会话（Mcp-Session-Id）绑定到**建立会话时的授权**（grant）。
//
// ⚠️ 为什么要绑：会话 ID 由服务端签发、只在请求头里流转，SDK 的会话层只认 ID 不认人。
// 不绑的话，拿着**另一把凭据**的人只要知道（或截获）一个会话 ID，就能接着别人的会话往下走 ——
// 2025 的 elicitation 回答、进度通知、GET SSE 上推给原会话的消息都挂在会话上。每个请求虽然
// 仍按本次凭据重算权限，但「会话属于谁」这件事必须在会话层面成立，否则 elicitation 的确认
// （MCPT-10）可以被另一凭据代答。
//
// 绑定的是 grant 而不是某把具体的令牌：OAuth 刷新会轮换访问令牌（同一个 grant），
// 绑令牌的话每次刷新都会把正在进行的会话踢掉。
//
// 会话在 SDK 的进程内存里，绑定也只在进程内存里（同 R20 ⑦：多实例需按会话粘滞）。
// 绑定**只在建立会话时写一次**：从 initialize 的响应头里取 SDK 签发的会话 ID。
type sessionBinder struct {
	idle time.Duration
	now  func() time.Time

	mu        sync.Mutex
	sessions  map[string]*boundSession
	lastSweep time.Time
}

type boundSession struct {
	grantID  int64
	lastSeen time.Time
	inflight int // 进行中的请求（GET SSE 长连接期间不能被当成空闲回收）
}

func newSessionBinder(idle time.Duration) *sessionBinder {
	return &sessionBinder{idle: idle, now: time.Now, sessions: map[string]*boundSession{}}
}

// wrap 在会话 handler 外面做绑定检查。必须放在凭据中间件之后（要用 Principal.GrantID）。
func (b *sessionBinder) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := app.PrincipalFrom(r.Context())
		sid := r.Header.Get(HeaderSessionID)
		if sid == "" {
			// 建立会话（initialize）：响应头里出现会话 ID 时绑定到本次凭据的 grant。
			// ⚠️ 必须在响应头发出之前绑定，不能等 ServeHTTP 返回：响应体一冲出去，
			// 客户端就会发 notifications/initialized，那时还没绑定就是 404「会话不存在」
			// （负载高时才出现，make check 里实测踩到）。
			cw := &captureWriter{ResponseWriter: w, onHeader: func(id string) { b.bind(id, p.GrantID) }}
			next.ServeHTTP(cw, r)
			return
		}
		switch b.enter(sid, p.GrantID) {
		case sessionUnknown:
			// 没有绑定 = 不是经由这里建立的会话（进程重启、已回收、伪造）。按规范回 404，
			// 客户端会重新 initialize —— 新会话自然绑定到它当前的凭据。**不放行给 SDK**：
			// 放行的话「没有绑定」就成了绕过绑定的办法。
			httpx.Fail(w, r, httpx.Errf(errcode.NotFound, "MCP 会话不存在或已过期，请重新 initialize"))
			return
		case sessionOtherGrant:
			httpx.Fail(w, r, httpx.Errf(errcode.PermissionDenied,
				"该 MCP 会话由另一个凭据建立，不能用当前凭据继续：会话只属于建立它的那个授权。"+
					"请用当前凭据重新 initialize 建立自己的会话").
				WithDetails(map[string]any{"sessionId": sid, "grantId": strconv.FormatInt(p.GrantID, 10)}))
			return
		}
		cw := &captureWriter{ResponseWriter: w}
		defer func() { b.leave(sid, r.Method == http.MethodDelete && cw.status() < 300) }()
		next.ServeHTTP(cw, r)
	})
}

type sessionCheck int

const (
	sessionOK sessionCheck = iota
	sessionUnknown
	sessionOtherGrant
)

func (b *sessionBinder) bind(sid string, grantID int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.sessions[sid] = &boundSession{grantID: grantID, lastSeen: now}
	b.sweepLocked(now)
}

func (b *sessionBinder) enter(sid string, grantID int64) sessionCheck {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.sessions[sid]
	if !ok {
		return sessionUnknown
	}
	if s.grantID != grantID {
		return sessionOtherGrant
	}
	s.inflight++
	s.lastSeen = b.now()
	return sessionOK
}

// leave 结束一次请求；closed 表示这是一次成功的 DELETE（客户端主动结束会话），绑定随之删除。
func (b *sessionBinder) leave(sid string, closed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.sessions[sid]
	if !ok {
		return
	}
	if closed {
		delete(b.sessions, sid)
		return
	}
	s.inflight--
	s.lastSeen = b.now()
}

// sweepLocked 回收空闲绑定。阈值与 SDK 的 SessionTimeout 相同：SDK 在会话空闲这么久之后
// 会关掉会话，届时绑定也没有意义了；还有请求在跑（GET SSE）的不回收。一分钟最多扫一次。
func (b *sessionBinder) sweepLocked(now time.Time) {
	if b.idle <= 0 || now.Sub(b.lastSweep) < time.Minute {
		return
	}
	b.lastSweep = now
	for id, s := range b.sessions {
		if s.inflight == 0 && now.Sub(s.lastSeen) > b.idle {
			delete(b.sessions, id)
		}
	}
}

// captureWriter 记下响应状态码与响应头里的会话 ID。
// ⚠️ 必须透传 Flush（SSE 靠它把事件逐条推出去，少了的话流式响应会攒到最后才一次性到达）
// 与 Unwrap（http.ResponseController 靠它找到底层连接）。
type captureWriter struct {
	http.ResponseWriter
	code int
	sid  string
	// onHeader 在成功响应的头发出之前、拿到会话 ID 时调用一次（只有建立会话的请求设置它）。
	onHeader func(sid string)
}

func (c *captureWriter) WriteHeader(code int) {
	if c.code == 0 {
		c.code = code
		c.sid = c.Header().Get(HeaderSessionID)
		if c.onHeader != nil && code < 300 && c.sid != "" {
			c.onHeader(c.sid)
		}
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *captureWriter) Write(p []byte) (int, error) {
	if c.code == 0 {
		c.WriteHeader(http.StatusOK)
	}
	return c.ResponseWriter.Write(p)
}

func (c *captureWriter) Flush() {
	if c.code == 0 {
		c.WriteHeader(http.StatusOK)
	}
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (c *captureWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

func (c *captureWriter) status() int {
	if c.code == 0 {
		return http.StatusOK
	}
	return c.code
}
