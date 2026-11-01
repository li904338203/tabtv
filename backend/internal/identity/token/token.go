// Package token 签发与校验 JWT，并配合 auth_session 支持撤销。
//
// 两层撤销（修 GAP-19 / CD-10）：
//   - 快路径：claims 里带 token_version，与用户当前值比对即可，无需查库。
//     改密/封禁时把用户的 token_version +1，一次性作废全部已签发 token。
//   - 慢路径：jti 落 auth_session，单设备退出只撤销那一条。
package token

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type Kind string

const (
	KindUser  Kind = "user"
	KindAdmin Kind = "admin"
)

type Type string

const (
	TypeAccess  Type = "access"
	TypeRefresh Type = "refresh"
)

var (
	ErrInvalid   = errors.New("凭证无效")
	ErrExpired   = errors.New("凭证已过期")
	ErrWrongKind = errors.New("凭证类型不匹配")
)

type Claims struct {
	jwt.RegisteredClaims
	Kind         Kind `json:"knd"`
	TokenType    Type `json:"typ"`
	TokenVersion int  `json:"tv"`
	// SessionID 是 access token 所属会话（同一对里 refresh 的 jti）。
	//
	// ⚠️ 会话表只登记 refresh（access 短命且量大，不逐条落库）。此前退出登录撤销的是
	// access 自己的 jti —— 会话表里根本没有那一行，**什么都没撤销**：退出之后同一对里的
	// refresh 还能续期整整 30 天。带上它，退出才能真正作废这一对。旧 token 没有这一项，
	// 退出时回落到 jti（行为与修之前相同，最多 15 分钟后自然过期）。
	SessionID string `json:"sid,omitempty"`
}

// Session 退出登录时要撤销的会话：有 sid 用 sid，没有（老 token、refresh 本身）用 jti。
func (c Claims) Session() string {
	if c.SessionID != "" {
		return c.SessionID
	}
	return c.ID
}

// SubjectID 取回主体 ID。对外是 string，内部转 int64（GO-34）。
func (c Claims) SubjectID() (int64, error) {
	return strconv.ParseInt(c.Subject, 10, 64)
}

type Config struct {
	Secret     []byte
	AccessTTL  time.Duration
	RefreshTTL time.Duration
	Issuer     string
}

func DefaultConfig(secret []byte) Config {
	return Config{
		Secret: secret,
		// access 短，refresh 长：access 泄漏的窗口要尽量小，
		// 而 refresh 每次使用都会查库校验会话与用户状态（CD-10）。
		AccessTTL:  15 * time.Minute,
		RefreshTTL: 30 * 24 * time.Hour,
		Issuer:     "seeaitv",
	}
}

type Service struct {
	cfg   Config
	nowFn func() time.Time
}

func New(cfg Config) (*Service, error) {
	if len(cfg.Secret) < 32 {
		return nil, fmt.Errorf("JWT 密钥过短（%d 字节），至少需要 32 字节", len(cfg.Secret))
	}
	return &Service{cfg: cfg, nowFn: time.Now}, nil
}

type Issued struct {
	Token     string
	JTI       string
	ExpiresAt time.Time
}

// Issue 签发一个 token。jti 由调用方提供，以便与 auth_session 行对应。
func (s *Service) Issue(subject int64, kind Kind, typ Type, tokenVersion int, jti string) (Issued, error) {
	return s.issue(subject, kind, typ, tokenVersion, jti, "")
}

// IssueAccess 签发 access token，并记下它所属的会话（同一对里 refresh 的 jti），
// 退出登录靠它撤销整对（见 Claims.SessionID）。
func (s *Service) IssueAccess(subject int64, kind Kind, tokenVersion int, jti, sessionID string) (Issued, error) {
	return s.issue(subject, kind, TypeAccess, tokenVersion, jti, sessionID)
}

func (s *Service) issue(subject int64, kind Kind, typ Type, tokenVersion int, jti, sessionID string) (Issued, error) {
	ttl := s.cfg.AccessTTL
	if typ == TypeRefresh {
		ttl = s.cfg.RefreshTTL
	}
	now := s.nowFn()
	exp := now.Add(ttl)

	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   strconv.FormatInt(subject, 10),
			Issuer:    s.cfg.Issuer,
			ID:        jti,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
			NotBefore: jwt.NewNumericDate(now),
		},
		Kind:         kind,
		TokenType:    typ,
		TokenVersion: tokenVersion,
		SessionID:    sessionID,
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.cfg.Secret)
	if err != nil {
		return Issued{}, fmt.Errorf("签发 token 失败: %w", err)
	}
	return Issued{Token: signed, JTI: jti, ExpiresAt: exp}, nil
}

// Parse 校验签名与有效期，并检查 kind/type 是否符合预期。
// ⚠️ 必须显式限定签名算法：不限定会让攻击者用 alg=none 伪造 token。
func (s *Service) Parse(raw string, wantKind Kind, wantType Type) (*Claims, error) {
	var claims Claims
	_, err := jwt.ParseWithClaims(raw, &claims, func(t *jwt.Token) (any, error) {
		return s.cfg.Secret, nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(s.cfg.Issuer),
	)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrExpired
		}
		return nil, ErrInvalid
	}
	// ⚠️ 用户 token 绝不能调用管理接口（GO-02）；
	// refresh token 也不能当 access 用。
	if claims.Kind != wantKind || claims.TokenType != wantType {
		return nil, ErrWrongKind
	}
	return &claims, nil
}
