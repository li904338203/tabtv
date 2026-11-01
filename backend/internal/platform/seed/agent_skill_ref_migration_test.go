package seed

import (
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/migrations"
)

// 「节点与连线」参考的已有环境更新（DRT-29，迁移 00066）。
//
// 技能以 `on conflict do nothing` 写入，改 seed 只影响新环境。迁移只覆盖**仍是旧版 seed 原文**的行，
// 旧原文由下面冻结的旧生成器逐字生成：
//   - v1：导演台进节点表之前（DRT-05 之前）—— 节点表与连线规则去掉导演台；
//   - v2：DRT-05 ~ DRT-28 —— 完整的两张表，但导演台套的是 composite 的通用说法。
//
// ⚠️ 冻结的生成器**不能再改**：它描述的是已经写进别人库里的文本。新文本以 nodesAndEdgesRef() 为准；
// 再改那份文本时写一条新迁移，并把这里的「新文本」换成冻结的副本。

const directorSkillRefMigration = "00066_director_skill_ref.sql"

// legacyNodeRow / legacyRuleRow 与 nodeTypeRows / connectionRuleRows 的元素类型相同（别名）。
type legacyNodeRow = struct {
	code, name, kind string
	sort             int
}

type legacyRuleRow = struct{ src, dst, defaultOp string }

// legacyNodesAndEdgesRef 是 DRT-29 之前 nodesAndEdgesRef 的原样副本（只把两张表改成参数）。
func legacyNodesAndEdgesRef(nodes []legacyNodeRow, rules []legacyRuleRow) string {
	names := map[string]string{}
	var b strings.Builder
	b.WriteString(agentSkillPlaceholder + "\n\n# 节点与连线\n\n## 节点类型\n")
	kindNote := map[string]string{
		"generator": "能独立生成内容",
		"composite": "组合 / 批量类节点，由对应流程创建，不要手工新建",
		"material":  "没有独立生成能力",
	}
	for _, n := range nodes {
		names[n.code] = n.name
		fmt.Fprintf(&b, "- %s（%s）：%s\n", n.name, n.code, kindNote[n.kind])
	}
	b.WriteString("- 画框（frame）：纯视觉容器，只用来把相关节点框在一起；与「组」不同，没有批量执行与计费语义\n")

	b.WriteString("\n## 允许的连线（来源 → 目标）\n")
	for _, r := range rules {
		if r.defaultOp != "" {
			fmt.Fprintf(&b, "- %s → %s（默认操作 %s）\n", names[r.src], names[r.dst], r.defaultOp)
		} else {
			fmt.Fprintf(&b, "- %s → %s（只作为修饰，叠加到目标节点的提示词）\n", names[r.src], names[r.dst])
		}
	}
	b.WriteString(`
不在上表里的连线一律不允许。视频节点的实际操作以所选模式为准（文生视频 / 图生视频 / 首尾帧 / 全能参考 / 视频生视频），表里的默认操作只是建议。

## 规则
- 连线有执行语义：连上的上游会作为下游生成的输入，顺序按连线的 order。不要用连线表达「来源」「参考过」这类关系。
- 文档节点不能连到任何节点上当输入；规划文档与产物的关系用画框表达。
- 不修改用户的节点：要改用户建的或用户改过的节点时，在旁边新建一个，并在回复里说明它与原节点的关系。
- 不删除任何节点；确实需要删除时告诉用户，由用户自己删。
`)
	return b.String()
}

