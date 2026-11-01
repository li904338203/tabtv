package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// RateLimiter 是两级限流（app.Limiter 满足）。
type RateLimiter interface {
	Allow(ctx context.Context, p app.Principal) app.RateDecision
}

// rateLimit 是限流中间件：凭据之后、go-sdk 之前（开发 01 §7.3）。
//
// **只对 tools/call 计数**：initialize、tools/list、notifications、GET SSE 都是协议开销，
// 一个 2025 客户端每建一次会话就要发三四条 —— 计进去的话额度被握手吃掉，而用户的感知是
// 「我只调了十几次就被限流了」。阈值语义是「每分钟工具调用次数」（MCP-PARAM-01）。
//
// 超限回 **HTTP 429 + Retry-After 头 + RATE_LIMITED**，details 带 retry_after_seconds（MCP-I-06）。
// 被限流的调用**没有进 SDK**，审计在这里记（result=rate_limited）——
// 滥用恰恰是最需要留痕的那一类。
func (h *Handler) rateLimit(next http.Handler) http.Handler {
	if h.limiter == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		calls := peekToolCalls(r)
		if len(calls) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		ctx := r.Context()
		p, _ := app.PrincipalFrom(ctx)
		for range calls {
			d := h.limiter.Allow(ctx, p)
			if d.Allowed {
				continue
			}
			h.recordLimited(r, p, calls)
			w.Header().Set("Retry-After", strconv.Itoa(d.RetryAfter))
			httpx.Fail(w, r, httpx.Errf(errcode.RateLimited, limitMessage(d)).WithDetails(map[string]any{
				"retry_after_seconds": d.RetryAfter,
				"scope":               d.Scope,
				"limit":               d.Limit,
			}))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// limitMessage 说清是哪一级、上限多少、多久之后再试（模型可以原样转述给用户）。
func limitMessage(d app.RateDecision) string {
	who := "该客户端"
	if d.Scope == app.RateScopeWorkspace {
		who = "该工作区（所有成员与客户端合计）"
	}
	return fmt.Sprintf("MCP 调用过于频繁：%s每分钟最多 %d 次工具调用，请 %d 秒后重试", who, d.Limit, d.RetryAfter)
}

func (h *Handler) recordLimited(r *http.Request, p app.Principal, calls []toolCall) {
	if h.calls == nil {
		return
	}
	now := time.Now()
	for _, c := range calls {
		e := app.EntryFor(p)
		e.Tool, e.Args = c.Params.Name, c.Params.Arguments
		e.Result, e.ErrorCode = domain.CallRateLimited, int(errcode.RateLimited)
		e.TraceID, e.At = httpx.TraceIDFrom(r.Context()), now
		h.calls.Record(e)
	}
}

// toolCall 是 JSON-RPC 消息里限流与审计要看的那几个字段。
type toolCall struct {
	Method string `json:"method"`
	Params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"params"`
}

// peekToolCalls 读出请求体里的 tools/call 消息，并把请求体**原样放回**给 SDK。
//
// 超过 MaxRequestBodyBytes 的不解析、原样放行（SDK 会回 413）；读失败、不是 JSON 的也放行 ——
// 限流是保护不是校验，解析不了的请求由 SDK 去拒，这里不替它判。
// 兼容 JSON-RPC 批量（2025-03-26 允许，之后的版本已移除）：数组里每条 tools/call 各计一次。
func peekToolCalls(r *http.Request) []toolCall {
	if r.Body == nil {
		return nil
	}
	buf, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBodyBytes+1))
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(buf), r.Body), r.Body}
	if err != nil || len(buf) > MaxRequestBodyBytes {
		return nil
	}
	trimmed := bytes.TrimLeft(buf, " \t\r\n")
	var msgs []toolCall
	if len(trimmed) > 0 && trimmed[0] == '[' {
		if json.Unmarshal(trimmed, &msgs) != nil {
			return nil
		}
	} else {
		var m toolCall
		if json.Unmarshal(trimmed, &m) != nil {
			return nil
		}
		msgs = []toolCall{m}
	}
	out := msgs[:0]
	for _, m := range msgs {
		if m.Method == "tools/call" {
			out = append(out, m)
		}
	}
	return out
}
