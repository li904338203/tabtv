package domain

import "fmt"

// ToolName 是发给模型的工具名。
type ToolName string

// 第一期工具（声明顺序 = AllowedTools 的返回顺序，便于断言请求体）。
const (
	ToolReadCanvas   ToolName = "read_canvas"
	ToolListAssets   ToolName = "list_assets"
	ToolQuote        ToolName = "quote"
	ToolAskUser      ToolName = "ask_user"
	ToolReadDoc      ToolName = "read_doc"
	ToolWriteDoc     ToolName = "write_doc"
	ToolSubmitPlan   ToolName = "submit_plan"
	ToolCanvasApply  ToolName = "canvas_apply"
	ToolGenerate     ToolName = "generate"
	ToolCancelTask   ToolName = "cancel_task"
	ToolLoadSkill    ToolName = "load_skill"
	ToolReadSkillRef ToolName = "read_skill_ref"
)

// 第二期（多段）工具（AGT-50）：驱动脚本线 → 时间线 → 渲染。**只在执行态下发**。
const (
	ToolScriptCreate     ToolName = "script_create"
	ToolScriptStoryboard ToolName = "script_storyboard"
	ToolScriptCompose    ToolName = "script_compose"
	ToolScriptAssets     ToolName = "script_assets"
	ToolGroupCreate      ToolName = "group_create"
	ToolGroupExecute     ToolName = "group_execute"
	ToolRunWorkflow      ToolName = "run_workflow"
	ToolTimelineAssemble ToolName = "timeline_assemble"
	ToolRender           ToolName = "render"
)

// 3D 导演台工具（docs/agent/director，DRT-29，DRD-32）：建导演台节点、AI 搭景。**只在执行态下发**。
const (
	ToolDirectorCreate ToolName = "director_create"
	ToolDirectorBuild  ToolName = "director_build"
)

// 智能剪辑工具（docs/agent/smartcut SCT-41，SC-F-160~162）：建节点（可同时连上素材）、只读详情、免费的 AI 剪辑。
// 导出沿用 render（给 smartcut_get 返回的 timelineId）。
const (
	ToolSmartcutCreate ToolName = "smartcut_create"
	ToolSmartcutGet    ToolName = "smartcut_get"
	ToolSmartcutPlan   ToolName = "smartcut_plan"
)

// 营销场景工具（MKT-T-18）：**只在营销会话下发**（ToolSpec.Scenes），通用会话的工具列表逐字不变。
const (
	// ToolListMarketingOptions 只读：列此刻在投的创意 / Hook / 风格（名称、说明、标签，不含片段原文）。
	ToolListMarketingOptions ToolName = "list_marketing_options"
	// ToolViewMedia 只读：按序号查看一张商品图（MKT-T-19）。图由 Runner 在下一次模型调用时附上，
	// 只能取本会话 scene_input 里的商品图 —— 不能借它按 ID 读任意媒体。
	ToolViewMedia ToolName = "view_media"
)

// 工作区动作（取值与 collaboration/domain.Action 逐字一致，app 层转换）。
// 本包不 import 协作域，所以用字符串常量。
const (
	ActionView     = "view"
	ActionEdit     = "edit"
	ActionGenerate = "generate"
	ActionExport   = "export"
)

// EditorAgent 是 Agent 写时间线时记下的编辑者来源（与时间线域 last_editor_kind 的取值逐字一致）：
// 开着剪辑台的人据此看到「时间线被 Agent 更新」（SC-F-166）。
const EditorAgent = "agent"

// ToolSpec 是一个工具的静态规格。
type ToolSpec struct {
	Name ToolName
	// Paid：调用会扣积分（走 PaidCall 外壳、检查额度）。
	Paid bool
	// CanvasWrite：会写画布（建 / 改节点、连线、画框）。
	CanvasWrite bool
	// Action：执行前要实时校验的工作区动作。
	Action string
	// States：在哪些会话状态下发给模型。
	States []State
	// Scenes：只在哪些会话场景下发；nil = 全部场景（通用工具）。
	// ⚠️ 场景只能收窄：营销专属工具在通用会话里既不下发、也不允许执行（MKR-10）。
	Scenes []Scene
}

var (
	planAndExec = []State{StatePlanning, StateExecuting}
	execOnly    = []State{StateExecuting}
	planOnly    = []State{StatePlanning}

	marketingOnly = []Scene{SceneMarketing}
)

