package app

import (
	"context"
	"time"

	"github.com/li904338203/tabtv/backend/internal/canvasgen"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/mcp/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
)

// 本文件是 MCP 对其他域的端口（开发 01 §4）。实现在 internal/wiring/mcp.go：
// MCP 不 import 其他域的 repo，也不自己查成员表 / 用户表 —— 成员资格与账号状态的判定
// 各自只有一份（collaboration / identity），这里只是调用方。

// Membership 是用户在某个工作区的**当前**成员资格。
type Membership struct {
	Role           collab.Role
	PayerAccountID int64
}

// Members 实时查成员资格（MT-4、MCP-F-11）。不是 active 成员（或工作区不存在 / 已归档）时
// 返回 NOT_WORKSPACE_MEMBER（协作域本就不区分这几种，区分等于给出工作区枚举接口）。
type Members interface {
	Resolve(ctx context.Context, userID, workspaceID int64) (Membership, error)
}

// Users 查账号是否可用（封禁 → false）。账号不存在返回 UNAUTHENTICATED。
// ⚠️ 每次调用都要过这一步：不过的话封禁后他的密钥照样能用（GAP-19 同类）。
type Users interface {
	Active(ctx context.Context, userID int64) (bool, error)
}

// MemberLevels 取付费主体的会员等级（billing/app.Service 直接满足）。取不到降级为 0，不报错。
// ⚠️ 等级只供工具里的模型门槛用，**不限制 MCP 本身**（MCP-F-15：任何等级都能用）。
type MemberLevels interface {
	MemberLevelID(ctx context.Context, payerID int64) int64
}

// Auditor 写平台审计（platform/audit.Logger 直接满足），失败只告警。
type Auditor interface {
	Write(ctx context.Context, e audit.Entry)
}

// Store 是 MCP 自己的表（mcp/repo.Repo 实现）。
type Store interface {
	CreateAccessKey(ctx context.Context, in repo.NewAccessKey) (repo.AccessKey, error)
	FindByHash(ctx context.Context, hash []byte) (repo.AccessKey, error)
	ListKeys(ctx context.Context, userID int64) ([]repo.AccessKey, error)
	GetKey(ctx context.Context, userID, keyID int64) (repo.AccessKey, error)
	ListGrants(ctx context.Context, userID int64) ([]repo.Grant, error)
	RevokeGrant(ctx context.Context, userID, grantID int64) (repo.Grant, error)
	TouchLastUsed(ctx context.Context, grantID int64) (bool, error)
	// FindToken 按哈希取 OAuth 令牌（MCPT-12）。
	FindToken(ctx context.Context, hash []byte) (repo.Token, error)
	// SetGrantPageConfirm 切换授权的「花钱需在 seeAitv 页面确认」（MCPT-13），返回改前与改后。
	SetGrantPageConfirm(ctx context.Context, userID, grantID int64, on bool) (repo.Grant, repo.Grant, error)
}

// ---------------------------------------------------------------- generate（MCPT-10）

// GenPrepared 是准备好的一次「画布上生成」：canvasgen 的归一结果 + 服务端解析出的输入角色计数。
// 角色计数进参数指纹（CanonicalGenerateItem.inputs），所以要在报价之前、由解析声明的输入 ID 得出 ——
// 与受理侧能力矩阵用的是同一份解析（canvas ResolveForAccept），不信客户端报的数。
type GenPrepared struct {
	canvasgen.Prepared
	RoleCounts map[string]int
}

// AcceptedTask 是按幂等键找回的已受理任务（重放用）。
type AcceptedTask struct {
	GenerationID int64
	ProjectID    int64
	NodeID       string
	Status       string
	Credits      int
}

// CanvasGeneration 是「画布上生成」的分步端口（开发 01 §12 R1：Agent 与 MCP 共用 internal/canvasgen；
// 实现 wiring.MCPCanvasGen 把 *canvasgen.Orchestrator 包一层，补上 MCP 需要的预检与按幂等键找回）。
//
// 顺序由调用方保证且不可换：Prepare → Quote（可多次）→ EnsureNode → Accept → Link。
// 确认服务插在 Quote 与 EnsureNode 之间 —— 用户没确认之前不建节点（拒绝了不留空节点）。
type CanvasGeneration interface {
	// NewNodeType 定下新建节点的类型（不给 nodeId 时）：没给 nodeType 按目录的 operation.out_node_type 推导，
	// 给了要与之一致（canvasgen.ResolveNewNodeType，与站内 Agent 同一个推导）。参数错误是 *canvasgen.NodeTypeError。
	NewNodeType(ctx context.Context, operationCode, declared string) (string, error)
	// Prepare 归一参数，并预检：项目在本工作区（否则 NOT_FOUND）、nodeId 存在且归本授权所有
	// （否则 canvasgen.ErrNodeNotOwned）、声明的输入能解析（否则 INVALID_ARGUMENT）。不写任何东西。
	Prepare(ctx context.Context, c canvasgen.Caller, req canvasgen.Request) (GenPrepared, error)
	// Quote 报价（与受理同一条 QuoteWithFacts）。
	Quote(ctx context.Context, c canvasgen.Caller, p GenPrepared) (int, error)
	// EnsureNode 新建节点或在本授权的节点上更新参数（批次 c.BatchID，同批次重放返回同一节点）。
	EnsureNode(ctx context.Context, c canvasgen.Caller, p GenPrepared) (string, error)
	// Accept 受理（扣费在受理自己的事务里）；余额不足返回 *canvasgen.InsufficientCreditsError。
	Accept(ctx context.Context, c canvasgen.Caller, p GenPrepared, nodeID, idemKey string) (canvasgen.AcceptResult, error)
	// Link 把任务挂到节点（批次 canvasgen.LinkBatchID(c.BatchID)）；失败只告警。
	Link(ctx context.Context, c canvasgen.Caller, nodeID string, generationID int64)
	// FindAccepted 按（付费主体, 幂等键）找已受理的任务；没有时 found=false。
	FindAccepted(ctx context.Context, payerID int64, idemKey string) (task AcceptedTask, found bool, err error)
}

