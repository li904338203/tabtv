// Package httpx 提供统一的错误响应与 traceId 处理。
//
// 两条契约决定落地在这里：
//   - CD-01：**HTTP 状态码必须与业务码前 3 位一致**。旧系统一律返回
//     HTTP 200 + 非零 code，导致只看 http_status 判不出成败。
//   - CD-02：**成功响应不套信封**，直接返回数据；traceId 走 X-Trace-Id
//     响应头，这样媒体下载这类非 JSON 响应也能拿到。
package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
)

const HeaderTraceID = "X-Trace-Id"

type ctxKey struct{}

// Error 是错误响应体，与 api/openapi.yaml 的 Error schema 一致。
type Error struct {
	Code    int            `json:"code"`
	Name    string         `json:"name"`
	Message string         `json:"message"`
	TraceID string         `json:"traceId"`
	Details map[string]any `json:"details,omitempty"`
}

// BizError 是领域层抛出的业务错误。
type BizError struct {
	Code    errcode.Code
	Message string
	Details map[string]any
	cause   error
}

func (e *BizError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Code.Message()
}
func (e *BizError) Unwrap() error { return e.cause }

// Err 构造业务错误，使用错误码的默认文案。
func Err(code errcode.Code) *BizError { return &BizError{Code: code} }

// Errf 构造业务错误并覆盖文案。
func Errf(code errcode.Code, msg string) *BizError { return &BizError{Code: code, Message: msg} }

// Wrap 保留底层错误供日志记录，但**不会**下发给客户端 ——
// 底层错误常含连接串、SQL 片段等不该外泄的信息。
func Wrap(code errcode.Code, cause error) *BizError {
	return &BizError{Code: code, cause: cause}
}

func (e *BizError) WithDetails(d map[string]any) *BizError {
	e.Details = d
	return e
}

// TraceID 中间件：复用上游传入的 traceId，没有则生成。
// 复用是必要的——AC-26 要求同一 traceId 能追受理、任务、Provider 调用与结果，
// 网关已经生成过就不该再换一个。
func TraceID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid := r.Header.Get(HeaderTraceID)
		if tid == "" {
			tid = newTraceID()
		}
		w.Header().Set(HeaderTraceID, tid)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, tid)))
	})
}

// WithTraceID 把 traceId 放进 ctx。只给「处理消息的 ctx 不是本次 HTTP 请求的 ctx」的场景用：
// MCP 的 2025 会话里 SDK 用的是建立会话那个请求的 ctx，不覆盖的话会话里每次调用的
// 日志与审计都挂在 initialize 那一次的 traceId 上（mcp/transport 的 requestScope）。
func WithTraceID(ctx context.Context, tid string) context.Context {
	return context.WithValue(ctx, ctxKey{}, tid)
}

func TraceIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKey{}).(string); ok {
		return v
	}
	return ""
}

// JSON 写成功响应。**不套信封**：直接返回数据本身。
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// Fail 写错误响应。HTTP 状态码由错误码推导，二者永远一致。
func Fail(w http.ResponseWriter, r *http.Request, err error) {
	var be *BizError
	if !errors.As(err, &be) {
		// 非业务错误一律按内部错误处理，**不回显原始错误信息**。
		be = Wrap(errcode.Internal, err)
	}

	tid := TraceIDFrom(r.Context())
	msg := be.Message
	if msg == "" {
		msg = be.Code.Message()
	}

	// 5xx 必须记日志：客户端只拿到 traceId，排障全靠服务端这条记录。
	//
	// ⚠️ **必须带上被包裹的原因**。只记 `be.Error()` 的话，日志里是一句
	// 「服务内部错误」—— 那正是我们为了不泄漏内部细节才给客户端看的文案，
	// 对排查毫无价值。实测踩到过：一个 500 在日志里只有这五个字，
	// 而真正的原因（一条 SQL 约束冲突）一个字都没留下。
	if be.Code.HTTPStatus() >= 500 {
		attrs := []any{"traceId", tid, "code", int(be.Code), "path", r.URL.Path,
			"error", be.Error()}
		if cause := errors.Unwrap(be); cause != nil {
			attrs = append(attrs, "cause", cause.Error())
		}
		slog.ErrorContext(r.Context(), "请求处理失败", attrs...)
	}

	// 把业务码留在上下文里，供请求日志分类（见 WithBizCodeSlot）。
	recordBizCode(r.Context(), int(be.Code))

	JSON(w, be.Code.HTTPStatus(), Error{
		Code:    int(be.Code),
		Name:    be.Code.Name(),
		Message: msg,
		TraceID: tid,
		Details: be.Details,
	})
}

// Recover 把 panic 转成 500，并保证响应体仍是标准错误结构。
// 没有它，panic 会让连接直接断开，客户端拿不到 traceId 也就无从上报。
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.ErrorContext(r.Context(), "handler panic",
					"traceId", TraceIDFrom(r.Context()), "path", r.URL.Path, "panic", rec)
				Fail(w, r, Err(errcode.Internal))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func newTraceID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "trace-unavailable"
	}
	return hex.EncodeToString(b)
}

// ---------------------------------------------------------------- 业务码留痕

// bizCodeKey 在请求上下文里挂一个"本次响应用了哪个业务码"的槽。
type bizCodeKey struct{}

// WithBizCodeSlot 在上下文里开一个槽，供 `Fail` 把业务码写进去。
//
// ⚠️ 这不是为了判断"成不成功"—— 本项目里 HTTP 状态码与业务码始终一致
// （CD-01），看 status 就知道成没成。它是为了**分类**：40001「参数非法」、
// 40002「缺幂等键」、40903「余额不足」在 HTTP 上都只是一个 400/409，
// 而请求日志要回答的恰恰是"今天这批 400 里哪一类最多"。
//
// ⚠️ 走上下文而不是**缓存响应体再解析**：产出图与音视频直接吐字节，
// 缓存响应等于在内存里再复制一份 —— 一次几百 MB 的成片下载就能把 API 打爆。
func WithBizCodeSlot(r *http.Request) (*http.Request, *int) {
	slot := new(int)
	return r.WithContext(context.WithValue(r.Context(), bizCodeKey{}, slot)), slot
}

// recordBizCode 由 `Fail` 调用。没有槽（没挂日志中间件）时什么都不做。
func recordBizCode(ctx context.Context, code int) {
	if slot, ok := ctx.Value(bizCodeKey{}).(*int); ok && slot != nil {
		*slot = code
	}
}
