// Package app 编排身份域用例，是事务边界所在。
package app

import (
	"context"
	"errors"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/li904338203/tabtv/backend/internal/identity/channel"
	"github.com/li904338203/tabtv/backend/internal/identity/domain"
	"github.com/li904338203/tabtv/backend/internal/identity/repo"
	"github.com/li904338203/tabtv/backend/internal/identity/token"
	"github.com/li904338203/tabtv/backend/internal/identity/verify"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/platform/secret"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

type Service struct {
	repo     *repo.Repo
	tokens   *token.Service
	codes    *verify.Service
	channels *channel.Registry
	nextID   func() int64
	revoker  SessionRevoker
	cipher   *secret.Cipher
	audit    *audit.Logger
}

// SetSecretCipher 注入密钥加密器，用于认证配置中的 SMTP/短信凭据。
func (s *Service) SetSecretCipher(c *secret.Cipher) { s.cipher = c }

// SetAudit 注入审计记录器。敏感操作必须写审计。
func (s *Service) SetAudit(a *audit.Logger) { s.audit = a }

func New(r *repo.Repo, t *token.Service, v *verify.Service, ch *channel.Registry, nextID func() int64) *Service {
	return &Service{repo: r, tokens: t, codes: v, channels: ch, nextID: nextID}
}

type TokenPair struct {
	AccessToken  string    `json:"accessToken"`
	RefreshToken string    `json:"refreshToken"`
	ExpiresAt    time.Time `json:"expiresAt"`
}

type Identity struct {
	Phone   string
	Email   string
	Channel verify.Channel
	Target  string
}

// resolve 把输入规范化并判定渠道。
func resolve(phone, email string) (Identity, error) {
	phone = domain.NormalizePhone(phone)
	email = domain.NormalizeEmail(email)
	switch {
	case phone != "" && email != "":
		return Identity{}, httpx.Errf(errcode.InvalidArgument, "手机号与邮箱只能填写一项")
	case phone != "":
		if err := domain.ValidatePhone(phone); err != nil {
			return Identity{}, httpx.Errf(errcode.InvalidArgument, err.Error())
		}
		return Identity{Phone: phone, Channel: verify.ChannelPhone, Target: phone}, nil
	case email != "":
		if err := domain.ValidateEmail(email); err != nil {
			return Identity{}, httpx.Errf(errcode.InvalidArgument, err.Error())
		}
		return Identity{Email: email, Channel: verify.ChannelEmail, Target: email}, nil
	default:
		return Identity{}, httpx.Errf(errcode.InvalidArgument, domain.ErrAccountRequired.Error())
	}
}

// SendCode 发送验证码。
//
// ⚠️ 无论账号是否存在都返回成功：否则这个接口就是一个账号枚举器，
// 攻击者可以批量探测哪些手机号已注册。
func (s *Service) SendCode(ctx context.Context, phone, email string, purpose verify.Purpose) error {
	idt, err := resolve(phone, email)
	if err != nil {
		return err
	}

	code, err := s.codes.Issue(ctx, idt.Channel, idt.Target, purpose)
	if err != nil {
		switch {
		case errors.Is(err, verify.ErrTooFrequent):
			return httpx.Errf(errcode.RateLimited, "请求过于频繁，请稍后再试")
		case errors.Is(err, verify.ErrQuotaExceeded):
			return httpx.Errf(errcode.RateLimited, "今日发送次数已达上限")
		default:
			return httpx.Wrap(errcode.Internal, err)
		}
	}

	kind := channel.SMS
	if idt.Channel == verify.ChannelEmail {
		kind = channel.Email
	}
	if !s.channels.Has(kind) {
		return httpx.Errf(errcode.VerificationChannelDown, "该渠道暂不可用")
	}
	if err := s.channels.Send(ctx, kind, idt.Target, code); err != nil {
		return httpx.Wrap(errcode.VerificationChannelDown, err)
	}
	return nil
}

type RegisterInput struct {
	Phone    string
	Email    string
	Password string
	Nickname string
	Code     string
}

func (s *Service) Register(ctx context.Context, in RegisterInput, ip, ua string) (TokenPair, error) {
	idt, err := resolve(in.Phone, in.Email)
	if err != nil {
		return TokenPair{}, err
	}
	if err := domain.ValidatePassword(in.Password); err != nil {
		return TokenPair{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	if in.Nickname == "" {
		in.Nickname = defaultNickname(idt)
	}
	if err := domain.ValidateNickname(in.Nickname); err != nil {
		return TokenPair{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}

	// 先消费验证码再建账号：顺序反了会在"码错但账号已建"时留下垃圾数据。
	if err := s.consumeCode(ctx, idt, verify.PurposeRegister, in.Code); err != nil {
		return TokenPair{}, err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		return TokenPair{}, httpx.Wrap(errcode.Internal, err)
	}

	res, err := s.repo.Register(ctx, domain.User{
		Phone: idt.Phone, Email: idt.Email,
		PasswordHash: string(hash), Nickname: in.Nickname,
	}, [3]int64{s.nextID(), s.nextID(), s.nextID()}, in.Nickname+"的空间")
	if err != nil {
		if errors.Is(err, repo.ErrDuplicate) {
			return TokenPair{}, httpx.Errf(errcode.DuplicateName, "该账号已注册")
		}
		return TokenPair{}, httpx.Wrap(errcode.Internal, err)
	}
	return s.issuePair(ctx, res.User, ip, ua)
}

type LoginInput struct {
	Phone    string
	Email    string
	Password string
	Code     string
}

func (s *Service) Login(ctx context.Context, in LoginInput, ip, ua string) (TokenPair, error) {
	idt, err := resolve(in.Phone, in.Email)
	if err != nil {
		return TokenPair{}, err
	}
	if in.Password == "" && in.Code == "" {
		return TokenPair{}, httpx.Errf(errcode.InvalidArgument, "请提供密码或验证码")
	}

	user, err := s.repo.FindByAccount(ctx, idt.Phone, idt.Email)
	if err != nil && !errors.Is(err, repo.ErrNotFound) {
		return TokenPair{}, httpx.Wrap(errcode.Internal, err)
	}
	found := err == nil

	if in.Code != "" {
		// 验证码登录：先消费码，再看账号是否存在。
		// 顺序反了会让"码是否被消费"泄漏账号存在性。
		if err := s.consumeCode(ctx, idt, verify.PurposeLogin, in.Code); err != nil {
			return TokenPair{}, err
		}
		if !found {
			return TokenPair{}, httpx.Errf(errcode.BadCredentials, "账号不存在")
		}
	} else {
		// 密码登录：账号不存在时也要走一次 bcrypt 比较，
		// 否则响应快慢会泄漏账号是否存在（时序侧信道）。
		hash := user.PasswordHash
		if !found {
			hash = dummyHash
		}
		if bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.Password)) != nil || !found {
			return TokenPair{}, httpx.Err(errcode.BadCredentials)
		}
	}

	if !user.IsActive() {
		return TokenPair{}, httpx.Err(errcode.AccountBanned)
	}
	_ = s.repo.TouchLogin(ctx, user.ID)
	return s.issuePair(ctx, user, ip, ua)
}

type ResetInput struct {
	Phone    string
	Email    string
	Code     string
	Password string
}

// ResetPassword 重置密码，并撤销该用户全部会话。
func (s *Service) ResetPassword(ctx context.Context, in ResetInput) error {
	idt, err := resolve(in.Phone, in.Email)
	if err != nil {
		return err
	}
	if err := domain.ValidatePassword(in.Password); err != nil {
		return httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	if err := s.consumeCode(ctx, idt, verify.PurposeResetPassword, in.Code); err != nil {
		return err
	}

	user, err := s.repo.FindByAccount(ctx, idt.Phone, idt.Email)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			// 码已消费但账号不存在：不回显，避免枚举。
			return httpx.Errf(errcode.BadCredentials, "重置失败，请确认账号")
		}
		return httpx.Wrap(errcode.Internal, err)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	// UpdatePassword 内部会递增 token_version，作废全部已签发 token。
	if err := s.repo.UpdatePassword(ctx, user.ID, string(hash)); err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	if _, err := s.repo.RevokeAllSessions(ctx, "user", user.ID, "password_changed"); err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	// 改密也要断开已建立的 WebSocket：否则旧连接仍在推送，
	// 用户以为"改了密码就安全了"，实际盗用者的连接还活着。
	if s.revoker != nil {
		s.revoker.RevokeUser(user.ID, "session_revoked")
	}
	return nil
}

// Refresh 换发新的 token 对。
// ⚠️ 必须同时校验会话未撤销、用户状态正常、token_version 未变（CD-10）。
func (s *Service) Refresh(ctx context.Context, refreshToken, ip, ua string) (TokenPair, error) {
	claims, err := s.tokens.Parse(refreshToken, token.KindUser, token.TypeRefresh)
	if err != nil {
		return TokenPair{}, mapTokenErr(err)
	}
	uid, err := claims.SubjectID()
	if err != nil {
		return TokenPair{}, httpx.Err(errcode.Unauthenticated)
	}

	if err := s.checkRefreshable(ctx, claims.ID); err != nil {
		return TokenPair{}, err
	}

	user, err := s.repo.FindByID(ctx, uid)
	if err != nil {
		return TokenPair{}, httpx.Err(errcode.Unauthenticated)
	}
	if !user.IsActive() {
		return TokenPair{}, httpx.Err(errcode.AccountBanned)
	}
	if user.TokenVersion != claims.TokenVersion {
		return TokenPair{}, httpx.Err(errcode.TokenRevoked)
	}

	// 轮换：旧 refresh 作废（让被盗用的窗口收敛），只留很短的宽限期给「响应丢了」的那一次（见 renew）。
	return s.renew(ctx, claims.ID, userActor(user.ID, user.TokenVersion), ip, ua)
}

func (s *Service) Me(ctx context.Context, userID int64) (domain.User, int64, error) {
	u, err := s.repo.FindByID(ctx, userID)
	if err != nil {
		return domain.User{}, 0, httpx.Err(errcode.Unauthenticated)
	}
	ws, err := s.repo.DefaultWorkspace(ctx, userID)
	if err != nil {
		return domain.User{}, 0, httpx.Wrap(errcode.Internal, err)
	}
	return u, ws, nil
}

func (s *Service) consumeCode(ctx context.Context, idt Identity, p verify.Purpose, code string) error {
	if code == "" {
		return httpx.Errf(errcode.InvalidArgument, "请提供验证码")
	}
	err := s.codes.Consume(ctx, idt.Channel, idt.Target, p, code)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, verify.ErrTooManyFailed):
		return httpx.Errf(errcode.BadVerificationCode, "错误次数过多，请重新获取验证码")
	case errors.Is(err, verify.ErrCodeInvalid):
		return httpx.Err(errcode.BadVerificationCode)
	default:
		return httpx.Wrap(errcode.Internal, err)
	}
}

func (s *Service) issuePair(ctx context.Context, u domain.User, ip, ua string) (TokenPair, error) {
	return s.openSession(ctx, userActor(u.ID, u.TokenVersion), ip, ua)
}

func mapTokenErr(err error) error {
	switch {
	case errors.Is(err, token.ErrExpired):
		return httpx.Err(errcode.TokenExpired)
	case errors.Is(err, token.ErrWrongKind):
		return httpx.Err(errcode.WrongTokenKind)
	default:
		return httpx.Err(errcode.Unauthenticated)
	}
}

func defaultNickname(idt Identity) string {
	if idt.Phone != "" {
		return "用户" + idt.Phone[len(idt.Phone)-4:]
	}
	at := 0
	for i, r := range idt.Email {
		if r == '@' {
			at = i
			break
		}
	}
	if at > 0 {
		return idt.Email[:at]
	}
	return "新用户"
}

// dummyHash 是一个真实的 bcrypt 哈希，用于账号不存在时的等时比较。
// 直接返回错误会让"账号不存在"比"密码错误"快一个数量级。
var dummyHash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"