// toolSpecs 是全部工具规格的**唯一定义**，一行一个工具，顺序即下发顺序。
//
// ⚠️ AllowedTools 是 AG-05 的唯一落点：任何「临时放开某工具」的需求都必须改这张表
// 并让测试变红，不允许在 runner 里加特例。
// planning = 只读 + ask_user + 文档读写 + submit_plan + 技能；
// executing = 全部工具 − submit_plan；其余状态不调用模型，为空集。
var toolSpecs = []ToolSpec{
	{Name: ToolReadCanvas, Action: ActionView, States: planAndExec},
	{Name: ToolListAssets, Action: ActionView, States: planAndExec},
	{Name: ToolQuote, Action: ActionView, States: planAndExec},
	{Name: ToolAskUser, Action: ActionEdit, States: planAndExec},
	{Name: ToolReadDoc, Action: ActionView, States: planAndExec},
	{Name: ToolWriteDoc, Action: ActionEdit, States: planAndExec},
	{Name: ToolSubmitPlan, Action: ActionEdit, States: planOnly},
	{Name: ToolCanvasApply, CanvasWrite: true, Action: ActionEdit, States: execOnly},
	// generate 会先建占位节点再受理，所以同时是画布写工具。
	{Name: ToolGenerate, Paid: true, CanvasWrite: true, Action: ActionGenerate, States: execOnly},
	// cancel_task 不扣费（退款由生成域完成），但会改变任务状态，只在执行态。
	{Name: ToolCancelTask, Action: ActionGenerate, States: execOnly},
	{Name: ToolLoadSkill, Action: ActionView, States: planAndExec},
	{Name: ToolReadSkillRef, Action: ActionView, States: planAndExec},
	// ---- 第二期：多段（AGT-50）。全部只在执行态；规划态一个都不下发（付费的由硬规则兜底，
	// 免费的 script_create / timeline_assemble 也会写画布或时间线，同样不是规划态该做的事）。
	// script_create 免费，但会建脚本节点并建脚本。
	{Name: ToolScriptCreate, CanvasWrite: true, Action: ActionEdit, States: execOnly},
	// 分镜 / 合成之后把镜头数、提示词就绪数写回 Agent 建的脚本节点（画布显示依据），所以也是画布写。
	{Name: ToolScriptStoryboard, Paid: true, CanvasWrite: true, Action: ActionGenerate, States: execOnly},
	{Name: ToolScriptAssets, Paid: true, Action: ActionGenerate, States: execOnly},
	{Name: ToolScriptCompose, Paid: true, CanvasWrite: true, Action: ActionGenerate, States: execOnly},
	// group_create：分镜图组**建组即提交 = 扣费**，视频组只落 draft。按会扣费的那一种判 generate
	// （HTTP 侧 POST /scripts/{id}/groups 仍挂默认 edit 闸，差异另行评估）。
	{Name: ToolGroupCreate, Paid: true, CanvasWrite: true, Action: ActionGenerate, States: execOnly},
	{Name: ToolGroupExecute, Paid: true, CanvasWrite: true, Action: ActionGenerate, States: execOnly},
	{Name: ToolRunWorkflow, Paid: true, Action: ActionGenerate, States: execOnly},
	{Name: ToolTimelineAssemble, Action: ActionEdit, States: execOnly},
	// render 把内容取出去（成片），按 export 判（同 HTTP 侧的渲染闸，CD-59）。
	{Name: ToolRender, Paid: true, Action: ActionExport, States: execOnly},
	// ---- 3D 导演台（DRT-29，DRD-32）：免费，但写画布与场景，不是规划态该做的事。
	// 不标 generate：那是「会花用户的钱」的标记（CD-59），标在免费动作上会把只有 edit 的角色误拦。
	// director_build 调一次看图的文本模型（免费、按用户限流，DRK-4）写场景；节点缺场景时还会补写 params.sceneId。
	{Name: ToolDirectorCreate, CanvasWrite: true, Action: ActionEdit, States: execOnly},
	{Name: ToolDirectorBuild, CanvasWrite: true, Action: ActionEdit, States: execOnly},
	// ---- 智能剪辑（SCT-41）：create / plan 免费（SCK-2），但写画布与时间线；plan 按节点自愈时会补写
	// params.timelineId，所以也是画布写。不标 generate（同导演台：免费动作标 generate 会把只有 edit 的角色误拦）。
	// get 只读，但同多段工具一样只在执行态下发（AGT-50：规划态一个多段工具都不下发，规划时看画布用 read_canvas）。
	{Name: ToolSmartcutCreate, CanvasWrite: true, Action: ActionEdit, States: execOnly},
	{Name: ToolSmartcutGet, Action: ActionView, States: execOnly},
	{Name: ToolSmartcutPlan, CanvasWrite: true, Action: ActionEdit, States: execOnly},
	// ---- 营销场景（MKT-T-18）：只读、免费，规划与执行都可用；只在营销会话下发。
	{Name: ToolListMarketingOptions, Action: ActionView, States: planAndExec, Scenes: marketingOnly},
	{Name: ToolViewMedia, Action: ActionView, States: planAndExec, Scenes: marketingOnly},
}

