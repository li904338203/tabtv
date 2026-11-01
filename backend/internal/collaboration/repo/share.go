package repo

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/li904338203/tabtv/backend/internal/collaboration/domain"
)

// Share 是一条共享授权。
type Share struct {
	ID          int64
	WorkspaceID int64
	Resource    domain.ResourceType
	ResourceID  int64
	GranteeType domain.GranteeType
	GranteeID   int64
	Permissions domain.PermSet
	GrantedBy   int64
	CreatedAt   time.Time
	// GranteeName 只在列表查询里填，用于展示"共享给了谁"。
	GranteeName string
}

const shareCols = `id, workspace_id, resource_type, resource_id,
	grantee_type, grantee_id, permissions, granted_by, created_at`

func scanShare(row pgx.Row) (Share, error) {
	var s Share
	var perms string
	err := row.Scan(&s.ID, &s.WorkspaceID, &s.Resource, &s.ResourceID,
		&s.GranteeType, &s.GranteeID, &perms, &s.GrantedBy, &s.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Share{}, ErrNotFound
	}
	s.Permissions = domain.ParsePermSet(perms)
	return s, err
}

// Grant 建一条共享授权。
//
// ⚠️ 重复授予走 **on conflict do update**，不是报 409。
// 用户的意图是"让这个人能看到"，他不关心之前有没有授过；报冲突只会逼他
// 先去列表里找到那条旧的、撤销、再授一次 —— 三步做一件事，而中间那一步
// 还会在审计里留下一条其实没发生过的"撤销"。改权限同样走这条路。
func (r *Repo) Grant(ctx context.Context, id int64, s Share) (Share, error) {
	row := r.pool.QueryRow(ctx, `
		insert into resource_share
		  (id, workspace_id, resource_type, resource_id, grantee_type, grantee_id,
		   permissions, granted_by)
		values ($1,$2,$3,$4,$5,$6,$7,$8)
		on conflict (resource_type, resource_id, grantee_type, grantee_id)
		  where revoked_at is null
		do update set permissions = excluded.permissions, granted_by = excluded.granted_by
		returning `+shareCols,
		id, s.WorkspaceID, s.Resource, s.ResourceID, s.GranteeType, s.GranteeID,
		s.Permissions.String(), s.GrantedBy)
	out, err := scanShare(row)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Share{}, ErrDuplicate
	}
	return out, err
}

// Revoke 撤销一条共享（软撤销，留痕）。
//
// ⚠️ 条件里必须带 workspace_id：少了它，任何工作区的管理员只要猜到
// share id 就能撤销别人的授权。
func (r *Repo) Revoke(ctx context.Context, workspaceID, shareID, actorID int64) (Share, error) {
	row := r.pool.QueryRow(ctx, `
		update resource_share set revoked_at = now(), revoked_by = $3
		where id = $1 and workspace_id = $2 and revoked_at is null
		returning `+shareCols, shareID, workspaceID, actorID)
	return scanShare(row)
}

