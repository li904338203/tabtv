package repo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// 异步导出任务的数据访问（NEW-15、GAP-12）。

// ErrExportBusy 表示任务已被别人领走或已是终态。
var ErrExportBusy = errors.New("导出任务不在可执行状态")

type ExportTask struct {
	ID          int64
	WorkspaceID int64
	UserID      int64
	Kind        string
	Status      string
	Items       json.RawMessage
	ItemCount   int
	MediaID     int64
	SizeBytes   int64
	ErrorCode   string
	ErrorMsg    string
	ExpiresAt   time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// InsertExport 在**一个事务里**写任务行与 outbox 消息。
//
// ⚠️ 两步分开写的话，写完任务行就崩溃会留下一个永远排队的导出 ——
// 与生成受理是同一条规则（GO-13）：写库成功则消息一定会发。
//
// ⚠️ outbox_message 是**平台表**（迁移 00013），不属于 generation 域。
// 已有的投递器（generation/app.Relay）认领时**不按 aggregate_type 过滤**，
// 所以这里写进去就会被它发出去，不需要第二个投递器。
func (r *Repo) InsertExport(ctx context.Context, t ExportTask,
	outboxID int64, topic string, payload json.RawMessage) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		insert into export_task
		  (id, workspace_id, user_id, kind, status, items, item_count, expires_at)
		values ($1,$2,$3,$4,'queued',$5::jsonb,$6,$7)`,
		t.ID, t.WorkspaceID, t.UserID, t.Kind, string(t.Items), t.ItemCount,
		t.ExpiresAt); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		insert into outbox_message (id, aggregate_type, aggregate_id, topic, payload)
		values ($1,'export',$2,$3,$4)`, outboxID, t.ID, topic, payload); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *Repo) GetExport(ctx context.Context, workspaceID, id int64) (ExportTask, error) {
	var t ExportTask
	err := r.pool.QueryRow(ctx, `
		select id, workspace_id, user_id, kind, status, items, item_count,
		       coalesce(media_object_id,0), coalesce(size_bytes,0),
		       coalesce(error_code,''), coalesce(error_message,''),
		       expires_at, created_at, updated_at
		from export_task where id = $1 and workspace_id = $2`, id, workspaceID).
		Scan(&t.ID, &t.WorkspaceID, &t.UserID, &t.Kind, &t.Status, &t.Items, &t.ItemCount,
			&t.MediaID, &t.SizeBytes, &t.ErrorCode, &t.ErrorMsg,
			&t.ExpiresAt, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ExportTask{}, ErrNotFound
	}
	return t, err
}

// GetExportForRun 供 Worker 按 id 取任务（不带工作区：消息里只有任务 id）。
func (r *Repo) GetExportForRun(ctx context.Context, id int64) (ExportTask, error) {
	var t ExportTask
	err := r.pool.QueryRow(ctx, `
		select id, workspace_id, user_id, kind, status, items, item_count,
		       coalesce(media_object_id,0), coalesce(size_bytes,0),
		       coalesce(error_code,''), coalesce(error_message,''),
		       expires_at, created_at, updated_at
		from export_task where id = $1`, id).
		Scan(&t.ID, &t.WorkspaceID, &t.UserID, &t.Kind, &t.Status, &t.Items, &t.ItemCount,
			&t.MediaID, &t.SizeBytes, &t.ErrorCode, &t.ErrorMsg,
			&t.ExpiresAt, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ExportTask{}, ErrNotFound
	}
	return t, err
}

