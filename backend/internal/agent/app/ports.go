// Package app 是画布 Agent 的编排层：API 侧服务（service.go，AGT-26）与 Worker 侧
// 轮次运行器（runner.go / loop.go，AGT-22）。
//
// ⚠️ 本包只做编排（dev/01 T-1）：生成、报价、账本、画布、文档、成员资格一律经下面的
// **端口**调用，由 `internal/wiring` 里的适配器实现 —— 谁定义端口谁拥有契约。
// 本包**不 import 任何其他域的 repo / app 包**，只认它们的领域类型（collab.Role）。
//
// ⚠️ 依赖方向是 `tools → app`（dev/01 §2）：工具接口 Tool / RunContext / ToolResult /
// Suspension 定义在这里，`agent/tools` 的注册表与各工具实现 import 本包；反过来 import
// 会成环。Runner 只认 ToolSet 端口，`tools.Registry` 实现它。
package app

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
)

// ---------------------------------------------------------------- 推理模型

// ChatMessage 是发给推理模型的一条历史消息（与 provider/protocol.ChatMessage 同形，
// 由 wiring 适配器转换 —— agent 不 import provider 的类型）。
type ChatMessage struct {
	// Role：system 不出现在这里（系统提示词走 ChatInput.System）；user / assistant / tool。
	Role    string
	Content string
	// ToolCalls 只在 assistant 消息上：模型上一次发起的工具调用，回填历史时原样带回。
	ToolCalls []ToolCall
	// ToolCallID 只在 tool 消息上：这条结果回答的是哪一次调用。
	ToolCallID string
	// Images 只在 user 消息上（URL 或 data: 形态）。
	Images []string
}

// ToolDef 是交给模型的工具定义。Parameters 是 JSON Schema。
type ToolDef struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// ToolCall 是模型发起的一次工具调用。
//
// ⚠️ Arguments 只在参数是合法 JSON 时才有值；不合法时为 nil、原文在 RawArguments ——
// Runner 把「参数格式错」作为工具错误结果回给模型让它改，不让本轮失败。
type ToolCall struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Arguments    json.RawMessage `json:"arguments,omitempty"`
	RawArguments string          `json:"rawArguments,omitempty"`
}

// ChatInput 是一次推理调用。
type ChatInput struct {
	ModelCode string
	System    string
	Messages  []ChatMessage
	// Tools 是**本次**下发的工具（AllowedTools 的结果）。规划态这里绝不含付费与画布写工具
	// —— 测试断言的就是这个入参（AG-05）。
	Tools   []ToolDef
	Options map[string]any
	// MaxOutputTokens 单次输出上限的缺省值。只有必须带上限的协议（Anthropic）使用；0 = 协议缺省。
	MaxOutputTokens int
}

// ChatOutput 是一次推理调用的完整结果。
type ChatOutput struct {
	Text         string
	ToolCalls    []ToolCall
	FinishReason string
	InputTokens  int
	OutputTokens int
}

// ChatModel 是推理模型：多轮 + 工具 + 流式（实现：provider/app.HTTPProvider.Chat）。
//
// ⚠️ 超时 / 中途断开**不重发**（dev/01 §7.2）：Runner 拿到错误就按结算规则处理本轮，
// 不自己重试 —— 上游可能已经计费。
type ChatModel interface {
	Chat(ctx context.Context, in ChatInput, onDelta func(string)) (ChatOutput, error)
}

// ---------------------------------------------------------------- 报价与账本

// Quote 是一次报价。
type Quote struct {
	Credits int
	ModelID int64
}

// QuoteInput 是生成类报价的入参（付费工具用，AGT-24 补全字段）。
type QuoteInput struct {
	ModelCode     string
	OperationCode string
	Params        json.RawMessage
	// RoleCount：按输入角色计数（image / video / audio / text…），供能力矩阵判定。
	RoleCount     map[string]int
	Text          string
	MemberLevelID int64
	// Count / DurationSec：张数与秒数（per_image / per_second 计价），0 按 1 张 / 默认时长。
	Count       int
	DurationSec int
	// WorkspaceID / ProjectID / NodeID / DeclaredInputs（AGT-24）：给了输入就由适配器按
	// **受理同一条解析**数出角色计数再报价 —— 模型报的数不可信，而不按真实输入数的话
	// 能力矩阵（img2video 恰好 1 图）会在报价时就把合法的调用拒掉。
	WorkspaceID    int64
	ProjectID      int64
	NodeID         string
	DeclaredInputs json.RawMessage
}

// Quoter 是报价端口：适配 catalog/app.Service.QuoteWithFacts。
type Quoter interface {
	QuoteGeneration(ctx context.Context, in QuoteInput) (Quote, error)
	// QuoteTurn 报一轮对话的价（operation=agent_chat），复用会员等级与可用性合取式。
	QuoteTurn(ctx context.Context, modelCode string, memberLevelID int64) (Quote, error)
}

