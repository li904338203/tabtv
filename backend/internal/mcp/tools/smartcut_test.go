package tools

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/canvasgen"
	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 智能剪辑工具（SCT-41）的单测：假端口 + 与 transport 相同的四步（call）。时间线真写入、来源 mcp 在集成测试里验
// （smartcut/app/integration：TestMcpSmartcut_建节点剪辑导出）。

type fakeSmartcuts struct {
	created  []app.SmartcutCreateInput
	planned  []app.SmartcutPlanInput
	exports  []string
	planErr  error
	credits  int
	issues   []app.RenderIssue
	noTL     bool
	getCalls int
}

var fakeSummary = app.SmartcutSummary{TimelineID: 9007199254740995, Version: 3, ClipCount: 2, TotalMs: 8000,
	EffectiveMs: 7500, LatestRender: &app.SmartcutRender{ID: 801, Status: "success", Progress: 100, OutputMediaID: 901}}

func (f *fakeSmartcuts) Create(_ context.Context, _ app.Principal, in app.SmartcutCreateInput) (app.SmartcutCreated, error) {
	f.created = append(f.created, in)
	return app.SmartcutCreated{NodeID: "a_sc", TimelineID: 9007199254740995, Connected: in.InputNodeIDs,
		Appended: len(in.InputNodeIDs), Summary: fakeSummary}, nil
}

func (f *fakeSmartcuts) Get(_ context.Context, _ app.Principal, _ int64, nodeID string) (app.SmartcutDetail, error) {
	f.getCalls++
	if nodeID == "zz" {
		return app.SmartcutDetail{}, httpx.Errf(errcode.NotFound, "画布上没有节点 zz")
	}
	if f.noTL {
		return app.SmartcutDetail{NodeID: nodeID}, nil
	}
	return app.SmartcutDetail{NodeID: nodeID, HasTimeline: true, SmartcutSummary: fakeSummary,
		Inputs: []app.SmartcutInput{{No: "1", Kind: "video", Status: "ready", Title: "镜头一", SourceNodeID: "v1",
			DurationMs: 5000, Used: true}},
		Clips: []app.SmartcutClip{{TrackKind: "video", No: 1, MediaID: 11, SourceNodeID: "v1", Origin: "sync",
			DurationMs: 5000}, {TrackKind: "audio", TrackRole: "sfx", No: 1, MediaID: 12, Hidden: true}}}, nil
}

func (f *fakeSmartcuts) Plan(_ context.Context, _ app.Principal, in app.SmartcutPlanInput) (app.SmartcutPlanResult, error) {
	f.planned = append(f.planned, in)
	if f.planErr != nil {
		return app.SmartcutPlanResult{}, f.planErr
	}
	return app.SmartcutPlanResult{TimelineID: 9007199254740995, CallID: 91, AppliedVariant: 0, SnapshotID: 92,
		Variants: []app.SmartcutVariant{{Index: 0, Status: "success", Summary: "15 秒卡点", TotalMs: 15000,
			Warnings: []string{"第 3 段用了两次"}}}, Summary: fakeSummary}, nil
}

func (f *fakeSmartcuts) PrecheckExport(_ context.Context, _ app.Principal, _ int64, _ string, _ app.SmartcutExportSpec) (
	int64, app.RenderPrecheck, error) {
	return 9007199254740995, app.RenderPrecheck{Issues: f.issues, DurationMs: 7500, Credits: f.credits}, nil
}

func (f *fakeSmartcuts) StartExport(_ context.Context, _ app.Principal, _ int64, nodeID string, spec app.SmartcutExportSpec,
	key string) (app.RenderView, error) {
	f.exports = append(f.exports, nodeID+"|"+spec.Resolution+"|"+key)
	return app.RenderView{ID: 802, TimelineID: 9007199254740995, Status: "queued", Credits: f.credits}, nil
}

func smartcutTestDeps(credits int) (Deps, *fakeSmartcuts, *fakeQuoteStore) {
	d, _, _, _, store := multiDeps(100)
	sc := &fakeSmartcuts{credits: credits}
	d.Smartcuts = sc
	return d, sc, store
}

var smartcutToolNames = []string{"smartcut_create", "smartcut_get", "smartcut_plan", "smartcut_export"}

// SC-AC-68：默认端点（画布工具集默认开启）四个都看得到；只读端点只看得到 smartcut_get。
func TestSmartcutTools_只读凭据只见get(t *testing.T) {
	names := listNames(t, connectAs(t, fakeDeps(), ownerAll(), "/mcp", bearerRT{}, nil))
	for _, n := range smartcutToolNames {
		if !slices.Contains(names, n) {
			t.Errorf("/mcp 应可见 %s：%v", n, names)
		}
	}
	names = listNames(t, connectAs(t, fakeDeps(), ownerAll(), "/mcp/readonly", allToolsetsRT{}, nil))
	for _, n := range smartcutToolNames {
		if want := n == "smartcut_get"; slices.Contains(names, n) != want {
			t.Errorf("/mcp/readonly 对 %s 的可见性应为 %v：%v", n, want, names)
		}
	}
}

