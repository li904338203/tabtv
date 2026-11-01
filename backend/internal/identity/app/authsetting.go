package app

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/li904338203/tabtv/backend/internal/platform/secret"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// SecretAction 是密钥字段的三种操作（02 §3.2）。
//
// ⚠️ **必须三态而不是"传了就改、没传就不改"**：
// 后者无法表达"清空密钥"，运营只能靠填空格之类的土办法；
// 而且管理端读到的是掩码，如果把掩码原样提交就会把密钥覆盖成 `sk-****abcd`。
type SecretAction string

const (
	// SecretKeep 保留原值。管理端默认提交这个。
	SecretKeep SecretAction = "keep"
	// SecretReplace 用 value 替换。
	SecretReplace SecretAction = "replace"
	// SecretClear 清空。
	SecretClear SecretAction = "clear"
)

// SecretField 是一个密钥字段的提交形态。
type SecretField struct {
	Action SecretAction `json:"action"`
	Value  string       `json:"value,omitempty"`
}

var ErrMaskedSubmitted = errors.New("提交的值看起来是掩码，请重新输入真实密钥")

// Resolve 按三态算出新的密文。
//   - keep    → 返回原密文
//   - clear   → 返回空
//   - replace → 加密新值；**拒绝掩码形态的值**
func (f SecretField) Resolve(cipher *secret.Cipher, existing string) (string, error) {
	switch f.Action {
	case "", SecretKeep:
		return existing, nil
	case SecretClear:
		return "", nil
	case SecretReplace:
		if f.Value == "" {
			return "", errors.New("replace 必须提供 value")
		}
		// ⚠️ 掩码回传保护：管理端显示的是 sk-a****ijkl 这类掩码，
		// 用户若未改动直接提交，会把真实密钥覆盖成掩码字符串。
		if looksMasked(f.Value) {
			return "", ErrMaskedSubmitted
		}
		return cipher.Encrypt(f.Value)
	default:
		return "", errors.New("action 取值非法")
	}
}

// looksMasked 判断值是否是掩码形态（含连续 4 个及以上的 *）。
func looksMasked(s string) bool {
	run := 0
	for _, r := range s {
		if r == '*' {
			run++
			if run >= 4 {
				return true
			}
		} else {
			run = 0
		}
	}
	return false
}

// AuthSetting 是对外返回的认证配置。密钥只回显状态，**绝不回显密文或明文**。
type AuthSetting struct {
	IsPhoneEnabled         bool   `json:"isPhoneEnabled"`
	IsEmailEnabled         bool   `json:"isEmailEnabled"`
	IsRegisterCodeRequired bool   `json:"isRegisterCodeRequired"`
	SMTPHost               string `json:"smtpHost"`
	SMTPPort               int    `json:"smtpPort"`
	SMTPUsername           string `json:"smtpUsername"`
	HasSMTPPassword        bool   `json:"hasSmtpPassword"`
	SMSProvider            string `json:"smsProvider"`
	SMSAccessKeyID         string `json:"smsAccessKeyId"`
	HasSMSSecret           bool   `json:"hasSmsSecret"`
	Version                int    `json:"version"`
}

type AuthSettingUpdate struct {
	IsPhoneEnabled         *bool        `json:"isPhoneEnabled"`
	IsEmailEnabled         *bool        `json:"isEmailEnabled"`
	IsRegisterCodeRequired *bool        `json:"isRegisterCodeRequired"`
	SMTPHost               *string      `json:"smtpHost"`
	SMTPPort               *int         `json:"smtpPort"`
	SMTPUsername           *string      `json:"smtpUsername"`
	SMTPPassword           *SecretField `json:"smtpPassword"`
	SMSProvider            *string      `json:"smsProvider"`
	SMSAccessKeyID         *string      `json:"smsAccessKeyId"`
	SMSSecret              *SecretField `json:"smsSecret"`
	// Version 乐观锁：并发编辑时后提交者必须先重载（AC-03 同理）。
	Version int `json:"version"`
}

type smtpConfig struct {
	Host           string `json:"host,omitempty"`
	Port           int    `json:"port,omitempty"`
	Username       string `json:"username,omitempty"`
	PasswordCipher string `json:"passwordCipher,omitempty"`
}

type smsConfig struct {
	Provider     string `json:"provider,omitempty"`
	AccessKeyID  string `json:"accessKeyId,omitempty"`
	SecretCipher string `json:"secretCipher,omitempty"`
}

