package repo

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type AdminSummary struct {
	ID          int64
	Username    string
	Status      string
	Roles       []string
	CreatedAt   time.Time
	LastLoginAt *time.Time
}

func (r *Repo) ListAdmins(ctx context.Context) ([]AdminSummary, error) {
	rows, err := r.pool.Query(ctx, `
		select a.id, a.username, a.status,
		       coalesce(array_agg(r.code) filter (where r.code is not null), '{}'),
		       a.created_at, a.last_login_at
		from admin_user a
		left join admin_user_role ar on ar.admin_user_id = a.id
		left join role r on r.id = ar.role_id
		group by a.id, a.username, a.status, a.created_at, a.last_login_at
		order by a.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminSummary
	for rows.Next() {
		var a AdminSummary
		if err := rows.Scan(&a.ID, &a.Username, &a.Status, &a.Roles, &a.CreatedAt, &a.LastLoginAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r *Repo) CreateAdmin(ctx context.Context, id int64, username, passwordHash string, roleCodes []string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`insert into admin_user (id, username, password_hash) values ($1,$2,$3)`,
		id, username, passwordHash); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrDuplicate
		}
		return err
	}
	// 角色不存在则整体失败，不静默创建一个无权限的管理员。
	for i, code := range roleCodes {
		tag, err := tx.Exec(ctx, `
			insert into admin_user_role (id, admin_user_id, role_id)
			select $1, $2, id from role where code = $3`, id+int64(i)+1, id, code)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errors.New("角色不存在: " + code)
		}
	}
	return tx.Commit(ctx)
}

// SetAdminStatus 启停管理员，并撤销其全部会话。
// ⚠️ 与封禁用户同理：只改状态不撤会话，被停用的管理员手里的 token 仍能用到过期。
func (r *Repo) SetAdminStatus(ctx context.Context, adminID int64, status string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		update admin_user set status = $2, token_version = token_version + 1, updated_at = now()
		where id = $1`, adminID, status)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if status == "disabled" {
		if _, err := tx.Exec(ctx, `
			update auth_session set revoked_at = now(), revoked_reason = 'admin_revoked'
			where actor_kind = 'admin' and actor_id = $1 and revoked_at is null`, adminID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// SetAdminRoles 覆盖式设置角色。
func (r *Repo) SetAdminRoles(ctx context.Context, adminID int64, nextID func() int64, roleCodes []string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `delete from admin_user_role where admin_user_id = $1`, adminID); err != nil {
		return err
	}
	for _, code := range roleCodes {
		tag, err := tx.Exec(ctx, `
			insert into admin_user_role (id, admin_user_id, role_id)
			select $1, $2, id from role where code = $3`, nextID(), adminID, code)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errors.New("角色不存在: " + code)
		}
	}
	return tx.Commit(ctx)
}

// AdminHasRole 管理员当前是否持有某个角色。
func (r *Repo) AdminHasRole(ctx context.Context, adminID int64, roleCode string) (bool, error) {
	var has bool
	err := r.pool.QueryRow(ctx, `
		select exists (
		  select 1 from admin_user_role ar join role ro on ro.id = ar.role_id
		  where ar.admin_user_id = $1 and ro.code = $2)`, adminID, roleCode).Scan(&has)
	return has, err
}

// CountActiveAdminsWithRole 持有某个角色的**启用中**管理员数（排除 excludeAdminID）。
func (r *Repo) CountActiveAdminsWithRole(ctx context.Context, roleCode string, excludeAdminID int64) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, `
		select count(distinct a.id) from admin_user a
		join admin_user_role ar on ar.admin_user_id = a.id
		join role ro on ro.id = ar.role_id
		where ro.code = $1 and a.status = 'active' and a.id <> $2`, roleCode, excludeAdminID).Scan(&n)
	return n, err
}

// RolesGrant 这组角色合起来是否授予某个权限。
func (r *Repo) RolesGrant(ctx context.Context, roleCodes []string, permCode string) (bool, error) {
	var has bool
	err := r.pool.QueryRow(ctx, `
		select exists (
		  select 1 from role ro
		  join role_permission rp on rp.role_id = ro.id
		  join permission p on p.id = rp.permission_id
		  where ro.code = any($1) and p.code = $2)`, roleCodes, permCode).Scan(&has)
	return has, err
}

type RoleDetail struct {
	Code        string
	Name        string
	Description string
	Permissions []string
}

