//go:build integration

// 输入同步与素材条（docs/agent/smartcut SCT-25）：连线即入轨、剪过之后不动轨道、断开、多产物、归属、幂等、事件。
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"

	genrepo "github.com/li904338203/tabtv/backend/internal/generation/repo"
	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	mediarepo "github.com/li904338203/tabtv/backend/internal/media/repo"
	smartcutapp "github.com/li904338203/tabtv/backend/internal/smartcut/app"
	smartcutdomain "github.com/li904338203/tabtv/backend/internal/smartcut/domain"
	timelineapp "github.com/li904338203/tabtv/backend/internal/timeline/app"
	"github.com/li904338203/tabtv/backend/internal/timeline/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

// timelineMediaLookup 把媒体域接成时间线的素材端口（同 wiring.timelineMedia）。
type timelineMediaLookup struct{ svc *mediaapp.Service }

func (a timelineMediaLookup) Lookup(ctx context.Context, ws, mediaID int64) (timelineapp.MediaInfo, error) {
	m, err := a.svc.Info(ctx, ws, mediaID)
	if err != nil {
		return timelineapp.MediaInfo{}, err
	}
	return timelineapp.MediaInfo{Kind: mediaapp.KindOf(m.ContentType), DurationMs: m.DurationMs}, nil
}

type eventLog struct {
	mu      sync.Mutex
	reasons []string
}

func (l *eventLog) Publish(_ context.Context, _ string, payload []byte) error {
	var ev timelineapp.TimelineUpdated
	if err := json.Unmarshal(payload, &ev); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reasons = append(l.reasons, ev.Reason)
	return nil
}

func (l *eventLog) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.reasons
	l.reasons = nil
	return out
}

type syncEnv struct {
	env
	events *eventLog
	svc    *smartcutapp.Service
}

func setupSync(t *testing.T) syncEnv {
	t.Helper()
	e := setup(t)
	media := mediaapp.New(mediarepo.New(e.pool), nil, e.next)
	e.tl.SetMedia(timelineMediaLookup{media})
	events := &eventLog{}
	e.tl.SetEvents(events)
	svc := wiring.NewSmartcutService(wiring.SmartcutInput{Timeline: e.tl, Canvas: e.canvas, Media: media,
		Generations: genrepo.New(e.pool)})
	e.exec(t, `insert into operation (id, code, name, domain, out_node_type) values
		(9301, 'text2audio', '文字转语音', 'audio', 'audio'), (9302, 'text2img', '文生图', 'image', 'image')`)
	return syncEnv{env: e, events: events, svc: svc}
}

// setCanvas 把画布改成给定的节点与连线（模拟前端保存）。
func (e syncEnv) setCanvas(t *testing.T, pid int64, nodes []map[string]any, edges []map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"nodes": nodes, "edges": edges})
	e.exec(t, `update project set canvas_data = $2::jsonb where id = $1`, pid, string(raw))
}

// mediaOf 插一份媒体，时长可指定（图片为 0）。
func (e syncEnv) mediaOf(t *testing.T, workspace int64, contentType string, durationMs int) int64 {
	t.Helper()
	id := e.next()
	e.exec(t, `insert into media_object (id, workspace_id, bucket, object_key, content_type, size_bytes, purpose,
		created_by_user_id, duration_ms) values ($1,$2,'b',$3,$4,10,'upload_reference',$5,nullif($6,0))`,
		id, workspace, fmt.Sprintf("k/%d", id), contentType, user, durationMs)
	return id
}

// generation 插一次生成与它的产物，返回生成 ID。
func (e syncEnv) generation(t *testing.T, workspace, pid int64, status string, operationID int64, outputs ...int64) int64 {
	t.Helper()
	id := e.next()
	e.exec(t, `insert into generation (id, workspace_id, payer_account_id, created_by_user_id, project_id, node_id,
		operation_id, model_id, result_type, idempotency_key, status, error_message)
		values ($1,$2,1,$3,$4,'n',$5,1,'image',$6,$7::text, case when $7::text = 'failed' then '上游超时' end)`,
		id, workspace, user, pid, operationID, fmt.Sprintf("k-%d", id), status)
	for i, m := range outputs {
		e.exec(t, `insert into generation_output (id, generation_id, idx, media_object_id, output_type)
			values ($1,$2,$3,$4,'image')`, e.next(), id, i, m)
	}
	return id
}

func nodeWith(id, typ string, result map[string]any) map[string]any {
	n := map[string]any{"id": id, "type": typ, "title": "节点 " + id, "params": map[string]any{}}
	if result != nil {
		n["result"] = result
	}
	return n
}

