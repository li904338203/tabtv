package domain

import (
	"sort"
	"strings"
)

// 提示词（MCPT-16，MCP-F-37）：从站内技能派生，正文是**渲染后的**技能内容 + 本节的「站内工具名 → MCP 工具名」对照。
//
// ⚠️ 对照由代码生成，不进可编辑的技能正文：技能正文写的是站内 Agent 的工具名（read_canvas、ask_user……），
// 外部模型照着调会找不到工具；把对照写进技能正文的话，运营改一个字就会让站内 Agent 也看到一段说外部工具的话。
// ⚠️ 表要覆盖站内 Agent 的**全部**工具名（wiring 里有绊线测试对着 agent/domain.Tools() 核）：漏一个，
// 模型照着技能调那个工具就是「工具不存在」，而它不知道该换成哪个。

// AgentToolMapping 是站内 Agent 工具名 → MCP 里怎么做（工具.method 或说明）。
var AgentToolMapping = map[string]string{
	"read_canvas":       "canvas_read（或资源 seeaitv://projects/{projectId}/canvas）",
	"list_assets":       "MCP 不提供跨脚本资产库：本脚本的资产用 script.get；要复用别的脚本里的角色，请用户在 seeAitv 里手动挑选",
	"quote":             "quote",
	"ask_user":          "直接在对话里问用户（MCP 没有提问卡片）",
	"read_doc":          "documents.get（或资源 seeaitv://projects/{projectId}/documents/{docId}）",
	"write_doc":         "documents.put",
	"submit_plan":       "MCP 没有规划卡：把计划写进对话或 documents（/plan.md）；每个付费步骤由平台单独请用户确认费用",
	"canvas_apply":      "canvas_apply",
	"generate":          "generate",
	"cancel_task":       "tasks.cancel",
	"load_skill":        "本提示词即技能正文（prompts/get）；其它技能用 prompts/list 查看",
	"read_skill_ref":    "MCP 不提供技能附属文档",
	"script_create":     "script.create",
	"script_storyboard": "script.storyboard",
	"script_compose":    "script.compose",
	"script_assets":     "script.assets",
	"group_create":      "script.group_create",
	"group_execute":     "script.group_execute",
	"run_workflow":      "workflow.plan（免费预览）→ workflow.run",
	"timeline_assemble": "timeline.assemble（默认 dryRun=true，确认后传 dryRun=false）；不走脚本时用 smartcut_create（inputNodeIds 按顺序列出视频节点）",
	"render":            "智能剪辑节点的成片用 smartcut_export（默认开启、导出免费）；其余时间线用 timeline.render（timeline 工具集默认关闭）",
	// 3D 导演台（DRT-29）：同名工具。
	"director_create": "director_create",
	"director_build":  "director_build",
	// 智能剪辑（SCT-41）：同名工具，导出另有 smartcut_export。
	"smartcut_create": "smartcut_create",
	"smartcut_get":    "smartcut_get",
	"smartcut_plan":   "smartcut_plan",
	// 营销场景专属（MKT-T-18）：只在站内营销会话下发，MCP 没有营销会话。
	"list_marketing_options": "MCP 不提供营销选项：创意 / Hook / 风格只能在 seeAitv 营销页选择",
	"view_media":             "media.get（按媒体 ID 取信息与缩略图；MCP 没有营销会话的商品图序号）",
}

// AgentToolNotice 是追加在每个技能提示词末尾的对照说明（代码拥有，按工具名排序，输出稳定）。
func AgentToolNotice() string {
	names := make([]string, 0, len(AgentToolMapping))
	for k := range AgentToolMapping {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("\n\n---\n【在 seeAitv MCP 中使用本技能】上文出现的是 seeAitv 站内 Agent 的工具名，在这里请改用对应的 MCP 工具：\n")
	for _, n := range names {
		b.WriteString("- ")
		b.WriteString(n)
		b.WriteString(" → ")
		b.WriteString(AgentToolMapping[n])
		b.WriteString("\n")
	}
	b.WriteString("付费步骤直接调用即可，平台会请用户确认费用；提交后用 tasks 查询，宿主超时不代表失败，不要重复提交。")
	return b.String()
}