// drt29NodesAndEdgesRef 是 DRT-29 ~ SCT-08 之前 nodesAndEdgesRef 的原样副本（两张表改成参数）。
// 00066 写进库里的就是它的输出（TestAgentSkillMigration_冻结的DRT29文本与00066一致 钉住）；
// 00068 只覆盖仍是这段文本的行。⚠️ 同样不能再改。
func drt29NodesAndEdgesRef(nodes []legacyNodeRow, rules []legacyRuleRow) string {
	names := map[string]string{}
	var b strings.Builder
	b.WriteString(agentSkillPlaceholder + "\n\n# 节点与连线\n\n## 节点类型\n")
	kindNote := map[string]string{
		"generator": "能独立生成内容",
		"composite": "组合 / 批量类节点，由对应流程创建，不要手工新建",
		"material":  "没有独立生成能力",
	}
	typeNote := map[string]string{
		"director": "用 director_create 建、director_build 搭景；**不能截图**，搭完要提醒用户打开导演台截图；" +
			"截图之后才能给下游当输入；连视频节点时建议用全能参考",
	}
	for _, n := range nodes {
		names[n.code] = n.name
		note := kindNote[n.kind]
		if t, ok := typeNote[n.code]; ok {
			note = t
		}
		fmt.Fprintf(&b, "- %s（%s）：%s\n", n.name, n.code, note)
	}
	b.WriteString("- 画框（frame）：纯视觉容器，只用来把相关节点框在一起；与「组」不同，没有批量执行与计费语义\n")
	edgeNote := map[[2]string]string{
		{"image", "director"}: "作导演台的全景背景或搭景参考图，不产生生成任务",
	}
	b.WriteString("\n## 允许的连线（来源 → 目标）\n")
	for _, r := range rules {
		switch note, ok := edgeNote[[2]string{r.src, r.dst}]; {
		case r.defaultOp != "":
			fmt.Fprintf(&b, "- %s → %s（默认操作 %s）\n", names[r.src], names[r.dst], r.defaultOp)
		case ok:
			fmt.Fprintf(&b, "- %s → %s（%s）\n", names[r.src], names[r.dst], note)
		default:
			fmt.Fprintf(&b, "- %s → %s（只作为修饰，叠加到目标节点的提示词）\n", names[r.src], names[r.dst])
		}
	}
	b.WriteString(`
不在上表里的连线一律不允许。视频节点的实际操作以所选模式为准（文生视频 / 图生视频 / 首尾帧 / 全能参考 / 视频生视频），表里的默认操作只是建议。

## 规则
- 连线有执行语义：连上的上游会作为下游生成的输入，顺序按连线的 order。不要用连线表达「来源」「参考过」这类关系。
- 文档节点不能连到任何节点上当输入；规划文档与产物的关系用画框表达。
- 导演台没有截图时，下游生成会被拦下；你不能截图，搭完景要提醒用户打开导演台截图，不要用 generate 去补。
- 不修改用户的节点：要改用户建的或用户改过的节点时，在旁边新建一个，并在回复里说明它与原节点的关系。
- 不删除任何节点；确实需要删除时告诉用户，由用户自己删。
`)
	return b.String()
}

