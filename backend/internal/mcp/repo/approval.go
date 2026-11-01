package repo

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// 页面审批（MCPT-13，MCP-F-44）：凭据开了「花钱需在 seeAitv 页面确认」时，报价由发起授权的用户本人
// 在 /mcp/approve/{quoteId} 上批准或拒绝。
//
// ⚠️ 这里的读取**不按 grant 过滤**（审批页手上只有 quoteId 与登录用户）：是不是本人由 app 层拿
// grant.user_id 比对后再决定给不给内容 —— 仓储只负责把事实取全。写入（批准 / 拒绝）则把
// `g.user_id = 当前用户 and g.status = 'active'` 写进 CAS 条件本身：判定与写入之间授权被撤销、
// 或者有人绕过 app 层直接调用，都不会写进去。

// QuoteApproval 是审批页要的事实：报价 + 它所属的授权。
type QuoteApproval struct {
	Quote Quote
	Grant Grant
}

// GetQuoteForApproval 按 quoteId 取报价与所属授权。不存在 → ErrNotFound。
func (r *Repo) GetQuoteForApproval(ctx context.Context, quoteID int64) (QuoteApproval, error) {
	var a QuoteApproval
	q := &a.Quote
	var detail []byte
	err := r.pool.QueryRow(ctx, `
		select q.id, q.grant_id, q.tool, q.fingerprint, q.credits, q.payer_account_id, q.detail, q.expires_at,
		       q.approved_at, q.rejected_at, q.used_at, q.created_at, coalesce(q.detail->>'voided', ''),
		       `+grantCols+`
		from mcp_quote q join mcp_grant g on g.id = q.grant_id
		where q.id = $1`, quoteID).
		Scan(append([]any{&q.ID, &q.GrantID, &q.Tool, &q.Fingerprint, &q.Credits, &q.PayerAccountID, &detail,
			&q.ExpiresAt, &q.ApprovedAt, &q.RejectedAt, &q.UsedAt, &q.CreatedAt, &q.VoidedReason},
			grantDest(&a.Grant)...)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return QuoteApproval{}, ErrNotFound
	}
	if err != nil {
		return QuoteApproval{}, err
	}
	q.Detail = detail
	return a, nil
}

// ApproveQuote 批准：**CAS** —— 未批准、未拒绝、未用、未过期（进程时钟 now，与签发 / 判定同一个时钟）、
// 授权仍 active 且属于 userID。返回本次是否真的写了；0 行由调用方重读真实状态返回。
func (r *Repo) ApproveQuote(ctx context.Context, userID, quoteID int64, now time.Time) (bool, error) {
	return r.decideQuote(ctx, "approved_at", userID, quoteID, now)
}

// RejectQuote 拒绝：条件同 ApproveQuote，写 rejected_at。拒绝之后这张报价不能再被批准或用掉。
func (r *Repo) RejectQuote(ctx context.Context, userID, quoteID int64, now time.Time) (bool, error) {
	return r.decideQuote(ctx, "rejected_at", userID, quoteID, now)
}

func (r *Repo) decideQuote(ctx context.Context, col string, userID, quoteID int64, now time.Time) (bool, error) {
	// col 只来自上面两个常量，不拼用户输入。
	tag, err := r.pool.Exec(ctx, `
		update mcp_quote q set `+col+` = now()
		from mcp_grant g
		where q.id = $1 and g.id = q.grant_id and g.user_id = $2 and g.status = 'active'
		  and q.approved_at is null and q.rejected_at is null and q.used_at is null
		  and q.expires_at > $3`, quoteID, userID, now)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// SetGrantPageConfirm 切换授权的「花钱需在 seeAitv 页面确认」（PATCH /me/mcp/grants/{grantId}）。
// 只改本人的 active 授权；返回改之前与改之后的两份（审计用）。不存在或不属于本人 → ErrNotFound；
// 已撤销 → ErrAlreadyRevoked。
func (r *Repo) SetGrantPageConfirm(ctx context.Context, userID, grantID int64, on bool) (Grant, Grant, error) {
	before, err := r.GetGrant(ctx, userID, grantID)
	if err != nil {
		return Grant{}, Grant{}, err
	}
	if before.Status != GrantActive {
		return before, Grant{}, ErrAlreadyRevoked
	}
	var prefix string
	after, err := scanGrant(r.pool.QueryRow(ctx, `
		update mcp_grant g set require_page_confirm = $3
		where g.id = $1 and g.user_id = $2 and g.status = 'active'
		returning `+grantCols+`,
		  coalesce((select k.prefix from mcp_access_key k where k.grant_id = g.id order by k.id desc limit 1), '')`,
		grantID, userID, on), &prefix)
	if errors.Is(err, pgx.ErrNoRows) { // 读与写之间被撤销了
		return before, Grant{}, ErrAlreadyRevoked
	}
	if err != nil {
		return Grant{}, Grant{}, err
	}
	after.KeyPrefix = prefix
	return before, after, nil
}

func grantDest(g *Grant) []any {
	return []any{&g.ID, &g.UserID, &g.WorkspaceID, &g.ClientName, &g.Source, &g.Scopes, &g.Toolsets,
		&g.RequirePageConfirm, &g.Status, &g.LastUsedAt, &g.CreatedAt, &g.RevokedAt}
}
