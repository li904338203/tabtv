package repo

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/li904338203/tabtv/backend/internal/billing/domain"
)

// 订单退款与权益回收（P11-03）。
//
// ⚠️ **回收在前、退钱在后**。顺序反过来的话：退钱成功而回收失败，
// 用户白拿权益，且没有任何办法自动补救（钱已经出去了）。
// 反过来（回收成功而退钱失败）是**可恢复**的：权益已经收回、
// `refund` 行留在 pending，重试或人工介入都能接上。
// 两种都不完美，但只有一种能收场。

// RefundRow 是一笔退款。
type RefundRow struct {
	ID          int64
	OrderID     int64
	PayerID     int64
	AmountCents int64
	Reason      string
	Status      string
	// RevokedCredits 这次收回了多少积分。
	RevokedCredits int64
	// RevokedGrantID / RevokedMembershipID / RestoredMembershipID 回收与恢复的对象。
	RevokedGrantID       int64
	RevokedMembershipID  int64
	RestoredMembershipID int64
	ChannelRefundID      string
	ChannelResult        string
	RequestedByUserID    int64
	CreatedAt            time.Time
}

// RefundFactsOf 查出判定可退性要用到的全部事实。
//
// ⚠️ 一次查齐而不是分几次查：分开查的话两次查询之间用户可能刚好花掉积分，
// 于是判定用的是"刚才还没花"的事实 —— 而那一刻他其实已经花了。
// 真正的防线是回收时的 CAS（见 RevokeForRefund），这里只是尽早告诉用户。
func (r *Repo) RefundFactsOf(ctx context.Context, payerID, orderID int64) (
	domain.RefundFacts, Order, error) {
	o, err := r.Order(ctx, payerID, orderID)
	if err != nil {
		return domain.RefundFacts{}, Order{}, err
	}
	f := domain.RefundFacts{
		PayStatus: o.PayStatus, PaidAt: o.PaidAt,
		ExpectCredits: o.Credits,
		IsMembership:  o.Type == OrderMembership,
	}
	// 这一单发出的批次（`source_ref_id` 就是订单 ID，见 Fulfil）。
	err = r.pool.QueryRow(ctx, `
		select amount, remaining from credit_grant
		where payer_account_id = $1 and source_ref_id = $2
		  and source in ('membership','purchase') limit 1`,
		payerID, itoa(orderID)).Scan(&f.GrantAmount, &f.GrantRemaining)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		f.HasGrant = false
	case err != nil:
		return domain.RefundFacts{}, Order{}, err
	default:
		f.HasGrant = true
	}
	// 已有的退款申请。
	var pending int
	if err := r.pool.QueryRow(ctx, `
		select count(*) from refund
		where order_id = $1 and status in ('pending','success')`, orderID).
		Scan(&pending); err != nil {
		return domain.RefundFacts{}, Order{}, err
	}
	f.HasPendingRefund = pending > 0
	// 这一单买到的会员。
	var expire time.Time
	err = r.pool.QueryRow(ctx, `
		select expire_at from membership where source_order_id = $1
		order by id desc limit 1`, orderID).Scan(&expire)
	if err == nil {
		f.MembershipExpireAt = &expire
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return domain.RefundFacts{}, Order{}, err
	}
	return f, o, nil
}

// ErrRefundRaced 回收时发现权益已经变了（并发用掉了积分）。
var ErrRefundRaced = errors.New("权益在申请期间发生变化，退款已中止")

