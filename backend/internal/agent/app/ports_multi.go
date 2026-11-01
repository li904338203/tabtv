package app

import (
	"context"
	"errors"

	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
)

// 多段工具（AGT-50）的端口：脚本线 / 整图 / 时间线，以及扫描循环按业务引用查终态。
//
// ⚠️ 端口只描述「编排要用的事实」，实现（internal/wiring/agent_ports.go）只做参数转换与报价组合，
// **不复制任何业务规则**：结算晚于校验、轮次随调用次数变、子项带 groupItemId、资产带 scriptAssetId、
// 余额不足停掉后续项……都由脚本 / 整图 / 时间线域自己保证，agent 一条都不重写。
// ⚠️ 报价与真实扣费同一个口径：没有只读报价入口的（分镜、资产、组执行）由适配器按
// 「真实受理那一刻会用的同一组参数」逐项问目录，不按模型报的数、不按写死的单价。

// ---------------------------------------------------------------- 脚本

// ScriptCreateInput 建脚本（按画布节点幂等：节点上已有脚本时返回那一个）。
type ScriptCreateInput struct {
	WorkspaceID  int64
	ProjectID    int64
	CanvasNodeID string
	// SourceType ∈ script | character | manual。
	SourceType  string
	RawInput    string
	GlobalStyle string
}

// ScriptShot 是脚本里的一个镜头（只带编排要用的字段）。
type ScriptShot struct {
	ID                int64  `json:"id,string"`
	No                int    `json:"shotNo"`
	DurationSec       int    `json:"durationSec,omitempty"`
	Summary           string `json:"summary,omitempty"`
	ImagePromptStatus string `json:"imagePromptStatus,omitempty"`
	VideoPromptStatus string `json:"videoPromptStatus,omitempty"`
}

// ScriptView 是脚本的编排视图。
type ScriptView struct {
	ID           int64
	ProjectID    int64
	CanvasNodeID string
	SourceType   string
	Status       string
	CurrentStep  int
	Version      int
	Shots        []ScriptShot
}

// StoryboardRequest 是一次分镜生成（同步、计费在 script_llm_call）。
type StoryboardRequest struct {
	WorkspaceID    int64
	PayerAccountID int64
	UserID         int64
	ScriptID       int64
	MemberLevelID  int64
	ModelCode      string
	ShotCount      int
	ExtraContext   string
}

// StoryboardOutcome 是分镜结果。CallID 是这次同步调用（账本 ref_type=script_llm 的 refId）。
type StoryboardOutcome struct {
	Script  ScriptView
	CallID  int64
	Credits int
}

