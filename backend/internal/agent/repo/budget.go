package repo

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// 额度预留状态（与 ck_agent_budget_status 逐字一致）。
const (
	BudgetReserved = "reserved"
	BudgetCharged  = "charged"
	BudgetReleased = "released"
	BudgetRefunded = "refunded"
)

// 额度条目关联的业务对象（与 ck_agent_budget_ref_type 逐字一致，00047）。
const (
	RefGeneration    = "generation"
	RefScriptLLMCall = "script_llm_call"
	RefScriptCompose = "script_compose"
	RefAssetBatch    = "asset_batch"
	RefScriptGroup   = "script_group"
	RefWorkflowRun   = "workflow_run"
	RefRenderJob     = "render_job"
)

// BudgetEntry 是 agent_budget_entry 的一行：一次付费工具调用的额度占用。
type BudgetEntry struct {
	ID           int64
	SessionID    int64
	StepID       int64
	Quoted       int
	Status       string
	GenerationID int64
	// RefType / RefID / RefIDs 关联的业务对象（00047）；RefType 为空 = 还没受理成功。
	RefType string
	RefID   int64
	RefIDs  []int64
	// Refunded 已退回的积分（部分退回累加）；占用口径 = Quoted − Refunded。
	Refunded  int
	SettledAt *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Occupied 是这一条占用的额度：reserved / charged 计 quoted − refunded，其余为 0。
func (e BudgetEntry) Occupied() int {
	if e.Status != BudgetReserved && e.Status != BudgetCharged {
		return 0
	}
	return e.Quoted - e.Refunded
}

// Returned 是这一条已归还（退回）的额度：refunded 状态整笔，charged 的部分退回按 refunded 列。
func (e BudgetEntry) Returned() int {
	switch e.Status {
	case BudgetRefunded:
		return e.Quoted
	case BudgetCharged:
		return e.Refunded
	}
	return 0
}

var budgetCols = budgetColsWith("")

func scanBudget(row pgx.Row) (BudgetEntry, error) {
	var e BudgetEntry
	err := row.Scan(&e.ID, &e.SessionID, &e.StepID, &e.Quoted, &e.Status, &e.GenerationID,
		&e.RefType, &e.RefID, &e.RefIDs, &e.Refunded, &e.SettledAt, &e.CreatedAt, &e.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return BudgetEntry{}, ErrNotFound
	}
	return e, err
}

// UsedInTx 统计会话已占用的额度：reserved + charged 的 quoted − refunded 合计。
//
// ⚠️ 必须在**锁住会话行**的事务里调用，并与 InsertReserved 同一事务（dev/01 §5.2）：
// 检查与预留拆开的话，并发的付费工具都会看到「够」然后一起超额。
// released / refunded 不计 —— 额度归还；charged 的部分退回（组部分成功，00047）减掉已退的那部分。
func (r *Repo) UsedInTx(ctx context.Context, tx pgx.Tx, sessionID int64) (int, error) {
	var used int
	err := tx.QueryRow(ctx, `
		select coalesce(sum(quoted - refunded), 0)::int from agent_budget_entry
		where session_id = $1 and status in ('reserved', 'charged')`, sessionID).Scan(&used)
	return used, err
}

// InsertReserved 写一行预留。同一步骤第二行撞 uk_agent_budget_step → ErrDuplicate。
func (r *Repo) InsertReserved(ctx context.Context, tx pgx.Tx, e BudgetEntry) (BudgetEntry, error) {
	out := e
	out.Status = BudgetReserved
	err := tx.QueryRow(ctx, `
		insert into agent_budget_entry (id, session_id, step_id, quoted, status)
		values ($1, $2, $3, $4, 'reserved')
		returning created_at, updated_at`, e.ID, e.SessionID, e.StepID, e.Quoted).
		Scan(&out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return BudgetEntry{}, mapDuplicate(err)
	}
	return out, nil
}

// MarkCharged CAS reserved → charged，挂上生成任务 ID（受理成功）。
func (r *Repo) MarkCharged(ctx context.Context, id, generationID int64) (bool, error) {
	return r.budgetCAS(ctx, `
		update agent_budget_entry set status = 'charged', generation_id = nullif($2::bigint, 0),
			updated_at = now()
		where id = $1 and status = 'reserved'`, id, generationID)
}

// MarkReleased CAS reserved → released（受理失败，额度归还）。
func (r *Repo) MarkReleased(ctx context.Context, id int64) (bool, error) {
	return r.budgetCAS(ctx, `
		update agent_budget_entry set status = 'released', updated_at = now()
		where id = $1 and status = 'reserved'`, id)
}

// MarkRefunded CAS charged → refunded（任务失败已退款，额度归还）。
func (r *Repo) MarkRefunded(ctx context.Context, id int64) (bool, error) {
	return r.budgetCAS(ctx, `
		update agent_budget_entry set status = 'refunded', updated_at = now()
		where id = $1 and status = 'charged'`, id)
}

func (r *Repo) budgetCAS(ctx context.Context, sql string, args ...any) (bool, error) {
	tag, err := r.pool.Exec(ctx, sql, args...)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// BudgetBySession 列出会话的全部额度行（对账与测试用）。
func (r *Repo) BudgetBySession(ctx context.Context, sessionID int64) ([]BudgetEntry, error) {
	rows, err := r.pool.Query(ctx, `select `+budgetCols+`
		from agent_budget_entry where session_id = $1 order by created_at, id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BudgetEntry
	for rows.Next() {
		e, err := scanBudget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
