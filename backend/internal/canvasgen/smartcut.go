package canvasgen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	smartcut "github.com/li904338203/tabtv/backend/internal/smartcut/domain"
)

// 智能剪辑节点（docs/agent/smartcut SCT-07，SCT-41 的 Agent / MCP 工具用）：照导演台的形状 ——
// ① 节点先落盘 → ② 按新节点 ID 建时间线（按（项目，节点）幂等，SCD-20）→ ③ 用独立批次写回 params.timelineId。
// ② 失败时节点保留（*ObjectError）：打开剪辑台时按节点自愈。
//
// ⚠️ 剪辑之前要按**节点**取时间线，不能只信 params.timelineId（SmartcutTimelineOfNode）：粘贴出来、还没打开过的
// 节点带的是原节点的 timelineId，拿它剪会把内容写进原节点的时间线 —— 改一个、另一个跟着变（SC-F-07）。

const (
	// DefaultSmartcutTitle 智能剪辑节点的默认标题（与 seed 的 node_type 名称一致）。
	DefaultSmartcutTitle = "智能剪辑"
	// smartcutWriteBack 是写回 params.timelineId 那一批的后缀（批次 `{BatchID}:timeline`）。
	smartcutWriteBack = "timeline"
	// smartcutInputsBatch 是把素材节点连进来那一批的后缀（批次 `{BatchID}:inputs`）。
	smartcutInputsBatch = "inputs"
	// MaxSmartcutInputs 一次最多替用户连几个节点（与素材条的上限同量级；再多的话一次请求里写一大批边）。
	MaxSmartcutInputs = 50
)

// ErrSmartcutTimelineNotFound 时间线不在本项目（别的项目粘贴来的、所在项目已清理）。
var ErrSmartcutTimelineNotFound = errors.New("时间线不在本项目")

// NotSmartcutError 节点存在，但不是智能剪辑。
type NotSmartcutError struct {
	NodeID string
	Type   string
}

func (e *NotSmartcutError) Error() string {
	return fmt.Sprintf("节点 %s 是 %s，不是智能剪辑（%s）", e.NodeID, e.Type, smartcut.NodeType)
}

// SmartcutTimelines 是建时间线与查时间线的端口（实现：wiring 适配 smartcut/app）。
type SmartcutTimelines interface {
	// Create 为画布节点建时间线，按（项目，节点）幂等：节点已有时间线时直接返回那一条（不分叉）。
	// sourceTimelineID 非 0 时复制那一条（同一工作区）。返回时间线 ID。
	Create(ctx context.Context, c Caller, nodeID string, sourceTimelineID int64) (int64, error)
	// TimelineNode 取本项目里一条时间线绑定的画布节点 ID；不在本项目时返回 ErrSmartcutTimelineNotFound。
	TimelineNode(ctx context.Context, c Caller, timelineID int64) (string, error)
}

// SmartcutNodeInput 是新建智能剪辑节点的入参。
type SmartcutNodeInput struct {
	// Title 为空时用 DefaultSmartcutTitle。
	Title    string
	Position *Position
}

// 节点的时间线被补上时说给用户听的话（空串 = 节点本来就指向自己的时间线）。
const (
	SmartcutRepairCreated = "这个智能剪辑还没有关联时间线，已补上"
	SmartcutRepairForked  = "这个智能剪辑与别的智能剪辑共用同一条时间线（复制出来还没打开过），已为它分出独立的时间线"
	SmartcutRepairMissing = "这个智能剪辑记的时间线不在本项目，已为它换上新的时间线"
)

// CreateSmartcutNode 新建智能剪辑节点并建好时间线：返回节点 ID 与时间线 ID。
//
// 节点建失败原样返回画布的错误；时间线建失败返回 *ObjectError（节点保留，打开时按节点补建）。
// 写回 params.timelineId 失败只告警：时间线按节点绑定，打开时按节点能找回它。
func CreateSmartcutNode(ctx context.Context, o Orchestrator, c Caller, in SmartcutNodeInput,
	timelines SmartcutTimelines) (string, int64, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = DefaultSmartcutTitle
	}
	var timelineID int64
	nodeID, err := o.CreateObjectNode(ctx, c, ObjectNode{NodeType: smartcut.NodeType, Title: title,
		Params: map[string]any{}, Position: in.Position}, smartcutWriteBack,
		func(ctx context.Context, nodeID string) (map[string]any, error) {
			id, err := timelines.Create(ctx, c, nodeID, 0)
			if err != nil {
				return nil, err
			}
			timelineID = id
			return smartcut.NodeParams(id), nil
		})
	if err != nil {
		return nodeID, 0, err
	}
	return nodeID, timelineID, nil
}

