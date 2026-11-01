package wiring

// 「画布上生成」共享编排（internal/canvasgen，MCPT-08）的 wiring 级适配器：直接接 canvas / catalog /
// generation / billing 的服务，供 MCP（MCPT-10）用 Principal 组 canvasgen.Caller 调用。
//
// Agent 侧**不走这里**：它仍由 NewAgentPaidTools 装配，agent/tools 在文件内把 agent 的端口包成
// canvasgen 端口（额度外壳 PaidCallTool 是 Agent 独有的）。两侧共用的是编排本身与报价口径
// （quoteWithResolvedInputs），以及同一条受理（generation/app.Service.Accept）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	canvasapp "github.com/li904338203/tabtv/backend/internal/canvas/app"
	canvasdomain "github.com/li904338203/tabtv/backend/internal/canvas/domain"
	"github.com/li904338203/tabtv/backend/internal/canvasgen"
	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// CanvasGenInput 是 NewCanvasGen 的入参。
type CanvasGenInput struct {
	// Canvas 写画布 + 报价时的输入解析。⚠️ 要与受理侧（Gen 的输入解析）读同一个库。
	Canvas  *canvasapp.Service
	Catalog *catalogapp.Service
	Gen     *genapp.Service
	// Billing 余额不足时取当前余额（好让调用方说出「还差多少」）。可为 nil。
	Billing *billingrepo.Repo
}

// NewCanvasGen 装出共享编排。Logger 由调用方按需填（为 nil 时用 slog.Default）。
func NewCanvasGen(in CanvasGenInput) *canvasgen.Orchestrator {
	return &canvasgen.Orchestrator{
		Canvas:   canvasGenCanvas{in.Canvas},
		Quoter:   canvasGenQuoter{Catalog: in.Catalog, Canvas: in.Canvas},
		Accepter: canvasGenAccepter{Gen: in.Gen, Ledger: in.Billing},
		Params:   AgentNodeParams{in.Catalog}, // 与 Agent 同一份参数编码来源（Config 合成快照）
		Outputs:  CatalogOperationOutputs{in.Catalog},
	}
}

// CatalogOperationOutputs 按目录的 operation.out_node_type 给出操作的产出节点类型
// （canvasgen.OperationOutputs；Agent 与 MCP 的 generate 推导新建节点类型共用）。
// 操作表只有十几行，直接取目录列表，不为它单开查询。
type CatalogOperationOutputs struct{ Catalog *catalogapp.Service }

func (a CatalogOperationOutputs) OperationOutNodeType(ctx context.Context, operationCode string) (string, error) {
	ops, err := a.Catalog.ListOperations(ctx)
	if err != nil {
		return "", err
	}
	for _, o := range ops {
		if o.Code == operationCode {
			return o.OutNodeType, nil
		}
	}
	return "", nil
}

// canvasGenCanvas 读 / 写画布。节点摘要与 Agent 读画布同一个实现（SummarizeCanvas），不另解析一份。
type canvasGenCanvas struct{ Svc *canvasapp.Service }

func (a canvasGenCanvas) Read(ctx context.Context, ws, projectID int64) (canvasgen.CanvasView, error) {
	p, err := a.Svc.Get(ctx, ws, projectID, collab.RoleViewer)
	if err != nil {
		return canvasgen.CanvasView{}, err
	}
	nodes, _ := agentapp.SummarizeCanvas(p.CanvasData)
	params := canvasgen.ParamsByNode(p.CanvasData)
	out := make([]canvasgen.CanvasNode, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, canvasgen.CanvasNode{ID: n.ID, Type: n.Type, Params: params[n.ID]})
	}
	return canvasgen.CanvasView{Nodes: out, Edges: canvasgen.EdgesOf(p.CanvasData)}, nil
}

