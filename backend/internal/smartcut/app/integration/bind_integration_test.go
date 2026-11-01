//go:build integration

// 智能剪辑节点 ↔ 时间线（docs/agent/smartcut SCT-07）：建与分叉、摘要、复制项目、清理、孤儿回收。
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	canvasapp "github.com/li904338203/tabtv/backend/internal/canvas/app"
	canvasrepo "github.com/li904338203/tabtv/backend/internal/canvas/repo"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	mediarepo "github.com/li904338203/tabtv/backend/internal/media/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
	smartcutapp "github.com/li904338203/tabtv/backend/internal/smartcut/app"
	timelineapp "github.com/li904338203/tabtv/backend/internal/timeline/app"
	"github.com/li904338203/tabtv/backend/internal/timeline/domain"
	timelinerepo "github.com/li904338203/tabtv/backend/internal/timeline/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

const (
	ws    = int64(9101)
	other = int64(9102)
	user  = int64(501)
)

type env struct {
	ctx    context.Context
	pool   *pgxpool.Pool
	next   func() int64
	svc    *smartcutapp.Service
	tl     *timelineapp.Service
	tlRepo *timelinerepo.Repo
	canvas *canvasapp.Service
}

func setup(t *testing.T) env {
	t.Helper()
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	tlRepo := timelinerepo.New(pool, gen.Next)
	tl := timelineapp.New(tlRepo)
	canvas := canvasapp.New(canvasrepo.New(pool), gen.Next)
	canvas.SetProjectTimelines(tlRepo)
	media := mediaapp.New(mediarepo.New(pool), nil, gen.Next)
	svc := wiring.NewSmartcutService(wiring.SmartcutInput{Timeline: tl, Canvas: canvas, Media: media})
	return env{ctx: context.Background(), pool: pool, next: gen.Next, svc: svc, tl: tl, tlRepo: tlRepo, canvas: canvas}
}

func (e env) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
		t.Fatalf("%s：%v", sql, err)
	}
}

// count 跑一条 count 查询。
func (e env) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// project 建一个项目，画布上放这些智能剪辑节点。
func (e env) project(t *testing.T, workspace int64, nodeIDs ...string) int64 {
	t.Helper()
	nodes := []map[string]any{}
	for _, n := range nodeIDs {
		nodes = append(nodes, map[string]any{"id": n, "type": "smartcut", "title": "智能剪辑", "params": map[string]any{}})
	}
	canvas, _ := json.Marshal(map[string]any{"nodes": nodes, "edges": []any{}})
	id := e.next()
	e.exec(t, `insert into project (id, workspace_id, created_by_user_id, name, canvas_data) values ($1,$2,$3,'项目',$4::jsonb)`,
		id, workspace, user, string(canvas))
	return id
}

// media 插一份媒体（video / image），返回 ID。
func (e env) media(t *testing.T, workspace int64, contentType string) int64 {
	t.Helper()
	id := e.next()
	e.exec(t, `insert into media_object (id, workspace_id, bucket, object_key, content_type, size_bytes, purpose,
		created_by_user_id, duration_ms) values ($1,$2,'b',$3,$4,10,'upload_reference',$5,4000)`,
		id, workspace, fmt.Sprintf("k/%d", id), contentType, user)
	return id
}

// poster 给视频插一张封面帧（Worker 截的 video_poster 派生图）。
func (e env) poster(t *testing.T, workspace, videoID int64) int64 {
	t.Helper()
	id := e.next()
	e.exec(t, `insert into media_object (id, workspace_id, bucket, object_key, content_type, size_bytes, purpose,
		created_by_user_id, source_media_id, derive_kind, derive_index) values ($1,$2,'b',$3,'image/jpeg',10,'generation_output',$4,$5,'video_poster',0)`,
		id, workspace, fmt.Sprintf("k/%d", id), user, videoID)
	return id
}

func code(t *testing.T, err error) errcode.Code {
	t.Helper()
	var be *httpx.BizError
	if !errors.As(err, &be) {
		t.Fatalf("应当是业务错误，实际 %v", err)
	}
	return be.Code
}

