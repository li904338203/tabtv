package repo

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/li904338203/tabtv/backend/internal/timeline/domain"
)

// 整份保存（docs/agent/smartcut SCT-05、SCD-15）：一个事务把整条时间线写成给定的样子。
//
// app 层已经做完：内容校验、素材归属与种类、落点（画面轨按顺序与转场、其它轨按起点）、
// 给新对象分配正式 ID、把引用临时 ID 的地方（字幕锚点、字幕与文字的所属轨道）改写成正式 ID。
// 这里只管写，以及**在锁住时间线之后**核对：内容里的正式 ID 确实是这条时间线上的对象。

// ContentWrite 是整份保存要写进库的东西。
type ContentWrite struct {
	TimelineID  int64
	Version     int
	Editor      Editor
	AspectRatio string
	FPS         int
	Width       int
	Height      int
	Tracks      []TrackWrite
	Subtitles   []SubtitleWrite
	Texts       []TextWrite
	// Snapshot 非空时先把「写入前」的完整内容存一份快照（同一事务：写入失败时快照也不留，SCD-18）。
	Snapshot *SnapshotWrite
	// Fingerprints 非空时同一事务里记下素材指纹（输入同步铺轨，SCT-25）。
	Fingerprints *FingerprintWrite
}

// FingerprintWrite 素材指纹（dev/01 §4.6）：Synced 是这次同步看到的；Planned 为 true 时同时写进
// planned_fingerprint（自动铺轨或 AI 剪辑之后，「剪辑依据的素材」就是这一份）。
type FingerprintWrite struct {
	Synced  string
	Planned bool
	// AfterPlan AI 剪辑（含方案切换、追加指令）写入之后：剪辑依据的素材就是这一份，planned 无条件写
	// （与自动铺轨不同 —— AI 剪辑本身就是一次「剪过」，不存在被它抹掉的用户剪辑）。
	AfterPlan bool
}

// TrackWrite 一条轨道。New = 新建（ID 由 app 分配）；Idx 是同种轨道里的顺序（1 起）。
type TrackWrite struct {
	Track
	New   bool
	Clips []ClipWrite
}

// ClipWrite 一个片段。Clip.Idx / StartMs / DurationMs 已算好。
//
// ⚠️ 新片段的 SourceShotID 由 app 剥掉（复制出来的片段不撞 uk_clip_shot）；已有片段的不更新（以库里为准），
// pending_generation_id 也不碰（见 SaveClips 的说明）。
type ClipWrite struct {
	Clip domain.Clip
	New  bool
}

// SubtitleWrite 一条字幕（TrackID 与锚点里的片段 ID 都已是正式 ID）。
type SubtitleWrite struct {
	Subtitle
	New bool
}

// TextWrite 一个文字层。
type TextWrite struct {
	Text domain.TextLayer
	New  bool
}

// ObjectError 内容里的正式 ID 对不上这条时间线：不是它上面的对象（或种类不对），或者改了轨道种类。
// app 按 ID 映射回字段路径。
type ObjectError struct {
	ID     int64
	Reason string
}

func (e *ObjectError) Error() string { return fmt.Sprintf("对象 %d：%s", e.ID, e.Reason) }

