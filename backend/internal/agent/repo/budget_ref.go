package repo

import (
	"context"
)

// 多段工具（AGT-50）的额度条目查询：按业务引用结算、部分退回（00047）。
//
// ⚠️ 与 budget.go / budget_paid.go 分文件：这里只放「非生成对象」那条路径。
// 第一期 generate 的条目（ref_type = generation）仍走 scan.go 的 ChargedRefundCandidates +
// MarkRefunded，两条路径互不重叠（候选查询按 ref_type 互斥）。

// BudgetsBySteps 按步骤取额度条目（唤醒时找等待步骤挂的是什么对象）。没有条目的步骤不在结果里。
func (r *Repo) BudgetsBySteps(ctx context.Context, q Querier, stepIDs []int64) (map[int64]BudgetEntry, error) {
	out := map[int64]BudgetEntry{}
	if len(stepIDs) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `select `+budgetCols+`
		from agent_budget_entry where step_id = any($1::bigint[])`, stepIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		e, err := scanBudget(rows)
		if err != nil {
			return nil, err
		}
		out[e.StepID] = e
	}
	return out, rows.Err()
}

// SettleCandidate 是一条待按账本结算的条目（扫描 E 的第二条路径）。
type SettleCandidate struct {
	Entry          BudgetEntry
	WorkspaceID    int64
	ProjectID      int64
	PayerAccountID int64
	TurnID         int64
	Tool           string
	StepStatus     string
}

// UnsettledCandidates 取「charged、关联了非生成对象、还没结算」且步骤已经不在 running 的条目。
//
// 步骤还在 waiting 且会话仍在 waiting_task 的交给唤醒（A）—— 它会先回写步骤终态，下一次扫描
// 这里再结算；会话已不在 waiting_task 的（用户追问让它跑起来了）A 不会再替它回写，这里一并补。
// ⚠️ 只看最近 7 天（同 ChargedRefundCandidates）：结算正常在几秒到几分钟内完成。
// 走 idx_agent_budget_unsettled（00047）。
func (r *Repo) UnsettledCandidates(ctx context.Context, limit int) ([]SettleCandidate, error) {
	rows, err := r.pool.Query(ctx, `
		select `+budgetColsWith("e.")+`, s.workspace_id, s.project_id, s.payer_account_id,
		       st.turn_id, st.tool, st.status
		from agent_budget_entry e
		join agent_step st on st.id = e.step_id
		join agent_session s on s.id = e.session_id
		where e.status = 'charged' and e.settled_at is null
		  and e.ref_type is not null and e.ref_type <> 'generation'
		  and e.updated_at > now() - interval '7 days'
		  and (st.status in ('success', 'failed', 'cancelled')
		       or (st.status = 'waiting' and s.state <> 'waiting_task'))
		order by e.updated_at, e.id limit $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SettleCandidate
	for rows.Next() {
		var c SettleCandidate
		e := &c.Entry
		if err := rows.Scan(&e.ID, &e.SessionID, &e.StepID, &e.Quoted, &e.Status, &e.GenerationID,
			&e.RefType, &e.RefID, &e.RefIDs, &e.Refunded, &e.SettledAt, &e.CreatedAt, &e.UpdatedAt,
			&c.WorkspaceID, &c.ProjectID, &c.PayerAccountID, &c.TurnID, &c.Tool, &c.StepStatus); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SettleEntry 按账本结算一条 charged 条目（CAS：只结算一次）。
//
//	consumed == 0           → released（一分钱都没真扣，例如组里每一项都在受理时就失败了）
//	refunded >= consumed    → refunded（全部退回）
//	其余                     → 仍是 charged，refunded 记部分退回的金额
//
// quoted 校正为账本实扣：报价是预估（整图运行按计划报价，实际可能少跑几个被卡住的节点），
// 结算后「还剩多少授权」要与账本一致。refIDs 非空时覆盖（整图运行的子任务要到结算时才知道全）。
func (r *Repo) SettleEntry(ctx context.Context, id int64, consumed, refunded int, refIDs []int64) (bool, error) {
	if refunded > consumed {
		refunded = consumed
	}
	if refunded < 0 {
		refunded = 0
	}
	var ids any
	if len(refIDs) > 0 {
		ids = refIDs
	}
	return r.budgetCAS(ctx, `
		update agent_budget_entry set
			status = case when $2::int = 0 then 'released'
			              when $3::int >= $2::int then 'refunded' else 'charged' end,
			quoted = case when $2::int = 0 then quoted else $2::int end,
			refunded = case when $2::int = 0 then 0 else $3::int end,
			ref_ids = coalesce($4::bigint[], ref_ids),
			settled_at = now(), updated_at = now()
		where id = $1 and status = 'charged' and settled_at is null`, id, consumed, refunded, ids)
}

// budgetColsWith 是带表别名前缀的条目列清单（与 scanBudget 的顺序一致）。
func budgetColsWith(a string) string {
	return a + `id, ` + a + `session_id, ` + a + `step_id, ` + a + `quoted, ` + a + `status, coalesce(` + a + `generation_id, 0),
	coalesce(` + a + `ref_type, ''), coalesce(` + a + `ref_id, 0), coalesce(` + a + `ref_ids, '{}'::bigint[]), ` + a + `refunded, ` + a + `settled_at,
	` + a + `created_at, ` + a + `updated_at`
}

// BudgetByStepIn 在给定的查询者（事务）里取步骤的额度条目；没有返回 ErrNotFound。
func (r *Repo) BudgetByStepIn(ctx context.Context, q Querier, stepID int64) (BudgetEntry, error) {
	return scanBudget(q.QueryRow(ctx, `select `+budgetCols+`
		from agent_budget_entry where step_id = $1`, stepID))
}