func trackOf(t *testing.T, v timelineapp.View, kind string) timelineapp.TrackView {
	t.Helper()
	for _, tr := range v.Tracks {
		if tr.Track.Kind == kind {
			return tr
		}
	}
	t.Fatalf("没有 %s 轨", kind)
	return timelineapp.TrackView{}
}

var editor = timelinerepo.Editor{UserID: user, Kind: timelinerepo.EditorUser}

// SC-AC-01（后端部分）：建节点后摘要里有这条时间线；同一节点重复建返回同一条（第二次 created=false → 200）。
func TestIntegration_建节点按项目与节点幂等(t *testing.T) {
	e := setup(t)
	pid := e.project(t, ws, "s1")
	first, created, err := e.svc.Create(e.ctx, ws, pid, "s1", 0)
	if err != nil || !created {
		t.Fatalf("第一次应新建：%v %v", created, err)
	}
	again, created, err := e.svc.Create(e.ctx, ws, pid, " s1 ", 0)
	if err != nil || created || again.TimelineID != first.TimelineID {
		t.Fatalf("第二次应收敛到同一条：%v %d≠%d %v", created, again.TimelineID, first.TimelineID, err)
	}
	list, err := e.svc.List(e.ctx, ws, pid)
	if err != nil || len(list) != 1 {
		t.Fatalf("摘要：%+v %v", list, err)
	}
	s := list[0]
	if s.CanvasNodeID != "s1" || s.IsArranged || s.Version != 1 || s.RenderStale || s.LatestRender != nil ||
		len(s.Strip) != 0 || s.CoverMediaID != 0 {
		t.Fatalf("新建的智能剪辑：%+v", s)
	}
	v, err := e.tl.Get(e.ctx, ws, s.TimelineID)
	if err != nil || len(v.Tracks) != 3 || v.Timeline.AspectRatio != "9:16" || v.Timeline.Width != 1080 {
		t.Fatalf("三条默认轨、9:16：%+v %v", v.Timeline, err)
	}
	// 回收站里的项目不能新建（40905），节点 ID 太长或为空是 40001。
	e.exec(t, `update project set status = 'deleted', deleted_at = now() where id = $1`, pid)
	if _, _, err := e.svc.Create(e.ctx, ws, pid, "s9", 0); code(t, err) != errcode.InvalidStateTransition {
		t.Fatalf("回收站中的项目应 40905：%v", err)
	}
	if _, err := e.svc.List(e.ctx, ws, pid); err != nil {
		t.Fatalf("回收站中的项目照样能读摘要：%v", err)
	}
	if _, _, err := e.svc.Create(e.ctx, ws, e.project(t, ws), "", 0); code(t, err) != errcode.InvalidArgument {
		t.Fatalf("空节点 ID 应 40001：%v", err)
	}
	if _, err := e.svc.List(e.ctx, other, pid); code(t, err) != errcode.NotFound {
		t.Fatalf("别的工作区读不到：%v", err)
	}
}

