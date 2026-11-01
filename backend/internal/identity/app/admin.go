package app

import (
	"context"
	"errors"

	"golang.org/x/crypto/bcrypt"

	"github.com/li904338203/tabtv/backend/internal/identity/repo"
	"github.com/li904338203/tabtv/backend/internal/identity/token"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// SessionRevoker 由 WebSocket hub 实现，用于在撤销时断开已建立的连接。
// 定义在 app 侧而不是直接依赖 ws 包：身份域不该知道传输层细节。
type SessionRevoker interface {
	RevokeUser(userID int64, reason string)
}

// SetRevoker 注入连接撤销器。为 nil 时撤销仅作用于 API 与 refresh，
// 已建立的 WebSocket 不会被断开。
func (s *Service) SetRevoker(r SessionRevoker) { s.revoker = r }

type AdminLoginInput struct {
	Username string
	Password string
}

func (s *Service) AdminLogin(ctx context.Context, in AdminLoginInput, ip, ua string) (TokenPair, error) {
	if in.Username == "" || in.Password == "" {
		return TokenPair{}, httpx.Errf(errcode.InvalidArgument, "请填写用户名与密码")
	}

	admin, err := s.repo.FindAdminByUsername(ctx, in.Username)
	found := err == nil
	if err != nil && !errors.Is(err, repo.ErrNotFound) {
		return TokenPair{}, httpx.Wrap(errcode.Internal, err)
	}

	// 账号不存在时也走一次 bcrypt，避免时序侧信道暴露管理员用户名。
	hash := admin.PasswordHash
	if !found {
		hash = dummyHash
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.Password)) != nil || !found {
		return TokenPair{}, httpx.Err(errcode.BadCredentials)
	}
	if !admin.IsActive() {
		return TokenPair{}, httpx.Err(errcode.AccountBanned)
	}

	pair, err := s.issueAdminPair(ctx, admin, ip, ua)
	if err != nil {
		return TokenPair{}, err
	}
	// 写失败不影响登录（与用户端同一条：它只是一条记录，不是登录的前提）
	_ = s.repo.TouchAdminLogin(ctx, admin.ID)
	return pair, nil
}

func (s *Service) issueAdminPair(ctx context.Context, a repo.Admin, ip, ua string) (TokenPair, error) {
	// ⚠️ kind 必须是 KindAdmin（adminActor）。用 KindUser 签发会让这个 token 能调用户接口。
	return s.openSession(ctx, adminActor(a.ID, a.TokenVersion), ip, ua)
}

// AdminRefresh 用 refresh 换一对新的（并轮换：旧 refresh 立刻作废）。
//
// ⚠️ 此前后台**根本没有续期接口**：登录时发了 refresh，前端也不存，于是 access 的 15 分钟一到，
// 下一次操作就被踢回登录页 —— 不管中间有没有在用（P15-05 之后发现）。
// 校验与用户端同一套：会话没被撤销 · 账号还启用 · token_version 没变（停用、改密都会让它变）。
func (s *Service) AdminRefresh(ctx context.Context, refreshToken, ip, ua string) (TokenPair, error) {
	claims, err := s.tokens.Parse(refreshToken, token.KindAdmin, token.TypeRefresh)
	if err != nil {
		return TokenPair{}, mapTokenErr(err)
	}
	id, err := claims.SubjectID()
	if err != nil {
		return TokenPair{}, httpx.Err(errcode.Unauthenticated)
	}
	if err := s.checkRefreshable(ctx, claims.ID); err != nil {
		return TokenPair{}, err
	}
	a, err := s.repo.FindAdminByID(ctx, id)
	if err != nil {
		return TokenPair{}, httpx.Err(errcode.Unauthenticated)
	}
	if !a.IsActive() {
		return TokenPair{}, httpx.Err(errcode.AccountBanned)
	}
	if a.TokenVersion != claims.TokenVersion {
		return TokenPair{}, httpx.Err(errcode.TokenRevoked)
	}
	// 轮换与宽限期与用户端同一套（见 renew）。
	return s.renew(ctx, claims.ID, adminActor(a.ID, a.TokenVersion), ip, ua)
}

// AdminPermissions 取管理员权限集，供中间件判定。
func (s *Service) AdminPermissions(ctx context.Context, adminID int64) (map[string]bool, error) {
	perms, err := s.repo.AdminPermissions(ctx, adminID)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return perms, nil
}

func (s *Service) AdminByID(ctx context.Context, id int64) (repo.Admin, error) {
	a, err := s.repo.FindAdminByID(ctx, id)
	if err != nil {
		return repo.Admin{}, httpx.Err(errcode.Unauthenticated)
	}
	if !a.IsActive() {
		return repo.Admin{}, httpx.Err(errcode.AccountBanned)
	}
	return a, nil
}

// BanUser 封禁/解封用户，并断开其已建立的 WebSocket 连接。
//
// 三处撤销必须同步（NEW-18、FULL-06）：API 鉴权、媒体访问、事件订阅。
// 只改数据库状态而不断连接，被封用户仍会持续收到事件推送。
func (s *Service) BanUser(ctx context.Context, operatorID, userID int64, banned bool, reason string) error {
	before, _ := s.repo.UserStatus(ctx, userID)
	if err := s.repo.BanUser(ctx, userID, banned); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return httpx.Err(errcode.NotFound)
		}
		return httpx.Wrap(errcode.Internal, err)
	}
	if banned && s.revoker != nil {
		s.revoker.RevokeUser(userID, "session_revoked")
	}
	next := "active"
	if banned {
		next = "banned"
	}
	// 封禁是高影响操作，必须留下操作者与原因（02 §4、AC-24）。
	s.writeAudit(ctx, operatorID, "user.set_status", "app_user", userID,
		map[string]any{"status": before}, map[string]any{"status": next}, reason)
	return nil
}

func (s *Service) UserStatus(ctx context.Context, userID int64) (string, error) {
	st, err := s.repo.UserStatus(ctx, userID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return "", httpx.Err(errcode.NotFound)
		}
		return "", httpx.Wrap(errcode.Internal, err)
	}
	return st, nil
}

// CanSubscribe 判定用户能否订阅某资源的事件。
//
// 统一口径（AC-02）：资源的 workspace_id ∈ 当前用户的 active 成员资格。
// ⚠️ 成员被移除后这里立即返回 false，新的 subscribe 会被拒；
// 已建立的订阅由 hub 的 RevokeTopic/RevokeUser 主动断开。
func (s *Service) CanSubscribe(ctx context.Context, userID int64, kind string, id int64) (bool, error) {
	ok, err := s.repo.CanAccessResource(ctx, userID, kind, id)
	if err != nil {
		return false, httpx.Wrap(errcode.Internal, err)
	}
	return ok, nil
}

// AuthState 是一次鉴权所需的最小用户状态。
type AuthState struct {
	Active       bool
	TokenVersion int
}

// UserAuthState 供 RequireUser 中间件比对。
// 只取鉴权必需的两个字段，不拉整行：鉴权路径在每个请求上，要尽量轻。
func (s *Service) UserAuthState(ctx context.Context, userID int64) (AuthState, error) {
	u, err := s.repo.FindByID(ctx, userID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return AuthState{}, httpx.Err(errcode.Unauthenticated)
		}
		return AuthState{}, httpx.Wrap(errcode.Internal, err)
	}
	return AuthState{Active: u.IsActive(), TokenVersion: u.TokenVersion}, nil
}
