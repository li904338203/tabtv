package domain

import (
	"reflect"
	"strings"
	"testing"
)

func okDraft() SkillDraft {
	return SkillDraft{
		Code: "story_topic", Name: "故事选题", Category: "创作", Body: "当前状态 {{state}}",
		Tags:  []string{"故事"},
		Steps: []SkillStep{{No: 1, Name: "了解需求"}, {No: 2, Name: "确认", Checkpoint: true}},
		Tools: []string{"read_canvas", "ask_user"},
	}
}

func hasProblem(ps []string, sub string) bool {
	for _, p := range ps {
		if strings.Contains(p, sub) {
			return true
		}
	}
	return false
}

func TestValidateSkill_合法配置没有问题(t *testing.T) {
	if ps := ValidateSkill(okDraft(), ToolNames()); len(ps) != 0 {
		t.Fatalf("合法配置报了问题：%v", ps)
	}
	// 空白名单 = 不收窄，合法。
	d := okDraft()
	d.Tools = nil
	if ps := ValidateSkill(d, ToolNames()); len(ps) != 0 {
		t.Fatalf("空白名单报了问题：%v", ps)
	}
}

func TestValidateSkill_未注册工具被拒(t *testing.T) {
	d := okDraft()
	// 第二期占位工具（script_storyboard）不在规格表里时同样被拒；接入后它会自动进入 ToolNames()。
	d.Tools = []string{"read_canvas", "shell_exec", "read_canvas"}
	ps := ValidateSkill(d, ToolNames())
	if !hasProblem(ps, "「shell_exec」不是已注册的工具") {
		t.Errorf("未注册工具没有被拒：%v", ps)
	}
	if !hasProblem(ps, "「read_canvas」重复") {
		t.Errorf("重复工具没有被拒：%v", ps)
	}
}

func TestValidateSkill_步骤序号必须连续(t *testing.T) {
	d := okDraft()
	d.Steps = []SkillStep{{No: 1, Name: "a"}, {No: 3, Name: "b"}}
	ps := ValidateSkill(d, ToolNames())
	if !hasProblem(ps, "第 2 项的序号是 3") {
		t.Fatalf("序号跳号没有被拒：%v", ps)
	}
	d.Steps = []SkillStep{{No: 0, Name: "a"}}
	if ps := ValidateSkill(d, ToolNames()); !hasProblem(ps, "从 1 起连续") {
		t.Fatalf("序号不从 1 起没有被拒：%v", ps)
	}
	d.Steps = []SkillStep{{No: 1, Name: "  "}}
	if ps := ValidateSkill(d, ToolNames()); !hasProblem(ps, "第 1 步缺少名称") {
		t.Fatalf("空步骤名没有被拒：%v", ps)
	}
}

func TestValidateSkill_一次报完全部问题(t *testing.T) {
	d := SkillDraft{Code: "Bad Code", Tags: []string{"a", "a"}, Sort: -1}
	ps := ValidateSkill(d, ToolNames())
	for _, want := range []string{"code「Bad Code」格式不对", "名称不能为空", "分类不能为空",
		"主说明不能为空", "排序 -1", "标签「a」重复"} {
		if !hasProblem(ps, want) {
			t.Errorf("缺少问题 %q；全部：%v", want, ps)
		}
	}
}

func TestValidateSkillRef(t *testing.T) {
	if ps := ValidateSkillRef("写法指南", "正文"); len(ps) != 0 {
		t.Fatalf("合法附属文档报了问题：%v", ps)
	}
	ps := ValidateSkillRef("", " ")
	if !hasProblem(ps, "名称不能为空") || !hasProblem(ps, "正文不能为空") {
		t.Fatalf("空名称 / 空正文没有被拒：%v", ps)
	}
}

// 收窄的落点是 AllowedTools（状态允许的集合 ∩ 技能白名单），AGT-20 起就是交集 ——
// 卡片里的 NarrowTools 不另写一份，这两条测试钉住「只能收窄」的语义。

