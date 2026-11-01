package app

import (
	"context"

	"github.com/li904338203/tabtv/backend/internal/canvasgen"
)

// 多段工具集（MCPT-16，MCP-F-33~35）的端口：脚本线 / 整图 / 时间线。实现在 internal/wiring/mcp_multi.go，
// **委托** AGT-50 的 Agent 适配器（wiring.AgentScripts / AgentWorkflows / AgentTimelines）只做类型转换 ——
// 报价口径（「真正提交时会发出去的那组参数」逐项问目录）与参数组合逻辑只有一份（dev/01 §12 R2）。
//
// ⚠️ 不新增任何业务判断（C-1、C-4）：分镜「结算晚于校验」、轮次随调用次数变、资产图带 scriptAssetId、
// 组子项带 groupItemId、余额不足停掉后续项、整图的计划与复用、装配只补缺口、渲染预检……全部在被调用的域里。
// ⚠️ 收的是 Principal：租户一律 Principal.WorkspaceID、操作者 Principal.UserID、付费主体 Principal.PayerAccountID、
// 会员等级 Principal.MemberLevelID；写画布的来源是本授权（Principal.GrantID）。
// ⚠️ 端口**不收金额**：付不付钱、付多少由报价方法给，确认服务决定能不能受理（MCPT-10）。

// ---------------------------------------------------------------- 脚本

// ScriptCreateInput 建脚本节点 + 脚本（「建节点 → 建脚本 → 写回」在 internal/canvasgen，与 Agent 共用）。
type ScriptCreateInput struct {
	ProjectID   int64
	SourceType  string
	RawInput    string
	GlobalStyle string
	Title       string
	// BatchID 建节点那一批的画布幂等键（同键重放返回同一个节点 → 同一个脚本）。
	BatchID string
}

// ScriptShotView 是一个镜头。
type ScriptShotView struct {
	ID                int64
	No                int
	DurationSec       int
	Description       string
	ImagePromptStatus string
	VideoPromptStatus string
	AssetIDs          []int64
}

// ScriptView 是脚本概况 + 镜头表。
type ScriptView struct {
	ID           int64
	ProjectID    int64
	CanvasNodeID string
	SourceType   string
	Status       string
	CurrentStep  int
	Version      int
	Shots        []ScriptShotView
}

// ScriptAssetView 是本脚本的一个资产（只在本脚本内归并，AV-20）。
type ScriptAssetView struct {
	ID      int64
	Name    string
	Type    string
	Status  string
	MediaID int64
}

// ScriptGroupBrief 是本脚本下的一个组。
type ScriptGroupBrief struct {
	ID           int64
	GroupType    string
	Name         string
	Status       string
	CanvasNodeID string
}

// ScriptCallView 是一次同步文本调用（分镜 / 提示词合成，script_llm_call）。
type ScriptCallView struct {
	ID      int64
	Purpose string
	Status  string
	Credits int
	Error   string
}

// ScriptDetail 是 script.get：脚本 + 镜头 + 资产 + 组 + 最近的同步调用。
type ScriptDetail struct {
	Script ScriptView
	Assets []ScriptAssetView
	Groups []ScriptGroupBrief
	Calls  []ScriptCallView
}

// StoryboardInput 是一次分镜生成。
type StoryboardInput struct {
	ScriptID     int64
	ModelCode    string
	ShotCount    int
	ExtraContext string
	// BatchID 非空时把结果写回本授权建的脚本节点（批次 `{BatchID}:script`）；用户的节点只告警不改。
	BatchID string
}

// StoryboardResult 是分镜结果（CallID 是账本 ref_type=script_llm 的 refId）。
type StoryboardResult struct {
	Script  ScriptView
	CallID  int64
	Credits int
}

// AssetsInput 是资产对账 + 批量出图。AssetIDs 为空 = 本脚本全部未就绪的资产。
type AssetsInput struct {
	ScriptID  int64
	ModelCode string
	AssetIDs  []int64
	Params    map[string]any
}

// AssetsQuote 是出图前的报价（先对账：免费、幂等）。
type AssetsQuote struct {
	Pending   []ScriptAssetView
	Total     int
	Unmatched []string
}

// AssetItemResult 是批量出图的一项。
type AssetItemResult struct {
	AssetID      int64
	Name         string
	GenerationID int64
	Skipped      bool
	Reason       string
}

