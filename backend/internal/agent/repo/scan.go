package repo

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// 扫描循环（AGT-25，dev/01 §4.3）用到的查询。
//
// ⚠️ 这里只负责「捞出候选」，不做任何判定：真正的推进在 app/scanner.go 里
// **锁住会话行后重新读一遍再做**（CAS / for update）—— 候选查询与处理之间
// 别的实例、Runner、API 都可能已经动过它，多实例同时扫描的安全全靠那一步。
//
// ⚠️ 「最近已经投过」用 outbox_message 判（idx_outbox_aggregate）而不是另加一列：
// 一轮刚建好（API 写了 outbox、投递器还没发出去 / Worker 还没取到）时租约也是空的，
// 不看它的话扫描会给每个新轮多投一条；Worker 全忙时它也让重投节流成「每 quiet 一次」，
// 而不是每 2 秒往队列里再塞一条。

const recentOutboxCond = `not exists (select 1 from outbox_message o
	where o.aggregate_type = 'agent' and o.aggregate_id = s.id
	  and o.created_at > now() - $1::bigint * interval '1 millisecond')`

// LeaseFreeRunning 取「运行态（planning / executing）而租约为空或已过期」的会话（扫描 B）。
//
// 走 idx_agent_session_scan。quiet 内有过更新或投递的跳过（见文件头）。
func (r *Repo) LeaseFreeRunning(ctx context.Context, quiet time.Duration, limit int) ([]Session, error) {
	return r.querySessions(ctx, `select `+sessionCols+` from agent_session s
		where s.state in ('planning', 'executing')
		  and (s.runner_lease_until is null or s.runner_lease_until < now())
		  and s.updated_at < now() - $1::bigint * interval '1 millisecond'
		  and `+recentOutboxCond+`
		order by s.last_active_at, s.id limit $2`, quiet.Milliseconds(), limit)
}

// QueuedTurnSessions 取「有 pending 轮、却不在 planning / executing、且没人持有租约」的会话。
//
// 来源：运行中发的消息排成下一轮，那条队列消息被 Runner 当作 Busy 丢弃（AGT-22 已知缺口），
// 或在上一轮结束、会话已回 idle / 挂起之后才被取到。这些轮**没有任何人会再投它**。
// 走 idx_agent_turn_pending（00044）。
func (r *Repo) QueuedTurnSessions(ctx context.Context, quiet time.Duration, limit int) ([]Session, error) {
	return r.querySessions(ctx, `select `+sessionCols+` from agent_session s
		where s.id in (
			select t.session_id from agent_turn t
			where t.status = 'pending'
			  and t.created_at < now() - $1::bigint * interval '1 millisecond')
		  and s.state not in ('planning', 'executing')
		  and (s.runner_lease_until is null or s.runner_lease_until < now())
		  and `+recentOutboxCond+`
		order by s.last_active_at, s.id limit $2`, quiet.Milliseconds(), limit)
}

// WaitingTaskSessions 取 waiting_task 且没人持有租约、也没有未结束轮次的会话（扫描 A 的候选）。
//
// 有 pending / running 轮的交给 B / 排队轮处理：那一轮跑完时 Runner 会按「是否仍有等待步骤」
// 自己回到 waiting_task，这里不抢着再建一个系统轮。走 idx_agent_session_scan。
func (r *Repo) WaitingTaskSessions(ctx context.Context, limit int) ([]Session, error) {
	return r.querySessions(ctx, `select `+sessionCols+` from agent_session s
		where s.state = 'waiting_task'
		  and (s.runner_lease_until is null or s.runner_lease_until < now())
		  and not exists (select 1 from agent_turn t
		                  where t.session_id = s.id and t.status in ('pending', 'running'))
		order by s.last_active_at, s.id limit $1`, limit)
}

// LeaseFree 报告会话租约是否为空或已过期（在锁住会话行之后复核用）。
func LeaseFree(s Session, now time.Time) bool {
	return s.RunnerLeaseUntil == nil || s.RunnerLeaseUntil.Before(now)
}

// DBNow 取数据库时钟（租约是按库里的 now() 写的，复核也要用同一个钟）。
func (r *Repo) DBNow(ctx context.Context, q Querier) (time.Time, error) {
	var t time.Time
	err := q.QueryRow(ctx, `select now()`).Scan(&t)
	return t, err
}

// UnfinishedTurn 取会话里该先跑的那一轮：includeRunning=true 时 running 的优先（崩溃接手），
// 否则只看 pending；同类取最早的。没有返回 ErrNotFound。
func (r *Repo) UnfinishedTurn(ctx context.Context, q Querier, sessionID int64, includeRunning bool) (Turn, error) {
	return scanTurn(q.QueryRow(ctx, `select `+turnCols+` from agent_turn
		where session_id = $1 and (status = 'pending' or ($2 and status = 'running'))
		order by (status = 'running') desc, created_at, id limit 1`, sessionID, includeRunning))
}

