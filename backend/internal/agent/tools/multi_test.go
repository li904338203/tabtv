package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
)

// ---------------------------------------------------------------- 假端口

// fakeBudget 模拟 PaidCall 的「同一步骤已受理 → 回那一次引用、不再执行」与额度检查。
type fakeBudget struct {
	limit  int
	used   int
	byStep map[int64]app.PaidResult
	runs   int
	quotes []int
}

func newFakeBudget(limit int) *fakeBudget {
	return &fakeBudget{limit: limit, byStep: map[int64]app.PaidResult{}}
}

func (b *fakeBudget) PaidCallTool(_ context.Context, rc app.RunContext, _ string, quote int,
	run func(string) (app.PaidResult, error)) (app.PaidResult, *app.Suspension, error) {
	b.quotes = append(b.quotes, quote)
	if prev, ok := b.byStep[rc.StepID]; ok {
		prev.Replayed = true
		return prev, nil, nil
	}
	if b.used+quote > b.limit {
		return app.PaidResult{}, &app.Suspension{Event: domain.EvOverBudget, CardID: 1}, nil
	}
	b.runs++
	res, err := run(domain.PaidIdempotencyKey(rc.SessionID, rc.StepID, 1))
	if err != nil {
		return app.PaidResult{}, nil, err
	}
	b.used += quote
	b.byStep[rc.StepID] = res
	return res, nil, nil
}

type fakeWorkflows struct {
	plan   app.WorkflowPlanView
	starts int
}

func (w *fakeWorkflows) Plan(context.Context, app.WorkflowPlanRequest) (app.WorkflowPlanView, error) {
	return w.plan, nil
}

func (w *fakeWorkflows) Start(context.Context, app.WorkflowStartRequest) (app.WorkflowRunView, error) {
	w.starts++
	return app.WorkflowRunView{ID: int64(900 + w.starts), Status: "running"}, nil
}

func (w *fakeWorkflows) Run(_ context.Context, _ int64, id int64) (app.WorkflowRunView, error) {
	return app.WorkflowRunView{ID: id, Status: "running"}, nil
}

type fakeTimelines struct {
	plan      app.AssembleOutcome
	assembled int
	// lastAssemble 最近一次装配请求（含空跑）。
	lastAssemble app.AssembleRequest
	pre          app.RenderPrecheck
	renders      []app.RenderRequest
}

func (f *fakeTimelines) Ensure(_ context.Context, in app.TimelineEnsureRequest) (app.TimelineView, error) {
	return app.TimelineView{ID: 77, ScriptID: in.ScriptID, Version: 3}, nil
}

func (f *fakeTimelines) Assemble(_ context.Context, in app.AssembleRequest) (app.AssembleOutcome, error) {
	f.lastAssemble = in
	if !in.DryRun {
		f.assembled++
	}
	return f.plan, nil
}

func (f *fakeTimelines) PrecheckRender(context.Context, app.RenderRequest) (app.RenderPrecheck, error) {
	return f.pre, nil
}

func (f *fakeTimelines) StartRender(_ context.Context, in app.RenderRequest) (app.RenderView, error) {
	f.renders = append(f.renders, in)
	return app.RenderView{ID: int64(500 + len(f.renders)), Status: "queued", Credits: f.pre.Credits}, nil
}

func multiRC(step int64) app.RunContext {
	return app.RunContext{WorkspaceID: 1, ProjectID: 2, SessionID: 3, TurnID: 4, StepID: step, Attempt: 1,
		UserID: 5, Role: collab.RoleEditor, PayerAccountID: 6, State: domain.StateExecuting}
}

func contentMap(t *testing.T, res app.ToolResult) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(res.Content, &m); err != nil {
		t.Fatalf("content 不是 JSON：%s", res.Content)
	}
	return m
}

// ---------------------------------------------------------------- 测试

