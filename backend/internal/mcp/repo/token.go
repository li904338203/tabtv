package repo

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// 令牌种类（与 ck_mcp_token_kind 逐字一致）。
const (
	TokenAccess  = "access"
	TokenRefresh = "refresh"
)

// Token 是一枚 OAuth 令牌（**只有哈希**，没有明文）及其授权。
type Token struct {
	ID         int64
	GrantID    int64
	FamilyID   int64
	Kind       string
	Audience   string
	ExpiresAt  time.Time
	ReplacedBy *int64
	RevokedAt  *time.Time
	CreatedAt  time.Time
	// ClientID 是授权所属的 OAuth 客户端（mcp_grant.client_id）。
	ClientID int64
	Grant    Grant
}

// NewToken 是签发入参。
type NewToken struct {
	ID        int64
	Kind      string
	Hash      []byte
	Audience  string
	ExpiresAt time.Time
}

// FindToken 按哈希取令牌（连同授权）。不在 SQL 里过滤撤销 / 过期：调用方据此回不同的错误。
func (r *Repo) FindToken(ctx context.Context, hash []byte) (Token, error) {
	var t Token
	var clientID *int64
	g := &t.Grant
	err := r.pool.QueryRow(ctx, `
		select t.id, t.grant_id, t.family_id, t.kind, t.audience, t.expires_at, t.replaced_by, t.revoked_at,
		       t.created_at, g.client_id, `+grantCols+`
		from mcp_token t join mcp_grant g on g.id = t.grant_id
		where t.token_hash = $1`, hash).Scan(
		&t.ID, &t.GrantID, &t.FamilyID, &t.Kind, &t.Audience, &t.ExpiresAt, &t.ReplacedBy, &t.RevokedAt,
		&t.CreatedAt, &clientID,
		&g.ID, &g.UserID, &g.WorkspaceID, &g.ClientName, &g.Source, &g.Scopes, &g.Toolsets,
		&g.RequirePageConfirm, &g.Status, &g.LastUsedAt, &g.CreatedAt, &g.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Token{}, ErrNotFound
	}
	if clientID != nil {
		t.ClientID = *clientID
	}
	return t, err
}

// IssueTokens 在一个事务里写入同一条链（family）上的一组令牌（授权码兑换：access + refresh）。
func (r *Repo) IssueTokens(ctx context.Context, grantID, familyID int64, toks []NewToken) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := insertTokens(ctx, tx, grantID, familyID, toks); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func insertTokens(ctx context.Context, tx pgx.Tx, grantID, familyID int64, toks []NewToken) error {
	for _, t := range toks {
		if _, err := tx.Exec(ctx, `
			insert into mcp_token (id, grant_id, family_id, kind, token_hash, audience, expires_at)
			values ($1,$2,$3,$4,$5,$6,$7)`,
			t.ID, grantID, familyID, t.Kind, t.Hash, t.Audience, t.ExpiresAt); err != nil {
			return err
		}
	}
	return nil
}

// RotateRefresh 轮换一枚刷新令牌：**CAS** 作废旧行（`revoked_at is null`）并让它指向新的刷新令牌，
// 同一事务里写入新的一组。返回 false 表示旧行已经不是有效状态（并发刷新输了 / 已被撤销），什么都没写。
//
// ⚠️ 必须是 CAS：两个并发的刷新请求都读到「有效」再各自轮换的话，一枚刷新令牌就换出了两条链，
// 而重放检测只认得其中一条。
func (r *Repo) RotateRefresh(ctx context.Context, oldID, grantID, familyID, newRefreshID int64, toks []NewToken) (bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `
		update mcp_token set revoked_at = now(), replaced_by = $2
		where id = $1 and kind = 'refresh' and revoked_at is null`, oldID, newRefreshID)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if err := insertTokens(ctx, tx, grantID, familyID, toks); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// RevokeFamily 撤销整条轮换链（含其中的访问令牌）。返回撤销了几枚。
