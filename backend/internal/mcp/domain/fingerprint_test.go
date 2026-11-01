package domain

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestFingerprint_规范化(t *testing.T) {
	fp := func(tool, s string) []byte { return Fingerprint(tool, json.RawMessage(s)) }
	same := []struct{ name, a, b string }{
		{"键序不同", `{"a":1,"b":{"y":2,"x":3}}`, `{"b":{"x":3,"y":2},"a":1}`},
		{"数字写法不同", `{"n":1,"f":2.50}`, `{"n":1.0,"f":2.5}`},
		{"忽略顶层 confirm 字段", `{"a":1}`, `{"a":1,"confirmQuoteId":"q1"}`},
		{"空与 null 与空对象", ``, `null`},
		{"空白不同", `{"a": [1, 2]}`, `{"a":[1,2]}`},
	}
	for _, c := range same {
		if !bytes.Equal(fp("t", c.a), fp("t", c.b)) {
			t.Errorf("%s：期望同指纹", c.name)
		}
	}
	if !bytes.Equal(fp("t", ``), fp("t", `{}`)) {
		t.Error("空参数应视同 {}")
	}
	diff := []struct{ name, ta, a, tb, b string }{
		{"改一个字", "t", `{"p":"猫"}`, "t", `{"p":"狗"}`},
		{"工具不同", "a", `{}`, "b", `{}`},
		{"嵌套里的 confirm 不忽略", "t", `{"x":{"confirm":1}}`, "t", `{"x":{}}`},
		{"两份不同的坏参数", "t", `{bad`, "t", `{worse`},
	}
	for _, c := range diff {
		if bytes.Equal(fp(c.ta, c.a), fp(c.tb, c.b)) {
			t.Errorf("%s：期望不同指纹", c.name)
		}
	}
	if n := len(fp("t", `{}`)); n != 32 {
		t.Errorf("指纹长度 %d，期望 32", n)
	}
}
