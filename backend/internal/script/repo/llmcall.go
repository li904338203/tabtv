package repo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/li904338203/tabtv/backend/internal/script/domain"
)

// ErrCallDuplicate 同一个幂等键已经有一次调用了。
var ErrCallDuplicate = errors.New("这次生成已经在进行中或已完成")

// LLMCall 是一次同步 LLM 调用的记录。
type LLMCall struct {
	ID             int64
	WorkspaceID    int64
	PayerAccountID int64
	ScriptID       int64
	ShotID         int64
	Purpose        string
	ModelID        int64
	IdempotencyKey string
	// CreatedByUserID 是**谁发起的**（P12-03、NEW-20）。可能为 0：
	// P12-03 之前的历史行没有这个信息，**不编造**一个（默认成工作区所有者的话，
	// 审计会指向一个完全无辜的人，而且看不出那是猜的）。
	CreatedByUserID int64
	Status          string
	CreditsCost     int
	Result          json.RawMessage
	ErrorMessage    string
}

// NextRound 取下一轮的轮次号。
//
// ⚠️ 按**调用次数**算，不按产出物数量：只按产出物算的话，一次失败
// 什么都没改变，重试会算出同一个轮次、拼出同一个幂等键、命中那条
// failed 记录，于是永远冲突 —— **用户再也点不动「重新生成」**。
// 所以这里数的是 `script_llm_call` 的行数（含 failed）。
func (r *Repo) NextRound(ctx context.Context, scriptID int64, purpose domain.Purpose) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, `
		select count(*) from script_llm_call where script_id = $1 and purpose = $2`,
		scriptID, string(purpose)).Scan(&n)
	return n + 1, err
}

// InsertCall 开一次调用记录（running）。撞幂等键返回 ErrCallDuplicate。
func (r *Repo) InsertCall(ctx context.Context, c LLMCall) error {
	_, err := r.pool.Exec(ctx, `
		insert into script_llm_call (id, workspace_id, payer_account_id, script_id,
			shot_id, purpose, model_id, idempotency_key, status, credits_cost,
			created_by_user_id)
		values ($1,$2,$3,$4,nullif($5::bigint,0),$6,$7,$8,'running',$9,
			nullif($10::bigint,0))`,
		c.ID, c.WorkspaceID, c.PayerAccountID, c.ScriptID, c.ShotID,
		c.Purpose, c.ModelID, c.IdempotencyKey, c.CreditsCost, c.CreatedByUserID)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrCallDuplicate
	}
	return err
}

// StaleCall 是停在 running 太久的同步模型调用（恢复扫描用）。
type StaleCall struct {
	ID             int64
	PayerAccountID int64
	CreditsCost    int
}

// StaleRunningCalls 取停在 running 超过 olderThan 的调用，最早的在前。
func (r *Repo) StaleRunningCalls(ctx context.Context, olderThan time.Duration, limit int) ([]StaleCall, error) {
	rows, err := r.pool.Query(ctx, `
		select id, payer_account_id, credits_cost from script_llm_call
		where status = $1 and created_at < now() - make_interval(secs => $2)
		order by created_at limit $3`, string(domain.LLMRunning), olderThan.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StaleCall
	for rows.Next() {
		var c StaleCall
		if err := rows.Scan(&c.ID, &c.PayerAccountID, &c.CreditsCost); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// FailCallIfRunning 只在仍是 running 时判失败（CAS），返回「本次是否真的改了状态」。
// ⚠️ 不带状态条件的话，恢复扫描会把一个刚好结束的成功调用改回失败。
func (r *Repo) FailCallIfRunning(ctx context.Context, id int64, reason string) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		update script_llm_call set status = $2, error_message = nullif(left($3, 1024), ''), updated_at = now()
		where id = $1 and status = $4`,
		id, string(domain.LLMFailed), reason, string(domain.LLMRunning))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// FinishCall 写终态。
func (r *Repo) FinishCall(ctx context.Context, id int64, status domain.LLMStatus,
	result json.RawMessage, errMsg string) error {
	_, err := r.pool.Exec(ctx, `
		update script_llm_call set status = $2, result = coalesce($3, result),
			error_message = nullif(left($4, 1024), ''), updated_at = now()
		where id = $1`, id, string(status), nilJSON(result), errMsg)
	return err
}

func (r *Repo) GetCall(ctx context.Context, id int64) (LLMCall, error) {
	var c LLMCall
	var shotID *int64
	err := r.pool.QueryRow(ctx, `
		select id, workspace_id, payer_account_id, script_id, shot_id, purpose,
		       model_id, idempotency_key, status, credits_cost, result,
		       coalesce(error_message,'')
		from script_llm_call where id = $1`, id).
		Scan(&c.ID, &c.WorkspaceID, &c.PayerAccountID, &c.ScriptID, &shotID, &c.Purpose,
			&c.ModelID, &c.IdempotencyKey, &c.Status, &c.CreditsCost, &c.Result,
			&c.ErrorMessage)
	if errors.Is(err, pgx.ErrNoRows) {
		return LLMCall{}, ErrNotFound
	}
	if shotID != nil {
		c.ShotID = *shotID
	}
	return c, err
}

// ListCalls 列出一个脚本的调用记录（费用可对账）。
func (r *Repo) ListCalls(ctx context.Context, scriptID int64, limit int) ([]LLMCall, error) {
	rows, err := r.pool.Query(ctx, `
		select id, workspace_id, payer_account_id, script_id, coalesce(shot_id,0), purpose,
		       model_id, idempotency_key, status, credits_cost, coalesce(error_message,'')
		from script_llm_call where script_id = $1 order by created_at desc limit $2`,
		scriptID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LLMCall
	for rows.Next() {
		var c LLMCall
		if err := rows.Scan(&c.ID, &c.WorkspaceID, &c.PayerAccountID, &c.ScriptID,
			&c.ShotID, &c.Purpose, &c.ModelID, &c.IdempotencyKey, &c.Status,
			&c.CreditsCost, &c.ErrorMessage); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
