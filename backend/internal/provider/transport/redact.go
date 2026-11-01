package transport

import (
	"github.com/li904338203/tabtv/backend/internal/platform/logredact"
)

// 请求日志的脱敏与截断 —— **实现下沉到了 `platform/logredact`**
// （第二个调用方是 API 请求日志，P13-04）。
//
// 这里保留一组同名薄封装：Provider 侧的调用点很多，而它们要表达的是
// "发给/收自上游的文本"，读起来仍然属于这一层。改脱敏规则改那个包，
// **不要在这里加第二份逻辑** —— 两份规则不一致的形态是某一类密钥在一条
// 日志里被打码、在另一条里原样躺着，而两边看起来都在工作。

// MaxBodyChars 请求/响应体入库的截断长度。
const MaxBodyChars = logredact.MaxBodyChars

// Redact 脱敏并截断一段文本。
func Redact(s string) string { return logredact.Redact(s) }

// Truncate 截断并标明原长度。
func Truncate(s string) string { return logredact.Truncate(s) }

// SafeText 把任意字节变成能存进 PostgreSQL 文本/jsonb 列的文本。
func SafeText(s string) string { return logredact.SafeText(s) }

// RedactJSON 对 JSON 做按字段脱敏，再走通用脱敏。
func RedactJSON(raw []byte) string { return logredact.RedactJSON(raw) }

// RedactHeaders 给请求头脱敏，用于日志。
func RedactHeaders(h map[string]string) map[string]string { return logredact.RedactHeaders(h) }
