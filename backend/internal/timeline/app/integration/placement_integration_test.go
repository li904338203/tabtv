//go:build integration

// 成片时间与按起点放置的轨道（docs/agent/smartcut SCT-04）：逐项接口按轨道种类分流。
//
// ⚠️ 必须走真库：落点是"读出来再算一次"的，库里存的只是缓存 ——
// 只测纯函数的话看不出读取路径把音频轨又挤回首尾相接（SCB-2 的原样）。
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
	timelineapp "github.com/li904338203/tabtv/backend/internal/timeline/app"
	"github.com/li904338203/tabtv/backend/internal/timeline/domain"
	timelinerepo "github.com/li904338203/tabtv/backend/internal/timeline/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

const ws = int64(9001)

type fakeMedia map[int64]timelineapp.MediaInfo

func (f fakeMedia) Lookup(_ context.Context, _, mediaID int64) (timelineapp.MediaInfo, error) {
	if m, ok := f[mediaID]; ok {
		return m, nil
	}
	return timelineapp.MediaInfo{}, errors.New("不存在")
}

// 素材：101~103 视频各 4 秒，201 音频 3 秒，202 音频 5 秒。
var media = fakeMedia{
	101: {Kind: "video", DurationMs: 4000}, 102: {Kind: "video", DurationMs: 4000},
	103: {Kind: "video", DurationMs: 4000},
	201: {Kind: "audio", DurationMs: 3000}, 202: {Kind: "audio", DurationMs: 5000},
}

type env struct {
	svc  *timelineapp.Service
	pool *pgxpool.Pool
	next func() int64
	ctx  context.Context
}

func setup(t *testing.T) env {
	t.Helper()
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	svc := timelineapp.New(timelinerepo.New(pool, gen.Next))
	svc.SetMedia(media)
	return env{svc: svc, pool: pool, next: gen.Next, ctx: context.Background()}
}

