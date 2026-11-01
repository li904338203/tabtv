package logredact

import (
	"strings"
	"testing"
)

// 一把形态合法的 MCP 访问密钥（sk_mcp_ + 43 位 base62），只用于测试。
const mcpKey = "sk_mcp_0aB1cD2eF3gH4iJ5kL6mN7oP8qR9sT0uV1wX2yZ3aB4c"

func TestRedact_MCP访问密钥打码(t *testing.T) {
	cases := map[string]string{
		"自由文本":    "我的密钥是 " + mcpKey + "，请帮我配置",
		"JSON 字段": `{"prompt":"用 ` + mcpKey + ` 调一下"}`,
		"行首":      mcpKey,
		"大小写":     strings.ToUpper(mcpKey[:7]) + mcpKey[7:],
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			out := Redact(in)
			if strings.Contains(out, mcpKey[7:]) || strings.Contains(out, mcpKey[7:20]) {
				t.Fatalf("密钥主体未打码：%s", out)
			}
			if !strings.Contains(strings.ToLower(out), "sk_mcp_***") {
				t.Fatalf("应保留 sk_mcp_ 前缀并打码：%s", out)
			}
		})
	}
	// 按字段 + 正则两条路都要成立：RedactJSON 里密钥既可能在敏感字段，也可能在普通字段的文本里。
	out := RedactJSON([]byte(`{"token":"` + mcpKey + `","args":{"note":"` + mcpKey + `"}}`))
	if strings.Contains(out, mcpKey[7:]) {
		t.Fatalf("RedactJSON 未打码：%s", out)
	}
	// 太短的不是密钥（sk_mcp_ 后不足 8 位），不误伤正常文本。
	if got := Redact("sk_mcp_abc"); got != "sk_mcp_abc" {
		t.Fatalf("短串被误打码：%s", got)
	}
}

func TestRedact_原有sk与Bearer规则不变(t *testing.T) {
	cases := []struct{ in, want string }{
		{"key sk-abcdefghijkl1234 end", "key *** end"},
		{"Authorization: Bearer abc.def-ghi_jkl", "Authorization: Bearer ***"},
		{"pk_ABCDEFGHIJKL1", "***"},
		{"普通文本不变", "普通文本不变"},
	}
	for _, c := range cases {
		if got := Redact(c.in); got != c.want {
			t.Errorf("Redact(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
	// Bearer 后跟 MCP 密钥：Bearer 规则先吃掉，结果与原来一致。
	if got := Redact("Bearer " + mcpKey); got != "Bearer ***" {
		t.Errorf("Bearer + MCP 密钥 = %q", got)
	}
}

// MCP OAuth 的令牌与授权码（MCPT-12）：令牌端点的请求体是表单，按字段名打码打不到。
func TestRedact_MCPOAuth令牌打码(t *testing.T) {
	body := "grant_type=authorization_code&code=mcp_ac_0123456789ABCDEFGHIJabcdefghij0123456789ABC" +
		"&refresh_token=mcp_rt_0123456789ABCDEFGHIJabcdefghij0123456789ABC&x=mcp_at_0123456789ABCDEFGHIJ"
	got := Redact(body)
	for _, leak := range []string{"0123456789ABCDEFGHIJ"} {
		if strings.Contains(got, leak) {
			t.Fatalf("令牌明文泄漏：%q", got)
		}
	}
	for _, keep := range []string{"code=mcp_ac_***", "refresh_token=mcp_rt_***", "x=mcp_at_***"} {
		if !strings.Contains(got, keep) {
			t.Fatalf("应保留前缀 %q：%q", keep, got)
		}
	}
}

// 推理模型看图（MKT-T-19）：两家协议的内联图在请求日志里都只留摘要，不留 base64 原文。
func TestRedactJSON_两家协议的内联图都摘要化(t *testing.T) {
	b64 := strings.Repeat("QUJD", 2000) // 8000 字符
	openai := `{"messages":[{"role":"user","content":[{"type":"text","text":"看图"},` +
		`{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,` + b64 + `"}}]}]}`
	anthropic := `{"messages":[{"role":"user","content":[{"type":"image","source":` +
		`{"type":"base64","media_type":"image/png","data":"` + b64 + `"}},{"type":"text","text":"看图"}]}],` +
		`"system":"系统提示词"}`
	for name, raw := range map[string]string{"openai": openai, "anthropic": anthropic} {
		got := RedactJSON([]byte(raw))
		if strings.Contains(got, "QUJDQUJD") {
			t.Errorf("%s：日志里留下了 base64 原文：%.200s", name, got)
		}
		if !strings.Contains(got, "[base64 image/") || !strings.Contains(got, "看图") {
			t.Errorf("%s：应留摘要与文本：%.300s", name, got)
		}
	}
	// 普通字段名叫 data 的不受影响（只认 type=base64 的图片源）。
	if got := RedactJSON([]byte(`{"data":"` + b64[:120] + `"}`)); !strings.Contains(got, b64[:120]) {
		t.Errorf("非图片源的 data 字段不应被改写：%s", got)
	}
}