// RevokeForRefund 建退款单并**在同一个事务里**回收权益。
//
// ⚠️ 三件事必须一个事务：建 refund 行、收回积分、回收会员（并恢复被顶替的那条）。
// 分开做的话中间崩一次就会出现"积分收了但会员还在"或反过来，
// 而两种都只能人工对账。
//
// ⚠️ 回收积分用**带条件的更新**（`remaining = amount`）：申请与回收之间
// 用户可能刚好花掉一笔。0 行更新时整笔中止 —— 这是最后一道防线，
// 前面那次 `CheckRefundable` 只是尽早告诉用户。
func (r *Repo) RevokeForRefund(ctx context.Context, in RevokeInput) (RefundRow, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return RefundRow{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	refundID := r.nextID()
	out := RefundRow{
		ID: refundID, OrderID: in.OrderID, PayerID: in.PayerID,
		AmountCents: in.AmountCents, Reason: in.Reason,
		Status: domain.RefundPending, RequestedByUserID: in.UserID,
	}

	// ① 收积分。⚠️ 条件里带 `remaining = amount`：用掉一分就不许退。
	if in.RevokeCredits > 0 {
		var grantID int64
		var revoked int
		err := tx.QueryRow(ctx, `
			update credit_grant set status = 'revoked', remaining = 0,
				revoked_by_refund_id = $3, updated_at = now()
			where payer_account_id = $1 and source_ref_id = $2
			  and source in ('membership','purchase')
			  and status = 'active' and remaining = amount
			returning id, amount`,
			in.PayerID, itoa(in.OrderID), refundID).Scan(&grantID, &revoked)
		if errors.Is(err, pgx.ErrNoRows) {
			// 批次不在 active、或者已经被花掉了一部分 —— 整笔中止。
			return RefundRow{}, ErrRefundRaced
		}
		if err != nil {
			return RefundRow{}, err
		}
		out.RevokedGrantID = grantID
		out.RevokedCredits = int64(revoked)
		// 记一条流水。⚠️ 用 `adjust` 而不是 `refund`：
		// `refund` 在这张表里的语义是"任务失败把积分还给你"，方向相反。
		// 复用的话看板上的"退款总额"会把回收也算成给用户的钱。
		if err := r.insertTx(ctx, tx, txInput{
			PayerID: in.PayerID, Type: TxAdjust, Change: -revoked,
			RefType: "order_refund", RefID: itoa(in.OrderID),
			Reason: "订单退款回收积分", OperatorAdmin: in.AdminID,
		}); err != nil {
			return RefundRow{}, err
		}
	}

	// ② 回收会员，并**恢复被它顶替的那条**。
	if in.RevokeMembership {
		var mid int64
		err := tx.QueryRow(ctx, `
			update membership set status = 'refunded', updated_at = now()
			where source_order_id = $1 and status = 'active'
			returning id`, in.OrderID).Scan(&mid)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return RefundRow{}, err
		}
		if err == nil {
			out.RevokedMembershipID = mid
			// ⚠️ **恢复被顶替的那条**：升级/续费时旧行被标成 `superseded`，
			// 不恢复的话用户退的是升级差价、结果连原来的基础会员也没了。
			// 取"这个付费主体下最近一条 superseded 且**还没到期**的"——
			// 已经到期的恢复了也没用，反而会让"当前会员"查询多命中一行。
			var restored int64
			err := tx.QueryRow(ctx, `
				update membership set status = 'active', updated_at = now()
				where id = (
					select id from membership
					where payer_account_id = $1 and status = 'superseded'
					  and expire_at > now()
					order by id desc limit 1)
				returning id`, in.PayerID).Scan(&restored)
			if err == nil {
				out.RestoredMembershipID = restored
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return RefundRow{}, err
			}
		}
	}

	// ③ 建退款单。⚠️ 唯一索引 `uk_refund_order_active` 挡住并发的第二次申请。
	_, err = tx.Exec(ctx, `
		insert into refund (id, order_id, payer_account_id, amount_cents, reason,
			status, revoked_credits, revoked_grant_id, revoked_membership_id,
			restored_membership_id, requested_by_user_id)
		values ($1,$2,$3,$4,$5,'pending',$6,
			nullif($7,0::bigint), nullif($8,0::bigint), nullif($9,0::bigint),
			nullif($10,0::bigint))`,
		refundID, in.OrderID, in.PayerID, in.AmountCents, in.Reason,
		out.RevokedCredits, out.RevokedGrantID, out.RevokedMembershipID,
		out.RestoredMembershipID, in.UserID)
	if isDuplicate(err) {
		return RefundRow{}, ErrRefundRaced
	}
	if err != nil {
		return RefundRow{}, err
	}
	return out, tx.Commit(ctx)
}

// RevokeInput 是一次回收。
type RevokeInput struct {
	OrderID          int64
	PayerID          int64
	UserID           int64
	AdminID          int64
	AmountCents      int64
	Reason           string
	RevokeCredits    int
	RevokeMembership bool
}

// FinishRefund 记渠道退款的结果。
//
// ⚠️ 返回"本次是否真的改了状态"：重复回调或重复点击不能把一笔
// 已经成功的退款再改一次（也不能反过来把成功改成失败）。
func (r *Repo) FinishRefund(ctx context.Context, refundID int64, ok bool,
	channelRefundID, result string) (bool, error) {
	status := domain.RefundFailed
	if ok {
		status = domain.RefundSuccess
	}
	tag, err := r.pool.Exec(ctx, `
		update refund set status = $2, channel_refund_id = nullif($3,''),
			channel_result = left($4, 500), updated_at = now()
		where id = $1 and status = 'pending'`,
		refundID, status, channelRefundID, result)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if ok {
		// 订单转 refunded。⚠️ 只在**退款真的成功**时改：
		// 退款失败时订单还是 paid（钱没退回去），否则对账会认为这笔已经退了。
		if _, err := r.pool.Exec(ctx, `
			update purchase_order set pay_status = 'refunded', updated_at = now()
			where id = (select order_id from refund where id = $1)
			  and pay_status = 'paid'`, refundID); err != nil {
			return true, err
		}
	}
	return true, nil
}

