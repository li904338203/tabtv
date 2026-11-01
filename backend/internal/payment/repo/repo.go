// Package repo 是支付渠道配置与回调留痕的数据访问。
package repo

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/payment/domain"
)

type Repo struct {
	pool   *pgxpool.Pool
	nextID func() int64
}

func New(pool *pgxpool.Pool, nextID func() int64) *Repo {
	return &Repo{pool: pool, nextID: nextID}
}

// ErrNotFound 渠道不存在。
var ErrNotFound = errors.New("支付渠道不存在")

// ChannelRow 是一行渠道配置（密钥仍是密文）。
type ChannelRow struct {
	ID         int64
	Code       string
	Name       string
	MerchantID string
	AppID      string
	// 下面三项是**密文**。解密由调用方用 platform/secret 做。
	PrivateKeyCipher  string
	PlatformKeyCipher string
	APIv3KeyCipher    string
	KeyVersion        int
	BaseURL           string
	NotifyURL         string
	IsSandbox         bool
	Status            string
}

const channelCols = `select id, code, name, merchant_id, app_id,
	coalesce(private_key_cipher,''), coalesce(platform_key_cipher,''),
	coalesce(api_v3_key_cipher,''), key_version, base_url, notify_url,
	is_sandbox, status from payment_channel `

func scanChannel(row pgx.Row) (ChannelRow, error) {
	var c ChannelRow
	err := row.Scan(&c.ID, &c.Code, &c.Name, &c.MerchantID, &c.AppID,
		&c.PrivateKeyCipher, &c.PlatformKeyCipher, &c.APIv3KeyCipher,
		&c.KeyVersion, &c.BaseURL, &c.NotifyURL, &c.IsSandbox, &c.Status)
	return c, err
}

// ListChannels 列全部渠道（管理端）。
func (r *Repo) ListChannels(ctx context.Context) ([]ChannelRow, error) {
	rows, err := r.pool.Query(ctx, channelCols+`order by code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChannelRow
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// EnabledChannels 只取启用的（装配适配器时用）。
func (r *Repo) EnabledChannels(ctx context.Context) ([]ChannelRow, error) {
	rows, err := r.pool.Query(ctx, channelCols+`where status='enabled' order by code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChannelRow
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *Repo) Channel(ctx context.Context, code string) (ChannelRow, error) {
	c, err := scanChannel(r.pool.QueryRow(ctx, channelCols+`where code = $1`, code))
	if errors.Is(err, pgx.ErrNoRows) {
		return ChannelRow{}, ErrNotFound
	}
	return c, err
}

// SaveChannelInput 保存渠道配置。
//
// ⚠️ 三个密钥都是**三态**：nil = 保留原值、空串 = 清除、有值 = 替换。
// 只用"有值就写"的话，管理端回显的掩码（`sk-****`）会被当成新密钥存回去
// —— 而那一刻起这个渠道就再也用不了了，症状是"什么都没改却开始验签失败"。
type SaveChannelInput struct {
	Code        string
	Name        string
	MerchantID  string
	AppID       string
	BaseURL     string
	NotifyURL   string
	IsSandbox   bool
	Status      string
	PrivateKey  *string
	PlatformKey *string
	APIv3Key    *string
	KeyVersion  int
}

// SaveChannel 新增或更新。`code` 是业务键。
func (r *Repo) SaveChannel(ctx context.Context, in SaveChannelInput) error {
	_, err := r.pool.Exec(ctx, `
		insert into payment_channel (id, code, name, merchant_id, app_id,
			base_url, notify_url, is_sandbox, status,
			private_key_cipher, platform_key_cipher, api_v3_key_cipher, key_version)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		on conflict (code) do update set
			name = excluded.name, merchant_id = excluded.merchant_id,
			app_id = excluded.app_id, base_url = excluded.base_url,
			notify_url = excluded.notify_url, is_sandbox = excluded.is_sandbox,
			status = excluded.status,
			-- ⚠️ 密钥三态：传 null 表示**保留原值**（coalesce 到旧值），
			-- 传空串表示清除，传密文表示替换。
			private_key_cipher = coalesce($10, payment_channel.private_key_cipher),
			platform_key_cipher = coalesce($11, payment_channel.platform_key_cipher),
			api_v3_key_cipher = coalesce($12, payment_channel.api_v3_key_cipher),
			key_version = excluded.key_version, updated_at = now()`,
		r.nextID(), in.Code, in.Name, in.MerchantID, in.AppID,
		in.BaseURL, in.NotifyURL, in.IsSandbox, in.Status,
		in.PrivateKey, in.PlatformKey, in.APIv3Key, in.KeyVersion)
	return err
}

// ---------------------------------------------------------------- 回调留痕

// Record 实现 `payment/app.Recorder`。
//
// ⚠️ 幂等靠 `uk_callback_event(channel, channel_event_id)` 的**唯一冲突**，
// 不是"先查再写"：并发的两个回调各自查一遍都会说"没处理过"。
func (r *Repo) Record(ctx context.Context, in domain.CallbackRecord) (bool, error) {
	_, err := r.pool.Exec(ctx, `
		insert into payment_callback (id, channel, channel_event_id, raw_payload,
			is_signature_valid, stage, channel_code, out_trade_no, amount_cents,
			error_message, processed_at, result)
		values ($1,$2,$3,$4,$5,$6,$2,$7,$8,$9,now(),$6)`,
		r.nextID(), in.Channel, in.EventID, in.RawPayload, in.SignatureOK,
		in.Stage, nullStr(in.OutTradeNo), nullInt64(in.AmountCents),
		nullStr(truncate(in.ErrorMessage, 500)))
	if isDuplicate(err) {
		return true, nil
	}
	return false, err
}

// UpdateStage 更新处理阶段。
func (r *Repo) UpdateStage(ctx context.Context, channel, eventID, stage,
	errMsg string, orderID int64) error {
	_, err := r.pool.Exec(ctx, `
		update payment_callback set stage = $3, result = $3,
			error_message = nullif($4,''), order_id = nullif($5,0::bigint),
			processed_at = now()
		where channel = $1 and channel_event_id = $2`,
		channel, eventID, stage, truncate(errMsg, 500), orderID)
	return err
}

// CallbackRow 是管理端/联调要看的一条回调记录。
type CallbackRow struct {
	ID           int64
	Channel      string
	EventID      string
	Stage        string
	SignatureOK  bool
	OutTradeNo   string
	AmountCents  int64
	OrderID      int64
	ErrorMessage string
}

// ListCallbacks 按订单号查回调（排障入口）。
func (r *Repo) ListCallbacks(ctx context.Context, outTradeNo string, limit int) (
	[]CallbackRow, error) {
	rows, err := r.pool.Query(ctx, `
		select id, channel, channel_event_id, stage, is_signature_valid,
			coalesce(out_trade_no,''), coalesce(amount_cents,0), coalesce(order_id,0),
			coalesce(error_message,'')
		from payment_callback
		where ($1 = '' or out_trade_no = $1)
		order by id desc limit $2`, outTradeNo, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CallbackRow
	for rows.Next() {
		var c CallbackRow
		if err := rows.Scan(&c.ID, &c.Channel, &c.EventID, &c.Stage,
			&c.SignatureOK, &c.OutTradeNo, &c.AmountCents, &c.OrderID,
			&c.ErrorMessage); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func truncate(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func isDuplicate(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return err != nil && strings.Contains(err.Error(), "23505")
}