func (r *Repo) ListRoles(ctx context.Context) ([]RoleDetail, error) {
	rows, err := r.pool.Query(ctx, `
		select r.code, r.name, coalesce(r.description,''),
		       coalesce(array_agg(p.code order by p.code) filter (where p.code is not null), '{}')
		from role r
		left join role_permission rp on rp.role_id = r.id
		left join permission p on p.id = rp.permission_id
		group by r.code, r.name, r.description
		order by r.code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RoleDetail
	for rows.Next() {
		var d RoleDetail
		if err := rows.Scan(&d.Code, &d.Name, &d.Description, &d.Permissions); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

type PermissionDetail struct {
	Code   string
	Name   string
	Module string
}

func (r *Repo) ListPermissions(ctx context.Context) ([]PermissionDetail, error) {
	rows, err := r.pool.Query(ctx,
		`select code, name, module from permission order by module, code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PermissionDetail
	for rows.Next() {
		var p PermissionDetail
		if err := rows.Scan(&p.Code, &p.Name, &p.Module); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetRolePermissions 覆盖式设置角色权限。
func (r *Repo) SetRolePermissions(ctx context.Context, roleCode string, nextID func() int64, permCodes []string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var roleID int64
	err = tx.QueryRow(ctx, `select id from role where code = $1`, roleCode).Scan(&roleID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `delete from role_permission where role_id = $1`, roleID); err != nil {
		return err
	}
	for _, code := range permCodes {
		tag, err := tx.Exec(ctx, `
			insert into role_permission (id, role_id, permission_id)
			select $1, $2, id from permission where code = $3`, nextID(), roleID, code)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errors.New("权限不存在: " + code)
		}
	}
	return tx.Commit(ctx)
}

type UserSummary struct {
	ID       int64
	Nickname string
	Phone    string
	Email    string
	Status   string
	// WatermarkOff 用户自己在 /account 里关掉了 AI 水印。后台**只读**显示
	// （给后台一条改它的路径等于允许"替用户关掉 AI 标识"）。
	WatermarkOff bool
	CreatedAt    time.Time
}

// SearchUsers 按关键词搜索。返回**脱敏**账号（02 §1：列表与详情一律脱敏）。
func (r *Repo) SearchUsers(ctx context.Context, keyword, status string, limit int) ([]UserSummary, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.pool.Query(ctx, `
		select id, nickname, coalesce(phone,''), coalesce(email,''), status,
		       is_ai_watermark_off, created_at
		from app_user
		where ($1 = '' or nickname ilike '%'||$1||'%' or phone like '%'||$1||'%' or email ilike '%'||$1||'%')
		  and ($2 = '' or status = $2)
		order by created_at desc
		limit $3`, keyword, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserSummary
	for rows.Next() {
		var u UserSummary
		if err := rows.Scan(&u.ID, &u.Nickname, &u.Phone, &u.Email, &u.Status,
			&u.WatermarkOff, &u.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UpdateProfile 更新昵称与水印偏好。
func (r *Repo) UpdateProfile(ctx context.Context, userID int64, nickname string, watermarkOff *bool) error {
	tag, err := r.pool.Exec(ctx, `
		update app_user
		set nickname = coalesce(nullif($2,''), nickname),
		    is_ai_watermark_off = coalesce($3, is_ai_watermark_off),
		    updated_at = now()
		where id = $1 and status = 'active'`, userID, nickname, watermarkOff)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

type AuthSettingRow struct {
	IsPhoneEnabled         bool
	IsEmailEnabled         bool
	IsRegisterCodeRequired bool
	SMTPConfig             []byte
	SMSConfig              []byte
	KeyVersion             int
	Version                int
}

// GetAuthSetting 取单行配置；不存在则返回默认值（不自动建行，避免并发重复插入）。
func (r *Repo) GetAuthSetting(ctx context.Context) (AuthSettingRow, error) {
	var a AuthSettingRow
	err := r.pool.QueryRow(ctx, `
		select is_phone_enabled, is_email_enabled, is_register_code_required,
		       smtp_config, sms_config, key_version, version
		from auth_setting order by id limit 1`).
		Scan(&a.IsPhoneEnabled, &a.IsEmailEnabled, &a.IsRegisterCodeRequired,
			&a.SMTPConfig, &a.SMSConfig, &a.KeyVersion, &a.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return AuthSettingRow{
			IsPhoneEnabled: true, IsEmailEnabled: true, IsRegisterCodeRequired: true,
			SMTPConfig: []byte("{}"), SMSConfig: []byte("{}"), KeyVersion: 1, Version: 0,
		}, nil
	}
	return a, err
}

// UpdateAuthSetting 带版本的 upsert。version 不匹配则 0 行更新 → 冲突。
func (r *Repo) UpdateAuthSetting(ctx context.Context, a AuthSettingRow) error {
	if a.Version == 0 {
		_, err := r.pool.Exec(ctx, `
			insert into auth_setting
			  (id, is_phone_enabled, is_email_enabled, is_register_code_required,
			   smtp_config, sms_config, key_version, version)
			values (1,$1,$2,$3,$4,$5,$6,1)
			on conflict (id) do nothing`,
			a.IsPhoneEnabled, a.IsEmailEnabled, a.IsRegisterCodeRequired,
			a.SMTPConfig, a.SMSConfig, a.KeyVersion)
		return err
	}
	tag, err := r.pool.Exec(ctx, `
		update auth_setting
		set is_phone_enabled=$1, is_email_enabled=$2, is_register_code_required=$3,
		    smtp_config=$4, sms_config=$5, key_version=$6,
		    version = version + 1, updated_at = now()
		where version = $7`,
		a.IsPhoneEnabled, a.IsEmailEnabled, a.IsRegisterCodeRequired,
		a.SMTPConfig, a.SMSConfig, a.KeyVersion, a.Version)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
