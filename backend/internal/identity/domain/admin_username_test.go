package domain

import (
	"strings"
	"testing"
)

func TestValidateAdminUsername(t *testing.T) {
	for _, ok := range []string{"admin", "ro-1790146008", "ops.lead", "a.b@corp", strings.Repeat("a", 64)} {
		if err := ValidateAdminUsername(ok); err != nil {
			t.Errorf("%q 应通过：%v", ok, err)
		}
	}
	// 带空格的用户名：首尾空格被输入法吞掉就再也登不进去
	for _, bad := range []string{"", "ab", " admin", "admin ", "张三", "-admin", strings.Repeat("a", 65)} {
		if err := ValidateAdminUsername(bad); err == nil {
			t.Errorf("%q 应被拒", bad)
		}
	}
}