// sct08NodesAndEdgesRef 是 SCT-08 ~ SCT-41 之前 nodesAndEdgesRef 的原样副本（两张表改成参数）。
// 00068 写进库里的就是它的输出（TestAgentSkillMigration_冻结的SCT08文本与00068一致 钉住）；
// 00072 只覆盖仍是这段文本的行。⚠️ 同样不能再改。
func sct08NodesAndEdgesRef(nodes []legacyNodeRow, rules []legacyRuleRow) string {
	names := map[string]string{}
	var b strings.Builder
	b.WriteString(agentSkillPlaceholder + "\n\n# 节点与连线\n\n## 节点类型\n")
	kindNote := map[string]string{
		"generator": "能独立生成内容",
		"composite": "组合 / 批量类节点，由对应流程创建，不要手工新建",
		"material":  "没有独立生成能力",
	}
	typeNote := map[string]string{
		"director": "用 director_create 建、director_build 搭景；**不能截图**，搭完要提醒用户打开导演台截图；" +
			"截图之后才能给下游当输入；连视频节点时建议用全能参考",
		"smartcut": "由用户在画布上新建；连进来的视频 / 图片 / 音频 / 文本按连线顺序自动铺上时间线，" +
			"剪辑在剪辑台里完成；导出成片之后才能给下游当输入",
	}
	for _, n := range nodes {
		names[n.code] = n.name
		note := kindNote[n.kind]
		if t, ok := typeNote[n.code]; ok {
			note = t
		}
		fmt.Fprintf(&b, "- %s（%s）：%s\n", n.name, n.code, note)
	}
	b.WriteString("- 画框（frame）：纯视觉容器，只用来把相关节点框在一起；与「组」不同，没有批量执行与计费语义\n")
	edgeNote := map[[2]string]string{
		{"image", "director"}: "作导演台的全景背景或搭景参考图，不产生生成任务",
	}
	for _, src := range []string{"video", "image", "audio", "text", "script", "group"} {
		edgeNote[[2]string{src, "smartcut"}] = "作剪辑素材，连上即铺上时间线，不产生生成任务"
	}
	b.WriteString("\n## 允许的连线（来源 → 目标）\n")
	for _, r := range rules {
		switch note, ok := edgeNote[[2]string{r.src, r.dst}]; {
		case r.defaultOp != "":
			fmt.Fprintf(&b, "- %s → %s（默认操作 %s）\n", names[r.src], names[r.dst], r.defaultOp)
		case ok:
			fmt.Fprintf(&b, "- %s → %s（%s）\n", names[r.src], names[r.dst], note)
		default:
			fmt.Fprintf(&b, "- %s → %s（只作为修饰，叠加到目标节点的提示词）\n", names[r.src], names[r.dst])
		}
	}
	b.WriteString(`
不在上表里的连线一律不允许。视频节点的实际操作以所选模式为准（文生视频 / 图生视频 / 首尾帧 / 全能参考 / 视频生视频），表里的默认操作只是建议。

## 规则
- 连线有执行语义：连上的上游会作为下游生成的输入，顺序按连线的 order。不要用连线表达「来源」「参考过」这类关系。
- 文档节点不能连到任何节点上当输入；规划文档与产物的关系用画框表达。
- 导演台没有截图时，下游生成会被拦下；你不能截图，搭完景要提醒用户打开导演台截图，不要用 generate 去补。
- 智能剪辑没有导出成片时，下游生成会被拦下；导出要在剪辑台里完成。
- 不修改用户的节点：要改用户建的或用户改过的节点时，在旁边新建一个，并在回复里说明它与原节点的关系。
- 不删除任何节点；确实需要删除时告诉用户，由用户自己删。
`)
	return b.String()
}

// legacyRefV4 是 SCT-08 ~ SCT-41 之前的原文（00068 写进库里的那一段）。
func legacyRefV4() string {
	return sct08NodesAndEdgesRef(nodeTypeRows, withoutScriptInputs(connectionRuleRows))
}

// withoutSmartcut 当前两张表去掉智能剪辑（SCT-08 之前的样子）。
func withoutSmartcut() ([]legacyNodeRow, []legacyRuleRow) {
	var nodes []legacyNodeRow
	for _, n := range nodeTypeRows {
		if n.code != "smartcut" {
			nodes = append(nodes, n)
		}
	}
	var rules []legacyRuleRow
	for _, r := range withoutScriptInputs(connectionRuleRows) {
		if r.src != "smartcut" && r.dst != "smartcut" {
			rules = append(rules, r)
		}
	}
	return nodes, rules
}

// legacyRefV3 是 DRT-29 ~ SCT-08 之前的原文（00066 写进库里的那一段）。
func legacyRefV3() string { return drt29NodesAndEdgesRef(withoutSmartcut()) }

// legacyRefV1 / legacyRefV2 是两段旧原文。
func legacyRefV1() string {
	all, allRules := withoutSmartcut()
	var nodes []legacyNodeRow
	for _, n := range all {
		if n.code != "director" {
			nodes = append(nodes, n)
		}
	}
	var rules []legacyRuleRow
	for _, r := range allRules {
		if r.src != "director" && r.dst != "director" {
			rules = append(rules, r)
		}
	}
	return legacyNodesAndEdgesRef(nodes, rules)
}

func legacyRefV2() string { return legacyNodesAndEdgesRef(withoutSmartcut()) }

var dollarQuoted = regexp.MustCompile(`(?s)\$ref\$(.*?)\$ref\$`)

