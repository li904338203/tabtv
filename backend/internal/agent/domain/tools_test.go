package domain

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestAllowedTools_规划态不含付费工具(t *testing.T) {
	for _, name := range AllowedTools(StatePlanning, nil) {
		spec, ok := SpecOf(name)
		if !ok {
			t.Fatalf("下发了没有规格的工具 %s", name)
		}
		if spec.Paid {
			t.Errorf("规划态下发了付费工具 %s", name)
		}
	}
	// 规格表里确实存在付费工具，上面的断言才有意义。
	paid := 0
	for _, s := range Tools() {
		if s.Paid {
			paid++
		}
	}
	if paid == 0 {
		t.Fatal("规格表里没有付费工具，断言失去意义")
	}
}

func TestAllowedTools_规划态不含画布写工具(t *testing.T) {
	for _, name := range AllowedTools(StatePlanning, nil) {
		spec, _ := SpecOf(name)
		if spec.CanvasWrite {
			t.Errorf("规划态下发了画布写工具 %s", name)
		}
	}
	// 即使把全部工具塞进白名单也一样（白名单只能收窄）。
	var all []ToolName
	for _, s := range Tools() {
		all = append(all, s.Name)
	}
	for _, name := range AllowedTools(StatePlanning, all) {
		spec, _ := SpecOf(name)
		if spec.Paid || spec.CanvasWrite {
			t.Errorf("白名单放宽了规划态：%s", name)
		}
	}
}

func TestAllowedTools_规划与执行的集合(t *testing.T) {
	plan := AllowedTools(StatePlanning, nil)
	wantPlan := []ToolName{ToolReadCanvas, ToolListAssets, ToolQuote, ToolAskUser,
		ToolReadDoc, ToolWriteDoc, ToolSubmitPlan, ToolLoadSkill, ToolReadSkillRef}
	if !reflect.DeepEqual(plan, wantPlan) {
		t.Errorf("planning 得到 %v，期望 %v", plan, wantPlan)
	}
	exec := AllowedTools(StateExecuting, nil)
	var wantExec []ToolName
	for _, s := range Tools() {
		// 场景专属工具（MKT-T-18，Scenes 非空）不在通用会话里。
		if s.Name != ToolSubmitPlan && len(s.Scenes) == 0 {
			wantExec = append(wantExec, s.Name)
		}
	}
	if !reflect.DeepEqual(exec, wantExec) {
		t.Errorf("executing 得到 %v，期望全部通用工具 − submit_plan %v", exec, wantExec)
	}
}

func TestAllowedTools_挂起态为空集(t *testing.T) {
	for _, s := range []State{StateIdle, StateAwaitingInput, StateAwaitingApproval,
		StateWaitingTask, StateFailed, StateCancelled} {
		if got := AllowedTools(s, nil); len(got) != 0 {
			t.Errorf("%s 应为空集，得到 %v", s, got)
		}
	}
}

func TestAllowedTools_技能白名单只能收窄(t *testing.T) {
	wl := []ToolName{ToolGenerate, ToolReadCanvas, ToolSubmitPlan}
	got := AllowedTools(StatePlanning, wl)
	want := []ToolName{ToolReadCanvas, ToolSubmitPlan}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("planning + 白名单得到 %v，期望 %v", got, want)
	}
	for _, n := range got {
		if n == ToolGenerate {
			t.Error("白名单含 generate 不能让规划态拿到 generate")
		}
	}
	// 执行态：白名单收窄到交集；未知名字被忽略。
	got = AllowedTools(StateExecuting, []ToolName{ToolGenerate, ToolRender, "rm_rf"})
	if !reflect.DeepEqual(got, []ToolName{ToolGenerate, ToolRender}) {
		t.Errorf("executing + 白名单得到 %v", got)
	}
	// 空白名单 = 不收窄。
	if len(AllowedTools(StateExecuting, []ToolName{})) != len(AllowedTools(StateExecuting, nil)) {
		t.Error("空白名单不应收窄")
	}
}

func TestAllowedTools_顺序稳定(t *testing.T) {
	a := AllowedTools(StateExecuting, []ToolName{ToolReadSkillRef, ToolGenerate, ToolReadCanvas})
	b := AllowedTools(StateExecuting, []ToolName{ToolReadCanvas, ToolGenerate, ToolReadSkillRef})
	want := []ToolName{ToolReadCanvas, ToolGenerate, ToolReadSkillRef}
	if !reflect.DeepEqual(a, want) || !reflect.DeepEqual(b, want) {
		t.Errorf("顺序应按声明顺序：a=%v b=%v", a, b)
	}
}

