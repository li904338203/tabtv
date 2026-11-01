package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 智能剪辑工具（SCT-41）的单测：有状态的假画布（按操作真的改节点与连线）+ 假智能剪辑端口。
// 真写入、事件与来源在集成测试里验（smartcut/app/integration：TestAgentSmartcut_端到端写入与事件）。

// scCanvas 是有状态的假画布：add_node / update_node / add_edge 真的改内容，读到的是改过之后的。
type scCanvas struct {
	nodes   []map[string]any
	edges   []map[string]any
	applied []app.ApplyOpsInput
	seq     int
	// notOwned 往这些节点上接线时回「不是本会话的节点」。
	notOwned map[string]bool
}

func (c *scCanvas) data() json.RawMessage {
	b, _ := json.Marshal(map[string]any{"nodes": c.nodes, "edges": c.edges})
	return b
}

func (c *scCanvas) Read(context.Context, int64, int64) (app.CanvasView, error) {
	raw := c.data()
	nodes, edges := app.SummarizeCanvas(raw)
	return app.CanvasView{Version: len(c.applied) + 1, CanvasData: raw, Nodes: nodes, EdgeCount: edges}, nil
}

func (c *scCanvas) Apply(_ context.Context, in app.ApplyOpsInput) (app.ApplyOpsResult, error) {
	var ops []struct {
		Kind   string         `json:"kind"`
		NodeID string         `json:"nodeId"`
		Node   map[string]any `json:"node"`
		Patch  map[string]any `json:"patch"`
		Edge   map[string]any `json:"edge"`
	}
	if err := json.Unmarshal(in.Ops, &ops); err != nil {
		return app.ApplyOpsResult{}, err
	}
	var created []string
	for _, op := range ops {
		if op.Kind == "add_edge" && c.notOwned[op.Edge["target"].(string)] {
			return app.ApplyOpsResult{}, app.ErrNodeNotOwned
		}
	}
	c.applied = append(c.applied, in)
	for _, op := range ops {
		switch op.Kind {
		case "add_node":
			c.seq++
			id := fmt.Sprintf("a_new%d", c.seq)
			op.Node["id"] = id
			c.nodes = append(c.nodes, op.Node)
			created = append(created, id)
		case "update_node":
			for _, n := range c.nodes {
				if n["id"] == op.NodeID {
					params, _ := n["params"].(map[string]any)
					if params == nil {
						params = map[string]any{}
					}
					if p, ok := op.Patch["params"].(map[string]any); ok {
						for k, v := range p {
							params[k] = v
						}
					}
					n["params"] = params
				}
			}
		case "add_edge":
			c.edges = append(c.edges, op.Edge)
		}
	}
	return app.ApplyOpsResult{NodeIDs: created}, nil
}

func (c *scCanvas) opsOf(i int) string { return string(c.applied[i].Ops) }

// fakeSmartcuts 按（节点）幂等建时间线；owners 是本项目里已有的时间线 → 绑定的节点。
type fakeSmartcuts struct {
	owners   map[int64]string
	creates  []string
	binds    []string
	syncs    []int64
	aspects  []string
	plans    []app.SmartcutPlanRequest
	planErr  error
	syncErr  error
	detail   app.SmartcutDetail
	arranged bool
}

func (f *fakeSmartcuts) CreateTimeline(_ context.Context, _, _ int64, nodeID string, source int64) (int64, error) {
	for id, owner := range f.owners {
		if owner == nodeID {
			return id, nil
		}
	}
	if f.owners == nil {
		f.owners = map[int64]string{}
	}
	id := int64(8007199254740001 + len(f.creates))
	f.owners[id] = nodeID
	f.creates = append(f.creates, fmt.Sprintf("%s<-%d", nodeID, source))
	return id, nil
}

func (f *fakeSmartcuts) TimelineNode(_ context.Context, _, _, timelineID int64) (string, error) {
	if owner, ok := f.owners[timelineID]; ok {
		return owner, nil
	}
	return "", app.ErrSmartcutTimelineNotFound
}