// AssetBrief 是一个资产的摘要。
type AssetBrief struct {
	ID     int64  `json:"id,string"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Status string `json:"status"`
}

// AssetsRequest 是资产对账 + 批量出图的入参。AssetIDs 为空 = 本脚本全部未就绪的资产。
type AssetsRequest struct {
	WorkspaceID    int64
	PayerAccountID int64
	UserID         int64
	ScriptID       int64
	MemberLevelID  int64
	ModelCode      string
	AssetIDs       []int64
	Params         map[string]any
}

// AssetsQuote 是出图前的报价：先对账（免费、幂等），再按「待出图资产数 × asset_gen 单价」报。
type AssetsQuote struct {
	Pending []AssetBrief
	// Unit 一张资产图的价（与受理同一条 QuoteWithFacts）。
	Unit  int
	Total int
	// Unmatched 模型没给档案、退回了模板描述的实体（只报不拦）。
	Unmatched []string
}

// AssetItemOutcome 是批量出图的一项。
type AssetItemOutcome struct {
	AssetID      int64
	Name         string
	GenerationID int64
	Skipped      bool
	Reason       string
}

// AssetsOutcome 是批量出图结果（允许部分成功；余额不足停掉后续项）。
type AssetsOutcome struct {
	Items         []AssetItemOutcome
	GenerationIDs []int64
}

// ComposeRequest 是提示词合成。Source ∈ ai | template。
type ComposeRequest struct {
	WorkspaceID    int64
	PayerAccountID int64
	UserID         int64
	ScriptID       int64
	ShotIDs        []int64
	Kinds          []string
	Source         string
	ModelCode      string
	Force          bool
}

// ComposeQuote 是合成报价（脚本域 DryRun 的原样投影）。
type ComposeQuote struct {
	AICount       int
	TemplateCount int
	SkipCount     int
	Unit          int
	Total         int
}

// ComposeItemOutcome 是合成的一项。
type ComposeItemOutcome struct {
	ShotID  int64
	ShotNo  int
	Kind    string
	Status  string
	Skipped bool
	Reason  string
}

// ComposeOutcome 是合成结果。CallIDs 是这次合成产生的同步调用（逐条在账本上，ref_type=script_llm）。
type ComposeOutcome struct {
	Quote   ComposeQuote
	Items   []ComposeItemOutcome
	CallIDs []int64
}

// GroupCreateRequest 建组。GroupType ∈ asset | storyboard_image | video_generator。
type GroupCreateRequest struct {
	WorkspaceID    int64
	PayerAccountID int64
	UserID         int64
	ScriptID       int64
	CanvasNodeID   string
	GroupType      string
	Name           string
	ShotIDs        []int64
	ModelCode      string
	Operation      string
	DurationSec    int
	Durations      map[int64]int
	Params         map[string]any
}

// GroupExecuteRequest 执行组（或指定子项）。
type GroupExecuteRequest struct {
	WorkspaceID    int64
	PayerAccountID int64
	UserID         int64
	GroupID        int64
	ModelCode      string
	Operation      string
	DurationSec    int
	Force          bool
	ItemIDs        []int64
}

// GroupItemQuote 是组里一项的报价（与真实受理同一组参数逐项问目录）。
type GroupItemQuote struct {
	ItemID      int64  `json:"itemId,string,omitempty"`
	ShotID      int64  `json:"shotId,string,omitempty"`
	ShotNo      int    `json:"shotNo,omitempty"`
	Operation   string `json:"operation"`
	DurationSec int    `json:"durationSec,omitempty"`
	Inputs      int    `json:"inputs"`
	Credits     int    `json:"credits"`
}

// GroupQuote 是组的报价：Items 是会提交的项，Skipped 是不会提交的项与原因（镜号 + 原因，不静默）。
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

// Scripts 是脚本线端口（实现：wiring.AgentScripts → script/app.Service）。
type Scripts interface {
	CreateScript(ctx context.Context, in ScriptCreateInput) (ScriptView, error)
	Script(ctx context.Context, ws, scriptID int64) (ScriptView, error)
	QuoteStoryboard(ctx context.Context, in StoryboardRequest) (int, error)
	Storyboard(ctx context.Context, in StoryboardRequest) (StoryboardOutcome, error)
	// QuoteAssets 先对账（ReconcileAssets，免费）再报价。
	QuoteAssets(ctx context.Context, in AssetsRequest) (AssetsQuote, error)
	GenerateAssets(ctx context.Context, in AssetsRequest) (AssetsOutcome, error)
	QuoteCompose(ctx context.Context, in ComposeRequest) (ComposeQuote, error)
	Compose(ctx context.Context, in ComposeRequest) (ComposeOutcome, error)
	// QuoteGroupCreate 只对分镜图组有意义（建组即提交）；视频组 / 资产组返回 0 元的预览。
	QuoteGroupCreate(ctx context.Context, in GroupCreateRequest) (GroupQuote, error)
	CreateGroup(ctx context.Context, in GroupCreateRequest) (GroupView, error)
	QuoteGroupExecute(ctx context.Context, in GroupExecuteRequest) (GroupQuote, error)
	ExecuteGroup(ctx context.Context, in GroupExecuteRequest) (GroupView, error)
	Group(ctx context.Context, ws, groupID int64) (GroupView, error)
}

// ---------------------------------------------------------------- 整图

// WorkflowPlanRequest 是整图运行前预览（只读、不扣费）。
type WorkflowPlanRequest struct {
	WorkspaceID   int64
	ProjectID     int64
	MemberLevelID int64
	TargetNodeIDs []string
	Force         bool
}

// WorkflowIssue 是计划里的一个问题（带涉及的节点）。
type WorkflowIssue struct {
	Message string   `json:"message"`
	Nodes   []string `json:"nodes,omitempty"`
}

// WorkflowBlocked 是一个跑不了的节点与原因。
type WorkflowBlocked struct {
	NodeID string `json:"nodeId"`
	Reason string `json:"reason"`
}

// WorkflowPlanView 是预览结果。
type WorkflowPlanView struct {
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

// WorkflowStartRequest 启动整图运行。
type WorkflowStartRequest struct {
	WorkflowPlanRequest
	PayerAccountID int64
	UserID         int64
}

// WorkflowRunView 是一次运行的状态。
type WorkflowRunView struct {
	ID           int64
	Status       string
	TotalCredits int
	Nodes        int
}

// Workflows 是整图端口（实现：wiring.AgentWorkflows → workflow/app.Service）。
//
// ⚠️ Start 没有运行级幂等键（子任务键是 wf:{run}:{node}:{attempt}），防重靠 agent 的步骤状态：
// 同一步骤的额度条目已挂着运行 ID 时，工具直接返回那次运行，不再 Start。
type Workflows interface {
	Plan(ctx context.Context, in WorkflowPlanRequest) (WorkflowPlanView, error)
	Start(ctx context.Context, in WorkflowStartRequest) (WorkflowRunView, error)
	Run(ctx context.Context, ws, runID int64) (WorkflowRunView, error)
}

// ---------------------------------------------------------------- 时间线

// TimelineEnsureRequest 取或建时间线：TimelineID 非 0 取那一条，否则按 ScriptID 建一条新的。
type TimelineEnsureRequest struct {
	WorkspaceID int64
	ProjectID   int64
	ScriptID    int64
	TimelineID  int64
	Name        string
	AspectRatio string
}

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

// AssembleGap 是装配缺口：镜号 + 原因（脚本域给的原因原样透出，NEW-09 不静默跳过）。
type AssembleGap struct {
	ShotNo int    `json:"shotNo"`
	Reason string `json:"reason"`
}

// AssembleRequest 按分镜装配。
type AssembleRequest struct {
	WorkspaceID int64
	TimelineID  int64
	Version     int
	Force       bool
	DryRun      bool
	// UserID / EditorKind 记进时间线的最后编辑者（agent / mcp，SC-F-166）：开着剪辑台的人看到「时间线被 Agent 更新」。
	UserID     int64
	EditorKind string
}

// AssembleOutcome 是装配结果（DryRun 时是计划）。
type AssembleOutcome struct {
	Summary  string
	Append   int
	Skipped  []int
	Gaps     []AssembleGap
	Timeline TimelineView
}

// RenderRequest 是渲染（DryRun = 预检 + 报价）。
type RenderRequest struct {
	WorkspaceID    int64
	PayerAccountID int64
	UserID         int64
	TimelineID     int64
	MemberLevelID  int64
	IdempotencyKey string
	DryRun         bool
}

// RenderIssue 是预检问题（ClipIdx 指出第几段，0 表示与片段无关）。
type RenderIssue struct {
	Code    string `json:"code"`
	Detail  string `json:"detail"`
	ClipIdx int    `json:"clipIdx,omitempty"`
}

// RenderPrecheck 是预检 + 报价。
type RenderPrecheck struct {
	Issues     []RenderIssue
	DurationMs int
	Credits    int
}

// RenderView 是渲染任务。
type RenderView struct {
	ID      int64
	Status  string
	Credits int
}

// Timelines 是时间线端口（实现：wiring.AgentTimelines → timeline/app.Service）。
type Timelines interface {
	Ensure(ctx context.Context, in TimelineEnsureRequest) (TimelineView, error)
	Assemble(ctx context.Context, in AssembleRequest) (AssembleOutcome, error)
	PrecheckRender(ctx context.Context, in RenderRequest) (RenderPrecheck, error)
	StartRender(ctx context.Context, in RenderRequest) (RenderView, error)
}

// ---------------------------------------------------------------- 3D 导演台（DRT-29）

// ErrDirectorSceneNotFound 场景不在本项目（别的项目粘贴来的、所在项目已清理）：调用方按「没有场景」处理。
var ErrDirectorSceneNotFound = errors.New("场景不在本项目")

// DirectorSceneRequest 为画布节点建场景：按（项目，节点）幂等，节点已有场景时返回那一份；
// SourceSceneID 非 0 时复制那一份（复制出来的导演台还没有自己的场景时）。
type DirectorSceneRequest struct {
	WorkspaceID   int64
	ProjectID     int64
	UserID        int64
	SessionID     int64
	Role          collab.Role
	CanvasNodeID  string
	SourceSceneID int64
}

// DirectorBuildRequest 是一次 AI 搭景（免费、按用户限流，DRT-19）。结果由服务端以乐观锁直接写入场景，来源记 agent。
type DirectorBuildRequest struct {
	WorkspaceID       int64
	ProjectID         int64
	UserID            int64
	SessionID         int64
	Role              collab.Role
	MemberLevelID     int64
	SceneID           int64
	Prompt            string
	ReferenceMediaIDs []int64
}

// DirectorBuildOutcome 是已写入场景的结果。Added / Updated / Removed 是场景里的对象 ID；
// Notice 是固定的截图提醒（服务端不能截图，DR-F-162）。
type DirectorBuildOutcome struct {
	SceneID  int64
	Version  int
	Added    []string
	Updated  []string
	Removed  []string
	Defaults []string
	Notice   string
}

// Directors 是 3D 导演台端口（实现：wiring.AgentDirectors → canvas/app 的场景 + director/app 的搭景）。
//
// ⚠️ 业务错误原样返回 *httpx.BizError：限流（42901 / 42902，details 带 retry_after_seconds）、
// 方案无法解析（42216）、不可用（42203，details.userFixable）、写入冲突（40902）—— 工具据此把原因交给模型转述。
type Directors interface {
	CreateScene(ctx context.Context, in DirectorSceneRequest) (int64, error)
	// SceneNode 取本项目里一份场景绑定的画布节点 ID；场景不在本项目时返回 ErrDirectorSceneNotFound。
	SceneNode(ctx context.Context, ws, projectID, sceneID int64, role collab.Role) (string, error)
	// BuildAndApply 搭景并写入（冲突时重读场景、只重做一次，仍冲突就返回冲突，不强行覆盖）。
	BuildAndApply(ctx context.Context, in DirectorBuildRequest) (DirectorBuildOutcome, error)
}

// ---------------------------------------------------------------- 智能剪辑（docs/agent/smartcut SCT-41）

// ErrSmartcutTimelineNotFound 时间线不在本项目、或没绑节点：调用方按「没有时间线」处理。
var ErrSmartcutTimelineNotFound = errors.New("时间线不在本项目")

// SmartcutRender 最近一次导出。
type SmartcutRender struct {
	ID            int64
	Status        string
	Progress      int
	Stage         string
	OutputMediaID int64
	Error         string
}

// SmartcutActiveCall 进行中的 AI 调用（AI 剪辑 / 追加指令 / 翻译）。
type SmartcutActiveCall struct {
	ID    int64
	Kind  string
	Stage string
}

// SmartcutInput 素材条的一项（编号按连线顺序，多产物的节点是 1-1、1-2……）。
type SmartcutInput struct {
	No           string
	Kind         string
	Status       string
	Title        string
	SourceNodeID string
	MediaID      string
	DurationMs   int
	Used         bool
	Reason       string
}

// SmartcutClip 片段清单的一行：哪条轨、第几段（每条轨从 1 起）、什么素材、来源节点、成片起点与时长。
type SmartcutClip struct {
	TrackKind    string
	TrackName    string
	TrackRole    string
	No           int
	MediaID      int64
	Origin       string
	SourceNodeID string
	StartMs      int
	DurationMs   int
	Hidden       bool
}

// SmartcutSummary 时间线摘要（版本、段数、时长、剪过与否、过期）。
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
	ActiveCall   *SmartcutActiveCall
}

// SmartcutDetail 是 smartcut_get 的只读详情（SC-F-161）。
type SmartcutDetail struct {
	SmartcutSummary
	Inputs  []SmartcutInput
	Omitted int
	Clips   []SmartcutClip
}

// SmartcutSyncOutcome 是一次「连线即入轨」：铺上了几段、还有几项素材没用上；之后的摘要。
type SmartcutSyncOutcome struct {
	Appended int
	Unused   int
	Summary  SmartcutSummary
}

// SmartcutPlanRequest 一次 AI 剪辑（免费、按用户限流，与网页共用额度，SCK-2）。结果直接写入并存快照，来源记 agent。
type SmartcutPlanRequest struct {
	WorkspaceID   int64
	ProjectID     int64
	TimelineID    int64
	UserID        int64
	SessionID     int64
	MemberLevelID int64
	Mode          string
	Instruction   string
	TargetSec     int
	AspectRatio   string
	Variants      int
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

// SmartcutPlanOutcome 已写入的结果。AppliedVariant=-1 表示没有写入任何一套。
type SmartcutPlanOutcome struct {
	CallID         int64
	AppliedVariant int
	Variants       []SmartcutVariant
	SnapshotID     int64
	Summary        SmartcutSummary
}

// Smartcuts 是智能剪辑端口（实现：wiring.AgentSmartcuts → smartcut/app）。
//
// ⚠️ 业务错误原样返回 *httpx.BizError：限流（42901 / 42902，details 带 retry_after_seconds）、模式与素材不匹配
// （42217，details 带 reason 与出路）、没有可用模型（42218）、写入冲突（40902）—— 工具据此把原因交给模型转述。
type Smartcuts interface {
	// CreateTimeline 为画布节点建时间线（按（项目，节点）幂等；sourceTimelineID 非 0 时复制那一条）。
	CreateTimeline(ctx context.Context, ws, projectID int64, nodeID string, sourceTimelineID int64) (int64, error)
	// TimelineNode 本项目里一条时间线绑定的节点；不在本项目或没绑节点时返回 ErrSmartcutTimelineNotFound。
	TimelineNode(ctx context.Context, ws, projectID, timelineID int64) (string, error)
	// BindTimeline 把一条还没绑节点的时间线绑到节点上（timeline_assemble 的脚本时间线，SCD-39）。
	BindTimeline(ctx context.Context, ws, projectID, timelineID int64, nodeID string) error
	// SetAspectRatio 改一条还没有片段的新时间线的画幅（按节点拼时给的画幅；尺寸取画幅的缺省值）。
	// 不算「剪过」：之后的连线即入轨照常铺。
	SetAspectRatio(ctx context.Context, ws, projectID, timelineID int64, aspect string) error
	// Sync 连线即入轨（autofill）：还没剪过时把就绪、没用到的素材按编号铺上轨道；剪过的只更新素材条。
	Sync(ctx context.Context, ws, projectID, timelineID int64) (SmartcutSyncOutcome, error)
	// Detail 只读详情（smartcut_get）。
	Detail(ctx context.Context, ws, projectID, timelineID int64) (SmartcutDetail, error)
	// Plan AI 剪辑并直接写入（写入冲突时重读、按同一套方案重新写入一次，仍冲突就返回冲突）。
	Plan(ctx context.Context, in SmartcutPlanRequest) (SmartcutPlanOutcome, error)
}

// ---------------------------------------------------------------- 按引用查终态（扫描循环）

// BudgetRef 是额度条目挂的业务对象（agent_budget_entry.ref_type / ref_id / ref_ids）。
type BudgetRef struct {
	Type string
	ID   int64
	IDs  []int64
	// PayerAccountID 对账本时按付费主体卡一道（同 TransactionsByRefs）。
	PayerAccountID int64
}

// RefResult 是一个业务对象的当前结局与账本事实。
type RefResult struct {
	// Found=false：对象不存在或不属于这个工作区（按失败处理，不会永远等它）。
	Found bool
	// Terminal：对象（以及这次调用受理的全部子任务）都已结束。
	Terminal bool
	// Status ∈ success | partial_success | failed | cancelled | running。
	Status string
	// Summary 一句话结局（进唤醒说明，让模型知道成败与缺口）。
	Summary string
	// Error 失败原因（Status=failed / cancelled 时）。
	Error string
	// Consumed / Refunded **只认账本**（credit_transaction），不认业务表上的 credits_cost（报价）。
	Consumed int
	Refunded int
	// Settled：已终态，且失败 / 取消的部分在账本上都已退款（或本来就没扣）——
	// 账还没退就先归还额度的话，Agent 能在钱没回来时再花一次。
	Settled bool
	// LedgerIDs 这个对象在账本上对应的全部子任务 ID（整图运行的子任务要到运行跑起来才知道全）；
	// 非空时结算会写回条目的 ref_ids，供每轮对账取流水。
	LedgerIDs []int64
}

// RefStatuses 按业务引用查终态（实现：wiring.AgentRefStatuses）。
//
// ⚠️ ref_type=generation 不走这里（第一期的 TaskStatuses 路径），两条路径按 ref_type 互斥。
type RefStatuses interface {
	RefStatus(ctx context.Context, ws int64, ref BudgetRef) (RefResult, error)
}

// LedgerRefsOf 把一个额度条目的业务引用换成账本流水的引用（每轮对账 TurnLedger 用）。
//
// ⚠️ 与账本的 ref_type 逐字一致：生成任务 generation、同步文本调用 script_llm、渲染 render。
// 整图运行在结算前不知道全部子任务（ref_ids 为空）——那时这一轮的对账里暂时没有它。
func LedgerRefsOf(refType string, refID int64, refIDs []int64) []LedgerRef {
	ids := refIDs
	if len(ids) == 0 && refID != 0 {
		ids = []int64{refID}
	}
	var typ string
	switch refType {
	case "generation", "asset_batch", "script_group", "workflow_run":
		typ = RefTypeGeneration
		if refType == "workflow_run" && len(refIDs) == 0 {
			return nil
		}
	case "script_llm_call", "script_compose":
		typ = LedgerRefScriptLLM
	case "render_job":
		typ = LedgerRefRender
	default:
		return nil
	}
	out := make([]LedgerRef, 0, len(ids))
	for _, id := range ids {
		out = append(out, LedgerRef{Type: typ, ID: idStr(id)})
	}
	return out
}

// 账本上的 ref_type（与脚本域 / 时间线域写流水时用的逐字一致）。
const (
	LedgerRefScriptLLM = "script_llm"
	LedgerRefRender    = "render"
)