// OrderRefund 按 ID 取一笔订单退款。
//
// ⚠️ 名字里的 `Order` 不能省：`ledger.go` 里的 `Refund` 是
// "任务失败把积分还回去"，方向完全相反。
func (r *Repo) OrderRefund(ctx context.Context, id int64) (RefundRow, error) {
	var f RefundRow
	err := r.pool.QueryRow(ctx, `
		select id, order_id, coalesce(payer_account_id,0), amount_cents, reason,
			status, revoked_credits, coalesce(revoked_grant_id,0),
			coalesce(revoked_membership_id,0), coalesce(restored_membership_id,0),
			coalesce(channel_refund_id,''), coalesce(channel_result,''),
			coalesce(requested_by_user_id,0), created_at
		from refund where id = $1`, id).
		Scan(&f.ID, &f.OrderID, &f.PayerID, &f.AmountCents, &f.Reason, &f.Status,
			&f.RevokedCredits, &f.RevokedGrantID, &f.RevokedMembershipID,
			&f.RestoredMembershipID, &f.ChannelRefundID, &f.ChannelResult,
			&f.RequestedByUserID, &f.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return RefundRow{}, ErrNotFound
	}
	return f, err
}

// RefundsOfOrder 一张订单的退款历史（含失败的）。
//
// ⚠️ 失败的也要留着：用户问"我申请过退款"时，只留成功的话查不到任何记录。
func (r *Repo) RefundsOfOrder(ctx context.Context, orderID int64) ([]RefundRow, error) {
	rows, err := r.pool.Query(ctx, `
		select id, order_id, coalesce(payer_account_id,0), amount_cents, reason,
			status, revoked_credits, coalesce(revoked_grant_id,0),
			coalesce(revoked_membership_id,0), coalesce(restored_membership_id,0),
			coalesce(channel_refund_id,''), coalesce(channel_result,''),
			coalesce(requested_by_user_id,0), created_at
		from refund where order_id = $1 order by id desc`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RefundRow
	for rows.Next() {
		var f RefundRow
		if err := rows.Scan(&f.ID, &f.OrderID, &f.PayerID, &f.AmountCents,
			&f.Reason, &f.Status, &f.RevokedCredits, &f.RevokedGrantID,
			&f.RevokedMembershipID, &f.RestoredMembershipID, &f.ChannelRefundID,
			&f.ChannelResult, &f.RequestedByUserID, &f.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- 对账

// MarkReconciled 落一次对账结论。
func (r *Repo) MarkReconciled(ctx context.Context, orderID int64,
	status, detail string) error {
	tag, err := r.pool.Exec(ctx, `
		update purchase_order set reconcile_status = $2,
			reconcile_detail = nullif(left($3, 500), ''), reconciled_at = now(),
			updated_at = now() where id = $1`, orderID, status, detail)
	if err != nil {
		return err
	}
	// ⚠️ 0 行要说出来：否则人工标记一张不存在的订单也「成功」，还会留下一条审计。
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ReconcileCandidates 取一批待对账的订单。
//
// ⚠️ 只取**有渠道的**（拉起过支付的）：没拉起过的单渠道那边根本没有记录，
// 去查一遍只会把"交易不存在"当成差异刷满对账页。
//
// ⚠️ 也取 `pending` 的：那正是"渠道收了钱而回调丢了"会藏身的地方 ——
// 只对已支付的单对账的话，最该被发现的那一类永远发现不了。
func (r *Repo) ReconcileCandidates(ctx context.Context, limit int) ([]Order, error) {
	rows, err := r.pool.Query(ctx,
		orderCols+`where o.channel is not null and o.channel <> 'dev'
		and o.pay_status in ('paid','pending','expired')
		order by o.reconciled_at asc nulls first, o.id desc limit $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ReconcileDiffs 列出有差异的订单（管理端对账页）。
func (r *Repo) ReconcileDiffs(ctx context.Context, limit int) ([]Order, error) {
	rows, err := r.pool.Query(ctx,
		orderCols+`where o.reconcile_status = 'diff'
		order by o.reconciled_at desc nulls last limit $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// PayerOfOrder 按订单取付费主体（后台用，不带工作区条件）。
func (r *Repo) PayerOfOrder(ctx context.Context, orderID int64) (int64, error) {
	var payer int64
	err := r.pool.QueryRow(ctx,
		`select payer_account_id from purchase_order where id = $1`, orderID).
		Scan(&payer)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	return payer, err
}