func TestIsToolAllowed_与AllowedTools一致(t *testing.T) {
	names := append([]ToolName{"bogus"}, multiSegmentTools...)
	for _, s := range Tools() {
		names = append(names, s.Name)
	}
	whitelists := [][]ToolName{nil, {ToolGenerate, ToolReadDoc}, {ToolSubmitPlan}}
	for _, st := range AllStates {
		for _, wl := range whitelists {
			allowedSet := map[ToolName]bool{}
			for _, n := range AllowedTools(st, wl) {
				allowedSet[n] = true
			}
			for _, n := range names {
				if IsToolAllowed(st, wl, n) != allowedSet[n] {
					t.Errorf("状态 %s 白名单 %v 工具 %s：IsToolAllowed 与 AllowedTools 不一致", st, wl, n)
				}
			}
		}
	}
}

// multiSegmentTools 是 AGT-50 新增的多段工具。
var multiSegmentTools = []ToolName{ToolScriptCreate, ToolScriptStoryboard, ToolScriptCompose,
	ToolScriptAssets, ToolGroupCreate, ToolGroupExecute, ToolRunWorkflow, ToolTimelineAssemble, ToolRender}

func TestAllowedTools_规划态不含多段付费工具(t *testing.T) {
	plan := map[ToolName]bool{}
	for _, n := range AllowedTools(StatePlanning, nil) {
		plan[n] = true
	}
	// 白名单把它们全写上也不行（白名单只能收窄）。
	for _, n := range AllowedTools(StatePlanning, multiSegmentTools) {
		plan[n] = true
	}
	for _, n := range multiSegmentTools {
		if plan[n] {
			t.Errorf("规划态下发了多段工具 %s", n)
		}
		if IsToolAllowed(StatePlanning, nil, n) {
			t.Errorf("规划态二次校验放行了多段工具 %s", n)
		}
		if !IsToolAllowed(StateExecuting, nil, n) {
			t.Errorf("执行态应下发多段工具 %s", n)
		}
	}
}

func TestToolSpecs_多段工具的付费与权限(t *testing.T) {
	want := map[ToolName]struct {
		paid   bool
		action string
	}{
		ToolScriptCreate:     {false, ActionEdit},
		ToolScriptStoryboard: {true, ActionGenerate},
		ToolScriptAssets:     {true, ActionGenerate},
		ToolScriptCompose:    {true, ActionGenerate},
		// 分镜图组建组即扣费：Agent 工具按 generate 判。
		ToolGroupCreate:      {true, ActionGenerate},
		ToolGroupExecute:     {true, ActionGenerate},
		ToolRunWorkflow:      {true, ActionGenerate},
		ToolTimelineAssemble: {false, ActionEdit},
		ToolRender:           {true, ActionExport},
	}
	for n, w := range want {
		s, ok := SpecOf(n)
		if !ok {
			t.Errorf("多段工具 %s 不在规格表", n)
			continue
		}
		if s.Paid != w.paid || s.Action != w.action {
			t.Errorf("%s：paid=%v action=%s，期望 paid=%v action=%s", n, s.Paid, s.Action, w.paid, w.action)
		}
		if !reflect.DeepEqual(s.States, []State{StateExecuting}) {
			t.Errorf("%s 只应在执行态，得到 %v", n, s.States)
		}
	}
}

// 3D 导演台工具（DRT-29，DRD-32）：免费（不是 Paid、不标 generate），但写画布与场景，只在执行态下发；
// 规划态白名单写上它们也放不开（白名单只能收窄）。
func TestToolSpecs_导演台工具免费且只在执行态(t *testing.T) {
	directorTools := []ToolName{ToolDirectorCreate, ToolDirectorBuild}
	for _, n := range directorTools {
		s, ok := SpecOf(n)
		if !ok {
			t.Fatalf("%s 不在规格表", n)
		}
		if s.Paid || !s.CanvasWrite || s.Action != ActionEdit || s.Scenes != nil {
			t.Errorf("%s：paid=%v canvasWrite=%v action=%s scenes=%v，期望免费、写画布、edit、通用场景",
				n, s.Paid, s.CanvasWrite, s.Action, s.Scenes)
		}
		if !reflect.DeepEqual(s.States, []State{StateExecuting}) {
			t.Errorf("%s 只应在执行态，得到 %v", n, s.States)
		}
		if IsToolAllowed(StatePlanning, nil, n) || IsToolAllowed(StatePlanning, directorTools, n) {
			t.Errorf("规划态不应放行 %s（白名单也放不开）", n)
		}
		if !IsToolAllowed(StateExecuting, nil, n) {
			t.Errorf("执行态应下发 %s", n)
		}
	}
	for _, n := range AllowedTools(StatePlanning, directorTools) {
		if n == ToolDirectorCreate || n == ToolDirectorBuild {
			t.Errorf("规划态下发了 %s", n)
		}
	}
}

