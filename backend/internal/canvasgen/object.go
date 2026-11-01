package canvasgen

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
)

// 「建承载业务对象的节点 → 建业务对象 → 把对象 ID 写回节点」（MCPT-16，dev/01 §12 R2）：
// 脚本节点（script）与组节点（group）都是这个形状。自 agent/tools/multi_common.go 的
// addAgentNode / syncNodeParams / scriptNodeParams / groupNodeParams **原样**下沉，Agent 与 MCP 共用 ——
// 与 MT-5 同一类坑：业务对象建好了却不写回节点参数，前端按 params.scriptId / params.groupId 取对象，
// 节点在画布上一直是空的（AGT-24 缺陷修复）。另写一份必然再踩一次。
//
// ⚠️ 写回用**独立批次**（`{BatchID}:{suffix}`）：复用建节点那一批会命中幂等、什么都不写，而且不报错。
// ⚠️ 写回失败只告警：对象已经建好（钱可能已经扣了），报错只会让调用方再做一遍。
// ⚠️ 节点不是本来源建的、或用户改过（所有权规则拒绝）时同样只告警：那是用户的节点，前端自己会刷新。

// ObjectNode 是承载业务对象的新节点。
type ObjectNode struct {
	NodeType string
	Title    string
	Params   map[string]any
	Position *Position
}

// AddNode 新建一个节点（批次 c.BatchID；c.StampNode 给调用方写附加元数据），返回落盘后的节点 ID。
// 画布的错误原样返回（由调用方翻译）；画布没返回 ID 时返回 ErrNoNodeID。
func (o Orchestrator) AddNode(ctx context.Context, c Caller, n ObjectNode) (string, error) {
	node := map[string]any{"type": n.NodeType, "title": n.Title, "params": n.Params}
	op := map[string]any{"kind": "add_node", "node": node}
	if c.StampNode != nil {
		c.StampNode(op)
	}
	if n.Position != nil {
		op["position"] = map[string]any{"x": n.Position.X, "y": n.Position.Y}
	}
	ops, _ := json.Marshal([]any{op})
	res, err := o.Canvas.Apply(ctx, ApplyInput{
		WorkspaceID: c.WorkspaceID, ProjectID: c.ProjectID, UserID: c.UserID, Role: c.Role,
		Origin: c.Origin, BatchID: c.BatchID, Ops: ops,
	})
	if err != nil {
		return "", err
	}
	if len(res.NodeIDs) == 0 {
		return "", ErrNoNodeID
	}
	return res.NodeIDs[0], nil
}

// WriteBackBatchID 是写回那一批的幂等键：`{batchID}:{suffix}`（suffix 区分同一次调用里的不同写回）。
func WriteBackBatchID(batchID, suffix string) string { return batchID + ":" + suffix }

// WriteBack 用独立批次把业务对象的状态写回节点参数（merge patch）。失败只告警。
func (o Orchestrator) WriteBack(ctx context.Context, c Caller, nodeID, suffix string, params map[string]any) {
	if nodeID == "" || len(params) == 0 || o.Canvas == nil {
		return
	}
	ops, _ := json.Marshal([]any{map[string]any{"kind": "update_node", "nodeId": nodeID,
		"patch": map[string]any{"params": params}}})
	_, err := o.Canvas.Apply(ctx, ApplyInput{
		WorkspaceID: c.WorkspaceID, ProjectID: c.ProjectID, UserID: c.UserID, Role: c.Role,
		Origin: c.Origin, BatchID: WriteBackBatchID(c.BatchID, suffix), Ops: ops,
	})
	if err != nil {
		attrs := append(append([]any{}, c.LogAttrs...), "node", nodeID, "batch", suffix, "err", err)
		o.logger().Warn("业务对象已就绪，但写回画布节点失败（前端按节点重新打开时会再取一次）", attrs...)
	}
}

// ObjectError 是「节点已建好，但建业务对象失败」：节点保留（调用方据此说清节点 ID，不要再建一个）。
type ObjectError struct {
	NodeID string
	Err    error
}

func (e *ObjectError) Error() string {
	return fmt.Sprintf("节点 %s 已建好，但建业务对象失败：%v", e.NodeID, e.Err)
}

// Unwrap 保留业务错误码。
func (e *ObjectError) Unwrap() error { return e.Err }

