package repo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// 画布操作日志（AGT-10，迁移 00038）。
//
// ⚠️ 服务端写入用**行锁**而不是乐观重试：服务端写入方只有 ApplyOpsTx 这一个，
// `for update` 让并发的 Agent 批次与真人保存天然排队；前端保存仍走原来的乐观锁
// （它撞上行锁只是等一下，提交后版本已变就照常 40902）。
// ⚠️ 行锁事务里**不得**有远程调用或发事件 —— 回调 fn 只允许做纯计算。

// OpLogEntry 是 canvas_op_log 的一行。
type OpLogEntry struct {
	ID             int64
	ProjectID      int64
	FromVersion    int
	ToVersion      int
	BatchID        string
	ActorKind      string
	AgentSessionID int64
	// McpGrantID actor_kind=mcp 时的授权 ID（MCPT-09，迁移 00052）；否则 0。
	McpGrantID int64
	UserID     int64
	Ops        json.RawMessage
	CreatedAt  time.Time
}

// OpWrite 是回调算出来要写的东西。
type OpWrite struct {
	LogID          int64
	CanvasData     json.RawMessage
	Ops            json.RawMessage
	ActorKind      string
	AgentSessionID int64
	McpGrantID     int64
	UserID         int64
}

const opLogCols = `id, project_id, from_version, to_version, batch_id, actor_kind,
	coalesce(agent_session_id, 0), coalesce(mcp_grant_id, 0), coalesce(user_id, 0), ops, created_at`

func scanOpLog(row pgx.Row) (OpLogEntry, error) {
	var e OpLogEntry
	err := row.Scan(&e.ID, &e.ProjectID, &e.FromVersion, &e.ToVersion, &e.BatchID, &e.ActorKind,
		&e.AgentSessionID, &e.McpGrantID, &e.UserID, &e.Ops, &e.CreatedAt)
	return e, err
}

// ApplyOpsTx 在一个事务里：锁住项目行 → 同批次幂等 → 回调计算新画布 → 版本 +1 → 写日志。
//
// replayed=true 表示这一批之前已经应用过，返回的是那一次记录下的结果（回调不会被调用）。
//
// ⚠️ 幂等检查放在**锁之后**：同一批次被并发提交两次时，第二个会等第一个提交，
// 再查就能看到它的日志行；放在锁之前的话两个都查不到、都去应用，
// 唯一索引 uk_canvas_op_batch 会让后一个报 23505 —— 那是兜底，不是正常路径。
// ⚠️ 条件带 workspace_id 与 status='active'：跨工作区与回收站里的项目一律 ErrNotFound。
func (r *Repo) ApplyOpsTx(ctx context.Context, workspaceID, projectID int64, batchID string,
	fn func(canvas json.RawMessage, version int) (OpWrite, error)) (OpLogEntry, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return OpLogEntry{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var canvas json.RawMessage
	var version int
	err = tx.QueryRow(ctx, `
		select coalesce(canvas_data, '{}'::jsonb), version from project
		where id = $1 and workspace_id = $2 and status = 'active'
		for update`, projectID, workspaceID).Scan(&canvas, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return OpLogEntry{}, false, ErrNotFound
	}
	if err != nil {
		return OpLogEntry{}, false, err
	}

	prev, err := scanOpLog(tx.QueryRow(ctx,
		`select `+opLogCols+` from canvas_op_log where project_id = $1 and batch_id = $2`,
		projectID, batchID))
	if err == nil {
		return prev, true, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return OpLogEntry{}, false, err
	}

	w, err := fn(canvas, version)
	if err != nil {
		return OpLogEntry{}, false, err
	}

	// ⚠️ 三列一起写（不是「只写本种的那一列」）：上一次是 Agent 写的、这一次是 MCP 写的，
	// 不把会话列清掉的话冲突判定会看到两种机器来源同时在场。
	var to int
	err = tx.QueryRow(ctx, `
		update project set canvas_data = $2, version = version + 1, updated_at = now(),
			last_editor_user_id = nullif($3::bigint, 0),
			last_editor_agent_session_id = nullif($4::bigint, 0),
			last_editor_mcp_grant_id = nullif($5::bigint, 0)
		where id = $1
		returning version`, projectID, w.CanvasData, w.UserID, w.AgentSessionID, w.McpGrantID).Scan(&to)
	if err != nil {
		return OpLogEntry{}, false, err
	}

	e := OpLogEntry{
		ID: w.LogID, ProjectID: projectID, FromVersion: version, ToVersion: to, BatchID: batchID,
		ActorKind: w.ActorKind, AgentSessionID: w.AgentSessionID, McpGrantID: w.McpGrantID,
		UserID: w.UserID, Ops: w.Ops,
	}
	err = tx.QueryRow(ctx, `
		insert into canvas_op_log (id, project_id, from_version, to_version, batch_id, actor_kind,
			agent_session_id, mcp_grant_id, user_id, ops)
		values ($1, $2, $3, $4, $5, $6, nullif($7::bigint, 0), nullif($8::bigint, 0), nullif($9::bigint, 0), $10)
		returning created_at`,
		e.ID, projectID, e.FromVersion, e.ToVersion, batchID, e.ActorKind,
		e.AgentSessionID, e.McpGrantID, e.UserID, e.Ops).Scan(&e.CreatedAt)
	if err != nil {
		return OpLogEntry{}, false, mapDuplicate(err)
	}
	return e, false, tx.Commit(ctx)
}

// OpsSince 返回项目当前版本与 (since, current] 区间内的操作批次（按版本升序，最多 limit 条）。
//
// 是否需要整页重载由调用方按「批次是否首尾相接地铺满整个区间」判断 ——
// 中间有真人整份保存、或批次已过保留期被清理，都会留下缺口。
func (r *Repo) OpsSince(ctx context.Context, workspaceID, projectID int64, since, limit int) (
	int, []OpLogEntry, error) {
	var current int
	err := r.pool.QueryRow(ctx, `
		select version from project where id = $1 and workspace_id = $2 and status = 'active'`,
		projectID, workspaceID).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, ErrNotFound
	}
	if err != nil {
		return 0, nil, err
	}
	if since >= current {
		return current, nil, nil
	}
	rows, err := r.pool.Query(ctx, `
		select `+opLogCols+` from canvas_op_log
		where project_id = $1 and to_version > $2 and to_version <= $3
		order by to_version limit $4`, projectID, since, current, limit)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	var out []OpLogEntry
	for rows.Next() {
		e, err := scanOpLog(rows)
		if err != nil {
			return 0, nil, err
		}
		out = append(out, e)
	}
	return current, out, rows.Err()
}

// CleanupOpLog 删掉 before 之前的操作日志，单次最多 limit 行。
//
// ⚠️ 分批：一次删几十万行会长时间持锁，把正在写日志的 Agent 批次堵住 ——
// 那时表现为「Agent 画布写入整体变慢」，看起来完全不像清理的问题。
func (r *Repo) CleanupOpLog(ctx context.Context, before time.Time, limit int) (int64, error) {
	tag, err := r.pool.Exec(ctx, `
		delete from canvas_op_log
		where id in (select id from canvas_op_log
		             where created_at < $1 order by created_at limit $2)`, before, limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
