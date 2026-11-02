// Package secret 提供带版本的对称加密，用于 Provider API key、
// 支付商户密钥、SMTP/短信凭据等落库密文。
//
// 关键设计（07 §8.2「密文可轮换、可版本化」）：
//   - 密文自带 key 版本前缀，轮换时新密文用新 key，旧密文仍可解 ——
//     否则轮换必须停机重写全表，实际上永远不会发生。
//   - **不兼容旧系统密文**：新环境重新录入凭据（D-03、INIT-07）。
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// 密文格式： v<版本>:<base64(nonce||ciphertext||tag)>
// 前缀可见是有意的：运维一眼能看出某行用的是哪个 key 版本，
// 排查"为什么这条解不开"时不必去猜。
const prefix = "v"

var (
	ErrEmptyKey      = errors.New("密钥材料为空")
	ErrMalformed     = errors.New("密文格式非法")
	ErrUnknownVerson = errors.New("密文使用了未知的密钥版本")
)

// Cipher 持有一组带版本的密钥。current 用于加密，全部用于解密。
type Cipher struct {
	current int
	aeads   map[int]cipher.AEAD
	// plain 为 true 时不加密（tabtv：开源单机工具，Key 明文存库，dev/01 TTD-13）。
	plain bool
}

// Plain 返回一个直通的 Cipher：Encrypt / Decrypt 原样返回。
//
// 保留 Cipher 这一层而不是删掉调用点：管理端「保留 / 替换 / 清除」三态、掩码不回显、
// 日志脱敏这些逻辑都挂在它周围，直通之后它们照常工作。
func Plain() *Cipher { return &Cipher{plain: true} }

// New 建立 Cipher。keys 的 map 键是版本号，值是密钥材料（任意长度字符串）。
// current 指明加密时使用哪个版本。
func New(keys map[int]string, current int) (*Cipher, error) {
	if len(keys) == 0 {
		return nil, ErrEmptyKey
	}
	if _, ok := keys[current]; !ok {
		return nil, fmt.Errorf("current 版本 %d 不在密钥集合中", current)
	}
	c := &Cipher{current: current, aeads: make(map[int]cipher.AEAD, len(keys))}
	for ver, material := range keys {
		if strings.TrimSpace(material) == "" {
			return nil, fmt.Errorf("版本 %d 的密钥材料为空", ver)
		}
		// 用 SHA-256 把任意长度的密钥材料规约成 32 字节 AES key。
		sum := sha256.Sum256([]byte(material))
		block, err := aes.NewCipher(sum[:])
		if err != nil {
			return nil, fmt.Errorf("版本 %d 建立 AES 失败: %w", ver, err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("版本 %d 建立 GCM 失败: %w", ver, err)
		}
		c.aeads[ver] = aead
	}
	return c, nil
}

// Encrypt 用当前版本加密。每次生成随机 nonce —— GCM 下 nonce 重用会直接泄漏明文。
func (c *Cipher) Encrypt(plaintext string) (string, error) {
	if c.plain {
		return plaintext, nil
	}
	aead := c.aeads[c.current]
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("生成 nonce 失败: %w", err)
	}
	sealed := aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return prefix + strconv.Itoa(c.current) + ":" + base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt 按密文自带的版本选 key 解密。
func (c *Cipher) Decrypt(ciphertext string) (string, error) {
	if c.plain {
		return ciphertext, nil
	}
	ver, payload, err := parse(ciphertext)
	if err != nil {
		return "", err
	}
	aead, ok := c.aeads[ver]
	if !ok {
		return "", fmt.Errorf("%w: v%d", ErrUnknownVerson, ver)
	}
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", fmt.Errorf("%w: base64 解码失败", ErrMalformed)
	}
	if len(raw) < aead.NonceSize() {
		return "", fmt.Errorf("%w: 长度不足", ErrMalformed)
	}
	nonce, body := raw[:aead.NonceSize()], raw[aead.NonceSize():]
	plain, err := aead.Open(nil, nonce, body, nil)
	if err != nil {
		// 不回显底层错误：区分"key 不对"与"密文被篡改"对攻击者有价值。
		return "", errors.New("解密失败")
	}
	return string(plain), nil
}

// NeedsRotation 判断密文是否用的旧版本 key。
// 轮换作业据此挑出待重新加密的行，不必全表重写。
func (c *Cipher) NeedsRotation(ciphertext string) bool {
	if c.plain {
		return false
	}
	ver, _, err := parse(ciphertext)
	return err == nil && ver != c.current
}

// CurrentVersion 供落库时记录 key_version 列。
func (c *Cipher) CurrentVersion() int { return c.current }

// Mask 用于对外返回与日志。
// ⚠️ 管理端只能看到掩码或"是否已配置"，**掩码值不得被当作密钥重新提交**
// （02 §3.2：保留/替换/清除是三个不同操作）。
func Mask(plaintext string) string {
	switch n := len([]rune(plaintext)); {
	case n == 0:
		return ""
	case n <= 8:
		return strings.Repeat("*", n)
	default:
		r := []rune(plaintext)
		return string(r[:4]) + strings.Repeat("*", n-8) + string(r[n-4:])
	}
}

func parse(ciphertext string) (int, string, error) {
	if !strings.HasPrefix(ciphertext, prefix) {
		return 0, "", fmt.Errorf("%w: 缺少版本前缀", ErrMalformed)
	}
	idx := strings.Index(ciphertext, ":")
	if idx <= len(prefix) {
		return 0, "", fmt.Errorf("%w: 缺少分隔符", ErrMalformed)
	}
	ver, err := strconv.Atoi(ciphertext[len(prefix):idx])
	if err != nil {
		return 0, "", fmt.Errorf("%w: 版本号非数字", ErrMalformed)
	}
	return ver, ciphertext[idx+1:], nil
}