// CreateObjectNode 是整条「建节点 → 建对象 → 写回」：
//
//	① 节点先落盘（对象与画布节点一一对应，要挂在一个真实存在的节点上）；
//	② create(nodeID) 按节点建对象（按节点幂等：节点上已有对象时返回那一个），返回要写回的节点参数；
//	③ 用独立批次 `{BatchID}:{suffix}` 写回（失败只告警）。
//
// ① 的错误原样返回；② 的错误包成 *ObjectError（节点已建好）。
func (o Orchestrator) CreateObjectNode(ctx context.Context, c Caller, n ObjectNode, suffix string,
	create func(ctx context.Context, nodeID string) (map[string]any, error)) (string, error) {
	nodeID, err := o.AddNode(ctx, c, n)
	if err != nil {
		return "", err
	}
	params, err := create(ctx, nodeID)
	if err != nil {
		return nodeID, &ObjectError{NodeID: nodeID, Err: err}
	}
	o.WriteBack(ctx, c, nodeID, suffix, params)
	return nodeID, nil
}

// ---------------------------------------------------------------- 节点参数（与前端写的键一致）

// ScriptFacts 是脚本节点要写回的事实。
type ScriptFacts struct {
	ID          int64
	SourceType  string
	Status      string
	CurrentStep int
	Version     int
	// Shots 每个镜头的两个提示词状态（ready / stale / pending …）。
	Shots []ShotPromptFacts
}

// ShotPromptFacts 是一个镜头的提示词状态。
type ShotPromptFacts struct {
	ImagePromptStatus string
	VideoPromptStatus string
}

// ScriptNodeParams 是脚本节点的参数（与前端 ScriptNodePanel / updateScriptNode 写的键一致）。
func ScriptNodeParams(v ScriptFacts) map[string]any {
	img, vid, both := 0, 0, 0
	for _, sh := range v.Shots {
		if sh.ImagePromptStatus == "ready" {
			img++
		}
		if sh.VideoPromptStatus == "ready" {
			vid++
		}
		if sh.ImagePromptStatus == "ready" && sh.VideoPromptStatus == "ready" {
			both++
		}
	}
	return map[string]any{
		"scriptId": strconv.FormatInt(v.ID, 10), "sourceType": v.SourceType, "scriptStatus": v.Status,
		"scriptStep": v.CurrentStep, "scriptVersion": v.Version, "scriptShotCount": len(v.Shots),
		"scriptPromptReady": both, "scriptImagePromptReady": img, "scriptVideoPromptReady": vid,
	}
}

// GroupFacts 是组节点要写回的事实。
type GroupFacts struct {
	ID        int64
	ProjectID int64
	ScriptID  int64
	GroupType string
	Status    string
	Version   int
	Items     []GroupItemFacts
}

// GroupItemFacts 是组里一项。
type GroupItemFacts struct {
	ID           int64
	ShotID       int64
	ShotNo       int
	AssetID      int64
	GenerationID int64
	Status       string
	Operation    string
	DurationSec  int
	Error        string
}

// GroupNodeParams 是组节点的参数（与前端 addGroupNode / updateGroupNode 写的键、useScriptApi 归一化后的
// 子项形状一致）。子项必须带 itemId：缺了的话前端判为坏节点去修。
func GroupNodeParams(g GroupFacts, modelCode string) map[string]any {
	idS := func(v int64) string { return strconv.FormatInt(v, 10) }
	outputType := "image"
	if g.GroupType == "video_generator" {
		outputType = "video"
	}
	items := make([]any, 0, len(g.Items))
	for _, it := range g.Items {
		snap := map[string]any{}
		if it.ShotNo > 0 {
			snap["shotNo"] = it.ShotNo
		}
		if it.Operation != "" {
			snap["operation"] = it.Operation
		}
		if it.DurationSec > 0 {
			snap["duration"] = it.DurationSec
		}
		item := map[string]any{
			"itemId": idS(it.ID), "shotId": "", "assetId": nil, "shotVersion": 0, "canvasNodeId": "",
			"generationId": nil, "status": it.Status, "progress": 0, "outputType": outputType,
			"outputUrl": nil, "paramsSnapshot": snap, "inputRefs": []any{}, "error": nil,
		}
		if it.ShotID != 0 {
			item["shotId"] = idS(it.ShotID)
		}
		if it.AssetID != 0 {
			item["assetId"] = idS(it.AssetID)
		}
		if it.GenerationID != 0 {
			item["generationId"] = idS(it.GenerationID)
		}
		if it.Status == "success" {
			item["progress"] = 100
		}
		if it.Error != "" {
			item["error"] = it.Error
		}
		items = append(items, item)
	}
	p := map[string]any{
		"groupId": idS(g.ID), "projectId": idS(g.ProjectID), "groupType": g.GroupType,
		"groupStatus": g.Status, "groupVersion": g.Version, "scriptId": idS(g.ScriptID),
		"itemCount": len(g.Items), "items": items,
	}
	if modelCode != "" {
		p["modelCode"] = modelCode
	}
	return p
}
