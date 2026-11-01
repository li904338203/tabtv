package lenientjson

import (
	"os/exec"
	"strings"
	"testing"
)

// 这一组用例随代码从 script/domain 搬来（DRT-18），测试名不变。

func TestLenientJSON_模型的脏输出(t *testing.T) {
	type payload struct {
		A string `json:"a"`
		B int    `json:"b"`
	}
	cases := []struct{ name, raw string }{
		{"干净 JSON", `{"a":"x","b":1}`},
		{"代码块围栏", "```json\n{\"a\":\"x\",\"b\":1}\n```"},
		{"前后有说明文字", "好的，这是结果：\n{\"a\":\"x\",\"b\":1}\n以上。"},
		{"尾随逗号", `{"a":"x","b":1,}`},
		{"围栏没标语言", "```\n{\"a\":\"x\",\"b\":1}\n```"},
	}
	for _, c := range cases {
		var got payload
		if err := Decode(c.raw, &got); err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got.A != "x" || got.B != 1 {
			t.Errorf("%s: 解出来不对 %+v", c.name, got)
		}
	}
}

func TestLenientJSON_字符串里的裸引号(t *testing.T) {
	// ⚠️ 这是老系统真实踩到的：输出过长时模型在字符串里写出裸引号，
	// 16228 字符的整批响应作废、用户白扣了 60 积分。
	var got struct {
		Desc string `json:"desc"`
	}
	raw := `{"desc":"他说"你好"然后走了"}`
	if err := Decode(raw, &got); err != nil {
		t.Fatalf("裸引号应当能救回来: %v", err)
	}
	if !strings.Contains(got.Desc, "你好") {
		t.Fatalf("内容丢了: %q", got.Desc)
	}
}

func TestLenientJSON_后面跟着别的大括号(t *testing.T) {
	// 按括号配对取，不是"第一个 { 到最后一个 }"。
	var got struct {
		A int `json:"a"`
	}
	if err := Decode(`{"a":1}

说明：这里的 {x} 只是举例。`, &got); err != nil {
		t.Fatal(err)
	}
	if got.A != 1 {
		t.Fatalf("取错了范围: %+v", got)
	}
}

func TestLenientJSON_救不回来时要带上原文(t *testing.T) {
	// ⚠️ 救不回来就**按失败处理**，绝不吞掉：吞掉的结果是落 0 个镜头
	// 却照样扣了钱。错误里要带原文片段，否则运营无从判断是模型跑题了
	// 还是我们的提示词写坏了。
	var got map[string]any
	err := Decode("模型今天不想干活。", &got)
	if err == nil {
		t.Fatal("这不该被解析成功")
	}
	if !strings.Contains(err.Error(), "模型今天不想干活") {
		t.Fatalf("错误里要带原文片段: %v", err)
	}
}

func TestLenientJSON_通篇是文字时说清楚而不是报乱码(t *testing.T) {
	// 2026-10-14 实测：模型只拿到「三打白骨精」四个字，回了一段反问。
	// encoding/json 的原始错误是 `invalid character 'æ' ...`，用户看到的像乱码。
	var got map[string]any
	err := Decode(`我需要原文内容才能生成分镜。请提供"三打白骨精"的具体剧情原文。`, &got)
	if err == nil {
		t.Fatal("这不该被解析成功")
	}
	msg := err.Error()
	if strings.Contains(msg, "invalid character") || !strings.Contains(msg, "没有按要求输出 JSON") {
		t.Fatalf("应当说清「模型没写 JSON」，得到: %v", msg)
	}
	if !strings.Contains(msg, "我需要原文内容") {
		t.Fatalf("错误里仍要带原文片段: %v", msg)
	}
	// 有括号但写坏了的，保留解析器的原始错误（那时它指向的位置是有用的）。
	if err := Decode(`{"shots": [}`, &got); err == nil || !strings.Contains(err.Error(), "无法解析为 JSON") {
		t.Fatalf("写坏的 JSON 应保留原始解析错误: %v", err)
	}
}

// TestPackage_只依赖标准库：叶子包只依赖标准库（CLAUDE.md §3 platform 叶子包的判据）。
// 依赖了别的内部包的话，导演台与脚本域就会经由它间接依赖对方。
func TestPackage_只依赖标准库(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", ".").Output()
	if err != nil {
		t.Skipf("go list 不可用：%v", err)
	}
	for _, dep := range strings.Fields(string(out)) {
		if dep != "github.com/li904338203/tabtv/backend/internal/platform/lenientjson" {
			t.Errorf("lenientjson 依赖了非标准库的 %s", dep)
		}
	}
}