func mediaResult(mediaID int64) map[string]any {
	return map[string]any{"status": "success", "mediaId": fmt.Sprint(mediaID)}
}

func genResult(genID int64) map[string]any {
	return map[string]any{"id": fmt.Sprint(genID), "status": "success"}
}

func edgeTo(src string, order int) map[string]any {
	return map[string]any{"id": "e-" + src, "source": src, "target": "sc", "order": order}
}

func (e syncEnv) newSmartcut(t *testing.T) (pid, tid int64) {
	t.Helper()
	pid = e.project(t, ws, "sc")
	sum, _, err := e.svc.Create(e.ctx, ws, pid, "sc", 0)
	if err != nil {
		t.Fatal(err)
	}
	e.events.take()
	return pid, sum.TimelineID
}

func clipsOf(t *testing.T, v timelineapp.View, kind string) []domain.Clip {
	t.Helper()
	var out []domain.Clip
	for _, tr := range v.Tracks {
		if tr.Track.Kind == kind {
			out = append(out, tr.Clips...)
		}
	}
	return out
}

// SC-AC-02：连 3 段视频与 1 张图片后 autofill：画面轨按编号铺好 4 段，图片 3 秒；没有「剪过」。
// 同步两次结果相同，版本只在第一次进位（幂等）；每次真的写了才发一条 timeline.updated（reason=sync）。
func TestSync_连线即入轨_幂等(t *testing.T) {
	e := setupSync(t)
	pid, tid := e.newSmartcut(t)
	v1, v2, v3 := e.mediaOf(t, ws, "video/mp4", 4000), e.mediaOf(t, ws, "video/mp4", 5000), e.mediaOf(t, ws, "video/mp4", 6000)
	img := e.mediaOf(t, ws, "image/png", 0)
	e.setCanvas(t, pid, []map[string]any{nodeWith("sc", "smartcut", nil),
		nodeWith("a", "video", mediaResult(v1)), nodeWith("b", "video", mediaResult(v2)),
		nodeWith("c", "image", mediaResult(img)), nodeWith("d", "video", mediaResult(v3))},
		[]map[string]any{edgeTo("d", 4), edgeTo("a", 1), edgeTo("c", 3), edgeTo("b", 2)})

	res, err := e.svc.Sync(e.ctx, ws, pid, tid, smartcutapp.SyncAutofill)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.AppendedClipIDs) != 4 || res.UnusedCount != 0 || len(res.DisconnectedClipIDs) != 0 {
		t.Fatalf("铺了 4 段、没有未使用与断开：%+v", res)
	}
	for _, it := range res.Inputs {
		if !it.Used {
			t.Fatalf("铺上去的素材标为已使用：%+v", it)
		}
	}
	v, _ := e.tl.Get(e.ctx, ws, tid)
	clips := clipsOf(t, v, domain.TrackVideo)
	var got []int64
	for _, c := range clips {
		got = append(got, c.MediaID)
		if c.Origin != domain.OriginSync || c.SourceNodeID == "" {
			t.Fatalf("片段来源记成 sync 与来源节点：%+v", c)
		}
	}
	if !slices.Equal(got, []int64{v1, v2, img, v3}) || clips[2].DurationMs != 3000 || clips[3].StartMs != 4000+5000+3000 {
		t.Fatalf("按编号铺、图片 3 秒：%v %+v", got, clips)
	}
	if v.Timeline.IsArranged || v.Timeline.SyncedFingerprint != res.Fingerprint || v.Timeline.PlannedFingerprint != res.Fingerprint {
		t.Fatalf("自动铺轨不算剪过；两个指纹都记下：%+v", v.Timeline)
	}
	if r := e.events.take(); !slices.Equal(r, []string{timelineapp.ReasonSync}) {
		t.Fatalf("写了一次，发一条 reason=sync：%v", r)
	}

	again, err := e.svc.Sync(e.ctx, ws, pid, tid, smartcutapp.SyncAutofill)
	if err != nil || len(again.AppendedClipIDs) != 0 || again.TimelineVersion != res.TimelineVersion ||
		again.Fingerprint != res.Fingerprint {
		t.Fatalf("第二次不再追加、版本不变：%+v %v", again, err)
	}
	if r := e.events.take(); len(r) != 0 {
		t.Fatalf("没写就不发事件：%v", r)
	}
	ins, err := e.svc.Inputs(e.ctx, ws, pid, tid)
	if err != nil || len(ins.Items) != 4 || ins.Fingerprint != res.Fingerprint {
		t.Fatalf("只读素材条与同步结果一致：%+v %v", ins, err)
	}
}

