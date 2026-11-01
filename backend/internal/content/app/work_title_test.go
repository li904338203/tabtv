package app

import (
	"strings"
	"testing"
)

func TestNormalizeWorkTitle(t *testing.T) {
	if got, err := normalizeWorkTitle("  夏日汽水  "); err != nil || got != "夏日汽水" {
		t.Fatalf("应去首尾空白：%q %v", got, err)
	}
	if got, err := normalizeWorkTitle("   "); err != nil || got != "未命名作品" {
		t.Fatalf("空标题应给默认名：%q %v", got, err)
	}
	if _, err := normalizeWorkTitle(strings.Repeat("字", 100)); err != nil {
		t.Fatalf("100 个字（按字符不按字节）应放行：%v", err)
	}
	if _, err := normalizeWorkTitle(strings.Repeat("字", 101)); err == nil {
		t.Fatal("超过 100 个字应报参数错误，而不是落库失败成 500")
	}
}
