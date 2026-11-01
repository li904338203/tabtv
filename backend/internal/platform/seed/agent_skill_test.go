package seed

import (
	"strings"
	"testing"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	agentdomain "github.com/li904338203/tabtv/backend/internal/agent/domain"
	catalogdomain "github.com/li904338203/tabtv/backend/internal/catalog/domain"
)

// 渲染变量取 agent/app.SkillVars 本身（而不是在测试里抄一份）：两边不一致时这里直接红。
func skillVars() map[string]string {
	return agentapp.SkillVars(agentdomain.StatePlanning, "m-agent")
}

func TestAgentSkillSeed_正文渲染无残留变量(t *testing.T) {
	for _, s := range agentSkillSeeds() {
		texts := map[string]string{"主说明": s.body}
		for _, r := range s.refs {
			texts["附属文档 "+r.name] = r.body
		}
		for label, text := range texts {
			out, err := catalogdomain.RenderPrompt(text, skillVars())
			if err != nil {
				t.Errorf("%s %s 渲染失败：%v", s.code, label, err)
				continue
			}
			if strings.Contains(out, "{{") || strings.Contains(out, "}}") {
				t.Errorf("%s %s 渲染后残留占位符", s.code, label)
			}
		}
	}
}

func TestAgentSkillSeed_白名单只含已定义工具(t *testing.T) {
	for _, s := range agentSkillSeeds() {
		var wl []agentdomain.ToolName
		for _, n := range s.tools {
			if _, ok := agentdomain.SpecOf(agentdomain.ToolName(n)); !ok {
				t.Errorf("%s 白名单里有未定义（或第二期占位）的工具 %q", s.code, n)
			}
			wl = append(wl, agentdomain.ToolName(n))
		}
		// 白名单只能收窄：收窄后的结果必须是不收窄时的子集，且规划态永远没有付费 / 画布写工具。
		for _, st := range []agentdomain.State{agentdomain.StatePlanning, agentdomain.StateExecuting} {
			full := map[agentdomain.ToolName]bool{}
			for _, n := range agentdomain.AllowedTools(st, nil) {
				full[n] = true
			}
			for _, n := range agentdomain.AllowedTools(st, wl) {
				if !full[n] {
					t.Errorf("%s 在 %s 下放宽出了 %s", s.code, st, n)
				}
				spec, _ := agentdomain.SpecOf(n)
				if st == agentdomain.StatePlanning && (spec.Paid || spec.CanvasWrite) {
					t.Errorf("%s 规划态出现付费 / 画布写工具 %s", s.code, n)
				}
			}
		}
	}
}

func TestAgentSkillSeed_首行标注待运营定稿(t *testing.T) {
	for _, s := range agentSkillSeeds() {
		if first, _, _ := strings.Cut(s.body, "\n"); !strings.Contains(first, "待运营定稿") {
			t.Errorf("%s 主说明首行未标注待运营定稿：%q", s.code, first)
		}
		for _, r := range s.refs {
			if first, _, _ := strings.Cut(r.body, "\n"); !strings.Contains(first, "待运营定稿") {
				t.Errorf("%s 附属文档 %s 首行未标注待运营定稿：%q", s.code, r.name, first)
			}
		}
	}
}

// 卡片约定的三个技能、分类、白名单与检查点（AGT-30 范围表）。
func TestAgentSkillSeed_三个技能与卡片一致(t *testing.T) {
	got := map[string]agentSkillSeed{}
	for _, s := range agentSkillSeeds() {
		if _, dup := got[s.code]; dup {
			t.Fatalf("技能 code 重复：%s", s.code)
		}
		got[s.code] = s
		if r := []rune(s.description); len(r) > 500 {
			t.Errorf("%s description 超过 500 字", s.code)
		}
		if r := []rune(s.name); len(r) > 64 {
			t.Errorf("%s name 超过 64 字", s.code)
		}
		names := map[string]bool{}
		for _, r := range s.refs {
			if names[r.name] {
				t.Errorf("%s 附属文档重名 %s", s.code, r.name)
			}
			names[r.name] = true
		}
		for i, st := range s.steps {
			if st.No != i+1 {
				t.Errorf("%s 步骤编号不连续：%+v", s.code, s.steps)
			}
		}
	}
	// 首批 3 个 + 2026-11-01 借鉴 GitHub 调研补充的 10 个（docs/agent/skills/01-GitHub调研清单.md）。
	if len(got) != 13 {
		t.Fatalf("应为 13 个技能，实际 %d", len(got))
	}
	cb, st, vs := got["canvas_basics"], got["story_topic"], got["video_single"]
	if len(cb.tools) != 0 || len(vs.tools) != 0 {
		t.Errorf("画布技能与单段视频不收窄：%v %v", cb.tools, vs.tools)
	}
	for _, n := range st.tools {
		if spec, _ := agentdomain.SpecOf(agentdomain.ToolName(n)); spec.Paid || spec.CanvasWrite {
			t.Errorf("故事选题白名单不应含付费 / 画布写工具：%s", n)
		}
	}
	if cb.quickEntry || !st.quickEntry || !vs.quickEntry {
		t.Error("快捷入口应为：选题、单段视频")
	}
	checkpoints := func(s agentSkillSeed) []bool {
		var out []bool
		for _, x := range s.steps {
			out = append(out, x.Checkpoint)
		}
		return out
	}
	if c := checkpoints(st); len(c) != 3 || !c[0] || !c[1] || c[2] {
		t.Errorf("故事选题检查点应为 1、2：%v", c)
	}
	if c := checkpoints(vs); len(c) != 4 || !c[0] || !c[1] || c[2] || c[3] {
		t.Errorf("单段视频检查点应为 1、2：%v", c)
	}
	if !hasRef(vs, "单段流程工作指引") || !hasRef(cb, "节点与连线") {
		t.Error("缺少卡片约定的附属文档")
	}
}

