//go:build integration

// 整份保存（docs/agent/smartcut SCT-05）。
//
// ⚠️ 乐观锁要在**真并发**下测：顺序调用永远先撞上版本检查，那种测试把 CAS 条件删掉照样绿。
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	timelineapp "github.com/li904338203/tabtv/backend/internal/timeline/app"
	"github.com/li904338203/tabtv/backend/internal/timeline/domain"
	timelinerepo "github.com/li904338203/tabtv/backend/internal/timeline/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

var (
	alice = timelinerepo.Editor{UserID: 501, Kind: timelinerepo.EditorUser}
	bob   = timelinerepo.Editor{UserID: 502, Kind: timelinerepo.EditorUser}
)

// contentOf 把当前时间线写成整份内容（ID 用正式 ID），让测试在它上面改。
func contentOf(t *testing.T, v timelineapp.View) map[string]any {
	t.Helper()
	tracks := []any{}
	for _, tr := range v.Tracks {
		clips := []any{}
		for _, c := range tr.Clips {
			cl := map[string]any{"id": fmt.Sprint(c.ID), "mediaId": fmt.Sprint(c.MediaID),
				"sourceInMs": c.SourceInMs, "sourceOutMs": c.SourceOutMs, "speed": c.Speed, "volume": c.Volume}
			if tr.Track.Kind != domain.TrackVideo {
				cl["startMs"] = c.StartMs
			}
			if c.SourceShotID != 0 {
				cl["sourceShotId"] = fmt.Sprint(c.SourceShotID)
			}
			clips = append(clips, cl)
		}
		tracks = append(tracks, map[string]any{"id": fmt.Sprint(tr.Track.ID), "kind": tr.Track.Kind, "clips": clips})
	}
	return map[string]any{"version": v.Timeline.Version, "aspectRatio": v.Timeline.AspectRatio,
		"fps": v.Timeline.FPS, "width": v.Timeline.Width, "height": v.Timeline.Height, "tracks": tracks}
}

func (e env) save(content map[string]any, ed timelinerepo.Editor, timelineID int64) (timelineapp.SaveContentResult, error) {
	raw, err := json.Marshal(content)
	if err != nil {
		return timelineapp.SaveContentResult{}, err
	}
	c, err := domain.DecodeContent(raw)
	if err != nil {
		return timelineapp.SaveContentResult{}, timelineapp.ContentError(err)
	}
	return e.svc.SaveContent(e.ctx, timelineapp.SaveContentInput{WorkspaceID: ws, TimelineID: timelineID,
		Editor: ed, Content: c})
}

func tracksOf(content map[string]any) []any { return content["tracks"].([]any) }

func trackIn(content map[string]any, kind string) map[string]any {
	for _, tr := range tracksOf(content) {
		if m := tr.(map[string]any); m["kind"] == kind {
			return m
		}
	}
	return nil
}

// trackIndex 某种轨道在内容里的下标（读出来的轨道按种类排序，画面轨不一定在第一个）。
func trackIndex(content map[string]any, kind string) int {
	for i, tr := range tracksOf(content) {
		if tr.(map[string]any)["kind"] == kind {
			return i
		}
	}
	return -1
}

func bizErr(t *testing.T, err error) *httpx.BizError {
	t.Helper()
	var be *httpx.BizError
	if !errors.As(err, &be) {
		t.Fatalf("应当是业务错误，实际 %v", err)
	}
	return be
}

