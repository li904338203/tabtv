package domain

import (
	"crypto/sha256"
	"errors"
	"io"
	"math/big"
	"strings"
)

// 访问密钥（MCP-F-03/04、开发 01 §7.1 + §12 R4）。
//
//	sk_mcp_ + 32 字节随机数的 base62（定长 43 位，左补 0）
//
// 前缀让人和扫描工具一眼认出这是什么（泄漏扫描、日志打码都靠它）；定长让 ParseKey 能在
// 查库之前就把明显不是密钥的东西挡掉 —— 尤其是用户端 / 管理端 JWT（MCP-F-06）。
const (
	KeyPrefix = "sk_mcp_"
	// keyBodyLen 是 32 字节随机数的 base62 位数：62^43 > 2^256 > 62^42。
	keyBodyLen = 43
	keyLen     = len(KeyPrefix) + keyBodyLen
	// DisplayPrefixLen 是库里存、界面上显示的明文前缀长度（sk_mcp_ 之后的前 8 位）。
	DisplayPrefixLen = 8
)

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// ErrNotAccessKey 表示 Bearer 值不是访问密钥的形态（前缀不对 / 长度不对 / 含非法字符）。
var ErrNotAccessKey = errors.New("不是 MCP 访问密钥")

// GenerateKey 用 rand（生产传 crypto/rand.Reader）生成一把新密钥。
func GenerateKey(rand io.Reader) (string, error) { return generateOpaque(rand, KeyPrefix) }

// generateOpaque 生成「前缀 + 32 字节随机数的 43 位 base62」形态的不透明凭据
// （访问密钥与 OAuth 的令牌 / 授权码共用同一形态，只是前缀不同）。
func generateOpaque(rand io.Reader, prefix string) (string, error) {
	var b [32]byte
	if _, err := io.ReadFull(rand, b[:]); err != nil {
		return "", err
	}
	n := new(big.Int).SetBytes(b[:])
	body := make([]byte, 0, keyBodyLen)
	base := big.NewInt(62)
	mod := new(big.Int)
	for n.Sign() > 0 {
		n.DivMod(n, base, mod)
		body = append(body, base62[mod.Int64()])
	}
	for len(body) < keyBodyLen {
		body = append(body, '0')
	}
	// 上面是低位在前，翻转成高位在前（只为可读；顺序本身不影响安全性）。
	for i, j := 0, len(body)-1; i < j; i, j = i+1, j-1 {
		body[i], body[j] = body[j], body[i]
	}
	return prefix + string(body), nil
}

// isOpaque 判断 raw 是不是「prefix + 43 位 base62」形态（不查库）。
func isOpaque(raw, prefix string) bool {
	if len(raw) != len(prefix)+keyBodyLen || !strings.HasPrefix(raw, prefix) {
		return false
	}
	for i := len(prefix); i < len(raw); i++ {
		if strings.IndexByte(base62, raw[i]) < 0 {
			return false
		}
	}
	return true
}

// ParseKey 校验 Bearer 值是不是访问密钥的形态。**不查库**：形态不对的直接拒，
// 用户端 / 管理端 JWT（`eyJ…`，含 `.`）在这里就被挡下（MCP-F-06）。
func ParseKey(raw string) (string, error) {
	if !isOpaque(raw, KeyPrefix) {
		return "", ErrNotAccessKey
	}
	return raw, nil
}

// HashKey 是库里存的哈希：完整密钥（含前缀）的 sha256，**不加盐**（R4：高熵随机串，
// 要按哈希走唯一索引直查；每把各加随机盐的话就只能全表比对）。
func HashKey(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}

// DisplayPrefix 取库里存的明文前缀（sk_mcp_ 之后的前 8 位）。
func DisplayPrefix(key string) string {
	body := strings.TrimPrefix(key, KeyPrefix)
	if len(body) > DisplayPrefixLen {
		body = body[:DisplayPrefixLen]
	}
	return body
}