// 补充附属文档按技能 code 挂上去（withExtraRefs），code 写错的话那份文档会被静默丢掉 —— 在这里点出来。
func TestAgentSkillSeed_补充附属文档都挂得上(t *testing.T) {
	codes := map[string]bool{}
	for _, s := range agentSkillSeeds() {
		codes[s.code] = true
	}
	want := map[string][]string{}
	for _, extra := range []map[string][]agentSkillRef{filmExtraRefs(), videoExtraRefs(), imageExtraRefs()} {
		for code, refs := range extra {
			if !codes[code] {
				t.Errorf("补充附属文档挂到了不存在的技能 %s", code)
			}
			for _, r := range refs {
				want[code] = append(want[code], r.name)
			}
		}
	}
	got := map[string]agentSkillSeed{}
	for _, s := range agentSkillSeeds() {
		got[s.code] = s
	}
	for code, names := range want {
		for _, n := range names {
			if !hasRef(got[code], n) {
				t.Errorf("%s 缺少补充附属文档 %s", code, n)
			}
		}
	}
	// 补给已有技能的内容一律是新文档，不改旧的那份（seed 不覆盖已有正文）。
	for _, n := range []string{"Prompt 编写指南（进阶）", "分模型写法速查"} {
		if !hasRef(got["video_single"], n) {
			t.Errorf("video_single 缺少 %s", n)
		}
	}
	if !hasRef(got["story_topic"], "前提与转折") || !hasRef(got["canvas_basics"], "创作简报") {
		t.Error("故事选题 / 画布技能缺少补充附属文档")
	}
}

// 通用技能的分类（2026-11-01 细分、去掉「通用」，迁移 00079 同步已有环境）：
// 只允许这几类 —— 新增技能随手写一个新分类的话，技能弹层就多出一个只有一项的标签。
// 要加分类先改这里，并想清楚已有环境怎么迁。
func TestAgentSkillSeed_分类只用细分后的七类(t *testing.T) {
	allowed := map[string]bool{"画布与工作流": true, "创意策划": true, "视频生成": true, "视频分析与修改": true,
		"声音与配乐": true, "图片生成": true, "图片分析与复核": true}
	for _, s := range agentSkillSeeds() {
		if !allowed[s.category] {
			t.Errorf("%s 的分类「%s」不在细分后的七类里", s.code, s.category)
		}
	}
}

func hasRef(s agentSkillSeed, name string) bool {
	for _, r := range s.refs {
		if r.name == name {
			return true
		}
	}
	return false
}

// 「节点与连线」由 connection_rule 的 seed 行生成：每条规则都要出现，且不出现规则外的配对。
func TestAgentSkillSeed_节点与连线与连线规则一致(t *testing.T) {
	ref := nodesAndEdgesRef()
	names := map[string]string{}
	for _, n := range nodeTypeRows {
		names[n.code] = n.name
		if !strings.Contains(ref, n.name+"（"+n.code+"）") {
			t.Errorf("节点类型 %s 未列出", n.code)
		}
	}
	allowed := map[string]bool{}
	for _, r := range connectionRuleRows {
		pair := names[r.src] + " → " + names[r.dst]
		allowed[pair] = true
		if !strings.Contains(ref, "- "+pair+"（") {
			t.Errorf("连线规则 %s 未列出", pair)
		}
	}
	for _, line := range strings.Split(ref, "\n") {
		if strings.HasPrefix(line, "- ") && strings.Contains(line, " → ") {
			pair, _, _ := strings.Cut(strings.TrimPrefix(line, "- "), "（")
			if !allowed[pair] {
				t.Errorf("出现了 seed 之外的连线：%s", line)
			}
		}
	}
	if strings.Contains(ref, "文档 →") {
		t.Error("文档节点不应作为任何连线的来源")
	}
}

// 单段 / 多段分界不写死秒数（design/01 §9）；技能不得声称能跳过确认（design/01 §1 第 3 条）。
func TestAgentSkillSeed_不写死时长且不跳过确认(t *testing.T) {
	for _, s := range agentSkillSeeds() {
		texts := []string{s.body}
		for _, r := range s.refs {
			texts = append(texts, r.body)
		}
		for _, text := range texts {
			for _, bad := range []string{"15 秒", "15秒", "跳过确认", "无需确认", "不用确认"} {
				if strings.Contains(text, bad) {
					t.Errorf("%s 正文出现 %q", s.code, bad)
				}
			}
		}
	}
}
