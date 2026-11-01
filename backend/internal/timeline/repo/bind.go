package repo

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/li904338203/tabtv/backend/internal/timeline/domain"
)

// 智能剪辑节点与时间线的绑定（docs/agent/smartcut SCT-07，SCD-20 / SCD-21）。
//
// 一个画布节点 ↔ 一条有效时间线（uk_timeline_node(project_id, canvas_node_id) where status='editing'）。
// 复制节点得到独立的一条（分叉），复制项目时随项目复制，清理项目时随项目删除（SCB-11）。

// GetByNode 按（项目，节点）取有效时间线。
func (r *Repo) GetByNode(ctx context.Context, workspaceID, projectID int64, nodeID string) (Timeline, error) {
	return scanTimeline(r.pool.QueryRow(ctx, timelineCols+`
		where workspace_id = $1 and project_id = $2 and canvas_node_id = $3 and status = 'editing'`,
		workspaceID, projectID, nodeID))
}

// ErrBindConflict 绑定冲突：时间线已经绑在别的节点上，或节点已经有了一条有效时间线。
var ErrBindConflict = errors.New("时间线或节点已经有绑定")

// BindNode 把一条还没绑节点的有效时间线绑到画布节点上（docs/agent/smartcut SCT-41，SCD-39）：Agent 的 timeline_assemble
// 让它建 / 用的脚本时间线出现在画布上 —— 不绑的话那条时间线用户在界面上看不到（SCK-19）。
//
// 只改 canvas_node_id，不推进版本（内容一点没变，推进版本会让开着剪辑台的人撞一次假冲突）。已经绑在这个节点上的算成功
// （重试）；绑在别的节点上、或这个节点已有时间线（uk_timeline_node）回 ErrBindConflict；不在本项目回 ErrNotFound。
func (r *Repo) BindNode(ctx context.Context, workspaceID, projectID, timelineID int64, nodeID string) error {
	tag, err := r.pool.Exec(ctx, `
		update timeline set canvas_node_id = $4, updated_at = now()
		where id = $1 and workspace_id = $2 and project_id = $3 and status = 'editing' and canvas_node_id is null`,
		timelineID, workspaceID, projectID, nodeID)
	if isUniqueViolation(err) {
		return ErrBindConflict
	}
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var current string
	err = r.pool.QueryRow(ctx, `
		select coalesce(canvas_node_id, '') from timeline
		where id = $1 and workspace_id = $2 and project_id = $3 and status = 'editing'`,
		timelineID, workspaceID, projectID).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if current == nodeID {
		return nil
	}
	return ErrBindConflict
}

// CreateForNode 为画布节点建时间线：三条默认轨（画面 / 音频 / 字幕），`is_arranged=false`
// （还没剪过：连进来的素材会自动铺上轨道，SCD-17）。
//
// 按（项目，节点）幂等：已有有效时间线时返回那一条与 false。并发两次建（两个标签页、网络重试）由唯一索引兜底，
// 后到的那个拿回先建的那条。
func (r *Repo) CreateForNode(ctx context.Context, t Timeline) (Timeline, bool, error) {
	return r.createForNode(ctx, t, 0)
}

// ForkForNode 为画布节点分叉一份 src（复制、粘贴出来的节点，SC-F-07）：轨道、片段、字幕、文字一并复制。
// 已有有效时间线时返回那一条与 false，不复制。
func (r *Repo) ForkForNode(ctx context.Context, srcID int64, t Timeline) (Timeline, bool, error) {
	return r.createForNode(ctx, t, srcID)
}

