package repo

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// AccessKey 是一把访问密钥（只有哈希与前缀，**没有明文**）及其授权。
type AccessKey struct {
	ID        int64
	GrantID   int64
	Prefix    string
	ExpiresAt *time.Time
	CreatedAt time.Time
	Grant     Grant
}

// NewAccessKey 是建密钥的入参：授权与密钥在一个事务里建。
type NewAccessKey struct {
	Grant     Grant
	KeyID     int64
	Prefix    string
	KeyHash   []byte
	ExpiresAt *time.Time
}

// CreateAccessKey 建 grant + key（**一个事务**：分开写的话中途失败会留下一条没有密钥的授权，
// 它出现在「已连接的客户端」里而用户手上没有任何能用它的东西）。
func (r *Repo) CreateAccessKey(ctx context.Context, in NewAccessKey) (AccessKey, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return AccessKey{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	g := in.Grant
	if err := tx.QueryRow(ctx, `
		insert into mcp_grant (id, user_id, workspace_id, client_name, source, scopes, toolsets, require_page_confirm)
		values ($1,$2,$3,$4,'access_key',$5,$6,$7)
		returning status, created_at`,
		g.ID, g.UserID, g.WorkspaceID, g.ClientName, g.Scopes, g.Toolsets, g.RequirePageConfirm,
	).Scan(&g.Status, &g.CreatedAt); err != nil {
		return AccessKey{}, err
	}
	g.Source = SourceAccessKey
	g.KeyPrefix = in.Prefix
	k := AccessKey{ID: in.KeyID, GrantID: g.ID, Prefix: in.Prefix, ExpiresAt: in.ExpiresAt, Grant: g}
	if err := tx.QueryRow(ctx, `
		insert into mcp_access_key (id, grant_id, prefix, key_hash, expires_at)
		values ($1,$2,$3,$4,$5) returning created_at`,
		in.KeyID, g.ID, in.Prefix, in.KeyHash, in.ExpiresAt).Scan(&k.CreatedAt); err != nil {
		return AccessKey{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AccessKey{}, err
	}
	return k, nil
}

const keyCols = `k.id, k.grant_id, k.prefix, k.expires_at, k.created_at, ` + grantCols

func scanKey(row pgx.Row) (AccessKey, error) {
	var k AccessKey
	g := &k.Grant
	err := row.Scan(&k.ID, &k.GrantID, &k.Prefix, &k.ExpiresAt, &k.CreatedAt,
		&g.ID, &g.UserID, &g.WorkspaceID, &g.ClientName, &g.Source, &g.Scopes, &g.Toolsets,
		&g.RequirePageConfirm, &g.Status, &g.LastUsedAt, &g.CreatedAt, &g.RevokedAt)
	g.KeyPrefix = k.Prefix
	return k, err
}

// FindByHash 按哈希取密钥（凭据解析的唯一查询，走 uk_mcp_access_key_hash）。
// 不在 SQL 里过滤撤销 / 过期：调用方要据此回不同的错误码（TOKEN_REVOKED / TOKEN_EXPIRED）。
func (r *Repo) FindByHash(ctx context.Context, hash []byte) (AccessKey, error) {
	k, err := scanKey(r.pool.QueryRow(ctx, `
		select `+keyCols+`
		from mcp_access_key k join mcp_grant g on g.id = k.grant_id
		where k.key_hash = $1`, hash))
	if errors.Is(err, pgx.ErrNoRows) {
		return AccessKey{}, ErrNotFound
	}
	return k, err
}

// ListKeys 列出用户的全部访问密钥（含已撤销 / 已过期），新的在前，上限 200（同 ListGrants）。
func (r *Repo) ListKeys(ctx context.Context, userID int64) ([]AccessKey, error) {
	rows, err := r.pool.Query(ctx, `
		select `+keyCols+`
		from mcp_access_key k join mcp_grant g on g.id = k.grant_id
		where g.user_id = $1
		order by k.id desc
		limit 200`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AccessKey{}
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// GetKey 取用户自己的一把密钥。
func (r *Repo) GetKey(ctx context.Context, userID, keyID int64) (AccessKey, error) {
	k, err := scanKey(r.pool.QueryRow(ctx, `
		select `+keyCols+`
		from mcp_access_key k join mcp_grant g on g.id = k.grant_id
		where k.id = $1 and g.user_id = $2`, keyID, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return AccessKey{}, ErrNotFound
	}
	return k, err
}
