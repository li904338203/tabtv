// Package canvasgen 是「画布上生成」的共享编排（MCPT-08，dev/01 §1 MT-5、§12 R1）：
// 参数归一 → 建 / 改节点并落盘 → 报价 → 受理 → link_generation。
//
// 自 agent/tools/generate.go **原样**下沉，Agent 与 MCP 共用同一份 —— 这条链上踩过两个坑
// （AGT-24 缺陷修复 2026-10-16：受理后不挂 link_generation → 节点永远空；参数键不是画布编码 →
// 面板回落默认值），另写一份必然再踩一次。
//
// ⚠️ 本包只依赖标准库与 collaboration/domain：**不 import agent / mcp / 任何域的 repo、transport**
// （imports_test.go 绊线）。引用 agent 的类型的话，MCP 用它就会把整个 agent 域拖进来。
//
// ⚠️ 分步暴露（Prepare → EnsureNode → Quote → Accept → Link）而不是一个整体 Submit：
// MCP 要在报价与受理之间插入花钱确认，Agent 要在受理外面包额度外壳（PaidCallTool，不下沉）。
// 顺序由调用方保证，且**不可换**：
//   - 节点先落盘再报价 / 受理 —— 受理侧的输入解析按 (project, node) 查画布，节点还没落盘就受理的话，
//     生成任务挂在一个画布上不存在的节点上，前端永远对不上这条任务；
//   - 受理成功后必须 Link —— 不挂的话画布无从知道节点上有任务，节点一直显示成空的。
//
// ⚠️ 错误文案是调用方的事：本包把端口错误**原样**返回（Agent 把它翻成给模型的话，MCP 翻成给客户端的错误码）。
package canvasgen

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"

	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
)

// ErrNoNodeID 新建节点后画布没有返回节点 ID（文案与搬家前一致）。
var ErrNoNodeID = errors.New("画布没有返回新建节点的 ID")

// 端口适配器翻译出来的哨兵（wiring.NewCanvasGen 用；Agent 侧的适配器透传 agent 自己的哨兵）。
var (
	// ErrNodeNotOwned 节点不是该来源创建、或用户之后改过（canvas ErrNotOwned 的翻译）。
	ErrNodeNotOwned = errors.New("该节点由用户创建或已被用户修改")
	// ErrInsufficientCredits 受理时余额不足。
	ErrInsufficientCredits = errors.New("积分不足")
)

// InsufficientCreditsError 受理时余额不足。Balance<0 表示没取到余额。
// errors.Is(err, ErrInsufficientCredits) 成立。
type InsufficientCreditsError struct {
	Balance int
	Cause   error
}

func (e *InsufficientCreditsError) Error() string {
	if e.Cause != nil {
		return "积分不足: " + e.Cause.Error()
	}
	return "积分不足"
}

// Is 让 errors.Is(err, ErrInsufficientCredits) 成立。
func (e *InsufficientCreditsError) Is(target error) bool { return target == ErrInsufficientCredits }

// Unwrap 保留原始错误（业务错误码）。
func (e *InsufficientCreditsError) Unwrap() error { return e.Cause }

// Origin 是写画布的来源（canvas_op_log 的操作者）：agent 带 SessionID，mcp 带 GrantID / ClientName（MCPT-09）。
type Origin struct {
	Kind      string
	SessionID int64
	// GrantID MCP 授权 ID（所有权按它判）；ClientName 只是展示用（写进节点 origin.clientName）。
	GrantID    int64
	ClientName string
}

// 来源类型（与 canvas/domain.ActorAgent / ActorMCP 同值；本包不依赖 canvas 域）。
const (
	OriginAgent = "agent"
	OriginMCP   = "mcp"
)

// Caller 是一次「画布上生成」的调用方（中性：Agent 用 RunContext 组、MCP 用 Principal 组）。
type Caller struct {
	WorkspaceID    int64
	ProjectID      int64
	UserID         int64
	Role           collab.Role
	PayerAccountID int64
	MemberLevelID  int64
	Origin         Origin
	// BatchID 建 / 改节点那一批的幂等键（调用方给，带 attempt、不带时间戳）。
	// link_generation 那一批由本包派生为 LinkBatchID(BatchID)。
	// ⚠️ canvas_op_log.batch_id 是 varchar(64)：派生后的长度要在调用方的单测里钉住。
	BatchID string
	// StampNode 只在 add_node 时调用，给调用方往新节点上写附加元数据（Agent 写 agent.stepId）。
	// 入参是整条 add_node 操作（节点在 op["node"]）。可为 nil。
	StampNode func(op map[string]any)
	// LogAttrs 是告警日志里调用方的定位字段（Agent：session / step），排在本包字段之前。可为 nil。
	LogAttrs []any
}

