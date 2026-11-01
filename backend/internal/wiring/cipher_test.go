package wiring

import (
	"testing"

	"github.com/li904338203/tabtv/backend/internal/platform/config"
	"github.com/li904338203/tabtv/backend/internal/platform/secret"
)

// 回落值由调用方决定（AGT-01）：两个进程各自传原来的回落值，本函数不替它们统一。
// 无 SecretKey 时用传入的 fallback；有 SecretKey 时忽略 fallback（且带上历史版本）。
func TestNewCipher_回落值由调用方决定(t *testing.T) {
	roundTrip := func(t *testing.T, enc *secret.Cipher, keys map[int]string, current int) bool {
		t.Helper()
		ct, err := enc.Encrypt("provider-api-key")
		if err != nil {
			t.Fatal(err)
		}
		dec, err := secret.New(keys, current)
		if err != nil {
			t.Fatal(err)
		}
		pt, err := dec.Decrypt(ct)
		return err == nil && pt == "provider-api-key"
	}

	t.Run("未配置 SecretKey 时用 fallback", func(t *testing.T) {
		c, err := NewCipher(config.Config{}, "fallback-material-A")
		if err != nil {
			t.Fatal(err)
		}
		if !roundTrip(t, c, map[int]string{1: "fallback-material-A"}, 1) {
			t.Fatal("应以传入的 fallback 为密钥材料")
		}
		if roundTrip(t, c, map[int]string{1: "fallback-material-B"}, 1) {
			t.Fatal("换一个 fallback 不应解得开")
		}
	})

	t.Run("配置了 SecretKey 时忽略 fallback", func(t *testing.T) {
		cfg := config.Config{Auth: config.Auth{
			SecretKey: "real-material", SecretKeyVersion: 2,
			OldSecretKeys: map[int]string{1: "old-material"},
		}}
		c, err := NewCipher(cfg, "fallback-material-A")
		if err != nil {
			t.Fatal(err)
		}
		if !roundTrip(t, c, map[int]string{2: "real-material"}, 2) {
			t.Fatal("应以 SecretKey 为当前版本密钥材料")
		}
		if roundTrip(t, c, map[int]string{2: "fallback-material-A"}, 2) {
			t.Fatal("配置了 SecretKey 时不应使用 fallback")
		}
		// 历史版本要一起交进去，否则轮换后旧密文解不开。
		old, err := secret.New(map[int]string{1: "old-material"}, 1)
		if err != nil {
			t.Fatal(err)
		}
		ct, err := old.Encrypt("legacy")
		if err != nil {
			t.Fatal(err)
		}
		if pt, err := c.Decrypt(ct); err != nil || pt != "legacy" {
			t.Fatalf("历史版本密文应能解开：%q %v", pt, err)
		}
	})
}
