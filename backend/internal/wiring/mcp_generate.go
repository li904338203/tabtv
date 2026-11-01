package wiring

// 对外 MCP generate（MCPT-10）的画布生成端口：把共享编排（internal/canvasgen，与站内 Agent 同一份）
// 包一层，补上 MCP 需要、而 Agent 不需要的两件事 ——
//
//   - 预检（Prepare）：项目在本工作区、nodeId 存在且归本授权所有、声明的输入解析得开。都在**报价与确认之前**，
//     否则用户确认完才发现「节点不是你的」，白点一次确认；
//   - 按幂等键找回已受理的任务（FindAccepted）：已用的 quote 重放时直接拿回原任务，不再建节点、不再受理。
//
// ⚠️ 所有权由画布域判（canvas/domain.OwnedByActor，与 ApplyOps 写入时同一个函数），这里只是提前问一次；
// 真正的闸门仍是 EnsureNode 写画布时的那次校验。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	canvasapp "github.com/li904338203/tabtv/backend/internal/canvas/app"
	canvasdomain "github.com/li904338203/tabtv/backend/internal/canvas/domain"
	"github.com/li904338203/tabtv/backend/internal/canvasgen"
	genrepo "github.com/li904338203/tabtv/backend/internal/generation/repo"
	mcpapp "github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// MCPCanvasGen 实现 mcpapp.CanvasGeneration。
type MCPCanvasGen struct {
	Orch    *canvasgen.Orchestrator
	Canvas  *canvasapp.Service
	GenRepo *genrepo.Repo
}

func (a MCPCanvasGen) NewNodeType(ctx context.Context, operationCode, declared string) (string, error) {
	return a.Orch.NewNodeType(ctx, operationCode, declared)
}

func (a MCPCanvasGen) Prepare(ctx context.Context, c canvasgen.Caller, req canvasgen.Request) (mcpapp.GenPrepared, error) {
	proj, err := a.Canvas.Get(ctx, c.WorkspaceID, c.ProjectID, c.Role)
	if err != nil {
		return mcpapp.GenPrepared{}, err
	}
	if req.NodeID != "" {
		node, ok := findCanvasNode(proj.CanvasData, req.NodeID)
		if !ok {
			return mcpapp.GenPrepared{}, httpx.Errf(errcode.NotFound, "节点 "+req.NodeID+" 不在这个项目的画布上")
		}
		actor := canvasdomain.Actor{Kind: canvasdomain.ActorMCP, UserID: c.UserID, GrantID: c.Origin.GrantID}
		if !canvasdomain.OwnedByActor(node, actor) {
			return mcpapp.GenPrepared{}, canvasgen.ErrNodeNotOwned
		}
	}
	prep := a.Orch.Prepare(ctx, c, req)
	out := mcpapp.GenPrepared{Prepared: prep}
	if len(prep.Inputs) > 0 {
		// 与受理侧同一条解析（ResolveForAccept）：角色数按服务端解析结果数，不信客户端报的数。
		res, err := a.Canvas.ResolveForAccept(ctx, c.WorkspaceID, c.ProjectID, prep.NodeID, prep.OperationCode,
			prep.Inputs)
		if err != nil {
			return mcpapp.GenPrepared{}, err
		}
		if len(res.Issues) > 0 {
			return mcpapp.GenPrepared{}, httpx.Errf(errcode.InvalidArgument, res.Issues[0]).
				WithDetails(map[string]any{"issues": res.Issues})
		}
		out.RoleCounts = res.RoleCounts
	}
	return out, nil
}

func (a MCPCanvasGen) Quote(ctx context.Context, c canvasgen.Caller, p mcpapp.GenPrepared) (int, error) {
	q, err := a.Orch.Quote(ctx, c, p.Prepared, p.NodeID)
	if err != nil {
		return 0, err
	}
	return q.Credits, nil
}

func (a MCPCanvasGen) EnsureNode(ctx context.Context, c canvasgen.Caller, p mcpapp.GenPrepared) (string, error) {
	return a.Orch.EnsureNode(ctx, c, p.Prepared)
}

func (a MCPCanvasGen) Accept(ctx context.Context, c canvasgen.Caller, p mcpapp.GenPrepared, nodeID,
	idemKey string) (canvasgen.AcceptResult, error) {
	return a.Orch.Accept(ctx, c, p.Prepared, nodeID, idemKey)
}

func (a MCPCanvasGen) Link(ctx context.Context, c canvasgen.Caller, nodeID string, generationID int64) {
	a.Orch.Link(ctx, c, nodeID, generationID)
}

func (a MCPCanvasGen) FindAccepted(ctx context.Context, payerID int64, idemKey string) (mcpapp.AcceptedTask, bool, error) {
	g, err := a.GenRepo.FindByIdempotencyKey(ctx, payerID, idemKey)
	if errors.Is(err, genrepo.ErrNotFound) {
		return mcpapp.AcceptedTask{}, false, nil
	}
	if err != nil {
		return mcpapp.AcceptedTask{}, false, err
	}
	return mcpapp.AcceptedTask{GenerationID: g.ID, ProjectID: g.ProjectID, NodeID: g.NodeID, Status: g.Status,
		Credits: g.CreditsCost}, true, nil
}

// findCanvasNode 在画布原文里找节点（只认顶层 nodes[].id；解不开按「找不到」）。
func findCanvasNode(raw json.RawMessage, id string) (map[string]any, bool) {
	var doc struct {
		Nodes []map[string]any `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, false
	}
	for _, n := range doc.Nodes {
		if nid, _ := n["id"].(string); strings.TrimSpace(nid) == id {
			return n, true
		}
	}
	return nil, false
}

var _ mcpapp.CanvasGeneration = MCPCanvasGen{}