// ---------------------------------------------------------------- 委托站内 Agent（MCPT-17）

// AgentSessions 是委托站内 Agent 的端口（MCP-F-80）。实现 wiring.MCPAgentSessions 调 agent/app.Service 的
// CreateSession / PostMessage / SubmitCard / Get / EstimateTurn —— **不直接查改 agent 的表**，状态机、
// 发起人校验、规划确认的重新报价、额度、计费全部在站内 Agent 里。
//
// ⚠️ 收的是 Principal：工作区 / 付费主体 / 操作者 / 当前角色 / 会员等级一一换成 agent 的 SessionActor。
// 会话的发起人就是凭据用户本人（created_by），画布写入的来源仍是站内 Agent（它写的），MCP 只是发起者。
// ⚠️ 端口里**没有「批准」这种方法**：批准规划 / 追加额度只是 SubmitCard 的一种提交，能不能提交由
// tools/agent.go 在调用前过确认服务（MCP-F-82）。
type AgentSessions interface {
	Start(ctx context.Context, p Principal, in AgentStartInput) (AgentWrite, error)
	Send(ctx context.Context, p Principal, sessionID int64, in AgentSendInput) (AgentWrite, error)
	Get(ctx context.Context, p Principal, sessionID int64) (AgentSession, error)
	SubmitCard(ctx context.Context, p Principal, sessionID, cardID int64, submission []byte) (AgentWrite, error)
	// EstimateTurn 估一轮对话费（agent_chat，站内运行器同一条报价路）；sessionID 为 0 时按用户偏好估。
	EstimateTurn(ctx context.Context, p Principal, sessionID int64) (AgentTurnEstimate, error)
}

// AgentStartInput 是 agent.start。ProjectID 与 CreateProjectName 二选一。
type AgentStartInput struct {
	ProjectID         int64
	CreateProjectName *string
	Text              string
	SkillCode         string
	IdempotencyKey    string
}

// AgentSendInput 是 agent.send。
type AgentSendInput struct {
	Text           string
	SkillCode      string
	IdempotencyKey string
}

// AgentWrite 是写操作（建会话 / 发消息 / 提交卡片）的结果。
type AgentWrite struct {
	SessionID  int64
	ProjectID  int64
	State      string
	TurnID     int64
	CardID     int64
	CardStatus string
	// Queued：会话正在运行，这条消息排队为下一轮；Replayed：同一个幂等键已处理过。
	Queued   bool
	Replayed bool
}

// AgentTurnEstimate 是一轮对话费的预估。
type AgentTurnEstimate struct {
	ModelCode string
	Credits   int
}

// AgentCard 是一张卡片（payload 原样，由 domain.StructureAgentCard 结构化）。
type AgentCard struct {
	ID        int64
	Kind      string
	Status    string
	Payload   []byte
	CreatedAt time.Time
}

// AgentTurn 是一轮（计费单位，refType=agent_turn）。
type AgentTurn struct {
	ID           int64
	Trigger      string
	Status       string
	ModelCode    string
	Credits      int
	ErrorCode    string
	ErrorMessage string
}

// AgentMessage 是会话里的一条消息（只含用户 / Agent / 系统说明，不含工具调用原文）。
type AgentMessage struct {
	ID        int64
	Role      string
	Content   string
	CardID    int64
	CreatedAt time.Time
}

// AgentSession 是 agent.get 的会话快照。
type AgentSession struct {
	ID              int64
	ProjectID       int64
	Title           string
	State           string
	CreatedByUserID int64
	InitiatorName   string
	Todo            []byte
	// 额度（agent_budget_entry 汇总，与站内会话页同一口径）。
	Approved     int
	Used         int
	Refunded     int
	PendingCards []AgentCard
	LatestTurn   *AgentTurn
	Messages     []AgentMessage
}
