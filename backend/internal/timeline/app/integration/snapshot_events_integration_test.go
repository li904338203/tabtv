//go:build integration

// 快照与 timeline.updated（docs/agent/smartcut SCT-06）。
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	timelineapp "github.com/li904338203/tabtv/backend/internal/timeline/app"
	"github.com/li904338203/tabtv/backend/internal/timeline/domain"
	timelinerepo "github.com/li904338203/tabtv/backend/internal/timeline/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
)

// recorder 是假发布器：记下每一条事件。
type recorder struct {
	mu     sync.Mutex
	topics []string
	events []timelineapp.TimelineUpdated
}

func (r *recorder) Publish(_ context.Context, topic string, payload []byte) error {
	var ev timelineapp.TimelineUpdated
	if err := json.Unmarshal(payload, &ev); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.topics = append(r.topics, topic)
	r.events = append(r.events, ev)
	return nil
}

func (r *recorder) take() ([]string, []timelineapp.TimelineUpdated) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, e := r.topics, r.events
	r.topics, r.events = nil, nil
	return t, e
}

type fakeShots struct{}

func (fakeShots) ShotClips(context.Context, int64, int64) ([]domain.ShotClip, error) {
	return []domain.ShotClip{{ShotID: 9001, ShotNo: 1, MediaID: 103}}, nil
}