// SC-AC-03：整份保存删掉一段（剪过）后再连一段视频：autofill 不动轨道，unusedCount=1。
// SC-AC-04：断开一条连线后同步：素材条少一条，用到它的片段仍在并进 disconnectedClipIds。
func TestSync_剪过之后不动轨道_断开只报不删(t *testing.T) {
	e := setupSync(t)
	pid, tid := e.newSmartcut(t)
	v1, v2 := e.mediaOf(t, ws, "video/mp4", 4000), e.mediaOf(t, ws, "video/mp4", 5000)
	nodes := []map[string]any{nodeWith("sc", "smartcut", nil), nodeWith("a", "video", mediaResult(v1)),
		nodeWith("b", "video", mediaResult(v2))}
	e.setCanvas(t, pid, nodes, []map[string]any{edgeTo("a", 1), edgeTo("b", 2)})
	if _, err := e.svc.Sync(e.ctx, ws, pid, tid, smartcutapp.SyncAutofill); err != nil {
		t.Fatal(err)
	}
	// 用户在剪辑台删掉第 2 段并保存（整份保存，编辑者 user → 剪过）。
	v, _ := e.tl.Get(e.ctx, ws, tid)
	c := timelineapp.ContentFromView(v)
	for i := range c.Tracks {
		if c.Tracks[i].Kind == domain.TrackVideo {
			c.Tracks[i].Clips = c.Tracks[i].Clips[:1]
		}
	}
	if _, err := e.tl.SaveContent(e.ctx, timelineapp.SaveContentInput{WorkspaceID: ws, TimelineID: tid,
		Editor: editor, Content: c}); err != nil {
		t.Fatal(err)
	}
	planned := v.Timeline.PlannedFingerprint

	v3 := e.mediaOf(t, ws, "video/mp4", 6000)
	nodes = append(nodes, nodeWith("c", "video", mediaResult(v3)))
	e.setCanvas(t, pid, nodes, []map[string]any{edgeTo("a", 1), edgeTo("b", 2), edgeTo("c", 3)})
	res, err := e.svc.Sync(e.ctx, ws, pid, tid, smartcutapp.SyncAutofill)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.AppendedClipIDs) != 0 || res.UnusedCount != 2 {
		t.Fatalf("剪过之后不动轨道：b 被删了、c 是新的，未使用 2：%+v", res)
	}
	after, _ := e.tl.Get(e.ctx, ws, tid)
	if len(clipsOf(t, after, domain.TrackVideo)) != 1 || after.Timeline.PlannedFingerprint != planned ||
		after.Timeline.SyncedFingerprint != res.Fingerprint {
		t.Fatalf("轨道不动，planned 不变（素材有变化），synced 记新的：%+v", after.Timeline)
	}

	// 断开 a：素材条少一条，用到 a 的片段仍在，进 disconnectedClipIds。
	e.setCanvas(t, pid, nodes, []map[string]any{edgeTo("b", 2), edgeTo("c", 3)})
	res, err = e.svc.Sync(e.ctx, ws, pid, tid, smartcutapp.SyncListOnly)
	if err != nil {
		t.Fatal(err)
	}
	kept := clipsOf(t, after, domain.TrackVideo)[0].ID
	if len(res.Inputs) != 2 || !slices.Equal(res.DisconnectedClipIDs, []int64{kept}) {
		t.Fatalf("断开只报不删：%+v", res)
	}
	still, _ := e.tl.Get(e.ctx, ws, tid)
	if len(clipsOf(t, still, domain.TrackVideo)) != 1 {
		t.Fatal("片段不删")
	}
}

