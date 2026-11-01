// Package logredact 是请求日志的脱敏与截断。
//
// ⚠️ 起因（老系统的真实经历）：排查一次 502 只能去库里解密 API key、
// 手动 curl 上游复现，才发现问题是"图片以 URL 形态发出去、上游拉不动"。
// 有了请求日志这类问题一眼可见 —— 但**日志本身绝不能成为新的泄漏面**。
//
// ⚠️ 它从 `provider/transport` 下沉到 platform，是因为第二个调用方出现了
// （API 请求日志，P13-04）。让可观测域去依赖 Provider 适配层是反的，
// 而各抄一份的话两份早晚会不一致**且没有编译错误** —— 不一致的具体形态是
// **某一类密钥在一条日志里被打码、在另一条里原样躺着**，而两边看起来都在工作。
// 同 `platform/pgtext` 与 `provider/runcfg` 的理由。
package logredact

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/platform/pgtext"
)

// MaxBodyChars 请求/响应体入库的截断长度。
//
// ⚠️ 不截断的话，一张内联参考图 base64 后就是几 MB，
// 一条日志能把表撑爆（老系统实测单张 4.6MB）。
const MaxBodyChars = 4000

var (
	// base64 内联数据：只留摘要，不留内容。
	dataURLRe = regexp.MustCompile(`data:([a-zA-Z0-9/+.-]+);base64,[A-Za-z0-9+/=]{100,}`)
	// 明文 key 形态（sk-、Bearer 等）。
	bearerRe = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._\-]{8,}`)
	apiKeyRe = regexp.MustCompile(`(?i)\b(sk|pk|api|key)[-_][A-Za-z0-9]{12,}`)
	// MCP 访问密钥（sk_mcp_ + base62，MCPT-06、开发 01 §12 R4）。
	//
	// ⚠️ apiKeyRe 匹配不到它：`sk_` 之后是 `mcp_`，在第二个下划线处断开，`[A-Za-z0-9]{12,}`
	// 只吃得到「mcp」三个字符。字段名命中 token/authorization 的会被按字段打码，但出现在
	// **自由文本**里（用户把密钥贴进提示词、错误信息里回显了请求头）的会原样进日志。
	// 留下 `sk_mcp_` 前缀：看日志的人仍知道「这里曾有一把 MCP 密钥」，便于判断要不要撤销。
	mcpKeyRe = regexp.MustCompile(`(?i)\bsk_mcp_[A-Za-z0-9]{8,}`)
	// MCP OAuth 的访问令牌 / 刷新令牌 / 授权码（mcp_at_ / mcp_rt_ / mcp_ac_，MCPT-12）。
	// ⚠️ 令牌端点的请求体是表单（`code=mcp_ac_…&refresh_token=mcp_rt_…`），不是 JSON，按字段名打码
	// 打不到它们；API 请求日志会原样记下请求体。apiKeyRe 同样匹配不到（`mcp` 后就断开了）。
	mcpOAuthRe = regexp.MustCompile(`(?i)\bmcp_(at|rt|ac)_[A-Za-z0-9]{8,}`)
)

// 敏感字段名：值一律打码，不论嵌套多深。
var sensitiveKeys = map[string]bool{
	"authorization": true, "api_key": true, "apikey": true, "apiKey": true,
	"secret": true, "password": true, "token": true, "access_token": true,
	"refresh_token": true, "x-api-key": true, "credential": true,
}

// Redact 脱敏并截断一段文本。
func Redact(s string) string {
	if s == "" {
		return ""
	}
	// ⚠️ base64 先替换成摘要再截断：反过来的话会把一张图的前 4000 个
	// 字符原样存进去 —— 既没用又占地方。
	s = dataURLRe.ReplaceAllStringFunc(s, func(m string) string {
		mime := "binary"
		if i := strings.Index(m, ":"); i >= 0 {
			if j := strings.Index(m[i+1:], ";"); j >= 0 {
				mime = m[i+1 : i+1+j]
			}
		}
		// 体积按 base64 长度估算（每 4 字符约 3 字节）。
		approx := (len(m) * 3) / 4
		return fmt.Sprintf("[base64 %s %.2fMB]", mime, float64(approx)/(1<<20))
	})
	s = bearerRe.ReplaceAllString(s, "${1}***")
	s = mcpKeyRe.ReplaceAllString(s, "sk_mcp_***")
	s = mcpOAuthRe.ReplaceAllString(s, "mcp_${1}_***")
	s = apiKeyRe.ReplaceAllString(s, "***")
	return Truncate(s)
}

// Truncate 截断并标明原长度 —— 只截不标的话，看日志的人不知道后面还有多少。
func Truncate(s string) string {
	s = pgtext.Safe(s)
	r := []rune(s)
	if len(r) <= MaxBodyChars {
		return s
	}
	return string(r[:MaxBodyChars]) + fmt.Sprintf("…[截断，原长 %d 字符]", len(r))
}

// RedactJSON 对 JSON 做按字段脱敏，再走通用脱敏。
//
// 按字段名打码比正则可靠：正则只能猜"看起来像 key 的字符串"，
// 而字段名是上游协议里写明的。
func RedactJSON(raw []byte) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		// 不是 JSON（multipart、二进制）就走通用脱敏。
		return Redact(string(raw))
	}
	out, err := json.Marshal(redactValue(v))
	if err != nil {
		return Redact(string(raw))
	}
	return Redact(string(out))
}

func redactValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		// Anthropic 的内联图是 {"type":"base64","media_type":"image/png","data":"iVBOR…"}：data 不带 `data:` 前缀，
		// dataURLRe 匹配不到，不在这里摘要的话整段 base64 会占满截断后的 4000 字（MKT-T-19 核实时发现）。
		base64Src := t["type"] == "base64"
		for k, val := range t {
			if base64Src && k == "data" {
				if s, ok := val.(string); ok && len(s) >= 100 {
					mime, _ := t["media_type"].(string)
					if mime == "" {
						mime = "binary"
					}
					out[k] = fmt.Sprintf("[base64 %s %.2fMB]", mime, float64(len(s)*3/4)/(1<<20))
					continue
				}
			}
			if sensitiveKeys[strings.ToLower(k)] {
				out[k] = "***"
				continue
			}
			out[k] = redactValue(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = redactValue(val)
		}
		return out
	}
	return v
}

// RedactHeaders 给请求头脱敏，用于日志。
func RedactHeaders(h map[string]string) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if sensitiveKeys[strings.ToLower(k)] {
			out[k] = "***"
			continue
		}
		out[k] = Redact(v)
	}
	return out
}

// SafeText 是 `pgtext.Safe` 的别名，供日志侧按语义命名调用。
func SafeText(s string) string { return pgtext.Safe(s) }
