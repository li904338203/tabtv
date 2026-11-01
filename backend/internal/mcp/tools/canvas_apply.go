package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// canvas_apply（MCPT-11，MCP-F-25 / 60 / 61 / 62）：按操作写画布。
//
// ⚠️ 走画布域的 ApplyOps（与站内 Agent 同一条路径：在最新版本上应用、canvas_op_log、canvas.ops_applied 事件），
// 打开着画布的浏览器实时重放、不弹冲突（MCP-F-60）。新节点的来源由画布域按操作者**强制写入**
// （origin{kind:mcp, grantId, clientName}），客户端传了也会被覆盖 —— 来源不能伪造。
// ⚠️ 所有权（MCP-F-62）由画布域判（canvas/domain.OwnedByActor，与 generate 预检同一个函数）：
// 改、移、往里接线的目标节点必须是**本授权**创建且用户之后没动过的；否则 MCP_NODE_NOT_OWNED。
// ⚠️ 操作白名单**不含** link_generation（只由 generate 受理后产生，否则客户端能把任意任务挂到节点上）
// 与 set_frame_members（画框成员由 add_frame 一次给齐）；画布域本来也没有删除操作（MCP-F-90）。
// ⚠️ 批次幂等靠 uk_canvas_op_batch(project_id, batch_id)：客户端重试要带**同一个** idempotencyKey，
// 否则会建出两份节点（不带时每次调用都是新的一批）。

// canvasApplyKinds 是允许的操作种类（与 canvas/domain.Op* 逐字一致；顺序即文档顺序）。
var canvasApplyKinds = []string{"add_node", "update_node", "add_edge", "add_frame", "move_node"}

// maxCanvasApplyOps 与 canvas/domain.MaxOpsPerBatch 一致（那边也会拦，这里先给一句看得懂的话）。
const maxCanvasApplyOps = 200

type canvasPointIn struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type canvasOpIn struct {
	Kind     string         `json:"kind" jsonschema:"add_node 新建节点 / update_node 改节点标题与参数 / add_edge 连线 / add_frame 画框 / move_node 移动"`
	NodeID   string         `json:"nodeId,omitempty" jsonschema:"update_node / move_node：节点 ID，或本批 add_node 起的 ref:xxx"`
	Ref      string         `json:"ref,omitempty" jsonschema:"add_node / add_frame：给新节点起名（ref:xxx），供同批后续操作引用"`
	Node     map[string]any `json:"node,omitempty" jsonschema:"add_node：{type, title, params}；type 是画布节点类型码（以 canvas_read 返回的节点为准）；建导演台请用 director_create（它会同时建好场景）；id 由服务端分配"`
	Patch    map[string]any `json:"patch,omitempty" jsonschema:"update_node：只允许 title 与 params（params 按 JSON merge patch 合并）"`
	Edge     map[string]any `json:"edge,omitempty" jsonschema:"add_edge：{source, target, order}；source / target 可以是 ref:xxx；order 决定首尾帧与参考图顺序，不给则排在最后"`
	Frame    map[string]any `json:"frame,omitempty" jsonschema:"add_frame：{title, position, size, members}"`
	Position *canvasPointIn `json:"position,omitempty" jsonschema:"add_node / add_frame / move_node：位置"`
}

type canvasApplyIn struct {
	ProjectID      string       `json:"projectId" jsonschema:"项目 ID"`
	Ops            []canvasOpIn `json:"ops" jsonschema:"操作（按顺序应用，最多 200 个；全部成功或全部不生效）"`
	IdempotencyKey string       `json:"idempotencyKey,omitempty" jsonschema:"幂等键：重试同一批操作时必须带同一个值，否则会写出两份；不带则每次调用都是新的一批"`
}

type canvasApplyOut struct {
	ProjectID   string   `json:"projectId"`
	NodeIDs     []string `json:"nodeIds" jsonschema:"本批新建的节点（含画框）ID，按创建顺序"`
	Version     int      `json:"version" jsonschema:"写入后的画布版本号"`
	FromVersion int      `json:"fromVersion"`
	Replayed    bool     `json:"replayed" jsonschema:"true：同一幂等键的这批操作之前已写过，本次没有任何写入，返回的是第一次的结果"`
}

const canvasApplyDescription = "按操作修改项目画布：add_node 新建节点 / update_node 改节点的标题与参数 / " +
	"add_edge 连线 / add_frame 画框 / move_node 移动。一批操作全部成功或全部不生效；新节点 ID 由服务端分配并在 nodeIds 里返回，" +
	"同批后续操作可用 ref:xxx 引用本批新建的节点。只能修改本凭据创建、且用户之后没改过的节点（连线的 target 也算修改），" +
	"其他节点请在旁边新建。不能删除节点；生成请用 generate（它会把任务挂到节点上）。" +
	"重试同一批操作时带同一个 idempotencyKey，避免写出两份。不花积分。"