func (r *Repo) createForNode(ctx context.Context, t Timeline, srcID int64) (Timeline, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Timeline{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	t.ID = r.nextID()
	t.Version = 1
	tag, err := tx.Exec(ctx, `
		insert into timeline (id, workspace_id, project_id, name, version, aspect_ratio, fps, width, height,
			status, canvas_node_id, is_arranged, planned_fingerprint, synced_fingerprint)
		values ($1,$2,$3,$4,1,$5,$6,$7,$8,'editing',$9,$10,nullif($11,''),nullif($12,''))
		on conflict (project_id, canvas_node_id) where canvas_node_id is not null and status = 'editing'
		do nothing`,
		t.ID, t.WorkspaceID, t.ProjectID, t.Name, t.AspectRatio, t.FPS, t.Width, t.Height,
		t.CanvasNodeID, t.IsArranged, t.PlannedFingerprint, t.SyncedFingerprint)
	if err != nil {
		return Timeline{}, false, err
	}
	if tag.RowsAffected() == 0 {
		_ = tx.Rollback(ctx)
		existing, err := r.GetByNode(ctx, t.WorkspaceID, t.ProjectID, t.CanvasNodeID)
		return existing, false, err
	}
	if srcID == 0 {
		for _, tr := range []struct{ kind, name string }{
			{domain.TrackVideo, "画面"}, {domain.TrackAudio, "音频"}, {domain.TrackSubtitle, "字幕"},
		} {
			if _, err := tx.Exec(ctx, `insert into timeline_track (id, timeline_id, kind, idx, name)
				values ($1,$2,$3,1,$4)`, r.nextID(), t.ID, tr.kind, tr.name); err != nil {
				return Timeline{}, false, err
			}
		}
	} else if err := copyContentTx(ctx, tx, r.nextID, srcID, t.ID); err != nil {
		return Timeline{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Timeline{}, false, err
	}
	got, err := r.Get(ctx, t.WorkspaceID, t.ID)
	return got, true, err
}

// CopyForProjectInTx 复制项目时（SC-F-170）：源项目里绑定节点的有效时间线逐条复制到新项目，节点 ID 不变
// （副本画布的节点 ID 与源项目相同）。返回「源时间线 → 副本时间线」供改写画布里的 params.timelineId。
//
// ⚠️ 在**调用方的事务里**做（canvas/repo.InsertCopy）：分开做的话中间失败会留下指向源项目时间线的副本节点
// （同导演台 DRD-30）。版本从 1 起、不带最后编辑者；剪过与否、素材指纹原样复制。
func (r *Repo) CopyForProjectInTx(ctx context.Context, tx pgx.Tx, workspaceID, srcProjectID,
	dstProjectID int64) (map[int64]int64, error) {
	rows, err := tx.Query(ctx, `
		select id from timeline
		where workspace_id = $1 and project_id = $2 and canvas_node_id is not null and status = 'editing'
		order by id`, workspaceID, srcProjectID)
	if err != nil {
		return nil, err
	}
	srcIDs, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, err
	}
	ids := make(map[int64]int64, len(srcIDs))
	for _, srcID := range srcIDs {
		newID := r.nextID()
		if _, err := tx.Exec(ctx, `
			insert into timeline (id, workspace_id, project_id, name, version, aspect_ratio, fps, width, height,
				status, canvas_node_id, is_arranged, planned_fingerprint, synced_fingerprint)
			select $1, workspace_id, $2, name, 1, aspect_ratio, fps, width, height,
				'editing', canvas_node_id, is_arranged, planned_fingerprint, synced_fingerprint
			from timeline where id = $3`, newID, dstProjectID, srcID); err != nil {
			return nil, err
		}
		if err := copyContentTx(ctx, tx, r.nextID, srcID, newID); err != nil {
			return nil, err
		}
		ids[srcID] = newID
	}
	return ids, nil
}

// copyContentTx 把 src 的轨道、片段、字幕、文字复制到 dst（dst 的时间线行已经插好）。
//
// 新片段一律新 ID、**剥掉 source_shot_id**（复制出来的片段不撞 uk_clip_shot、也不再是「第 N 镜自动装配出来的那一段」）、
// 不带进行中的生成（生成只回填原片段）；字幕锚点里的片段 ID 换成副本的片段 ID（不换的话锚点指回源时间线，
// 副本的字幕跟着别人的片段走）。不复制快照、导出任务与 AI 调用记录。
func copyContentTx(ctx context.Context, tx pgx.Tx, nextID func() int64, srcID, dstID int64) error {
	trackMap := map[int64]int64{}
	rows, err := tx.Query(ctx, `select id from timeline_track where timeline_id = $1 order by kind, idx`, srcID)
	if err != nil {
		return err
	}
	trackIDs, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	for _, old := range trackIDs {
		nw := nextID()
		trackMap[old] = nw
		if _, err := tx.Exec(ctx, `
			insert into timeline_track (id, timeline_id, kind, idx, name, is_muted, volume, role, lang, is_hidden, style)
			select $1, $2, kind, idx, name, is_muted, volume, role, lang, is_hidden, style
			from timeline_track where id = $3`, nw, dstID, old); err != nil {
			return err
		}
	}

	clipMap := map[string]string{}
	rows, err = tx.Query(ctx, `
		select c.id, c.track_id, c.anchor from timeline_clip c join timeline_track k on k.id = c.track_id
		where k.timeline_id = $1 order by c.track_id, c.idx`, srcID)
	if err != nil {
		return err
	}
	type clipRef struct {
		id, track, copied int64
		anchor            []byte
	}
	clips, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (clipRef, error) {
		var c clipRef
		err := row.Scan(&c.id, &c.track, &c.anchor)
		return c, err
	})
	if err != nil {
		return err
	}
	for i, c := range clips {
		nw := nextID()
		clips[i].copied = nw
		clipMap[strconv.FormatInt(c.id, 10)] = strconv.FormatInt(nw, 10)
		if _, err := tx.Exec(ctx, `
			insert into timeline_clip (id, track_id, idx, media_object_id, source_in_ms, source_out_ms,
				start_ms, duration_ms, transition_in, transition_out, speed, volume, fade_in_ms, fade_out_ms,
				transform, origin, source_node_id, alternates)
			select $1, $2, idx, media_object_id, source_in_ms, source_out_ms,
				start_ms, duration_ms, transition_in, transition_out, speed, volume, fade_in_ms, fade_out_ms,
				transform, origin, source_node_id, alternates
			from timeline_clip where id = $3`, nw, trackMap[c.track], c.id); err != nil {
			return err
		}
	}
	// 跟随片段的音频（SC-F-56）：锚点指向的片段换成复制出来的那一段。放在所有片段都复制完之后 ——
	// 锚点指向的画面片段可能排在后面，边复制边改写会漏掉。
	for _, c := range clips {
		if a := remapAnchor(c.anchor, clipMap); a != nil {
			if _, err := tx.Exec(ctx, `update timeline_clip set anchor = $2::jsonb where id = $1`, c.copied, a); err != nil {
				return err
			}
		}
	}

	rows, err = tx.Query(ctx, `
		select id, track_id, start_ms, end_ms, text, coalesce(style,'{}'::jsonb), anchor
		from timeline_subtitle where timeline_id = $1`, srcID)
	if err != nil {
		return err
	}
	type subRow struct {
		track            int64
		start, end       int
		text             string
		style, anchorRaw []byte
	}
	subs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (subRow, error) {
		var s subRow
		var id int64
		err := row.Scan(&id, &s.track, &s.start, &s.end, &s.text, &s.style, &s.anchorRaw)
		return s, err
	})
	if err != nil {
		return err
	}
	for _, s := range subs {
		if _, err := tx.Exec(ctx, `
			insert into timeline_subtitle (id, timeline_id, track_id, start_ms, end_ms, text, style, anchor)
			values ($1,$2,$3,$4,$5,$6,$7::jsonb,$8::jsonb)`,
			nextID(), dstID, trackMap[s.track], s.start, s.end, s.text, string(s.style),
			remapAnchor(s.anchorRaw, clipMap)); err != nil {
			return err
		}
	}

	rows, err = tx.Query(ctx, `select id, track_id from timeline_text where timeline_id = $1`, srcID)
	if err != nil {
		return err
	}
	texts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (clipRef, error) {
		var c clipRef
		err := row.Scan(&c.id, &c.track)
		return c, err
	})
	if err != nil {
		return err
	}
	for _, x := range texts {
		if _, err := tx.Exec(ctx, `
			insert into timeline_text (id, timeline_id, track_id, start_ms, end_ms, text, style, box)
			select $1, $2, $3, start_ms, end_ms, text, style, box from timeline_text where id = $4`,
			nextID(), dstID, trackMap[x.track], x.id); err != nil {
			return err
		}
	}
	return nil
}

