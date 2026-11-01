package repo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// 客户端登记方式（与 ck_mcp_client_reg 逐字一致）。
const (
	RegistrationCIMD = "cimd"
	RegistrationDCR  = "dcr"
)

// Client 是一个 OAuth 客户端（MCPT-12）。
type Client struct {
	ID           int64
	Key          string // CIMD URL 或 DCR 生成的 client_id
	Name         string
	RedirectURIs []string
	Registration string
	Metadata     json.RawMessage
	FetchedAt    *time.Time
	CreatedAt    time.Time
}

const clientCols = `id, client_key, name, redirect_uris, registration, metadata, fetched_at, created_at`

func scanClient(row pgx.Row) (Client, error) {
	var c Client
	err := row.Scan(&c.ID, &c.Key, &c.Name, &c.RedirectURIs, &c.Registration, &c.Metadata, &c.FetchedAt, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Client{}, ErrNotFound
	}
	return c, err
}

// GetClient 按内部 ID 取客户端。
func (r *Repo) GetClient(ctx context.Context, id int64) (Client, error) {
	return scanClient(r.pool.QueryRow(ctx, `select `+clientCols+` from mcp_client where id = $1`, id))
}

// GetClientByKey 按 client_id（CIMD URL / DCR ID）取客户端。
func (r *Repo) GetClientByKey(ctx context.Context, key string) (Client, error) {
	return scanClient(r.pool.QueryRow(ctx, `select `+clientCols+` from mcp_client where client_key = $1`, key))
}

// UpsertCIMDClient 写入（或刷新）一个 CIMD 客户端的缓存，fetched_at = now()。
// ⚠️ 只覆盖 registration='cimd' 的行：DCR 的 ID 不会是 https URL，但万一撞上也不能被一份远端文档改掉。
func (r *Repo) UpsertCIMDClient(ctx context.Context, c Client) (Client, error) {
	meta := c.Metadata
	if len(meta) == 0 {
		meta = json.RawMessage(`{}`)
	}
	return scanClient(r.pool.QueryRow(ctx, `
		insert into mcp_client (id, client_key, name, redirect_uris, registration, metadata, fetched_at)
		values ($1,$2,$3,$4,'cimd',$5, now())
		on conflict (client_key) do update
		  set name = excluded.name, redirect_uris = excluded.redirect_uris,
		      metadata = excluded.metadata, fetched_at = excluded.fetched_at
		  where mcp_client.registration = 'cimd'
		returning `+clientCols, c.ID, c.Key, c.Name, c.RedirectURIs, meta))
}

// CreateDCRClient 登记一个动态注册的客户端（registration='dcr'）。
func (r *Repo) CreateDCRClient(ctx context.Context, c Client) (Client, error) {
	meta := c.Metadata
	if len(meta) == 0 {
		meta = json.RawMessage(`{}`)
	}
	return scanClient(r.pool.QueryRow(ctx, `
		insert into mcp_client (id, client_key, name, redirect_uris, registration, metadata)
		values ($1,$2,$3,$4,'dcr',$5)
		returning `+clientCols, c.ID, c.Key, c.Name, c.RedirectURIs, meta))
}