func (r *Repo) RevokeFamily(ctx context.Context, familyID int64) (int64, error) {
	tag, err := r.pool.Exec(ctx, `update mcp_token set revoked_at = now() where family_id = $1 and revoked_at is null`, familyID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// RevokeGrantTokens 撤销某个授权下已签发的全部令牌（授权码重放时用）。
func (r *Repo) RevokeGrantTokens(ctx context.Context, grantID int64) (int64, error) {
	tag, err := r.pool.Exec(ctx, `update mcp_token set revoked_at = now() where grant_id = $1 and revoked_at is null`, grantID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// RevokeToken 撤销单枚令牌（RFC 7009 撤销访问令牌）。已撤销的不再改。
func (r *Repo) RevokeToken(ctx context.Context, id int64) error {
	_, err := r.pool.Exec(ctx, `update mcp_token set revoked_at = now() where id = $1 and revoked_at is null`, id)
	return err
}

// ---------------------------------------------------------------- 授权码

// AuthCode 是一个授权码（主键即哈希，没有明文）。
type AuthCode struct {
	Hash          []byte
	GrantID       int64
	ClientID      int64
	RedirectURI   string
	CodeChallenge string
	Resource      string
	ExpiresAt     time.Time
	UsedAt        *time.Time
}

// CreateAuthCode 写入授权码。
func (r *Repo) CreateAuthCode(ctx context.Context, c AuthCode) error {
	_, err := r.pool.Exec(ctx, `
		insert into mcp_auth_code (code_hash, grant_id, client_id, redirect_uri, code_challenge, resource, expires_at)
		values ($1,$2,$3,$4,$5,$6,$7)`,
		c.Hash, c.GrantID, c.ClientID, c.RedirectURI, c.CodeChallenge, c.Resource, c.ExpiresAt)
	return err
}

// GetAuthCode 按哈希取授权码（含已用的 —— 调用方要据此识别重放）。
func (r *Repo) GetAuthCode(ctx context.Context, hash []byte) (AuthCode, error) {
	var c AuthCode
	err := r.pool.QueryRow(ctx, `
		select code_hash, grant_id, client_id, redirect_uri, code_challenge, resource, expires_at, used_at
		from mcp_auth_code where code_hash = $1`, hash).Scan(
		&c.Hash, &c.GrantID, &c.ClientID, &c.RedirectURI, &c.CodeChallenge, &c.Resource, &c.ExpiresAt, &c.UsedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AuthCode{}, ErrNotFound
	}
	return c, err
}

// ConsumeAuthCode 用 CAS 标记授权码已用。返回 false = 它已经被用过（重放，或并发兑换输了）。
func (r *Repo) ConsumeAuthCode(ctx context.Context, hash []byte) (bool, error) {
	tag, err := r.pool.Exec(ctx, `update mcp_auth_code set used_at = now() where code_hash = $1 and used_at is null`, hash)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ---------------------------------------------------------------- OAuth 授权

// UpsertOAuthGrant 写同意结果：同一（用户, 工作区, 客户端）已有 active 的 OAuth 授权时**更新那一条**
// （uk_mcp_grant_oauth_client），否则新建。返回最终的授权与是否新建。
func (r *Repo) UpsertOAuthGrant(ctx context.Context, g Grant, clientID int64) (Grant, bool, error) {
	var inserted bool
	out, err := scanGrant(r.pool.QueryRow(ctx, `
		insert into mcp_grant as g (id, user_id, workspace_id, client_id, client_name, source, scopes, toolsets, require_page_confirm)
		values ($1,$2,$3,$4,$5,'oauth',$6,$7,$8)
		on conflict (user_id, workspace_id, client_id) where status = 'active' and source = 'oauth'
		do update set client_name = excluded.client_name, scopes = excluded.scopes,
		              toolsets = excluded.toolsets, require_page_confirm = excluded.require_page_confirm
		returning `+grantCols+`, (xmax = 0)`,
		g.ID, g.UserID, g.WorkspaceID, clientID, g.ClientName, g.Scopes, g.Toolsets, g.RequirePageConfirm), &inserted)
	if err != nil {
		return Grant{}, false, err
	}
	return out, inserted, nil
}

// GetGrantByID 按 ID 取授权（不限用户：令牌端点只有 grant ID，没有登录用户）。
func (r *Repo) GetGrantByID(ctx context.Context, grantID int64) (Grant, error) {
	g, err := scanGrant(r.pool.QueryRow(ctx, `select `+grantCols+` from mcp_grant g where g.id = $1`, grantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Grant{}, ErrNotFound
	}
	return g, err
}
