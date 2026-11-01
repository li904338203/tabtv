package repo

import (
	"context"
	"time"
)

// 工作区用量审计（P12-04；NEW-20「能审计追踪」）。
//
// 数据到 P12-03 为止已经齐全（工作区 + 操作者 + 费用承担账户三要素），
// 但**没有任何入口能问「这个团队的积分被谁花了」** —— 有数据不等于能审计。
//
// ⚠️ **口径只能有一个，且必须是账本**。按各业务表的 `credits_cost` 相加是错的：
// 那一列是**报价**，任务失败退款之后它不会变 —— 于是一个失败又退款的任务
// 会被算成花了钱，而两个数都"没错"（旧系统在看板口径上踩过同一类：
// 按 detail 子表汇总会重复计算）。
//
// ⚠️ **退款单列，不从消耗里减掉**。这条规则在本项目已经出现第四次
// （整图费用对账、后台订单汇总、退款历史、这里）：把退回的悄悄减掉之后，
// 用户拿这个数去对账本流水会**对不上**，而他无从知道差额是怎么来的。
// 净消耗由看的人自己减 —— 两个数都在，他减得出来；只给净值就再也还原不回去。

// MemberUsage 是一个操作者在某段时间里的用量。
type MemberUsage struct {
	// UserID 为 0 表示**没有操作者**的流水（过期扣减、系统调账）。
	// ⚠️ 这一行不能丢：丢掉的话各成员之和对不上总额，
	// 而那个差额没有任何人解释得了。
	UserID   int64
	Nickname string
	// Consumed 是扣掉的总额（正数），Refunded 是退回的总额（正数）。
	Consumed int
	Refunded int
	// Tasks 是产生过扣费的业务条数（按 ref_id 去重）。
	// ⚠️ 不是流水条数：一次生成只有一条 consume，但退款会再加一条，
	// 数流水的话"任务数"会随失败率上涨。
	Tasks  int
	LastAt time.Time
}

// UsageSummary 是工作区整体。
type UsageSummary struct {
	Consumed int
	Refunded int
	Tasks    int
	// Members 是有过用量的人数（含"无操作者"那一行）。
	Members int
}

// UsageFilter 限定时间窗。两端都可空 —— 不传就是全部历史。
type UsageFilter struct {
	From *time.Time
	To   *time.Time
}

func (f UsageFilter) where(payerID int64) (string, []any) {
	sql := ` where t.payer_account_id = $1 and t.type in ('consume','refund')`
	args := []any{payerID}
	if f.From != nil {
		args = append(args, *f.From)
		sql += ` and t.created_at >= $2`
	}
	if f.To != nil {
		args = append(args, *f.To)
		sql += ` and t.created_at < $` + itoa(int64(len(args)))
	}
	return sql, args
}

// WorkspaceUsage 按操作者汇总一个付费主体的消耗与退回。
//
// ⚠️ 退款流水上的 `operator_user_id` 是**发起退款的人**，而自动退款（任务失败）
// 那条上是空的 —— 直接按它分组的话，同一笔任务的扣费算在操作者头上、
// 退款算在"未知"头上，两边对不齐。所以退款**按原消费流水的操作者**归集：
// 用 (ref_type, ref_id) 关回那条 consume。关不上的（历史数据、人工调账）
// 落到"未知"那一行，而不是丢掉。
func (r *Repo) WorkspaceUsage(ctx context.Context, payerID int64, f UsageFilter) (
	[]MemberUsage, UsageSummary, error) {
	cond, args := f.where(payerID)
	rows, err := r.pool.Query(ctx, `
		with tx as (
			select t.id, t.type, t.change, t.ref_type, t.ref_id, t.created_at,
			       -- 退款归给**原消费**的操作者：自动退款那条自己没有操作者，
			       -- 按它自己分组会让扣费与退款落在两个人头上。
			       coalesce(t.operator_user_id, c.operator_user_id, 0) as actor
			from credit_transaction t
			left join lateral (
				select operator_user_id from credit_transaction c0
				where c0.payer_account_id = t.payer_account_id
				  and c0.ref_type = t.ref_type and c0.ref_id = t.ref_id
				  and c0.type = 'consume'
				limit 1
			) c on t.type = 'refund'
			`+cond+`
		)
		-- ⚠️ 每一列都要限定表名：tx 与 app_user 都有 created_at，
		-- 不限定的话 PG 直接报 "column reference is ambiguous"
		-- （实测第一次跑就红在这里）。
		select tx.actor,
		       coalesce(u.nickname, '') as nickname,
		       sum(case when tx.type = 'consume' then -tx.change else 0 end)::int as consumed,
		       sum(case when tx.type = 'refund'  then  tx.change else 0 end)::int as refunded,
		       count(distinct case when tx.type = 'consume'
		             then tx.ref_type || ':' || tx.ref_id end)::int as tasks,
		       max(tx.created_at) as last_at
		from tx
		left join app_user u on u.id = tx.actor
		group by tx.actor, u.nickname
		order by consumed desc, tx.actor`, args...)
	if err != nil {
		return nil, UsageSummary{}, err
	}
	defer rows.Close()

	var out []MemberUsage
	var sum UsageSummary
	for rows.Next() {
		var m MemberUsage
		if err := rows.Scan(&m.UserID, &m.Nickname, &m.Consumed, &m.Refunded,
			&m.Tasks, &m.LastAt); err != nil {
			return nil, UsageSummary{}, err
		}
		sum.Consumed += m.Consumed
		sum.Refunded += m.Refunded
		sum.Tasks += m.Tasks
		sum.Members++
		out = append(out, m)
	}
	return out, sum, rows.Err()
}

// UsageEntry 是一条用量明细。
type UsageEntry struct {
	TransactionID int64
	UserID        int64
	Nickname      string
	Type          string
	Change        int
	RefType       string
	RefID         string
	Reason        string
	CreatedAt     time.Time
}

// WorkspaceUsageDetail 列出明细，供"这个人到底花在什么上"追下去。
//
// ⚠️ 用游标不用 offset：流水一直在新增，offset 翻页会漏掉或重复。
func (r *Repo) WorkspaceUsageDetail(ctx context.Context, payerID, userID int64,
	f UsageFilter, limit int, cursorID int64) ([]UsageEntry, error) {
	cond, args := f.where(payerID)
	if userID > 0 {
		args = append(args, userID)
		cond += ` and t.operator_user_id = $` + itoa(int64(len(args)))
	}
	if cursorID > 0 {
		args = append(args, cursorID)
		cond += ` and t.id < $` + itoa(int64(len(args)))
	}
	args = append(args, limit)
	rows, err := r.pool.Query(ctx, `
		select t.id, coalesce(t.operator_user_id, 0), coalesce(u.nickname, ''),
		       t.type, t.change, t.ref_type, t.ref_id, coalesce(t.reason, ''), t.created_at
		from credit_transaction t
		left join app_user u on u.id = t.operator_user_id`+cond+`
		order by t.id desc limit $`+itoa(int64(len(args))), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageEntry
	for rows.Next() {
		var e UsageEntry
		if err := rows.Scan(&e.TransactionID, &e.UserID, &e.Nickname, &e.Type,
			&e.Change, &e.RefType, &e.RefID, &e.Reason, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
