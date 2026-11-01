package repo

import "context"

// 片段级生成进行中的标记（docs/agent/smartcut SCT-37，SC-F-138）。
//
// ⚠️ **不推进版本、不经过整份保存**：它不是一次编辑 —— 整份保存与逐项接口都不碰这一列（见 SaveClips 的说明），
// 推进版本的话剪辑台里正在自动保存的那一份会撞上冲突，而用户什么都没改。导出预检据此拦下「还在生成」的段。

// SetClipPending 记下这一段正在被这次生成替换。片段不在这条时间线上时返回 false。
func (r *Repo) SetClipPending(ctx context.Context, timelineID, clipID, generationID int64) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		update timeline_clip c set pending_generation_id = $3
		from timeline_track t
		where c.id = $2 and c.track_id = t.id and t.timeline_id = $1`, timelineID, clipID, generationID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ClearClipPending 清掉这一次生成的标记：只清 pending = generationID 的（期间又发起了一次的话不碰它）。
func (r *Repo) ClearClipPending(ctx context.Context, timelineID, clipID, generationID int64) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		update timeline_clip c set pending_generation_id = null
		from timeline_track t
		where c.id = $2 and c.track_id = t.id and t.timeline_id = $1 and c.pending_generation_id = $3`,
		timelineID, clipID, generationID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ClipPending 这一段现在挂着的生成（0 = 没有；片段不在这条时间线上也是 0）。
func (r *Repo) ClipPending(ctx context.Context, timelineID, clipID int64) (int64, error) {
	var id int64
	err := r.pool.QueryRow(ctx, `
		select coalesce(max(c.pending_generation_id), 0) from timeline_clip c
		join timeline_track t on t.id = c.track_id
		where c.id = $2 and t.timeline_id = $1`, timelineID, clipID).Scan(&id)
	return id, err
}