// migrationSections 读迁移文件，按 `-- +goose Down` 切成 Up / Down 两段，各自取出美元引号里的文本。
func migrationSections(t *testing.T, name string) (up, down []string) {
	t.Helper()
	raw, err := fs.ReadFile(migrations.FS, name)
	if err != nil {
		t.Fatalf("读不到迁移 %s：%v", name, err)
	}
	upPart, downPart, ok := strings.Cut(string(raw), "-- +goose Down")
	if !ok {
		t.Fatal("迁移缺 Down 段")
	}
	for _, m := range dollarQuoted.FindAllStringSubmatch(upPart, -1) {
		up = append(up, m[1])
	}
	for _, m := range dollarQuoted.FindAllStringSubmatch(downPart, -1) {
		down = append(down, m[1])
	}
	return up, down
}

// 00066 里的三段文本逐字等于：DRT-29 的文本（当时的新文本，现已冻结为 legacyRefV3）、两段旧原文。
func TestAgentSkillMigration_节点与连线旧文逐字钉住(t *testing.T) {
	up, down := migrationSections(t, directorSkillRefMigration)
	cur, v1, v2 := legacyRefV3(), legacyRefV1(), legacyRefV2()
	if len(up) != 3 || up[0] != cur || up[1] != v1 || up[2] != v2 {
		t.Fatalf("Up 段应依次是：新文本、DRT-05 之前的原文、DRT-05 之后的原文（逐字）；得到 %d 段，逐段相等=%v",
			len(up), equalEach(up, cur, v1, v2))
	}
	if len(down) != 2 || down[0] != v2 || down[1] != cur {
		t.Fatalf("Down 段应把新文本改回 DRT-05 之后的原文；得到 %d 段，逐段相等=%v", len(down), equalEach(down, v2, cur))
	}
	// 三段互不相同（否则 where 条件形同虚设）；正文里不能出现美元引号的定界符。
	if v1 == v2 || v2 == cur || v1 == cur {
		t.Fatal("新旧文本应各不相同")
	}
	for _, s := range []string{cur, v1, v2} {
		if strings.Contains(s, "$ref$") {
			t.Fatal("正文里出现了 $ref$，会截断美元引号")
		}
		for _, line := range strings.Split(s, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "--") {
				t.Fatalf("正文里有以 -- 开头的行，goose 会当成注释：%s", line)
			}
		}
	}
	// v1 确实没有导演台，v2 有导演台但没有新说明。
	if strings.Contains(v1, "director") || !strings.Contains(v2, "3D 导演台（director）") ||
		strings.Contains(v2, "director_create") {
		t.Fatal("两段旧原文的内容不符合各自的版本")
	}
}

func equalEach(got []string, want ...string) []bool {
	out := make([]bool, len(got))
	for i := range got {
		out[i] = i < len(want) && got[i] == want[i]
	}
	return out
}

// 新文本包含导演台的说明（DRT-29）：建与搭景的工具、不能截图要提醒用户、截图之后才能给下游、连视频建议全能参考；
// 规则段写明没有截图时下游生成会被拦下；图片 → 导演台不再说成「叠加提示词的修饰」。
func TestAgentSkillSeed_节点与连线含导演台说明(t *testing.T) {
	ref := nodesAndEdgesRef()
	line := ""
	for _, l := range strings.Split(ref, "\n") {
		if strings.HasPrefix(l, "- 3D 导演台（director）：") {
			line = l
		}
	}
	for _, want := range []string{"director_create", "director_build", "不能截图", "提醒用户打开导演台截图",
		"截图之后才能给下游当输入", "全能参考"} {
		if !strings.Contains(line, want) {
			t.Errorf("导演台一行缺「%s」：%q", want, line)
		}
	}
	if strings.Contains(line, "不要手工新建") {
		t.Errorf("导演台不该再套 composite 的通用说法：%q", line)
	}
	if !strings.Contains(ref, "- 导演台没有截图时，下游生成会被拦下") {
		t.Error("规则段缺「导演台没有截图时，下游生成会被拦下」")
	}
	if !strings.Contains(ref, "- 图片 → 3D 导演台（作导演台的全景背景或搭景参考图") ||
		strings.Contains(ref, "- 图片 → 3D 导演台（只作为修饰") {
		t.Error("图片 → 导演台的说明不对")
	}
	// 其余 composite（脚本、组）仍是通用说法。
	if !strings.Contains(ref, "- 脚本（script）：组合 / 批量类节点，由对应流程创建，不要手工新建") {
		t.Error("脚本的说法不应改变")
	}
}