// 账本端口的哨兵错误：由 wiring 适配器从 billing 的错误翻译过来，Runner 按它分流。
var (
	// ErrInsufficientCredits 余额不足（Consume）。
	ErrInsufficientCredits = errors.New("积分不足")
	// ErrLedgerDuplicate 同一 (refType, refID) 已有该类型流水：
	// Consume 返回它 = 这一轮之前已经扣过（消息重投），视为已扣、继续。
	ErrLedgerDuplicate = errors.New("同一业务引用已有该类型流水")
)

// Ledger 是账本端口，与脚本域同形（refType 自由字符串）。
//
// ⚠️ Refund 在找不到原消费流水时返回错误（账本的 ErrNotFound）—— **0 积分的轮不能调用**
// （domain.TurnSettle.CallRefund 已经把这条收进判定）。重复退款由适配器吞掉（返回 nil）。
type Ledger interface {
	Consume(ctx context.Context, payerID, userID int64, amount int, refType, refID, reason string) error
	Refund(ctx context.Context, payerID int64, refType, refID, reason string) error
}

// MemberLevels 取付费主体当前会员等级（Worker 没有 *http.Request，用 billing 的同一份实现）。
// 取不到返回 0（按免费档算），不报错。
type MemberLevels interface {
	MemberLevelID(ctx context.Context, payerID int64) int64
}

// ---------------------------------------------------------------- 权限、事件、提示词

// Members 判断会话发起人当前在工作区的角色。ok=false 表示**已不是成员**（被移出 / 工作区归档）。
type Members interface {
	Role(ctx context.Context, ws, userID int64) (collab.Role, bool, error)
}

// Events 发到通用事件总线（dev/01 §8）。payload 由实现序列化成 JSON。
//
// ⚠️ 事件只是对账信号（GO-26），发布失败只告警，不影响本轮。
type Events interface {
	Publish(ctx context.Context, topic string, payload any) error
}

// PromptSource 渲染技能主说明（prompt_template 机制）。
//
// ⚠️ 只暴露 Render，不暴露取原文：取原文直接发出去的话，模型收到的是字面量 `{{var}}`
// 而不报错（CLAUDE.md §5.7）。变量要给全。
type PromptSource interface {
	RenderPrompt(ctx context.Context, code, modelCode string, vars map[string]string) (string, error)
}

// ---------------------------------------------------------------- 生成、画布、文档（AGT-23/24 接入）

// GenerateInput 是付费生成工具的受理入参（AGT-24 补全字段）。
type GenerateInput struct {
	WorkspaceID    int64
	ProjectID      int64
	NodeID         string
	UserID         int64
	PayerAccountID int64
	MemberLevelID  int64
	ModelCode      string
	OperationCode  string
	Params         json.RawMessage
	Inputs         json.RawMessage
	IdempotencyKey string
	// Count / DurationSec / Text（AGT-24）：与 generation/app.AcceptInput 同义。
	Count       int
	DurationSec int
	Text        string
}

// GenerateResult 是受理结果。
type GenerateResult struct {
	GenerationID int64
	Credits      int
	// Status 受理后的任务状态（通常 queued；幂等重放时是那条任务的当前状态）。
	Status string
	// Replayed 命中同一幂等键的已有任务，本次没有新扣费。
	Replayed bool
}

// TaskStatus 是生成任务的当前状态。
type TaskStatus struct {
	Status   string
	Terminal bool
}

// Generator 适配 generation/app.Service（受理 / 查状态 / 取消）。
type Generator interface {
	Accept(ctx context.Context, in GenerateInput) (GenerateResult, error)
	Status(ctx context.Context, ws, generationID int64) (TaskStatus, error)
	Cancel(ctx context.Context, ws, generationID int64, reason string) error
}

// CanvasNodeSummary 是画布摘要里的一个节点：**不含 params 全文**（dev/01 §4.4）。
type CanvasNodeSummary struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Title   string `json:"title,omitempty"`
	Status  string `json:"status,omitempty"`
	ByAgent bool   `json:"byAgent,omitempty"`
}

// CanvasView 是读画布的结果。
type CanvasView struct {
	Version    int
	CanvasData json.RawMessage
	Nodes      []CanvasNodeSummary
	EdgeCount  int
}

// ApplyOpsInput 是按操作写画布的入参（canvas/app.ApplyOps 的投影，AGT-23 补全）。
type ApplyOpsInput struct {
	WorkspaceID    int64
	ProjectID      int64
	UserID         int64
	Role           collab.Role
	AgentSessionID int64
	// BatchID 是这一批的幂等键（同一批重复提交只写一次）：用「会话 + 步骤 + attempt」，不带时间戳。
	BatchID string
	// Ops 是 canvas/domain.Op 数组的 JSON（agent 不 import canvas 的类型）。
	Ops json.RawMessage
}

// ApplyOpsResult 是按操作写画布的结果。
type ApplyOpsResult struct {
	FromVersion int
	ToVersion   int
	// NodeIDs 本批新建节点（含画框）的真实 ID，按创建顺序。
	NodeIDs []string
	// Replayed 这一批此前已经应用过，本次没有写入。
	Replayed bool
}

