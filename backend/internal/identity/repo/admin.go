package repo

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

type Admin struct {
	ID           int64
	Username     string
	PasswordHash string
	Status       string
	TokenVersion int
}

func (a Admin) IsActive() bool { return a.Status == "active" }

func (r *Repo) FindAdminByUsername(ctx context.Context, username string) (Admin, error) {
	var a Admin
	err := r.pool.QueryRow(ctx, `
		select id, username, password_hash, status, token_version
		from admin_user where username = $1`, username).
		Scan(&a.ID, &a.Username, &a.PasswordHash, &a.Status, &a.TokenVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return Admin{}, ErrNotFound
	}
	return a, err
}

func (r *Repo) FindAdminByID(ctx context.Context, id int64) (Admin, error) {
	var a Admin
	err := r.pool.QueryRow(ctx, `
		select id, username, password_hash, status, token_version
		from admin_user where id = $1`, id).
		Scan(&a.ID, &a.Username, &a.PasswordHash, &a.Status, &a.TokenVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return Admin{}, ErrNotFound
	}
	return a, err
}

// AdminPermissions 取管理员通过角色获得的全部权限码。
//
// ⚠️ 用真实的关联行计算，**不做 super 通配**：
// 通配是不可审计的隐式规则，「这个管理员到底能做什么」必须能从库里查出来（06 §1）。
func (r *Repo) AdminPermissions(ctx context.Context, adminID int64) (map[string]bool, error) {
	rows, err := r.pool.Query(ctx, `
		select distinct p.code
		from admin_user_role aur
		join role_permission rp on rp.role_id = aur.role_id
		join permission p on p.id = rp.permission_id
		where aur.admin_user_id = $1`, adminID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	perms := make(map[string]bool)
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			return nil, err
		}
		perms[code] = true
	}
	return perms, rows.Err()
}

// BanUser 封禁用户。
//
// ⚠️ 封禁必须**同时**做三件事，缺一就等于没封：
//  1. 改状态（新登录被拒）
//  2. token_version +1（已签发的 access token 立即失效）
//  3. 撤销全部会话（refresh 无法再换新 access）
//
// 旧系统只做了第 1 件，被封用户手里的 token 仍能继续用到过期（GAP-19）。
func (r *Repo) BanUser(ctx context.Context, userID int64, banned bool) error {
	status := "active"
	reason := "admin_revoked"
	if banned {
		status = "banned"
		reason = "banned"
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		update app_user
		set status = $2, token_version = token_version + 1, updated_at = now()
		where id = $1`, userID, status)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}

	if banned {
		if _, err := tx.Exec(ctx, `
			update auth_session set revoked_at = now(), revoked_reason = $3
			where actor_kind = 'user' and actor_id = $1 and revoked_at is null
			  and $2 = $2`, userID, status, reason); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (r *Repo) UserStatus(ctx context.Context, userID int64) (string, error) {
	var s string
	err := r.pool.QueryRow(ctx, `select status from app_user where id = $1`, userID).Scan(&s)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return s, err
}
