package pgtext

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// CD-82：存不下的说明文字要截到列的上限，不能让它把整笔退款拖垮。
func TestFit_按字符截到上限(t *testing.T) {
	long := "上游返回 HTTP 400: " + strings.Repeat("错", 400)
	got := Fit(long, 255)
	if n := utf8.RuneCountInString(got); n != 255 {
		t.Fatalf("应截到 255 个字符（varchar 按字符计），得到 %d", n)
	}
	if !strings.HasSuffix(got, "…") || !strings.HasPrefix(got, "上游返回 HTTP 400: ") {
		t.Fatalf("截断后应保留开头并以省略号结尾：%q", got[:40])
	}
	if !utf8.ValidString(got) {
		t.Fatal("不能把多字节字符截成半个")
	}
	if got := Fit("短的原因", 255); got != "短的原因" {
		t.Fatalf("没超的不动，得到 %q", got)
	}
	if got := Fit("一二三", 3); got != "一二三" {
		t.Fatalf("恰好到上限不截，得到 %q", got)
	}
	if got := Fit("一二三四", 3); got != "一二…" {
		t.Fatalf("超一个就截并带省略号，得到 %q", got)
	}
	if got := Fit("a\x00b", 10); strings.ContainsRune(got, 0) {
		t.Fatalf("先清洗再截：%q", got)
	}
}