// SmartcutTimelineOfNode 取智能剪辑节点自己的时间线，按节点自愈（与前端打开剪辑台同一套规则）：
//
//   - params.timelineId 指向的时间线就绑定在这个节点上 → 用它（不写任何东西）；
//   - 指向别的节点的时间线（粘贴、复制出来还没打开过）→ 分叉一份给这个节点；
//   - 没有 timelineId，或那条时间线不在本项目 → 新建一条。
//
// 后两种按（项目，节点）幂等：节点其实已经有自己的时间线时拿回那一条。结果与 params.timelineId 不同时用独立批次
// 写回（失败只告警，前端打开时会再自愈一次）。第二个返回值是补救说明（SmartcutRepair*），没有补救时为空串。
func SmartcutTimelineOfNode(ctx context.Context, o Orchestrator, c Caller, nodeID string,
	timelines SmartcutTimelines) (int64, string, error) {
	v, err := o.Canvas.Read(ctx, c.WorkspaceID, c.ProjectID)
	if err != nil {
		return 0, "", err
	}
	var node *CanvasNode
	for i := range v.Nodes {
		if v.Nodes[i].ID == nodeID {
			node = &v.Nodes[i]
			break
		}
	}
	if node == nil {
		return 0, "", fmt.Errorf("%w：%s", ErrNodeNotFound, nodeID)
	}
	if node.Type != smartcut.NodeType {
		return 0, "", &NotSmartcutError{NodeID: nodeID, Type: node.Type}
	}
	current := smartcut.TimelineIDOf(node.Params)
	source, repair := int64(0), SmartcutRepairCreated
	if current != 0 {
		owner, err := timelines.TimelineNode(ctx, c, current)
		switch {
		case err == nil && owner == nodeID:
			return current, "", nil
		case err == nil:
			source, repair = current, SmartcutRepairForked
		case errors.Is(err, ErrSmartcutTimelineNotFound):
			repair = SmartcutRepairMissing
		default:
			return 0, "", err
		}
	}
	id, err := timelines.Create(ctx, c, nodeID, source)
	if err != nil {
		return 0, "", err
	}
	if id != current {
		o.WriteBack(ctx, c, nodeID, smartcutWriteBack, smartcut.NodeParams(id))
	}
	return id, repair, nil
}

// SmartcutInputError 要连进智能剪辑的节点不合适：不在画布上（Type 为空）、类型不能连进来、重复、就是它自己。
type SmartcutInputError struct {
	NodeID string
	Type   string
	Reason string
}

func (e *SmartcutInputError) Error() string { return e.Reason }

// CheckSmartcutInputs 校验要连进智能剪辑 target 的节点（纯函数）：个数不超上限、都在画布上、
// 类型能连进来（smartcut.AcceptsInbound，与 seed 的入边一致）、不重复、不是它自己。
func CheckSmartcutInputs(v CanvasView, target string, inputs []string) error {
	if len(inputs) > MaxSmartcutInputs {
		return &SmartcutInputError{Reason: fmt.Sprintf("一次最多连 %d 个节点", MaxSmartcutInputs)}
	}
	types := make(map[string]string, len(v.Nodes))
	for _, n := range v.Nodes {
		types[n.ID] = n.Type
	}
	seen := map[string]bool{}
	for i, id := range inputs {
		id = strings.TrimSpace(id)
		typ, ok := types[id]
		switch {
		case id == "":
			return &SmartcutInputError{Reason: fmt.Sprintf("第 %d 个节点 ID 为空", i+1)}
		case seen[id]:
			return &SmartcutInputError{NodeID: id, Type: typ, Reason: fmt.Sprintf("节点 %s 重复给了两次", id)}
		case id == target:
			return &SmartcutInputError{NodeID: id, Type: typ, Reason: "不能把智能剪辑连给它自己"}
		case !ok:
			return &SmartcutInputError{NodeID: id, Reason: fmt.Sprintf("画布上没有节点 %s", id)}
		case !smartcut.AcceptsInbound(typ):
			return &SmartcutInputError{NodeID: id, Type: typ, Reason: fmt.Sprintf(
				"节点 %s 是 %s，不能连进智能剪辑（只收 %s）", id, typ, strings.Join(smartcut.InboundNodeTypes, " / "))}
		}
		seen[id] = true
	}
	return nil
}

// SmartcutConnected 是一次连线的结果：这次新连的、本来就连着的（都按给的顺序）。
type SmartcutConnected struct {
	Added    []string
	Existing []string
}