func (s *Service) GetAuthSetting(ctx context.Context) (AuthSetting, error) {
	row, err := s.repo.GetAuthSetting(ctx)
	if err != nil {
		return AuthSetting{}, httpx.Wrap(errcode.Internal, err)
	}
	var smtp smtpConfig
	var sms smsConfig
	_ = json.Unmarshal(row.SMTPConfig, &smtp)
	_ = json.Unmarshal(row.SMSConfig, &sms)

	return AuthSetting{
		IsPhoneEnabled:         row.IsPhoneEnabled,
		IsEmailEnabled:         row.IsEmailEnabled,
		IsRegisterCodeRequired: row.IsRegisterCodeRequired,
		SMTPHost:               smtp.Host,
		SMTPPort:               smtp.Port,
		SMTPUsername:           smtp.Username,
		// 只回显"是否已配置"，不回显密文也不回显掩码明文。
		HasSMTPPassword: smtp.PasswordCipher != "",
		SMSProvider:     sms.Provider,
		SMSAccessKeyID:  sms.AccessKeyID,
		HasSMSSecret:    sms.SecretCipher != "",
		Version:         row.Version,
	}, nil
}

func (s *Service) UpdateAuthSetting(ctx context.Context, in AuthSettingUpdate) (AuthSetting, error) {
	if s.cipher == nil {
		return AuthSetting{}, httpx.Errf(errcode.Internal, "密钥加密未配置")
	}
	row, err := s.repo.GetAuthSetting(ctx)
	if err != nil {
		return AuthSetting{}, httpx.Wrap(errcode.Internal, err)
	}
	if in.Version != row.Version {
		return AuthSetting{}, httpx.Errf(errcode.VersionConflict, "配置已被他人修改，请重新加载")
	}

	var smtp smtpConfig
	var sms smsConfig
	_ = json.Unmarshal(row.SMTPConfig, &smtp)
	_ = json.Unmarshal(row.SMSConfig, &sms)

	apply := func(dst *string, src *string) {
		if src != nil {
			*dst = *src
		}
	}
	apply(&smtp.Host, in.SMTPHost)
	apply(&smtp.Username, in.SMTPUsername)
	if in.SMTPPort != nil {
		smtp.Port = *in.SMTPPort
	}
	apply(&sms.Provider, in.SMSProvider)
	apply(&sms.AccessKeyID, in.SMSAccessKeyID)

	if in.SMTPPassword != nil {
		v, err := in.SMTPPassword.Resolve(s.cipher, smtp.PasswordCipher)
		if err != nil {
			return AuthSetting{}, httpx.Errf(errcode.InvalidArgument, "SMTP 密码: "+err.Error())
		}
		smtp.PasswordCipher = v
	}
	if in.SMSSecret != nil {
		v, err := in.SMSSecret.Resolve(s.cipher, sms.SecretCipher)
		if err != nil {
			return AuthSetting{}, httpx.Errf(errcode.InvalidArgument, "短信密钥: "+err.Error())
		}
		sms.SecretCipher = v
	}

	next := row
	if in.IsPhoneEnabled != nil {
		next.IsPhoneEnabled = *in.IsPhoneEnabled
	}
	if in.IsEmailEnabled != nil {
		next.IsEmailEnabled = *in.IsEmailEnabled
	}
	if in.IsRegisterCodeRequired != nil {
		next.IsRegisterCodeRequired = *in.IsRegisterCodeRequired
	}

	// ⚠️ 不能关闭全部登录渠道（02 §4）：关完之后谁也登录不进来，
	// 包括改回这个配置的管理员自己 —— 只能直接改库才能恢复。
	if !next.IsPhoneEnabled && !next.IsEmailEnabled {
		return AuthSetting{}, httpx.Errf(errcode.InvalidArgument, "至少保留一个登录渠道")
	}

	smtpJSON, _ := json.Marshal(smtp)
	smsJSON, _ := json.Marshal(sms)
	next.SMTPConfig, next.SMSConfig = smtpJSON, smsJSON
	next.KeyVersion = s.cipher.CurrentVersion()

	if err := s.repo.UpdateAuthSetting(ctx, next); err != nil {
		return AuthSetting{}, httpx.Wrap(errcode.Internal, err)
	}
	return s.GetAuthSetting(ctx)
}