// SC-AC-10（后端部分）：新片段与引用它的字幕锚点一起保存，idMap 给出正式 ID，锚点已改写。
func TestIntegration_整份保存给出idMap并改写锚点(t *testing.T) {
	e := setup(t)
	v := e.create(t)
	c := contentOf(t, v)
	trackIn(c, domain.TrackVideo)["clips"] = []any{
		map[string]any{"id": "tmp-a", "mediaId": "101", "sourceInMs": 0, "sourceOutMs": 4000,
			"transform": map[string]any{"scale": 1.5, "future": "keep"}, "origin": "ai", "sourceNodeId": "node_x"},
		map[string]any{"id": "tmp-b", "mediaId": "102", "sourceInMs": 0, "sourceOutMs": 4000,
			"transitionIn": map[string]any{"kind": "dissolve", "durationMs": 1000}},
	}
	trackIn(c, domain.TrackAudio)["clips"] = []any{
		map[string]any{"id": "tmp-m", "mediaId": "202", "startMs": 2500, "sourceInMs": 0, "sourceOutMs": 5000},
	}
	subID := fmt.Sprint(trackOf(t, v, domain.TrackSubtitle).Track.ID)
	c["tracks"] = append(tracksOf(c), map[string]any{"id": "tmp-text", "kind": "text"})
	c["subtitles"] = []any{map[string]any{"id": "tmp-s", "trackId": subID, "startMs": 0, "endMs": 10, "text": "跟随第二段",
		"anchor": map[string]any{"clipId": "tmp-b", "srcStartMs": 1000, "srcEndMs": 2000, "future": 1}}}
	c["texts"] = []any{map[string]any{"id": "tmp-x", "trackId": "tmp-text", "startMs": 0, "endMs": 3000, "text": "标题",
		"box": map[string]any{"xPct": 50, "yPct": 20, "anchor": "center"}}}

	res, err := e.save(c, alice, v.Timeline.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"tmp-a", "tmp-b", "tmp-m", "tmp-s", "tmp-text", "tmp-x"} {
		if res.IDMap[ref] == "" {
			t.Errorf("idMap 缺 %s：%v", ref, res.IDMap)
		}
	}
	got := res.View
	if got.Timeline.Version != v.Timeline.Version+1 || got.Timeline.LastEditorKind != "user" ||
		got.Timeline.LastEditorUserID != alice.UserID || !got.Timeline.IsArranged {
		t.Errorf("版本进位一次、记下编辑者与「剪过」：%+v", got.Timeline)
	}
	starts := []int{}
	for _, cl := range trackOf(t, got, domain.TrackVideo).Clips {
		starts = append(starts, cl.StartMs)
	}
	if fmt.Sprint(starts) != "[0 3000]" || got.EffectiveMs != 7000 {
		t.Errorf("画面轨按成片时间落点：%v / %d", starts, got.EffectiveMs)
	}
	if a := clipByMedia(t, got, domain.TrackVideo, 101); !strings.Contains(string(a.Transform), "future") ||
		a.Origin != "ai" || a.SourceNodeID != "node_x" || fmt.Sprint(a.ID) != res.IDMap["tmp-a"] {
		t.Errorf("新片段的变换（含未知字段）、来源、来源节点、正式 ID：%+v", a)
	}
	if m := clipByMedia(t, got, domain.TrackAudio, 202); m.StartMs != 2500 {
		t.Errorf("配乐放在 2500：%d", m.StartMs)
	}
	if len(got.Subtitles) != 1 || len(got.Texts) != 1 {
		t.Fatalf("字幕与文字：%+v %+v", got.Subtitles, got.Texts)
	}
	s := got.Subtitles[0]
	anchor, ok := domain.ParseAnchor(s.Anchor)
	if !ok || fmt.Sprint(anchor.ClipID) != res.IDMap["tmp-b"] || !strings.Contains(string(s.Anchor), "future") {
		t.Errorf("锚点应改写成第二段的正式 ID 且保留未知字段：%s", s.Anchor)
	}
	// 跟随第二段（成片起点 3000）的素材 1000~2000 → 成片 4000~5000，而且存下来的就是这一刻的成片时间。
	if s.StartMs != 4000 || s.EndMs != 5000 {
		t.Errorf("跟随片段的字幕应在 4000~5000：%+v", s)
	}
	var storedStart int
	if err := e.pool.QueryRow(e.ctx, `select start_ms from timeline_subtitle where id = $1`, s.ID).Scan(&storedStart); err != nil ||
		storedStart != 4000 {
		t.Errorf("库里存的应是这一刻的成片时间 4000，实际 %d（%v）", storedStart, err)
	}
	if x := got.Texts[0]; fmt.Sprint(x.TrackID) != res.IDMap["tmp-text"] || !strings.Contains(string(x.Box), "center") {
		t.Errorf("文字层挂到新文字轨上：%+v", x)
	}

	// 同一请求重放：版本已经进位，撞乐观锁，不会再建出一份。
	if _, err := e.save(c, alice, v.Timeline.ID); bizErr(t, err).Code != errcode.VersionConflict {
		t.Fatalf("重放应撞版本冲突，实际 %v", err)
	}
	var clips int
	if err := e.pool.QueryRow(e.ctx, `select count(*) from timeline_clip c join timeline_track k on k.id = c.track_id
		where k.timeline_id = $1`, v.Timeline.ID).Scan(&clips); err != nil || clips != 3 {
		t.Fatalf("重放不该多建片段：%d（%v）", clips, err)
	}
}