func (f *fakeSmartcuts) BindTimeline(_ context.Context, _, _, timelineID int64, nodeID string) error {
	if f.owners == nil {
		f.owners = map[int64]string{}
	}
	f.owners[timelineID] = nodeID
	f.binds = append(f.binds, fmt.Sprintf("%d->%s", timelineID, nodeID))
	return nil
}

func (f *fakeSmartcuts) SetAspectRatio(_ context.Context, _, _, timelineID int64, aspect string) error {
	f.aspects = append(f.aspects, fmt.Sprintf("%d:%s", timelineID, aspect))
	return nil
}

func (f *fakeSmartcuts) Sync(_ context.Context, _, _, timelineID int64) (app.SmartcutSyncOutcome, error) {
	f.syncs = append(f.syncs, timelineID)
	if f.syncErr != nil {
		return app.SmartcutSyncOutcome{}, f.syncErr
	}
	if f.arranged {
		return app.SmartcutSyncOutcome{Unused: 2, Summary: app.SmartcutSummary{TimelineID: timelineID, Version: 9,
			IsArranged: true, ClipCount: 3, EffectiveMs: 9000}}, nil
	}
	return app.SmartcutSyncOutcome{Appended: 2, Summary: app.SmartcutSummary{TimelineID: timelineID, Version: 2,
		ClipCount: 2, TotalMs: 8000, EffectiveMs: 8000}}, nil
}

func (f *fakeSmartcuts) Detail(_ context.Context, _, _, timelineID int64) (app.SmartcutDetail, error) {
	d := f.detail
	d.TimelineID = timelineID
	return d, nil
}

func (f *fakeSmartcuts) Plan(_ context.Context, in app.SmartcutPlanRequest) (app.SmartcutPlanOutcome, error) {
	f.plans = append(f.plans, in)
	if f.planErr != nil {
		return app.SmartcutPlanOutcome{}, f.planErr
	}
	return app.SmartcutPlanOutcome{CallID: 7001, AppliedVariant: 0, SnapshotID: 7002,
		Variants: []app.SmartcutVariant{{Index: 0, Status: "success", Summary: "按节拍剪成 15 秒", TotalMs: 15000,
			Warnings: []string{"素材不够，第 3 段用了两次"}}},
		Summary: app.SmartcutSummary{TimelineID: in.TimelineID, Version: 5, ClipCount: 6, EffectiveMs: 15000}}, nil
}

func scDeps(c *scCanvas, s *fakeSmartcuts) MultiDeps {
	return MultiDeps{Canvas: c, Smartcuts: s, Timelines: &fakeTimelines{}}
}

func scNode(id, typ string, params map[string]any) map[string]any {
	if params == nil {
		params = map[string]any{}
	}
	return map[string]any{"id": id, "type": typ, "title": typ, "params": params}
}

// ---------------------------------------------------------------- smartcut_create

