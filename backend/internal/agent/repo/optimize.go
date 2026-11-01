package repo

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// 一键优化提示词的调用记录（迁移 00046，AGT-43）。每次点击一行，失败也留着。

// 调用状态。
const (
	OptimizeRunning = "running"
	OptimizeSuccess = "success"
	OptimizeFailed  = "failed"
)

// PromptOptimize 是 agent_prompt_optimize 的一行。
type PromptOptimize struct {
	ID             int64
	WorkspaceID    int64
	PayerAccountID int64
	UserID         int64
	ModelCode      string
	IdempotencyKey string
	Status         string
	CreditsCost    int
	InputText      string
	OutputText     string
	ErrorCode      string
	ErrorMessage   string
	CreatedAt      time.Time
	FinishedAt     *time.Time
}

const optimizeCols = `id, workspace_id, payer_account_id, user_id, model_code, idempotency_key, status,
	credits_cost, input_text, coalesce(output_text, ''), coalesce(error_code, ''), coalesce(error_message, ''),
	created_at, finished_at`

func scanOptimize(row pgx.Row) (PromptOptimize, error) {
	var o PromptOptimize
	err := row.Scan(&o.ID, &o.WorkspaceID, &o.PayerAccountID, &o.UserID, &o.ModelCode, &o.IdempotencyKey,
		&o.Status, &o.CreditsCost, &o.InputText, &o.OutputText, &o.ErrorCode, &o.ErrorMessage,
		&o.CreatedAt, &o.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return PromptOptimize{}, ErrNotFound
	}
	return o, err
}

// InsertOptimize 建一条 running 的调用记录。同一（工作区, 发起人, 幂等键）已存在 → ErrDuplicate。
func (r *Repo) InsertOptimize(ctx context.Context, o PromptOptimize) (PromptOptimize, error) {
	row := r.pool.QueryRow(ctx, `insert into agent_prompt_optimize
		(id, workspace_id, payer_account_id, user_id, model_code, idempotency_key, status, credits_cost, input_text)
		values ($1,$2,$3,$4,$5,$6,'running',$7,$8)
		returning `+optimizeCols,
		o.ID, o.WorkspaceID, o.PayerAccountID, o.UserID, o.ModelCode, o.IdempotencyKey, o.CreditsCost, o.InputText)
	out, err := scanOptimize(row)
	return out, mapDuplicate(err)
}

// GetOptimizeByKey 按幂等键取（作用域：工作区 + 发起人）。
func (r *Repo) GetOptimizeByKey(ctx context.Context, workspaceID, userID int64, key string) (PromptOptimize, error) {
	return scanOptimize(r.pool.QueryRow(ctx, `select `+optimizeCols+` from agent_prompt_optimize
		where workspace_id = $1 and user_id = $2 and idempotency_key = $3`, workspaceID, userID, key))
}

// GetOptimize 按 ID 取（测试与排障用）。
func (r *Repo) GetOptimize(ctx context.Context, id int64) (PromptOptimize, error) {
	return scanOptimize(r.pool.QueryRow(ctx, `select `+optimizeCols+` from agent_prompt_optimize where id = $1`, id))
}

// FinishOptimize 把 running 的调用落成终态（CAS）。返回本次是否真的改了状态 ——
// 调用方据此决定要不要退款：恢复扫描与请求本身同时结算时只有一方生效。
func (r *Repo) FinishOptimize(ctx context.Context, id int64, status, output, errCode, errMsg string) (bool, error) {
	tag, err := r.pool.Exec(ctx, `update agent_prompt_optimize
		set status = $2, output_text = nullif($3, ''), error_code = nullif($4, ''),
		    error_message = nullif(left($5, 1024), ''), finished_at = now()
		where id = $1 and status = 'running'`, id, status, output, errCode, errMsg)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// StaleRunningOptimizes 列出停在 running 超过 olderThan 的调用（恢复扫描用）。
func (r *Repo) StaleRunningOptimizes(ctx context.Context, olderThan time.Duration, limit int) ([]PromptOptimize, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.pool.Query(ctx, `select `+optimizeCols+` from agent_prompt_optimize
		where status = 'running' and created_at < now() - make_interval(secs => $1)
		order by created_at limit $2`, olderThan.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PromptOptimize
	for rows.Next() {
		o, err := scanOptimize(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