func (a canvasGenCanvas) Apply(ctx context.Context, in canvasgen.ApplyInput) (canvasgen.ApplyResult, error) {
	var ops []canvasdomain.Op
	if err := json.Unmarshal(in.Ops, &ops); err != nil {
		return canvasgen.ApplyResult{}, httpx.Errf(errcode.InvalidArgument, "画布操作不是合法的数组: "+err.Error())
	}
	var actor canvasdomain.Actor
	switch in.Origin.Kind {
	case canvasgen.OriginAgent:
		actor = canvasdomain.Actor{Kind: canvasdomain.ActorAgent, UserID: in.UserID, SessionID: in.Origin.SessionID}
	case canvasgen.OriginMCP:
		// MCPT-09：MCP 授权写画布。授权 ID 缺失由 canvas/domain.Apply 拒绝（INVALID_ARGUMENT）。
		actor = canvasdomain.Actor{Kind: canvasdomain.ActorMCP, UserID: in.UserID,
			GrantID: in.Origin.GrantID, ClientName: in.Origin.ClientName}
	default:
		return canvasgen.ApplyResult{}, fmt.Errorf("画布写入来源 %q 尚未接入", in.Origin.Kind)
	}
	r, err := a.Svc.ApplyOps(ctx, canvasapp.ApplyOpsInput{WorkspaceID: in.WorkspaceID, ProjectID: in.ProjectID,
		Actor: actor, BatchID: in.BatchID, Ops: ops})
	if errors.Is(err, canvasdomain.ErrNotOwned) {
		return canvasgen.ApplyResult{}, errors.Join(canvasgen.ErrNodeNotOwned, err)
	}
	if err != nil {
		return canvasgen.ApplyResult{}, err
	}
	return canvasgen.ApplyResult{FromVersion: r.FromVersion, ToVersion: r.ToVersion, NodeIDs: r.NodeIDs,
		Replayed: r.Replayed}, nil
}

// canvasGenQuoter 与 AgentGenQuoter 同一个报价口径（quoteWithResolvedInputs）。
type canvasGenQuoter struct {
	Catalog *catalogapp.Service
	Canvas  *canvasapp.Service
}

func (a canvasGenQuoter) QuoteGeneration(ctx context.Context, in canvasgen.QuoteInput) (canvasgen.Quote, error) {
	d, err := quoteWithResolvedInputs(ctx, a.Catalog, a.Canvas, genQuoteArgs{
		ModelCode: in.ModelCode, OperationCode: in.OperationCode,
		Text: in.Text, MemberLevelID: in.MemberLevelID, Count: in.Count, DurationSec: in.DurationSec,
		WorkspaceID: in.WorkspaceID, ProjectID: in.ProjectID, NodeID: in.NodeID, DeclaredInputs: in.DeclaredInputs,
	})
	if err != nil {
		return canvasgen.Quote{}, err
	}
	return canvasgen.Quote{Credits: d.Credits, ModelID: d.ModelID}, nil
}

// canvasGenAccepter 走 AgentGenerator 同一段受理（同一条 genapp.Accept、同一套余额不足翻译），
// 只把类型换成 canvasgen 的。
type canvasGenAccepter struct {
	Gen    *genapp.Service
	Ledger *billingrepo.Repo
}

func (a canvasGenAccepter) Accept(ctx context.Context, in canvasgen.AcceptInput) (canvasgen.AcceptResult, error) {
	r, err := AgentGenerator{Gen: a.Gen, Ledger: a.Ledger}.Accept(ctx, agentapp.GenerateInput{
		WorkspaceID: in.WorkspaceID, ProjectID: in.ProjectID, NodeID: in.NodeID, UserID: in.UserID,
		PayerAccountID: in.PayerAccountID, MemberLevelID: in.MemberLevelID,
		ModelCode: in.ModelCode, OperationCode: in.OperationCode, Params: in.Params,
		Inputs: in.Inputs, IdempotencyKey: in.IdempotencyKey, Count: in.Count, DurationSec: in.DurationSec,
		Text: in.Text,
	})
	if err != nil {
		var ins *agentapp.InsufficientCreditsError
		if errors.As(err, &ins) {
			return canvasgen.AcceptResult{}, &canvasgen.InsufficientCreditsError{Balance: ins.Balance, Cause: ins.Cause}
		}
		return canvasgen.AcceptResult{}, err
	}
	return canvasgen.AcceptResult{GenerationID: r.GenerationID, Credits: r.Credits, Status: r.Status,
		Replayed: r.Replayed}, nil
}

var (
	_ canvasgen.Canvas     = canvasGenCanvas{}
	_ canvasgen.Quoter     = canvasGenQuoter{}
	_ canvasgen.Accepter   = canvasGenAccepter{}
	_ canvasgen.NodeParams = AgentNodeParams{}
	// 新建节点补默认值（与参数面板同一份 defaultValue）：Agent 与 MCP 的 generate 都靠它。
	_ canvasgen.NodeParamDefaulter = AgentNodeParams{}
)
