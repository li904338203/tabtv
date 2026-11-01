package repo

import (
	"context"
	"time"
)

// CallLog 是 mcp_call_log 的一行（MCPT-06）。字段长度在 app 层已截到列宽。
type CallLog struct {
	ID              int64
	GrantID         int64
	UserID          int64
	WorkspaceID     int64
	ClientName      string
	Tool            string
	Method          string // 空 = 单 method 工具（存 null）
	ArgsFingerprint []byte
	ArgsSummary     string
	Credits         int
	Result          string
	ErrorCode       int // 0 = 没有错误码（存 null）
	DurationMS      int
	TraceID         string
	CreatedAt       time.Time
}

// InsertCallLog 写一条调用审计。
func (r *Repo) InsertCallLog(ctx context.Context, c CallLog) error {
	_, err := r.pool.Exec(ctx, `
		insert into mcp_call_log (id, grant_id, user_id, workspace_id, client_name, tool, method,
			args_fingerprint, args_summary, credits, result, error_code, duration_ms, trace_id, created_at)
		values ($1, $2, $3, $4, $5, $6, nullif($7, ''), $8, nullif($9, ''), $10, $11, nullif($12, 0),
			$13, nullif($14, ''), $15)`,
		c.ID, c.GrantID, c.UserID, c.WorkspaceID, c.ClientName, c.Tool, c.Method,
		c.ArgsFingerprint, c.ArgsSummary, c.Credits, c.Result, c.ErrorCode, c.DurationMS, c.TraceID, c.CreatedAt)
	return err
}

// CleanupCallLogBefore 删掉 before 之前的调用审计，**单次至多 limit 行**（分批：一次删几十万行
// 会长时间持锁，拖慢正在写日志的请求）。按 created_at 选行，走 idx_mcp_call_log_created。
func (r *Repo) CleanupCallLogBefore(ctx context.Context, before time.Time, limit int) (int64, error) {
	tag, err := r.pool.Exec(ctx, `
		delete from mcp_call_log
		where id in (select id from mcp_call_log
		             where created_at < $1 order by created_at limit $2)`, before, limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
