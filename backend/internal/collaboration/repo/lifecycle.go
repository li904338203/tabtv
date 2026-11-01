package repo

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/li904338203/tabtv/backend/internal/collaboration/domain"
)

// TransferOwner 把工作区转给另一个成员，一个事务三件事。
//
// ⚠️ 三件事必须在同一个事务里：
//   - workspace.owner_user_id 换人；
//   - 新 owner 的成员行改成 owner；
//   - 原 owner 降为 admin（**不是移除**）。
//
// 分开做的话，中间崩一次就会留下"两个 owner"或"一个 owner 都没有"的工作区，
// 而后者没有任何人能再转让或归档它 —— 只能改库。
//
// ⚠️ payer_account **不动**：团队工作区有自己的计费主体，转让的是管理权
// 不是账目。跟着换的话，历史消费流水会指向一个新主体，客服再也查不到
// "这笔钱是谁花的"（数据模型 §4 把计费主体与协作主体分表就是为了这个）。
func (r *Repo) TransferOwner(ctx context.Context, workspaceID, fromUserID, toUserID int64) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 条件带 owner_user_id：两个人同时点转让时只有一个能成功，
	// 另一个看到的是"你已不是所有者"而不是把前一次的结果覆盖掉。
	tag, err := tx.Exec(ctx, `
		update workspace set owner_user_id = $3, updated_at = now()
		where id = $1 and owner_user_id = $2 and kind = 'team' and status = 'active'`,
		workspaceID, fromUserID, toUserID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}

	// 接手人必须已经是 active 成员：0 行更新即代表不是。
	tag, err = tx.Exec(ctx, `
		update workspace_member set role = 'owner', updated_at = now()
		where workspace_id = $1 and user_id = $2 and status = 'active'`,
		workspaceID, toUserID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}

	// 原 owner 降为 admin 而不是移除：他多半还要继续在这个团队里干活，
	// 而移除之后他连自己刚转让的工作区都打不开了。
	if _, err := tx.Exec(ctx, `
		update workspace_member set role = 'admin', updated_at = now()
		where workspace_id = $1 and user_id = $2 and status = 'active'`,
		workspaceID, fromUserID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ArchiveResult 是一次归档的结果，供审计与提示使用。
type ArchiveResult struct {
	RevokedShares     int64
	RevokedInvitation int64
}

// Archive 归档（软删）团队工作区。
//
// 三件事一个事务：置 status='deleted' + 撤销它授出的全部共享 + 作废未接受的邀请。
//
// ⚠️ **成员行保留不动**。全部置 removed 的话，恢复时无从还原谁是谁、谁是什么角色，
// 而 ListForUser / MembershipWithPayer 本来就带 `w.status = 'active'` ——
// 归档后全员自动失去访问，不需要再动成员表。少改一张表就少一种恢复不回来的可能。
//
// ⚠️ 共享与邀请**必须跟着作废**：它们都是"让外面的人进得来"的凭据，
// 归档的语义是"这个团队关掉了"。只改 workspace.status 的话，
// 外部合作者仍能正常打开共享资源，手上的邀请码也仍然能用来加入一个已归档的工作区。
func (r *Repo) Archive(ctx context.Context, workspaceID, actorID int64) (ArchiveResult, error) {
	var out ArchiveResult
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		update workspace set status = 'deleted', deleted_at = now(), updated_at = now()
		where id = $1 and kind = 'team' and status = 'active'`, workspaceID)
	if err != nil {
		return out, err
	}
	if tag.RowsAffected() == 0 {
		return out, ErrNotFound
	}

	if out.RevokedShares, err = r.RevokeAllOfWorkspace(ctx, tx, workspaceID, actorID); err != nil {
		return out, err
	}

	inv, err := tx.Exec(ctx, `
		update workspace_invitation set status = 'revoked'
		where workspace_id = $1 and status = 'pending'`, workspaceID)
	if err != nil {
		return out, err
	}
	out.RevokedInvitation = inv.RowsAffected()
	return out, tx.Commit(ctx)
}

// ---------------------------------------------------------------- 邀请生命周期

// PendingInvitationTarget 是"同一目标已有待处理邀请"的查询结果。
type PendingInvitationTarget struct {
	ID   int64
	Role domain.Role
}

// RevokePendingForTarget 撤销同一工作区对同一目标的待处理邀请。
//
// ⚠️ 这一步在建新邀请**之前**跑，不是可选的优化：uk_invite_pending 会让
// 第二次邀请直接撞唯一索引，而 PG 报的是 23505 —— 映射到业务上就是
// 「名称已存在」，一句和"重新邀请同事"完全对不上号的错误。
// 显式撤销之后还要把这件事**告诉调用方**（上一条邀请码已失效），
// 否则用户会以为两个码都能用，把旧码发出去的人拿不到任何提示。
func (r *Repo) RevokePendingForTarget(ctx context.Context,
	workspaceID int64, kind, value string) (int, error) {
	tag, err := r.pool.Exec(ctx, `
		update workspace_invitation set status = 'revoked'
		where workspace_id = $1 and target_kind = $2 and target_value = $3
		  and status = 'pending'`, workspaceID, kind, value)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// MemberExistsByContact 判定某个联系方式对应的账号是否已是本工作区成员。
//
// ⚠️ 邀请一个已经在里面的人不是无害的：邀请码一旦被接受，
// AcceptInvitation 的 on conflict 分支会**把他的角色改成邀请里写的那个**。
// 于是"邀请同事"这个动作会悄悄把一个 admin 降成 viewer，
// 而操作者完全没有要改他角色的意思。改角色有专门的入口。
func (r *Repo) MemberExistsByContact(ctx context.Context,
	workspaceID int64, kind, value string) (bool, error) {
	col := "email"
	if kind == "phone" {
		col = "phone"
	}
	var n int
	err := r.pool.QueryRow(ctx, `
		select count(*) from workspace_member m
		join app_user u on u.id = m.user_id
		where m.workspace_id = $1 and m.status = 'active' and u.`+col+` = $2`,
		workspaceID, value).Scan(&n)
	return n > 0, err
}

// MyInvitation 是"我收到的邀请"一行。
//
// ⚠️ **不带 token**。列表接口回 token 等于把邀请码变成"只要能登录就能看到的东西"：
// 任何拿到这个账号会话的人都能直接加入，而邀请码的明文本该只在创建时出现一次。
// 接受邀请仍然要用收件箱/短信里的那个码。
type MyInvitation struct {
	ID            int64
	WorkspaceID   int64
	WorkspaceName string
	Role          domain.Role
	InvitedBy     string
	ExpiresAt     time.Time
}

// ListInvitationsFor 按联系方式列出发给我的待处理邀请。
func (r *Repo) ListInvitationsFor(ctx context.Context, email, phone string) ([]MyInvitation, error) {
	rows, err := r.pool.Query(ctx, `
		select i.id, i.workspace_id, w.name, i.role, coalesce(b.nickname, ''), i.expires_at
		from workspace_invitation i
		join workspace w on w.id = i.workspace_id and w.status = 'active'
		left join app_user b on b.id = i.invited_by
		where i.status = 'pending' and i.expires_at > now()
		  and ((i.target_kind = 'email' and i.target_value = $1 and $1 <> '')
		    or (i.target_kind = 'phone' and i.target_value = $2 and $2 <> ''))
		order by i.created_at desc`, email, phone)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MyInvitation
	for rows.Next() {
		var v MyInvitation
		if err := rows.Scan(&v.ID, &v.WorkspaceID, &v.WorkspaceName,
			&v.Role, &v.InvitedBy, &v.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ExpireInvitations 把过期的待处理邀请标成 expired，返回条数。
//
// ⚠️ 这不是"整理数据"，它是 uk_invite_pending 能用下去的前提：
// 唯一索引的条件是 status='pending'，过期的行若永远停在 pending，
// 同一个人就**再也邀请不了第二次** —— 而用户看到的错误会是「名称已存在」。
//
// 读取路径本来就带 `expires_at > now()`，所以过期邀请立刻失效不依赖这个作业；
// 作业只负责把状态落实（与提示词 ready/stale、导出过期是同一条规则：
// 对外行为算出来，作业只是把它写下来）。
func (r *Repo) ExpireInvitations(ctx context.Context, batch int) (int, error) {
	tag, err := r.pool.Exec(ctx, `
		update workspace_invitation set status = 'expired'
		where id in (
			select id from workspace_invitation
			where status = 'pending' and expires_at <= now()
			limit $1)`, batch)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// UserContact 取用户的联系方式，用于匹配发给他的邀请。
func (r *Repo) UserContact(ctx context.Context, userID int64) (email, phone string, err error) {
	err = r.pool.QueryRow(ctx,
		`select coalesce(email, ''), coalesce(phone, '') from app_user where id = $1`,
		userID).Scan(&email, &phone)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return email, phone, err
}
