// Package repo 是身份域的数据访问层。
//
// 手写 pgx 而非 ORM（ADR-0001 TECH-03）：本域需要带状态条件的更新、
// 部分唯一索引冲突识别与跨表事务，ORM 在这些地方只会碍事。
package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/identity/domain"
)

var (
	ErrNotFound  = errors.New("用户不存在")
	ErrDuplicate = errors.New("账号已被注册")
)

type Repo struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

const userCols = `id, coalesce(phone,''), coalesce(email,''), password_hash,
	nickname, status, token_version, is_ai_watermark_off`

func scanUser(row pgx.Row) (domain.User, error) {
	var u domain.User
	err := row.Scan(&u.ID, &u.Phone, &u.Email, &u.PasswordHash,
		&u.Nickname, &u.Status, &u.TokenVersion, &u.IsWatermarkOff)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.User{}, ErrNotFound
	}
	return u, err
}

func (r *Repo) FindByID(ctx context.Context, id int64) (domain.User, error) {
	return scanUser(r.pool.QueryRow(ctx, `select `+userCols+` from app_user where id = $1`, id))
}

// FindByAccount 按手机号或邮箱查找。传入值须已规范化。
func (r *Repo) FindByAccount(ctx context.Context, phone, email string) (domain.User, error) {
	return scanUser(r.pool.QueryRow(ctx, `select `+userCols+`
		from app_user
		where ($1 <> '' and phone = $1) or ($2 <> '' and email = $2)
		limit 1`, phone, email))
}

// RegisterResult 返回注册产生的三个实体。
type RegisterResult struct {
	User           domain.User
	WorkspaceID    int64
	PayerAccountID int64
}

// Register 在**一个事务**内创建用户、个人工作区与计费主体。
//
// ⚠️ 三者必须同时成功：有用户但没有工作区的账号，登录后每个接口都会因
// 缺少 workspace 而失败，而用户自己无法修复。
func (r *Repo) Register(ctx context.Context, u domain.User, ids [3]int64, wsName string) (RegisterResult, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return RegisterResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	userID, payerID, wsID := ids[0], ids[1], ids[2]

	_, err = tx.Exec(ctx, `
		insert into app_user (id, phone, email, password_hash, nickname)
		values ($1, nullif($2,''), nullif($3,''), $4, $5)`,
		userID, u.Phone, u.Email, u.PasswordHash, u.Nickname)
	if err != nil {
		// 部分唯一索引冲突即账号已存在。不回显是手机还是邮箱冲突 ——
		// 那等于提供了一个账号枚举接口。
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return RegisterResult{}, ErrDuplicate
		}
		return RegisterResult{}, fmt.Errorf("创建用户失败: %w", err)
	}

	// 计费主体先建：workspace 要引用它。
	if _, err = tx.Exec(ctx, `
		insert into payer_account (id, kind, owner_id) values ($1, 'user', $2)`,
		payerID, userID); err != nil {
		return RegisterResult{}, fmt.Errorf("创建计费主体失败: %w", err)
	}

	if _, err = tx.Exec(ctx, `
		insert into workspace (id, name, kind, owner_user_id, payer_account_id)
		values ($1, $2, 'personal', $3, $4)`,
		wsID, wsName, userID, payerID); err != nil {
		return RegisterResult{}, fmt.Errorf("创建个人工作区失败: %w", err)
	}

	if _, err = tx.Exec(ctx, `
		insert into workspace_member (id, workspace_id, user_id, role)
		values ($1, $2, $3, 'owner')`,
		wsID+1, wsID, userID); err != nil {
		return RegisterResult{}, fmt.Errorf("创建工作区成员失败: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return RegisterResult{}, err
	}

	u.ID = userID
	u.Status = domain.UserActive
	return RegisterResult{User: u, WorkspaceID: wsID, PayerAccountID: payerID}, nil
}

// DefaultWorkspace 取用户的个人工作区。
func (r *Repo) DefaultWorkspace(ctx context.Context, userID int64) (int64, error) {
	var id int64
	err := r.pool.QueryRow(ctx, `
		select id from workspace
		where owner_user_id = $1 and kind = 'personal' and status = 'active'
		limit 1`, userID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

// UpdatePassword 改密并**递增 token_version**，一次性作废该用户全部已签发 token。
// 这是 CD-10 的快路径：JWT 校验时比对版本号即可，无需查表。
func (r *Repo) UpdatePassword(ctx context.Context, userID int64, hash string) error {
	tag, err := r.pool.Exec(ctx, `
		update app_user
		set password_hash = $2, token_version = token_version + 1, updated_at = now()
		where id = $1 and status = 'active'`, userID, hash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchAdminLogin 记下管理员最近一次登录。
//
// ⚠️ 此前只有用户端登录写 last_login_at，后台登录从来不写 —— 管理员列表的「最近登录」
// 对所有人都是「从未登录」，而那正是判断「这个账号还有没有人在用」的依据（P15-05 发现）。
func (r *Repo) TouchAdminLogin(ctx context.Context, adminID int64) error {
	_, err := r.pool.Exec(ctx,
		`update admin_user set last_login_at = now() where id = $1`, adminID)
	return err
}

func (r *Repo) TouchLogin(ctx context.Context, userID int64) error {
	_, err := r.pool.Exec(ctx,
		`update app_user set last_login_at = now() where id = $1`, userID)
	return err
}

// CanAccessResource 判定用户对某资源是否有访问权。
//
// 统一走「资源 → workspace_id → active 成员资格」这一条路径（AC-02），
// 每种资源只是取 workspace_id 的方式不同，**判定逻辑只有一份**。
func (r *Repo) CanAccessResource(ctx context.Context, userID int64, kind string, id int64) (bool, error) {
	var q string
	switch kind {
	case "workspace":
		q = `select exists(
			select 1 from workspace_member
			where workspace_id = $2 and user_id = $1 and status = 'active')`
	case "project":
		q = `select exists(
			select 1 from project p
			join workspace_member m on m.workspace_id = p.workspace_id
			where p.id = $2 and m.user_id = $1 and m.status = 'active' and p.status = 'active')`
	case "generation":
		q = `select exists(
			select 1 from generation g
			join workspace_member m on m.workspace_id = g.workspace_id
			where g.id = $2 and m.user_id = $1 and m.status = 'active')`
	default:
		return false, nil
	}
	var ok bool
	if err := r.pool.QueryRow(ctx, q, userID, id).Scan(&ok); err != nil {
		return false, err
	}
	return ok, nil
}