// SaveContent 一个事务写入整份内容，返回新版本号。
//
// ⚠️ 顺序：先抢版本（顺带锁住时间线行）→ 再核对正式 ID → 删 → 两阶段重编号 → 写。
// 核对放在抢到版本之后：在那之前读到的「这条时间线上有哪些片段」随时可能被别的保存改掉。
func (r *Repo) SaveContent(ctx context.Context, w ContentWrite) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	next, err := bumpVersion(ctx, tx, w.TimelineID, w.Version, w.Editor)
	if err != nil {
		return 0, err
	}
	if w.Snapshot != nil {
		if err := insertSnapshot(ctx, tx, w.TimelineID, *w.Snapshot); err != nil {
			return 0, err
		}
	}
	if w.Fingerprints != nil {
		if err := setFingerprints(ctx, tx, w.TimelineID, *w.Fingerprints); err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec(ctx, `
		update timeline set aspect_ratio = $2, fps = $3, width = $4, height = $5 where id = $1`,
		w.TimelineID, w.AspectRatio, w.FPS, w.Width, w.Height); err != nil {
		return 0, err
	}

	have, err := loadExisting(ctx, tx, w.TimelineID)
	if err != nil {
		return 0, err
	}
	keepTracks, keepClips, err := checkOwned(w, have)
	if err != nil {
		return 0, err
	}

	// 删：内容里没有的片段与轨道；字幕与文字整批替换。
	var dropClips, dropTracks []int64
	for id := range have.clips {
		if !keepClips[id] {
			dropClips = append(dropClips, id)
		}
	}
	for id := range have.tracks {
		if !keepTracks[id] {
			dropTracks = append(dropTracks, id)
		}
	}
	for _, q := range []struct {
		sql string
		arg any
	}{
		{`delete from timeline_clip where id = any($1::bigint[])`, dropClips},
		// 被删的轨道上可能还挂着没被内容提到的片段（它们已在 dropClips 里）；这里只删轨道本身。
		{`delete from timeline_track where id = any($1::bigint[])`, dropTracks},
		{`delete from timeline_subtitle where timeline_id = $1`, w.TimelineID},
		{`delete from timeline_text where timeline_id = $1`, w.TimelineID},
	} {
		if _, err := tx.Exec(ctx, q.sql, q.arg); err != nil {
			return 0, err
		}
	}

	// 两阶段重编号。⚠️ 涉及的**每条轨道**都要挪：跨轨移动时源轨与目标轨都在变，
	// 只挪一边的话会时有时无地撞 uk_track_idx / uk_clip_idx（PG 不延迟检查唯一索引）。
	// 临时区取 `least(0, min(idx)) - row_number()`：低于所有现存值，上一次失败留下的负数行也撞不上。
	if _, err := tx.Exec(ctx, `
		update timeline_track t set idx = s.tmp
		from (select id, least(0, (select min(idx) from timeline_track where timeline_id = $1))
		             - row_number() over (order by kind, idx, id) as tmp
		      from timeline_track where timeline_id = $1) s
		where t.id = s.id`, w.TimelineID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `
		update timeline_clip c set idx = s.tmp
		from (select c2.id,
		             least(0, (select min(c3.idx) from timeline_clip c3 where c3.track_id = c2.track_id))
		             - row_number() over (partition by c2.track_id order by c2.idx, c2.id) as tmp
		      from timeline_clip c2 join timeline_track k on k.id = c2.track_id
		      where k.timeline_id = $1) s
		where c.id = s.id`, w.TimelineID); err != nil {
		return 0, err
	}

	for _, tw := range w.Tracks {
		if err := writeTrack(ctx, tx, w.TimelineID, tw); err != nil {
			return 0, err
		}
		for _, cw := range tw.Clips {
			if err := writeClip(ctx, tx, tw.ID, cw); err != nil {
				return 0, err
			}
		}
	}
	for _, sw := range w.Subtitles {
		style := sw.Style
		if len(style) == 0 {
			style = json.RawMessage(`{}`)
		}
		if _, err := tx.Exec(ctx, `
			insert into timeline_subtitle (id, timeline_id, track_id, start_ms, end_ms, text, style, anchor)
			values ($1, $2, $3, $4, $5, $6, $7::jsonb, $8::jsonb)`,
			sw.ID, w.TimelineID, sw.TrackID, sw.StartMs, sw.EndMs, sw.Text, string(style),
			objectJSON(sw.Anchor)); err != nil {
			return 0, err
		}
	}
	for _, xw := range w.Texts {
		t := xw.Text
		if _, err := tx.Exec(ctx, `
			insert into timeline_text (id, timeline_id, track_id, start_ms, end_ms, text, style, box)
			values ($1, $2, $3, $4, $5, $6, $7::jsonb, $8::jsonb)`,
			t.ID, w.TimelineID, t.TrackID, t.StartMs, t.EndMs, t.Text,
			jsonOrEmptyObject(t.Style), jsonOrEmptyObject(t.Box)); err != nil {
			return 0, err
		}
	}
	return next, tx.Commit(ctx)
}