// 摘要：迷你时间轴、封面（成片封面帧优先，其次第一段的封面帧）、剪辑已改成片未更新。
func TestIntegration_摘要的时间轴封面与成片过期(t *testing.T) {
	e := setup(t)
	pid := e.project(t, ws, "s1")
	sum, _, err := e.svc.Create(e.ctx, ws, pid, "s1", 0)
	if err != nil {
		t.Fatal(err)
	}
	vid, img := e.media(t, ws, "video/mp4"), e.media(t, ws, "image/png")
	vidPoster := e.poster(t, ws, vid)
	v, _ := e.tl.Get(e.ctx, ws, sum.TimelineID)
	video := trackOf(t, v, domain.TrackVideo).Track.ID
	if _, err := e.tlRepo.SaveClips(e.ctx, sum.TimelineID, video, v.Timeline.Version, editor, []domain.Clip{
		{Idx: 1, MediaID: vid, SourceOutMs: 4000, DurationMs: 4000, Speed: 100, Volume: 100},
		{Idx: 2, MediaID: img, SourceOutMs: 3000, DurationMs: 3000, Speed: 100, Volume: 100,
			TransitionIn: domain.Transition{Kind: domain.TransitionDissolve, DurationMs: 1000}},
	}); err != nil {
		t.Fatal(err)
	}
	list, _ := e.svc.List(e.ctx, ws, pid)
	s := list[0]
	if s.ClipCount != 2 || s.TotalMs != 7000 || s.EffectiveMs != 6000 || len(s.Strip) != 2 ||
		s.Strip[0].Kind != "video" || s.Strip[0].PosterMediaID != vidPoster ||
		s.Strip[1].Kind != "image" || s.Strip[1].StartMs != 3000 || s.Strip[1].PosterMediaID != img {
		t.Fatalf("迷你时间轴（成片时间）：%+v", s)
	}
	if s.CoverMediaID != vidPoster || !s.IsArranged || s.RenderStale {
		t.Fatalf("没有成片时封面取第一段的封面帧；用户保存过就算剪过：%+v", s)
	}
	// 导出过一次（版本 2）且成功：封面换成成片的封面帧；之后又改了一次 → 成片过期。
	out := e.media(t, ws, "video/mp4")
	outPoster := e.poster(t, ws, out)
	e.exec(t, `insert into render_job (id, workspace_id, timeline_id, timeline_version, snapshot, payer_account_id,
		created_by_user_id, status, progress, output_media_id, spec)
		values ($1,$2,$3,2,'{}'::jsonb,1,$4,'success',100,$5,'{"resolution":"1080p","fps":30}'::jsonb)`,
		e.next(), ws, sum.TimelineID, user, out)
	list, _ = e.svc.List(e.ctx, ws, pid)
	s = list[0]
	if s.CoverMediaID != outPoster || s.RenderStale || s.LatestRender == nil || s.OutputPosterID != outPoster ||
		s.LatestRender.TimelineVersion != 2 || len(s.LatestRender.Spec) == 0 {
		t.Fatalf("成片封面优先、版本相同不过期：%+v %+v", s, s.LatestRender)
	}
	if _, err := e.tl.UpdateTrack(e.ctx, ws, sum.TimelineID, trackOf(t, v, domain.TrackAudio).Track.ID, 2, editor, true, 80); err != nil {
		t.Fatal(err)
	}
	// 再来一次失败的导出：latestRender 是失败的那次，过期判定看最近一次**成功**的。
	e.exec(t, `insert into render_job (id, workspace_id, timeline_id, timeline_version, snapshot, payer_account_id,
		created_by_user_id, status, error_message, created_at)
		values ($1,$2,$3,3,'{}'::jsonb,1,$4,'failed','ffmpeg 出错', now() + interval '1 second')`,
		e.next(), ws, sum.TimelineID, user)
	list, _ = e.svc.List(e.ctx, ws, pid)
	s = list[0]
	if !s.RenderStale || s.LatestRender.Status != "failed" || s.LatestRender.ErrorMessage != "ffmpeg 出错" ||
		s.CoverMediaID != outPoster {
		t.Fatalf("改过之后成片过期；最近一次是失败的那次：%+v %+v", s, s.LatestRender)
	}
}

