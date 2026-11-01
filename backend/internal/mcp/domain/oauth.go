package domain

import (
	"io"
	"time"
)

// OAuth 2.1 的令牌与授权码（MCPT-12，开发 01 §7.2）。
//
// 与访问密钥同一形态：前缀 + 32 字节随机数的 43 位 base62，库里只存 sha256（HashKey，不加盐）。
// 前缀各不相同，于是：
//   - 凭据中间件只看前缀就知道走哪条路（sk_mcp_ 访问密钥 / mcp_at_ 访问令牌），JWT 在形态上就被挡下；
//   - 刷新令牌、授权码拿来当 Bearer 打 /mcp 时，形态上就不是访问令牌，直接 401；
//   - logredact 按前缀打码（出现在自由文本或表单请求体里也认得出来）。
const (
	AccessTokenPrefix  = "mcp_at_"
	RefreshTokenPrefix = "mcp_rt_"
	AuthCodePrefix     = "mcp_ac_"
)

// 时效（MCPT-12 卡片、开发 01 §7.2）。
const (
	// AccessTokenTTL 访问令牌 1 小时：泄漏面与刷新频率的折中。
	AccessTokenTTL = time.Hour
	// RefreshTokenTTL 刷新令牌 30 天（每次轮换重新起算：一直在用的客户端不会被踢出去）。
	RefreshTokenTTL = 30 * 24 * time.Hour
	// AuthCodeTTL 授权码 5 分钟、一次性。
	AuthCodeTTL = 5 * time.Minute
	// ConsentRequestTTL 是授权请求句柄（req）的有效期：用户在同意页上停留的上限。
	ConsentRequestTTL = 10 * time.Minute
	// RefreshReuseWindow 是被轮换掉的刷新令牌再次出现时「不连坐」的宽限期。
	// ⚠️ 与身份域 refreshReuseWindow（internal/identity/app/session.go）同值同理：服务端已轮换、
	// 响应却没回到客户端（网络断开、客户端崩溃、并发刷新）时，客户端手里只剩那张旧的 ——
	// 30 秒内再拿它来，只回 invalid_grant，不撤销整条链（否则一次网络抖动就让用户重新授权）。
	// 超过 30 秒还出现，视为泄漏：撤销整条链。
	RefreshReuseWindow = 30 * time.Second
	// CIMDCacheTTL 是客户端元数据文档的缓存时间；过期后懒刷新。
	CIMDCacheTTL = 24 * time.Hour
	// CIMDStaleGrace 是拉取失败时沿用旧缓存的额外时长（并告警）。
	CIMDStaleGrace = 24 * time.Hour
)

// GenerateAccessToken / GenerateRefreshToken / GenerateAuthCode 用 rand（生产传 crypto/rand.Reader）生成。
func GenerateAccessToken(rand io.Reader) (string, error) {
	return generateOpaque(rand, AccessTokenPrefix)
}

func GenerateRefreshToken(rand io.Reader) (string, error) {
	return generateOpaque(rand, RefreshTokenPrefix)
}

func GenerateAuthCode(rand io.Reader) (string, error) { return generateOpaque(rand, AuthCodePrefix) }

// IsAccessToken / IsRefreshToken / IsAuthCode 只看形态，不查库。
func IsAccessToken(raw string) bool  { return isOpaque(raw, AccessTokenPrefix) }
func IsRefreshToken(raw string) bool { return isOpaque(raw, RefreshTokenPrefix) }
func IsAuthCode(raw string) bool     { return isOpaque(raw, AuthCodePrefix) }
