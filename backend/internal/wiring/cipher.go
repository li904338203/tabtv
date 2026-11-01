package wiring

import (
	"github.com/li904338203/tabtv/backend/internal/platform/config"
	"github.com/li904338203/tabtv/backend/internal/platform/secret"
)

// NewCipher 构造两个进程共用的密钥解密器（AGT-01：只收口构造，不改行为）。
//
// fallback 是未配置 SEEAITV_SECRET_KEY 时的开发回落材料，**由调用方决定**：
// API 传它实际使用的 JWT 密钥（不足 32 字节时是随机生成的一次性密钥），
// Worker 传 cfg.Auth.JWTSecret。两者在开发态未配 SEEAITV_JWT_SECRET 时本来就不一致 ——
// 这是既有分叉，统一它是行为变更，不在本函数里悄悄做掉。
// 回落时的告警日志也留在调用方（各自原样保留）。
//
// ⚠️ **必须把历史版本一起交进去**，否则密钥轮换在物理上不可能：
// 密文自带版本前缀（`v1:`/`v2:`），换掉 SEEAITV_SECRET_KEY 而只给一个版本，
// 等于宣称"v1 就是新 key"——库里全部用旧 key 加的 v1 密文当场解不开
// （Provider 密钥、支付商户私钥、SMTP 凭据一起），而错误看起来像数据损坏。
// 轮换步骤见 config.oldSecretKeys 的注释与运维手册。
func NewCipher(cfg config.Config, fallback string) (*secret.Cipher, error) {
	keyMaterial := cfg.Auth.SecretKey
	if keyMaterial == "" {
		// 开发回落。production 由 config 强制要求独立配置。
		keyMaterial = fallback
	}
	keys := cfg.Auth.SecretKeys()
	if cfg.Auth.SecretKey == "" {
		keys = map[int]string{cfg.Auth.CurrentKeyVersion(): keyMaterial}
	}
	return secret.New(keys, cfg.Auth.CurrentKeyVersion())
}