// 智能剪辑工具（SCT-41）：三个都免费（不是 Paid、不标 generate，SCK-2）；建与剪写画布；smartcut_get 只读（view）；
// 三个都只在执行态（同多段工具，规划态一个都不下发）；都是通用场景。
func TestToolSpecs_智能剪辑工具免费且按读写分态(t *testing.T) {
	for _, c := range []struct {
		name   ToolName
		write  bool
		action string
		states []State
	}{
		{ToolSmartcutCreate, true, ActionEdit, []State{StateExecuting}},
		{ToolSmartcutGet, false, ActionView, []State{StateExecuting}},
		{ToolSmartcutPlan, true, ActionEdit, []State{StateExecuting}},
	} {
		s, ok := SpecOf(c.name)
		if !ok {
			t.Fatalf("%s 不在规格表", c.name)
		}
		if s.Paid || s.CanvasWrite != c.write || s.Action != c.action || s.Scenes != nil ||
			!reflect.DeepEqual(s.States, c.states) {
			t.Errorf("%s：%+v", c.name, s)
		}
	}
	if IsToolAllowed(StatePlanning, []ToolName{ToolSmartcutPlan}, ToolSmartcutPlan) {
		t.Error("规划态不能发起 AI 剪辑（白名单也放不开）")
	}
}

func TestToolSpecs_规格完整且不重复(t *testing.T) {
	seen := map[ToolName]bool{}
	for _, s := range Tools() {
		if seen[s.Name] {
			t.Errorf("工具 %s 重复声明", s.Name)
		}
		seen[s.Name] = true
		if s.Action == "" || len(s.States) == 0 {
			t.Errorf("工具 %s 规格不完整", s.Name)
		}
	}
}

func TestPaidIdempotencyKey_随attempt变化(t *testing.T) {
	k1 := PaidIdempotencyKey(11, 22, 1)
	k2 := PaidIdempotencyKey(11, 22, 2)
	if k1 == k2 {
		t.Error("attempt 变了幂等键必须变")
	}
	if k1 != "agent:11:22:1" {
		t.Errorf("格式不对：%s", k1)
	}
	if PaidIdempotencyKey(11, 23, 1) == k1 {
		t.Error("不同步骤的幂等键不能相同")
	}
}

// 包内不得碰库或网络：domain 是纯函数（验收标准）。
func TestDomain_不依赖数据库与网络(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Skipf("go list 不可用：%v", err)
	}
	for _, dep := range strings.Fields(string(out)) {
		switch {
		case dep == "database/sql", dep == "net/http", strings.Contains(dep, "jackc/pgx"),
			strings.HasPrefix(dep, "github.com/li904338203/tabtv/backend/internal/") &&
				dep != "github.com/li904338203/tabtv/backend/internal/agent/domain":
			t.Errorf("domain 包依赖了 %s", dep)
		}
	}
}