// ComposeInput 是提示词合成。Source ∈ ai | template。
type ComposeInput struct {
	ScriptID  int64
	ShotIDs   []int64
	Kinds     []string
	Source    string
	ModelCode string
	Force     bool
	BatchID   string
}

// ComposeQuote 是合成报价（脚本域 DryRun 的原样投影，GAP-21）。
type ComposeQuote struct {
	AICount       int
	TemplateCount int
	SkipCount     int
	Total         int
}

// ComposeItemResult 是合成的一项。
type ComposeItemResult struct {
	ShotID  int64
	ShotNo  int
	Kind    string
	Status  string
	Skipped bool
	Reason  string
}

// ComposeResult 是合成结果。
type ComposeResult struct {
	Items   []ComposeItemResult
	CallIDs []int64
}

// GroupCreateInput 建组。GroupType ∈ asset | storyboard_image | video_generator。
type GroupCreateInput struct {
	ScriptID    int64
	GroupType   string
	Name        string
	ShotIDs     []int64
	ModelCode   string
	Operation   string
	DurationSec int
	Durations   map[int64]int
	Params      map[string]any
	// BatchID 建组节点那一批的画布幂等键。⚠️ 报价时不建节点（用户确认之前不在画布上留东西）。
	BatchID string
}

// GroupExecuteInput 执行组（或指定子项）。
type GroupExecuteInput struct {
	GroupID     int64
	ModelCode   string
	Operation   string
	DurationSec int
	Force       bool
	ItemIDs     []int64
	BatchID     string
}

// GroupItemQuote 是组里一项的报价。
type GroupItemQuote struct {
	ItemID      int64
	ShotID      int64
	ShotNo      int
	Operation   string
	DurationSec int
	Inputs      int
	Credits     int
}

// GroupQuote 是组的报价：会提交的项 + 不会提交的项与原因（镜号 + 原因，不静默）。
type GroupQuote struct {
	Items   []GroupItemQuote
	Skipped []string
	Total   int
}

// GroupItemView 是组里一项的当前状态。
type GroupItemView struct {
	ID           int64
	ItemNo       int
	ShotID       int64
	ShotNo       int
	AssetID      int64
	GenerationID int64
	Status       string
	Credits      int
	Operation    string
	DurationSec  int
	Inputs       int
	Error        string
}

// GroupView 是组的当前状态。
type GroupView struct {
	ID           int64
	ProjectID    int64
	ScriptID     int64
	CanvasNodeID string
	GroupType    string
	Name         string
	Status       string
	Version      int
	Items        []GroupItemView
}

// Scripts 是脚本线端口（MCP-F-33）。
type Scripts interface {
	// Create 在画布上建 MCP 来源的 script 节点 → 按节点建脚本 → 写回节点；返回脚本与节点 ID。
	Create(ctx context.Context, p Principal, in ScriptCreateInput) (ScriptView, string, error)
	// Get 是 script.get。
	Get(ctx context.Context, p Principal, scriptID int64) (ScriptDetail, error)
	QuoteStoryboard(ctx context.Context, p Principal, in StoryboardInput) (int, error)
	// Storyboard 同步生成分镜：扣费在 script_llm_call，**解析失败由脚本域全额退款**（结算晚于校验）。
	Storyboard(ctx context.Context, p Principal, in StoryboardInput) (StoryboardResult, error)
	// QuoteAssets 先对账（免费、幂等）再按「待出图资产 × 每张真正会发出去的提示词」报价。
	QuoteAssets(ctx context.Context, p Principal, in AssetsInput) (AssetsQuote, error)
	GenerateAssets(ctx context.Context, p Principal, in AssetsInput) ([]AssetItemResult, error)
	// QuoteCompose 走脚本域自己的 DryRun。
	QuoteCompose(ctx context.Context, p Principal, in ComposeInput) (ComposeQuote, error)
	Compose(ctx context.Context, p Principal, in ComposeInput) (ComposeResult, error)
	// QuoteGroupCreate 只对分镜图组有金额（建组即提交）；视频组 / 资产组返回 0 元。
	QuoteGroupCreate(ctx context.Context, p Principal, in GroupCreateInput) (GroupQuote, error)
	// CreateGroup 建组节点（MCP 来源）→ 建组（分镜图组建组即提交）→ 写回；返回组与节点 ID。
	CreateGroup(ctx context.Context, p Principal, in GroupCreateInput) (GroupView, string, error)
	QuoteGroupExecute(ctx context.Context, p Principal, in GroupExecuteInput) (GroupQuote, error)
	ExecuteGroup(ctx context.Context, p Principal, in GroupExecuteInput) (GroupView, error)
	Group(ctx context.Context, p Principal, groupID int64) (GroupView, error)
}