// ---------------------------------------------------------------- 00068（智能剪辑，SCT-08）

const smartcutSkillRefMigration = "00068_smartcut_skill_ref.sql"

// 00068：Up 把仍是 DRT-29 原文（00066 写进去的那一段）的行改成 SCT-08 的文本（当时的新文本，现已冻结为 legacyRefV4）；Down 反过来。
func TestAgentSkillMigration_智能剪辑旧文逐字钉住(t *testing.T) {
	up, down := migrationSections(t, smartcutSkillRefMigration)
	cur, v3 := legacyRefV4(), legacyRefV3()
	if len(up) != 2 || up[0] != cur || up[1] != v3 {
		t.Fatalf("Up 段应依次是：新文本、DRT-29 的原文（逐字）；得到 %d 段，逐段相等=%v", len(up), equalEach(up, cur, v3))
	}
	if len(down) != 2 || down[0] != v3 || down[1] != cur {
		t.Fatalf("Down 段应把新文本改回 DRT-29 的原文；得到 %d 段，逐段相等=%v", len(down), equalEach(down, v3, cur))
	}
	if cur == v3 {
		t.Fatal("新旧文本应不同")
	}
	for _, line := range strings.Split(cur, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") || strings.Contains(line, "$ref$") {
			t.Fatalf("正文里有会被 goose 当注释或截断美元引号的行：%s", line)
		}
	}
}

// 新文本含智能剪辑：节点一行（三个 Agent 工具、连上即入轨、导出用 render、导出后才能给下游）、六条入边的说明、
// 两条出边、一条规则（SCT-41）；不提 smartcut_export —— 那是 MCP 才有的工具，站内 Agent 导出用 render。
func TestAgentSkillSeed_节点与连线含智能剪辑说明(t *testing.T) {
	ref := nodesAndEdgesRef()
	line := ""
	for _, l := range strings.Split(ref, "\n") {
		if strings.HasPrefix(l, "- 智能剪辑（smartcut）：") {
			line = l
		}
	}
	for _, want := range []string{"smartcut_create", "smartcut_plan", "smartcut_get", "按连线顺序自动铺上时间线",
		"render", "导出成片之后才能给下游当输入"} {
		if !strings.Contains(line, want) {
			t.Errorf("智能剪辑一行缺「%s」：%q", want, line)
		}
	}
	for _, want := range []string{
		"- 视频 → 智能剪辑（作剪辑素材，连上即铺上时间线，不产生生成任务）",
		"- 组 → 智能剪辑（作剪辑素材，连上即铺上时间线，不产生生成任务）",
		"- 智能剪辑 → 视频（默认操作 video2video）",
		"- 智能剪辑 → 文本（默认操作 video2prompt）",
		"- 智能剪辑没有导出成片时，下游生成会被拦下；导出用 render",
	} {
		if !strings.Contains(ref, want) {
			t.Errorf("缺「%s」", want)
		}
	}
	if strings.Contains(ref, "smartcut_export") {
		t.Error("smartcut_export 只在 MCP 里有，站内 Agent 的技能不该提它")
	}
}

// ---------------------------------------------------------------- 00072（智能剪辑的 Agent 工具，SCT-41）

const smartcutToolsSkillRefMigration = "00072_smartcut_agent_skill_ref.sql"

// 00072：Up 把仍是 SCT-08 原文（00068 写进去的那一段）的行改成新文本；Down 反过来。
func TestAgentSkillMigration_智能剪辑工具逐字钉住(t *testing.T) {
	up, down := migrationSections(t, smartcutToolsSkillRefMigration)
	// 「新文本」是 00072 写进去的那一段：00073 之后它成了旧文本，由冻结的 SCT-41 生成器给出
	cur, v4 := legacyRefV5(), legacyRefV4()
	if len(up) != 2 || up[0] != cur || up[1] != v4 {
		t.Fatalf("Up 段应依次是：新文本、SCT-08 的原文（逐字）；得到 %d 段，逐段相等=%v", len(up), equalEach(up, cur, v4))
	}
	if len(down) != 2 || down[0] != v4 || down[1] != cur {
		t.Fatalf("Down 段应把新文本改回 SCT-08 的原文；得到 %d 段，逐段相等=%v", len(down), equalEach(down, v4, cur))
	}
	if cur == v4 {
		t.Fatal("新旧文本应不同")
	}
	for _, line := range strings.Split(cur, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") || strings.Contains(line, "$ref$") {
			t.Fatalf("正文里有会被 goose 当注释或截断美元引号的行：%s", line)
		}
	}
}