func canvasApplyTool(d Deps) app.Tool {
	return app.Define(app.Tool{
		Name:        "canvas_apply",
		Toolset:     domain.ToolsetCanvas,
		Description: canvasApplyDescription,
		// 不破坏：只新建、改本授权自己建的节点，画布域没有删除操作。
		Methods: []app.Method{{Action: collab.ActionEdit, Write: true}},
	}, func(ctx context.Context, _ string, in canvasApplyIn) (canvasApplyOut, string, error) {
		p, err := principal(ctx)
		if err != nil {
			return canvasApplyOut{}, "", err
		}
		projectID, ops, err := canvasApplyArgs(in)
		if err != nil {
			return canvasApplyOut{}, "", err
		}
		if d.CanvasWriter == nil {
			return canvasApplyOut{}, "", unwired("CanvasWriter")
		}
		// 不带幂等键时用随机串：JSON-RPC 的请求 ID 每个会话都从 1 数起，拿它当批次会让两次无关的调用撞成「重放」。
		batch := domain.CanvasBatchID(p.GrantID, strings.TrimSpace(in.IdempotencyKey), randomSuffix())
		res, err := d.CanvasWriter.Apply(ctx, p, app.CanvasApplyInput{ProjectID: projectID, BatchID: batch, Ops: ops})
		if errors.Is(err, app.ErrNodeNotOwned) {
			return canvasApplyOut{}, "", httpx.Errf(errcode.McpNodeNotOwned,
				"该节点不是你创建的或已被用户修改，请在旁边新建（"+err.Error()+"）")
		}
		if err != nil {
			return canvasApplyOut{}, "", err
		}
		out := canvasApplyOut{ProjectID: idStr(projectID), NodeIDs: res.NodeIDs, Version: res.ToVersion,
			FromVersion: res.FromVersion, Replayed: res.Replayed}
		if out.NodeIDs == nil {
			out.NodeIDs = []string{}
		}
		if res.Replayed {
			return out, fmt.Sprintf("这批操作之前已写过（同一 idempotencyKey），本次没有写入；画布 v%d", res.ToVersion), nil
		}
		return out, fmt.Sprintf("已写入 %d 个操作，新建 %d 个节点；画布 v%d → v%d", len(in.Ops), len(out.NodeIDs),
			res.FromVersion, res.ToVersion), nil
	})
}

// canvasApplyArgs 校验入参并转成画布域的操作 JSON。种类白名单在这里先挡一道（说清能用哪些），
// 画布域还会再校验一次结构与所有权。
func canvasApplyArgs(in canvasApplyIn) (int64, json.RawMessage, error) {
	projectID, err := parseID("projectId", in.ProjectID)
	if err != nil {
		return 0, nil, err
	}
	switch {
	case len(in.Ops) == 0:
		return 0, nil, httpx.Errf(errcode.InvalidArgument, "ops 不能为空")
	case len(in.Ops) > maxCanvasApplyOps:
		return 0, nil, httpx.Errf(errcode.InvalidArgument,
			fmt.Sprintf("一次最多 %d 个操作（本次 %d 个），请分批", maxCanvasApplyOps, len(in.Ops)))
	case len(in.IdempotencyKey) > 200:
		return 0, nil, httpx.Errf(errcode.InvalidArgument, "idempotencyKey 最多 200 字符")
	}
	ops := make([]map[string]any, 0, len(in.Ops))
	for i, op := range in.Ops {
		if !containsStr(canvasApplyKinds, op.Kind) {
			hint := ""
			switch op.Kind {
			case "link_generation":
				hint = "：任务挂到节点由 generate 完成"
			case "set_frame_members":
				hint = "：画框成员请在 add_frame 时一次给齐"
			}
			return 0, nil, httpx.Errf(errcode.InvalidArgument, fmt.Sprintf("第 %d 个操作的 kind %q 不支持%s（可用：%s；不能删除节点）",
				i+1, op.Kind, hint, strings.Join(canvasApplyKinds, " / ")))
		}
		m := map[string]any{"kind": op.Kind}
		put := func(k string, v any, ok bool) {
			if ok {
				m[k] = v
			}
		}
		put("nodeId", op.NodeID, op.NodeID != "")
		put("ref", op.Ref, op.Ref != "")
		put("node", op.Node, op.Node != nil)
		put("patch", op.Patch, op.Patch != nil)
		put("edge", op.Edge, op.Edge != nil)
		put("frame", op.Frame, op.Frame != nil)
		if op.Position != nil {
			m["position"] = map[string]any{"x": op.Position.X, "y": op.Position.Y}
		}
		ops = append(ops, m)
	}
	raw, err := json.Marshal(ops)
	if err != nil {
		return 0, nil, httpx.Wrap(errcode.Internal, err)
	}
	return projectID, raw, nil
}