// 都在画布工具集、单方法；get 只读；create / plan 是不花钱的 edit 写（plan 调外部模型，如实标 openWorldHint）；
// export 是 Paid + Write + export（以报价金额判是否确认）。
func TestSmartcutTools_工具集与动作(t *testing.T) {
	reg := NewRegistry(Deps{})
	for _, c := range []struct {
		name               string
		action             string
		write, paid, world bool
	}{
		{"smartcut_create", "edit", true, false, false},
		{"smartcut_get", "view", false, false, false},
		{"smartcut_plan", "edit", true, false, true},
		{"smartcut_export", "export", true, true, false},
	} {
		tl, ok := reg.Lookup(c.name)
		if !ok {
			t.Fatalf("没有工具 %s", c.name)
		}
		m := tl.Methods
		if tl.Toolset != "canvas" || len(m) != 1 || m[0].Name != "" || string(m[0].Action) != c.action ||
			m[0].Write != c.write || m[0].Paid != c.paid || m[0].OpenWorld != c.world {
			t.Errorf("%s：toolset=%s %+v", c.name, tl.Toolset, m)
		}
	}
}

// 建：同一个 idempotencyKey 同一批（重试只建一个节点）；素材节点按给的顺序交给端口；结果 ID 一律字符串。
func TestSmartcutCreate_幂等批次与连线(t *testing.T) {
	d, sc, _ := smartcutTestDeps(0)
	reg := NewRegistry(d)
	args := `{"projectId":"11","title":"新品","inputNodeIds":["v2"," v1"],"idempotencyKey":"same"}`
	m := callOut(t, reg, "smartcut_create", args)
	callOut(t, reg, "smartcut_create", args)
	callOut(t, reg, "smartcut_create", `{"projectId":"11"}`)
	if m["nodeId"] != "a_sc" || m["timelineId"] != "9007199254740995" || m["appended"] != float64(2) ||
		!strings.Contains(m["message"].(string), "smartcut_plan") {
		t.Fatalf("结果：%v", m)
	}
	if len(sc.created) != 3 || sc.created[0].BatchID != sc.created[1].BatchID || sc.created[2].BatchID == sc.created[0].BatchID ||
		strings.Join(sc.created[0].InputNodeIDs, ",") != "v2,v1" || sc.created[0].ProjectID != 11 {
		t.Fatalf("同键同批、不带键每次新批、素材按顺序：%+v", sc.created)
	}
	if conn, ok := m["alreadyConnected"].([]any); !ok || len(conn) != 0 {
		t.Fatalf("alreadyConnected 应是空数组：%v", m["alreadyConnected"])
	}
	if err := callErr(t, reg, ctxWithPrincipal(), "smartcut_create",
		`{"projectId":"11","inputNodeIds":[`+strings.Repeat(`"x",`, canvasgen.MaxSmartcutInputs)+`"x"]}`); err == nil {
		t.Fatal("素材节点超过上限应拒绝")
	}
}

// 只读详情：素材标题放进 untrusted；没有自己的时间线时说清楚、不报错。
func TestSmartcutGet_详情与没有时间线(t *testing.T) {
	d, sc, _ := smartcutTestDeps(0)
	reg := NewRegistry(d)
	m := callOut(t, reg, "smartcut_get", `{"projectId":"11","nodeId":"s1"}`)
	inputs, _ := m["inputs"].([]any)
	clips, _ := m["clips"].([]any)
	if m["hasTimeline"] != true || len(inputs) != 1 || len(clips) != 2 {
		t.Fatalf("详情：%v", m)
	}
	if in := inputs[0].(map[string]any); in["untrusted"].(map[string]any)["title"] != "镜头一" {
		t.Fatalf("素材标题是用户内容，放进 untrusted：%v", in)
	}
	sc.noTL = true
	m = callOut(t, reg, "smartcut_get", `{"projectId":"11","nodeId":"s2"}`)
	if m["hasTimeline"] != false || !strings.Contains(m["message"].(string), "smartcut_plan") {
		t.Fatalf("没有时间线：%v", m)
	}
	if err := callErr(t, reg, ctxWithPrincipal(), "smartcut_get", `{"projectId":"11","nodeId":"zz"}`); err == nil {
		t.Fatal("节点不存在应报错")
	}
}

