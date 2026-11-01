package repo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Quote 是 mcp_quote 的一行（MCPT-05）：签发给某个授权的一次性报价凭证。
// MCPT-10 起被确认服务消费：用掉（MarkQuoteUsed）或因价格变化作废（VoidQuote），两者都 CAS 写 used_at。
type Quote struct {
	ID             int64
	GrantID        int64
	Tool           string // generate | plan（mcp/domain.QuoteTool*）
	Fingerprint    []byte // 32 字节，mcp/domain.Fingerprint
	Credits        int
	PayerAccountID int64
	Detail         json.RawMessage
	ExpiresAt      time.Time
	ApprovedAt     *time.Time
	// RejectedAt 是用户在审批页上拒绝的时间（MCPT-13）。拒绝的报价**直接无效**，不能被用掉也不能被批准。
	RejectedAt *time.Time
	UsedAt     *time.Time
	CreatedAt  time.Time
	// VoidedReason 是 detail.voided（作废原因，如 price_changed）；为空表示没被作废。
	// ⚠️ 「用过」与「作废」都写 used_at，只能靠它分辨 —— 作废的 quote 当成「已用」去重放的话，
	// 同一幂等键下查不到任务，受理会按新价正常受理一次（绕过了确认）。
	VoidedReason string
	// Result 是受理结果的引用（detail.result，MCPT-16）：没有幂等键可按的花钱方法（分镜、整图运行……）
	// 靠它在同一 quoteId 重试时回放原结果。为空表示还没记（受理还在进行、或受理之前崩溃过）。
	Result json.RawMessage
}

// InsertQuote 写一张报价。created_at 取库时间（默认值），expires_at 由调用方按进程时钟算好传入 ——
// 与访问密钥过期同一个时钟（MCPT-03），判定过期时也用进程时钟。
func (r *Repo) InsertQuote(ctx context.Context, q Quote) (Quote, error) {
	err := r.pool.QueryRow(ctx, `
		insert into mcp_quote (id, grant_id, tool, fingerprint, credits, payer_account_id, detail, expires_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8)
		returning created_at`,
		q.ID, q.GrantID, q.Tool, q.Fingerprint, q.Credits, q.PayerAccountID, []byte(q.Detail), q.ExpiresAt).
		Scan(&q.CreatedAt)
	return q, err
}

// GetQuote 按授权取一张报价。⚠️ 条件里带 grant_id：别的授权签发的报价一律当不存在
// （不区分「不存在」与「不是你的」—— 区分等于给出 quoteId 的枚举接口）。
func (r *Repo) GetQuote(ctx context.Context, grantID, quoteID int64) (Quote, error) {
	var q Quote
	var detail, result []byte
	err := r.pool.QueryRow(ctx, `
		select id, grant_id, tool, fingerprint, credits, payer_account_id, detail, expires_at,
		       approved_at, rejected_at, used_at, created_at, coalesce(detail->>'voided', ''), detail->'result'
		from mcp_quote where id = $1 and grant_id = $2`, quoteID, grantID).
		Scan(&q.ID, &q.GrantID, &q.Tool, &q.Fingerprint, &q.Credits, &q.PayerAccountID, &detail,
			&q.ExpiresAt, &q.ApprovedAt, &q.RejectedAt, &q.UsedAt, &q.CreatedAt, &q.VoidedReason, &result)
	if errors.Is(err, pgx.ErrNoRows) {
		return Quote{}, ErrNotFound
	}
	if err != nil {
		return Quote{}, err
	}
	q.Detail = detail
	if len(result) > 0 && string(result) != "null" {
		q.Result = result
	}
	return q, nil
}

// SetQuoteResult 把受理结果的引用记在**已用**的报价上（detail.result，MCPT-16）。只写一次：
// 条件 `used_at is not null and not detail ? 'voided' and not detail ? 'result'` —— 已作废的不记，
// 已记过的不覆盖（同一张 quote 的并发重放拿到的都是第一次的结果）。返回本次是否真的写了。
func (r *Repo) SetQuoteResult(ctx context.Context, grantID, quoteID int64, result json.RawMessage) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		update mcp_quote set detail = detail || jsonb_build_object('result', $3::jsonb)
		where id = $1 and grant_id = $2 and used_at is not null
		  and not (detail ? 'voided') and not (detail ? 'result')`, quoteID, grantID, []byte(result))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// MarkQuoteUsed 把报价标成已用：**CAS** `used_at is null and rejected_at is null`。返回本次是否真的改了这一行 ——
// 并发的同一确认只有一个拿到 true（进入受理），其余拿到 false（转重放，同一幂等键只会拿回原任务）。
func (r *Repo) MarkQuoteUsed(ctx context.Context, grantID, quoteID int64) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		update mcp_quote set used_at = now()
		where id = $1 and grant_id = $2 and used_at is null and rejected_at is null`, quoteID, grantID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// VoidQuote 作废一张报价（价格在确认之后变了，MCP-F-41）：同样 CAS 写 used_at，并在 detail 里记
// `voided = reason` —— 审计要能分辨「用过」与「作废」。返回本次是否真的改了这一行。
func (r *Repo) VoidQuote(ctx context.Context, grantID, quoteID int64, reason string) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		update mcp_quote set used_at = now(), detail = detail || jsonb_build_object('voided', $3::text)
		where id = $1 and grant_id = $2 and used_at is null and rejected_at is null`, quoteID, grantID, reason)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// CleanupQuotesBefore 删掉 expires_at 早于 before 的报价，**单次至多 limit 行**（分批，同调用审计）。
// 表上没有 expires_at 索引：报价量级远小于调用审计（每次花钱调用一两行），每小时一次的顺扫可以接受；
// 量级起来之后再加索引（迁移）。
func (r *Repo) CleanupQuotesBefore(ctx context.Context, before time.Time, limit int) (int64, error) {
	tag, err := r.pool.Exec(ctx, `
		delete from mcp_quote
		where id in (select id from mcp_quote
		             where expires_at < $1 order by expires_at limit $2)`, before, limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