// 跨轨移动、复制带 sourceShotId 的片段（新片段剥掉溯源）、整批替换字幕与文字、删除轨道。
func TestIntegration_整份保存跨轨移动与复制片段(t *testing.T) {
	e := setup(t)
	v := e.create(t)
	v = e.add(t, v, domain.TrackVideo, 101)
	v = e.add(t, v, domain.TrackVideo, 102)
	v = e.add(t, v, domain.TrackAudio, 201)
	// 第一段标成自动装配出来的（有溯源）。
	shotted := clipByMedia(t, v, domain.TrackVideo, 101)
	if _, err := e.pool.Exec(e.ctx, `update timeline_clip set source_shot_id = 7001 where id = $1`, shotted.ID); err != nil {
		t.Fatal(err)
	}
	v, _ = e.svc.Get(e.ctx, ws, v.Timeline.ID)
	c := contentOf(t, v)
	video := trackIn(c, domain.TrackVideo)
	// 复制第一段（带着 sourceShotId，新片段要剥掉）放到最后，并把音频片段挪到一条新的音效轨。
	dup := map[string]any{}
	for k, val := range video["clips"].([]any)[0].(map[string]any) {
		dup[k] = val
	}
	dup["id"] = "tmp-copy"
	video["clips"] = append(video["clips"].([]any), dup)
	audio := trackIn(c, domain.TrackAudio)
	moved := audio["clips"].([]any)[0].(map[string]any)
	moved["startMs"] = 6000
	audio["clips"] = []any{}
	c["tracks"] = append(tracksOf(c), map[string]any{"id": "tmp-sfx", "kind": "audio", "role": "sfx", "clips": []any{moved}})

	res, err := e.save(c, alice, v.Timeline.ID)
	if err != nil {
		t.Fatalf("复制带溯源的片段不该撞 uk_clip_shot：%v", err)
	}
	got := res.View
	videoClips := trackOf(t, got, domain.TrackVideo).Clips
	if len(videoClips) != 3 || videoClips[0].SourceShotID != 7001 || videoClips[2].SourceShotID != 0 ||
		fmt.Sprint(videoClips[2].ID) != res.IDMap["tmp-copy"] {
		t.Fatalf("原片段保留溯源、复制的剥掉：%+v", videoClips)
	}
	var sfx timelineapp.TrackView
	for _, tr := range got.Tracks {
		if fmt.Sprint(tr.Track.ID) == res.IDMap["tmp-sfx"] {
			sfx = tr
		}
	}
	if sfx.Track.Role != domain.RoleSfx || len(sfx.Clips) != 1 || fmt.Sprint(sfx.Clips[0].ID) != fmt.Sprint(moved["id"]) ||
		sfx.Clips[0].StartMs != 6000 {
		t.Fatalf("音频片段应保持 ID 移到新音效轨的 6000：%+v", sfx)
	}

	// 再存一次：把新音效轨删掉（连同上面的片段）、两条画面片段互换顺序。
	c2 := contentOf(t, got)
	var keep []any
	for _, tr := range tracksOf(c2) {
		if m := tr.(map[string]any); fmt.Sprint(m["id"]) != res.IDMap["tmp-sfx"] {
			keep = append(keep, tr)
		}
	}
	c2["tracks"] = keep
	vc := trackIn(c2, domain.TrackVideo)["clips"].([]any)
	vc[0], vc[1] = vc[1], vc[0]
	res2, err := e.save(c2, alice, got.Timeline.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.View.Tracks) != 3 {
		t.Fatalf("删掉的轨道不该还在：%d 条", len(res2.View.Tracks))
	}
	order := []int64{}
	for _, cl := range trackOf(t, res2.View, domain.TrackVideo).Clips {
		order = append(order, cl.MediaID)
	}
	if fmt.Sprint(order) != "[102 101 101]" {
		t.Fatalf("画面轨按内容里的顺序重排（两阶段重编号不撞索引）：%v", order)
	}
	var orphan int
	if err := e.pool.QueryRow(e.ctx, `select count(*) from timeline_clip where id = $1`, sfx.Clips[0].ID).Scan(&orphan); err != nil || orphan != 0 {
		t.Fatalf("随轨道删掉的片段不该留在库里：%d（%v）", orphan, err)
	}
}