// ---------------------------------------------------------------- 整图

// WorkflowPlanInput 是整图预览 / 运行的范围。
type WorkflowPlanInput struct {
	ProjectID     int64
	TargetNodeIDs []string
	Force         bool
}

// WorkflowIssue 是计划里的一个问题（带涉及的节点）。
type WorkflowIssue struct {
	Message string
	Nodes   []string
}

// WorkflowBlocked 是一个跑不了的节点与原因。
type WorkflowBlocked struct {
	NodeID string
	Reason string
}

// WorkflowPlan 是运行前预览（只读、不扣费，NEW-01）。
type WorkflowPlan struct {
	Executable       bool
	Summary          string
	Issues           []WorkflowIssue
	Blocked          []WorkflowBlocked
	RunCount         int
	ReuseCount       int
	SkipCount        int
	TotalCredits     int
	EstimatedSeconds int
}

// WorkflowNodeView 是运行里一个节点的状态（skip 与 blocked 分开给，CD-41）。
type WorkflowNodeView struct {
	NodeID       string
	Status       string
	GenerationID int64
	Credits      int
	Reused       bool
	Error        string
}

// WorkflowRunView 是一次运行。
type WorkflowRunView struct {
	ID           int64
	ProjectID    int64
	Status       string
	TotalCredits int
	Nodes        []WorkflowNodeView
}

// Workflows 是整图端口（MCP-F-34）。取消走既有的 TaskControl（kind=workflow），不另开一条取消路径。
type Workflows interface {
	Plan(ctx context.Context, p Principal, in WorkflowPlanInput) (WorkflowPlan, error)
	// Start 没有运行级幂等键：防重靠确认服务（quote 的 CAS + 记在 quote 上的运行 ID）。
	Start(ctx context.Context, p Principal, in WorkflowPlanInput) (WorkflowRunView, error)
	Run(ctx context.Context, p Principal, runID int64) (WorkflowRunView, error)
}

// ---------------------------------------------------------------- 时间线

// TimelineView 是时间线摘要。
type TimelineView struct {
	ID          int64
	ScriptID    int64
	Version     int
	Name        string
	AspectRatio string
	ClipCount   int
	TotalMs     int
	EffectiveMs int
	// Created：这次调用新建的。
	Created bool
}

// TimelineRef 指定一条时间线：TimelineID，或 ScriptID（取该脚本最新的一条）。
type TimelineRef struct {
	ProjectID  int64
	ScriptID   int64
	TimelineID int64
	// 新建时用。
	Name        string
	AspectRatio string
}

// AssembleGap 是装配缺口：镜号 + 原因（NEW-09 不静默跳过）。
type AssembleGap struct {
	ShotNo int
	Reason string
}

// AssembleOutcome 是装配结果（DryRun 时是计划）。Timeline 为 nil 表示还没有时间线（只预览了计划）。
type AssembleOutcome struct {
	Summary  string
	Append   int
	Skipped  []int
	Gaps     []AssembleGap
	Timeline *TimelineView
	DryRun   bool
}

// RenderIssue 是渲染预检问题（ClipIdx 指出第几段，0 表示与片段无关）。
type RenderIssue struct {
	Code    string
	Detail  string
	ClipIdx int
}

// RenderPrecheck 是预检 + 报价（与 StartRender 第一步同一个函数）。
type RenderPrecheck struct {
	Issues     []RenderIssue
	DurationMs int
	Credits    int
}

// RenderView 是渲染任务。
type RenderView struct {
	ID            int64
	TimelineID    int64
	Status        string
	Progress      int
	Stage         string
	Credits       int
	OutputMediaID int64
	Error         string
}

