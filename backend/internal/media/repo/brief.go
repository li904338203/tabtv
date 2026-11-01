package repo

import "context"

// Brief 是摘要用的媒体概况：类型（MIME）与封面帧（视频的 video_poster 派生图，没有为 0）。
type Brief struct {
	ContentType string
	PosterID    int64
}

// Briefs 一次查多份媒体的概况（智能剪辑节点卡的迷你时间轴与封面，docs/agent/smartcut SCT-07）。
//
// ⚠️ 带工作区条件：查不到的（不存在、不是你的、已删除）不出现在结果里 —— 与按 id 逐个取同一条归属规则。
// 封面帧取法与 PosterOf 一致（ListDerived 按 derive_index 排第一）。
func (r *Repo) Briefs(ctx context.Context, workspaceID int64, ids []int64) (map[int64]Brief, error) {
	out := map[int64]Brief{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `
		select m.id, m.content_type,
		       coalesce((select p.id from media_object p
		                 where p.source_media_id = m.id and p.derive_kind = 'video_poster'
		                   and coalesce(p.derive_key,'') = '' and p.status = 'active'
		                   and p.workspace_id = m.workspace_id
		                 order by p.derive_index, p.id limit 1), 0)
		from media_object m
		where m.workspace_id = $1 and m.id = any($2) and m.status = 'active'`, workspaceID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var b Brief
		if err := rows.Scan(&id, &b.ContentType, &b.PosterID); err != nil {
			return nil, err
		}
		out[id] = b
	}
	return out, rows.Err()
}

// Fact 是素材条要的媒体事实（智能剪辑 SCT-25）：归属、类型、时长、封面帧、末帧、有没有音轨。
type Fact struct {
	WorkspaceID int64
	ContentType string
	Status      string
	DurationMs  int
	PosterID    int64
	// LastFrameID 末帧（video_lastframe，补镜的参考图）；还没截出来为 0。
	LastFrameID int64
	// HasAudio 只在知道时非空：音频恒为有；视频看预览素材扫描器的波形结果（没做完就是不知道）。
	HasAudio *bool
}

// Facts 按 ID 查媒体事实，**不带工作区条件**：调用方要分清「平台素材」与「别的工作区 / 不存在」两种拒绝理由，
// 返回的 WorkspaceID 由调用方自己比对，不能直接把这里的结果给用户看。查不到的 ID 不在结果里。
func (r *Repo) Facts(ctx context.Context, ids []int64) (map[int64]Fact, error) {
	out := map[int64]Fact{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `
		select m.id, m.workspace_id, m.content_type, m.status, coalesce(m.duration_ms, 0),
		       coalesce((select p.id from media_object p
		                 where p.source_media_id = m.id and p.derive_kind = 'video_poster'
		                   and coalesce(p.derive_key,'') = '' and p.status = 'active'
		                   and p.workspace_id = m.workspace_id
		                 order by p.derive_index, p.id limit 1), 0),
		       coalesce((select p.id from media_object p
		                 where p.source_media_id = m.id and p.derive_kind = 'video_lastframe'
		                   and coalesce(p.derive_key,'') = '' and p.status = 'active'
		                   and p.workspace_id = m.workspace_id
		                 order by p.derive_index, p.id limit 1), 0),
		       (select coalesce((a.result->>'noAudio')::boolean, false)
		          from media_analysis a
		         where a.media_object_id = m.id and a.kind = 'peaks' and a.status = 'success'
		         order by a.created_at desc limit 1)
		from media_object m
		where m.id = any($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var f Fact
		var noAudio *bool
		if err := rows.Scan(&id, &f.WorkspaceID, &f.ContentType, &f.Status, &f.DurationMs, &f.PosterID,
			&f.LastFrameID, &noAudio); err != nil {
			return nil, err
		}
		switch {
		case len(f.ContentType) >= 6 && f.ContentType[:6] == "audio/":
			yes := true
			f.HasAudio = &yes
		case noAudio != nil:
			has := !*noAudio
			f.HasAudio = &has
		}
		out[id] = f
	}
	return out, rows.Err()
}