// LinkBatchID 派生 link_generation 那一批的幂等键：`{batchID}:link`。
//
// ⚠️ 必须与建节点那一批不同：同一次调用里已经用 batchID 写过一次画布，
// 复用它会命中幂等、直接返回那一批的结果 —— 什么都没写，而且不报错。
func LinkBatchID(batchID string) string { return batchID + ":link" }

// ---------------------------------------------------------------- 端口

// CanvasNode 是读画布时用到的节点信息（只要 ID 与类型）。
type CanvasNode struct {
	ID   string
	Type string
	// Params 节点上当前的参数（canvasData 里的 params；读不到为 nil）。在已有节点上生成时作为受理参数的底：
	// 与前端「从节点生成」发的是同一份（ParamPanel 发的是节点的全部 params）。
	Params map[string]any
}

// CanvasEdge 是读画布时用到的连线（只要两端与 order）。
type CanvasEdge struct {
	Source string
	Target string
	Order  int
}

// CanvasView 是读画布的结果。
type CanvasView struct {
	Nodes []CanvasNode
	// Edges 画布上的连线（EdgesOf 解析；替用户连线时判「已经连过」用）。读不到为 nil。
	Edges []CanvasEdge
}

// ApplyInput 是按操作写画布的入参（canvas/app.ApplyOps 的投影）。
type ApplyInput struct {
	WorkspaceID int64
	ProjectID   int64
	UserID      int64
	Role        collab.Role
	Origin      Origin
	BatchID     string
	// Ops 是 canvas/domain.Op 数组的 JSON（本包不 import canvas 的类型）。
	Ops json.RawMessage
}

// ApplyResult 是按操作写画布的结果。
type ApplyResult struct {
	FromVersion int
	ToVersion   int
	// NodeIDs 本批新建节点的真实 ID，按创建顺序。
	NodeIDs  []string
	Replayed bool
}

// Canvas 读与按操作写画布。
type Canvas interface {
	Read(ctx context.Context, ws, projectID int64) (CanvasView, error)
	Apply(ctx context.Context, in ApplyInput) (ApplyResult, error)
}

// QuoteInput 是报价入参：给了 DeclaredInputs 时，适配器要按**受理同一条解析**数出角色计数再报价。
type QuoteInput struct {
	ModelCode      string
	OperationCode  string
	Params         json.RawMessage
	Text           string
	MemberLevelID  int64
	Count          int
	DurationSec    int
	WorkspaceID    int64
	ProjectID      int64
	NodeID         string
	DeclaredInputs json.RawMessage
}

// Quote 是报价结果。
type Quote struct {
	Credits int
	ModelID int64
}

// Quoter 是报价端口（与受理同一条 QuoteWithFacts）。
type Quoter interface {
	QuoteGeneration(ctx context.Context, in QuoteInput) (Quote, error)
}

// AcceptInput 是受理入参（与 generation/app.AcceptInput 同义）。
type AcceptInput struct {
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
	Count          int
	DurationSec    int
	Text           string
}

// AcceptResult 是受理结果。
type AcceptResult struct {
	GenerationID int64
	Credits      int
	Status       string
	// Replayed 命中同一幂等键的已有任务，本次没有新扣费。
	Replayed bool
}

// Accepter 是受理端口（generation/app.Service.Accept）。
type Accepter interface {
	Accept(ctx context.Context, in AcceptInput) (AcceptResult, error)
}

// NodeParams 给出画布某类节点绑定的参数编码（node_param → param_def.code）。
type NodeParams interface {
	NodeParamCodes(ctx context.Context, nodeType string) ([]string, error)
}

// NodeParamDefaulter（可选）给出画布某类节点各参数的默认值（参数编码 → 默认值，空的不列），
// 与 GET /canvas/config 的 defaultValue 同一份。NodeParams 的实现同时实现它时，新建节点补上
// 调用方没给的参数（FillNodeDefaults）；没实现时不补（行为与原先一致）。
type NodeParamDefaulter interface {
	NodeParamDefaults(ctx context.Context, nodeType string) (map[string]string, error)
}

// ---------------------------------------------------------------- 编排

// Position 是新建节点的位置。
type Position struct {
	X float64
	Y float64
}

// Request 是一次生成请求（已校验过的参数；Params 已经过 BuildNodeParams）。
type Request struct {
	// NodeID 已有节点；为空时新建 NodeType 类型的节点。
	NodeID        string
	NodeType      string
	Title         string
	OperationCode string
	ModelCode     string
	Prompt        string
	Params        map[string]any
	Inputs        json.RawMessage
	DurationSec   int
	Count         int
	Position      *Position
}