// remapAnchor 锚点里的片段 ID 换成副本的；指向的片段不在这条时间线上（失效锚点）原样留着。其余字段原样保留。
func remapAnchor(raw []byte, clipMap map[string]string) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	var ref string
	if json.Unmarshal(m["clipId"], &ref) == nil {
		if nw, ok := clipMap[ref]; ok {
			m["clipId"], _ = json.Marshal(nw)
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return string(out)
}

// PurgeProjectsInTx 清理项目时（SC-F-171、SCB-11）：删掉这些项目的全部时间线与子表（轨道、片段、字幕、文字）、
// 快照、AI 调用记录与导出任务。
//
// ⚠️ 在**调用方的事务里**做（canvas/repo.Purge / PurgeExpired），与删项目行同一个事务：此前项目清理不删时间线，
// 被清理项目的时间线片段永远挡住素材删除（检查器把它们算作「在用」）。
// ⚠️ 不删媒体：成片与生成产物照媒体域的规则回收（引用检查器不再看到这些时间线，自然放行）。
func (r *Repo) PurgeProjectsInTx(ctx context.Context, tx pgx.Tx, projectIDs []int64) error {
	if len(projectIDs) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `select id from timeline where project_id = any($1)`, projectIDs)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `delete from smartcut_llm_call where project_id = any($1)`, projectIDs); err != nil {
		return err
	}
	return deleteTimelinesTx(ctx, tx, ids)
}

