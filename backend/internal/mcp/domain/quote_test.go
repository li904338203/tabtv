package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// 指纹的完整表驱动在 fingerprint_test.go（MCPT-06 已落）；这里按 MCPT-05 卡片的三条用例名再钉一次
// quote 实际会喂进去的形态（单项 item 对象）。

func TestFingerprint_键序不同指纹相同(t *testing.T) {
	a := Fingerprint(QuoteToolGenerate, json.RawMessage(`{"modelCode":"m","operation":"text2img","count":2,"inputs":{"image":1,"text":1}}`))
	b := Fingerprint(QuoteToolGenerate, json.RawMessage(`{"inputs":{"text":1,"image":1},"count":2.0,"operation":"text2img","modelCode":"m"}`))
	if !bytes.Equal(a, b) {
		t.Fatal("键序 / 数字写法不同应同指纹")
	}
}

func TestFingerprint_改一个字指纹不同(t *testing.T) {
	a := Fingerprint(QuoteToolGenerate, json.RawMessage(`{"modelCode":"m","text":"一只猫"}`))
	b := Fingerprint(QuoteToolGenerate, json.RawMessage(`{"modelCode":"m","text":"一只狗"}`))
	if bytes.Equal(a, b) {
		t.Fatal("改一个字应不同指纹")
	}
	if bytes.Equal(Fingerprint(QuoteToolGenerate, json.RawMessage(`{}`)), Fingerprint(QuoteToolPlan, json.RawMessage(`{}`))) {
		t.Fatal("工具不同应不同指纹")
	}
}

func TestFingerprint_忽略confirm字段(t *testing.T) {
	a := Fingerprint(QuoteToolGenerate, json.RawMessage(`{"modelCode":"m","count":1}`))
	b := Fingerprint(QuoteToolGenerate, json.RawMessage(`{"modelCode":"m","count":1,"confirmQuoteId":"9","confirmCredits":30}`))
	if !bytes.Equal(a, b) {
		t.Fatal("顶层 confirm* 字段不应影响指纹")
	}
}

func TestSumCredits_溢出报错(t *testing.T) {
	if n, err := SumCredits([]int{3, 0, 40}); err != nil || n != 43 {
		t.Fatalf("合计：%d %v", n, err)
	}
	if n, err := SumCredits(nil); err != nil || n != 0 {
		t.Fatalf("空合计：%d %v", n, err)
	}
	if n, err := SumCredits([]int{MaxQuoteCredits}); err != nil || n != MaxQuoteCredits {
		t.Fatalf("恰好上限：%d %v", n, err)
	}
	if _, err := SumCredits([]int{MaxQuoteCredits, 1}); !errors.Is(err, ErrCreditsOverflow) {
		t.Fatalf("超上限应报溢出：%v", err)
	}
	if _, err := SumCredits([]int{MaxQuoteCredits - 5, 3, 3}); !errors.Is(err, ErrCreditsOverflow) {
		t.Fatalf("累加越界应报溢出（不能回绕）：%v", err)
	}
	if _, err := SumCredits([]int{1, -1}); err == nil {
		t.Fatal("负数应报错")
	}
}

func TestShortfall(t *testing.T) {
	cases := []struct{ total, balance, want int }{{10, 30, 0}, {30, 30, 0}, {31, 30, 1}, {5, 0, 5}, {5, -2, 7}}
	for _, c := range cases {
		if got := Shortfall(c.total, c.balance); got != c.want {
			t.Errorf("Shortfall(%d,%d)=%d，期望 %d", c.total, c.balance, got, c.want)
		}
	}
}

func TestQuoteDecide_有不可用项不签发quoteId(t *testing.T) {
	ok := QuoteLine{Credits: 10, Unit: "per_image", Available: true}
	bad := QuoteLine{BlockedCode: "MEMBER_LEVEL_REQUIRED", BlockedReason: "需要更高会员等级", UserFixable: true}
	if d := DecideQuote([]QuoteLine{ok, ok}); !d.Issue || d.Reason != "" || len(d.Blocked) != 0 {
		t.Fatalf("全部可用应签发：%+v", d)
	}
	d := DecideQuote([]QuoteLine{ok, bad, ok, {BlockedCode: "PRICING_MISSING"}})
	if d.Issue {
		t.Fatal("有不可用项不应签发")
	}
	if len(d.Blocked) != 2 || d.Blocked[0] != 1 || d.Blocked[1] != 3 {
		t.Fatalf("不可用项下标：%v", d.Blocked)
	}
	for _, want := range []string{"第 2 项：需要更高会员等级", "第 4 项：PRICING_MISSING", "未签发 quoteId"} {
		if !strings.Contains(d.Reason, want) {
			t.Errorf("原因应包含 %q：%s", want, d.Reason)
		}
	}
	if d := DecideQuote(nil); d.Issue || d.Reason == "" {
		t.Fatalf("没有项不应签发：%+v", d)
	}
}