// Timelines 是时间线端口（MCP-F-35）。
type Timelines interface {
	// Assemble 按分镜装配。DryRun 时**不写库**：没有时间线也不建（只给计划）；非 DryRun 时没有就建一条。
	Assemble(ctx context.Context, p Principal, ref TimelineRef, force, dryRun bool) (AssembleOutcome, error)
	PrecheckRender(ctx context.Context, p Principal, timelineID int64) (RenderPrecheck, error)
	// StartRender 预检 → 报价 → 扣费 → 独立队列（timeline 域）；idemKey = mcp:{grant}:{quote}。
	StartRender(ctx context.Context, p Principal, timelineID int64, idemKey string) (RenderView, error)
	// Get 是时间线 + 最近一次渲染（没有渲染时 nil）。
	Get(ctx context.Context, p Principal, timelineID int64) (TimelineView, *RenderView, error)
	Render(ctx context.Context, p Principal, renderID int64) (RenderView, error)
}

// ---------------------------------------------------------------- 3D 导演台（DRT-29，DR-F-163）

// DirectorCreateInput 建导演台节点 + 场景（「建节点 → 建场景 → 写回」在 internal/canvasgen，与 Agent 共用）。
type DirectorCreateInput struct {
	ProjectID int64
	Title     string
	Position  *canvasgen.Position
	// BatchID 建节点那一批的画布幂等键（同键重放返回同一个节点 → 同一份场景）。
	BatchID string
}

// DirectorBuildInput 是一次 AI 搭景（免费、按用户限流）。
type DirectorBuildInput struct {
	ProjectID         int64
	NodeID            string
	Prompt            string
	ReferenceMediaIDs []int64
	// BatchID 是节点缺场景、要补写 sceneId 时那一批的前缀（写回批次 `{BatchID}:scene`）。
	BatchID string
}

// DirectorBuildResult 是已写入场景的结果。Repair 非空表示这次顺带给节点补上 / 分出了自己的场景。
type DirectorBuildResult struct {
	SceneID  int64
	Version  int
	Added    []string
	Updated  []string
	Removed  []string
	Defaults []string
	Notice   string
	Repair   string
}

// Directors 是 3D 导演台端口（DR-I-06）。
//
// ⚠️ 业务错误原样返回 *httpx.BizError：限流 42901 / 42902（details 带 retry_after_seconds）、方案无法解析 42216、
// 不可用 42203（details.userFixable）、写入冲突 40902 —— transport 把码与详情原样交给客户端（MCP-I-06）。
type Directors interface {
	// Create 在画布上建 MCP 来源的导演台节点 → 按节点建场景 → 写回 params.sceneId；返回节点与场景 ID。
	Create(ctx context.Context, p Principal, in DirectorCreateInput) (string, int64, error)
	// Build 按节点取场景（缺了先建、共用别人的先分叉）→ AI 搭景并以乐观锁写入（来源 mcp）。
	Build(ctx context.Context, p Principal, in DirectorBuildInput) (DirectorBuildResult, error)
}

// ---------------------------------------------------------------- 智能剪辑（docs/agent/smartcut SCT-41，SC-F-160~165）

// SmartcutCreateInput 建智能剪辑节点 + 时间线，可同时把素材节点按顺序连进来（连上即铺上时间线）。
type SmartcutCreateInput struct {
	ProjectID    int64
	Title        string
	Position     *canvasgen.Position
	InputNodeIDs []string
	// BatchID 建节点那一批的画布幂等键（同键重放返回同一个节点）。
	BatchID string
}

// SmartcutCreated 建好的节点、时间线与连线结果。SyncError 非空：素材已连上，但铺上时间线失败（打开剪辑台时会再铺）。
type SmartcutCreated struct {
	NodeID     string
	TimelineID int64
	Connected  []string
	Existing   []string
	Appended   int
	SyncError  string
	Summary    SmartcutSummary
}

// SmartcutRender 最近一次导出。
type SmartcutRender struct {
	ID            int64
	Status        string
	Progress      int
	Stage         string
	OutputMediaID int64
	Error         string
}

// SmartcutSummary 时间线摘要。
type SmartcutSummary struct {
	TimelineID   int64
	Version      int
	IsArranged   bool
	ClipCount    int
	TotalMs      int
	EffectiveMs  int
	InputsStale  bool
	RenderStale  bool
	LatestRender *SmartcutRender
	// ActiveCallKind / ActiveCallStage 进行中的 AI 调用（AI 剪辑 / 追加指令 / 翻译），没有时为空。
	ActiveCallKind  string
	ActiveCallStage string
}

