// Package repo 是时间线的数据访问层。
package repo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/timeline/domain"
)

var (
	ErrNotFound = errors.New("时间线不存在")
	// ErrVersionConflict 乐观锁没撞上：别人先改了。
	ErrVersionConflict = errors.New("时间线版本冲突")
)

type Repo struct {
	pool   *pgxpool.Pool
	nextID func() int64
}

// 编辑者来源（00067 的 ck_timeline_editor_kind）。
const (
	EditorUser   = "user"
	EditorAI     = "ai"
	EditorAgent  = "agent"
	EditorMCP    = "mcp"
	EditorSync   = "sync"
	EditorSystem = "system"
)

// Editor 这次写入是谁、从哪条路来的（docs/agent/smartcut SCD-16）。每一条推进版本的写路径都带它。
//
// 零值 = 说不清（Agent / MCP 的写路径接入 SCT-41 之前）：两列写空，冲突时退回中性说法。
// ⚠️ 不能「没给就不动」：留着上一个 user 的话，冲突提示会把 Agent 的改动说成「你在另一个窗口改的」，
// 用户点「以我为准」就把它覆盖掉了（同导演台 directorBySelf 的那条）。
type Editor struct {
	UserID int64
	Kind   string
}

// arranges 这次写入算不算「剪过」（SCD-17）：连线自动铺轨（sync）与系统回填（system）不算。
// 「剪过」只会从 false 变成 true：之后的自动铺轨不能把用户排好的东西推翻。
func (e Editor) arranges() bool { return e.Kind != EditorSync && e.Kind != EditorSystem }

func New(pool *pgxpool.Pool, nextID func() int64) *Repo {
	return &Repo{pool: pool, nextID: nextID}
}

// NewID 发一个新的对象 ID（整份保存要在写库之前把临时 ID 换成正式 ID，引用它们的锚点才能一起改写）。
func (r *Repo) NewID() int64 { return r.nextID() }

type Timeline struct {
	ID          int64
	WorkspaceID int64
	ProjectID   int64
	ScriptID    int64
	Name        string
	// Version 是**服务端乐观锁**，不是 JSON 结构版本 —— 两者不能混用。
	Version     int
	AspectRatio string
	FPS         int
	Width       int
	Height      int
	Status      string

	// 智能剪辑（00067，docs/agent/smartcut dev/02 §1.1）。
	// CanvasNodeID 绑定的画布节点；旧时间线与脚本时间线为空。
	CanvasNodeID string
	// IsArranged AI 或用户剪过（SCD-17）；连线自动铺上的不算。
	IsArranged       bool
	LastEditorUserID int64
	LastEditorKind   string
	// LastEditorName 最后编辑者的昵称（事件与冲突提示用；账号被清理时为空）。
	LastEditorName string
	// PlannedFingerprint / SyncedFingerprint 素材指纹：AI 剪辑或自动铺轨时的、最近一次同步看到的。
	PlannedFingerprint string
	SyncedFingerprint  string
}

type Track struct {
	ID      int64
	Kind    string
	Idx     int
	Name    string
	IsMuted bool
	Volume  int
	// Role 音频轨的用途（music / voice / sfx）；画面轨可标 voice。空 = 没标。
	Role string
	// Lang 字幕轨的语言。
	Lang     string
	IsHidden bool
	// Style 原样 JSON：字幕轨的统一样式、配乐轨的人声闪避开关。不是对象时当没有（domain.ObjectOrNil）。
	Style json.RawMessage
}

type Subtitle struct {
	ID int64
	// TrackID 所属字幕轨（00067 起必填）。
	TrackID int64
	StartMs int
	EndMs   int
	Text    string
	Style   json.RawMessage
	// Anchor 跟随片段的锚点（原样 JSON，读取用 domain.ParseAnchor）。没有时为空。
	Anchor json.RawMessage
	// Hidden 读取时算出（domain.PlaceAnchored），**不落库**：跟随的片段把它所在的区间裁掉了。
	Hidden bool
}

