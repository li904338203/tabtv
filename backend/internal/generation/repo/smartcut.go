package repo

import (
	"context"
	"strconv"
	"time"
)

// 智能剪辑名下的任务（docs/agent/smartcut SCT-33 / SCT-37）：按时间线列出、记下产物已经回填。

// SmartcutRow 一条智能剪辑名下的任务与它的第一个产物（还没有产物时为 0）。
type SmartcutRow struct {
	Generation
	OutputMediaID int64
}

// ListBySmartcutTimeline 一条时间线名下的识别与片段级生成，按受理时间倒序（游标是上一页最后一条的时间与 ID）。
// 归属在 SQL 里；索引 idx_generation_smartcut（`smartcut_target->>'timelineId'`）。
func (r *Repo) ListBySmartcutTimeline(ctx context.Context, workspaceID, timelineID int64, beforeAt time.Time, beforeID int64,
	limit int) ([]SmartcutRow, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	cursor := ""
	args := []any{workspaceID, strconv.FormatInt(timelineID, 10), limit}
	if beforeID > 0 {
		cursor = ` and (created_at, id) < ($4, $5)`
		args = append(args, beforeAt, beforeID)
	}
	rows, err := r.pool.Query(ctx, `
		with page as (`+selectCols+`
		  from generation
		  where workspace_id = $1 and smartcut_target is not null and smartcut_target->>'timelineId' = $2`+cursor+`
		  order by created_at desc, id desc limit $3)
		select page.*, coalesce((select o.media_object_id from generation_output o
		                          where o.generation_id = page.id and o.media_object_id is not null
		                          order by o.idx limit 1), 0)
		from page order by created_at desc, id desc`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SmartcutRow
	for rows.Next() {
		var row SmartcutRow
		if err := rows.Scan(append(scanTargets(&row.Generation), &row.OutputMediaID)...); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// MarkSmartcutLanded 记下这次任务的产物已经回填进时间线（回写目标里加 `landed: true`）。
func (r *Repo) MarkSmartcutLanded(ctx context.Context, id int64) error {
	_, err := r.pool.Exec(ctx, `update generation set smartcut_target = smartcut_target || '{"landed": true}'::jsonb
		where id = $1 and smartcut_target is not null`, id)
	return err
}
