package repo

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// 本文件是三级连接管理（用户 / 工作区 / 平台）与平台开关的数据访问（MCPT-14）。

// GrantScope 是列表与撤销的范围：零值字段不约束。
//
//	用户       → {UserID}
//	工作区管理员 → {WorkspaceID}
//	平台管理员   → {}（全站）
//
// ⚠️ 范围条件写进 **同一条 SQL 的 where**（撤销是 `update … where id and 范围 and status='active'`），
// 不是「先按 ID 查出来再比一下归属」：两步做的话中间换了归属（理论上不会，但代码不该依赖它），
// 而且「查到了但不归你」与「不存在」会被写成两个分支 —— 区分开等于给出一个枚举接口。
type GrantScope struct {
	UserID      int64
	WorkspaceID int64
}

// GrantFilter 是连接列表的筛选（管理端全用；用户 / 工作区只用范围）。
//
// ⚠️ 统计（Stats）用**同一个 GrantFilter**：汇总与列表各写一套条件的话，
// 运营看到的「共 N 次调用」与下面列出来的连接不是同一批 —— 而两个数字都"没错"（CD-56）。
type GrantFilter struct {
	GrantScope
	// ClientName 是客户端名关键词（包含匹配，不区分大小写）。
	ClientName string
	// Source：access_key | oauth；空 = 全部。
	Source string
	// Status：active | revoked；空 = 全部。
	Status string
	// Registration：cimd | dcr（只对 OAuth 有意义）；空 = 全部。
	Registration string
	// CursorID 为上一页最后一行的 id（游标分页，id 倒序）。
	CursorID int64
	Limit    int
}

// GrantRow 是列表里的一行：授权 + 客户端登记信息 + 调用次数。
type GrantRow struct {
	Grant
	// ClientRegistration：OAuth 客户端的登记方式（cimd | dcr）；访问密钥为空。
	ClientRegistration string
	// ClientKey：CIMD 是元数据文档 URL（据此显示「已验证来源（域名）」），DCR 是生成的 ID。
	ClientKey string
	// Calls 是 mcp_call_log 里该授权的调用次数 —— 日志保留 30 天，所以就是「近 30 天」。
	Calls int64
}

