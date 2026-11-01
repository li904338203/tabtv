package repo

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
)

// Turn 是 agent_turn 的一行（计费单位，refType=agent_turn，refID=ID）。
type Turn struct {
	ID             int64
	SessionID      int64
	Trigger        domain.TurnTrigger
	Status         domain.TurnStatus
	ModelCode      string
	CreditsCost    int
	CallCount      int
	ValidOutputs   int
	IdempotencyKey string
	ErrorCode      string
	ErrorMessage   string
	StartedAt      *time.Time
	FinishedAt     *time.Time
	CreatedAt      time.Time
}

const turnCols = `id, session_id, trigger, status, coalesce(model_code, ''), credits_cost, call_count,
	valid_outputs, coalesce(idempotency_key, ''), coalesce(error_code, ''), coalesce(error_message, ''),
	started_at, finished_at, created_at`

func scanTurn(row pgx.Row) (Turn, error) {
	var t Turn
	err := row.Scan(&t.ID, &t.SessionID, &t.Trigger, &t.Status, &t.ModelCode, &t.CreditsCost,
		&t.CallCount, &t.ValidOutputs, &t.IdempotencyKey, &t.ErrorCode, &t.ErrorMessage,
		&t.StartedAt, &t.FinishedAt, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Turn{}, ErrNotFound
	}
	return t, err
}

// InsertTurn 建轮次（status=pending）。同一会话同一幂等键第二次插入返回 ErrDuplicate，
// 调用方用 GetByIdempotencyKey 取回已建的那一轮。
//
// ⚠️ 轮次每次新建，失败的轮也保留记录：不复用失败轮的幂等键（CD-34 的坑）。
func (r *Repo) InsertTurn(ctx context.Context, q Querier, t Turn) (Turn, error) {
	out, err := scanTurn(q.QueryRow(ctx, `
		insert into agent_turn (id, session_id, trigger, status, model_code, credits_cost, idempotency_key)
		values ($1, $2, $3, 'pending', $4, $5, $6)
		returning `+turnCols,
		t.ID, t.SessionID, t.Trigger, nullStr(t.ModelCode), t.CreditsCost, nullStr(t.IdempotencyKey)))
	if err != nil {
		return Turn{}, mapDuplicate(err)
	}
	return out, nil
}

// GetTurn 取轮次。
func (r *Repo) GetTurn(ctx context.Context, id int64) (Turn, error) {
	return scanTurn(r.pool.QueryRow(ctx, `select `+turnCols+` from agent_turn where id = $1`, id))
}

// GetTurnByIdempotencyKey 按（会话, 幂等键）取轮次。
func (r *Repo) GetTurnByIdempotencyKey(ctx context.Context, sessionID int64, key string) (Turn, error) {
	return scanTurn(r.pool.QueryRow(ctx, `select `+turnCols+` from agent_turn
		where session_id = $1 and idempotency_key = $2`, sessionID, key))
}

// SetTurnQuote 记本轮的推理模型与报价（扣费之前）。只在 pending 时生效。
func (r *Repo) SetTurnQuote(ctx context.Context, id int64, modelCode string, credits int) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		update agent_turn set model_code = $2, credits_cost = $3
		where id = $1 and status = 'pending'`, id, nullStr(modelCode), credits)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// MarkTurnRunning CAS pending → running，记开始时间。
func (r *Repo) MarkTurnRunning(ctx context.Context, id int64) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		update agent_turn set status = 'running', started_at = now()
		where id = $1 and status = 'pending'`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ErrNotTerminal 给 FinishTurn 传了非终态。
var ErrNotTerminal = errors.New("轮次结束状态必须是终态")

// FinishTurn 把轮次结束到终态。CAS **只从 pending / running 出** ——
// 终态不被覆盖：重复消息、扫描循环与 Runner 同时结算时只有一方生效，
// 调用方据返回值决定要不要退款（不看的话就退两次）。
func (r *Repo) FinishTurn(ctx context.Context, id int64, status domain.TurnStatus,
	callCount, validOutputs int, errCode, errMsg string) (bool, error) {
	if !status.IsTerminal() {
		return false, ErrNotTerminal
	}
	tag, err := r.pool.Exec(ctx, `
		update agent_turn set status = $2, call_count = $3, valid_outputs = $4,
			error_code = $5, error_message = left($6, 1024), finished_at = now()
		where id = $1 and status in ('pending', 'running')`,
		id, status, callCount, validOutputs, nullStr(errCode), nullStr(errMsg))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// StaleRunning 取「running 超过 olderThan **且会话租约已过期**」的轮次（扫描循环退款 + 标 failed）。
//
// ⚠️ 只看时长会误杀：调用上限 × 空闲超时恰好可能就是阈值，合法的长轮会被判死；
// 租约还有效说明 Worker 还活着在续，不能动它（dev/01 §4.3）。
func (r *Repo) StaleRunning(ctx context.Context, olderThan time.Duration, limit int) ([]Turn, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.pool.Query(ctx, `
		select `+turnCols+` from agent_turn where id in (
			select t.id from agent_turn t
			join agent_session s on s.id = t.session_id
			where t.status = 'running'
			  and t.started_at < now() - $1::bigint * interval '1 millisecond'
			  and (s.runner_lease_until is null or s.runner_lease_until < now())
			order by t.started_at limit $2)
		order by started_at`, olderThan.Milliseconds(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Turn
	for rows.Next() {
		t, err := scanTurn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