// MKR-10：营销改动不得泄漏到通用会话。列表写死成改动前的样子（逐字），多一个、少一个、换顺序都红。
func TestToolSpecs_营销工具只在营销会话下发(t *testing.T) {
	generalPlan := []ToolName{"read_canvas", "list_assets", "quote", "ask_user", "read_doc", "write_doc",
		"submit_plan", "load_skill", "read_skill_ref"}
	generalExec := []ToolName{"read_canvas", "list_assets", "quote", "ask_user", "read_doc", "write_doc",
		"canvas_apply", "generate", "cancel_task", "load_skill", "read_skill_ref", "script_create",
		"script_storyboard", "script_assets", "script_compose", "group_create", "group_execute", "run_workflow",
		"timeline_assemble", "render",
		// 3D 导演台（DRT-29）：通用工具，只在执行态。
		"director_create", "director_build",
		// 智能剪辑（SCT-41）：通用工具；建与剪只在执行态。
		"smartcut_create", "smartcut_get", "smartcut_plan"}
	for _, sc := range []Scene{SceneGeneral, ""} {
		if got := AllowedToolsIn(sc, StatePlanning, nil); !reflect.DeepEqual(got, generalPlan) {
			t.Errorf("通用会话（%q）规划态工具变了：%v", sc, got)
		}
		if got := AllowedToolsIn(sc, StateExecuting, nil); !reflect.DeepEqual(got, generalExec) {
			t.Errorf("通用会话（%q）执行态工具变了：%v", sc, got)
		}
		if IsToolAllowedIn(sc, StatePlanning, nil, ToolListMarketingOptions) ||
			IsToolAllowedIn(sc, StateExecuting, nil, ToolViewMedia) {
			t.Errorf("通用会话（%q）二次校验放行了营销工具", sc)
		}
	}
	if !reflect.DeepEqual(AllowedTools(StatePlanning, nil), generalPlan) {
		t.Error("AllowedTools 应等价于通用会话")
	}
	// 营销会话 = 通用集合 + 营销工具（追加在末尾，不打乱通用工具的顺序）。
	for st, base := range map[State][]ToolName{StatePlanning: generalPlan, StateExecuting: generalExec} {
		want := append(append([]ToolName{}, base...), ToolListMarketingOptions, ToolViewMedia)
		if got := AllowedToolsIn(SceneMarketing, st, nil); !reflect.DeepEqual(got, want) {
			t.Errorf("营销会话 %s 工具 = %v，期望 %v", st, got, want)
		}
	}
	// MKT-AC-16：营销会话规划态仍不含任何付费 / 画布写工具。
	for _, n := range AllowedToolsIn(SceneMarketing, StatePlanning, nil) {
		if s, _ := SpecOf(n); s.Paid || s.CanvasWrite {
			t.Errorf("营销会话规划态下发了付费 / 画布写工具 %s", n)
		}
	}
	// 白名单只能收窄：技能写了营销工具，通用会话里照样没有。
	if got := AllowedToolsIn(SceneGeneral, StatePlanning, []ToolName{ToolListMarketingOptions}); len(got) != 0 {
		t.Errorf("白名单不能把营销工具放进通用会话：%v", got)
	}
	for _, n := range []ToolName{ToolListMarketingOptions, ToolViewMedia} {
		s, ok := SpecOf(n)
		if !ok || s.Paid || s.CanvasWrite || s.Action != ActionView || !reflect.DeepEqual(s.States, planAndExec) {
			t.Errorf("%s 规格不对：%+v", n, s)
		}
	}
	// 二次校验与下发集合一致。
	for _, st := range AllStates {
		set := map[ToolName]bool{}
		for _, n := range AllowedToolsIn(SceneMarketing, st, nil) {
			set[n] = true
		}
		for _, sp := range Tools() {
			if IsToolAllowedIn(SceneMarketing, st, nil, sp.Name) != set[sp.Name] {
				t.Errorf("营销会话 %s 工具 %s：IsToolAllowedIn 与 AllowedToolsIn 不一致", st, sp.Name)
			}
		}
	}
}

// 04 文档 P-51：营销技能收窄了白名单，却漏了 view_media / list_marketing_options，
// 而平台自己的营销输入段就让模型用它们。场景专属的只读工具不受技能白名单收窄。
func TestAllowedToolsIn_场景专属只读工具不受技能白名单收窄(t *testing.T) {
	wl := []ToolName{ToolReadCanvas, ToolAskUser}
	got := AllowedToolsIn(SceneMarketing, StatePlanning, wl)
	has := func(n ToolName) bool {
		for _, g := range got {
			if g == n {
				return true
			}
		}
		return false
	}
	if !has(ToolViewMedia) || !has(ToolListMarketingOptions) {
		t.Fatalf("营销会话里场景只读工具应始终下发：%v", got)
	}
	if has(ToolQuote) || has(ToolReadDoc) {
		t.Fatalf("其余工具照样按白名单收窄：%v", got)
	}
	if got := AllowedToolsIn(SceneGeneral, StatePlanning, wl); len(got) != 2 {
		t.Fatalf("通用会话不受影响：%v", got)
	}
}
