package repo

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// 付费工具外壳（AGT-24，app/budget.go）额外要用的额度查询。
//
// ⚠️ 与 budget.go 分文件：budget.go 归 AGT-21 / AGT-25（扫描回写 refunded）维护，
// 这里只放 PaidCall 才用的几条。

// RefundedInTx 统计会话已归还（任务失败退款）的额度合计，只用于追加额度卡的展示。
func (r *Repo) RefundedInTx(ctx context.Context, tx pgx.Tx, sessionID int64) (int, error) {
	var n int
	// refunded 状态整笔归还；charged 的部分退回（组部分成功，00047）按 refunded 列。
	err := tx.QueryRow(ctx, `
		select coalesce(sum(case when status = 'refunded' then quoted else refunded end), 0)::int
		from agent_budget_entry
		where session_id = $1 and status in ('refunded', 'charged')`, sessionID).Scan(&n)
	return n, err
}

// ReReserveInTx 把同一步骤上一次**已归还**的条目重新预留（同一步骤的下一个 attempt）。
//
// uk_agent_budget_step 让一个步骤只能有一行；上一次受理失败（released）或任务失败已退款
// （refunded，AGT-25 回写）之后同一步骤再试，直接 insert 会撞唯一索引。这里只认这两种：
// 上一次仍是 reserved / charged 的话说明它还占着额度（或任务还活着），返回 ErrDuplicate ——
// 不能让同一步骤占两份额度。
func (r *Repo) ReReserveInTx(ctx context.Context, tx pgx.Tx, stepID int64, quoted int) (BudgetEntry, error) {
	// ⚠️ 引用、退回金额与结算时间一并清掉（00047）：留着上一次的 ref 的话，扫描循环会拿上一次
	// 那批任务的结局去结算这一次。
	e, err := scanBudget(tx.QueryRow(ctx, `
		update agent_budget_entry set status = 'reserved', quoted = $2, generation_id = null,
			ref_type = null, ref_id = null, ref_ids = null, refunded = 0, settled_at = null, updated_at = now()
		where step_id = $1 and status in ('released', 'refunded')
		returning `+budgetCols, stepID, quoted))
	if errors.Is(err, ErrNotFound) {
		return BudgetEntry{}, ErrDuplicate
	}
	return e, err
}

// MarkChargedCredits CAS reserved → charged，挂上生成任务 ID，并把 quoted 校正为受理实扣的金额。
//
// ⚠️ 受理按**受理那一刻**的价格扣费：报价与受理之间运营改了价的话，条目若还记旧报价，
// 「还剩多少授权」就和账本对不上。charged 的条目以实扣为准；credits<0 表示不校正。
func (r *Repo) MarkChargedCredits(ctx context.Context, id, generationID int64, credits int) (bool, error) {
	ref := ChargeRef{GenerationID: generationID}
	if generationID != 0 {
		ref.Type, ref.ID = RefGeneration, generationID
	}
	return r.MarkChargedRef(ctx, id, credits, ref)
}

// ChargeRef 是受理成功后挂到条目上的业务引用（00047）。
type ChargeRef struct {
	// GenerationID 只在关联单个生成任务时给（第一期 generate 的路径按它找任务）。
	GenerationID int64
	Type         string
	ID           int64
	IDs          []int64
}

// MarkChargedRef CAS reserved → charged，挂上业务引用，并把 quoted 校正为实扣（credits<0 不校正）。
//
// ⚠️ 引用与 charged 在同一条 update 里写：分两步的话中间崩一次，条目就是一个「已扣费却说不出
// 扣在哪」的 charged —— 扫描循环找不到对象，额度永远占着。
func (r *Repo) MarkChargedRef(ctx context.Context, id int64, credits int, ref ChargeRef) (bool, error) {
	var ids any
	if len(ref.IDs) > 0 {
		ids = ref.IDs
	}
	return r.budgetCAS(ctx, `
		update agent_budget_entry set status = 'charged', generation_id = nullif($2::bigint, 0),
			quoted = case when $3::int >= 0 then $3::int else quoted end,
			ref_type = nullif($4, ''), ref_id = nullif($5::bigint, 0), ref_ids = $6::bigint[],
			updated_at = now()
		where id = $1 and status = 'reserved'`, id, ref.GenerationID, credits, ref.Type, ref.ID, ids)
}

// BudgetByStep 取步骤的额度条目（测试与对账用）。
func (r *Repo) BudgetByStep(ctx context.Context, stepID int64) (BudgetEntry, error) {
	return scanBudget(r.pool.QueryRow(ctx, `select `+budgetCols+`
		from agent_budget_entry where step_id = $1`, stepID))
}