// 非法内容回 40001 带字段路径：素材不属于本工作区、种类不对、别的时间线的片段 ID、改轨道种类。
func TestIntegration_整份保存的非法内容带字段路径(t *testing.T) {
	e := setup(t)
	v := e.create(t)
	other := e.create(t)
	other = e.add(t, other, domain.TrackVideo, 101)
	foreign := trackOf(t, other, domain.TrackVideo).Clips[0].ID

	probe := contentOf(t, v)
	vi, ai := trackIndex(probe, domain.TrackVideo), trackIndex(probe, domain.TrackAudio)
	vp := func(rest string) string { return fmt.Sprintf("tracks[%d].clips[0].%s", vi, rest) }
	cases := []struct {
		name   string
		mutate func(c map[string]any)
		path   string
	}{
		{"素材不属于本工作区", func(c map[string]any) {
			trackIn(c, domain.TrackVideo)["clips"] = []any{map[string]any{"id": "tmp-1", "mediaId": "999", "sourceInMs": 0, "sourceOutMs": 1000}}
		}, vp("mediaId")},
		{"音频放进画面轨", func(c map[string]any) {
			trackIn(c, domain.TrackVideo)["clips"] = []any{map[string]any{"id": "tmp-1", "mediaId": "201", "sourceInMs": 0, "sourceOutMs": 1000}}
		}, vp("mediaId")},
		{"取用区间超出素材", func(c map[string]any) {
			trackIn(c, domain.TrackVideo)["clips"] = []any{map[string]any{"id": "tmp-1", "mediaId": "101", "sourceInMs": 0, "sourceOutMs": 4500}}
		}, vp("sourceOutMs")},
		{"备选的素材不属于本工作区", func(c map[string]any) {
			trackIn(c, domain.TrackVideo)["clips"] = []any{map[string]any{"id": "tmp-1", "mediaId": "101", "sourceInMs": 0, "sourceOutMs": 1000,
				"alternates": []any{map[string]any{"mediaId": "999", "sourceInMs": 0, "sourceOutMs": 1000, "kind": "original"}}}}
		}, vp("alternates[0].mediaId")},
		{"别的时间线的片段", func(c map[string]any) {
			trackIn(c, domain.TrackVideo)["clips"] = []any{map[string]any{"id": fmt.Sprint(foreign), "mediaId": "101", "sourceInMs": 0, "sourceOutMs": 1000}}
		}, vp("id")},
		{"改已有轨道的种类", func(c map[string]any) {
			trackIn(c, domain.TrackAudio)["kind"] = "text"
			trackIn(c, "text")["clips"] = []any{}
		}, fmt.Sprintf("tracks[%d].id", ai)},
		{"形状不对（来自内容校验）", func(c map[string]any) {
			trackIn(c, domain.TrackVideo)["clips"] = []any{map[string]any{"id": "tmp-1", "mediaId": "101", "sourceInMs": 0, "sourceOutMs": 1000, "speed": 900}}
		}, vp("speed")},
	}
	for _, tc := range cases {
		cur, _ := e.svc.Get(e.ctx, ws, v.Timeline.ID)
		c := contentOf(t, cur)
		tc.mutate(c)
		_, err := e.save(c, alice, v.Timeline.ID)
		be := bizErr(t, err)
		if be.Code != errcode.InvalidArgument || fmt.Sprint(be.Details["path"]) != tc.path {
			t.Errorf("%s：期望 40001 + %s，实际 %d %v（%s）", tc.name, tc.path, be.Code, be.Details, be.Message)
		}
	}
	// 被拒的保存不留痕：别的时间线那一段原样不动。
	if c := trackOf(t, func() timelineapp.View { x, _ := e.svc.Get(e.ctx, ws, other.Timeline.ID); return x }(),
		domain.TrackVideo).Clips[0]; c.ID != foreign || c.SourceOutMs != 4000 {
		t.Fatalf("越权写：别的时间线的片段被改了 %+v", c)
	}
}

