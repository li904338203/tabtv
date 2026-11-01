package repo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// 步骤状态（与 ck_agent_step_status 逐字一致）。
const (
	StepRunning   = "running"
	StepSuccess   = "success"
	StepFailed    = "failed"
	StepCancelled = "cancelled"
	StepWaiting   = "waiting"
)

// Step 是 agent_step 的一行：一次工具调用。GenerationID 为 0 表示没有关联任务。
type Step struct {
	ID            int64
	SessionID     int64
	TurnID        int64
	Tool          string
	Status        string
	Attempt       int
	Args          json.RawMessage
	Display       json.RawMessage
	GenerationID  int64
	CreditsQuoted int
	ErrorMessage  string
	// PlanItemKey / ErrorCode（MKT-T-22，00061）：营销会话付费步骤对应的计划项键、失败的错误码
	// （工具层常量如 plan_item_mismatch，或受理错误码名如 INSUFFICIENT_CREDITS）。
	// ⚠️ 停后续 / 部分成功按 ErrorCode 现算（MKT-T-27），不按 ErrorMessage 的文案。
	PlanItemKey string
	ErrorCode   string
	StartedAt   time.Time
	FinishedAt  *time.Time
}

const stepCols = `id, session_id, turn_id, tool, status, attempt, args, display,
	coalesce(generation_id, 0), credits_quoted, coalesce(error_message, ''),
	coalesce(plan_item_key, ''), coalesce(error_code, ''), started_at, finished_at`

func scanStep(row pgx.Row) (Step, error) {
	var s Step
	err := row.Scan(&s.ID, &s.SessionID, &s.TurnID, &s.Tool, &s.Status, &s.Attempt, &s.Args,
		&s.Display, &s.GenerationID, &s.CreditsQuoted, &s.ErrorMessage, &s.PlanItemKey, &s.ErrorCode,
		&s.StartedAt, &s.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Step{}, ErrNotFound
	}
	return s, err
}

// InsertStep 记一次工具调用（status=running）。
func (r *Repo) InsertStep(ctx context.Context, q Querier, s Step) (Step, error) {
	attempt := s.Attempt
	if attempt <= 0 {
		attempt = 1
	}
	out, err := scanStep(q.QueryRow(ctx, `
		insert into agent_step (id, session_id, turn_id, tool, status, attempt, args, display, credits_quoted)
		values ($1, $2, $3, $4, 'running', $5, $6, $7, $8)
		returning `+stepCols,
		s.ID, s.SessionID, s.TurnID, s.Tool, attempt, jsonOr(s.Args, "{}"), jsonOr(s.Display, "{}"),
		s.CreditsQuoted))
	if err != nil {
		return Step{}, mapDuplicate(err)
	}
	return out, nil
}

// GetStep 取步骤。
func (r *Repo) GetStep(ctx context.Context, id int64) (Step, error) {
	return scanStep(r.pool.QueryRow(ctx, `select `+stepCols+` from agent_step where id = $1`, id))
}

// FinishStep 把步骤结束到 success / failed / cancelled。CAS 只从 running / waiting 出，
// 终态不被晚到的结果覆盖。display=nil 表示不改展示内容。
func (r *Repo) FinishStep(ctx context.Context, q Querier, id int64, status string,
	display json.RawMessage, errMsg string) (bool, error) {
	if status != StepSuccess && status != StepFailed && status != StepCancelled {
		return false, ErrNotTerminal
	}
	var disp any
	if len(display) > 0 {
		disp = display
	}
	tag, err := q.Exec(ctx, `
		update agent_step set status = $2, display = coalesce($3::jsonb, display),
			error_message = left($4, 1024), finished_at = now()
		where id = $1 and status in ('running', 'waiting')`, id, status, disp, nullStr(errMsg))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// TagStep 记步骤的计划项键与错误码（MKT-T-22）。空串 = 这一项不动（不把已记的值清掉）。
//
// 不带状态条件：标记是步骤的事实（它对应哪一项、为什么失败），与状态推进无关；
// 由 Runner 在结束 / 挂起步骤之前写。超长截断到列宽（键已由 ValidPlanKey 限 32 位）。
func (r *Repo) TagStep(ctx context.Context, q Querier, id int64, planItemKey, errorCode string) error {
	if planItemKey == "" && errorCode == "" {
		return nil
	}
	_, err := q.Exec(ctx, `
		update agent_step set plan_item_key = coalesce(nullif(left($2, 32), ''), plan_item_key),
			error_code = coalesce(nullif(left($3, 64), ''), error_code)
		where id = $1`, id, planItemKey, errorCode)
	return err
}

// SetStepWaiting CAS running → waiting，并挂上受理得到的生成任务 ID。
//
// generationID=0：等的不是单个生成任务（组 / 整图运行 / 渲染，AGT-50），等的对象在这一步的
// 额度条目上（ref_type / ref_id，一步一条），扫描循环按它查终态。此时 generation_id 留空。
func (r *Repo) SetStepWaiting(ctx context.Context, q Querier, id, generationID int64) (bool, error) {
	tag, err := q.Exec(ctx, `
		update agent_step set status = 'waiting', generation_id = nullif($2::bigint, 0)
		where id = $1 and status = 'running'`, id, generationID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// WaitingBySession 列出会话里仍在等任务的步骤（扫描循环判断「所等任务是否全部终态」）。
func (r *Repo) WaitingBySession(ctx context.Context, sessionID int64) ([]Step, error) {
	return r.querySteps(ctx, `select `+stepCols+` from agent_step
		where session_id = $1 and status = 'waiting' order by started_at, id`, sessionID)
}

// StepsByGeneration 按生成任务反查步骤（任务终态回写 / 退款时找到对应会话）。
func (r *Repo) StepsByGeneration(ctx context.Context, generationID int64) ([]Step, error) {
	return r.querySteps(ctx, `select `+stepCols+` from agent_step
		where generation_id = $1 order by started_at, id`, generationID)
}

func (r *Repo) querySteps(ctx context.Context, sql string, args ...any) ([]Step, error) {
	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Step
	for rows.Next() {
		s, err := scanStep(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