// Create 建时间线并铺好三条默认轨道。
//
// ⚠️ 轨道与时间线**一个事务**建：分两次写的话，写完时间线就崩溃会留下
// 一条没有任何轨道的时间线 —— 打开是空白，而用户不知道该点哪儿才能修好它。
func (r *Repo) Create(ctx context.Context, t Timeline) (Timeline, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Timeline{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	t.ID = r.nextID()
	t.Version = 1
	if _, err := tx.Exec(ctx, `
		insert into timeline (id, workspace_id, project_id, script_id, name,
			version, aspect_ratio, fps, width, height, status)
		values ($1,$2,$3,nullif($4,0::bigint),$5,1,$6,$7,$8,$9,'editing')`,
		t.ID, t.WorkspaceID, t.ProjectID, t.ScriptID, t.Name,
		t.AspectRatio, t.FPS, t.Width, t.Height); err != nil {
		return Timeline{}, err
	}
	// 三条默认轨：画面、对白/配乐、字幕。少了任何一条，
	// 用户第一次想加东西时都要先"建一条轨道"——那不是他在想的事。
	for _, tr := range []struct {
		kind, name string
	}{
		{domain.TrackVideo, "画面"},
		{domain.TrackAudio, "音频"},
		{domain.TrackSubtitle, "字幕"},
	} {
		if _, err := tx.Exec(ctx, `
			insert into timeline_track (id, timeline_id, kind, idx, name)
			values ($1,$2,$3,1,$4)`, r.nextID(), t.ID, tr.kind, tr.name); err != nil {
			return Timeline{}, err
		}
	}
	return t, tx.Commit(ctx)
}

const timelineCols = `select id, workspace_id, project_id, coalesce(script_id,0), name,
	version, aspect_ratio, fps, width, height, status,
	coalesce(canvas_node_id,''), is_arranged, coalesce(last_editor_user_id,0),
	coalesce(last_editor_kind,''), coalesce(planned_fingerprint,''), coalesce(synced_fingerprint,''),
	coalesce((select u.nickname from app_user u where u.id = timeline.last_editor_user_id), '')
	from timeline `

func scanTimeline(row pgx.Row) (Timeline, error) {
	var t Timeline
	err := row.Scan(&t.ID, &t.WorkspaceID, &t.ProjectID, &t.ScriptID, &t.Name,
		&t.Version, &t.AspectRatio, &t.FPS, &t.Width, &t.Height, &t.Status,
		&t.CanvasNodeID, &t.IsArranged, &t.LastEditorUserID, &t.LastEditorKind,
		&t.PlannedFingerprint, &t.SyncedFingerprint, &t.LastEditorName)
	if errors.Is(err, pgx.ErrNoRows) {
		return Timeline{}, ErrNotFound
	}
	return t, err
}

// Get 按工作区取时间线。归属条件落在 SQL 里 —— 查不到就是没有。
func (r *Repo) Get(ctx context.Context, workspaceID, id int64) (Timeline, error) {
	return scanTimeline(r.pool.QueryRow(ctx,
		timelineCols+`where id = $1 and workspace_id = $2`, id, workspaceID))
}

func (r *Repo) ListByProject(ctx context.Context, workspaceID, projectID int64) (
	[]Timeline, error) {
	rows, err := r.pool.Query(ctx,
		timelineCols+`where workspace_id = $1 and project_id = $2
		order by updated_at desc`, workspaceID, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Timeline
	for rows.Next() {
		t, err := scanTimeline(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

const trackCols = `select id, kind, idx, coalesce(name,''), is_muted, volume,
	coalesce(role,''), coalesce(lang,''), is_hidden, coalesce(style,'null'::jsonb)
	from timeline_track `

func scanTrack(row pgx.Row) (Track, error) {
	var t Track
	if err := row.Scan(&t.ID, &t.Kind, &t.Idx, &t.Name, &t.IsMuted, &t.Volume,
		&t.Role, &t.Lang, &t.IsHidden, &t.Style); err != nil {
		return Track{}, err
	}
	t.Style = domain.ObjectOrNil(t.Style)
	return t, nil
}

func (r *Repo) Tracks(ctx context.Context, timelineID int64) ([]Track, error) {
	rows, err := r.pool.Query(ctx, trackCols+`where timeline_id = $1 order by kind, idx`, timelineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Track
	for rows.Next() {
		t, err := scanTrack(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Track 按 ID 取一条轨道，同时校验它属于这条时间线。
func (r *Repo) Track(ctx context.Context, timelineID, trackID int64) (Track, error) {
	t, err := scanTrack(r.pool.QueryRow(ctx,
		trackCols+`where id = $1 and timeline_id = $2`, trackID, timelineID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Track{}, ErrNotFound
	}
	return t, err
}

func (r *Repo) Clips(ctx context.Context, trackID int64) ([]domain.Clip, error) {
	rows, err := r.pool.Query(ctx, `
		select id, track_id, idx, media_object_id, source_in_ms, source_out_ms,
		       start_ms, duration_ms, speed, volume, fade_in_ms, fade_out_ms,
		       coalesce(source_shot_id,0),
		       coalesce(transition_in,'null'::jsonb), coalesce(transition_out,'null'::jsonb),
		       coalesce(transform,'null'::jsonb), origin, coalesce(source_node_id,''),
		       alternates, coalesce(pending_generation_id,0), coalesce(anchor,'null'::jsonb)
		from timeline_clip where track_id = $1 order by idx`, trackID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Clip
	for rows.Next() {
		var c domain.Clip
		var tin, tout, alternates, anchor json.RawMessage
		if err := rows.Scan(&c.ID, &c.TrackID, &c.Idx, &c.MediaID, &c.SourceInMs,
			&c.SourceOutMs, &c.StartMs, &c.DurationMs, &c.Speed, &c.Volume,
			&c.FadeInMs, &c.FadeOutMs, &c.SourceShotID, &tin, &tout,
			&c.Transform, &c.Origin, &c.SourceNodeID, &alternates, &c.PendingGenerationID, &anchor); err != nil {
			return nil, err
		}
		c.Anchor = domain.ObjectOrNil(anchor)
		// ⚠️ 坏值当"没有"，不让整条时间线打不开（见 domain.ParseTransition）：
		// 转场、变换、备选都一样。
		c.TransitionIn = domain.ParseTransition(tin)
		c.TransitionOut = domain.ParseTransition(tout)
		c.Transform = domain.ObjectOrNil(c.Transform)
		c.Alternates = domain.ParseAlternates(alternates)
		out = append(out, c)
	}
	return out, rows.Err()
}

// SaveClips 用一次事务把整条轨道的片段写成给定的样子，并推进时间线版本。
//
// ⚠️ **两阶段重编号**：`uk_clip_idx(track_id, idx)` 是普通唯一索引，
// PG **不延迟检查** —— `update ... set idx = idx + 1` 在 PG 上当场就报重复。
// 所以先把要保留的行挪到一个**低于所有现存值**的临时区，再写回正式号。
//
// ⚠️ 临时区必须用 `least(0, min(idx)) - row_number()` 而不是简单取负：
// 上一次失败可能已经在负数区留了行，取负会与它们撞上，
// 而撞不撞取决于执行计划 —— 表现为**时有时无**的失败
// （脚本域的镜号重排踩过同一个坑，这是第二次）。
//
// ⚠️ 删除 + 插入 + 重排必须在**同一个事务**里：分开做的话，
// 失败会留一行在临时区，之后任何插入都会撞号，而那条轨道再也编辑不了。
func (r *Repo) SaveClips(ctx context.Context, timelineID, trackID int64,
	version int, ed Editor, clips []domain.Clip) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 先抢版本：抢不到就别动数据。
	next, err := bumpVersion(ctx, tx, timelineID, version, ed)
	if err != nil {
		return 0, err
	}

	keep := make([]int64, 0, len(clips))
	for _, c := range clips {
		if c.ID != 0 {
			keep = append(keep, c.ID)
		}
	}
	// 不在清单里的行删掉（用户删了那一段）。
	if _, err := tx.Exec(ctx, `
		delete from timeline_clip where track_id = $1 and not (id = any($2::bigint[]))`,
		trackID, keep); err != nil {
		return 0, err
	}
	// 第一阶段：把留下的行挪进临时区。
	if _, err := tx.Exec(ctx, `
		update timeline_clip c set idx = t.tmp
		from (select id, least(0, (select min(idx) from timeline_clip where track_id = $1))
		             - row_number() over (order by idx) as tmp
		      from timeline_clip where track_id = $1) t
		where c.id = t.id`, trackID); err != nil {
		return 0, err
	}

	// 第二阶段：写回正式号，并把新段插进来。
	//
	// ⚠️ pending_generation_id 只在插入时写空、更新时**不碰**：它由片段级生成在这条编辑链路之外置上，
	// 这里拿着的是读出来那一刻的值 —— 照写回去的话，一次普通的裁切就会把刚开始的生成标记抹掉，
	// 导出预检随之放行一段还在重拍的片段（SC-F-138）。
	for _, c := range clips {
		if c.ID == 0 {
			id := r.nextID()
			if _, err := tx.Exec(ctx, `
				insert into timeline_clip (id, track_id, idx, media_object_id,
					source_in_ms, source_out_ms, start_ms, duration_ms,
					speed, volume, fade_in_ms, fade_out_ms, source_shot_id,
					transition_in, transition_out,
					transform, origin, source_node_id, alternates, anchor)
				values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,nullif($13,0::bigint),
				        $14::jsonb,$15::jsonb,
				        $16::jsonb, coalesce(nullif($17,''),'user'), nullif($18,''), $19::jsonb, $20::jsonb)`,
				id, trackID, c.Idx, c.MediaID, c.SourceInMs, c.SourceOutMs,
				c.StartMs, c.DurationMs, c.Speed, c.Volume, c.FadeInMs, c.FadeOutMs,
				c.SourceShotID, transitionJSON(c.TransitionIn),
				transitionJSON(c.TransitionOut),
				objectJSON(c.Transform), c.Origin, c.SourceNodeID, alternatesJSON(c.Alternates),
				objectJSON(c.Anchor)); err != nil {
				return 0, err
			}
			continue
		}
		if _, err := tx.Exec(ctx, `
			update timeline_clip set idx = $2, media_object_id = $3,
				source_in_ms = $4, source_out_ms = $5, start_ms = $6, duration_ms = $7,
				speed = $8, volume = $9, fade_in_ms = $10, fade_out_ms = $11,
				transition_in = $12::jsonb, transition_out = $13::jsonb,
				transform = $14::jsonb, origin = coalesce(nullif($15,''),'user'),
				source_node_id = nullif($16,''), alternates = $17::jsonb, anchor = $18::jsonb
			where id = $1`,
			c.ID, c.Idx, c.MediaID, c.SourceInMs, c.SourceOutMs, c.StartMs,
			c.DurationMs, c.Speed, c.Volume, c.FadeInMs, c.FadeOutMs,
			transitionJSON(c.TransitionIn), transitionJSON(c.TransitionOut),
			objectJSON(c.Transform), c.Origin, c.SourceNodeID, alternatesJSON(c.Alternates),
			objectJSON(c.Anchor)); err != nil {
			return 0, err
		}
	}
	return next, tx.Commit(ctx)
}

// bumpVersion 乐观锁推进，同时记下这次的编辑者（SCD-16）与「剪过」（SCD-17）。0 行更新即冲突。
//
// ⚠️ 与画布保存同一条规则：0 行更新返回冲突，**不能静默当成功** ——
// 静默的话两个人同时编辑，后保存的那份把前一份悄悄覆盖掉。
func bumpVersion(ctx context.Context, tx pgx.Tx, timelineID int64, version int, ed Editor) (int, error) {
	var next int
	err := tx.QueryRow(ctx, `
		update timeline set version = version + 1, updated_at = now(),
			last_editor_user_id = nullif($3, 0::bigint), last_editor_kind = nullif($4, ''),
			is_arranged = is_arranged or $5
		where id = $1 and version = $2 and status = 'editing'
		returning version`, timelineID, version, ed.UserID, ed.Kind, ed.arranges()).Scan(&next)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrVersionConflict
	}
	return next, err
}

// UpdateMeta 改名、画幅、帧率。同样走乐观锁。
func (r *Repo) UpdateMeta(ctx context.Context, timelineID int64, version int, ed Editor,
	name, aspect string, fps, width, height int) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	next, err := bumpVersion(ctx, tx, timelineID, version, ed)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `
		update timeline set name = $2, aspect_ratio = $3, fps = $4,
			width = $5, height = $6 where id = $1`,
		timelineID, name, aspect, fps, width, height); err != nil {
		return 0, err
	}
	return next, tx.Commit(ctx)
}

// ---------------------------------------------------------------- 字幕

// Subtitles 这条时间线全部字幕轨上的字幕。
func (r *Repo) Subtitles(ctx context.Context, timelineID int64) ([]Subtitle, error) {
	rows, err := r.pool.Query(ctx, `
		select id, track_id, start_ms, end_ms, text, coalesce(style,'{}'::jsonb),
		       coalesce(anchor,'null'::jsonb)
		from timeline_subtitle where timeline_id = $1 order by start_ms, id`, timelineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Subtitle
	for rows.Next() {
		var s Subtitle
		if err := rows.Scan(&s.ID, &s.TrackID, &s.StartMs, &s.EndMs, &s.Text, &s.Style,
			&s.Anchor); err != nil {
			return nil, err
		}
		if _, ok := domain.ParseAnchor(s.Anchor); !ok {
			s.Anchor = nil
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Texts 这条时间线的文字层（写入在整份保存里，SCT-05）。
func (r *Repo) Texts(ctx context.Context, timelineID int64) ([]domain.TextLayer, error) {
	rows, err := r.pool.Query(ctx, `
		select id, track_id, start_ms, end_ms, text, style, box
		from timeline_text where timeline_id = $1 order by start_ms, id`, timelineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.TextLayer
	for rows.Next() {
		var t domain.TextLayer
		if err := rows.Scan(&t.ID, &t.TrackID, &t.StartMs, &t.EndMs, &t.Text, &t.Style, &t.Box); err != nil {
			return nil, err
		}
		t.Style = domain.ObjectOrNil(t.Style)
		t.Box = domain.ObjectOrNil(t.Box)
		out = append(out, t)
	}
	return out, rows.Err()
}

// SaveSubtitles 整批替换**默认字幕轨**的字幕，并推进版本。
//
// ⚠️ 整批替换而不是逐条增删：字幕的编辑是"改一整段时间轴"，
// 逐条 diff 要在前后端各写一份匹配逻辑，而两份早晚不一致。
// 字幕行很轻（几十字节），整批重写的代价可以忽略。
//
// ⚠️ 只动默认字幕轨（idx 最小的那条）：这条旧接口的请求体里没有轨道，自 00067 起一条时间线可以有
// 好几条字幕轨（翻译出来的各语言）。照旧「删光再插进默认轨」的话，一次旧编辑器里的保存就把
// 其它语言的字幕全并进默认轨，而界面上只是多了几行字。多字幕轨的读写走整份保存（SCT-05）。
// 请求体也没有锚点：默认轨上的字幕经这里保存之后不再跟随片段。
func (r *Repo) SaveSubtitles(ctx context.Context, timelineID int64, version int, ed Editor,
	subs []Subtitle) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	next, err := bumpVersion(ctx, tx, timelineID, version, ed)
	if err != nil {
		return 0, err
	}
	var trackID int64
	if err := tx.QueryRow(ctx, `
		select id from timeline_track where timeline_id = $1 and kind = 'subtitle'
		order by idx, id limit 1`, timelineID).Scan(&trackID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	if _, err := tx.Exec(ctx,
		`delete from timeline_subtitle where timeline_id = $1 and track_id = $2`,
		timelineID, trackID); err != nil {
		return 0, err
	}
	for _, s := range subs {
		style := s.Style
		if len(style) == 0 {
			style = json.RawMessage(`{}`)
		}
		if _, err := tx.Exec(ctx, `
			insert into timeline_subtitle (id, timeline_id, track_id, start_ms, end_ms, text, style)
			values ($1, $2, $3, $4, $5, $6, $7::jsonb)`,
			r.nextID(), timelineID, trackID, s.StartMs, s.EndMs, s.Text, string(style)); err != nil {
			return 0, err
		}
	}
	return next, tx.Commit(ctx)
}

// UpdateTrack 改轨道的静音与音量。
func (r *Repo) UpdateTrack(ctx context.Context, timelineID, trackID int64, version int, ed Editor,
	muted bool, volume int) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	next, err := bumpVersion(ctx, tx, timelineID, version, ed)
	if err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `
		update timeline_track set is_muted = $3, volume = $4
		where id = $2 and timeline_id = $1`, timelineID, trackID, muted, volume)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() == 0 {
		return 0, ErrNotFound
	}
	return next, tx.Commit(ctx)
}

// objectJSON 原样 JSON 写进 jsonb：不是对象时写 null（与 transitionJSON 同一条：null 的语义就是"没有"）。
func objectJSON(raw json.RawMessage) any {
	if obj := domain.ObjectOrNil(raw); obj != nil {
		return string(obj)
	}
	return nil
}

// alternatesJSON 备选写成 jsonb 数组。没有时写 `[]`（列是 not null default '[]'）。
func alternatesJSON(list []domain.Alternate) string {
	if len(list) == 0 {
		return "[]"
	}
	b, err := json.Marshal(list)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// transitionJSON 把转场写成 jsonb。没设转场时写 null 而不是 `{}` ——
// `{}` 会让读出来的那一条"存在但类型为空"，下游要再判一次；
// null 的语义就是"没有"。
func transitionJSON(t domain.Transition) any {
	if !t.IsActive() {
		return nil
	}
	b, err := json.Marshal(t)
	if err != nil {
		return nil
	}
	return string(b)
}

// EditorState 是冲突详情要的当前状态：版本、最后编辑者与他的昵称、来源。
type EditorState struct {
	Version    int
	EditorID   int64
	EditorName string
	EditorKind string
	UpdatedAt  time.Time
}

// CurrentEditor 取时间线当前的版本与最后编辑者。
//
// ⚠️ `left join`：编辑者可能已被清理，内连接会让整行消失，调用方拿到 ErrNotFound、
// 界面上说「时间线不存在」—— 而它明明还在（同画布的 last_editor 那一条）。
func (r *Repo) CurrentEditor(ctx context.Context, timelineID int64) (EditorState, error) {
	var st EditorState
	err := r.pool.QueryRow(ctx, `
		select t.version, coalesce(t.last_editor_user_id, 0), coalesce(u.nickname, ''),
		       coalesce(t.last_editor_kind, ''), t.updated_at
		from timeline t left join app_user u on u.id = t.last_editor_user_id
		where t.id = $1`, timelineID).
		Scan(&st.Version, &st.EditorID, &st.EditorName, &st.EditorKind, &st.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return EditorState{}, ErrNotFound
	}
	return st, err
}