// Canvas 读与按操作写画布（实现在 canvas/app，经 wiring 适配）。
type Canvas interface {
	Read(ctx context.Context, ws, projectID int64) (CanvasView, error)
	Apply(ctx context.Context, in ApplyOpsInput) (ApplyOpsResult, error)
}

// NodeParams 给出画布某类节点绑定的参数编码（`node_param` → `param_def.code`，实现在 catalog，经 wiring 适配）。
//
// generate 工具据此把模型给的参数名归一成画布的编码（`aspectRatio` → `aspect_ratio`）：
// 节点 params 的键就是参数面板读的键，对不上的话面板读不到、回落默认值，
// 用户看到的规格与实际生成的不一致，从节点重跑还会生成错误规格。
type NodeParams interface {
	NodeParamCodes(ctx context.Context, nodeType string) ([]string, error)
}

// Document 是项目文档（project_document 归 canvas 域，AGT-09）。
type Document struct {
	ID      int64
	Path    string
	Content string
	Version int
}

// PutDocumentInput 是写文档的入参。Version=0 表示新建。
type PutDocumentInput struct {
	WorkspaceID    int64
	ProjectID      int64
	UserID         int64
	Role           collab.Role
	AgentSessionID int64
	Path           string
	Content        string
	Version        int
}

// ErrDocumentNotFound 文档不存在（Documents.Get）。上下文组装时视为「没有项目记忆」跳过。
var ErrDocumentNotFound = errors.New("文档不存在")

// Documents 读写项目文档（带 version 乐观锁）。
type Documents interface {
	Get(ctx context.Context, ws, projectID int64, role collab.Role, path string) (Document, error)
	Put(ctx context.Context, in PutDocumentInput) (Document, error)
}

// ---------------------------------------------------------------- 工具

// RunContext 是一次工具执行的上下文（dev/01 §5.1）。
type RunContext struct {
	WorkspaceID    int64
	ProjectID      int64
	SessionID      int64
	TurnID         int64
	StepID         int64
	Attempt        int
	UserID         int64
	Role           collab.Role
	PayerAccountID int64
	MemberLevelID  int64
	State          domain.State
	ModelCode      string
	// Scene 会话场景（MKT-T-22：generate 据此进营销分支）。空 = 调用方没给，工具自己读会话。
	Scene domain.Scene
	// Spec 视频规格的生效值（偏好 + 问答卡里确认过的回答，Runner 每轮算一次）：
	// generate 给新建节点补模型没给的画幅 / 清晰度；营销会话的 submit_plan 据此要求先确认、并核对时长。
	Spec VideoSpecState
}

// Suspension 是工具要求的挂起：Runner 按 Event 迁移会话状态（CAS）后结束本轮（T-4）。
//
// 卡片由工具自己写（ask_user / submit_plan / 追加额度）；这里只带卡片 ID 用于推送。
type Suspension struct {
	Event  domain.Event
	CardID int64
	Reason string
}

// ToolResult 是工具执行结果。
type ToolResult struct {
	// Content 回给模型的内容（精简 JSON）。
	Content json.RawMessage
	// Display 给前端步骤行展示的内容。
	Display json.RawMessage
	// Suspend 非空则挂起本轮。
	Suspend *Suspension
	// Outputs 有效产出计数（轮次结算用；成功步骤至少算 1）。
	Outputs int
	// GenerationID 非 0：步骤已受理一个生成任务，Runner 把步骤标 waiting 而不是 success。
	GenerationID int64
	// WaitRef（AGT-50）：步骤等的不是单个生成任务，而是这一步额度条目上挂的对象
	// （组 / 整图运行 / 渲染 / 一批资产图）。Runner 同样把步骤标 waiting（generation_id 留空），
	// 扫描循环按条目的 ref_type / ref_id 查终态并唤醒。
	WaitRef bool
	// AttachImages（MKT-T-19，view_media）：要给推理模型看的图（data: 内联地址）。Runner 只在**下一次**
	// 模型调用里、紧跟这一批工具结果之后附一条临时 user 消息带上它们 —— 不落 agent_message。
	AttachImages []string
	// PlanItemKey（MKT-T-22）：这一步对应的营销计划项键，Runner 写进 agent_step.plan_item_key。
	// 失败时的键与错误码经 *StepError 带出。
	PlanItemKey string
}

// Tool 是一个 Agent 工具（dev/01 §5.1）。白名单与付费 / 画布写属性的**唯一定义**在
// domain.toolSpecs；这里的 Paid / Action 只是实现自报，Runner 以 domain 为准。
type Tool interface {
	Name() domain.ToolName
	Description() string
	// Schema 是发给模型的参数 JSON Schema。
	Schema() json.RawMessage
	Paid() bool
	Action() collab.Action
	Run(ctx context.Context, rc RunContext, args json.RawMessage) (ToolResult, error)
}

// ToolSet 是 Runner 认的工具集合（tools.Registry 实现）。
type ToolSet interface {
	Lookup(name domain.ToolName) (Tool, bool)
	// Schemas 按 names 的顺序输出已注册工具的定义；未注册的名字跳过。
	Schemas(names []domain.ToolName) []ToolDef
}
