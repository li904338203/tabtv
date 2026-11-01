//go:build integration

// 追加指令与多方案（docs/agent/smartcut SCT-29）：SC-AC-44、SC-AC-45（后端部分），模型回复用测试替身固定。
package integration

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/platform/config"
	smartcutapp "github.com/li904338203/tabtv/backend/internal/smartcut/app"
	smartcutdomain "github.com/li904338203/tabtv/backend/internal/smartcut/domain"
	timelineapp "github.com/li904338203/tabtv/backend/internal/timeline/app"
	"github.com/li904338203/tabtv/backend/internal/timeline/domain"
	timelinerepo "github.com/li904338203/tabtv/backend/internal/timeline/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// planned 先 AI 剪一版（三段，origin=ai），再模拟用户亲手改过第一段（origin=user）。返回当前版本。
func (e planEnv) planned(t *testing.T) int {
	t.Helper()
	if _, err := e.svc.Plan(e.ctx, e.req(smartcutdomain.ModeMix)); err != nil {
		t.Fatal(err)
	}
	v, err := e.tl.Get(e.ctx, ws, e.tid)
	if err != nil {
		t.Fatal(err)
	}
	c := timelineapp.ContentFromView(v)
	for i := range c.Tracks {
		if c.Tracks[i].Kind == domain.TrackVideo {
			c.Tracks[i].Clips[0].Origin = domain.OriginUser
		}
	}
	res, err := e.tl.SaveContent(e.ctx, timelineapp.SaveContentInput{WorkspaceID: ws, TimelineID: e.tid,
		Editor: timelinerepo.Editor{UserID: planUser, Kind: timelinerepo.EditorUser}, Content: c})
	if err != nil {
		t.Fatal(err)
	}
	return res.View.Timeline.Version
}

func (e planEnv) videoClips(t *testing.T) []domain.Clip {
	t.Helper()
	v, err := e.tl.Get(e.ctx, ws, e.tid)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range v.Tracks {
		if tr.Track.Kind == domain.TrackVideo {
			return tr.Clips
		}
	}
	return nil
}

func (e planEnv) editReq(instruction string, base int) smartcutapp.EditRequest {
	return smartcutapp.EditRequest{WorkspaceID: ws, ProjectID: e.pid, TimelineID: e.tid,
		Actor: smartcutapp.Actor{UserID: planUser}, Instruction: instruction, BaseVersion: base}
}

// SC-AC-44：「把第 3 段换短一点」：只有第 3 段的取用区间变短；用户改过的第 1 段不变并说明；改动清单；快照 ai_edit，可撤销。
func TestEdit_只改被提到的AI片段(t *testing.T) {
	e := setupPlan(t, config.Smartcut{})
	base := e.planned(t)
	before := e.videoClips(t)
	e.text.replies = []string{`{"ops":[{"op":"trim","clip":"c3","take":"head","ratio":0.5},
		{"op":"trim","clip":"c1","take":"head","ratio":0.5}],"summary":"第 3 段改短"}`}
	res, err := e.svc.Edit(e.ctx, e.editReq("把第 3 段换短一点", base))
	if err != nil {
		t.Fatal(err)
	}
	after := e.videoClips(t)
	if len(after) != 3 || after[2].SourceLen() != before[2].SourceLen()/2 {
		t.Fatalf("第 3 段取前一半：%+v → %+v", before[2], after[2])
	}
	for _, i := range []int{0, 1} {
		if after[i].SourceInMs != before[i].SourceInMs || after[i].SourceOutMs != before[i].SourceOutMs {
			t.Fatalf("第 %d 段不动：%+v → %+v", i+1, before[i], after[i])
		}
	}
	if len(res.Changes) != 1 || !strings.HasPrefix(res.Changes[0], "第 3 段：只取前 50%") {
		t.Fatalf("改动清单：%v", res.Changes)
	}
	skipped := false
	for _, w := range res.Variants[0].Warnings {
		skipped = skipped || w == "第 1 段是你改过的，没有动"
	}
	if !skipped || res.Variants[0].Summary != "第 3 段改短" {
		t.Fatalf("说明跳过了用户改过的：%+v", res.Variants[0])
	}
	req := e.text.requests()[len(e.text.requests())-1]
	if !strings.Contains(req.User, "- c1：素材编号 2（视频）") || !strings.Contains(req.User, "你改过（不要改）") ||
		!strings.Contains(req.System, "模式：edit") {
		t.Fatalf("片段清单交给模型：%s", req.User)
	}
	snaps, _ := e.tl.ListSnapshots(e.ctx, ws, e.tid)
	if len(snaps) == 0 || snaps[0].Reason != "ai_edit" || snaps[0].ID != res.SnapshotID {
		t.Fatalf("写入前存 ai_edit 快照：%+v", snaps)
	}
	var kind string
	_ = e.pool.QueryRow(e.ctx, `select kind from smartcut_llm_call where id = $1`, res.CallID).Scan(&kind)
	if kind != "edit" {
		t.Fatalf("调用记录的种类：%s", kind)
	}
	// 撤销：恢复 ai_edit 快照回到改之前。
	if _, err := e.tl.RestoreSnapshot(e.ctx, timelineapp.RestoreInput{WorkspaceID: ws, TimelineID: e.tid,
		SnapshotID: snaps[0].ID, BaseVersion: base + 1, Editor: timelinerepo.Editor{UserID: planUser, Kind: timelinerepo.EditorUser}}); err != nil {
		t.Fatal(err)
	}
	if got := e.videoClips(t); got[2].SourceLen() != before[2].SourceLen() {
		t.Fatalf("可撤销：%+v", got[2])
	}
}

