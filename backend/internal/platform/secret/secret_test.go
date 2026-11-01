package secret

import (
	"strings"
	"testing"
)

func mustNew(t *testing.T, keys map[int]string, cur int) *Cipher {
	t.Helper()
	c, err := New(keys, cur)
	if err != nil {
		t.Fatalf("建立 Cipher 失败: %v", err)
	}
	return c
}

func TestEncryptDecrypt_往返(t *testing.T) {
	c := mustNew(t, map[int]string{1: "key-one"}, 1)
	const plain = "sk-上游密钥-abc123"
	ct, err := c.Encrypt(plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ct, plain) {
		t.Fatal("密文中不应出现明文")
	}
	got, err := c.Decrypt(ct)
	if err != nil {
		t.Fatal(err)
	}
	if got != plain {
		t.Fatalf("往返不一致: %q", got)
	}
}

// 相同明文两次加密必须产生不同密文。
// 否则攻击者可以通过比对密文判断两个 Provider 是否用了同一个 key。
func TestEncrypt_随机nonce(t *testing.T) {
	c := mustNew(t, map[int]string{1: "key-one"}, 1)
	a, _ := c.Encrypt("same")
	b, _ := c.Encrypt("same")
	if a == b {
		t.Fatal("相同明文两次加密产生了相同密文，nonce 未随机化")
	}
}

// 轮换的核心：换 key 后旧密文仍可解，新密文用新 key。
// 做不到这点，轮换就必须停机重写全表，实际上永远不会发生。
func TestRotation_旧密文仍可解(t *testing.T) {
	old := mustNew(t, map[int]string{1: "key-one"}, 1)
	ctV1, _ := old.Encrypt("secret-v1")

	rotated := mustNew(t, map[int]string{1: "key-one", 2: "key-two"}, 2)

	got, err := rotated.Decrypt(ctV1)
	if err != nil {
		t.Fatalf("轮换后旧密文应仍可解: %v", err)
	}
	if got != "secret-v1" {
		t.Fatalf("旧密文解出 %q", got)
	}
	if !rotated.NeedsRotation(ctV1) {
		t.Error("旧版本密文应被标记为需轮换")
	}

	ctV2, _ := rotated.Encrypt("secret-v2")
	if !strings.HasPrefix(ctV2, "v2:") {
		t.Errorf("新密文应使用当前版本，实际 %q", ctV2[:3])
	}
	if rotated.NeedsRotation(ctV2) {
		t.Error("当前版本密文不应被标记为需轮换")
	}
}

func TestDecrypt_未知版本(t *testing.T) {
	c1 := mustNew(t, map[int]string{9: "k9"}, 9)
	ct, _ := c1.Encrypt("x")
	c2 := mustNew(t, map[int]string{1: "k1"}, 1)
	if _, err := c2.Decrypt(ct); err == nil {
		t.Fatal("未知版本应解密失败")
	}
}

// 篡改密文必须失败（GCM 的认证标签），且错误信息不区分失败原因。
func TestDecrypt_篡改被拒(t *testing.T) {
	c := mustNew(t, map[int]string{1: "k"}, 1)
	ct, _ := c.Encrypt("payload")
	tampered := ct[:len(ct)-3] + "AAA"
	if _, err := c.Decrypt(tampered); err == nil {
		t.Fatal("被篡改的密文应解密失败")
	}
}

func TestDecrypt_格式非法(t *testing.T) {
	c := mustNew(t, map[int]string{1: "k"}, 1)
	for _, bad := range []string{"", "nope", "v:abc", "vx:abc", "v1"} {
		if _, err := c.Decrypt(bad); err == nil {
			t.Errorf("非法密文 %q 应被拒绝", bad)
		}
	}
}

func TestNew_拒绝空密钥与错误current(t *testing.T) {
	if _, err := New(nil, 1); err == nil {
		t.Error("空密钥集合应被拒绝")
	}
	if _, err := New(map[int]string{1: "  "}, 1); err == nil {
		t.Error("空白密钥材料应被拒绝")
	}
	if _, err := New(map[int]string{1: "k"}, 2); err == nil {
		t.Error("current 不在集合中应被拒绝")
	}
}

// 掩码不能泄漏足以复原的信息，也不能把短密钥暴露大半。
func TestMask(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"short", "*****"},
		{"12345678", "********"},
		{"sk-abcdefghijkl", "sk-a*******ijkl"},
	}
	for _, c := range cases {
		if got := Mask(c.in); got != c.want {
			t.Errorf("Mask(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}