// RevokeAllOfWorkspace 撤销某工作区授出的全部共享（归档时用）。
//
// ⚠️ 这一步不能省。受让方的读取路径按 resource_share 授权，
// 归档只改 workspace.status 的话，被归档工作区的资源仍然共享得好好的 ——
// owner 以为"整个团队已经关掉了"，而外部合作者还在正常打开那些资产。
func (r *Repo) RevokeAllOfWorkspace(ctx context.Context, tx pgx.Tx,
	workspaceID, actorID int64) (int64, error) {
	tag, err := tx.Exec(ctx, `
		update resource_share set revoked_at = now(), revoked_by = $2
		where workspace_id = $1 and revoked_at is null`, workspaceID, actorID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ListGranted 列出某工作区**授出**的全部有效共享。
func (r *Repo) ListGranted(ctx context.Context, workspaceID int64) ([]Share, error) {
	rows, err := r.pool.Query(ctx, `
		select s.id, s.workspace_id, s.resource_type, s.resource_id,
		       s.grantee_type, s.grantee_id, s.permissions, s.granted_by, s.created_at,
		       coalesce(u.nickname, w.name, '') as grantee_name
		from resource_share s
		-- 受让方可能是用户也可能是工作区，两张表各连一次；
		-- 用 left join 而不是两次查询：受让方被删（账号注销、工作区归档）时
		-- 这条授权仍要出现在列表里 —— 看不到它就没法撤销它。
		left join app_user  u on s.grantee_type = 'user'      and u.id = s.grantee_id
		left join workspace w on s.grantee_type = 'workspace' and w.id = s.grantee_id
		where s.workspace_id = $1 and s.revoked_at is null
		order by s.created_at desc`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Share
	for rows.Next() {
		var s Share
		var perms string
		if err := rows.Scan(&s.ID, &s.WorkspaceID, &s.Resource, &s.ResourceID,
			&s.GranteeType, &s.GranteeID, &perms, &s.GrantedBy, &s.CreatedAt,
			&s.GranteeName); err != nil {
			return nil, err
		}
		s.Permissions = domain.ParsePermSet(perms)
		out = append(out, s)
	}
	return out, rows.Err()
}

// Received 是"共享给我的"一行。
type Received struct {
	Share
	// OwnerWorkspaceName 是资源所属工作区的名字 —— 受让方要知道东西是谁给的。
	OwnerWorkspaceName string
}

// ListReceived 列出共享给某用户的全部有效授权。
//
// 命中方式有两条，都要算上：
//   - grantee_type='user' 且 grantee_id = 我；
//   - grantee_type='workspace' 且 grantee_id 是我**当前所属**的任一工作区。
//
// ⚠️ 第二条必须实时按成员资格算，不能把工作区 ID 缓存在会话里：
// 成员被移除之后，他手上的旧会话仍会带着那份工作区列表，
// 于是"移除成员"这个动作对共享资源完全不生效（NEW-18 要求撤销同步作用于 API）。
//
// ⚠️ 还要排掉源工作区已归档的：归档时会撤销全部共享，但那是应用层的一次写操作，
// 万一漏了（或历史数据里本来就有），这里是第二道防线 —— 归档的工作区不该还能
// 把资源给出去。
func (r *Repo) ListReceived(ctx context.Context, userID int64) ([]Received, error) {
	rows, err := r.pool.Query(ctx, `
		select s.id, s.workspace_id, s.resource_type, s.resource_id,
		       s.grantee_type, s.grantee_id, s.permissions, s.granted_by, s.created_at,
		       ow.name
		from resource_share s
		join workspace ow on ow.id = s.workspace_id and ow.status = 'active'
		where s.revoked_at is null
		  and (
		    (s.grantee_type = 'user' and s.grantee_id = $1)
		    or (s.grantee_type = 'workspace' and s.grantee_id in (
		          select m.workspace_id from workspace_member m
		          where m.user_id = $1 and m.status = 'active'))
		  )
		order by s.created_at desc`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Received
	for rows.Next() {
		var v Received
		var perms string
		if err := rows.Scan(&v.ID, &v.WorkspaceID, &v.Resource, &v.ResourceID,
			&v.GranteeType, &v.GranteeID, &perms, &v.GrantedBy, &v.CreatedAt,
			&v.OwnerWorkspaceName); err != nil {
			return nil, err
		}
		v.Permissions = domain.ParsePermSet(perms)
		out = append(out, v)
	}
	return out, rows.Err()
}

// EffectivePermissions 算某用户对某资源**凭共享**拥有的权限。
//
// 返回的是全部命中授权的**并集**（见 domain.PermSet.Union 的说明）。
// 一条都没命中时返回空集而不是错误 —— "没有共享给你"与"查询出错"是两件事，
// 混成一个的话一次数据库抖动会被呈现成"权限被撤销了"。
func (r *Repo) EffectivePermissions(ctx context.Context, resource domain.ResourceType,
	resourceID, userID int64) (domain.PermSet, error) {
	rows, err := r.pool.Query(ctx, `
		select s.permissions
		from resource_share s
		join workspace ow on ow.id = s.workspace_id and ow.status = 'active'
		where s.revoked_at is null
		  and s.resource_type = $1 and s.resource_id = $2
		  and (
		    (s.grantee_type = 'user' and s.grantee_id = $3)
		    or (s.grantee_type = 'workspace' and s.grantee_id in (
		          select m.workspace_id from workspace_member m
		          where m.user_id = $3 and m.status = 'active'))
		  )`, resource, resourceID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := domain.PermSet{}
	for rows.Next() {
		var perms string
		if err := rows.Scan(&perms); err != nil {
			return nil, err
		}
		out = out.Union(domain.ParsePermSet(perms))
	}
	return out, rows.Err()
}

// UserName / WorkspaceName 校验受让方真的存在，并回带它的名字。
//
// ⚠️ 不做存在性校验的话，共享列表里会挂着一条永远无人认领的授权 ——
// 而管理员看到的是"已经共享给 12345 了"，他不会想到那个 ID 根本不对应任何人。
// 顺手把名字取回来，是为了让创建响应也能显示"共享给了谁"，
// 不必让前端再查一次（而它查不到 —— 受让方可能不在当前工作区里）。
func (r *Repo) UserName(ctx context.Context, id int64) (string, bool, error) {
	var name string
	err := r.pool.QueryRow(ctx,
		`select nickname from app_user where id = $1 and status = 'active'`, id).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return name, err == nil, err
}

func (r *Repo) WorkspaceName(ctx context.Context, id int64) (string, bool, error) {
	var name string
	err := r.pool.QueryRow(ctx,
		`select name from workspace where id = $1 and status = 'active'`, id).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return name, err == nil, err
}