// 冻结的 SCT-08 生成器逐字等于 00068 写进库里的文本 —— 证明冻结是原样的。
func TestAgentSkillMigration_冻结的SCT08文本与00068一致(t *testing.T) {
	up, _ := migrationSections(t, smartcutSkillRefMigration)
	if len(up) == 0 || up[0] != legacyRefV4() {
		t.Fatal("冻结的 SCT-08 文本应逐字等于 00068 Up 段的新文本")
	}
}

// 冻结的 DRT-29 生成器去掉智能剪辑之后，逐字等于 00066 写进库里的文本 —— 证明冻结是原样的。
func TestAgentSkillMigration_冻结的DRT29文本与00066一致(t *testing.T) {
	up, _ := migrationSections(t, directorSkillRefMigration)
	if len(up) == 0 || up[0] != legacyRefV3() {
		t.Fatal("冻结的 DRT-29 文本应逐字等于 00066 Up 段的新文本")
	}
}

// ---------------------------------------------------------------- 00073（文本 / 图片连进脚本，2026-10-30）

const scriptInputsSkillRefMigration = "00073_script_inputs_skill_ref.sql"

// sct41NodesAndEdgesRef 是 SCT-41 ~ 00073 之前 nodesAndEdgesRef 的原样副本（只把两张表改成参数）。⚠️ 不能再改。
func sct41NodesAndEdgesRef(nodes []legacyNodeRow, rules []legacyRuleRow) string {
	names := map[string]string{}
	var b strings.Builder
	b.WriteString(agentSkillPlaceholder + "\n\n# 节点与连线\n\n## 节点类型\n")
	kindNote := map[string]string{
		"generator": "能独立生成内容",
		"composite": "组合 / 批量类节点，由对应流程创建，不要手工新建",
		"material":  "没有独立生成能力",
	}
	// typeNote 按类型覆盖 kindNote 的通用说法：「由对应流程创建」对导演台不成立 —— 它有自己的建节点工具（DRT-29）。
	typeNote := map[string]string{
		"director": "用 director_create 建、director_build 搭景；**不能截图**，搭完要提醒用户打开导演台截图；" +
			"截图之后才能给下游当输入；连视频节点时建议用全能参考",
		// 智能剪辑（docs/agent/smartcut SCT-08 建节点表与连线，SCT-41 补上 Agent 工具）。
		"smartcut": "用 smartcut_create 建（可同时把素材节点按顺序连进来）、smartcut_plan 让 AI 按要求剪（免费，按用户限流）、" +
			"smartcut_get 查看时间线与素材条；连进来的视频 / 图片 / 音频 / 文本按连线顺序自动铺上时间线（还没剪过时）；" +
			"用户也可以在剪辑台里接着剪；导出成片用 render（给 smartcut_get 返回的 timelineId，导出免费）；" +
			"导出成片之后才能给下游当输入",
	}
	for _, n := range nodes {
		names[n.code] = n.name
		note := kindNote[n.kind]
		if t, ok := typeNote[n.code]; ok {
			note = t
		}
		fmt.Fprintf(&b, "- %s（%s）：%s\n", n.name, n.code, note)
	}
	b.WriteString("- 画框（frame）：纯视觉容器，只用来把相关节点框在一起；与「组」不同，没有批量执行与计费语义\n")

	// edgeNote 是没有默认操作、又不是素材修饰的连线（图片 → 导演台：全景背景或搭景参考，不叠加提示词）。
	edgeNote := map[[2]string]string{
		{"image", "director"}: "作导演台的全景背景或搭景参考图，不产生生成任务",
	}
	// 连进智能剪辑的连线：作剪辑素材，连上即铺上时间线（SC-F-04），不产生生成任务。
	for _, src := range []string{"video", "image", "audio", "text", "script", "group"} {
		edgeNote[[2]string{src, "smartcut"}] = "作剪辑素材，连上即铺上时间线，不产生生成任务"
	}
	b.WriteString("\n## 允许的连线（来源 → 目标）\n")
	for _, r := range rules {
		switch note, ok := edgeNote[[2]string{r.src, r.dst}]; {
		case r.defaultOp != "":
			fmt.Fprintf(&b, "- %s → %s（默认操作 %s）\n", names[r.src], names[r.dst], r.defaultOp)
		case ok:
			fmt.Fprintf(&b, "- %s → %s（%s）\n", names[r.src], names[r.dst], note)
		default:
			fmt.Fprintf(&b, "- %s → %s（只作为修饰，叠加到目标节点的提示词）\n", names[r.src], names[r.dst])
		}
	}
	b.WriteString(`
不在上表里的连线一律不允许。视频节点的实际操作以所选模式为准（文生视频 / 图生视频 / 首尾帧 / 全能参考 / 视频生视频），表里的默认操作只是建议。

## 规则
- 连线有执行语义：连上的上游会作为下游生成的输入，顺序按连线的 order。不要用连线表达「来源」「参考过」这类关系。
- 文档节点不能连到任何节点上当输入；规划文档与产物的关系用画框表达。
- 导演台没有截图时，下游生成会被拦下；你不能截图，搭完景要提醒用户打开导演台截图，不要用 generate 去补。
- 智能剪辑没有导出成片时，下游生成会被拦下；导出用 render（给 smartcut_get 返回的 timelineId），或请用户在剪辑台里导出。
- 不修改用户的节点：要改用户建的或用户改过的节点时，在旁边新建一个，并在回复里说明它与原节点的关系。
- 不删除任何节点；确实需要删除时告诉用户，由用户自己删。
`)
	return b.String()
}