// 每条写路径各发、且只发一条 timeline.updated，reason 对，投递到 project:{id}。
func TestIntegration_每条写路径恰好发一条事件(t *testing.T) {
	e := setup(t)
	rec := &recorder{}
	e.svc.SetEvents(rec)
	e.svc.SetShots(fakeShots{})
	v, err := e.svc.Create(e.ctx, timelineapp.CreateInput{WorkspaceID: ws, ProjectID: 777, ScriptID: 55, AspectRatio: "16:9"})
	if err != nil {
		t.Fatal(err)
	}
	if _, evs := rec.take(); len(evs) != 0 {
		t.Fatalf("建时间线不是写入，不发事件：%d 条", len(evs))
	}
	cur := func() timelineapp.View {
		x, err := e.svc.Get(e.ctx, ws, v.Timeline.ID)
		if err != nil {
			t.Fatal(err)
		}
		return x
	}
	video := func(x timelineapp.View) int64 { return trackOf(t, x, domain.TrackVideo).Track.ID }
	first := func(x timelineapp.View) domain.Clip { return trackOf(t, x, domain.TrackVideo).Clips[0] }
	speed := 200

	steps := []struct {
		name   string
		reason string
		run    func(x timelineapp.View) error
	}{
		{"AddClip", timelineapp.ReasonEdit, func(x timelineapp.View) error {
			_, err := e.svc.AddClip(e.ctx, timelineapp.AddClipInput{WorkspaceID: ws, Editor: alice, TimelineID: x.Timeline.ID,
				TrackID: video(x), Version: x.Timeline.Version, MediaID: 101})
			return err
		}},
		{"AddClip 第二段", timelineapp.ReasonEdit, func(x timelineapp.View) error {
			_, err := e.svc.AddClip(e.ctx, timelineapp.AddClipInput{WorkspaceID: ws, Editor: alice, TimelineID: x.Timeline.ID,
				TrackID: video(x), Version: x.Timeline.Version, MediaID: 102})
			return err
		}},
		{"TrimClip", timelineapp.ReasonEdit, func(x timelineapp.View) error {
			_, err := e.svc.TrimClip(e.ctx, timelineapp.TrimInput{WorkspaceID: ws, Editor: alice, TimelineID: x.Timeline.ID,
				TrackID: video(x), ClipID: first(x).ID, Version: x.Timeline.Version, SourceInMs: 0, SourceOutMs: 3000})
			return err
		}},
		{"SplitClip", timelineapp.ReasonEdit, func(x timelineapp.View) error {
			_, err := e.svc.SplitClip(e.ctx, timelineapp.SplitInput{WorkspaceID: ws, Editor: alice, TimelineID: x.Timeline.ID,
				TrackID: video(x), ClipID: first(x).ID, Version: x.Timeline.Version, AtMs: 1000})
			return err
		}},
		{"MoveClip", timelineapp.ReasonEdit, func(x timelineapp.View) error {
			_, err := e.svc.MoveClip(e.ctx, timelineapp.MoveInput{WorkspaceID: ws, Editor: alice, TimelineID: x.Timeline.ID,
				TrackID: video(x), ClipID: first(x).ID, Version: x.Timeline.Version, ToPosition: 2})
			return err
		}},
		{"UpdateClip", timelineapp.ReasonEdit, func(x timelineapp.View) error {
			_, err := e.svc.UpdateClip(e.ctx, timelineapp.ClipPropsInput{WorkspaceID: ws, Editor: alice, TimelineID: x.Timeline.ID,
				TrackID: video(x), ClipID: first(x).ID, Version: x.Timeline.Version, Props: domain.ClipProps{Speed: &speed}})
			return err
		}},
		{"RemoveClip", timelineapp.ReasonEdit, func(x timelineapp.View) error {
			_, err := e.svc.RemoveClip(e.ctx, ws, x.Timeline.ID, video(x), first(x).ID, x.Timeline.Version, alice)
			return err
		}},
		{"UpdateTrack", timelineapp.ReasonEdit, func(x timelineapp.View) error {
			_, err := e.svc.UpdateTrack(e.ctx, ws, x.Timeline.ID, trackOf(t, x, domain.TrackAudio).Track.ID,
				x.Timeline.Version, alice, true, 50)
			return err
		}},
		{"UpdateMeta", timelineapp.ReasonEdit, func(x timelineapp.View) error {
			_, err := e.svc.UpdateMeta(e.ctx, ws, x.Timeline.ID, x.Timeline.Version, alice, "改名", "", 0, 0, 0)
			return err
		}},
		{"SaveSubtitles", timelineapp.ReasonSubtitles, func(x timelineapp.View) error {
			_, err := e.svc.SaveSubtitles(e.ctx, ws, x.Timeline.ID, x.Timeline.Version, alice,
				[]timelinerepo.Subtitle{{StartMs: 0, EndMs: 1000, Text: "一"}})
			return err
		}},
		{"Assemble", timelineapp.ReasonAssemble, func(x timelineapp.View) error {
			_, err := e.svc.Assemble(e.ctx, timelineapp.AssembleInput{WorkspaceID: ws, Editor: alice,
				TimelineID: x.Timeline.ID, Version: x.Timeline.Version})
			return err
		}},
		{"SaveContent", timelineapp.ReasonSave, func(x timelineapp.View) error {
			_, err := e.save(contentOf(t, x), alice, x.Timeline.ID)
			return err
		}},
		{"SaveContent 带快照", timelineapp.ReasonSave, func(x timelineapp.View) error {
			raw, _ := json.Marshal(contentOf(t, x))
			c, err := domain.DecodeContent(raw)
			if err != nil {
				return err
			}
			_, err = e.svc.SaveContent(e.ctx, timelineapp.SaveContentInput{WorkspaceID: ws, TimelineID: x.Timeline.ID,
				Editor: alice, Content: c, Snapshot: &timelineapp.SnapshotSpec{Reason: timelinerepo.SnapshotAIPlan}})
			return err
		}},
		{"RestoreSnapshot", timelineapp.ReasonRestore, func(x timelineapp.View) error {
			list, err := e.svc.ListSnapshots(e.ctx, ws, x.Timeline.ID)
			if err != nil || len(list) == 0 {
				return fmt.Errorf("没有快照：%v", err)
			}
			_, err = e.svc.RestoreSnapshot(e.ctx, timelineapp.RestoreInput{WorkspaceID: ws, TimelineID: x.Timeline.ID,
				SnapshotID: list[0].ID, BaseVersion: x.Timeline.Version, Editor: alice})
			return err
		}},
	}
	for _, st := range steps {
		before := cur()
		if err := st.run(before); err != nil {
			t.Fatalf("%s：%v", st.name, err)
		}
		topics, evs := rec.take()
		if len(evs) != 1 {
			t.Fatalf("%s 应恰好发一条事件，实际 %d 条", st.name, len(evs))
		}
		ev := evs[0]
		if ev.Reason != st.reason || ev.Version != before.Timeline.Version+1 || ev.Type != timelineapp.TimelineUpdatedType ||
			ev.EditorKind != "user" || ev.EditorID == nil || *ev.EditorID != "501" ||
			topics[0] != "project:777" || ev.TimelineID != fmt.Sprint(v.Timeline.ID) {
			t.Fatalf("%s 的事件不对：%s %+v", st.name, topics[0], ev)
		}
	}
	// 失败的写入不发事件（版本冲突）。
	x := cur()
	_, err = e.svc.UpdateTrack(e.ctx, ws, x.Timeline.ID, trackOf(t, x, domain.TrackAudio).Track.ID,
		x.Timeline.Version-1, alice, false, 100)
	if bizErr(t, err).Code != errcode.VersionConflict {
		t.Fatalf("应撞版本冲突：%v", err)
	}
	if _, evs := rec.take(); len(evs) != 0 {
		t.Fatalf("没写成的不该发事件：%d 条", len(evs))
	}
}