// SmartcutInput 素材条的一项。
type SmartcutInput struct {
	No           string
	Kind         string
	Status       string
	Title        string
	SourceNodeID string
	DurationMs   int
	Used         bool
	Reason       string
}

// SmartcutClip 片段清单的一行。
type SmartcutClip struct {
	TrackKind    string
	TrackRole    string
	No           int
	MediaID      int64
	Origin       string
	SourceNodeID string
	StartMs      int
	DurationMs   int
	Hidden       bool
}

// SmartcutDetail 是 smartcut_get 的只读详情。HasTimeline=false：节点还没有自己的时间线（只读路径不替它补建）。
type SmartcutDetail struct {
	NodeID      string
	HasTimeline bool
	SmartcutSummary
	Inputs  []SmartcutInput
	Omitted int
	Clips   []SmartcutClip
}

// SmartcutPlanInput 一次 AI 剪辑（免费、按用户限流，与网页共用额度）。
type SmartcutPlanInput struct {
	ProjectID   int64
	NodeID      string
	Mode        string
	Instruction string
	TargetSec   int
	AspectRatio string
	Variants    int
	// BatchID 是节点缺时间线、要补写 timelineId 时那一批的前缀（写回批次 `{BatchID}:timeline`）。
	BatchID string
}

// SmartcutVariant 一套方案的结果。
type SmartcutVariant struct {
	Index        int
	Status       string
	Summary      string
	TotalMs      int
	Warnings     []string
	UsedFallback bool
	Error        string
}

// SmartcutPlanResult 已写入的结果。Repair 非空表示这次顺带给节点补上 / 分出了自己的时间线。
type SmartcutPlanResult struct {
	TimelineID     int64
	CallID         int64
	AppliedVariant int
	Variants       []SmartcutVariant
	SnapshotID     int64
	Repair         string
	Summary        SmartcutSummary
}

// SmartcutExportSpec 导出规格（空 / 0 = 按时间线自身）。
type SmartcutExportSpec struct {
	Resolution string
	FPS        int
}

// Smartcuts 是智能剪辑端口（SC-F-165）。租户、操作者、会员等级一律取 Principal；来源 = 本授权。
//
// ⚠️ 业务错误原样返回 *httpx.BizError：限流 42901 / 42902（details 带 retry_after_seconds）、素材不合适 42217、
// 没有可用模型 42218、写入冲突 40902 —— transport 把码与详情原样交给客户端。
type Smartcuts interface {
	// Create 建 MCP 来源的智能剪辑节点 → 按节点建时间线 → 写回 params.timelineId → 连上素材并铺轨。
	Create(ctx context.Context, p Principal, in SmartcutCreateInput) (SmartcutCreated, error)
	// Get 只读详情：不建、不写（只读凭据也能调）。
	Get(ctx context.Context, p Principal, projectID int64, nodeID string) (SmartcutDetail, error)
	// Plan 按节点取时间线（缺了先补建、共用别人的先分叉）→ AI 剪辑并直接写入（来源 mcp）。
	Plan(ctx context.Context, p Principal, in SmartcutPlanInput) (SmartcutPlanResult, error)
	// PrecheckExport 导出预检（免费、只读）：返回节点自己的时间线与预检结果；节点还没有时间线时回 40905。
	PrecheckExport(ctx context.Context, p Principal, projectID int64, nodeID string, spec SmartcutExportSpec) (
		int64, RenderPrecheck, error)
	// StartExport 走同一条导出路径（timeline 域：预检 → 报价 → 独立队列），idemKey 同键只建一个任务。
	StartExport(ctx context.Context, p Principal, projectID int64, nodeID string, spec SmartcutExportSpec,
		idemKey string) (RenderView, error)
}

// ---------------------------------------------------------------- 提示词（F-37）

// SkillPrompt 是一个启用的站内技能（agent_skill）派生出的提示词。Body 已渲染（变量给全，无 `{{` 残留）。
type SkillPrompt struct {
	Code        string
	Name        string
	Description string
	Body        string
}

// Skills 取启用的技能并渲染正文（实现：wiring.MCPSkills → agent/app.Service.Skills + SkillRenderer）。
// ⚠️ 停用的技能不出现；渲染失败的技能**不出现**并告警（原样给出的话模型会照着 `{{var}}` 字面量执行）。
type Skills interface {
	Prompts(ctx context.Context) ([]SkillPrompt, error)
}