// deleteTimelinesTx 删掉这些时间线及其全部从属行。
func deleteTimelinesTx(ctx context.Context, tx pgx.Tx, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	for _, q := range []string{
		`delete from timeline_clip where track_id in (select id from timeline_track where timeline_id = any($1))`,
		`delete from timeline_track where timeline_id = any($1)`,
		`delete from timeline_subtitle where timeline_id = any($1)`,
		`delete from timeline_text where timeline_id = any($1)`,
		`delete from timeline_snapshot where timeline_id = any($1)`,
		`delete from smartcut_llm_call where timeline_id = any($1)`,
		`delete from render_job where timeline_id = any($1)`,
		`delete from timeline where id = any($1)`,
	} {
		if _, err := tx.Exec(ctx, q, ids); err != nil {
			return err
		}
	}
	return nil
}

// SweepOrphans 清理孤儿时间线（SC-F-06、SC-PARAM-22）：绑定的节点已不在项目画布上（节点被删了），
// 或者所属项目已经不存在（清理修复之前留下的），且 updated_at 早于 before。一次最多 limit 条，返回删了几条。
//
// ⚠️ 判定只读该项目一行的 canvas_data，用 jsonb 运算（不在 Go 里整份解析画布）。回收站里的项目不算「不存在」：
// 它的节点还在画布上，恢复之后时间线照常可用。`for update skip locked`：多实例同时扫不会互相等。
func (r *Repo) SweepOrphans(ctx context.Context, before time.Time, limit int) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		select t.id from timeline t
		where t.canvas_node_id is not null and t.updated_at < $1
		  and not exists (
			select 1 from project p
			where p.id = t.project_id
			  and jsonb_path_exists(coalesce(p.canvas_data, '{}'::jsonb), '$.nodes[*] ? (@.id == $nid)',
			      jsonb_build_object('nid', t.canvas_node_id)))
		order by t.updated_at
		limit $2
		for update of t skip locked`, before, limit)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	if err := deleteTimelinesTx(ctx, tx, ids); err != nil {
		return 0, err
	}
	return len(ids), tx.Commit(ctx)
}

// ---------------------------------------------------------------- 摘要

// RenderBrief 一条时间线最近一次导出的概况（摘要的 latestRender）。
type RenderBrief struct {
	ID              int64
	Status          string
	Progress        int
	Stage           string
	OutputMediaID   int64
	TimelineVersion int
	Spec            json.RawMessage
	ErrorMessage    string
}

// NodeTimeline 摘要用的一条绑定时间线：时间线行 + 最近一次导出 + 最近一次**成功**导出的版本与成片。
type NodeTimeline struct {
	Timeline
	UpdatedAt      time.Time
	LatestRender   *RenderBrief
	SuccessVersion int
	SuccessMediaID int64
}

// NodeTimelines 本项目全部绑定节点的有效时间线，各带最近一次导出与最近一次成功导出（一条 SQL，distinct on）。
// 回收站里的项目也能读（节点卡在回收站预览里照常显示）。
func (r *Repo) NodeTimelines(ctx context.Context, workspaceID, projectID int64) ([]NodeTimeline, error) {
	rows, err := r.pool.Query(ctx, `
		with t as (
			select * from timeline
			where workspace_id = $1 and project_id = $2 and canvas_node_id is not null and status = 'editing'),
		latest as (
			select distinct on (j.timeline_id) j.timeline_id, j.id as rid, j.status as rstatus, j.progress as rprog,
				j.stage as rstage, j.output_media_id as rmedia, j.timeline_version as rver, j.spec as rspec,
				j.error_message as rerr
			from render_job j where j.timeline_id in (select id from t)
			order by j.timeline_id, j.created_at desc, j.id desc),
		ok as (
			select distinct on (j.timeline_id) j.timeline_id, j.timeline_version as over, j.output_media_id as omedia
			from render_job j where j.timeline_id in (select id from t) and j.status = 'success'
			order by j.timeline_id, j.created_at desc, j.id desc)
		select t.id, t.workspace_id, t.project_id, coalesce(t.script_id,0), t.name, t.version, t.aspect_ratio,
			t.fps, t.width, t.height, t.status, t.canvas_node_id, t.is_arranged, coalesce(t.last_editor_user_id,0),
			coalesce(t.last_editor_kind,''), coalesce(t.planned_fingerprint,''), coalesce(t.synced_fingerprint,''),
			t.updated_at,
			l.rid, coalesce(l.rstatus,''), coalesce(l.rprog,0), coalesce(l.rstage,''), coalesce(l.rmedia,0),
			coalesce(l.rver,0), l.rspec, coalesce(l.rerr,''),
			coalesce(ok.over, 0), coalesce(ok.omedia, 0)
		from t
		left join latest l on l.timeline_id = t.id
		left join ok on ok.timeline_id = t.id
		order by t.created_at, t.id`, workspaceID, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NodeTimeline
	for rows.Next() {
		var n NodeTimeline
		var rid *int64
		var b RenderBrief
		var rspec []byte
		if err := rows.Scan(&n.ID, &n.WorkspaceID, &n.ProjectID, &n.ScriptID, &n.Name, &n.Version, &n.AspectRatio,
			&n.FPS, &n.Width, &n.Height, &n.Status, &n.CanvasNodeID, &n.IsArranged, &n.LastEditorUserID,
			&n.LastEditorKind, &n.PlannedFingerprint, &n.SyncedFingerprint, &n.UpdatedAt,
			&rid, &b.Status, &b.Progress, &b.Stage, &b.OutputMediaID, &b.TimelineVersion, &rspec, &b.ErrorMessage,
			&n.SuccessVersion, &n.SuccessMediaID); err != nil {
			return nil, err
		}
		if rid != nil {
			b.ID = *rid
			b.Spec = domain.ObjectOrNil(rspec)
			n.LatestRender = &b
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// StripClips 摘要用的片段：各时间线的画面轨与第一条音频轨（按 idx 排序；落点由调用方用 domain 算）。
type StripClips struct {
	Video []domain.Clip
	Audio []domain.Clip
}

// StripClipsOf 一次取多条时间线的画面轨与第一条音频轨的片段。
func (r *Repo) StripClipsOf(ctx context.Context, timelineIDs []int64) (map[int64]StripClips, error) {
	out := map[int64]StripClips{}
	if len(timelineIDs) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `
		with first_audio as (
			select distinct on (timeline_id) timeline_id, id from timeline_track
			where timeline_id = any($1) and kind = 'audio' order by timeline_id, idx, id)
		select k.timeline_id, k.kind, c.id, c.idx, c.media_object_id, c.source_in_ms, c.source_out_ms,
			c.start_ms, c.speed, coalesce(c.transition_in,'null'::jsonb)
		from timeline_clip c join timeline_track k on k.id = c.track_id
		where k.timeline_id = any($1)
		  and (k.kind = 'video' or k.id in (select id from first_audio))
		order by k.timeline_id, k.kind, c.idx`, timelineIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var tid int64
		var kind string
		var c domain.Clip
		var tin json.RawMessage
		if err := rows.Scan(&tid, &kind, &c.ID, &c.Idx, &c.MediaID, &c.SourceInMs, &c.SourceOutMs,
			&c.StartMs, &c.Speed, &tin); err != nil {
			return nil, err
		}
		c.TransitionIn = domain.ParseTransition(tin)
		sc := out[tid]
		if kind == domain.TrackVideo {
			sc.Video = append(sc.Video, c)
		} else {
			sc.Audio = append(sc.Audio, c)
		}
		out[tid] = sc
	}
	return out, rows.Err()
}

// TimelineNodeIn 本项目里一条有效时间线绑定的节点 ID（按节点自愈用）。不在本项目、或没有绑定节点时 ErrNotFound。
func (r *Repo) TimelineNodeIn(ctx context.Context, workspaceID, projectID, timelineID int64) (string, error) {
	var node string
	err := r.pool.QueryRow(ctx, `
		select canvas_node_id from timeline
		where id = $1 and workspace_id = $2 and project_id = $3 and status = 'editing' and canvas_node_id is not null`,
		timelineID, workspaceID, projectID).Scan(&node)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return node, err
}
