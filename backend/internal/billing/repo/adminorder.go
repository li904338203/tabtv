package repo

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/li904338203/tabtv/backend/internal/billing/domain"
)

// 后台订单检索与人工调账（AD-06、GAP-07、P11-04）。

// OrderFilter 后台订单过滤条件。
//
// ⚠️ 每个条件都**可选**（零值＝不筛）：做成必填的话客服每次都要把
// 六个下拉都选一遍，而他手上通常只有一个订单号。
type OrderFilter struct {
	// Keyword 订单号 / 渠道交易号 / 付费主体 ID。
	//
	// ⚠️ 三者共用一个输入框：客服手上拿到的可能是任意一个 ——
	// 分成三个框的话他得先判断"这串数字是什么"，而那正是他来查的原因。
	Keyword string
	Type    string
	Status  string
	Channel string
	// Reconcile 对账状态（none/pending/matched/diff）。
	Reconcile string
	From, To  *time.Time
	Limit     int
	// CursorID 游标分页。⚠️ 用 id 不用 offset：订单一直在新增，
	// offset 翻页会漏掉或重复（新单插到第一页，后面整体后移一位）。
	CursorID int64
}

// OrderSummary 一组订单的汇总。
//
// ⚠️ 汇总要与列表**同一套过滤条件**：分开算的话运营看到的
// "共 132 笔 / ¥8,900" 与下面列出来的不是同一批，而两个数字都"没错"。
type OrderSummary struct {
	Count int
	// PaidCount / PaidCents 只算**已支付**的：把待支付的也算进收入
	// 会让报表凭空多出一截，而那些钱根本没收到。
	PaidCount   int
	PaidCents   int64
	RefundCount int
	RefundCents int64
	DiffCount   int
}

// buildOrderWhere 拼过滤条件。
//
// ⚠️ 参数化拼接，**绝不拼字符串字面量**：关键词来自后台输入框，
// 直接拼进 SQL 就是一个注入口 —— 而后台账号恰恰权限最大。
func buildOrderWhere(f OrderFilter) (string, []any) {
	var sb strings.Builder
	args := []any{}
	add := func(cond string, vals ...any) {
		sb.WriteString(" and " + cond)
		args = append(args, vals...)
	}
	sb.WriteString("where 1=1")
	if f.Keyword != "" {
		args = append(args, f.Keyword)
		n := len(args)
		// ⚠️ 订单号与付费主体是**精确匹配**（它们是雪花号，前缀匹配没有意义
		// 且会全表扫），渠道交易号才用模糊 —— 客服常常只抄到后半截。
		sb.WriteString(" and (o.id::text = $" + itoa(int64(n)) +
			" or o.payer_account_id::text = $" + itoa(int64(n)) +
			" or o.channel_txn_id like '%'||$" + itoa(int64(n)) + "||'%')")
	}
	if f.Type != "" {
		add("o.type = $"+itoa(int64(len(args)+1)), f.Type)
	}
	if f.Status != "" {
		add("o.pay_status = $"+itoa(int64(len(args)+1)), f.Status)
	}
	if f.Channel != "" {
		add("o.channel = $"+itoa(int64(len(args)+1)), f.Channel)
	}
	if f.Reconcile != "" {
		add("o.reconcile_status = $"+itoa(int64(len(args)+1)), f.Reconcile)
	}
	if f.From != nil {
		add("o.created_at >= $"+itoa(int64(len(args)+1)), *f.From)
	}
	if f.To != nil {
		add("o.created_at < $"+itoa(int64(len(args)+1)), *f.To)
	}
	return sb.String(), args
}

// OrderPageSize 归一后台订单检索的每页条数（缺省 50，最多 200）。
//
// 导出是因为处理器要用**同一个数**判断还有没有下一页：各算各的话，
// 调用方传 500、仓储按 50 取，处理器拿 50 条去比 500，永远判成「没有下一页」。
func OrderPageSize(n int) int {
	if n <= 0 || n > 200 {
		return 50
	}
	return n
}