// ConnectSmartcutInputs 把 inputs 按给的顺序连进智能剪辑节点 target（批次 `{BatchID}:inputs`）。
//
//   - 先校验再写（CheckSmartcutInputs）：任何一个不合适就一条都不连 —— 连一半的话素材条的编号与要求的顺序对不上；
//   - 已经连着的跳过：重试一次不多连一条（同一个素材连两次，素材条上就是两个编号）；
//   - 新连的不给 order：画布按 target 现有入边续上（与前端连线同一条规则），所以顺序 = 给的顺序。
//
// target 不在画布上回 ErrNodeNotFound、不是智能剪辑回 *NotSmartcutError；画布写入的错误原样返回
// （节点不是本来源建的、用户改过时由调用方翻成「在旁边新建一个」）。
func ConnectSmartcutInputs(ctx context.Context, o Orchestrator, c Caller, target string,
	inputs []string) (SmartcutConnected, error) {
	v, err := o.Canvas.Read(ctx, c.WorkspaceID, c.ProjectID)
	if err != nil {
		return SmartcutConnected{}, err
	}
	if err := smartcutNode(v, target); err != nil {
		return SmartcutConnected{}, err
	}
	if err := CheckSmartcutInputs(v, target, inputs); err != nil {
		return SmartcutConnected{}, err
	}
	linked := map[string]bool{}
	for _, e := range v.Edges {
		if e.Target == target {
			linked[e.Source] = true
		}
	}
	out := SmartcutConnected{Added: []string{}, Existing: []string{}}
	ops := make([]any, 0, len(inputs))
	for _, raw := range inputs {
		id := strings.TrimSpace(raw)
		if linked[id] {
			out.Existing = append(out.Existing, id)
			continue
		}
		ops = append(ops, map[string]any{"kind": "add_edge", "edge": map[string]any{"source": id, "target": target}})
		out.Added = append(out.Added, id)
	}
	if len(ops) == 0 {
		return out, nil
	}
	raw, _ := json.Marshal(ops)
	if _, err := o.Canvas.Apply(ctx, ApplyInput{WorkspaceID: c.WorkspaceID, ProjectID: c.ProjectID, UserID: c.UserID,
		Role: c.Role, Origin: c.Origin, BatchID: WriteBackBatchID(c.BatchID, smartcutInputsBatch), Ops: raw}); err != nil {
		return SmartcutConnected{}, err
	}
	return out, nil
}

// smartcutNode 校验 target 是画布上的智能剪辑节点。
func smartcutNode(v CanvasView, target string) error {
	for _, n := range v.Nodes {
		if n.ID != target {
			continue
		}
		if n.Type != smartcut.NodeType {
			return &NotSmartcutError{NodeID: target, Type: n.Type}
		}
		return nil
	}
	return fmt.Errorf("%w：%s", ErrNodeNotFound, target)
}

// SmartcutBoundTimeline 只读地取智能剪辑节点自己的时间线（smartcut_get、导出用）：params.timelineId 指向的
// 时间线绑定的就是这个节点时返回它；否则 ok=false（还没有、或复制出来还没打开过 —— 只读路径不替它补建或分叉，
// 那是写操作，只读凭据也做不了）。target 不在画布上回 ErrNodeNotFound、不是智能剪辑回 *NotSmartcutError。
func SmartcutBoundTimeline(ctx context.Context, o Orchestrator, c Caller, nodeID string,
	timelines SmartcutTimelines) (int64, bool, error) {
	v, err := o.Canvas.Read(ctx, c.WorkspaceID, c.ProjectID)
	if err != nil {
		return 0, false, err
	}
	if err := smartcutNode(v, nodeID); err != nil {
		return 0, false, err
	}
	var params map[string]any
	for _, n := range v.Nodes {
		if n.ID == nodeID {
			params = n.Params
		}
	}
	current := smartcut.TimelineIDOf(params)
	if current == 0 {
		return 0, false, nil
	}
	owner, err := timelines.TimelineNode(ctx, c, current)
	switch {
	case errors.Is(err, ErrSmartcutTimelineNotFound):
		return 0, false, nil
	case err != nil:
		return 0, false, err
	}
	return current, owner == nodeID, nil
}

// BindSmartcutNode 为一条已有的时间线在画布上建智能剪辑节点并绑上它（Agent 的 timeline_assemble，SCD-39）：
// ① 节点先落盘 → ② bind(nodeID) 把时间线绑到节点上（时间线域按（项目，节点）唯一）→ ③ 独立批次写回 params.timelineId。
// ① 的错误原样返回；② 的错误包成 *ObjectError（节点已建好）；③ 失败只告警（时间线按节点绑定，打开时能找回）。
func BindSmartcutNode(ctx context.Context, o Orchestrator, c Caller, in SmartcutNodeInput, timelineID int64,
	bind func(ctx context.Context, nodeID string) error) (string, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = DefaultSmartcutTitle
	}
	return o.CreateObjectNode(ctx, c, ObjectNode{NodeType: smartcut.NodeType, Title: title,
		Params: map[string]any{}, Position: in.Position}, smartcutWriteBack,
		func(ctx context.Context, nodeID string) (map[string]any, error) {
			if err := bind(ctx, nodeID); err != nil {
				return nil, err
			}
			return smartcut.NodeParams(timelineID), nil
		})
}

// ScriptNodeOf 画布上承载这个脚本的节点（脚本与画布节点一一对应）；没有时为空串（节点被删了）。
func ScriptNodeOf(v CanvasView, scriptID int64) string {
	want := strconv.FormatInt(scriptID, 10)
	for _, n := range v.Nodes {
		if n.Type != "script" {
			continue
		}
		switch id := n.Params["scriptId"].(type) {
		case string:
			if strings.TrimSpace(id) == want {
				return n.ID
			}
		case float64:
			if int64(id) == scriptID {
				return n.ID
			}
		}
	}
	return ""
}