// SC-AC-61（后端部分）：分叉后两条时间线独立；锚点跟着副本的片段；溯源剥掉；跨工作区来源 404。
func TestIntegration_分叉独立且跨工作区404(t *testing.T) {
	e := setup(t)
	pid := e.project(t, ws, "s1", "s2")
	src, _, err := e.svc.Create(e.ctx, ws, pid, "s1", 0)
	if err != nil {
		t.Fatal(err)
	}
	vid := e.media(t, ws, "video/mp4")
	v, _ := e.tl.Get(e.ctx, ws, src.TimelineID)
	if _, err := e.tlRepo.SaveClips(e.ctx, src.TimelineID, trackOf(t, v, domain.TrackVideo).Track.ID, 1, editor,
		[]domain.Clip{{Idx: 1, MediaID: vid, SourceOutMs: 4000, DurationMs: 4000, Speed: 100, Volume: 100}}); err != nil {
		t.Fatal(err)
	}
	v, _ = e.tl.Get(e.ctx, ws, src.TimelineID)
	srcClip := trackOf(t, v, domain.TrackVideo).Clips[0]
	e.exec(t, `update timeline_clip set source_shot_id = 7001 where id = $1`, srcClip.ID)
	e.exec(t, `insert into timeline_subtitle (id, timeline_id, track_id, start_ms, end_ms, text, anchor)
		values ($1,$2,$3,0,1000,'跟随',$4::jsonb)`, e.next(), src.TimelineID,
		trackOf(t, v, domain.TrackSubtitle).Track.ID, fmt.Sprintf(`{"clipId":"%d","srcStartMs":0,"srcEndMs":1000,"future":1}`, srcClip.ID))
	// 跟随片段的音效（SCT-39，SC-F-56）：副本的锚点同样改指副本的片段。
	sfx := e.media(t, ws, "audio/mpeg")
	e.exec(t, `insert into timeline_clip (id, track_id, idx, media_object_id, source_in_ms, source_out_ms, start_ms, duration_ms,
		speed, volume, fade_in_ms, fade_out_ms, origin, alternates, anchor)
		values ($1,$2,1,$3,0,3000,0,3000,100,100,0,0,'user','[]',$4::jsonb)`, e.next(), trackOf(t, v, domain.TrackAudio).Track.ID,
		sfx, fmt.Sprintf(`{"clipId":"%d","srcStartMs":1000,"srcEndMs":4000}`, srcClip.ID))

	fork, created, err := e.svc.Create(e.ctx, ws, pid, "s2", src.TimelineID)
	if err != nil || !created || fork.TimelineID == src.TimelineID {
		t.Fatalf("分叉：%v %d %v", created, fork.TimelineID, err)
	}
	fv, _ := e.tl.Get(e.ctx, ws, fork.TimelineID)
	fc := trackOf(t, fv, domain.TrackVideo).Clips
	if len(fc) != 1 || fc[0].ID == srcClip.ID || fc[0].MediaID != vid || fc[0].SourceShotID != 0 {
		t.Fatalf("片段新 ID、同素材、剥掉溯源：%+v", fc)
	}
	if len(fv.Subtitles) != 1 {
		t.Fatalf("字幕一并复制：%+v", fv.Subtitles)
	}
	if a, ok := domain.ParseAnchor(fv.Subtitles[0].Anchor); !ok || a.ClipID != fc[0].ID {
		t.Fatalf("副本字幕的锚点应指向副本的片段：%s", fv.Subtitles[0].Anchor)
	}
	ac := trackOf(t, fv, domain.TrackAudio).Clips
	if len(ac) != 1 {
		t.Fatalf("音效一并复制：%+v", ac)
	}
	if a, ok := domain.ParseAnchor(ac[0].Anchor); !ok || a.ClipID != fc[0].ID || a.SrcStartMs != 1000 {
		t.Fatalf("副本音效的锚点应指向副本的片段：%s", ac[0].Anchor)
	}
	// 锚点 1~4 秒、片段取 0~4 秒：音效从成片 1 秒处起，取它自己的 0~3 秒（存的起点 0 是旧的，读的时候按锚点算）。
	if ac[0].StartMs != 1000 || ac[0].SourceInMs != 0 || ac[0].SourceOutMs != 3000 || ac[0].Hidden {
		t.Fatalf("副本音效按锚点算出的落点：%+v", ac[0])
	}
	// 改副本不影响原来那条。
	if _, err := e.tl.UpdateMeta(e.ctx, ws, fork.TimelineID, fv.Timeline.Version, editor, "副本", "", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if again, _ := e.tl.Get(e.ctx, ws, src.TimelineID); again.Timeline.Name == "副本" ||
		len(trackOf(t, again, domain.TrackVideo).Clips) != 1 {
		t.Fatalf("原时间线不该被改：%+v", again.Timeline)
	}
	// 别的工作区的时间线不能当来源。
	otherPid := e.project(t, other, "x1")
	foreign, _, err := e.svc.Create(e.ctx, other, otherPid, "x1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.Create(e.ctx, ws, pid, "s3", foreign.TimelineID); code(t, err) != errcode.NotFound {
		t.Fatalf("跨工作区来源应 404：%v", err)
	}
}

// SC-AC-61（后端部分）：复制项目后副本节点的 timelineId 指向新时间线，片段与字幕一并复制。
func TestIntegration_复制项目带上时间线并改指(t *testing.T) {
	e := setup(t)
	pid := e.project(t, ws, "s1")
	src, _, err := e.svc.Create(e.ctx, ws, pid, "s1", 0)
	if err != nil {
		t.Fatal(err)
	}
	// 画布节点写回 timelineId（前端拿到摘要后做的事）。
	e.exec(t, `update project set canvas_data = jsonb_set(canvas_data, '{nodes,0,params}', jsonb_build_object('timelineId', $2::text))
		where id = $1`, pid, fmt.Sprint(src.TimelineID))
	vid := e.media(t, ws, "video/mp4")
	v, _ := e.tl.Get(e.ctx, ws, src.TimelineID)
	if _, err := e.tlRepo.SaveClips(e.ctx, src.TimelineID, trackOf(t, v, domain.TrackVideo).Track.ID, 1, editor,
		[]domain.Clip{{Idx: 1, MediaID: vid, SourceOutMs: 4000, DurationMs: 4000, Speed: 100, Volume: 100}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.tl.SaveSubtitles(e.ctx, ws, src.TimelineID, 2, editor, []timelinerepo.Subtitle{{StartMs: 0, EndMs: 900, Text: "一"}}); err != nil {
		t.Fatal(err)
	}
	dup, err := e.canvas.Duplicate(e.ctx, ws, user, pid, collab.RoleOwner, "")
	if err != nil {
		t.Fatal(err)
	}
	list, err := e.svc.List(e.ctx, ws, dup.ID)
	if err != nil || len(list) != 1 || list[0].TimelineID == src.TimelineID || list[0].CanvasNodeID != "s1" {
		t.Fatalf("副本项目应有自己的时间线：%+v %v", list, err)
	}
	var param string
	if err := e.pool.QueryRow(e.ctx, `select canvas_data->'nodes'->0->'params'->>'timelineId' from project where id = $1`, dup.ID).
		Scan(&param); err != nil || param != fmt.Sprint(list[0].TimelineID) {
		t.Fatalf("副本节点的 timelineId 应改指新时间线：%q（%v）", param, err)
	}
	cv, _ := e.tl.Get(e.ctx, ws, list[0].TimelineID)
	if len(trackOf(t, cv, domain.TrackVideo).Clips) != 1 || len(cv.Subtitles) != 1 || cv.Timeline.Version != 1 {
		t.Fatalf("片段与字幕一并复制、版本从 1 起：%+v", cv.Timeline)
	}
}

// 项目清理（SC-F-171、SCB-11）：该项目的时间线及其子表全部删除；别的项目不动。
func TestIntegration_清理项目删时间线(t *testing.T) {
	e := setup(t)
	pid, keep := e.project(t, ws, "s1"), e.project(t, ws, "k1")
	s, _, _ := e.svc.Create(e.ctx, ws, pid, "s1", 0)
	k, _, _ := e.svc.Create(e.ctx, ws, keep, "k1", 0)
	v, _ := e.tl.Get(e.ctx, ws, s.TimelineID)
	vid := e.media(t, ws, "video/mp4")
	if _, err := e.tlRepo.SaveClips(e.ctx, s.TimelineID, trackOf(t, v, domain.TrackVideo).Track.ID, 1, editor,
		[]domain.Clip{{Idx: 1, MediaID: vid, SourceOutMs: 4000, DurationMs: 4000, Speed: 100, Volume: 100}}); err != nil {
		t.Fatal(err)
	}
	e.exec(t, `insert into timeline_snapshot (id, timeline_id, version, content, reason) values ($1,$2,1,'{}'::jsonb,'ai_plan')`,
		e.next(), s.TimelineID)
	e.exec(t, `insert into render_job (id, workspace_id, timeline_id, timeline_version, snapshot, payer_account_id,
		created_by_user_id, status) values ($1,$2,$3,1,'{}'::jsonb,1,$4,'success')`, e.next(), ws, s.TimelineID, user)
	e.exec(t, `update project set status = 'deleted', deleted_at = now() where id = $1`, pid)
	if err := e.canvas.Purge(e.ctx, ws, user, pid, collab.RoleOwner); err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		sql string
		arg int64
	}{
		{`select count(*) from timeline where project_id = $1`, pid},
		{`select count(*) from timeline_track where timeline_id = $1`, s.TimelineID},
		{`select count(*) from timeline_clip c join timeline_track k on k.id = c.track_id where k.timeline_id = $1`, s.TimelineID},
		{`select count(*) from timeline_snapshot where timeline_id = $1`, s.TimelineID},
		{`select count(*) from render_job where timeline_id = $1`, s.TimelineID},
	} {
		var n int
		if err := e.pool.QueryRow(e.ctx, q.sql, q.arg).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s 应为 0，实际 %d（%v）", q.sql, n, err)
		}
	}
	if _, err := e.tl.Get(e.ctx, ws, k.TimelineID); err != nil {
		t.Fatalf("别的项目的时间线不动：%v", err)
	}
}

// 孤儿时间线（SC-F-06）：节点已不在画布上且超过保留期的删掉；节点仍在、或还没到期的不动；项目已不存在的也删。
func TestIntegration_孤儿时间线按保留期回收(t *testing.T) {
	e := setup(t)
	pid := e.project(t, ws, "alive")
	alive, _, _ := e.svc.Create(e.ctx, ws, pid, "alive", 0)
	gone, _, _ := e.svc.Create(e.ctx, ws, e.project(t, ws, "gone"), "gone", 0)
	recent, _, _ := e.svc.Create(e.ctx, ws, e.project(t, ws, "recent"), "recent", 0)
	ghost, _, _ := e.svc.Create(e.ctx, ws, e.project(t, ws, "ghost"), "ghost", 0)
	// 节点被删了（画布上没有这个节点）。
	e.exec(t, `update project set canvas_data = '{"nodes":[],"edges":[]}'::jsonb
		where id in (select project_id from timeline where id = any($1))`, []int64{gone.TimelineID, recent.TimelineID})
	// 所属项目已经不存在（清理修复之前留下的）。
	e.exec(t, `delete from project where id = (select project_id from timeline where id = $1)`, ghost.TimelineID)
	// 三条到期、一条刚改过。
	e.exec(t, `update timeline set updated_at = now() - interval '40 days' where id = any($1)`,
		[]int64{alive.TimelineID, gone.TimelineID, ghost.TimelineID})
	n, err := e.tlRepo.SweepOrphans(e.ctx, time.Now().Add(-30*24*time.Hour), 100)
	if err != nil || n != 2 {
		t.Fatalf("应删 2 条（节点没了的、项目没了的）：%d %v", n, err)
	}
	for id, want := range map[int64]bool{alive.TimelineID: true, gone.TimelineID: false, recent.TimelineID: true, ghost.TimelineID: false} {
		var c int
		_ = e.pool.QueryRow(e.ctx, `select count(*) from timeline where id = $1`, id).Scan(&c)
		if (c == 1) != want {
			t.Errorf("时间线 %d：期望保留=%v，实际行数 %d", id, want, c)
		}
	}
	// 回收站里的项目不算「不存在」：节点还在画布上，恢复之后照常用。
	e.exec(t, `update project set status = 'deleted', deleted_at = now() where id = $1`, pid)
	if n, _ := e.tlRepo.SweepOrphans(e.ctx, time.Now().Add(-30*24*time.Hour), 100); n != 0 {
		t.Fatalf("回收站里项目的时间线不该被当孤儿：删了 %d 条", n)
	}
}
