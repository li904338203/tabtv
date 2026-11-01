// Package log 基于标准库 log/slog 构建结构化日志（ADR-0001 TECH-11）。
//
// 关键日志统一携带 traceId / userId / adminId / projectId / generationId /
// provider / model / operation / attempt / durationMs / errorCategory
// （08 §9.1）。字段级脱敏与截断在本包统一实现，不让各领域自己写。
package log

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

// 统一字段名。各领域一律用这些常量，避免同一含义出现 user_id / userId / uid 三种写法。
const (
	KeyTraceID       = "traceId"
	KeyUserID        = "userId"
	KeyAdminID       = "adminId"
	KeyWorkspaceID   = "workspaceId"
	KeyProjectID     = "projectId"
	KeyGenerationID  = "generationId"
	KeyProvider      = "provider"
	KeyModel         = "model"
	KeyOperation     = "operation"
	KeyAttempt       = "attempt"
	KeyDurationMS    = "durationMs"
	KeyErrorCategory = "errorCategory"
)

// sensitiveKeys 命中即打码。匹配为小写包含，覆盖 apiKey / api_key / X-Api-Key 等写法。
var sensitiveKeys = []string{
	"password", "passwd", "secret", "token", "apikey", "api_key",
	"authorization", "cookie", "credential", "privatekey", "private_key",
	"accesskey", "access_key", "secretkey", "secret_key", "signature",
}

// New 按配置建一个 slog.Logger。format 为 json 或 text。
func New(level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{
		Level:       parseLevel(level),
		ReplaceAttr: redact,
	}
	var h slog.Handler
	if format == "json" {
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(h)
}

// redact 对敏感字段打码。这是最后一道防线：
// Provider api key、JWT secret、数据库密码不得出现在任何日志里（ADR-0001 §8.1）。
func redact(_ []string, a slog.Attr) slog.Attr {
	lower := strings.ToLower(a.Key)
	for _, s := range sensitiveKeys {
		if strings.Contains(lower, s) {
			return slog.String(a.Key, "[REDACTED]")
		}
	}
	return a
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

type ctxKey struct{}

// Into 把 logger 放进 context，便于跨层携带 traceId 等已绑定字段。
func Into(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// From 从 context 取 logger；没有则回落到默认 logger，不返回 nil。
func From(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}