// withoutScriptInputs 当前连线表去掉文本 / 图片 → 脚本（00073 之前的样子）。
func withoutScriptInputs(rules []legacyRuleRow) []legacyRuleRow {
	var out []legacyRuleRow
	for _, r := range rules {
		if r.dst != "script" {
			out = append(out, r)
		}
	}
	return out
}

// legacyRefV5 是 SCT-41 ~ 00073 之前的原文（00072 写进库里的那一段）。
func legacyRefV5() string {
	return sct41NodesAndEdgesRef(nodeTypeRows, withoutScriptInputs(connectionRuleRows))
}

// 00073：Up 把仍是 SCT-41 原文（00072 写进去的那一段）的行改成新文本；Down 反过来。
func TestAgentSkillMigration_脚本入边逐字钉住(t *testing.T) {
	up, down := migrationSections(t, scriptInputsSkillRefMigration)
	cur, v5 := nodesAndEdgesRef(), legacyRefV5()
	if len(up) != 2 || up[0] != cur || up[1] != v5 {
		t.Fatalf("Up 段应依次是：新文本、SCT-41 的原文（逐字）；得到 %d 段，逐段相等=%v", len(up), equalEach(up, cur, v5))
	}
	if len(down) != 2 || down[0] != v5 || down[1] != cur {
		t.Fatalf("Down 段应把新文本改回 SCT-41 的原文；得到 %d 段，逐段相等=%v", len(down), equalEach(down, v5, cur))
	}
	if cur == v5 {
		t.Fatal("新旧文本应不同")
	}
	for _, want := range []string{"- 文本 → 脚本（作脚本的剧本原文或参考文本", "- 图片 → 脚本（作脚本的角色参考图"} {
		if !strings.Contains(cur, want) {
			t.Errorf("新文本缺「%s」", want)
		}
	}
}

// 冻结的 SCT-41 生成器逐字等于 00072 写进库里的文本 —— 证明冻结是原样的。
func TestAgentSkillMigration_冻结的SCT41文本与00072一致(t *testing.T) {
	up, _ := migrationSections(t, smartcutToolsSkillRefMigration)
	if len(up) == 0 || up[0] != legacyRefV5() {
		t.Fatal("冻结的 SCT-41 文本应逐字等于 00072 Up 段的新文本")
	}
}
