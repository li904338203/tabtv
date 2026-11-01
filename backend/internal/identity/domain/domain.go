// Package domain 定义身份域的实体与规则。
package domain

import (
	"errors"
	"regexp"
	"strings"
)

type UserStatus string

const (
	UserActive UserStatus = "active"
	UserBanned UserStatus = "banned"
)

type User struct {
	ID             int64
	Phone          string
	Email          string
	PasswordHash   string
	Nickname       string
	Status         UserStatus
	TokenVersion   int
	IsWatermarkOff bool
}

func (u User) IsActive() bool { return u.Status == UserActive }

type Workspace struct {
	ID             int64
	Name           string
	Kind           string
	OwnerUserID    int64
	PayerAccountID int64
}

var (
	ErrAccountRequired = errors.New("手机号与邮箱至少填写一项")
	ErrInvalidEmail    = errors.New("邮箱格式不正确")
	ErrInvalidPhone    = errors.New("手机号格式不正确")
	ErrWeakPassword    = errors.New("密码至少 8 位，且需包含字母与数字")
	ErrNicknameLength  = errors.New("昵称长度需在 1~32 字之间")
)

var (
	emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
	// 仅做基本形态校验。真实归属由验证码证明 —— 正则再严也挡不住填别人的号。
	phoneRe = regexp.MustCompile(`^1[3-9]\d{9}$`)
)

func NormalizeEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
func NormalizePhone(s string) string { return strings.TrimSpace(s) }

func ValidateEmail(s string) error {
	if !emailRe.MatchString(s) {
		return ErrInvalidEmail
	}
	return nil
}

func ValidatePhone(s string) error {
	if !phoneRe.MatchString(s) {
		return ErrInvalidPhone
	}
	return nil
}

var adminUsernameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@-]{2,63}$`)

// ErrAdminUsername 管理员用户名不合规。
var ErrAdminUsername = errors.New("管理员用户名为 3~64 位，以字母或数字开头，只能包含字母、数字与 _ . @ -")

// ValidateAdminUsername 校验管理员用户名。
//
// ⚠️ 此前只判非空：列是 varchar(64)，超长的用户名在写库时是 500；带空格的用户名
// 登录时首尾空格一被输入法吞掉就再也登不进去，而管理员以为是密码错了。
func ValidateAdminUsername(s string) error {
	if !adminUsernameRe.MatchString(s) {
		return ErrAdminUsername
	}
	return nil
}

// ValidatePassword 只做**下限**约束，不强制特殊字符、不强制定期更换 ——
// 那类规则会把用户推向"Password1!"这种可预测写法。
func ValidatePassword(s string) error {
	if len([]rune(s)) < 8 {
		return ErrWeakPassword
	}
	var hasLetter, hasDigit bool
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			hasLetter = true
		}
	}
	if !hasLetter || !hasDigit {
		return ErrWeakPassword
	}
	return nil
}

func ValidateNickname(s string) error {
	n := len([]rune(strings.TrimSpace(s)))
	if n < 1 || n > 32 {
		return ErrNicknameLength
	}
	return nil
}

// MaskPhone / MaskEmail 用于对外返回。
// 账号是个人信息，列表与详情一律返回脱敏值。
func MaskPhone(s string) string {
	r := []rune(s)
	if len(r) < 7 {
		return strings.Repeat("*", len(r))
	}
	return string(r[:3]) + "****" + string(r[len(r)-4:])
}

func MaskEmail(s string) string {
	at := strings.Index(s, "@")
	if at <= 0 {
		return "***"
	}
	name, domain := s[:at], s[at:]
	r := []rune(name)
	if len(r) <= 2 {
		return strings.Repeat("*", len(r)) + domain
	}
	return string(r[0]) + strings.Repeat("*", len(r)-2) + string(r[len(r)-1]) + domain
}