// SC-AC-08 / SC-AC-09（后端部分）：同一版本并发保存只成功一个；失败的拿到冲突详情，bySelf 区分「我的另一个标签页」与别人。
func TestIntegration_整份保存真并发只成功一个(t *testing.T) {
	e := setup(t)
	// 预热连接池：池里只有一条连接时，并发请求会在取连接时排成队，删掉 CAS 也照样只有一个成功。
	var warm sync.WaitGroup
	for i := 0; i < 8; i++ {
		warm.Add(1)
		go func() { defer warm.Done(); _, _ = e.pool.Exec(e.ctx, `select pg_sleep(0.05)`) }()
	}
	warm.Wait()

	for round := 0; round < 5; round++ {
		v := e.create(t)
		base := contentOf(t, v)
		const n = 8
		var wg sync.WaitGroup
		errs := make([]error, n)
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			c := contentOf(t, v)
			trackIn(c, domain.TrackVideo)["clips"] = []any{map[string]any{"id": fmt.Sprintf("tmp-%d", i),
				"mediaId": "101", "sourceInMs": 0, "sourceOutMs": 1000 + i*100}}
			wg.Add(1)
			go func(i int, c map[string]any) {
				defer wg.Done()
				<-start
				ed := alice
				if i%2 == 1 {
					ed = bob
				}
				_, errs[i] = e.save(c, ed, v.Timeline.ID)
			}(i, c)
		}
		close(start)
		wg.Wait()
		ok := 0
		for _, err := range errs {
			if err == nil {
				ok++
				continue
			}
			if be := bizErr(t, err); be.Code != errcode.VersionConflict || be.Details["currentVersion"] == nil {
				t.Fatalf("第 %d 轮：失败的应是 40902 带冲突详情：%v", round, err)
			}
		}
		if ok != 1 {
			t.Fatalf("第 %d 轮：同一版本并发保存应恰好成功一个，实际 %d 个", round, ok)
		}
		got, _ := e.svc.Get(e.ctx, ws, v.Timeline.ID)
		if got.Timeline.Version != v.Timeline.Version+1 || len(trackOf(t, got, domain.TrackVideo).Clips) != 1 {
			t.Fatalf("第 %d 轮：版本只进位一次、只留一份内容：v%d，%d 段", round, got.Timeline.Version,
				len(trackOf(t, got, domain.TrackVideo).Clips))
		}
		_ = base
	}
}