// 修改全落在用户改过的片段上 / 模型给不出可执行的修改：不写时间线、不存快照，结果里说清。
func TestEdit_没有可执行的修改(t *testing.T) {
	e := setupPlan(t, config.Smartcut{})
	base := e.planned(t)
	e.text.replies = []string{`{"ops":[{"op":"delete","clip":"c1"}]}`}
	res, err := e.svc.Edit(e.ctx, e.editReq("删掉第一段", base))
	if err != nil {
		t.Fatal(err)
	}
	if res.AppliedVariant != nil || len(res.Changes) != 0 || len(res.Timeline) != 0 || e.currentVersion(t) != base {
		t.Fatalf("只碰用户改过的片段：不写：%+v", res)
	}
	e.text.replies = []string{"好的，我已经改好了。", "改好了"}
	res, err = e.svc.Edit(e.ctx, e.editReq("随便改改", base))
	if err != nil {
		t.Fatal(err)
	}
	if res.Variants[0].Status != "failed" || !strings.Contains(res.Variants[0].Error, "AI 没能给出可执行的修改") ||
		e.currentVersion(t) != base {
		t.Fatalf("解析不了：不兜底、不写：%+v", res.Variants[0])
	}
	if snaps, _ := e.tl.ListSnapshots(e.ctx, ws, e.tid); len(snaps) != 1 {
		t.Fatalf("两次都没写，只有那轮 AI 剪辑的快照：%d", len(snaps))
	}
}

// 追加指令期间被改过：40902 带 callId；用它带 force「仍然应用」，按当前内容重放原子操作。
func TestEdit_冲突与仍然应用(t *testing.T) {
	e := setupPlan(t, config.Smartcut{})
	base := e.planned(t)
	e.text.replies = []string{`{"ops":[{"op":"speed","clip":"c2","value":2}]}`}
	e.text.during = func() {
		v, _ := e.tl.Get(e.ctx, ws, e.tid)
		c := timelineapp.ContentFromView(v)
		track := textTrack(c)
		if track == "" {
			// 默认的时间线没有文字轨：另一个会话加文字时会先加一条。
			track = "tmp-tt"
			c.Tracks = append(c.Tracks, domain.ContentTrack{ID: track, Kind: domain.TrackText})
		}
		c.Texts = append(c.Texts, domain.ContentText{ID: "tmp-x", TrackID: track, StartMs: 0, EndMs: 1000, Text: "别人加的"})
		if _, err := e.tl.SaveContent(e.ctx, timelineapp.SaveContentInput{WorkspaceID: ws, TimelineID: e.tid,
			Editor: timelinerepo.Editor{UserID: 777, Kind: timelinerepo.EditorUser}, Content: c}); err != nil {
			t.Error(err)
		}
	}
	_, err := e.svc.Edit(e.ctx, e.editReq("第二段快一点", base))
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.VersionConflict || be.Details["callId"] == nil {
		t.Fatalf("期望 40902 带 callId：%v", err)
	}
	var callID int64
	fmt.Sscan(be.Details["callId"].(string), &callID)
	res, err := e.svc.ApplyVariant(e.ctx, smartcutapp.ApplyRequest{WorkspaceID: ws, ProjectID: e.pid, TimelineID: e.tid,
		CallID: callID, BaseVersion: base, Force: true, Actor: smartcutapp.Actor{UserID: planUser}})
	if err != nil {
		t.Fatal(err)
	}
	clips := e.videoClips(t)
	if clips[1].Speed != 200 {
		t.Fatalf("按当前内容重放：第二段 2 倍速：%+v", clips[1])
	}
	vt, _ := e.tl.Get(e.ctx, ws, e.tid)
	found := false
	for _, tx := range vt.Texts {
		found = found || tx.Text == "别人加的"
	}
	if !found || res.SnapshotID == 0 {
		t.Fatalf("重放不丢掉别人刚加的文字（不是拿旧内容整份覆盖）：%v", found)
	}
}