// 同一步骤重入（租约过期重放）：额度条目已挂着运行 ID → 回那一次运行，不再 Start。
func TestRunWorkflow_同一步骤重入不重复Start(t *testing.T) {
	wf := &fakeWorkflows{plan: app.WorkflowPlanView{Executable: true, RunCount: 2, TotalCredits: 40,
		Summary: "运行 2 个"}}
	b := newFakeBudget(100)
	tool := RunWorkflow{MultiDeps{Workflows: wf, Budget: b}}

	first, err := tool.Run(context.Background(), multiRC(11), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	again, err := tool.Run(context.Background(), multiRC(11), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if wf.starts != 1 {
		t.Fatalf("同一步骤重入不应再 Start，实际 Start %d 次", wf.starts)
	}
	if contentMap(t, first)["runId"] != contentMap(t, again)["runId"] {
		t.Errorf("重入应回同一次运行：%s vs %s", first.Content, again.Content)
	}
	if contentMap(t, again)["replayed"] != true {
		t.Errorf("重入结果应标 replayed：%s", again.Content)
	}
	if !first.WaitRef || first.Suspend == nil || first.Suspend.Event != domain.EvWaitTask {
		t.Errorf("默认应挂起等运行结束：%+v", first)
	}
	// 另一个步骤是另一次运行。
	if _, err := tool.Run(context.Background(), multiRC(12), json.RawMessage(`{"wait":false}`)); err != nil {
		t.Fatal(err)
	}
	if wf.starts != 2 {
		t.Errorf("不同步骤应各自 Start，实际 %d", wf.starts)
	}
}

func TestRunWorkflow_计划不可执行不扣费(t *testing.T) {
	wf := &fakeWorkflows{plan: app.WorkflowPlanView{Executable: false, Summary: "有环",
		Issues: []app.WorkflowIssue{{Message: "图里有环", Nodes: []string{"a", "b"}}}}}
	b := newFakeBudget(100)
	res, err := RunWorkflow{MultiDeps{Workflows: wf, Budget: b}}.Run(context.Background(), multiRC(1), nil)
	if err != nil {
		t.Fatal(err)
	}
	if b.runs != 0 || wf.starts != 0 || len(b.quotes) != 0 {
		t.Fatalf("计划不可执行时不应进额度、不应 Start：runs=%d starts=%d", b.runs, wf.starts)
	}
	if !strings.Contains(string(res.Content), `"a"`) || !strings.Contains(string(res.Content), "图里有环") {
		t.Errorf("要把问题与涉及的节点回给模型：%s", res.Content)
	}
}

func TestRender_工具动作为export(t *testing.T) {
	if (Render{}).Action() != collab.ActionExport {
		t.Fatal("render 应按 export 判权限")
	}
	spec, ok := domain.SpecOf(domain.ToolRender)
	if !ok || spec.Action != domain.ActionExport || !spec.Paid {
		t.Fatalf("规格表里 render 应为付费 + export：%+v", spec)
	}
	// 能导出不等于能生成：只读角色两者都没有；编辑者有 export。
	if collab.RoleViewer.Can(collab.ActionExport) {
		t.Error("只读成员不应有 export")
	}

	tl := &fakeTimelines{pre: app.RenderPrecheck{Credits: 12, DurationMs: 12000}}
	b := newFakeBudget(100)
	tool := Render{MultiDeps{Timelines: tl, Budget: b}}
	if _, err := tool.Run(context.Background(), multiRC(21), json.RawMessage(`{"timelineId":"77"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Run(context.Background(), multiRC(21), json.RawMessage(`{"timelineId":"77"}`)); err != nil {
		t.Fatal(err)
	}
	if len(tl.renders) != 1 {
		t.Fatalf("同一步骤重入不应建第二个渲染任务，实际 %d 个", len(tl.renders))
	}
	if tl.renders[0].IdempotencyKey != "agent:3:21:1" {
		t.Errorf("渲染幂等键应为 agent:{session}:{step}:{attempt}，实际 %q", tl.renders[0].IdempotencyKey)
	}
}

func TestRender_预检不通过不扣费并逐条指出(t *testing.T) {
	tl := &fakeTimelines{pre: app.RenderPrecheck{Issues: []app.RenderIssue{
		{Code: "media_missing", Detail: "第 2 段素材已不存在", ClipIdx: 2}}}}
	b := newFakeBudget(100)
	res, err := Render{MultiDeps{Timelines: tl, Budget: b}}.Run(context.Background(), multiRC(1),
		json.RawMessage(`{"timelineId":"77"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(b.quotes) != 0 || len(tl.renders) != 0 {
		t.Fatal("预检不通过不应进额度、不应受理")
	}
	if !strings.Contains(string(res.Content), "第 2 段素材已不存在") {
		t.Errorf("应逐条回预检问题：%s", res.Content)
	}
}

func TestTimelineAssemble_缺口逐条回给模型(t *testing.T) {
	tl := &fakeTimelines{plan: app.AssembleOutcome{Append: 2, Gaps: []app.AssembleGap{
		{ShotNo: 3, Reason: "生成失败：上游超时"}, {ShotNo: 5, Reason: "还没有加入视频组"}}}}
	tool := TimelineAssemble{MultiDeps{Timelines: tl}}
	res, err := tool.Run(context.Background(), multiRC(1), json.RawMessage(`{"scriptId":"9"}`))
	if err != nil {
		t.Fatal(err)
	}
	if tl.assembled != 0 {
		t.Fatal("有缺口时默认不应装配（不改用户的时间线）")
	}
	c := string(res.Content)
	for _, want := range []string{"第 3 镜：生成失败：上游超时", "第 5 镜：还没有加入视频组", `"shotNo":3`, `"shotNo":5`} {
		if !strings.Contains(c, want) {
			t.Errorf("缺口要逐条带镜号与原因，缺 %q：%s", want, c)
		}
	}
	// 接受缺口时照样装配，缺口仍然要说出来（不静默）。
	res, err = tool.Run(context.Background(), multiRC(2), json.RawMessage(`{"scriptId":"9","allowGaps":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if tl.assembled != 1 || !strings.Contains(string(res.Content), "第 3 镜") {
		t.Errorf("allowGaps=true 应装配并仍列出缺口：assembled=%d %s", tl.assembled, res.Content)
	}
}

func TestMultiTools_规划态拒绝(t *testing.T) {
	rc := multiRC(1)
	rc.State = domain.StatePlanning
	for _, tool := range MultiTools(MultiDeps{Budget: newFakeBudget(100), Workflows: &fakeWorkflows{},
		Timelines: &fakeTimelines{}}) {
		if _, err := tool.Run(context.Background(), rc, json.RawMessage(`{}`)); err == nil {
			t.Errorf("%s 在规划态应拒绝", tool.Name())
		}
		spec, ok := domain.SpecOf(tool.Name())
		if !ok {
			t.Errorf("%s 不在规格表", tool.Name())
			continue
		}
		if string(tool.Action()) != spec.Action || tool.Paid() != spec.Paid {
			t.Errorf("%s 的自报属性与规格表不一致", tool.Name())
		}
	}
}

func TestPaidError_余额不足说清还差多少(t *testing.T) {
	err := paidError("渲染", 50, &app.InsufficientCreditsError{Balance: 20})
	if !strings.Contains(err.Error(), "还差 30") || !strings.Contains(err.Error(), "不要重试") {
		t.Errorf("得到 %v", err)
	}
	if !strings.Contains(paidError("渲染", 50, errors.New("x")).Error(), "渲染失败") {
		t.Error("其他错误原样带出")
	}
}

func TestFlexID_字符串与数字都收(t *testing.T) {
	var a struct {
		ID  flexID   `json:"id"`
		IDs []flexID `json:"ids"`
	}
	if err := json.Unmarshal([]byte(`{"id":"104503480802082816","ids":[1,"2"]}`), &a); err != nil {
		t.Fatal(err)
	}
	if a.ID != 104503480802082816 || len(ids64(a.IDs)) != 2 {
		t.Errorf("得到 %+v", a)
	}
	if err := json.Unmarshal([]byte(`{"id":"abc"}`), &a); err == nil {
		t.Error("非数字应报错")
	}
}

func TestGroupNodeParams_子项带itemId(t *testing.T) {
	p := groupNodeParams(app.GroupView{ID: 10, ProjectID: 2, ScriptID: 9, GroupType: "video_generator",
		Status: "draft", Version: 1, Items: []app.GroupItemView{{ID: 101, ShotID: 7, ShotNo: 3, Status: "draft",
			Operation: "ref2video", DurationSec: 8}}}, "kling")
	if p["groupId"] != "10" || p["modelCode"] != "kling" || p["itemCount"] != 1 {
		t.Fatalf("得到 %+v", p)
	}
	items := p["items"].([]any)
	it := items[0].(map[string]any)
	if it["itemId"] != "101" || it["outputType"] != "video" || it["paramsSnapshot"].(map[string]any)["shotNo"] != 3 {
		t.Errorf("子项形状不对：%+v", it)
	}
}

func TestPlanItemFrom_多段计划项(t *testing.T) {
	cases := []struct {
		spec     planSpec
		unit     string
		quantity int
		model    string
	}{
		{planSpec{Type: "storyboard", ModelCode: "gpt"}, "time", 1, "gpt"},
		{planSpec{Type: "compose", ModelCode: "gpt", Count: 24}, "time", 24, "gpt"},
		{planSpec{Type: "asset", ModelCode: "img", Count: 6}, "image", 6, "img"},
		{planSpec{Type: "image", ModelCode: "img", Count: 12}, "image", 12, "img"},
		// 渲染不绑模型：模型给了也丢掉（按 timeline_render 的兜底价报）。
		{planSpec{Type: "render", ModelCode: "x", DurationSec: 40}, "second", 40, ""},
	}
	for _, c := range cases {
		it, err := planItemFrom(c.spec)
		if err != nil {
			t.Fatalf("%+v：%v", c.spec, err)
		}
		if it.Unit != c.unit || it.Quantity != c.quantity || it.ModelCode != c.model || it.Label == "" {
			t.Errorf("%+v 得到 %+v", c.spec, it)
		}
	}
	for _, bad := range []planSpec{
		{Type: "render"},     // 缺时长
		{Type: "storyboard"}, // 缺模型
		{Type: "asset", ModelCode: "img", DurationSec: 3}, // 图没有时长
		{Type: "video", ModelCode: "v", Count: 3},         // 视频逐段列
	} {
		if _, err := planItemFrom(bad); err == nil {
			t.Errorf("%+v 应拒绝", bad)
		}
	}
}

// 只读成员：任何多段付费工具在执行前就被 Runner 按规格表的动作拒绝（实时查角色，loop.execTool）。
func TestMultiTools_只读成员没有任何付费工具的权限(t *testing.T) {
	for _, tool := range MultiTools(MultiDeps{}) {
		spec, _ := domain.SpecOf(tool.Name())
		if spec.Paid && collab.RoleViewer.Can(collab.Action(spec.Action)) {
			t.Errorf("只读成员不应能调用付费工具 %s（动作 %s）", tool.Name(), spec.Action)
		}
		if spec.Paid && !collab.RoleEditor.Can(collab.Action(spec.Action)) {
			t.Errorf("编辑者应能调用 %s", tool.Name())
		}
	}
}