// RecentOutbox 报告会话在 quiet 内是否已经写过 outbox（锁住会话行之后复核，
// 防止两个扫描实例先后拿到同一个候选、各投一条）。
func (r *Repo) RecentOutbox(ctx context.Context, q Querier, sessionID int64, quiet time.Duration) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `select exists (select 1 from outbox_message o
		where o.aggregate_type = 'agent' and o.aggregate_id = $1
		  and o.created_at > now() - $2::bigint * interval '1 millisecond')`,
		sessionID, quiet.Milliseconds()).Scan(&ok)
	return ok, err
}

// RefundCandidate 是一条可能要归还额度的条目（扫描 E）。
type RefundCandidate struct {
	EntryID      int64
	SessionID    int64
	WorkspaceID  int64
	ProjectID    int64
	StepID       int64
	TurnID       int64
	Tool         string
	StepStatus   string
	GenerationID int64
}

// ChargedRefundCandidates 取「额度条目 charged、关联了生成任务，且步骤已失败 / 取消，
// 或步骤还在等待而会话已不在 waiting_task（A 不会再替它回写）」的条目。
//
// ⚠️ 只看最近 7 天（同 generation/repo.FindUnrefundedFailures）：正常情况下几秒内就归还了，
// 更早的要人工核对；不加窗口的话成功任务的 charged 条目永远留在扫描范围里。
// 走 idx_agent_budget_charged（00044）。
func (r *Repo) ChargedRefundCandidates(ctx context.Context, limit int) ([]RefundCandidate, error) {
	rows, err := r.pool.Query(ctx, `
		select e.id, e.session_id, s.workspace_id, s.project_id, st.id, st.turn_id, st.tool, st.status,
		       e.generation_id
		from agent_budget_entry e
		join agent_step st on st.id = e.step_id
		join agent_session s on s.id = e.session_id
		where e.status = 'charged' and e.generation_id is not null
		  and e.updated_at > now() - interval '7 days'
		  and (st.status in ('failed', 'cancelled')
		       or (st.status = 'waiting' and s.state <> 'waiting_task'))
		order by e.updated_at, e.id limit $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RefundCandidate
	for rows.Next() {
		var c RefundCandidate
		if err := rows.Scan(&c.EntryID, &c.SessionID, &c.WorkspaceID, &c.ProjectID, &c.StepID, &c.TurnID,
			&c.Tool, &c.StepStatus, &c.GenerationID); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// HasPendingCards 报告会话是否还有待处理的卡片（卡片过期后决定会话要不要回 idle）。
func (r *Repo) HasPendingCards(ctx context.Context, q Querier, sessionID int64) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `select exists (select 1 from agent_card
		where session_id = $1 and status = 'pending')`, sessionID).Scan(&ok)
	return ok, err
}

// WaitingStepsInTx 在事务里列出会话等待中的步骤（锁住会话行之后复核用）。
func (r *Repo) WaitingStepsInTx(ctx context.Context, tx pgx.Tx, sessionID int64) ([]Step, error) {
	rows, err := tx.Query(ctx, `select `+stepCols+` from agent_step
		where session_id = $1 and status = 'waiting' order by started_at, id`, sessionID)
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

func (r *Repo) querySessions(ctx context.Context, sql string, args ...any) ([]Session, error) {
	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// UnrefundedTurn 是一轮「失败 / 取消、扣过对话费、却没有退款流水」的轮次（补退款的候选）。
type UnrefundedTurn struct {
	TurnID         int64
	SessionID      int64
	PayerAccountID int64
	Credits        int
}

// UnrefundedTurns 取补退款的候选（04 文档 P-17）：失败 / 取消、扣过对话费、没有退款流水、
// 结束超过 settleAfter、7 天内。
//
// ⚠️ 起因：Runner 结算时退款失败只打一行日志（多半是数据库瞬时故障），而超时轮（C）、作废的排队轮（Q）
// 之外没有任何地方会再退 —— 钱扣着、轮次已判失败。照生成域的 FindUnrefundedFailures：只认账本
// （有 consume、没有 refund），不认 credits_cost（那是报价）。7 天之外的要人工核对。
// settleAfter 给 Runner 自己的结算留时间，免得与它同时退（账本唯一索引兜底，只是少一次无谓的冲突）。
func (r *Repo) UnrefundedTurns(ctx context.Context, settleAfter time.Duration, limit int) ([]UnrefundedTurn, error) {
	rows, err := r.pool.Query(ctx, `
		select t.id, t.session_id, s.payer_account_id, t.credits_cost
		from agent_turn t
		join agent_session s on s.id = t.session_id
		where t.status in ('failed', 'cancelled') and t.credits_cost > 0
		  and t.finished_at < now() - $1::bigint * interval '1 millisecond'
		  and t.finished_at > now() - interval '7 days'
		  and exists (select 1 from credit_transaction c
		              where c.ref_type = 'agent_turn' and c.ref_id = t.id::text and c.type = 'consume')
		  and not exists (select 1 from credit_transaction c
		                  where c.ref_type = 'agent_turn' and c.ref_id = t.id::text and c.type = 'refund')
		order by t.finished_at, t.id
		limit $2`, settleAfter.Milliseconds(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UnrefundedTurn
	for rows.Next() {
		var u UnrefundedTurn
		if err := rows.Scan(&u.TurnID, &u.SessionID, &u.PayerAccountID, &u.Credits); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
