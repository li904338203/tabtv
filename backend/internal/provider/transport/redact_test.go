package transport

import (
	"strings"
	"testing"
)

// 一张内联参考图 base64 后几 MB，原样入库一条就能把表撑爆。
func TestRedact_base64只留摘要(t *testing.T) {
	blob := "data:image/png;base64," + strings.Repeat("A", 200000)
	got := Redact(`{"image":"` + blob + `"}`)
	if strings.Contains(got, strings.Repeat("A", 200)) {
		t.Fatal("base64 内容不该进日志")
	}
	if !strings.Contains(got, "[base64 image/png") {
		t.Fatalf("应留下类型与体积摘要，实际 %q", got)
	}
	if !strings.Contains(got, "MB]") {
		t.Errorf("摘要应带体积，实际 %q", got)
	}
	// 摘要本身很短，不该再被截断成一半。
	if strings.Contains(got, "截断") {
		t.Errorf("替换成摘要后不该还需要截断：%q", got)
	}
}

func TestRedact_密钥形态(t *testing.T) {
	cases := map[string]string{
		"Bearer": "Authorization: Bearer sk-abcdefghijklmnop",
		"sk 前缀":  `{"key":"sk-1234567890abcdef"}`,
		"api 前缀": `api_ABCDEFGHIJKLMNOP`,
	}
	for name, in := range cases {
		got := Redact(in)
		if strings.Contains(got, "abcdefghijklmnop") || strings.Contains(got, "1234567890abcdef") ||
			strings.Contains(got, "ABCDEFGHIJKLMNOP") {
			t.Errorf("%s: 密钥没被打码: %q", name, got)
		}
		if !strings.Contains(got, "***") {
			t.Errorf("%s: 应留下打码标记: %q", name, got)
		}
	}
}

// 按字段名打码比正则可靠：字段名是上游协议里写明的。
func TestRedactJSON_按字段名打码(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-4",
		"api_key":"whatever-not-matching-any-pattern",
		"nested":{"authorization":"plain text value","prompt":"一只猫"},
		"list":[{"token":"abc"},{"safe":"ok"}]
	}`)
	got := RedactJSON(raw)
	for _, leak := range []string{"whatever-not-matching", "plain text value", `"abc"`} {
		if strings.Contains(got, leak) {
			t.Errorf("敏感字段未打码: %q 出现在 %q", leak, got)
		}
	}
	// 非敏感内容要保留，否则日志就没有排查价值了。
	for _, keep := range []string{"gpt-4", "一只猫", "ok"} {
		if !strings.Contains(got, keep) {
			t.Errorf("正常内容不该被抹掉: %q 不在 %q", keep, got)
		}
	}
}

func TestRedactJSON_非JSON也能处理(t *testing.T) {
	got := RedactJSON([]byte("--boundary\r\nContent-Disposition: form-data\r\nsk-abcdefghijklmnop"))
	if strings.Contains(got, "abcdefghijklmnop") {
		t.Errorf("非 JSON 也要走通用脱敏，实际 %q", got)
	}
}

// 只截不标的话，看日志的人不知道后面还有多少。
func TestTruncate(t *testing.T) {
	short := strings.Repeat("字", 10)
	if Truncate(short) != short {
		t.Error("短文本不该被改动")
	}
	long := strings.Repeat("字", MaxBodyChars+500)
	got := Truncate(long)
	if !strings.Contains(got, "截断") || !strings.Contains(got, "4500") {
		t.Errorf("应标明截断与原长，实际尾部 %q", got[len(got)-40:])
	}
	// 按字符截断不按字节：中文一字 3 字节，按字节截会切出半个字。
	if strings.Contains(got, "�") {
		t.Error("不该出现半个字符")
	}
}

func TestRedactHeaders(t *testing.T) {
	got := RedactHeaders(map[string]string{
		"Authorization": "Bearer sk-abcdefghijklmnop",
		"X-API-Key":     "secret-value-here",
		"Content-Type":  "application/json",
	})
	if got["Authorization"] != "***" || got["X-API-Key"] != "***" {
		t.Errorf("认证头应整体打码，实际 %v", got)
	}
	if got["Content-Type"] != "application/json" {
		t.Errorf("普通头应保留，实际 %q", got["Content-Type"])
	}
}

// ⚠️ 这条钉住的是一次会**卡死任务**的故障：上游回二进制（TTS 的音频、
// 被网关拦截的错误页），响应体里的 0 字节让 PG 的 text 列拒收、
// jsonb 拒绝 \u0000 转义 —— 产物落库整条回滚，执行器判可重试，
// 一直重试到撞死线，最后报 deadline_exceeded。而上游早就成功了。
func TestSafeText_能存进PG的文本(t *testing.T) {
	cases := []struct{ name, in string }{
		{"NUL", "ID3\x03\x00\x00\x00音频"},
		{"C0 控制字符", "a\x01b\x1fc"},
		{"DEL", "a\x7fb"},
	}
	for _, c := range cases {
		got := SafeText(c.in)
		if strings.ContainsRune(got, 0) {
			t.Errorf("%s: 清理后仍含 NUL: %q", c.name, got)
		}
		for _, r := range got {
			if r < 0x20 && r != '\t' && r != '\n' && r != '\r' {
				t.Errorf("%s: 清理后仍含控制字符 %q", c.name, r)
			}
		}
		if !strings.Contains(got, "已丢弃") {
			t.Errorf("%s: 丢了字符要说出来，否则日志里看不出为什么变短: %q", c.name, got)
		}
	}
}

func TestSafeText_正常文本原样返回(t *testing.T) {
	// 绝大多数响应是正常 JSON，不该为它们多做一次拷贝。
	for _, s := range []string{`{"a":1}`, "带换行\n和制表\t符", "中文与 emoji 🎬"} {
		if got := SafeText(s); got != s {
			t.Errorf("正常文本不该被改动: %q → %q", s, got)
		}
	}
}

func TestTruncate_先清理再截断(t *testing.T) {
	// 顺序要紧：先截断再清理的话，截断计数会把控制字符算进去。
	got := Truncate("a\x00b")
	if strings.ContainsRune(got, 0) {
		t.Fatalf("截断后仍含 NUL: %q", got)
	}
}