// SC-F-160：建节点 → 按新节点建时间线 → 写回 timelineId → 按给的顺序连上素材 → 连线即入轨。
func TestSmartcutCreate_连上给定节点(t *testing.T) {
	c := &scCanvas{nodes: []map[string]any{scNode("v1", "video", nil), scNode("i1", "image", nil)}}
	s := &fakeSmartcuts{}
	res, err := SmartcutCreate{scDeps(c, s)}.Run(context.Background(), multiRC(11),
		json.RawMessage(`{"title":"新品剪辑","inputNodeIds":["i1","v1"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.applied) != 3 {
		t.Fatalf("三批：建节点、写回、连线：%d", len(c.applied))
	}
	if add := c.opsOf(0); !strings.Contains(add, `"type":"smartcut"`) || !strings.Contains(add, `"title":"新品剪辑"`) {
		t.Fatalf("建智能剪辑节点：%s", add)
	}
	if len(s.creates) != 1 || s.creates[0] != "a_new1<-0" {
		t.Fatalf("按新节点建时间线、不分叉：%v", s.creates)
	}
	if back := c.opsOf(1); c.applied[1].BatchID != "agent:3:11:1:timeline" ||
		!strings.Contains(back, `"timelineId":"8007199254740001"`) {
		t.Fatalf("独立批次写回 timelineId：%s %s", c.applied[1].BatchID, back)
	}
	edges := c.opsOf(2)
	if c.applied[2].BatchID != "agent:3:11:1:inputs" || strings.Index(edges, `"i1"`) > strings.Index(edges, `"v1"`) ||
		strings.Count(edges, `"target":"a_new1"`) != 2 {
		t.Fatalf("按给的顺序连进新节点：%s %s", c.applied[2].BatchID, edges)
	}
	if len(s.syncs) != 1 || s.syncs[0] != 8007199254740001 {
		t.Fatalf("连上之后铺一次时间线：%v", s.syncs)
	}
	m := contentMap(t, res)
	if m["nodeId"] != "a_new1" || m["timelineId"] != "8007199254740001" || m["appended"] != float64(2) ||
		fmt.Sprint(m["connected"]) != "[i1 v1]" {
		t.Fatalf("结果：%v", m)
	}
}

// 给错的节点：说清是哪个、为什么，而且**一个节点都不建**（不然模型重试时会再建一个）。
func TestSmartcutCreate_给错节点不建节点(t *testing.T) {
	cases := map[string]string{
		`{"inputNodeIds":["nope"]}`:     "画布上没有节点 nope",
		`{"inputNodeIds":["d1"]}`:       "不能连进智能剪辑",
		`{"inputNodeIds":["v1","v1"]}`:  "重复",
		`{"inputNodeIds":["v1"],"x":1}`: "参数格式错误",
	}
	for args, want := range cases {
		c := &scCanvas{nodes: []map[string]any{scNode("v1", "video", nil), scNode("d1", "director", nil)}}
		s := &fakeSmartcuts{}
		_, err := SmartcutCreate{scDeps(c, s)}.Run(context.Background(), multiRC(11), json.RawMessage(args))
		if err == nil || !strings.Contains(err.Error(), want) || len(c.applied) != 0 || len(s.creates) != 0 {
			t.Errorf("%s：应拒绝且不建：%v applied=%d", args, err, len(c.applied))
		}
	}
	// 规划态不能建（只在执行态下发，也不能执行）。
	rc := multiRC(11)
	rc.State = domain.StatePlanning
	if _, err := (SmartcutCreate{scDeps(&scCanvas{}, &fakeSmartcuts{})}).Run(context.Background(), rc,
		json.RawMessage(`{}`)); err == nil {
		t.Fatal("规划态应拒绝")
	}
}

// 铺轨失败不让工具失败：节点与连线已经在画布上了，报错只会让模型再建一个。
func TestSmartcutCreate_铺轨失败照样回节点(t *testing.T) {
	c := &scCanvas{nodes: []map[string]any{scNode("v1", "video", nil)}}
	s := &fakeSmartcuts{syncErr: errors.New("库挂了")}
	res, err := SmartcutCreate{scDeps(c, s)}.Run(context.Background(), multiRC(11),
		json.RawMessage(`{"inputNodeIds":["v1"]}`))
	if err != nil {
		t.Fatal(err)
	}
	m := contentMap(t, res)
	if m["nodeId"] != "a_new1" || !strings.Contains(fmt.Sprint(m["syncError"]), "打开剪辑台时会再铺一次") {
		t.Fatalf("结果：%v", m)
	}
}

// ---------------------------------------------------------------- smartcut_get

// SC-F-161：只读 —— 不建、不写；绑定在自己身上的时间线才读；没有自己的时间线时说清楚（不替它补建）。
func TestSmartcutGet_只读详情(t *testing.T) {
	c := &scCanvas{nodes: []map[string]any{scNode("s1", "smartcut", map[string]any{"timelineId": "77"}),
		scNode("s2", "smartcut", map[string]any{"timelineId": "77"})}}
	render := &app.SmartcutRender{ID: 9, Status: "success", Progress: 100, OutputMediaID: 99}
	s := &fakeSmartcuts{owners: map[int64]string{77: "s1"}, detail: app.SmartcutDetail{
		SmartcutSummary: app.SmartcutSummary{Version: 4, ClipCount: 2, TotalMs: 9000, EffectiveMs: 8500,
			RenderStale: true, LatestRender: render},
		Inputs: []app.SmartcutInput{{No: "1", Kind: "video", Status: "ready", SourceNodeID: "v1", Used: true, DurationMs: 5000},
			{No: "2", Kind: "audio", Status: "failed", SourceNodeID: "a1", Reason: "上游生成失败"}},
		Clips: []app.SmartcutClip{{TrackKind: "video", No: 1, MediaID: 11, SourceNodeID: "v1", Origin: "sync",
			StartMs: 0, DurationMs: 5000}, {TrackKind: "audio", TrackRole: "sfx", No: 1, MediaID: 12, Hidden: true}}}}
	deps := scDeps(c, s)
	res, err := SmartcutGet{deps}.Run(context.Background(), multiRC(12), json.RawMessage(`{"nodeId":"s1"}`))
	if err != nil {
		t.Fatal(err)
	}
	m := contentMap(t, res)
	clips, _ := m["clips"].([]any)
	inputs, _ := m["inputs"].([]any)
	lr, _ := m["latestRender"].(map[string]any)
	if m["timelineId"] != "77" || m["effectiveMs"] != float64(8500) || len(clips) != 2 || len(inputs) != 2 ||
		lr["outputMediaId"] != "99" || m["renderStale"] != true || !strings.Contains(fmt.Sprint(m["next"]), "render") {
		t.Fatalf("详情：%v", m)
	}
	if c2 := clips[1].(map[string]any); c2["hidden"] != true || c2["role"] != "sfx" {
		t.Fatalf("跟随画面被裁掉的音效标 hidden：%v", c2)
	}
	// s2 带着 s1 的时间线（复制出来还没打开过）：不读别人的，也不替它分叉。
	res, err = SmartcutGet{deps}.Run(context.Background(), multiRC(13), json.RawMessage(`{"nodeId":"s2"}`))
	if err != nil {
		t.Fatal(err)
	}
	if m := contentMap(t, res); m["hasTimeline"] != false || !strings.Contains(fmt.Sprint(m["message"]), "smartcut_plan") {
		t.Fatalf("没有自己的时间线：%v", m)
	}
	if len(c.applied) != 0 || len(s.creates) != 0 {
		t.Fatal("只读工具不建、不写")
	}
	for args, want := range map[string]string{`{"nodeId":"x"}`: "画布上没有节点 x", `{}`: "nodeId 必填"} {
		if _, err := (SmartcutGet{deps}).Run(context.Background(), multiRC(15), json.RawMessage(args)); err == nil ||
			!strings.Contains(err.Error(), want) {
			t.Errorf("%s：%v", args, err)
		}
	}
}

// ---------------------------------------------------------------- smartcut_plan

// SC-F-162：来源记 agent（会话与发起人）；按节点取时间线（缺了先补建并写回）；结果与警告交给模型。
func TestSmartcutPlan_来源记agent(t *testing.T) {
	c := &scCanvas{nodes: []map[string]any{scNode("s1", "smartcut", nil)}}
	s := &fakeSmartcuts{}
	rc := multiRC(16)
	rc.MemberLevelID = 8
	res, err := SmartcutPlan{scDeps(c, s)}.Run(context.Background(), rc, json.RawMessage(
		`{"nodeId":"s1","mode":"mix","instruction":" 剪成 15 秒卡点 ","targetSec":15,"aspectRatio":"9:16","variants":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.creates) != 1 || len(c.applied) != 1 || !strings.Contains(c.opsOf(0), `"timelineId":"8007199254740001"`) {
		t.Fatalf("节点还没有时间线：先补建并写回：%v %d", s.creates, len(c.applied))
	}
	if len(s.plans) != 1 {
		t.Fatal("应发起一次 AI 剪辑")
	}
	p := s.plans[0]
	if p.SessionID != 3 || p.UserID != 5 || p.MemberLevelID != 8 || p.TimelineID != 8007199254740001 ||
		p.Mode != "mix" || p.Instruction != "剪成 15 秒卡点" || p.TargetSec != 15 || p.AspectRatio != "9:16" || p.Variants != 2 {
		t.Fatalf("请求：%+v", p)
	}
	m := contentMap(t, res)
	if m["callId"] != "7001" || m["appliedVariant"] != float64(0) || m["clipCount"] != float64(6) ||
		m["repair"] == nil || !strings.Contains(fmt.Sprint(m["warnings"]), "第 3 段用了两次") {
		t.Fatalf("结果：%v", m)
	}
	for args, want := range map[string]string{
		`{"nodeId":"s1","mode":"cut"}`: "mode 只能是",
		`{"mode":"mix"}`:               "nodeId 必填",
	} {
		if _, err := (SmartcutPlan{scDeps(c, s)}).Run(context.Background(), multiRC(17), json.RawMessage(args)); err == nil ||
			!strings.Contains(err.Error(), want) {
			t.Errorf("%s：%v", args, err)
		}
	}
}

// 业务错误原样交给模型（码、原因、详情、该怎么做），不是一句「失败了」。
func TestSmartcutPlan_业务错误原样交给模型(t *testing.T) {
	cases := []struct {
		err      error
		code     string
		wantNext string
	}{
		{httpx.Errf(errcode.RateLimited, "AI 剪辑每小时最多 20 次").WithDetails(map[string]any{"retry_after_seconds": 120}),
			"RATE_LIMITED", "retry_after_seconds"},
		{httpx.Errf(errcode.SmartcutInputUnsuitable, "卡点需要配乐").WithDetails(map[string]any{"reason": "no_music"}),
			"SMARTCUT_INPUT_UNSUITABLE", "补齐素材"},
		{httpx.Errf(errcode.VersionConflict, "AI 剪辑期间时间线有改动").WithDetails(map[string]any{"callId": "7"}),
			"VERSION_CONFLICT", "不会覆盖用户的改动"},
	}
	for _, c := range cases {
		cv := &scCanvas{nodes: []map[string]any{scNode("s1", "smartcut", map[string]any{"timelineId": "77"})}}
		s := &fakeSmartcuts{owners: map[int64]string{77: "s1"}, planErr: c.err}
		_, err := SmartcutPlan{scDeps(cv, s)}.Run(context.Background(), multiRC(18),
			json.RawMessage(`{"nodeId":"s1","mode":"mix","instruction":"卡点"}`))
		var rej *domain.ToolRejection
		if !errors.As(err, &rej) || rej.Code != c.code || !strings.Contains(fmt.Sprint(rej.Details["next"]), c.wantNext) {
			t.Errorf("%s：%v", c.code, err)
			continue
		}
		if c.code == "RATE_LIMITED" && rej.Details["retry_after_seconds"] != 120 {
			t.Errorf("限流要带等多久：%v", rej.Details)
		}
	}
	// 节点不是智能剪辑：说清楚该怎么办，不发起。
	cv := &scCanvas{nodes: []map[string]any{scNode("v1", "video", nil)}}
	s := &fakeSmartcuts{}
	if _, err := (SmartcutPlan{scDeps(cv, s)}).Run(context.Background(), multiRC(19),
		json.RawMessage(`{"nodeId":"v1","mode":"mix"}`)); err == nil || !strings.Contains(err.Error(), "不是智能剪辑") ||
		len(s.plans) != 0 {
		t.Fatalf("不是智能剪辑：%v", err)
	}
}

// ---------------------------------------------------------------- timeline_assemble（SCD-39）

// 走脚本：时间线还没挂在节点上 → 新建智能剪辑节点、绑上、写回、连上画布上的脚本节点；装配的编辑者记 agent。
func TestTimelineAssemble_脚本路建节点(t *testing.T) {
	c := &scCanvas{nodes: []map[string]any{scNode("sc1", "script", map[string]any{"scriptId": "21"})}}
	s := &fakeSmartcuts{}
	tl := &fakeTimelines{plan: app.AssembleOutcome{Summary: "装了 3 镜", Append: 3,
		Timeline: app.TimelineView{ID: 77, Version: 4, ClipCount: 3}}}
	deps := MultiDeps{Canvas: c, Smartcuts: s, Timelines: tl}
	res, err := TimelineAssemble{deps}.Run(context.Background(), multiRC(20), json.RawMessage(`{"scriptId":"21"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.binds) != 1 || s.binds[0] != "77->a_new1" || len(s.creates) != 0 {
		t.Fatalf("把脚本时间线绑到新节点（不另建时间线）：binds=%v creates=%v", s.binds, s.creates)
	}
	if len(c.applied) != 3 || !strings.Contains(c.opsOf(0), `"type":"smartcut"`) ||
		!strings.Contains(c.opsOf(1), `"timelineId":"77"`) || !strings.Contains(c.opsOf(2), `"source":"sc1"`) {
		t.Fatalf("建节点 → 写回 → 连上脚本节点：%d", len(c.applied))
	}
	if tl.assembled != 1 || tl.lastAssemble.EditorKind != "agent" || tl.lastAssemble.UserID != 5 {
		t.Fatalf("装配一次、编辑者记 agent：%+v", tl.lastAssemble)
	}
	if m := contentMap(t, res); m["nodeId"] != "a_new1" || m["timelineId"] != "77" || m["assembled"] != true {
		t.Fatalf("结果多一个 nodeId，原有字段不变：%v", m)
	}
	// 再装一次：已经挂在节点上了，复用，不再建。
	if _, err := (TimelineAssemble{deps}).Run(context.Background(), multiRC(21),
		json.RawMessage(`{"scriptId":"21"}`)); err != nil {
		t.Fatal(err)
	}
	if len(s.binds) != 1 || len(c.applied) != 3 {
		t.Fatalf("已绑定的直接复用：binds=%v applied=%d", s.binds, len(c.applied))
	}
}

// 有缺口不装配时，时间线也已经在画布上了（不然用户看不到它）。
func TestTimelineAssemble_脚本路有缺口也挂到画布上(t *testing.T) {
	c := &scCanvas{}
	s := &fakeSmartcuts{}
	tl := &fakeTimelines{plan: app.AssembleOutcome{Gaps: []app.AssembleGap{{ShotNo: 2, Reason: "视频失败"}}}}
	res, err := TimelineAssemble{MultiDeps{Canvas: c, Smartcuts: s, Timelines: tl}}.Run(context.Background(),
		multiRC(22), json.RawMessage(`{"scriptId":"21"}`))
	if err != nil {
		t.Fatal(err)
	}
	if m := contentMap(t, res); m["assembled"] != false || m["nodeId"] != "a_new1" || tl.assembled != 0 {
		t.Fatalf("不装配，但节点已建：%v", m)
	}
}

// 不接智能剪辑时照旧只装配（不碰画布，结果里没有 nodeId）。
func TestTimelineAssemble_未接智能剪辑照旧装配(t *testing.T) {
	tl := &fakeTimelines{plan: app.AssembleOutcome{Append: 1, Timeline: app.TimelineView{ID: 77}}}
	res, err := TimelineAssemble{MultiDeps{Timelines: tl}}.Run(context.Background(), multiRC(23),
		json.RawMessage(`{"scriptId":"21"}`))
	if err != nil {
		t.Fatal(err)
	}
	if m := contentMap(t, res); m["nodeId"] != nil || tl.assembled != 1 {
		t.Fatalf("结果：%v", m)
	}
}

// AI 剪辑关着（SEEAITV_SMARTCUT_AI_ENABLED，这一版只保留手动剪辑）：不装 smartcut_plan，建节点与查看照常。
func TestMultiTools_AI剪辑关着不装plan(t *testing.T) {
	names := func(d MultiDeps) map[domain.ToolName]bool {
		out := map[domain.ToolName]bool{}
		for _, tool := range MultiTools(d) {
			out[tool.Name()] = true
		}
		return out
	}
	off := names(MultiDeps{SmartcutAIDisabled: true})
	if off[domain.ToolSmartcutPlan] || !off[domain.ToolSmartcutCreate] || !off[domain.ToolSmartcutGet] {
		t.Fatalf("关着时：%v", off)
	}
	if on := names(MultiDeps{}); !on[domain.ToolSmartcutPlan] {
		t.Fatal("开着时应当有 smartcut_plan")
	}
}
