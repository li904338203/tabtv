// Package bootstrapadmin 创建首个管理员与角色绑定（INIT-02）。
//
// 幂等：重复执行不会重复创建，也不会覆盖已有管理员的密码 ——
// 否则一次误执行就把线上管理员密码重置了。
package bootstrapadmin

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

type Result struct {
	Username string
	// Password 仅在**本次新建**时返回，且只打印一次。
	// 已存在时为空：不能从 hash 反推，也不该反推。
	Password string
	Created  bool
}

// Apply 创建首个管理员。password 为空时随机生成。
func Apply(ctx context.Context, db *sql.DB, nextID func() int64, username, password string) (Result, error) {
	if username == "" {
		username = "admin"
	}

	var existing int64
	err := db.QueryRowContext(ctx, `select id from admin_user where username = $1`, username).Scan(&existing)
	if err == nil {
		// 已存在即认为 bootstrap 完成。**不重置密码**。
		return Result{Username: username, Created: false}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Result{}, fmt.Errorf("查询管理员失败: %w", err)
	}

	generated := false
	if password == "" {
		raw := make([]byte, 18)
		if _, err := rand.Read(raw); err != nil {
			return Result{}, fmt.Errorf("生成随机密码失败: %w", err)
		}
		password = base64.RawURLEncoding.EncodeToString(raw)
		generated = true
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return Result{}, fmt.Errorf("密码哈希失败: %w", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = tx.Rollback() }()

	adminID := nextID()
	if _, err := tx.ExecContext(ctx, `
		insert into admin_user (id, username, password_hash) values ($1, $2, $3)`,
		adminID, username, string(hash)); err != nil {
		return Result{}, fmt.Errorf("创建管理员失败: %w", err)
	}

	// 绑定 super_admin 角色。角色不存在说明 seed 没跑，直接报错而不是静默建一个无权限的管理员。
	res, err := tx.ExecContext(ctx, `
		insert into admin_user_role (id, admin_user_id, role_id)
		select $1, $2, r.id from role r where r.code = 'super_admin'`,
		nextID(), adminID)
	if err != nil {
		return Result{}, fmt.Errorf("绑定角色失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Result{}, errors.New("角色 super_admin 不存在，请先执行 seed")
	}

	if err := tx.Commit(); err != nil {
		return Result{}, err
	}

	out := Result{Username: username, Created: true}
	if generated {
		out.Password = password
	}
	return out, nil
}