func (r *Repo) ListExports(ctx context.Context, workspaceID int64, limit int,
	cursorID int64) ([]ExportTask, error) {
	sql := `select id, workspace_id, user_id, kind, status, items, item_count,
	               coalesce(media_object_id,0), coalesce(size_bytes,0),
	               coalesce(error_code,''), coalesce(error_message,''),
	               expires_at, created_at, updated_at
	        from export_task where workspace_id = $1`
	args := []any{workspaceID}
	if cursorID > 0 {
		args = append(args, cursorID)
		sql += ` and id < $2`
	}
	args = append(args, limit)
	if cursorID > 0 {
		sql += ` order by id desc limit $3`
	} else {
		sql += ` order by id desc limit $2`
	}
	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExportTask
	for rows.Next() {
		var t ExportTask
		if err := rows.Scan(&t.ID, &t.WorkspaceID, &t.UserID, &t.Kind, &t.Status,
			&t.Items, &t.ItemCount, &t.MediaID, &t.SizeBytes, &t.ErrorCode,
			&t.ErrorMsg, &t.ExpiresAt, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ClaimExport 领取任务：CAS `queued → running`。
//
// ⚠️ 消费幂等全靠这一步：at-least-once 投递意味着同一条消息会重复到达，
// 没有 CAS 的话同一个导出会被打两遍包，两份都往对象存储里传。
func (r *Repo) ClaimExport(ctx context.Context, id int64) (ExportTask, error) {
	tag, err := r.pool.Exec(ctx, `
		update export_task set status = 'running', updated_at = now()
		where id = $1 and status = 'queued'`, id)
	if err != nil {
		return ExportTask{}, err
	}
	if tag.RowsAffected() == 0 {
		return ExportTask{}, ErrExportBusy
	}
	return r.GetExportForRun(ctx, id)
}

// FinishExport 标记成功并记下包本身。
//
// ⚠️ 带 `status='running'` 条件：晚到的结果不能覆盖已经被人工置失败的任务。
func (r *Repo) FinishExport(ctx context.Context, id, mediaID, size int64) error {
	tag, err := r.pool.Exec(ctx, `
		update export_task
		set status = 'success', media_object_id = $2, size_bytes = $3, updated_at = now()
		where id = $1 and status = 'running'`, id, mediaID, size)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrExportBusy
	}
	return nil
}

func (r *Repo) FailExport(ctx context.Context, id int64, code, msg string) error {
	_, err := r.pool.Exec(ctx, `
		update export_task
		set status = 'failed', error_code = $2, error_message = left($3,1024), updated_at = now()
		where id = $1 and status in ('queued','running')`, id, code, msg)
	return err
}

// ResetExport 把任务放回排队。用于「消费到但还不能执行」的情形。
func (r *Repo) ResetExport(ctx context.Context, id int64) error {
	_, err := r.pool.Exec(ctx, `
		update export_task set status = 'queued', updated_at = now()
		where id = $1 and status = 'running'`, id)
	return err
}

// ExpiredExport 是一条待清理的过期包。
type ExpiredExport struct {
	TaskID    int64
	MediaID   int64
	Bucket    string
	ObjectKey string
}

// FindExpiredExports 找出已过期、包还在的导出。
//
// ⚠️ 清理的是**包**不是任务行：任务行留着，用户点开历史看到的是
// "已过期"而不是"这次导出从没发生过"。
func (r *Repo) FindExpiredExports(ctx context.Context, now time.Time, limit int) (
	[]ExpiredExport, error) {
	rows, err := r.pool.Query(ctx, `
		select t.id, m.id, m.bucket, m.object_key
		from export_task t
		join media_object m on m.id = t.media_object_id
		where t.status = 'success' and t.media_object_id is not null
		  and t.expires_at < $1
		order by t.expires_at
		limit $2`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExpiredExport
	for rows.Next() {
		var e ExpiredExport
		if err := rows.Scan(&e.TaskID, &e.MediaID, &e.Bucket, &e.ObjectKey); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ClearExportMedia 在对象与媒体行删掉之后，把任务上的引用清空。
func (r *Repo) ClearExportMedia(ctx context.Context, taskID int64) error {
	_, err := r.pool.Exec(ctx,
		`update export_task set media_object_id = null, updated_at = now() where id = $1`, taskID)
	return err
}