// SC-AC-05 / SC-AC-06：出了 4 张图的节点 → 3-1~3-4；别的工作区的生成 / 媒体、平台素材为 invalid、不进轨；
// 失败、排队的生成不进轨；文字转语音来的音频进「人声」轨。
func TestSync_多产物_归属_未完成_音频用途(t *testing.T) {
	e := setupSync(t)
	pid, tid := e.newSmartcut(t)
	imgs := []int64{e.mediaOf(t, ws, "image/png", 0), e.mediaOf(t, ws, "image/png", 0),
		e.mediaOf(t, ws, "image/png", 0), e.mediaOf(t, ws, "image/png", 0)}
	four := e.generation(t, ws, pid, "success", 9302, imgs...)
	otherProject := e.project(t, ws, "x")
	foreignGen := e.generation(t, ws, otherProject, "success", 9302, e.mediaOf(t, ws, "image/png", 0))
	foreignMedia := e.mediaOf(t, other, "video/mp4", 4000)
	platform := e.mediaOf(t, 0, "image/png", 0)
	failed := e.generation(t, ws, pid, "failed", 9302)
	queued := e.generation(t, ws, pid, "queued", 9302)
	voice := e.mediaOf(t, ws, "audio/mpeg", 7000)
	tts := e.generation(t, ws, pid, "success", 9301, voice)

	e.setCanvas(t, pid, []map[string]any{nodeWith("sc", "smartcut", nil),
		nodeWith("m", "image", genResult(four)), nodeWith("f", "image", genResult(foreignGen)),
		nodeWith("w", "video", mediaResult(foreignMedia)), nodeWith("p", "image", mediaResult(platform)),
		nodeWith("x", "image", genResult(failed)), nodeWith("q", "image", genResult(queued)),
		nodeWith("v", "audio", genResult(tts))},
		[]map[string]any{edgeTo("m", 1), edgeTo("f", 2), edgeTo("w", 3), edgeTo("p", 4), edgeTo("x", 5),
			edgeTo("q", 6), edgeTo("v", 7)})

	res, err := e.svc.Sync(e.ctx, ws, pid, tid, smartcutapp.SyncAutofill)
	if err != nil {
		t.Fatal(err)
	}
	type row struct{ no, status, media string }
	var got []row
	for _, it := range res.Inputs {
		got = append(got, row{it.No, it.Status, it.MediaID})
	}
	want := []row{{"1-1", "ready", fmt.Sprint(imgs[0])}, {"1-2", "ready", fmt.Sprint(imgs[1])},
		{"1-3", "ready", fmt.Sprint(imgs[2])}, {"1-4", "ready", fmt.Sprint(imgs[3])},
		{"2", "invalid", ""}, {"3", "invalid", fmt.Sprint(foreignMedia)}, {"4", "invalid", fmt.Sprint(platform)},
		{"5", "failed", ""}, {"6", "pending", ""}, {"7", "ready", fmt.Sprint(voice)}}
	if !slices.Equal(got, want) {
		t.Fatalf("素材条：\n  得到 %v\n  期望 %v", got, want)
	}
	byNo := map[string]smartcutdomain.Input{}
	for _, it := range res.Inputs {
		byNo[it.No] = it
	}
	if byNo["4"].Reason != "平台素材不能直接使用" || byNo["2"].Reason != "这个生成不属于本项目" ||
		byNo["5"].Reason != "上游超时" || byNo["7"].AudioRole != "voice" {
		t.Fatalf("原因与用途：%+v", res.Inputs)
	}
	if len(res.AppendedClipIDs) != 5 {
		t.Fatalf("只铺 4 张图 + 1 段人声：%v", res.AppendedClipIDs)
	}
	v, _ := e.tl.Get(e.ctx, ws, tid)
	if n := len(clipsOf(t, v, domain.TrackVideo)); n != 4 {
		t.Fatalf("画面轨 4 段：%d", n)
	}
	var voiceTrack *timelineapp.TrackView
	for i := range v.Tracks {
		if v.Tracks[i].Track.Kind == domain.TrackAudio && len(v.Tracks[i].Clips) > 0 {
			voiceTrack = &v.Tracks[i]
		}
	}
	if voiceTrack == nil || voiceTrack.Track.Role != "voice" || voiceTrack.Clips[0].MediaID != voice ||
		voiceTrack.Clips[0].StartMs != 0 {
		t.Fatalf("人声进认领来的音频轨（定成 voice）、从 0 开始：%+v", voiceTrack)
	}
}

// 回收站里的项目不能同步（40905）；原位替换还没上线（40001）；别的工作区的时间线 404；画布坏了不当成「没有输入」。
func TestSync_拒绝的情形(t *testing.T) {
	e := setupSync(t)
	pid, tid := e.newSmartcut(t)
	if _, err := e.svc.Sync(e.ctx, ws, pid, tid, smartcutapp.SyncReplaceChanged); code(t, err) != errcode.InvalidArgument {
		t.Fatalf("原位替换还没上线：%v", err)
	}
	if _, err := e.svc.Sync(e.ctx, other, pid, tid, smartcutapp.SyncAutofill); code(t, err) != errcode.NotFound {
		t.Fatalf("别的工作区 404：%v", err)
	}
	e.exec(t, `update project set canvas_data = '{"nodes": "坏的"}'::jsonb where id = $1`, pid)
	if _, err := e.svc.Inputs(e.ctx, ws, pid, tid); code(t, err) != errcode.Internal {
		t.Fatalf("画布坏了要报错，不能当成没有输入：%v", err)
	}
	e.exec(t, `update project set status = 'deleted', deleted_at = now() where id = $1`, pid)
	if _, err := e.svc.Sync(e.ctx, ws, pid, tid, smartcutapp.SyncAutofill); code(t, err) != errcode.InvalidStateTransition {
		t.Fatalf("回收站里的项目不能同步：%v", err)
	}
}
