package canvasgen

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// 3D 导演台节点（docs/agent/director，DRT-29，dev/01 §3.13）：Agent 的 director_create / director_build
// 与 MCP 的同名工具共用这一份。
//
// 建节点照 script_create 的形状（CreateObjectNode）：① 节点先落盘 → ② 按新节点 ID 建场景（按（项目，节点）
// 幂等，DRD-17）→ ③ 用独立批次写回 params.sceneId。② 失败时节点保留（*ObjectError）：前端打开导演台时
// 按节点自愈（DRD-43）。
//
// ⚠️ 搭景之前要按**节点**取场景，不能只信 params.sceneId（DirectorSceneOfNode）：粘贴出来、还没打开过的
// 节点带的是原节点的 sceneId，拿它搭景会把内容写进原节点的场景 —— 改一个、另一个跟着变（DR-AC-42）。
// 判定与前端打开导演台时的自愈同一套（dev/03 §6.3）：缺了就建、不在本项目就新建、属于别的节点就分叉。

// 导演台节点的类型码与默认标题（与前端新建导演台、seed 的 node_type 一致）。
const (
	DirectorNodeType     = "director"
	DefaultDirectorTitle = "3D 导演台"
	// directorWriteBack 是写回 params.sceneId 那一批的后缀（批次 `{BatchID}:scene`）。
	directorWriteBack = "scene"
)

var (
	// ErrDirectorSceneNotFound 场景不在本项目（别的项目粘贴来的、所在项目已清理）。
	// DirectorScenes.SceneNode 用它表达「按没有场景处理」，与其它错误分开。
	ErrDirectorSceneNotFound = errors.New("场景不在本项目")
	// ErrNodeNotFound 画布上没有这个节点。
	ErrNodeNotFound = errors.New("画布上没有这个节点")
)

// NotDirectorError 节点存在，但不是导演台。
type NotDirectorError struct {
	NodeID string
	Type   string
}

func (e *NotDirectorError) Error() string {
	return fmt.Sprintf("节点 %s 是 %s，不是 3D 导演台（%s）", e.NodeID, e.Type, DirectorNodeType)
}

// DirectorNodeInput 是新建导演台节点的入参。
type DirectorNodeInput struct {
	// Title 为空时用 DefaultDirectorTitle。
	Title    string
	Position *Position
}

// DirectorScenes 是建场景与查场景的端口（实现：wiring 适配 canvas/app，Agent 侧经 agent 的端口再包一层）。
type DirectorScenes interface {
	// Create 为画布节点建场景，按（项目，节点）幂等：节点已有场景时直接返回那一份（不分叉）。
	// sourceSceneID 非 0 时复制那一份（同一工作区）。返回场景 ID。
	Create(ctx context.Context, c Caller, nodeID string, sourceSceneID int64) (int64, error)
	// SceneNode 取本项目里一份场景绑定的画布节点 ID；场景不在本项目时返回 ErrDirectorSceneNotFound。
	SceneNode(ctx context.Context, c Caller, sceneID int64) (string, error)
}

// DirectorNodeParams 是写回导演台节点的参数（与前端 addDirectorNode 写的键一致：只有 sceneId，字符串）。
func DirectorNodeParams(sceneID int64) map[string]any {
	return map[string]any{"sceneId": strconv.FormatInt(sceneID, 10)}
}

// DirectorSceneIDOf 读节点参数里的 sceneId；缺失或不是正整数时返回 0（按「没有场景」处理）。
// 前端写的是字符串；数字形式只在 JS 安全整数范围内认（超出的已经丢了精度，认了就是指向别人的场景）。
func DirectorSceneIDOf(params map[string]any) int64 {
	switch v := params["sceneId"].(type) {
	case string:
		id, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err == nil && id > 0 {
			return id
		}
	case float64:
		if v > 0 && v <= 1<<53 && v == math.Trunc(v) {
			return int64(v)
		}
	}
	return 0
}

// CreateDirectorNode 新建导演台节点并建好场景（dev/01 §3.13）：返回节点 ID 与场景 ID。
//
// 节点建失败原样返回画布的错误；场景建失败返回 *ObjectError（节点保留，前端打开时按节点补建）。
// 写回 params.sceneId 失败只告警：场景按节点绑定，前端打开时按节点能找回它。
func CreateDirectorNode(ctx context.Context, o Orchestrator, c Caller, in DirectorNodeInput,
	scenes DirectorScenes) (string, int64, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = DefaultDirectorTitle
	}
	var sceneID int64
	nodeID, err := o.CreateObjectNode(ctx, c, ObjectNode{NodeType: DirectorNodeType, Title: title,
		Params: map[string]any{}, Position: in.Position}, directorWriteBack,
		func(ctx context.Context, nodeID string) (map[string]any, error) {
			id, err := scenes.Create(ctx, c, nodeID, 0)
			if err != nil {
				return nil, err
			}
			sceneID = id
			return DirectorNodeParams(id), nil
		})
	if err != nil {
		return nodeID, 0, err
	}
	return nodeID, sceneID, nil
}

// 节点的场景被补上时说给用户听的话（空串 = 节点本来就指向自己的场景）。
const (
	DirectorRepairCreated = "这个导演台还没有关联场景，已补上"
	DirectorRepairForked  = "这个导演台与别的导演台共用同一份场景（复制出来还没打开过），已为它分出独立的场景"
	DirectorRepairMissing = "这个导演台记的场景不在本项目，已为它换上新的场景"
)

// DirectorSceneOfNode 取导演台节点自己的场景，按节点自愈（与前端打开导演台同一套规则，dev/03 §6.3）：
//
//   - params.sceneId 指向的场景就绑定在这个节点上 → 用它（不写任何东西）；
//   - 指向别的节点的场景（粘贴、复制出来还没打开过）→ 分叉一份给这个节点；
//   - 没有 sceneId，或那份场景不在本项目 → 新建一份。
//
// 后两种按（项目，节点）幂等：节点其实已经有自己的场景时拿回那一份。结果与 params.sceneId 不同时用独立批次
// 写回（失败只告警，前端打开时会再自愈一次）。第二个返回值是补救说明（DirectorRepair*），没有补救时为空串。
func DirectorSceneOfNode(ctx context.Context, o Orchestrator, c Caller, nodeID string,
	scenes DirectorScenes) (int64, string, error) {
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
	if node.Type != DirectorNodeType {
		return 0, "", &NotDirectorError{NodeID: nodeID, Type: node.Type}
	}
	current := DirectorSceneIDOf(node.Params)
	source, repair := int64(0), DirectorRepairCreated
	if current != 0 {
		owner, err := scenes.SceneNode(ctx, c, current)
		switch {
		case err == nil && owner == nodeID:
			return current, "", nil
		case err == nil:
			source, repair = current, DirectorRepairForked
		case errors.Is(err, ErrDirectorSceneNotFound):
			repair = DirectorRepairMissing
		default:
			return 0, "", err
		}
	}
	id, err := scenes.Create(ctx, c, nodeID, source)
	if err != nil {
		return 0, "", err
	}
	if id != current {
		o.WriteBack(ctx, c, nodeID, directorWriteBack, DirectorNodeParams(id))
	}
	return id, repair, nil
}