// existing 是锁住时间线之后读到的现状。
type existing struct {
	tracks    map[int64]string // 轨道 ID → 种类
	clips     map[int64]bool
	subtitles map[int64]bool
	texts     map[int64]bool
}

func loadExisting(ctx context.Context, tx pgx.Tx, timelineID int64) (existing, error) {
	have := existing{tracks: map[int64]string{}, clips: map[int64]bool{},
		subtitles: map[int64]bool{}, texts: map[int64]bool{}}
	rows, err := tx.Query(ctx, `select id, kind from timeline_track where timeline_id = $1`, timelineID)
	if err != nil {
		return have, err
	}
	for rows.Next() {
		var id int64
		var kind string
		if err := rows.Scan(&id, &kind); err != nil {
			rows.Close()
			return have, err
		}
		have.tracks[id] = kind
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return have, err
	}
	for _, q := range []struct {
		sql  string
		into map[int64]bool
	}{
		{`select c.id from timeline_clip c join timeline_track k on k.id = c.track_id where k.timeline_id = $1`, have.clips},
		{`select id from timeline_subtitle where timeline_id = $1`, have.subtitles},
		{`select id from timeline_text where timeline_id = $1`, have.texts},
	} {
		rows, err := tx.Query(ctx, q.sql, timelineID)
		if err != nil {
			return have, err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return have, err
			}
			q.into[id] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return have, err
		}
	}
	return have, nil
}

// checkOwned 内容里每个「已有」对象都必须是这条时间线上的、同种类的；返回要保留的轨道与片段。
//
// ⚠️ 不核对的话，客户端拿别的时间线（甚至别的工作区）的片段 ID 放进来，update 语句就会把那一行改掉 ——
// 越权写。字幕与文字虽然是整批替换，带着别人的 ID 插入也会撞主键或顶掉别处的行，同样要核。
func checkOwned(w ContentWrite, have existing) (map[int64]bool, map[int64]bool, error) {
	keepTracks := map[int64]bool{}
	keepClips := map[int64]bool{}
	for _, tw := range w.Tracks {
		if !tw.New {
			kind, ok := have.tracks[tw.ID]
			if !ok {
				return nil, nil, &ObjectError{ID: tw.ID, Reason: "轨道不属于这条时间线"}
			}
			if kind != tw.Kind {
				return nil, nil, &ObjectError{ID: tw.ID, Reason: "已有轨道不能改种类"}
			}
			keepTracks[tw.ID] = true
		}
		for _, cw := range tw.Clips {
			if !cw.New {
				if !have.clips[cw.Clip.ID] {
					return nil, nil, &ObjectError{ID: cw.Clip.ID, Reason: "片段不属于这条时间线"}
				}
				keepClips[cw.Clip.ID] = true
			}
		}
	}
	for _, sw := range w.Subtitles {
		if !sw.New && !have.subtitles[sw.ID] {
			return nil, nil, &ObjectError{ID: sw.ID, Reason: "字幕不属于这条时间线"}
		}
	}
	for _, xw := range w.Texts {
		if !xw.New && !have.texts[xw.Text.ID] {
			return nil, nil, &ObjectError{ID: xw.Text.ID, Reason: "文字层不属于这条时间线"}
		}
	}
	return keepTracks, keepClips, nil
}

func writeTrack(ctx context.Context, tx pgx.Tx, timelineID int64, tw TrackWrite) error {
	if tw.New {
		_, err := tx.Exec(ctx, `
			insert into timeline_track (id, timeline_id, kind, idx, name, is_muted, volume,
				role, lang, is_hidden, style)
			values ($1,$2,$3,$4,nullif($5,''),$6,$7,nullif($8,''),nullif($9,''),$10,$11::jsonb)`,
			tw.ID, timelineID, tw.Kind, tw.Idx, tw.Name, tw.IsMuted, tw.Volume,
			tw.Role, tw.Lang, tw.IsHidden, objectJSON(tw.Style))
		return err
	}
	_, err := tx.Exec(ctx, `
		update timeline_track set idx = $2, name = nullif($3,''), is_muted = $4, volume = $5,
			role = nullif($6,''), lang = nullif($7,''), is_hidden = $8, style = $9::jsonb
		where id = $1`,
		tw.ID, tw.Idx, tw.Name, tw.IsMuted, tw.Volume, tw.Role, tw.Lang, tw.IsHidden, objectJSON(tw.Style))
	return err
}

