//go:build integration

// 素材过期与原位替换（docs/agent/smartcut SCT-30）：SC-AC-46、SC-AC-47。
package integration

import (
	"fmt"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/platform/config"
	smartcutapp "github.com/li904338203/tabtv/backend/internal/smartcut/app"
	timelineapp "github.com/li904338203/tabtv/backend/internal/timeline/app"
	"github.com/li904338203/tabtv/backend/internal/timeline/domain"
	timelinerepo "github.com/li904338203/tabtv/backend/internal/timeline/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
)

func (e planEnv) summary(t *testing.T) smartcutapp.Summary {
	t.Helper()
	list, err := e.svc.List(e.ctx, ws, e.pid)
	if err != nil || len(list) != 1 {
		t.Fatalf("摘要：%+v %v", list, err)
	}
	return list[0]
}

// SC-AC-46：上游重新生成后同步 → 素材有变化、那条 changed；原位替换：换成新产物、保留裁切与转场、原素材进备选、
// 新产物更短时报 shortened、快照 replace_changed；之后「素材有变化」消失；再替换一次是空操作。
func TestReplaceChanged_原位替换变了的素材(t *testing.T) {
	e := setupPlan(t, config.Smartcut{})
	// 另起一个项目与智能剪辑节点（setupPlan 铺上的那几段不要），上游一个生成节点、一个上传节点。
	e.pid, e.tid = e.newSmartcut(t)
	oldMedia := e.mediaOf(t, ws, "video/mp4", 5000)
	gen1 := e.generation(t, ws, e.pid, "success", 9302, oldMedia)
	other := e.mediaOf(t, ws, "video/mp4", 4000)
	canvas := func(gen int64) {
		e.setCanvas(t, e.pid, []map[string]any{nodeWith("sc", "smartcut", nil), nodeWith("g", "video", genResult(gen)),
			nodeWith("u", "video", mediaResult(other))}, []map[string]any{edgeTo("g", 1), edgeTo("u", 2)})
	}
	canvas(gen1)
	if _, err := e.svc.Sync(e.ctx, ws, e.pid, e.tid, smartcutapp.SyncAutofill); err != nil {
		t.Fatal(err)
	}
	// 用户裁了第一段、给第二段加了转场。
	v, _ := e.tl.Get(e.ctx, ws, e.tid)
	c := timelineapp.ContentFromView(v)
	for i := range c.Tracks {
		if c.Tracks[i].Kind == domain.TrackVideo {
			c.Tracks[i].Clips[0].SourceInMs, c.Tracks[i].Clips[0].SourceOutMs = 1000, 4000
			c.Tracks[i].Clips[1].TransitionIn = &domain.Transition{Kind: "dissolve", DurationMs: 400}
		}
	}
	saved, err := e.tl.SaveContent(e.ctx, timelineapp.SaveContentInput{WorkspaceID: ws, TimelineID: e.tid,
		Editor: timelinerepo.Editor{UserID: planUser, Kind: timelinerepo.EditorUser}, Content: c})
	if err != nil {
		t.Fatal(err)
	}
	if e.summary(t).InputsStale {
		t.Fatal("还没变")
	}
	// 上游重新生成：节点的 result 换成新的一次生成，产物更短（3 秒）。
	newMedia := e.mediaOf(t, ws, "video/mp4", 3000)
	gen2 := e.generation(t, ws, e.pid, "success", 9302, newMedia)
	canvas(gen2)
	synced, err := e.svc.Sync(e.ctx, ws, e.pid, e.tid, smartcutapp.SyncListOnly)
	if err != nil {
		t.Fatal(err)
	}
	if !e.summary(t).InputsStale {
		t.Fatal("同步之后：素材有变化")
	}
	changed := map[string]bool{}
	for _, it := range synced.Inputs {
		changed[it.SourceNodeID] = it.Changed
	}
	if !changed["g"] || changed["u"] {
		t.Fatalf("只有重新生成的那条 changed：%v", changed)
	}
	res, err := e.svc.ReplaceChanged(e.ctx, smartcutapp.ReplaceRequest{WorkspaceID: ws, ProjectID: e.pid, TimelineID: e.tid,
		BaseVersion: saved.View.Timeline.Version, Actor: smartcutapp.Actor{UserID: planUser}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ReplacedClipIDs) != 1 || len(res.Shortened) != 1 || res.Shortened[0].FromMs != 4000 || res.Shortened[0].ToMs != 3000 {
		t.Fatalf("换一段、截短到 3 秒：%+v", res)
	}
	clips := e.videoClips(t)
	if clips[0].MediaID != newMedia || clips[0].SourceInMs != 1000 || clips[0].SourceOutMs != 3000 ||
		len(clips[0].Alternates) != 1 || clips[0].Alternates[0].MediaID != oldMedia || clips[0].Alternates[0].SourceOutMs != 4000 ||
		clips[0].Alternates[0].Kind != domain.AlternateOriginal {
		t.Fatalf("保留裁切、原素材连同原区间进备选：%+v", clips[0])
	}
	if clips[1].MediaID != other || !clips[1].TransitionIn.IsActive() {
		t.Fatalf("其它片段与转场不动：%+v", clips[1])
	}
	snaps, _ := e.tl.ListSnapshots(e.ctx, ws, e.tid)
	if len(snaps) == 0 || snaps[0].Reason != "replace_changed" {
		t.Fatalf("快照 replace_changed：%+v", snaps)
	}
	if sm := e.summary(t); sm.InputsStale {
		t.Fatal("换完之后剪辑依据的素材就是现在这一份")
	}
	again, err := e.svc.ReplaceChanged(e.ctx, smartcutapp.ReplaceRequest{WorkspaceID: ws, ProjectID: e.pid, TimelineID: e.tid,
		BaseVersion: res.TimelineVersion, Actor: smartcutapp.Actor{UserID: planUser}})
	if err != nil || len(again.ReplacedClipIDs) != 0 || again.TimelineVersion != res.TimelineVersion {
		t.Fatalf("没有变化时是空操作、不进位：%+v %v", again, err)
	}
	if _, err := e.svc.ReplaceChanged(e.ctx, smartcutapp.ReplaceRequest{WorkspaceID: ws, ProjectID: e.pid, TimelineID: e.tid,
		BaseVersion: 1, Actor: smartcutapp.Actor{UserID: planUser}}); bizCode(err) != errcode.VersionConflict {
		t.Fatalf("版本对不上：40902：%v", err)
	}
	var calls, renders int
	_ = e.pool.QueryRow(e.ctx, `select count(*) from smartcut_llm_call`).Scan(&calls)
	_ = e.pool.QueryRow(e.ctx, `select count(*) from render_job`).Scan(&renders)
	if calls != 0 || renders != 0 {
		t.Fatalf("不自动重剪、不自动导出：%d %d", calls, renders)
	}
}

// SC-AC-47：导出后改一处并保存 → 成片未更新；重新导出成功后变回 false。从来没导出过时为 false。
func TestRenderStale_成片未更新(t *testing.T) {
	e := setupPlan(t, config.Smartcut{})
	if e.summary(t).RenderStale {
		t.Fatal("从来没导出过时为 false")
	}
	render := func(version int) {
		e.exec(t, `insert into render_job (id, workspace_id, timeline_id, timeline_version, snapshot, payer_account_id,
			created_by_user_id, status, progress, output_media_id) values ($1,$2,$3,$4,'{}'::jsonb,1,$5,'success',100,$6)`,
			e.next(), ws, e.tid, version, planUser, e.media[0])
	}
	render(e.currentVersion(t))
	if e.summary(t).RenderStale {
		t.Fatal("刚导出：不过期")
	}
	v, _ := e.tl.Get(e.ctx, ws, e.tid)
	if _, err := e.tl.SaveContent(e.ctx, timelineapp.SaveContentInput{WorkspaceID: ws, TimelineID: e.tid,
		Editor: timelinerepo.Editor{UserID: planUser, Kind: timelinerepo.EditorUser}, Content: keepFirstVideo(timelineapp.ContentFromView(v))}); err != nil {
		t.Fatal(err)
	}
	if !e.summary(t).RenderStale {
		t.Fatal("改了一处：成片未更新")
	}
	render(e.currentVersion(t))
	if e.summary(t).RenderStale {
		t.Fatal("重新导出成功后变回 false")
	}
	_ = fmt.Sprint
}