// 连续存 25 份快照后只剩最近 20 份。
func TestIntegration_快照只留最近20份(t *testing.T) {
	e := setup(t)
	v := e.create(t)
	for i := 0; i < 25; i++ {
		x, _ := e.svc.Get(e.ctx, ws, v.Timeline.ID)
		raw, _ := json.Marshal(contentOf(t, x))
		c, err := domain.DecodeContent(raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.SaveContent(e.ctx, timelineapp.SaveContentInput{WorkspaceID: ws, TimelineID: v.Timeline.ID,
			Editor: alice, Content: c, Snapshot: &timelineapp.SnapshotSpec{Reason: timelinerepo.SnapshotAIEdit, CallID: int64(i + 1)}}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := e.svc.ListSnapshots(e.ctx, ws, v.Timeline.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != timelinerepo.MaxSnapshots || list[0].CallID != 25 || list[len(list)-1].CallID != 6 {
		t.Fatalf("应只剩第 6~25 份（新的在前）：%d 份，首 %d 尾 %d", len(list), list[0].CallID, list[len(list)-1].CallID)
	}
	if list[0].Version != v.Timeline.Version+24 || list[0].EditorKind != "user" {
		t.Fatalf("快照记的是写入前的版本与编辑者：%+v", list[0])
	}
}

// SC-AC-33（后端部分）+ SC-AC-60：写入前存快照、恢复回到写入前；快照之后删掉的对象重新建出来；
// 素材已被删除的片段照常恢复并列进 missing；恢复前当前内容另存一份 restore 快照。
func TestIntegration_恢复快照回到写入前并报出缺素材(t *testing.T) {
	e := setup(t)
	v := e.create(t)
	v = e.add(t, v, domain.TrackVideo, 101)
	v = e.add(t, v, domain.TrackVideo, 102)
	v = e.add(t, v, domain.TrackAudio, 202)
	sub := trackOf(t, v, domain.TrackSubtitle).Track.ID
	keep := clipByMedia(t, v, domain.TrackVideo, 101)
	anchor, _ := json.Marshal(map[string]any{"clipId": fmt.Sprint(clipByMedia(t, v, domain.TrackVideo, 102).ID),
		"srcStartMs": 0, "srcEndMs": 1000})
	if _, err := e.pool.Exec(e.ctx, `insert into timeline_subtitle (id, timeline_id, track_id, start_ms, end_ms, text, anchor)
		values ($1,$2,$3,4000,5000,'跟随第二段',$4::jsonb)`, e.next(), v.Timeline.ID, sub, string(anchor)); err != nil {
		t.Fatal(err)
	}
	v, _ = e.svc.Get(e.ctx, ws, v.Timeline.ID)

	// 「AI 剪辑」：先存快照，再把时间线剪成只剩第一段、换个画幅。
	c := contentOf(t, v)
	trackIn(c, domain.TrackVideo)["clips"] = trackIn(c, domain.TrackVideo)["clips"].([]any)[:1]
	trackIn(c, domain.TrackAudio)["clips"] = []any{}
	c["aspectRatio"], c["width"], c["height"] = "9:16", 1080, 1920
	raw, _ := json.Marshal(c)
	dc, err := domain.DecodeContent(raw)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := e.svc.SaveContent(e.ctx, timelineapp.SaveContentInput{WorkspaceID: ws, TimelineID: v.Timeline.ID,
		Editor: alice, Content: dc, Snapshot: &timelineapp.SnapshotSpec{Reason: timelinerepo.SnapshotAIPlan, CallID: 42}})
	if err != nil || saved.SnapshotID == 0 {
		t.Fatalf("带快照的保存：%v（快照 %d）", err, saved.SnapshotID)
	}
	if len(saved.View.Subtitles) != 0 || saved.View.Timeline.AspectRatio != "9:16" {
		t.Fatalf("写入后应只剩一段、9:16、没有字幕：%+v", saved.View.Timeline)
	}

	// 第二段的素材在这期间被删了（媒体端口查不到它）。
	delete(media, 102)
	defer func() { media[102] = timelineapp.MediaInfo{Kind: "video", DurationMs: 4000} }()

	// 恢复撞版本冲突：40902。
	if _, err := e.svc.RestoreSnapshot(e.ctx, timelineapp.RestoreInput{WorkspaceID: ws, TimelineID: v.Timeline.ID,
		SnapshotID: saved.SnapshotID, BaseVersion: saved.View.Timeline.Version - 1, Editor: alice}); bizErr(t, err).Code != errcode.VersionConflict {
		t.Fatalf("基线版本过期应 40902：%v", err)
	}
	res, err := e.svc.RestoreSnapshot(e.ctx, timelineapp.RestoreInput{WorkspaceID: ws, TimelineID: v.Timeline.ID,
		SnapshotID: saved.SnapshotID, BaseVersion: saved.View.Timeline.Version, Editor: alice})
	if err != nil {
		t.Fatal(err)
	}
	got := res.View
	if got.Timeline.AspectRatio != "16:9" || len(trackOf(t, got, domain.TrackVideo).Clips) != 2 ||
		len(trackOf(t, got, domain.TrackAudio).Clips) != 1 || len(got.Subtitles) != 1 {
		t.Fatalf("应回到写入前：画幅 %s，画面 %d 段，音频 %d 段，字幕 %d 条", got.Timeline.AspectRatio,
			len(trackOf(t, got, domain.TrackVideo).Clips), len(trackOf(t, got, domain.TrackAudio).Clips), len(got.Subtitles))
	}
	// 保留下来的那一段 ID 不变；被删过的第二段重新建出来（新 ID），跟随它的字幕锚点跟着改过去。
	revived := clipByMedia(t, got, domain.TrackVideo, 102)
	if clipByMedia(t, got, domain.TrackVideo, 101).ID != keep.ID || revived.ID == 0 {
		t.Fatalf("保留的片段 ID 应不变：%+v", trackOf(t, got, domain.TrackVideo).Clips)
	}
	if a, ok := domain.ParseAnchor(got.Subtitles[0].Anchor); !ok || a.ClipID != revived.ID {
		t.Fatalf("字幕的锚点应跟着重新建出来的片段：%s（片段 %d）", got.Subtitles[0].Anchor, revived.ID)
	}
	if len(res.Missing) != 1 || res.Missing[0].MediaID != 102 || res.Missing[0].ClipID != fmt.Sprint(revived.ID) ||
		res.Missing[0].TrackKind != domain.TrackVideo {
		t.Fatalf("缺素材的那一段要列出来（恢复后的正式 ID）：%+v", res.Missing)
	}
	// 快照列表多一条 restore（恢复前的内容），且它就是返回的 snapshotId。
	list, _ := e.svc.ListSnapshots(e.ctx, ws, v.Timeline.ID)
	if len(list) != 2 || list[0].Reason != timelinerepo.SnapshotRestore || list[0].ID != res.SnapshotID ||
		list[1].Reason != timelinerepo.SnapshotAIPlan || list[1].CallID != 42 {
		t.Fatalf("快照列表：%+v", list)
	}
	// 再撤销一次（恢复 restore 那一份）就回到恢复前：只剩一段、9:16。
	media[102] = timelineapp.MediaInfo{Kind: "video", DurationMs: 4000}
	back, err := e.svc.RestoreSnapshot(e.ctx, timelineapp.RestoreInput{WorkspaceID: ws, TimelineID: v.Timeline.ID,
		SnapshotID: res.SnapshotID, BaseVersion: got.Timeline.Version, Editor: alice})
	if err != nil || back.View.Timeline.AspectRatio != "9:16" || len(trackOf(t, back.View, domain.TrackVideo).Clips) != 1 {
		t.Fatalf("撤销恢复应回到恢复前：%v", err)
	}
	// 别的时间线的快照取不到。
	other := e.create(t)
	if _, err := e.svc.RestoreSnapshot(e.ctx, timelineapp.RestoreInput{WorkspaceID: ws, TimelineID: other.Timeline.ID,
		SnapshotID: saved.SnapshotID, BaseVersion: other.Timeline.Version, Editor: alice}); bizErr(t, err).Code != errcode.NotFound {
		t.Fatalf("别的时间线的快照应 404：%v", err)
	}
}