func textTrack(c domain.Content) string {
	for _, tr := range c.Tracks {
		if tr.Kind == domain.TrackText {
			return tr.ID
		}
	}
	return ""
}

// SC-AC-45（后端部分）：一次 3 套：各有摘要；中间那套失败不影响其余两套；切到第 3 套写入并存 ai_variant；整次只计 1 次限流。
func TestPlan_多方案与切换(t *testing.T) {
	e := setupPlan(t, config.Smartcut{})
	e.text.respond = func(in smartcutapp.TextRequest) string {
		switch {
		case strings.Contains(in.User, "这是第 2 套剪法"):
			return "这一套我不会剪。"
		case strings.Contains(in.User, "这是第 3 套剪法"):
			return `{"segments":[{"clip":"3","take":"full","role":"hook","weight":2,"transition":"cut"},
				{"clip":"1","take":"full","role":"end","weight":1,"transition":"cut"}]}`
		}
		return mixReply
	}
	r := e.req(smartcutdomain.ModeMix)
	r.Variants = 3
	res, err := e.svc.Plan(e.ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Variants) != 3 || *res.AppliedVariant != 0 || res.Variants[0].Status != "success" ||
		res.Variants[1].Status != "failed" || !strings.Contains(res.Variants[1].Error, "无法解析") ||
		res.Variants[2].Status != "success" || strings.Join(res.Variants[2].UsedClips, ",") != "3,1" {
		t.Fatalf("三套：%+v", res.Variants)
	}
	var rows int
	_ = e.pool.QueryRow(e.ctx, `select count(*) from smartcut_llm_call where user_id = $1`, planUser).Scan(&rows)
	if rows != 1 {
		t.Fatalf("整次请求只计 1 次：%d", rows)
	}
	cur := e.currentVersion(t)
	if _, err := e.svc.ApplyVariant(e.ctx, smartcutapp.ApplyRequest{WorkspaceID: ws, ProjectID: e.pid, TimelineID: e.tid,
		CallID: res.CallID, Variant: 1, BaseVersion: cur, Actor: smartcutapp.Actor{UserID: planUser}}); bizCode(err) != errcode.InvalidStateTransition {
		t.Fatalf("失败的那一套不能切：%v", err)
	}
	out, err := e.svc.ApplyVariant(e.ctx, smartcutapp.ApplyRequest{WorkspaceID: ws, ProjectID: e.pid, TimelineID: e.tid,
		CallID: res.CallID, Variant: 2, BaseVersion: cur, Actor: smartcutapp.Actor{UserID: planUser}})
	if err != nil {
		t.Fatal(err)
	}
	if got := e.videoMedia(t); fmt.Sprint(got) != fmt.Sprint([]int64{e.media[2], e.media[0]}) || *out.AppliedVariant != 2 {
		t.Fatalf("切到第 3 套：%v", got)
	}
	snaps, _ := e.tl.ListSnapshots(e.ctx, ws, e.tid)
	if len(snaps) != 2 || snaps[0].Reason != "ai_variant" || snaps[1].Reason != "ai_plan" {
		t.Fatalf("切换前存 ai_variant：%+v", snaps)
	}
	_, _, applied, _, _ := e.callRow(t, res.CallID)
	if applied == nil || *applied != 2 {
		t.Fatalf("调用记录记下当前方案：%v", applied)
	}
	if len(e.text.requests()) != 4 {
		// 第 2 套解析不了会回灌修一次：3 + 1 次调用；切换不再调模型。
		t.Fatalf("调用次数：%d", len(e.text.requests()))
	}
	// 别的时间线 / 不存在的调用：404。
	if _, err := e.svc.ApplyVariant(e.ctx, smartcutapp.ApplyRequest{WorkspaceID: ws, ProjectID: e.pid, TimelineID: e.tid,
		CallID: 999, Variant: 0, BaseVersion: e.currentVersion(t), Actor: smartcutapp.Actor{UserID: planUser}}); bizCode(err) != errcode.NotFound {
		t.Fatalf("不存在的调用：%v", err)
	}
	if _, err := e.svc.ApplyVariant(e.ctx, smartcutapp.ApplyRequest{WorkspaceID: ws, ProjectID: e.pid, TimelineID: e.tid,
		CallID: res.CallID, Variant: 5, BaseVersion: e.currentVersion(t), Actor: smartcutapp.Actor{UserID: planUser}}); bizCode(err) != errcode.NotFound {
		t.Fatalf("不存在的方案：%v", err)
	}
}
