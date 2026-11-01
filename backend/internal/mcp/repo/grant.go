package repo

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrNotFound：对象不存在，或不属于调用者（不区分 —— 区分等于给出一个枚举接口）。
	ErrNotFound = errors.New("mcp: 资源不存在")
	// ErrAlreadyRevoked：撤销时它已经是 revoked。
	ErrAlreadyRevoked = errors.New("mcp: 已撤销")
)

// 授权来源与状态（与 ck_mcp_grant_source / ck_mcp_grant_status 逐字一致）。
const (
	SourceAccessKey = "access_key"
	SourceOAuth     = "oauth"

	GrantActive  = "active"
	GrantRevoked = "revoked"
)

// Repo 是 MCP 域的仓储。
type Repo struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

// Grant 是一条授权（用户 × 工作区 × 客户端）。
type Grant struct {
	ID                 int64
	UserID             int64
	WorkspaceID        int64
	ClientName         string
	Source             string
	Scopes             []string
	Toolsets           []string
	RequirePageConfirm bool
	Status             string
	LastUsedAt         *time.Time
	CreatedAt          time.Time
	RevokedAt          *time.Time
	// KeyPrefix 只在列表里给（访问密钥来源时为其前缀，便于用户认出是哪一把）。
	KeyPrefix string
}

const grantCols = `g.id, g.user_id, g.workspace_id, g.client_name, g.source, g.scopes, g.toolsets,
	g.require_page_confirm, g.status, g.last_used_at, g.created_at, g.revoked_at`

func scanGrant(row pgx.Row, extra ...any) (Grant, error) {
	var g Grant
	dest := append([]any{&g.ID, &g.UserID, &g.WorkspaceID, &g.ClientName, &g.Source, &g.Scopes, &g.Toolsets,
		&g.RequirePageConfirm, &g.Status, &g.LastUsedAt, &g.CreatedAt, &g.RevokedAt}, extra...)
	err := row.Scan(dest...)
	return g, err
}

// ListGrants 列出用户的全部授权（含已撤销 —— 用户问「我什么时候撤销过」时要查得到），新的在前。
// 上限 200：一个人不会有那么多客户端，超出的多半是脚本反复建密钥，列表本就不该无限长。
func (r *Repo) ListGrants(ctx context.Context, userID int64) ([]Grant, error) {
	rows, err := r.pool.Query(ctx, `
		select `+grantCols+`,
		       coalesce((select k.prefix from mcp_access_key k where k.grant_id = g.id order by k.id desc limit 1), '')
		from mcp_grant g
		where g.user_id = $1
		order by g.id desc
		limit 200`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Grant{}
	for rows.Next() {
		var prefix string
		g, err := scanGrant(rows, &prefix)
		if err != nil {
			return nil, err
		}
		g.KeyPrefix = prefix
		out = append(out, g)
	}
	return out, rows.Err()
}

// GetGrant 取用户自己的一条授权。
func (r *Repo) GetGrant(ctx context.Context, userID, grantID int64) (Grant, error) {
	g, err := scanGrant(r.pool.QueryRow(ctx, `select `+grantCols+` from mcp_grant g where g.id = $1 and g.user_id = $2`,
		grantID, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Grant{}, ErrNotFound
	}
	return g, err
}

// RevokeGrant 撤销用户自己的一条授权（连同它下面的 OAuth 令牌，见 RevokeGrantIn）。
// 重复撤销回 ErrAlreadyRevoked；返回撤销前的那一行（审计的 before）。
func (r *Repo) RevokeGrant(ctx context.Context, userID, grantID int64) (Grant, error) {
	g, _, err := r.RevokeGrantIn(ctx, GrantScope{UserID: userID}, grantID)
	return g, err
}

// TouchLastUsed 写最近使用时间，**同一授权一分钟内最多写一次**（条件写在 SQL 里，多实例也成立）。
// 返回是否真的写了。
func (r *Repo) TouchLastUsed(ctx context.Context, grantID int64) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		update mcp_grant set last_used_at = now()
		where id = $1 and (last_used_at is null or last_used_at < now() - interval '1 minute')`, grantID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}