// Prepared 是 Prepare 的结果：归一后的参数与要在 merge patch 里删掉的别名键。
type Prepared struct {
	// Request 中的 Params / DurationSec / Count 已归一。
	Request
	// ResolvedNodeType 给 NodeID 时是画布上该节点的类型（读不到为空），否则同 Request.NodeType。
	ResolvedNodeType string
	// Dropped 改名掉的别名键（aspectRatio 之类）：改已有节点时要在 merge patch 里显式删掉，
	// 否则节点上同时留着两份画幅，以后谁读到哪份取决于读的人。
	Dropped []string
	// AcceptParams 归一后参数的 JSON：报价与受理用同一份。
	AcceptParams json.RawMessage
}

// Orchestrator 是「画布上生成」的编排。Canvas / Quoter / Accepter 必填；Params、Logger 可为 nil。
type Orchestrator struct {
	Canvas   Canvas
	Quoter   Quoter
	Accepter Accepter
	// Params 画布节点的参数编码（目录）；为 nil 或取不到时退回 seed 的副本。
	Params NodeParams
	// Outputs 操作的产出节点类型（目录），新建节点不给 nodeType 时据此推导（NewNodeType）。
	// 为 nil 时退回「nodeType 必须显式给」。
	Outputs OperationOutputs
	Logger  *slog.Logger
}

// Prepare 参数归一 + 计算要删的别名键（不写任何东西）。
func (o Orchestrator) Prepare(ctx context.Context, c Caller, req Request) Prepared {
	nodeType := req.NodeType
	var nodeParams map[string]any
	if req.NodeID != "" {
		nodeType, nodeParams = o.nodeOf(ctx, c, req.NodeID)
	}
	raw0 := req.Params
	p := Prepared{Request: req, ResolvedNodeType: nodeType}
	codes := o.paramCodes(ctx, nodeType)
	p.Params, p.DurationSec, p.Count = NormalizeGenerateParams(nodeType, req.OperationCode, req.Params,
		codes, req.DurationSec, req.Count)
	accept := p.Params
	if req.NodeID == "" {
		// 新建节点补默认值（与参数面板同一条规则）；已有节点上的值是用户设的，不补。
		p.Params, p.DurationSec, p.Count = FillNodeDefaults(p.Params, o.paramDefaults(ctx, nodeType),
			p.DurationSec, p.Count)
		accept = p.Params
	} else {
		// 已有节点：受理参数 = 节点上当前的参数 + 这次给的（merge patch 仍只写这次给的）。
		accept, p.DurationSec, p.Count = OverlayNodeParams(nodeParams, p.Params, canvasParamKeys(nodeType, codes),
			p.DurationSec, p.Count)
	}
	for k := range raw0 {
		if _, ok := p.Params[k]; !ok {
			p.Dropped = append(p.Dropped, k)
		}
	}
	p.AcceptParams, _ = json.Marshal(accept)
	return p
}

// EnsureNode 新建节点，或在已有节点上更新参数（后者顺带过一次所有权校验），返回已落盘的节点 ID。
// Apply 的错误原样返回（由调用方翻译）。
func (o Orchestrator) EnsureNode(ctx context.Context, c Caller, p Prepared) (string, error) {
	var op map[string]any
	if p.NodeID != "" {
		pp := make(map[string]any, len(p.Params)+len(p.Dropped))
		for k, v := range p.Params {
			pp[k] = v
		}
		for _, k := range p.Dropped {
			pp[k] = nil // merge patch：null 删键
		}
		patch := map[string]any{"params": pp}
		if strings.TrimSpace(p.Title) != "" {
			patch["title"] = p.Title
		}
		op = map[string]any{"kind": "update_node", "nodeId": p.NodeID, "patch": patch}
	} else {
		title := strings.TrimSpace(p.Title)
		if title == "" {
			title = p.NodeType
		}
		node := map[string]any{"type": p.NodeType, "title": title, "params": p.Params}
		op = map[string]any{"kind": "add_node", "node": node}
		if c.StampNode != nil {
			c.StampNode(op)
		}
		if p.Position != nil {
			op["position"] = map[string]any{"x": p.Position.X, "y": p.Position.Y}
		}
	}
	ops, _ := json.Marshal([]any{op})
	res, err := o.Canvas.Apply(ctx, ApplyInput{
		WorkspaceID: c.WorkspaceID, ProjectID: c.ProjectID, UserID: c.UserID, Role: c.Role,
		Origin: c.Origin, BatchID: c.BatchID, Ops: ops,
	})
	if err != nil {
		return "", err
	}
	if p.NodeID != "" {
		return p.NodeID, nil
	}
	if len(res.NodeIDs) == 0 {
		return "", ErrNoNodeID
	}
	return res.NodeIDs[0], nil
}

