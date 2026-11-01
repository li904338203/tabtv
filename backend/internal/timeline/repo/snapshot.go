package repo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// 时间线快照（docs/agent/smartcut SCD-18、dev/02 §1.1 timeline_snapshot）。
//
// 快照存的是**写入前**的完整内容（与整份保存的请求体同形），恢复时走整份保存的同一条写路径，
// 不需要另一套解析。快照不算素材引用（SCK-18）：媒体回收不看这张表。

// MaxSnapshots 每条时间线保留的快照份数（SC-PARAM，设计建议值）。
const MaxSnapshots = 20

// 快照原因（00067 的 ck_snapshot_reason）。
const (
	SnapshotAIPlan         = "ai_plan"
	SnapshotAIEdit         = "ai_edit"
	SnapshotAIVariant      = "ai_variant"
	SnapshotClipFill       = "clip_fill"
	SnapshotReplaceChanged = "replace_changed"
	SnapshotSubtitles      = "subtitles"
	SnapshotTranslate      = "translate"
	SnapshotRestore        = "restore"
)

// SnapshotWrite 一份要存的快照。Version 是写入前的时间线版本。
type SnapshotWrite struct {
	ID           int64
	Version      int
	Content      json.RawMessage
	Reason       string
	CallID       int64
	GenerationID int64
	Editor       Editor
}

// Snapshot 快照的元信息（Content 只在取单份时带）。
type Snapshot struct {
	ID            int64
	TimelineID    int64
	Version       int
	Reason        string
	CallID        int64
	GenerationID  int64
	EditorKind    string
	CreatedByName string
	CreatedAt     time.Time
	Content       json.RawMessage
}

// insertSnapshot 在写入的事务里存一份快照，并删掉超出份数的旧快照。
//
// ⚠️ 删旧的与插新的在同一事务：分开做的话，删的那一步失败会让快照无限增长，而它每份都是一整条时间线。
func insertSnapshot(ctx context.Context, tx pgx.Tx, timelineID int64, s SnapshotWrite) error {
	if _, err := tx.Exec(ctx, `
		insert into timeline_snapshot (id, timeline_id, version, content, reason, call_id, generation_id,
			created_by_user_id, editor_kind)
		values ($1,$2,$3,$4::jsonb,$5,nullif($6,0::bigint),nullif($7,0::bigint),nullif($8,0::bigint),nullif($9,''))`,
		s.ID, timelineID, s.Version, string(s.Content), s.Reason, s.CallID, s.GenerationID,
		s.Editor.UserID, s.Editor.Kind); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		delete from timeline_snapshot where timeline_id = $1 and id not in (
			select id from timeline_snapshot where timeline_id = $1 order by id desc limit $2)`,
		timelineID, MaxSnapshots)
	return err
}

// ListSnapshots 快照列表（不含正文），新的在前。
func (r *Repo) ListSnapshots(ctx context.Context, timelineID int64) ([]Snapshot, error) {
	rows, err := r.pool.Query(ctx, `
		select s.id, s.timeline_id, s.version, s.reason, coalesce(s.call_id,0), coalesce(s.generation_id,0),
		       coalesce(s.editor_kind,''), coalesce(u.nickname,''), s.created_at
		from timeline_snapshot s left join app_user u on u.id = s.created_by_user_id
		where s.timeline_id = $1 order by s.id desc`, timelineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Snapshot
	for rows.Next() {
		var s Snapshot
		if err := rows.Scan(&s.ID, &s.TimelineID, &s.Version, &s.Reason, &s.CallID, &s.GenerationID,
			&s.EditorKind, &s.CreatedByName, &s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetSnapshot 取一份快照（含正文），同时校验它属于这条时间线。
func (r *Repo) GetSnapshot(ctx context.Context, timelineID, snapshotID int64) (Snapshot, error) {
	var s Snapshot
	err := r.pool.QueryRow(ctx, `
		select id, timeline_id, version, reason, coalesce(call_id,0), coalesce(generation_id,0),
		       coalesce(editor_kind,''), created_at, content
		from timeline_snapshot where id = $1 and timeline_id = $2`, snapshotID, timelineID).
		Scan(&s.ID, &s.TimelineID, &s.Version, &s.Reason, &s.CallID, &s.GenerationID, &s.EditorKind,
			&s.CreatedAt, &s.Content)
	if errors.Is(err, pgx.ErrNoRows) {
		return Snapshot{}, ErrNotFound
	}
	return s, err
}