// Tools 按声明顺序返回全部工具规格（副本）。
func Tools() []ToolSpec {
	out := make([]ToolSpec, len(toolSpecs))
	copy(out, toolSpecs)
	return out
}

// SpecOf 查一个工具的规格；未知工具返回 false。
func SpecOf(name ToolName) (ToolSpec, bool) {
	for _, s := range toolSpecs {
		if s.Name == name {
			return s, true
		}
	}
	return ToolSpec{}, false
}

// AllowedTools 给出**通用会话**某状态下发给模型的工具，等价于 AllowedToolsIn(SceneGeneral, …)。
func AllowedTools(state State, skillWhitelist []ToolName) []ToolName {
	return AllowedToolsIn(SceneGeneral, state, skillWhitelist)
}

// AllowedToolsIn 给出某场景、某状态下发给模型的工具：先按场景与状态取集合，再与技能白名单**取交集**。
//
//   - 白名单为空（nil 或长度 0）= 不收窄；白名单只能收窄，**不能放宽**
//     （技能把 generate 写进白名单，planning 下照样没有 generate）；
//   - 场景专属工具（Scenes 非空）只在对应场景下发；空场景按 general；
//   - 返回顺序固定为声明顺序，与白名单顺序无关。
func AllowedToolsIn(scene Scene, state State, skillWhitelist []ToolName) []ToolName {
	var out []ToolName
	for _, s := range toolSpecs {
		if allowed(s, scene, state, skillWhitelist) {
			out = append(out, s.Name)
		}
	}
	return out
}

// IsToolAllowed 是通用会话的执行前二次校验，等价于 IsToolAllowedIn(SceneGeneral, …)。
func IsToolAllowed(state State, skillWhitelist []ToolName, name ToolName) bool {
	return IsToolAllowedIn(SceneGeneral, state, skillWhitelist, name)
}

// IsToolAllowedIn 执行前二次校验：模型可能凭历史幻觉出一个本次没下发的工具名。
// 与 AllowedToolsIn 共用同一个判定。
func IsToolAllowedIn(scene Scene, state State, skillWhitelist []ToolName, name ToolName) bool {
	s, ok := SpecOf(name)
	return ok && allowed(s, scene, state, skillWhitelist)
}

func allowed(s ToolSpec, scene Scene, state State, wl []ToolName) bool {
	// ⚠️ 硬规则写在代码里而不只在表里：规格表哪天被误配，规划态也不会漏出
	// 付费或画布写工具（AG-05「由服务端保证」，测试同时按规格反查）。
	if state == StatePlanning && (s.Paid || s.CanvasWrite) {
		return false
	}
	if !containsState(s.States, state) {
		return false
	}
	if len(s.Scenes) > 0 && !containsScene(s.Scenes, scene) {
		return false
	}
	if len(wl) == 0 {
		return true
	}
	// 场景专属的只读工具不受技能白名单收窄（04 文档 P-51）：它们是平台给这个场景配的上下文工具，
	// 平台自己写的「营销输入」段就让模型「用 view_media 按序号查看」、场景段让它「需要时用
	// list_marketing_options」—— 被某个技能的白名单收掉的话，模型照着提示去调就被拒。
	// 只放行免费、不写画布的（付费 / 写画布的照样只能收窄）。
	if len(s.Scenes) > 0 && !s.Paid && !s.CanvasWrite {
		return true
	}
	for _, n := range wl {
		if n == s.Name {
			return true
		}
	}
	return false
}

// containsScene 判断场景是否在列表里；空场景按 general（旧会话、未带场景的调用方）。
func containsScene(ss []Scene, s Scene) bool {
	if s == "" {
		s = SceneGeneral
	}
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func containsState(ss []State, s State) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// PaidIdempotencyKey 是付费工具调用的幂等键：`agent:{session}:{step}:{attempt}`。
//
// ⚠️ 必须随 attempt 变：不变的话重试命中上一次那条失败记录，用户点多少次都是
// 同一个结果（CD-34 脚本轮次、整图重试已踩过两次）。
func PaidIdempotencyKey(sessionID, stepID int64, attempt int) string {
	return fmt.Sprintf("agent:%d:%d:%d", sessionID, stepID, attempt)
}