// grantWhere 拼列表与统计共用的 where（全部参数化，关键词不拼字面量：
// 管理端输入框里的东西进 SQL，而管理端账号恰恰权限最大）。g = mcp_grant、c = mcp_client。
func grantWhere(f GrantFilter, args []any) (string, []any) {
	var conds []string
	add := func(cond string, v any) {
		args = append(args, v)
		conds = append(conds, strings.ReplaceAll(cond, "?", "$"+strconv.Itoa(len(args))))
	}
	if f.UserID != 0 {
		add("g.user_id = ?", f.UserID)
	}
	if f.WorkspaceID != 0 {
		add("g.workspace_id = ?", f.WorkspaceID)
	}
	if f.Source != "" {
		add("g.source = ?", f.Source)
	}
	if f.Status != "" {
		add("g.status = ?", f.Status)
	}
	if f.Registration != "" {
		add("c.registration = ?", f.Registration)
	}
	if kw := strings.TrimSpace(f.ClientName); kw != "" {
		add(`g.client_name ilike ? escape '\'`, "%"+escapeLike(kw)+"%")
	}
	if len(conds) == 0 {
		return "true", args
	}
	return strings.Join(conds, " and "), args
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// ListGrantsBy 按范围与筛选列出授权（含已撤销），id 倒序、游标分页。Limit 缺省 50、上限 200。
func (r *Repo) ListGrantsBy(ctx context.Context, f GrantFilter) ([]GrantRow, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	where, args := grantWhere(f, nil)
	if f.CursorID > 0 {
		args = append(args, f.CursorID)
		where += " and g.id < $" + strconv.Itoa(len(args))
	}
	args = append(args, limit)
	rows, err := r.pool.Query(ctx, `
		select `+grantCols+`,
		       coalesce(c.registration, ''), coalesce(c.client_key, ''),
		       coalesce((select k.prefix from mcp_access_key k where k.grant_id = g.id order by k.id desc limit 1), ''),
		       (select count(*) from mcp_call_log l where l.grant_id = g.id)
		from mcp_grant g
		left join mcp_client c on c.id = g.client_id
		where `+where+`
		order by g.id desc
		limit $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GrantRow{}
	for rows.Next() {
		var row GrantRow
		var prefix string
		g, err := scanGrant(rows, &row.ClientRegistration, &row.ClientKey, &prefix, &row.Calls)
		if err != nil {
			return nil, err
		}
		g.KeyPrefix = prefix
		row.Grant = g
		out = append(out, row)
	}
	return out, rows.Err()
}

// GetGrantIn 在范围内取一条授权；不在范围内与不存在都回 ErrNotFound（不区分）。
func (r *Repo) GetGrantIn(ctx context.Context, s GrantScope, grantID int64) (Grant, error) {
	g, err := scanGrant(r.pool.QueryRow(ctx, `
		select `+grantCols+` from mcp_grant g
		where g.id = $1 and ($2::bigint = 0 or g.user_id = $2) and ($3::bigint = 0 or g.workspace_id = $3)`,
		grantID, s.UserID, s.WorkspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Grant{}, ErrNotFound
	}
	return g, err
}

// RevokeGrantIn 在范围内撤销一条授权，并在**同一事务**里作废它下面全部未作废的 OAuth 令牌。
// 返回撤销前的那一行（审计的 before）与作废的令牌数。
//
// ⚠️ 必须连带令牌：凭据链今天是先查 grant 状态的，但只改 grant 的话，哪天中间件改成
// 「先查令牌缓存」，撤销就不再实时 —— 而那不会有任何编译错误。
// 未使用的 quote 不用动：Verify 判 grant 非 active 时它自然失效。
//
// ⚠️ CAS（`status='active'`）：重复撤销回 ErrAlreadyRevoked 而不是静默成功 ——
// 静默成功的话审计里会出现两条「撤销」，而第二条什么都没发生。
func (r *Repo) RevokeGrantIn(ctx context.Context, s GrantScope, grantID int64) (Grant, int64, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Grant{}, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	g, err := scanGrant(tx.QueryRow(ctx, `
		update mcp_grant g set status = 'revoked', revoked_at = now()
		where g.id = $1 and ($2::bigint = 0 or g.user_id = $2) and ($3::bigint = 0 or g.workspace_id = $3)
		  and g.status = 'active'
		returning `+grantCols, grantID, s.UserID, s.WorkspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		if _, gerr := r.GetGrantIn(ctx, s, grantID); gerr != nil {
			return Grant{}, 0, gerr
		}
		return Grant{}, 0, ErrAlreadyRevoked
	}
	if err != nil {
		return Grant{}, 0, err
	}
	tag, err := tx.Exec(ctx, `update mcp_token set revoked_at = now() where grant_id = $1 and revoked_at is null`, grantID)
	if err != nil {
		return Grant{}, 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Grant{}, 0, err
	}
	return g, tag.RowsAffected(), nil
}

// ---------------------------------------------------------------- 平台开关

// mcpSettingID 是单行配置表 mcp_setting 的**固定主键**。
//
// ⚠️ 写死，不取雪花 ID：那样 `on conflict (id)` 永远不命中、每保存一次多一行，
// 而读取拿到的是最早那行 —— 「后台关了 MCP、保存成功，外部调用照样通」（render_setting 踩过）。
const mcpSettingID = 1

// PlatformSetting 是平台级 MCP 运行时开关。
type PlatformSetting struct {
	Enabled bool
	// Version 为 0 表示「这一行还不存在」（从没改过，按开启）。
	Version          int
	UpdatedByAdminID *int64
	UpdatedAt        *time.Time
}

// GetPlatformSetting 读平台开关；没有这一行时回 {Enabled:true, Version:0}（从没改过 = 开启）。
func (r *Repo) GetPlatformSetting(ctx context.Context) (PlatformSetting, error) {
	var s PlatformSetting
	var at time.Time
	err := r.pool.QueryRow(ctx, `
		select enabled, version, updated_by_admin_id, updated_at from mcp_setting where id = $1`, mcpSettingID).
		Scan(&s.Enabled, &s.Version, &s.UpdatedByAdminID, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return PlatformSetting{Enabled: true}, nil
	}
	if err != nil {
		return PlatformSetting{}, err
	}
	s.UpdatedAt = &at
	return s, nil
}

// SavePlatformSetting 写平台开关（upsert 到 id=1），返回写之前与之后的值。
func (r *Repo) SavePlatformSetting(ctx context.Context, enabled bool, adminID int64) (PlatformSetting, PlatformSetting, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return PlatformSetting{}, PlatformSetting{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	before := PlatformSetting{Enabled: true}
	var bat time.Time
	err = tx.QueryRow(ctx, `select enabled, version, updated_by_admin_id, updated_at from mcp_setting where id = $1 for update`,
		mcpSettingID).Scan(&before.Enabled, &before.Version, &before.UpdatedByAdminID, &bat)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return PlatformSetting{}, PlatformSetting{}, err
	default:
		before.UpdatedAt = &bat
	}
	var after PlatformSetting
	var aat time.Time
	if err := tx.QueryRow(ctx, `
		insert into mcp_setting (id, enabled, updated_by_admin_id) values ($1, $2, $3)
		on conflict (id) do update set enabled = excluded.enabled, version = mcp_setting.version + 1,
			updated_by_admin_id = excluded.updated_by_admin_id, updated_at = now()
		returning enabled, version, updated_by_admin_id, updated_at`,
		mcpSettingID, enabled, adminID).Scan(&after.Enabled, &after.Version, &after.UpdatedByAdminID, &aat); err != nil {
		return PlatformSetting{}, PlatformSetting{}, err
	}
	after.UpdatedAt = &aat
	return before, after, tx.Commit(ctx)
}
