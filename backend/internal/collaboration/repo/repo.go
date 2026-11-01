// Package repo 是协作域的数据访问层。
package repo

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/collaboration/domain"
)

var (
	ErrNotFound  = errors.New("资源不存在")
	ErrDuplicate = errors.New("已存在")
)

type Repo struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

type Workspace struct {
	ID             int64
	Name           string
	Kind           domain.Kind
	OwnerUserID    int64
	PayerAccountID int64
	Role           domain.Role // 当前用户在该工作区的角色
}

type Member struct {
	UserID   int64
	Nickname string
	Role     domain.Role
	Status   string
	JoinedAt time.Time
}

// ListForUser 列出用户有 active 成员资格的全部工作区。
func (r *Repo) ListForUser(ctx context.Context, userID int64) ([]Workspace, error) {
	rows, err := r.pool.Query(ctx, `
		select w.id, w.name, w.kind, w.owner_user_id, w.payer_account_id, m.role
		from workspace w
		join workspace_member m on m.workspace_id = w.id
		where m.user_id = $1 and m.status = 'active' and w.status = 'active'
		order by (w.kind = 'personal') desc, w.created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Workspace
	for rows.Next() {
		var w Workspace
		if err := rows.Scan(&w.ID, &w.Name, &w.Kind, &w.OwnerUserID, &w.PayerAccountID, &w.Role); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// Membership 取用户在某工作区的成员资格。
//
// ⚠️ 这是 CD-04 第 1 条校验的实现：调用者必须是该工作区的 **active** 成员。
// 第 2 条（资源自身的 workspace_id 一致）在各业务域的查询里体现。
func (r *Repo) Membership(ctx context.Context, workspaceID, userID int64) (domain.Role, error) {
	role, _, err := r.MembershipWithPayer(ctx, workspaceID, userID)
	return role, err
}

// MembershipWithPayer 一并返回**计费主体**。
//
// ⚠️ 账本只认 payer_account_id，不认 workspace/user：计费主体与协作主体的
// 生命周期不同 —— 工作区可以改名、转让、归档，而账本必须保持连续可审计。
// 在这里一次查出来，账本接口就不必为了拿 payer 再查一遍工作区。
func (r *Repo) MembershipWithPayer(ctx context.Context, workspaceID, userID int64) (
	domain.Role, int64, error) {
	var role domain.Role
	var payer int64
	err := r.pool.QueryRow(ctx, `
		select m.role, w.payer_account_id from workspace_member m
		join workspace w on w.id = m.workspace_id
		where m.workspace_id = $1 and m.user_id = $2
		  and m.status = 'active' and w.status = 'active'`,
		workspaceID, userID).Scan(&role, &payer)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, ErrNotFound
	}
	return role, payer, err
}

// CreateTeam 创建团队工作区：payer_account + workspace + owner 成员，一个事务。
//
// ⚠️ 团队工作区有**自己的** payer_account，不复用创建者的个人账户：
// 团队产生的消费必须记在团队名下，否则离职/转让时账目无法切割。
func (r *Repo) CreateTeam(ctx context.Context, ids [3]int64, name string, ownerUserID int64) (Workspace, error) {
	payerID, wsID, memberID := ids[0], ids[1], ids[2]

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Workspace{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		insert into payer_account (id, kind, owner_id) values ($1, 'workspace', $2)`,
		payerID, wsID); err != nil {
		return Workspace{}, err
	}
	if _, err := tx.Exec(ctx, `
		insert into workspace (id, name, kind, owner_user_id, payer_account_id)
		values ($1, $2, 'team', $3, $4)`, wsID, name, ownerUserID, payerID); err != nil {
		return Workspace{}, err
	}
	if _, err := tx.Exec(ctx, `
		insert into workspace_member (id, workspace_id, user_id, role)
		values ($1, $2, $3, 'owner')`, memberID, wsID, ownerUserID); err != nil {
		return Workspace{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Workspace{}, err
	}
	return Workspace{ID: wsID, Name: name, Kind: domain.KindTeam,
		OwnerUserID: ownerUserID, PayerAccountID: payerID, Role: domain.RoleOwner}, nil
}

func (r *Repo) Get(ctx context.Context, id int64) (Workspace, error) {
	var w Workspace
	err := r.pool.QueryRow(ctx, `
		select id, name, kind, owner_user_id, payer_account_id
		from workspace where id = $1 and status = 'active'`, id).
		Scan(&w.ID, &w.Name, &w.Kind, &w.OwnerUserID, &w.PayerAccountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Workspace{}, ErrNotFound
	}
	return w, err
}

func (r *Repo) Rename(ctx context.Context, id int64, name string) error {
	tag, err := r.pool.Exec(ctx,
		`update workspace set name = $2, updated_at = now() where id = $1 and status = 'active'`,
		id, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *Repo) ListMembers(ctx context.Context, workspaceID int64) ([]Member, error) {
	rows, err := r.pool.Query(ctx, `
		select m.user_id, u.nickname, m.role, m.status, m.joined_at
		from workspace_member m
		join app_user u on u.id = m.user_id
		where m.workspace_id = $1 and m.status = 'active'
		order by case m.role
			when 'owner' then 0 when 'admin' then 1 when 'editor' then 2 else 3 end,
			m.joined_at`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.UserID, &m.Nickname, &m.Role, &m.Status, &m.JoinedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (r *Repo) SetRole(ctx context.Context, workspaceID, userID int64, role domain.Role) error {
	tag, err := r.pool.Exec(ctx, `
		update workspace_member set role = $3, updated_at = now()
		where workspace_id = $1 and user_id = $2 and status = 'active'`,
		workspaceID, userID, role)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RemoveMember 移除成员（软删，保留审计痕迹）。
func (r *Repo) RemoveMember(ctx context.Context, workspaceID, userID int64) error {
	tag, err := r.pool.Exec(ctx, `
		update workspace_member
		set status = 'removed', removed_at = now(), updated_at = now()
		where workspace_id = $1 and user_id = $2 and status = 'active'`,
		workspaceID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

type Invitation struct {
	ID          int64
	WorkspaceID int64
	TargetKind  string
	TargetValue string
	Role        domain.Role
	Status      string
	ExpiresAt   time.Time
}

// CreateInvitation 建邀请。
// ⚠️ 只存 token 的哈希：邀请链接一旦泄漏（日志、截图、转发），
// 明文即可被任何人直接使用。
func (r *Repo) CreateInvitation(ctx context.Context, id int64, inv Invitation, tokenHash string, invitedBy int64) error {
	_, err := r.pool.Exec(ctx, `
		insert into workspace_invitation
		  (id, workspace_id, target_kind, target_value, role, token_hash, expires_at, invited_by)
		values ($1,$2,$3,$4,$5,$6,$7,$8)`,
		id, inv.WorkspaceID, inv.TargetKind, inv.TargetValue, inv.Role,
		tokenHash, inv.ExpiresAt, invitedBy)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrDuplicate
	}
	return err
}

// AcceptInvitation 按 token 哈希接受邀请，并在同一事务内加入成员。
//
// 条件更新保证一次性：status 必须仍是 pending 且未过期，
// 否则 0 行更新即代表「已被用过或已失效」，不会重复加入。
func (r *Repo) AcceptInvitation(ctx context.Context, tokenHash string, userID, memberID int64) (Workspace, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Workspace{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var wsID int64
	var role domain.Role
	err = tx.QueryRow(ctx, `
		update workspace_invitation
		set status = 'accepted', accepted_by_user_id = $2, accepted_at = now()
		where token_hash = $1 and status = 'pending' and expires_at > now()
		returning workspace_id, role`, tokenHash, userID).Scan(&wsID, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return Workspace{}, ErrNotFound
	}
	if err != nil {
		return Workspace{}, err
	}

	// 已是成员则恢复为 active 并更新角色；否则新建。
	// 不能直接 insert：被移除过的人再次受邀会撞 uk_ws_member。
	if _, err := tx.Exec(ctx, `
		insert into workspace_member (id, workspace_id, user_id, role)
		values ($1, $2, $3, $4)
		on conflict (workspace_id, user_id) do update
		set role = excluded.role, status = 'active', removed_at = null, updated_at = now()`,
		memberID, wsID, userID, role); err != nil {
		return Workspace{}, err
	}

	var w Workspace
	if err := tx.QueryRow(ctx, `
		select id, name, kind, owner_user_id, payer_account_id
		from workspace where id = $1`, wsID).
		Scan(&w.ID, &w.Name, &w.Kind, &w.OwnerUserID, &w.PayerAccountID); err != nil {
		return Workspace{}, err
	}
	w.Role = role
	return w, tx.Commit(ctx)
}

func (r *Repo) ListInvitations(ctx context.Context, workspaceID int64) ([]Invitation, error) {
	rows, err := r.pool.Query(ctx, `
		select id, workspace_id, target_kind, target_value, role, status, expires_at
		from workspace_invitation
		where workspace_id = $1 and status = 'pending' and expires_at > now()
		order by created_at desc`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Invitation
	for rows.Next() {
		var i Invitation
		if err := rows.Scan(&i.ID, &i.WorkspaceID, &i.TargetKind, &i.TargetValue,
			&i.Role, &i.Status, &i.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

func (r *Repo) RevokeInvitation(ctx context.Context, workspaceID, invitationID int64) error {
	tag, err := r.pool.Exec(ctx, `
		update workspace_invitation set status = 'revoked'
		where id = $1 and workspace_id = $2 and status = 'pending'`,
		invitationID, workspaceID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