func writeClip(ctx context.Context, tx pgx.Tx, trackID int64, cw ClipWrite) error {
	c := cw.Clip
	if cw.New {
		_, err := tx.Exec(ctx, `
			insert into timeline_clip (id, track_id, idx, media_object_id,
				source_in_ms, source_out_ms, start_ms, duration_ms,
				speed, volume, fade_in_ms, fade_out_ms,
				transition_in, transition_out, transform, origin, source_node_id, alternates, anchor)
			values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,
			        $13::jsonb,$14::jsonb,$15::jsonb, coalesce(nullif($16,''),'user'), nullif($17,''), $18::jsonb, $19::jsonb)`,
			c.ID, trackID, c.Idx, c.MediaID, c.SourceInMs, c.SourceOutMs, c.StartMs, c.DurationMs,
			c.Speed, c.Volume, c.FadeInMs, c.FadeOutMs,
			transitionJSON(c.TransitionIn), transitionJSON(c.TransitionOut), objectJSON(c.Transform),
			c.Origin, c.SourceNodeID, alternatesJSON(c.Alternates), objectJSON(c.Anchor))
		return err
	}
	// 跨轨移动：track_id 跟着改。source_shot_id 与 pending_generation_id 不碰。
	_, err := tx.Exec(ctx, `
		update timeline_clip set track_id = $2, idx = $3, media_object_id = $4,
			source_in_ms = $5, source_out_ms = $6, start_ms = $7, duration_ms = $8,
			speed = $9, volume = $10, fade_in_ms = $11, fade_out_ms = $12,
			transition_in = $13::jsonb, transition_out = $14::jsonb, transform = $15::jsonb,
			origin = coalesce(nullif($16,''),'user'), source_node_id = nullif($17,''), alternates = $18::jsonb,
			anchor = $19::jsonb
		where id = $1`,
		c.ID, trackID, c.Idx, c.MediaID, c.SourceInMs, c.SourceOutMs, c.StartMs, c.DurationMs,
		c.Speed, c.Volume, c.FadeInMs, c.FadeOutMs,
		transitionJSON(c.TransitionIn), transitionJSON(c.TransitionOut), objectJSON(c.Transform),
		c.Origin, c.SourceNodeID, alternatesJSON(c.Alternates), objectJSON(c.Anchor))
	return err
}

// jsonOrEmptyObject 文字层的样式与位置框列是 not null default '{}'：没有时写 {}。
func jsonOrEmptyObject(raw json.RawMessage) string {
	if obj := domain.ObjectOrNil(raw); obj != nil {
		return string(obj)
	}
	return "{}"
}

// SetFingerprints 只记素材指纹，不推进版本（同步没有动轨道时：指纹是元数据，不是内容）。
func (r *Repo) SetFingerprints(ctx context.Context, timelineID int64, f FingerprintWrite) error {
	return setFingerprints(ctx, r.pool, timelineID, f)
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// setFingerprints ⚠️ planned 只在「还没剪过」时才写（条件在 SQL 里，与写入同一刻判断）：
// 读状态与写指纹之间用户可能刚好剪了一刀，那时再把 planned 改成新素材的指纹，
// 「素材有变化」的提示就被悄悄抹掉了。
func setFingerprints(ctx context.Context, db execer, timelineID int64, f FingerprintWrite) error {
	_, err := db.Exec(ctx, `
		update timeline set synced_fingerprint = $2,
			planned_fingerprint = case when $4 or ($3 and not is_arranged) then $2 else planned_fingerprint end
		where id = $1 and status = 'editing'`, timelineID, f.Synced, f.Planned, f.AfterPlan)
	return err
}
