package app

import (
	"context"
	"fmt"
	"time"

	"github.com/li904338203/tabtv/backend/internal/identity/repo"
	"github.com/li904338203/tabtv/backend/internal/identity/token"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// refreshReuseWindow 是被轮换掉的 refresh 还能「接上这条链当前那一张」的宽限期。
//
// ⚠️ 起因（会话修复时实测踩到）：服务端已经轮换、响应却没回到浏览器 —— 续期途中刷新或关闭页面、
// 移动网络断开、两个标签页同时续 —— 浏览器手里只剩那张已作废的旧 refresh，下一次续期必然 40103，
// 用户被踢回登录页，而服务端与前端各自都"没错"。
//
// 宽限期内拿旧 refresh 来续，拿到的是**这条链当前那一张会话**的凭证：不再轮换、也不新开会话。
// 于是两个同时续的标签页最后持有的是同一个会话（不管谁的响应最后写进 cookie 都有效），退出登录撤销的也是它。
// ⚠️ 不能在宽限期里「再轮换一次」：那样两个标签页里总有一个最后写进存储的是已被轮换掉的那张，
//
//	15 分钟后下一次续期照样被踢。
//
// ⚠️ 只对「被轮换掉的」生效：退出、封禁、改密撤销的一律立即失效（ChainHead 只沿 rotated 走）。
// 窗口要短：被盗的旧 refresh 在这段时间里同样接得上。
const refreshReuseWindow = 30 * time.Second

// sessionActor 是签发凭证要的主体信息（用户与管理员共用一套续期逻辑）。
type sessionActor struct {
	kind    token.Kind // 凭证类型：用户 / 管理员，⚠️ 签错类型会让 token 能调另一侧的接口
	row     string     // auth_session.actor_kind
	id      int64
	version int    // token_version：改密、封禁、停用都会让它变，旧凭证随之失效
	prefix  string // jti 前缀（用户 a-/r-、管理员 aa-/ar-），只为排障时一眼认出是谁的
}

func userActor(id int64, version int) sessionActor {
	return sessionActor{kind: token.KindUser, row: "user", id: id, version: version}
}

func adminActor(id int64, version int) sessionActor {
	return sessionActor{kind: token.KindAdmin, row: "admin", id: id, version: version, prefix: "a"}
}

// signPair 为 sessionJTI 这个会话签一对：refresh 的 jti 就是会话本身，access 用 sid 指向它。
func (s *Service) signPair(a sessionActor, sessionJTI string) (TokenPair, time.Time, error) {
	accessJTI := fmt.Sprintf("%sa-%d", a.prefix, s.nextID())
	access, err := s.tokens.IssueAccess(a.id, a.kind, a.version, accessJTI, sessionJTI)
	if err != nil {
		return TokenPair{}, time.Time{}, httpx.Wrap(errcode.Internal, err)
	}
	refresh, err := s.tokens.Issue(a.id, a.kind, token.TypeRefresh, a.version, sessionJTI)
	if err != nil {
		return TokenPair{}, time.Time{}, httpx.Wrap(errcode.Internal, err)
	}
	return TokenPair{AccessToken: access.Token, RefreshToken: refresh.Token, ExpiresAt: access.ExpiresAt}, refresh.ExpiresAt, nil
}

func (s *Service) newSessionJTI(a sessionActor) string {
	return fmt.Sprintf("%sr-%d", a.prefix, s.nextID())
}

// openSession 登录：开一个新会话并签一对。
// 只登记 refresh：access 短命且量大，逐条落库既无必要也会把表撑大；撤销 access 靠 token_version 快路径。
func (s *Service) openSession(ctx context.Context, a sessionActor, ip, ua string) (TokenPair, error) {
	jti := s.newSessionJTI(a)
	pair, refreshExp, err := s.signPair(a, jti)
	if err != nil {
		return TokenPair{}, err
	}
	if err := s.repo.CreateSession(ctx, s.nextID(), repo.Session{
		JTI: jti, ActorKind: a.row, ActorID: a.id, ExpiresAt: refreshExp,
	}, ip, ua); err != nil {
		return TokenPair{}, httpx.Wrap(errcode.Internal, err)
	}
	return pair, nil
}

// checkRefreshable 先判会话：能不能续，不能的话没必要再去查账号。
func (s *Service) checkRefreshable(ctx context.Context, jti string) error {
	_, _, err := s.resolveRefresh(ctx, jti, time.Now())
	return err
}

// resolveRefresh 判断一张 refresh 能续哪个会话：
//   - 它自己还有效 → 正常轮换它（rotate=true）；
//   - 它在宽限期内被轮换掉了、而链上当前那一张还有效 → 为那一张补签（rotate=false）；
//   - 其余（退出、封禁、改密、过期、宽限期已过）→ 40103。
func (s *Service) resolveRefresh(ctx context.Context, jti string, now time.Time) (string, bool, error) {
	st, ok, err := s.repo.FindSession(ctx, jti)
	if err != nil {
		return "", false, httpx.Wrap(errcode.Internal, err)
	}
	if !ok {
		return "", false, httpx.Err(errcode.TokenRevoked)
	}
	if st.Active(now) {
		return jti, true, nil
	}
	if st.RevokedReason != repo.RevokedRotated || st.RevokedAt == nil || now.Sub(*st.RevokedAt) > refreshReuseWindow {
		return "", false, httpx.Err(errcode.TokenRevoked)
	}
	head, ok, err := s.repo.ChainHead(ctx, jti)
	if err != nil {
		return "", false, httpx.Wrap(errcode.Internal, err)
	}
	if !ok || !head.Active(now) {
		return "", false, httpx.Err(errcode.TokenRevoked)
	}
	return head.JTI, false, nil
}

// renew 用一张（已验签、账号已核过的）refresh 换一对新的。
// 正常时轮换（旧的作废、开后继）；宽限期内为链上当前那一张补签（见 refreshReuseWindow）。
func (s *Service) renew(ctx context.Context, jti string, a sessionActor, ip, ua string) (TokenPair, error) {
	// 第二轮只在轮换的 CAS 输给并发续期时才会走到：再判一次就落到宽限期那条路，接上它刚轮换出来的后继。
	for range 2 {
		target, rotate, err := s.resolveRefresh(ctx, jti, time.Now())
		if err != nil {
			return TokenPair{}, err
		}
		if !rotate {
			pair, _, err := s.signPair(a, target)
			return pair, err
		}
		next := s.newSessionJTI(a)
		pair, refreshExp, err := s.signPair(a, next)
		if err != nil {
			return TokenPair{}, err
		}
		ok, err := s.repo.RotateSession(ctx, jti, s.nextID(), repo.Session{
			JTI: next, ActorKind: a.row, ActorID: a.id, ExpiresAt: refreshExp,
		}, ip, ua)
		if err != nil {
			return TokenPair{}, httpx.Wrap(errcode.Internal, err)
		}
		if ok {
			return pair, nil
		}
	}
	return TokenPair{}, httpx.Err(errcode.TokenRevoked)
}

// Logout 结束 sessionID 所在的那条会话链。sessionID 取 Claims.Session()（access 所属那一对的 refresh jti）。
//
// ⚠️ 要沿轮换链撤销到当前那一张：手里的 access 可能属于一张已被轮换掉的 refresh（续期响应丢了，
//
//	或者别的标签页刚续过而这个请求带的还是旧 access）—— 只撤销 sid 自己的话那一行早已是 rotated，
//	等于什么都没做，而这次登录真正在用的后继会话还能续 30 天。
func (s *Service) Logout(ctx context.Context, sessionID string) error {
	head, ok, err := s.repo.ChainHead(ctx, sessionID)
	if err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	if !ok {
		return nil // 没登记过的（旧版本签发、不带 sid 的 access）：没有可撤销的
	}
	if err := s.repo.RevokeSession(ctx, head.JTI, "logout"); err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	return nil
}