func TestIntegration_冲突详情说清是谁改的(t *testing.T) {
	e := setup(t)
	v := e.create(t)
	stale := contentOf(t, v)
	if _, err := e.save(contentOf(t, v), alice, v.Timeline.ID); err != nil {
		t.Fatal(err)
	}
	// 同一个人的另一个标签页：bySelf = true。
	be := bizErr(t, func() error { _, err := e.save(stale, alice, v.Timeline.ID); return err }())
	if be.Code != errcode.VersionConflict || be.Details["bySelf"] != true || be.Details["editorKind"] != "user" ||
		be.Details["currentEditorId"] != "501" || be.Details["currentVersion"] != v.Timeline.Version+1 {
		t.Fatalf("本人另一个标签页：%v %v", be.Message, be.Details)
	}
	// 别人：bySelf = false。
	be = bizErr(t, func() error { _, err := e.save(stale, bob, v.Timeline.ID); return err }())
	if be.Details["bySelf"] != false {
		t.Fatalf("别人改的不能说成本人：%v", be.Details)
	}
	// Agent 改的（编辑者两列为空、来源 agent）：同一个用户也不算本人。
	if _, err := e.pool.Exec(e.ctx, `update timeline set last_editor_kind = 'agent' where id = $1`, v.Timeline.ID); err != nil {
		t.Fatal(err)
	}
	be = bizErr(t, func() error { _, err := e.save(stale, alice, v.Timeline.ID); return err }())
	if be.Details["bySelf"] != false || be.Details["editorKind"] != "agent" || !strings.Contains(be.Message, "Agent") {
		t.Fatalf("Agent 的改动不能说成本人另一个窗口：%v %v", be.Message, be.Details)
	}
}

// 逐项接口同样记编辑者与「剪过」；sync 来源的写入不把「剪过」置上（SCD-17）。
func TestIntegration_逐项接口也记编辑者(t *testing.T) {
	e := setup(t)
	v := e.create(t)
	if _, err := e.pool.Exec(e.ctx, `update timeline set is_arranged = false where id = $1`, v.Timeline.ID); err != nil {
		t.Fatal(err)
	}
	cur, _ := e.svc.Get(e.ctx, ws, v.Timeline.ID)
	sync := timelinerepo.Editor{Kind: timelinerepo.EditorSync}
	out, err := e.svc.AddClip(e.ctx, timelineapp.AddClipInput{WorkspaceID: ws, Editor: sync, TimelineID: cur.Timeline.ID,
		TrackID: trackOf(t, cur, domain.TrackVideo).Track.ID, Version: cur.Timeline.Version, MediaID: 101})
	if err != nil {
		t.Fatal(err)
	}
	if out.Timeline.IsArranged || out.Timeline.LastEditorKind != "sync" || out.Timeline.LastEditorUserID != 0 {
		t.Fatalf("连线自动铺轨不算剪过：%+v", out.Timeline)
	}
	out, err = e.svc.UpdateTrack(e.ctx, ws, out.Timeline.ID, trackOf(t, out, domain.TrackAudio).Track.ID,
		out.Timeline.Version, bob, true, 80)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Timeline.IsArranged || out.Timeline.LastEditorKind != "user" || out.Timeline.LastEditorUserID != bob.UserID {
		t.Fatalf("用户改过就算剪过：%+v", out.Timeline)
	}
	// 「剪过」只进不退：之后的自动铺轨不把它改回去。
	out, err = e.svc.RemoveClip(e.ctx, ws, out.Timeline.ID, trackOf(t, out, domain.TrackVideo).Track.ID,
		trackOf(t, out, domain.TrackVideo).Clips[0].ID, out.Timeline.Version, sync)
	if err != nil || !out.Timeline.IsArranged {
		t.Fatalf("自动铺轨不能把「剪过」改回去：%+v %v", out.Timeline, err)
	}
	// 逐项接口的冲突同样带详情。
	_, err = e.svc.UpdateTrack(e.ctx, ws, out.Timeline.ID, trackOf(t, out, domain.TrackAudio).Track.ID,
		out.Timeline.Version-1, alice, false, 100)
	if be := bizErr(t, err); be.Code != errcode.VersionConflict || be.Details["editorKind"] != "sync" {
		t.Fatalf("逐项接口的冲突也要说清来源：%v", be.Details)
	}
}

var _ = context.Background