// SearchOrders 后台按条件检索订单。
func (r *Repo) SearchOrders(ctx context.Context, f OrderFilter) ([]Order, error) {
	f.Limit = OrderPageSize(f.Limit)
	where, args := buildOrderWhere(f)
	if f.CursorID > 0 {
		args = append(args, f.CursorID)
		where += " and o.id < $" + itoa(int64(len(args)))
	}
	args = append(args, f.Limit)
	rows, err := r.pool.Query(ctx,
		orderCols+where+" order by o.id desc limit $"+itoa(int64(len(args))), args...)
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

// SummarizeOrders 按**同一套过滤条件**汇总。
func (r *Repo) SummarizeOrders(ctx context.Context, f OrderFilter) (OrderSummary, error) {
	where, args := buildOrderWhere(f)
	var s OrderSummary
	err := r.pool.QueryRow(ctx, `
		select count(*),
			count(*) filter (where o.pay_status = 'paid'),
			coalesce(sum(o.amount_cents) filter (where o.pay_status = 'paid'), 0),
			count(*) filter (where o.pay_status = 'refunded'),
			coalesce(sum(o.amount_cents) filter (where o.pay_status = 'refunded'), 0),
			count(*) filter (where o.reconcile_status = 'diff')
		from purchase_order o `+where, args...).
		Scan(&s.Count, &s.PaidCount, &s.PaidCents, &s.RefundCount,
			&s.RefundCents, &s.DiffCount)
	return s, err
}

// OrderDetail 后台看一张订单的全貌。
type OrderDetail struct {
	Order Order
	// Refunds 退款历史（含失败的）。
	Refunds []RefundRow
	// Grants 这一单发出的积分批次（含已回收的）。
	Grants []GrantRow
	// Membership 这一单买到的会员（如果有）。
	Membership *Membership
	// CallbackCount 收到过几条回调 —— **0 条**是"回调丢了"最直接的证据。
	CallbackCount int
}

// AdminOrderDetail 取订单全貌。**不带付费主体条件**（客服查别人的单）。
func (r *Repo) AdminOrderDetail(ctx context.Context, orderID int64) (
	OrderDetail, error) {
	o, err := scanOrder(r.pool.QueryRow(ctx, orderCols+`where o.id = $1`, orderID))
	if errors.Is(err, pgx.ErrNoRows) {
		return OrderDetail{}, ErrNotFound
	}
	if err != nil {
		return OrderDetail{}, err
	}
	out := OrderDetail{Order: o}
	if out.Refunds, err = r.RefundsOfOrder(ctx, orderID); err != nil {
		return OrderDetail{}, err
	}
	// ⚠️ 批次要**含已回收的**：只列 active 的话，一张退过款的订单
	// 在详情页上看起来"从来没发过积分"，而客服正是来查这个的。
	rows, err := r.pool.Query(ctx, `
		select id, source, amount, remaining, expire_at, status
		from credit_grant where source_ref_id = $1 order by id`, itoa(orderID))
	if err != nil {
		return OrderDetail{}, err
	}
	for rows.Next() {
		var g GrantRow
		if err := rows.Scan(&g.ID, &g.Source, &g.Amount, &g.Remaining,
			&g.ExpireAt, &g.Status); err != nil {
			rows.Close()
			return OrderDetail{}, err
		}
		out.Grants = append(out.Grants, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return OrderDetail{}, err
	}
	if m, err := scanMembership(r.pool.QueryRow(ctx,
		membershipCols+`where m.source_order_id = $1 order by m.id desc limit 1`,
		orderID)); err == nil {
		out.Membership = &m
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return OrderDetail{}, err
	}
	// ⚠️ 回调条数是"回调丢了"最直接的证据 —— 0 条 + 订单待支付
	// 就是那一类。详情里不给的话客服得再去翻回调留痕页。
	if err := r.pool.QueryRow(ctx,
		`select count(*) from payment_callback where out_trade_no = $1`,
		itoa(orderID)).Scan(&out.CallbackCount); err != nil {
		return OrderDetail{}, err
	}
	return out, nil
}

// ---------------------------------------------------------------- 人工调账

// AdjustInput 一次人工调账。
type AdjustInput struct {
	PayerID int64
	AdminID int64
	// Amount 正数＝加、负数＝减。
	Amount int
	Reason string
	// IdempotencyKey 幂等号。
	//
	// ⚠️ **必须有**：后台按钮一慢就会被点第二次，而这一下是直接改钱。
	// 靠 `uk_ct_ref_type(ref_type='admin_adjust', ref_id, type)` 兜底，
	// 不靠"先查再写"（并发的两次各自查一遍都会说"还没调过"）。
	IdempotencyKey string
	// ExpireAt 加积分时的有效期（nil＝永久）。
	ExpireAt *time.Time
}

// ErrAdjustDuplicate 幂等号重复。
var ErrAdjustDuplicate = errors.New("这个调账单号已经处理过")

// AdjustResult 调账结果。
type AdjustResult struct {
	TransactionID int64
	GrantID       int64
	BalanceAfter  int
}

// AdjustCredits 人工调账。
//
// ⚠️ **加与减是两条不同的路**，不能合成一条：
//   - 加 = 发一个新批次（有来源、有有效期，能被后续消费与退款正常处理）；
//   - 减 = 按**消费顺序**从现有批次里扣（先扣快到期的，与正常消费同一套规则）。
//
// 写成"直接改某个批次的 remaining"的话，逐批明细与流水就对不上了 ——
// 而那正是账本存在的理由。
//
// ⚠️ 扣减时余额不足**整笔拒绝**，不扣到 0：扣到 0 的话管理员以为扣了 500
// 而实际只扣了 120，审计里记的又是 500 —— 三方对不上。
func (r *Repo) AdjustCredits(ctx context.Context, in AdjustInput) (
	AdjustResult, error) {
	if in.Amount == 0 {
		return AdjustResult{}, domain.ErrAmountInvalid
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return AdjustResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var out AdjustResult
	if in.Amount > 0 {
		grantID := r.nextID()
		if _, err := tx.Exec(ctx, `
			insert into credit_grant (id, payer_account_id, source, source_ref_id,
				amount, remaining, expire_at)
			values ($1,$2,'admin_adjust',$3,$4,$4,$5)`,
			grantID, in.PayerID, in.IdempotencyKey, in.Amount, in.ExpireAt); err != nil {
			if isDuplicate(err) {
				return AdjustResult{}, ErrAdjustDuplicate
			}
			return AdjustResult{}, err
		}
		out.GrantID = grantID
	} else {
		// 扣减：按消费顺序锁定批次，复用正常消费的那套规则。
		grants, err := r.lockConsumable(ctx, tx, in.PayerID)
		if err != nil {
			return AdjustResult{}, err
		}
		allocs, err := domain.PlanConsume(grants, -in.Amount)
		if err != nil {
			// ⚠️ 余额不足**整笔拒绝**（PlanConsume 本身就是"要么全扣要么不扣"）。
			return AdjustResult{}, err
		}
		for _, a := range allocs {
			if _, err := tx.Exec(ctx, `
				update credit_grant set remaining = remaining - $2,
					status = case when remaining - $2 = 0 then 'exhausted' else status end,
					updated_at = now()
				where id = $1`, a.GrantID, a.Amount); err != nil {
				return AdjustResult{}, err
			}
		}
	}

	txID := r.nextID()
	if err := r.insertTxID(ctx, tx, txID, txInput{
		PayerID: in.PayerID, Type: TxAdjust, Change: in.Amount,
		// ⚠️ `ref_type` 固定 `admin_adjust`：幂等唯一键就落在
		// (admin_adjust, 幂等号, adjust) 上。混用别的 ref_type 的话
		// 同一个幂等号能被用两次。
		RefType: "admin_adjust", RefID: in.IdempotencyKey,
		Reason: in.Reason, OperatorAdmin: in.AdminID,
	}); err != nil {
		if isDuplicate(err) {
			return AdjustResult{}, ErrAdjustDuplicate
		}
		return AdjustResult{}, err
	}
	out.TransactionID = txID
	if err := tx.Commit(ctx); err != nil {
		return AdjustResult{}, err
	}
	bal, err := r.Balance(ctx, in.PayerID)
	if err != nil {
		return out, err
	}
	out.BalanceAfter = bal
	return out, nil
}

// ---------------------------------------------------------------- payer 一致性

// PayerView 一个付费主体的全貌（后台用户/团队详情要用）。
type PayerView struct {
	PayerID int64
	Kind    string
	OwnerID int64
	Balance int
	// Membership 当前有效会员（nil 表示没有）。
	Membership *Membership
	// WorkspaceIDs 挂在这个付费主体下的工作区。
	//
	// ⚠️ **可能不止一个**：个人工作区与团队工作区各有自己的 payer，
	// 但历史数据或误配会让两个工作区指向同一个 —— 那意味着
	// 一个团队的消费会记到个人账上。后台要能一眼看出来。
	WorkspaceIDs []int64
}

// PayerOverview 取付费主体全貌。
func (r *Repo) PayerOverview(ctx context.Context, payerID int64) (PayerView, error) {
	var v PayerView
	err := r.pool.QueryRow(ctx,
		`select id, kind, owner_id from payer_account where id = $1`, payerID).
		Scan(&v.PayerID, &v.Kind, &v.OwnerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return PayerView{}, ErrNotFound
	}
	if err != nil {
		return PayerView{}, err
	}
	if v.Balance, err = r.Balance(ctx, payerID); err != nil {
		return PayerView{}, err
	}
	if m, err := r.CurrentMembership(ctx, payerID); err == nil {
		v.Membership = &m
	} else if !errors.Is(err, ErrNotFound) {
		return PayerView{}, err
	}
	rows, err := r.pool.Query(ctx,
		`select id from workspace where payer_account_id = $1 and status = 'active'
		order by id`, payerID)
	if err != nil {
		return PayerView{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return PayerView{}, err
		}
		v.WorkspaceIDs = append(v.WorkspaceIDs, id)
	}
	return v, rows.Err()
}
