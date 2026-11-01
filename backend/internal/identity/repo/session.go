package repo

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type Session struct {
	JTI       string
	ActorKind string
	ActorID   int64
	ExpiresAt time.Time
	RevokedAt *time.Time
}

// CreateSession 登记一次签发。refresh 与 access 各一行，便于单独撤销。
func (r *Repo) CreateSession(ctx context.Context, id int64, s Session, ip, ua string) error {
	_, err := r.pool.Exec(ctx, `
		insert into auth_session (id, jti, actor_kind, actor_id, expires_at, ip, user_agent)
		values ($1, $2, $3, $4, $5, nullif($6,''), nullif($7,''))`,
		id, s.JTI, s.ActorKind, s.ActorID, s.ExpiresAt, ip, ua)
	return err
}

// RevokedRotated 标记「被续期轮换掉的」。
// ⚠️ 与 `logout` 分开：前者在很短的宽限期内还能接上后继（响应丢了的那一次续期），后者一秒都不能。
const RevokedRotated = "rotated"

// SessionState 是续期判定要读的那几列。
type SessionState struct {
	JTI           string
	RevokedAt     *time.Time
	RevokedReason string
	ReplacedBy    string
	ExpiresAt     time.Time
}

// Active 会话仍有效：没被撤销且没过期。
// ⚠️ refresh 必须过这一关（CD-10）：旧系统 refresh 只验签，导致退出登录后旧 refresh token 仍能继续换新 access。
func (s SessionState) Active(now time.Time) bool {
	return s.RevokedAt == nil && s.ExpiresAt.After(now)
}

// FindSession 读一张 refresh 的会话状态；不存在时 ok=false。
func (r *Repo) FindSession(ctx context.Context, jti string) (SessionState, bool, error) {
	s := SessionState{JTI: jti}
	err := r.pool.QueryRow(ctx, `
		select revoked_at, coalesce(revoked_reason, ''), coalesce(replaced_by, ''), expires_at
		from auth_session where jti = $1`, jti).
		Scan(&s.RevokedAt, &s.RevokedReason, &s.ReplacedBy, &s.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionState{}, false, nil
	}
	if err != nil {
		return SessionState{}, false, err
	}
	return s, true, nil
}

// maxChainHops 沿轮换链最多走几步。正常只有 0~2 步（入口是宽限期内的旧 refresh 或 15 分钟内的 access），
// 上限只防坏数据把查询拖成长跑。
const maxChainHops = 32

// ChainHead 从 jti 出发沿「轮换成了谁」走到这条链的最后一张，返回它的状态；jti 不存在时 ok=false。
//
// ⚠️ 只沿 rotated 走：退出、封禁、改密撤销的那一张就是终点（它的状态会说明它已失效），
//
//	宽限期因此接不上一张已经退出的会话。
func (r *Repo) ChainHead(ctx context.Context, jti string) (SessionState, bool, error) {
	var s SessionState
	err := r.pool.QueryRow(ctx, `
		with recursive chain as (
			select jti, revoked_at, revoked_reason, replaced_by, expires_at, 0 as depth
			from auth_session where jti = $1
			union all
			select n.jti, n.revoked_at, n.revoked_reason, n.replaced_by, n.expires_at, c.depth + 1
			from chain c join auth_session n on n.jti = c.replaced_by
			where c.revoked_reason = $3 and c.depth < $2
		)
		select jti, revoked_at, coalesce(revoked_reason, ''), coalesce(replaced_by, ''), expires_at
		from chain order by depth desc limit 1`, jti, maxChainHops, RevokedRotated).
		Scan(&s.JTI, &s.RevokedAt, &s.RevokedReason, &s.ReplacedBy, &s.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionState{}, false, nil
	}
	if err != nil {
		return SessionState{}, false, err
	}
	return s, true, nil
}

// RotateSession 轮换一张 refresh：旧行标成 rotated 并记下后继，同时登记后继会话 —— 一个事务。
// 返回 false 表示旧行已不是有效状态（并发续期抢先轮换了它），此时什么都没写。
//
// ⚠️ 旧行的更新是 CAS（`revoked_at is null and expires_at > now()`）：两个请求同时拿同一张 refresh 来续，
//
//	只有一个改得动，另一个由调用方按宽限期重判（接上抢先那个刚轮换出来的后继）。
//
// ⚠️ 必须与插入后继在同一个事务里：分开的话，CAS 失败会留下一个没人持有的后继会话，
//
//	插入失败则旧行已标成轮换、后继却不存在，宽限期里也接不上。
func (r *Repo) RotateSession(ctx context.Context, oldJTI string, id int64, next Session, ip, ua string) (bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		update auth_session set revoked_at = now(), revoked_reason = $2, replaced_by = $3
		where jti = $1 and revoked_at is null and expires_at > now()`,
		oldJTI, RevokedRotated, next.JTI)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `
		insert into auth_session (id, jti, actor_kind, actor_id, expires_at, ip, user_agent)
		values ($1, $2, $3, $4, $5, nullif($6,''), nullif($7,''))`,
		id, next.JTI, next.ActorKind, next.ActorID, next.ExpiresAt, ip, ua); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// RevokeSession 撤销单条会话（单设备退出）。
func (r *Repo) RevokeSession(ctx context.Context, jti, reason string) error {
	_, err := r.pool.Exec(ctx, `
		update auth_session set revoked_at = now(), revoked_reason = $2
		where jti = $1 and revoked_at is null`, jti, reason)
	return err
}

// RevokeAllSessions 撤销某主体的全部会话（封禁、改密）。
func (r *Repo) RevokeAllSessions(ctx context.Context, kind string, actorID int64, reason string) (int64, error) {
	tag, err := r.pool.Exec(ctx, `
		update auth_session set revoked_at = now(), revoked_reason = $3
		where actor_kind = $1 and actor_id = $2 and revoked_at is null`,
		kind, actorID, reason)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
