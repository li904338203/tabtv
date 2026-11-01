package oauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
)

// PKCE（RFC 7636，OAuth 2.1 必选）。**只支持 S256**：plain 等于把 verifier 原样放进授权请求的 URL，
// 截获授权码的人同时也截获了 verifier，PKCE 形同虚设。缺失 code_challenge_method 也拒
// （RFC 7636 的缺省是 plain）。

// MethodS256 是唯一接受的 code_challenge_method。
const MethodS256 = "S256"

// ValidChallenge：S256 的 challenge 是 32 字节摘要的 base64url（无填充），定长 43。
func ValidChallenge(c string) bool {
	if len(c) != 43 {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(c)
	return err == nil && len(b) == sha256.Size
}

// ValidVerifier：43~128 个 unreserved 字符（RFC 7636 §4.1）。
func ValidVerifier(v string) bool {
	if len(v) < 43 || len(v) > 128 {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '.', c == '_', c == '~':
		default:
			return false
		}
	}
	return true
}

// VerifyS256 校验 BASE64URL(SHA256(verifier)) == challenge（常量时间比较）。
func VerifyS256(verifier, challenge string) bool {
	if !ValidVerifier(verifier) || !ValidChallenge(challenge) {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(challenge)) == 1
}

// ChallengeS256 由 verifier 算出 challenge（测试与客户端用）。
func ChallengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