func (e env) create(t *testing.T) timelineapp.View {
	t.Helper()
	v, err := e.svc.Create(e.ctx, timelineapp.CreateInput{WorkspaceID: ws, ProjectID: 777, AspectRatio: "16:9"})
	if err != nil {
		t.Fatal(err)
	}
	return v
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

func clipByMedia(t *testing.T, v timelineapp.View, kind string, mediaID int64) domain.Clip {
	t.Helper()
	for _, c := range trackOf(t, v, kind).Clips {
		if c.MediaID == mediaID {
			return c
		}
	}
	t.Fatalf("%s 轨上没有素材 %d", kind, mediaID)
	return domain.Clip{}
}

func (e env) add(t *testing.T, v timelineapp.View, kind string, mediaID int64) timelineapp.View {
	t.Helper()
	out, err := e.svc.AddClip(e.ctx, timelineapp.AddClipInput{WorkspaceID: ws, TimelineID: v.Timeline.ID,
		TrackID: trackOf(t, v, kind).Track.ID, Version: v.Timeline.Version, MediaID: mediaID})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func startAt(ms int) *int { return &ms }

func pathOf(err error) string {
	var be *httpx.BizError
	if errors.As(err, &be) && be.Code == errcode.InvalidArgument {
		return fmt.Sprint(be.Details["path"])
	}
	return "<" + fmt.Sprint(err) + ">"
}

// SC-AC-12（后端部分）+ 卡片要求的「Agent 往音频轨加两段，第二段不被挤到第一段末尾」。
func TestIntegration_音频轨按起点放置且不随画面轨变化(t *testing.T) {
	e := setup(t)
	v := e.create(t)
	v = e.add(t, v, domain.TrackAudio, 201)
	v = e.add(t, v, domain.TrackAudio, 202)
	if c := clipByMedia(t, v, domain.TrackAudio, 202); c.StartMs != 3000 {
		t.Fatalf("追加的第二段默认放在上一段末尾 3000，实际 %d", c.StartMs)
	}
	b := clipByMedia(t, v, domain.TrackAudio, 202)
	v, err := e.svc.MoveClip(e.ctx, timelineapp.MoveInput{WorkspaceID: ws, TimelineID: v.Timeline.ID,
		TrackID: trackOf(t, v, domain.TrackAudio).Track.ID, ClipID: b.ID, Version: v.Timeline.Version,
		StartMs: startAt(10000)})
	if err != nil {
		t.Fatal(err)
	}
	// 读回来仍是 10000：读取路径不再把音频轨挤回首尾相接。
	got, err := e.svc.Get(e.ctx, ws, v.Timeline.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c := clipByMedia(t, got, domain.TrackAudio, 202); c.StartMs != 10000 || c.DurationMs != 5000 {
		t.Fatalf("音频片段应留在 10000（时长 5000），实际 %+v", c)
	}
	// 画面轨增删片段，配乐的位置不变。
	got = e.add(t, got, domain.TrackVideo, 101)
	got = e.add(t, got, domain.TrackVideo, 102)
	first := clipByMedia(t, got, domain.TrackVideo, 101)
	got, err = e.svc.RemoveClip(e.ctx, ws, got.Timeline.ID, trackOf(t, got, domain.TrackVideo).Track.ID,
		first.ID, got.Timeline.Version, timelinerepo.Editor{})
	if err != nil {
		t.Fatal(err)
	}
	if c := clipByMedia(t, got, domain.TrackAudio, 202); c.StartMs != 10000 {
		t.Fatalf("画面轨增删之后配乐应仍在 10000，实际 %d", c.StartMs)
	}
	if c := clipByMedia(t, got, domain.TrackVideo, 102); c.StartMs != 0 {
		t.Fatalf("画面轨删掉第一段后第二段补到 0，实际 %d", c.StartMs)
	}
}

func TestIntegration_音频轨移动撞上另一段报字段路径(t *testing.T) {
	e := setup(t)
	v := e.create(t)
	v = e.add(t, v, domain.TrackAudio, 201)
	v = e.add(t, v, domain.TrackAudio, 202)
	audio := trackOf(t, v, domain.TrackAudio).Track.ID
	b := clipByMedia(t, v, domain.TrackAudio, 202)
	_, err := e.svc.MoveClip(e.ctx, timelineapp.MoveInput{WorkspaceID: ws, TimelineID: v.Timeline.ID,
		TrackID: audio, ClipID: b.ID, Version: v.Timeline.Version, StartMs: startAt(1000)})
	if p := pathOf(err); p != "startMs" {
		t.Fatalf("压到 0~3000 的那一段上应报 startMs，实际 %s", p)
	}
	// 非画面轨的移动必须给 startMs（只给 toPosition 是画面轨的用法）。
	_, err = e.svc.MoveClip(e.ctx, timelineapp.MoveInput{WorkspaceID: ws, TimelineID: v.Timeline.ID,
		TrackID: audio, ClipID: b.ID, Version: v.Timeline.Version, ToPosition: 1})
	if p := pathOf(err); p != "startMs" {
		t.Fatalf("没给 startMs 应报 startMs，实际 %s", p)
	}
	// 首尾相接不算重叠。
	if _, err := e.svc.MoveClip(e.ctx, timelineapp.MoveInput{WorkspaceID: ws, TimelineID: v.Timeline.ID,
		TrackID: audio, ClipID: b.ID, Version: v.Timeline.Version, StartMs: startAt(3000)}); err != nil {
		t.Fatalf("放在上一段末尾 3000 应当可以：%v", err)
	}
}

func TestIntegration_非画面轨的裁切与分割不重排(t *testing.T) {
	e := setup(t)
	v := e.create(t)
	v = e.add(t, v, domain.TrackAudio, 201)
	v = e.add(t, v, domain.TrackAudio, 202)
	audio := trackOf(t, v, domain.TrackAudio).Track.ID
	a := clipByMedia(t, v, domain.TrackAudio, 201)
	b := clipByMedia(t, v, domain.TrackAudio, 202)
	v, err := e.svc.MoveClip(e.ctx, timelineapp.MoveInput{WorkspaceID: ws, TimelineID: v.Timeline.ID,
		TrackID: audio, ClipID: b.ID, Version: v.Timeline.Version, StartMs: startAt(10000)})
	if err != nil {
		t.Fatal(err)
	}
	// 裁短第一段：第二段不往前补（画面轨才会补）。
	v, err = e.svc.TrimClip(e.ctx, timelineapp.TrimInput{WorkspaceID: ws, TimelineID: v.Timeline.ID,
		TrackID: audio, ClipID: a.ID, Version: v.Timeline.Version, SourceInMs: 0, SourceOutMs: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if c := clipByMedia(t, v, domain.TrackAudio, 202); c.StartMs != 10000 {
		t.Fatalf("裁切音频轨上的一段不该挪动别的段，实际 %d", c.StartMs)
	}
	// 分割：右半段紧接左半段，两段合起来仍占原来的位置。
	v, err = e.svc.SplitClip(e.ctx, timelineapp.SplitInput{WorkspaceID: ws, TimelineID: v.Timeline.ID,
		TrackID: audio, ClipID: b.ID, Version: v.Timeline.Version, AtMs: 1500})
	if err != nil {
		t.Fatal(err)
	}
	clips := trackOf(t, v, domain.TrackAudio).Clips
	if len(clips) != 3 || clips[1].StartMs != 10000 || clips[1].DurationMs != 1500 ||
		clips[2].StartMs != 11500 || clips[2].DurationMs != 3500 {
		t.Fatalf("分割后应为 10000+1500 与 11500+3500：%+v", clips)
	}
	// 变速让这一段变长、压到后一段：明确拒绝。
	speed := 50
	_, err = e.svc.UpdateClip(e.ctx, timelineapp.ClipPropsInput{WorkspaceID: ws, TimelineID: v.Timeline.ID,
		TrackID: audio, ClipID: clips[1].ID, Version: v.Timeline.Version, Props: domain.ClipProps{Speed: &speed}})
	if p := pathOf(err); p != "startMs" {
		t.Fatalf("变速压到后一段应报 startMs，实际 %s", p)
	}
}

// SC-AC-11（后端部分）：三段 4 秒、两处 1 秒叠化 → 第 3 段成片起点 6000、成片 10000、各段之和 12000。
func TestIntegration_画面轨的成片起点扣掉转场重叠(t *testing.T) {
	e := setup(t)
	v := e.create(t)
	for _, m := range []int64{101, 102, 103} {
		v = e.add(t, v, domain.TrackVideo, m)
	}
	video := trackOf(t, v, domain.TrackVideo).Track.ID
	for _, m := range []int64{102, 103} {
		c := clipByMedia(t, v, domain.TrackVideo, m)
		var err error
		v, err = e.svc.UpdateClip(e.ctx, timelineapp.ClipPropsInput{WorkspaceID: ws, TimelineID: v.Timeline.ID,
			TrackID: video, ClipID: c.ID, Version: v.Timeline.Version,
			Props: domain.ClipProps{TransitionIn: &domain.Transition{Kind: domain.TransitionDissolve, DurationMs: 1000}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := e.svc.Get(e.ctx, ws, v.Timeline.ID)
	if err != nil {
		t.Fatal(err)
	}
	starts := []int{}
	for _, c := range trackOf(t, got, domain.TrackVideo).Clips {
		starts = append(starts, c.StartMs)
	}
	if fmt.Sprint(starts) != "[0 3000 6000]" || got.TotalMs != 12000 || got.EffectiveMs != 10000 {
		t.Fatalf("起点 %v、各段之和 %d、成片 %d，应为 [0 3000 6000] / 12000 / 10000", starts, got.TotalMs, got.EffectiveMs)
	}
}

// SC-AC-19（后端部分）：跟随片段的字幕在片段被裁掉开头 1 秒后整体前移 1 秒，落在被裁区间的隐藏；恢复后重新出现。
func TestIntegration_跟随片段的字幕随裁切换算(t *testing.T) {
	e := setup(t)
	v := e.create(t)
	v = e.add(t, v, domain.TrackVideo, 101)
	clip := clipByMedia(t, v, domain.TrackVideo, 101)
	subTrack := trackOf(t, v, domain.TrackSubtitle).Track.ID
	for i, r := range [][2]int{{0, 800}, {1800, 3000}} {
		anchor, _ := json.Marshal(map[string]any{"clipId": fmt.Sprint(clip.ID), "srcStartMs": r[0], "srcEndMs": r[1]})
		if _, err := e.pool.Exec(e.ctx, `insert into timeline_subtitle (id, timeline_id, track_id, start_ms, end_ms, text, anchor)
			values ($1,$2,$3,$4,$5,$6,$7::jsonb)`, e.next(), v.Timeline.ID, subTrack, r[0], r[1],
			fmt.Sprintf("第%d句", i+1), string(anchor)); err != nil {
			t.Fatal(err)
		}
	}
	trim := func(in int) timelineapp.View {
		cur, err := e.svc.Get(e.ctx, ws, v.Timeline.ID)
		if err != nil {
			t.Fatal(err)
		}
		out, err := e.svc.TrimClip(e.ctx, timelineapp.TrimInput{WorkspaceID: ws, TimelineID: v.Timeline.ID,
			TrackID: trackOf(t, cur, domain.TrackVideo).Track.ID, ClipID: clip.ID, Version: cur.Timeline.Version,
			SourceInMs: in, SourceOutMs: 4000})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	subs := trim(1000).Subtitles
	byText := map[string]timelinerepo.Subtitle{}
	for _, s := range subs {
		byText[s.Text] = s
	}
	if s := byText["第1句"]; !s.Hidden {
		t.Errorf("落在被裁掉的开头 1 秒里的字幕应隐藏：%+v", s)
	}
	if s := byText["第2句"]; s.Hidden || s.StartMs != 800 || s.EndMs != 2000 {
		t.Errorf("其余字幕应整体前移 1 秒到 800~2000：%+v", s)
	}
	for _, s := range trim(0).Subtitles {
		if s.Hidden {
			t.Errorf("恢复取用区间后应重新出现：%+v", s)
		}
	}
}