func TestNarrowTools_只能收窄不能放宽(t *testing.T) {
	base := AllowedTools(StateExecuting, nil)
	got := AllowedTools(StateExecuting, []ToolName{ToolAskUser, ToolReadCanvas})
	// 顺序取声明顺序，与白名单顺序无关。
	if want := []ToolName{ToolReadCanvas, ToolAskUser}; !reflect.DeepEqual(got, want) {
		t.Fatalf("收窄结果 %v，期望 %v", got, want)
	}
	for _, n := range got {
		found := false
		for _, b := range base {
			found = found || b == n
		}
		if !found {
			t.Fatalf("收窄结果里出现了状态本来不给的工具 %s", n)
		}
	}
	// 白名单里写了状态不给的 submit_plan（执行态没有），不能被「放宽」进来。
	got = AllowedTools(StateExecuting, []ToolName{ToolSubmitPlan, ToolReadDoc})
	if want := []ToolName{ToolReadDoc}; !reflect.DeepEqual(got, want) {
		t.Fatalf("白名单放宽了执行态：%v", got)
	}
}

func TestNarrowTools_规划态即使技能列了付费工具也不下发(t *testing.T) {
	got := AllowedTools(StatePlanning, []ToolName{ToolGenerate, ToolCanvasApply, ToolReadCanvas})
	if want := []ToolName{ToolReadCanvas}; !reflect.DeepEqual(got, want) {
		t.Fatalf("规划态下发了 %v，期望只有 %v", got, want)
	}
	if IsToolAllowed(StatePlanning, []ToolName{ToolGenerate}, ToolGenerate) {
		t.Fatal("执行前二次校验放行了规划态的 generate")
	}
}

func TestValidateSkill_场景槽位预填(t *testing.T) {
	d := okDraft()
	d.Scenes = []string{SkillSceneMarketing}
	d.PrefillText = strings.Repeat("字", SkillPrefillMax)
	if ps := ValidateSkill(d, ToolNames()); len(ps) != 0 {
		t.Fatalf("合法的场景与 200 字预填报了问题：%v", ps)
	}
	d.Scenes = []string{SkillSceneGeneral, SkillSceneMarketing}
	if ps := ValidateSkill(d, ToolNames()); len(ps) != 0 {
		t.Fatalf("同时属于两个场景应合法：%v", ps)
	}
	// 一次报完：未知场景、重复场景、预填超长（按码点：201 个汉字）。
	d.Scenes = []string{"writing", SkillSceneMarketing, SkillSceneMarketing}
	d.PrefillText = strings.Repeat("字", SkillPrefillMax+1)
	ps := ValidateSkill(d, ToolNames())
	for _, want := range []string{"场景「writing」不认识", "场景「marketing」重复", "预填描述 201 字"} {
		if !hasProblem(ps, want) {
			t.Errorf("缺少问题 %q：%v", want, ps)
		}
	}
	if len(ps) != 3 {
		t.Fatalf("应恰好 3 处问题：%v", ps)
	}
}

// 场景过滤只有 SkillInScene 一处实现（用户端列表按它判，MKT-AC-15）。
func TestSkills_场景过滤(t *testing.T) {
	cases := []struct {
		scenes             []string
		general, marketing bool
	}{
		{nil, true, false},                   // 存量技能：只在创造页
		{[]string{}, true, false},            // 同上
		{[]string{"marketing"}, false, true}, // 仅属营销：创造页不列
		{[]string{"general", "marketing"}, true, true},
		{[]string{"general"}, true, false},
	}
	for _, c := range cases {
		if got := SkillInScene(c.scenes, SkillSceneGeneral); got != c.general {
			t.Errorf("%v 在 general：%v，期望 %v", c.scenes, got, c.general)
		}
		if got := SkillInScene(c.scenes, SkillSceneMarketing); got != c.marketing {
			t.Errorf("%v 在 marketing：%v，期望 %v", c.scenes, got, c.marketing)
		}
	}
}

// 04 文档 P-54①：纯空白的主说明原先能存进去 —— 技能「已加载」而模型看不到任何说明。
func TestValidateSkill_主说明不能是纯空白(t *testing.T) {
	d := okDraft()
	d.Body = "   \n\t  "
	if ps := ValidateSkill(d, ToolNames()); !hasProblem(ps, "主说明不能为空") {
		t.Fatalf("纯空白的主说明应被拒：%v", ps)
	}
}
