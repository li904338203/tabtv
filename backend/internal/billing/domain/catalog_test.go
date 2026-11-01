package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateCatalogCode(t *testing.T) {
	for _, ok := range []string{"basic", "pack_1000", "pro2", strings.Repeat("a", 32)} {
		if err := ValidateCatalogCode(ok); err != nil {
			t.Errorf("%q 应通过：%v", ok, err)
		}
	}
	// 编码是业务键：「basic」与「Basic 」不能是两个等级
	for _, bad := range []string{"", "Basic", "basic ", " basic", "1pack", "基础", "pro-plus", strings.Repeat("a", 33)} {
		if err := ValidateCatalogCode(bad); !errors.Is(err, ErrCatalogCode) {
			t.Errorf("%q 应被拒，得到 %v", bad, err)
		}
	}
}

func TestValidateCatalogName_按字符数(t *testing.T) {
	// 64 个汉字是 192 字节：按字节算会把合法名称挡掉
	if err := ValidateCatalogName(strings.Repeat("字", 64)); err != nil {
		t.Fatalf("64 个字应通过：%v", err)
	}
	if err := ValidateCatalogName(strings.Repeat("字", 65)); !errors.Is(err, ErrCatalogName) {
		t.Fatalf("65 个字应被拒，得到 %v", err)
	}
	if err := ValidateCatalogName("   "); !errors.Is(err, ErrCatalogName) {
		t.Fatalf("空白名称应被拒，得到 %v", err)
	}
}

func TestValidateCatalogCredits_上限(t *testing.T) {
	if err := ValidateCatalogCredits(MaxCatalogCredits); err != nil {
		t.Fatalf("上限本身应通过：%v", err)
	}
	if err := ValidateCatalogCredits(MaxCatalogCredits + 1); !errors.Is(err, ErrCatalogCredits) {
		t.Fatalf("超过上限应被拒，得到 %v", err)
	}
}