// Quote 报价（节点已落盘，输入解析能按节点找到它）。错误原样返回。
func (o Orchestrator) Quote(ctx context.Context, c Caller, p Prepared, nodeID string) (Quote, error) {
	return o.Quoter.QuoteGeneration(ctx, QuoteInput{
		ModelCode: p.ModelCode, OperationCode: p.OperationCode, Params: p.AcceptParams,
		Text: strings.TrimSpace(p.Prompt), MemberLevelID: c.MemberLevelID,
		Count: p.Count, DurationSec: p.DurationSec,
		WorkspaceID: c.WorkspaceID, ProjectID: c.ProjectID, NodeID: nodeID, DeclaredInputs: p.Inputs,
	})
}

// Accept 受理（扣费在受理自己的事务里）。idemKey 由调用方给（Agent：额度条目的幂等键）。错误原样返回。
func (o Orchestrator) Accept(ctx context.Context, c Caller, p Prepared, nodeID, idemKey string) (AcceptResult, error) {
	return o.Accepter.Accept(ctx, AcceptInput{
		WorkspaceID: c.WorkspaceID, ProjectID: c.ProjectID, NodeID: nodeID, UserID: c.UserID,
		PayerAccountID: c.PayerAccountID, MemberLevelID: c.MemberLevelID,
		ModelCode: p.ModelCode, OperationCode: p.OperationCode, Params: p.AcceptParams,
		Inputs: p.Inputs, IdempotencyKey: idemKey, Count: p.Count, DurationSec: p.DurationSec,
		Text: strings.TrimSpace(p.Prompt),
	})
}

// Link 把已受理的任务挂到节点 result 上（canvas 的 link_generation），批次 LinkBatchID(c.BatchID)。
//
// ⚠️ 失败只告警：任务已受理、钱已扣，回给调用方一个错误只会让它再生成一次（再扣一次钱）。
// 前端靠 generation 表兜底。
func (o Orchestrator) Link(ctx context.Context, c Caller, nodeID string, generationID int64) {
	genID := strconv.FormatInt(generationID, 10)
	if genID == "" || genID == "0" {
		return
	}
	ops, _ := json.Marshal([]any{map[string]any{"kind": "link_generation", "nodeId": nodeID,
		"patch": map[string]any{"generationId": genID}}})
	in := ApplyInput{
		WorkspaceID: c.WorkspaceID, ProjectID: c.ProjectID, UserID: c.UserID, Role: c.Role,
		Origin: c.Origin, BatchID: LinkBatchID(c.BatchID), Ops: ops,
	}
	if _, err := o.Canvas.Apply(ctx, in); err != nil {
		attrs := append(append([]any{}, c.LogAttrs...), "node", nodeID, "generation", genID, "err", err)
		o.logger().Warn("生成已受理，但挂到画布节点失败（前端靠 generation 表兜底）", attrs...)
	}
}

// nodeOf 读出已有节点的类型与当前参数（给 nodeId 时）；读不到返回空（归一按所有类型的并集、受理只用这次给的）。
func (o Orchestrator) nodeOf(ctx context.Context, c Caller, nodeID string) (string, map[string]any) {
	v, err := o.Canvas.Read(ctx, c.WorkspaceID, c.ProjectID)
	if err != nil {
		return "", nil
	}
	for _, n := range v.Nodes {
		if n.ID == nodeID {
			return n.Type, n.Params
		}
	}
	return "", nil
}

// paramCodes 取目录里该节点类型的参数编码；取不到返回 nil（退回副本）。
func (o Orchestrator) paramCodes(ctx context.Context, nodeType string) []string {
	if o.Params == nil || nodeType == "" {
		return nil
	}
	codes, err := o.Params.NodeParamCodes(ctx, nodeType)
	if err != nil {
		o.logger().Warn("取画布参数编码失败，退回内置副本", "nodeType", nodeType, "err", err)
		return nil
	}
	return codes
}

// paramDefaults 取目录里该节点类型的参数默认值；Params 没实现 NodeParamDefaulter 或取不到时返回 nil（不补）。
func (o Orchestrator) paramDefaults(ctx context.Context, nodeType string) map[string]string {
	d, ok := o.Params.(NodeParamDefaulter)
	if !ok || nodeType == "" {
		return nil
	}
	defs, err := d.NodeParamDefaults(ctx, nodeType)
	if err != nil {
		o.logger().Warn("取画布参数默认值失败，新建节点不补默认值", "nodeType", nodeType, "err", err)
		return nil
	}
	return defs
}

func (o Orchestrator) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return slog.Default()
}
