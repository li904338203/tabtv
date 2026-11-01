package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
)

// canvas_apply（AGT-24，dev/01 §5.3 / §6）：按操作写画布。
//
// ⚠️ 只在 executing（规格表 + Runner 二次校验 + 这里第三道）。
// ⚠️ 批次 ID = `agent:{session}:{step}:{attempt}`：同一次调用重放只写一次，
// 重试（attempt 变）是新的一批。不带时间戳。
// ⚠️ 所有权：改不是本会话建的、或用户之后动过的节点 → 回给模型「在旁边新建」，不重试。

// 画布操作种类（与 canvas/domain.Op* 逐字一致；agent 不 import canvas 的类型）。
var canvasOpKinds = map[string]bool{
	"add_node": true, "update_node": true, "move_node": true,
	"add_edge": true, "add_frame": true, "set_frame_members": true,
}

// maxCanvasOps 与 canvas/domain.MaxOpsPerBatch 一致（那边也会拦，这里先给模型一句看得懂的话）。
const maxCanvasOps = 200

// NotOwnedHint 是所有权冲突时回给模型的话。
const NotOwnedHint = "该节点由用户创建或已被用户修改，不能改它；请在旁边新建一个节点"

// CanvasApply 是 canvas_apply 工具。
type CanvasApply struct{ Canvas app.Canvas }

func (CanvasApply) Name() domain.ToolName { return domain.ToolCanvasApply }
func (CanvasApply) Paid() bool            { return false }
func (CanvasApply) Action() collab.Action { return collab.ActionEdit }

func (CanvasApply) Description() string {
	return "按操作修改当前画布：新建节点 / 改自己建的节点的标题与参数 / 移动 / 连线 / 画框。" +
		"新节点 ID 由服务端分配并在结果里返回；同批后续操作可用 ref:xxx 引用本批新建的节点。" +
		"只能修改你在本会话里新建且用户没动过的节点，其他节点请在旁边新建。不能删除节点。"
}

func (CanvasApply) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"ops":{"type":"array","minItems":1,"maxItems":200,` +
		`"items":{"type":"object","properties":{` +
		`"kind":{"type":"string","enum":["add_node","update_node","move_node","add_edge","add_frame","set_frame_members"]},` +
		`"nodeId":{"type":"string","description":"被操作的节点 ID，或本批的 ref:xxx"},` +
		`"ref":{"type":"string","description":"给本批新建的节点 / 画框起名，形如 ref:xxx"},` +
		`"node":{"type":"object","description":"add_node：{type, title, params}"},` +
		`"patch":{"type":"object","description":"update_node：只允许 title 与 params（params 按 merge patch 合并）"},` +
		`"edge":{"type":"object","description":"add_edge：{source, target, order}"},` +
		`"frame":{"type":"object","description":"add_frame：{title, position, size, members}；set_frame_members：{members}"},` +
		`"position":{"type":"object","properties":{"x":{"type":"number"},"y":{"type":"number"}}}` +
		`},"required":["kind"]}}},"required":["ops"]}`)
}

// ParseCanvasApplyArgs 校验参数并返回操作数组（原样 JSON，保留未知字段）。
func ParseCanvasApplyArgs(args json.RawMessage) ([]map[string]any, error) {
	var in struct {
		Ops []json.RawMessage `json:"ops"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("参数不是合法的对象：%v", err)
	}
	if len(in.Ops) == 0 {
		return nil, errors.New("ops 不能为空")
	}
	if len(in.Ops) > maxCanvasOps {
		return nil, fmt.Errorf("一次最多 %d 个操作，实际 %d 个", maxCanvasOps, len(in.Ops))
	}
	out := make([]map[string]any, 0, len(in.Ops))
	for i, raw := range in.Ops {
		op, err := decodeObject(raw)
		if err != nil {
			return nil, fmt.Errorf("第 %d 个操作不是对象", i+1)
		}
		kind, _ := op["kind"].(string)
		if !canvasOpKinds[kind] {
			return nil, fmt.Errorf("第 %d 个操作的 kind %q 不支持（可用：add_node / update_node / move_node / "+
				"add_edge / add_frame / set_frame_members；不能删除节点）", i+1, kind)
		}
		out = append(out, op)
	}
	return out, nil
}

func (t CanvasApply) Run(ctx context.Context, rc app.RunContext, args json.RawMessage) (app.ToolResult, error) {
	if rc.State != domain.StateExecuting {
		return app.ToolResult{}, fmt.Errorf("canvas_apply 只能在执行阶段使用（当前 %s）", rc.State)
	}
	ops, err := ParseCanvasApplyArgs(args)
	if err != nil {
		return app.ToolResult{}, err
	}
	for _, op := range ops {
		if op["kind"] == "add_node" {
			stampAgentStep(op, "node", rc.StepID)
		}
	}
	raw, err := json.Marshal(ops)
	if err != nil {
		return app.ToolResult{}, err
	}
	res, err := applyCanvas(ctx, t.Canvas, rc, raw)
	if err != nil {
		return app.ToolResult{}, err
	}
	content, _ := json.Marshal(map[string]any{"nodeIds": nonNil(res.NodeIDs), "version": res.ToVersion})
	display, _ := json.Marshal(map[string]any{"nodeIds": nonNil(res.NodeIDs), "ops": len(ops),
		"fromVersion": res.FromVersion, "toVersion": res.ToVersion})
	return app.ToolResult{Content: content, Display: display, Outputs: 1}, nil
}

// applyCanvas 调画布端口并把所有权冲突翻成给模型的话。canvas_apply 与 generate 共用。
func applyCanvas(ctx context.Context, c app.Canvas, rc app.RunContext, ops json.RawMessage) (app.ApplyOpsResult, error) {
	if c == nil {
		return app.ApplyOpsResult{}, errors.New("画布服务未接入")
	}
	res, err := c.Apply(ctx, app.ApplyOpsInput{
		WorkspaceID: rc.WorkspaceID, ProjectID: rc.ProjectID, UserID: rc.UserID, Role: rc.Role,
		AgentSessionID: rc.SessionID, BatchID: BatchID(rc), Ops: ops,
	})
	if errors.Is(err, app.ErrNodeNotOwned) {
		return app.ApplyOpsResult{}, errors.New(NotOwnedHint)
	}
	if err != nil {
		return app.ApplyOpsResult{}, fmt.Errorf("写画布失败：%w", err)
	}
	return res, nil
}

// BatchID 是一次工具调用写画布的批次幂等键：`agent:{session}:{step}:{attempt}`。
func BatchID(rc app.RunContext) string {
	a := rc.Attempt
	if a <= 0 {
		a = 1
	}
	return fmt.Sprintf("agent:%d:%d:%d", rc.SessionID, rc.StepID, a)
}

// stampAgentStep 在 add_node 的节点上写 `agent.stepId`（sessionId 由画布域按操作者强制写入）。
func stampAgentStep(op map[string]any, key string, stepID int64) {
	n, _ := op[key].(map[string]any)
	if n == nil {
		return
	}
	meta, _ := n["agent"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	meta["stepId"] = strconv.FormatInt(stepID, 10)
	n["agent"] = meta
}

// decodeObject 解出 JSON 对象；用 UseNumber，否则 params 里的雪花号会丢精度。
func decodeObject(raw json.RawMessage) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errors.New("不是对象")
	}
	return m, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

var _ app.Tool = CanvasApply{}