// AI 剪辑：入参原样交给端口；结果带写入的那一套、警告与快照；限流原样返回（码与 retry_after_seconds）。
func TestSmartcutPlan_结果与业务错误(t *testing.T) {
	d, sc, _ := smartcutTestDeps(0)
	reg := NewRegistry(d)
	m := callOut(t, reg, "smartcut_plan",
		`{"projectId":"11","nodeId":"s1","mode":"mix","instruction":" 15 秒卡点 ","targetSec":15,"variants":2}`)
	if m["status"] != "completed" || m["callId"] != "91" || m["snapshotId"] != "92" || m["appliedVariant"] != float64(0) {
		t.Fatalf("结果：%v", m)
	}
	if got := sc.planned[0]; got.NodeID != "s1" || got.Mode != "mix" || got.Instruction != "15 秒卡点" ||
		got.TargetSec != 15 || got.Variants != 2 || got.BatchID == "" {
		t.Fatalf("入参：%+v", got)
	}
	if err := callErr(t, reg, ctxWithPrincipal(), "smartcut_plan", `{"projectId":"11","nodeId":"s1","mode":"cut"}`); err == nil ||
		!strings.Contains(err.Error(), "mode 只能是") {
		t.Errorf("剪辑方式不认识：%v", err)
	}
	limited := httpx.Errf(errcode.RateLimited, "本小时的 AI 剪辑次数已用完").WithDetails(map[string]any{"retry_after_seconds": 600})
	sc.planErr = limited
	err := callErr(t, reg, ctxWithPrincipal(), "smartcut_plan", `{"projectId":"11","nodeId":"s1","mode":"mix"}`)
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.RateLimited || be.Details["retry_after_seconds"] != 600 {
		t.Fatalf("限流应原样返回：%v", err)
	}
}

// 导出免费（SCK-3）：0 积分直接导出、不签发 quote；带 idempotencyKey 时键是确定的（重试同一个导出任务）。
func TestSmartcutExport_0积分不确认(t *testing.T) {
	d, sc, store := smartcutTestDeps(0)
	reg := NewRegistry(d)
	args := `{"projectId":"11","nodeId":"s1","resolution":"720p","idempotencyKey":"e1"}`
	m := callOut(t, reg, "smartcut_export", args)
	callOut(t, reg, "smartcut_export", args)
	if m["status"] != "queued" || m["needsConfirmation"] != nil || m["renderId"] != "802" || m["credits"] != float64(0) {
		t.Fatalf("0 积分应直接导出：%v", m)
	}
	if len(store.rows) != 0 || len(sc.exports) != 2 || sc.exports[0] != sc.exports[1] ||
		!strings.HasPrefix(sc.exports[0], "s1|720p|mcp:5:o:") {
		t.Fatalf("不签发 quote；同一个 idempotencyKey 得到同一把键：%v（quote %d 张）", sc.exports, len(store.rows))
	}
}

// 预检没通过：不导出、逐条说出是第几段。
func TestSmartcutExport_预检不过不导出(t *testing.T) {
	d, sc, _ := smartcutTestDeps(0)
	sc.issues = []app.RenderIssue{{Code: "generating", Detail: "第 2 段还在生成", ClipIdx: 2}}
	m := callOut(t, NewRegistry(d), "smartcut_export", `{"projectId":"11","nodeId":"s1"}`)
	issues, _ := m["issues"].([]any)
	if m["status"] != "not_renderable" || len(issues) != 1 || len(sc.exports) != 0 {
		t.Fatalf("预检不过：%v", m)
	}
}

// 运营在后台改了价：以报价金额为准，照常走确认（不按工具名硬编码成免费）。
func TestSmartcutExport_改过价时走确认(t *testing.T) {
	d, sc, store := smartcutTestDeps(5)
	reg := NewRegistry(d)
	args := `{"projectId":"11","nodeId":"s1"}`
	m := callOut(t, reg, "smartcut_export", args)
	if m["status"] != "needs_confirmation" || len(sc.exports) != 0 || len(store.rows) != 1 || store.rows[0].Credits != 5 {
		t.Fatalf("有价时先确认：%v", m)
	}
	m = callOut(t, reg, "smartcut_export", confirmed(args, m))
	if m["status"] != "queued" || len(sc.exports) != 1 {
		t.Fatalf("确认后导出：%v", m)
	}
}

// timeline.render 也一样：0 积分的时间线直接渲染、不进确认（SC-F-163）。
func TestTimelineRender_0积分不确认(t *testing.T) {
	d, _, _, tl, store := multiDeps(100)
	m := callOut(t, NewRegistry(d), "timeline", `{"method":"render","timelineId":"53"}`)
	if m["status"] != "queued" || m["needsConfirmation"] != nil || len(store.rows) != 0 || len(tl.renders) != 1 {
		t.Fatalf("0 积分应直接渲染：%v（quote %d 张）", m, len(store.rows))
	}
}

// AI 剪辑关着（SEEAITV_SMARTCUT_AI_ENABLED，这一版只保留手动剪辑）：smartcut_plan 不注册，其余三个照常。
func TestSmartcutTools_AI关着不注册plan(t *testing.T) {
	reg := NewRegistry(Deps{SmartcutAIDisabled: true})
	if _, ok := reg.Lookup("smartcut_plan"); ok {
		t.Fatal("AI 剪辑关着时不该有 smartcut_plan")
	}
	for _, name := range []string{"smartcut_create", "smartcut_get", "smartcut_export"} {
		if _, ok := reg.Lookup(name); !ok {
			t.Errorf("没有工具 %s", name)
		}
	}
}
