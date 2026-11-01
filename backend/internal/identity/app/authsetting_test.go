package app

import (
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/platform/secret"
)

func cipher(t *testing.T) *secret.Cipher {
	t.Helper()
	c, err := secret.New(map[int]string{1: "test-key-material"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSecretField_保留(t *testing.T) {
	c := cipher(t)
	existing, _ := c.Encrypt("original-secret")
	for _, action := range []SecretAction{SecretKeep, ""} {
		got, err := SecretField{Action: action}.Resolve(c, existing)
		if err != nil {
			t.Fatalf("action=%q 不应报错: %v", action, err)
		}
		if got != existing {
			t.Errorf("action=%q 应原样保留密文", action)
		}
	}
}

// clear 必须能表达"清空"。
// 若只有"传了就改、没传就不改"两态，运营根本没法删掉一个已配置的密钥。
func TestSecretField_清除(t *testing.T) {
	c := cipher(t)
	existing, _ := c.Encrypt("original-secret")
	got, err := SecretField{Action: SecretClear}.Resolve(c, existing)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("clear 后应为空，实际 %q", got)
	}
}

func TestSecretField_替换(t *testing.T) {
	c := cipher(t)
	existing, _ := c.Encrypt("old")
	got, err := SecretField{Action: SecretReplace, Value: "brand-new-secret"}.Resolve(c, existing)
	if err != nil {
		t.Fatal(err)
	}
	if got == existing {
		t.Fatal("replace 应产生新密文")
	}
	plain, err := c.Decrypt(got)
	if err != nil {
		t.Fatal(err)
	}
	if plain != "brand-new-secret" {
		t.Errorf("解密得到 %q", plain)
	}
}

// ⚠️ 最关键的一条：管理端显示的是掩码，用户未改动直接提交会把真实密钥
// 覆盖成 "sk-a****ijkl" 这样的字符串，而且**不会报错**——
// 直到某次调用上游失败才发现凭据坏了。
func TestSecretField_拒绝掩码回传(t *testing.T) {
	c := cipher(t)
	existing, _ := c.Encrypt("real-secret")
	masked := secret.Mask("sk-abcdefghijkl") // sk-a*******ijkl
	_, err := SecretField{Action: SecretReplace, Value: masked}.Resolve(c, existing)
	if err == nil {
		t.Fatal("掩码形态的值必须被拒绝")
	}
	if !strings.Contains(err.Error(), "掩码") {
		t.Errorf("错误信息应说明原因，实际 %v", err)
	}
}

func TestSecretField_替换需要值(t *testing.T) {
	c := cipher(t)
	if _, err := (SecretField{Action: SecretReplace}).Resolve(c, ""); err == nil {
		t.Fatal("replace 但不给 value 应报错")
	}
}

func TestSecretField_非法action(t *testing.T) {
	c := cipher(t)
	if _, err := (SecretField{Action: "delete"}).Resolve(c, ""); err == nil {
		t.Fatal("非法 action 应被拒绝")
	}
}

func TestLooksMasked(t *testing.T) {
	cases := map[string]bool{
		"sk-a*******ijkl": true,
		"****":            true,
		"ab**cd":          false, // 只有 2 个星号，可能是真实密钥的一部分
		"sk-real-secret":  false,
		"":                false,
	}
	for in, want := range cases {
		if got := looksMasked(in); got != want {
			t.Errorf("looksMasked(%q) = %v，期望 %v", in, got, want)
		}
	}
}
