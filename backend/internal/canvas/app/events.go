package app

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/li904338203/tabtv/backend/internal/canvas/domain"
	"github.com/li904338203/tabtv/backend/internal/canvas/repo"
)

// EventPublisher 把一条事件发到实时总线（platform/eventbus.Publisher 满足它）。
//
// ⚠️ 端口定义在画布域、由组装根接上：画布域不 import 总线包，Worker / 开发工具
// 也能各自接一个实现（或不接 —— 为 nil 时只是没有实时信号，前端靠补取对账）。
type EventPublisher interface {
	Publish(ctx context.Context, topic string, payload []byte) error
}

// SetEvents 接上实时事件发布。
func (s *Service) SetEvents(p EventPublisher) { s.events = p }

// CanvasOpsAppliedType 是事件的 type（开发 02 §7；总体设计 §7.2 写的 agent.canvas_ops 以本名为准）。
const CanvasOpsAppliedType = "canvas.ops_applied"

// MaxEventOpsBytes 是事件里携带 ops 的上限。超过就不带（truncated=true），
// 前端改走 GET /projects/{id}/canvas/ops 补取 —— 一条 WS 帧塞几 MB 会把慢连接堵住，
// 而事件本来就只是对账信号。
const MaxEventOpsBytes = 64 << 10

// CanvasOpsApplied 是 api/events/canvas-ops-applied.schema.json 的 Go 形状（测试直接读 schema 核对）。
type CanvasOpsApplied struct {
	Type        string  `json:"type"`
	OccurredAt  string  `json:"occurredAt"`
	WorkspaceID string  `json:"workspaceId"`
	ProjectID   string  `json:"projectId"`
	BatchID     string  `json:"batchId"`
	FromVersion int     `json:"fromVersion"`
	ToVersion   int     `json:"toVersion"`
	Actor       OpActor `json:"actor"`
	// Ops 是**已解析**的操作（ID 都是真实的），前端按序重放；truncated 时省略。
	Ops       json.RawMessage `json:"ops,omitempty"`
	Truncated bool            `json:"truncated"`
}

// OpActor 是一批操作的发起方。ID 对外一律 string；没有时为 null。
//
// ClientName（MCPT-09）只在**事件**里带（来自这一次调用的 MCP 授权），补取接口给 null ——
// 日志不存它，canvas 域也不读 mcp_* 表；前端回落到节点上的 origin.clientName。
type OpActor struct {
	Kind           string  `json:"kind"`
	UserID         *string `json:"userId"`
	AgentSessionID *string `json:"agentSessionId"`
	McpGrantID     *string `json:"mcpGrantId"`
	ClientName     *string `json:"clientName"`
}

// NewOpActor 由日志行构造对外的操作者（clientName 为 null）。
func NewOpActor(e repo.OpLogEntry) OpActor {
	return OpActor{Kind: e.ActorKind, UserID: optID(e.UserID), AgentSessionID: optID(e.AgentSessionID),
		McpGrantID: optID(e.McpGrantID)}
}

// NewCanvasOpsApplied 由刚写入的日志行构造事件。clientName 只对 mcp 写入有意义，空串时为 null。
func NewCanvasOpsApplied(workspaceID int64, e repo.OpLogEntry, clientName string,
	now time.Time) CanvasOpsApplied {
	ev := CanvasOpsApplied{
		Type:        CanvasOpsAppliedType,
		OccurredAt:  now.UTC().Format(time.RFC3339Nano),
		WorkspaceID: strconv.FormatInt(workspaceID, 10),
		ProjectID:   strconv.FormatInt(e.ProjectID, 10),
		BatchID:     e.BatchID,
		FromVersion: e.FromVersion,
		ToVersion:   e.ToVersion,
		Actor:       NewOpActor(e),
	}
	if clientName != "" && e.ActorKind == domain.ActorMCP {
		ev.Actor.ClientName = &clientName
	}
	if len(e.Ops) > MaxEventOpsBytes {
		ev.Truncated = true
	} else {
		ev.Ops = e.Ops
	}
	return ev
}

func optID(v int64) *string {
	if v == 0 {
		return nil
	}
	s := strconv.FormatInt(v, 10)
	return &s
}

// publishOps 在**事务提交之后**发事件。失败只告警：事件只是信号，
// 数据已经落库，前端会按版本号补取对账（GO-26）。
func (s *Service) publishOps(ctx context.Context, workspaceID int64, e repo.OpLogEntry, clientName string) {
	if s.events == nil {
		return
	}
	payload, err := json.Marshal(NewCanvasOpsApplied(workspaceID, e, clientName, time.Now()))
	if err == nil {
		err = s.events.Publish(ctx, "project:"+strconv.FormatInt(e.ProjectID, 10), payload)
	}
	if err != nil {
		s.logWarn(ctx, "推送画布操作事件失败", e.ProjectID, err)
	}
}

// ---------------------------------------------------------------- 3D 导演台（docs/agent/director，DRD-31）

// DirectorSceneUpdatedType 是场景保存成功事件的 type。
const DirectorSceneUpdatedType = "director.scene_updated"

// DirectorSceneUpdated 是 api/events/director-scene-updated.schema.json 的 Go 形状（测试直接读 schema 核对）。
//
// 只是对账信号：前端本地版本 < Version 时去取正文，不拿事件当正文（CLAUDE.md §5.3）。
type DirectorSceneUpdated struct {
	Type              string  `json:"type"`
	OccurredAt        string  `json:"occurredAt"`
	ProjectID         string  `json:"projectId"`
	SceneID           string  `json:"sceneId"`
	CanvasNodeID      string  `json:"canvasNodeId"`
	Version           int     `json:"version"`
	RenderFingerprint string  `json:"renderFingerprint"`
	EditorKind        string  `json:"editorKind"`
	EditorID          *string `json:"editorId"`
	EditorName        string  `json:"editorName"`
}

// NewDirectorSceneUpdated 由刚保存的场景构造事件。
func NewDirectorSceneUpdated(sc repo.DirectorScene, now time.Time) DirectorSceneUpdated {
	return DirectorSceneUpdated{
		Type:              DirectorSceneUpdatedType,
		OccurredAt:        now.UTC().Format(time.RFC3339Nano),
		ProjectID:         strconv.FormatInt(sc.ProjectID, 10),
		SceneID:           strconv.FormatInt(sc.ID, 10),
		CanvasNodeID:      sc.CanvasNodeID,
		Version:           sc.Version,
		RenderFingerprint: sc.RenderFingerprint,
		EditorKind:        sc.Source,
		EditorID:          optID(sc.EditorID),
		EditorName:        sc.EditorName,
	}
}

// publishDirectorScene 在保存成功之后发事件，网页 / Agent / MCP 三条写入路径都走这里（DRD-31）。
// 失败只告警：数据已经落库，前端按版本号对账（同画布事件）。
func (s *Service) publishDirectorScene(ctx context.Context, sc repo.DirectorScene) {
	if s.events == nil {
		return
	}
	payload, err := json.Marshal(NewDirectorSceneUpdated(sc, time.Now()))
	if err == nil {
		err = s.events.Publish(ctx, "project:"+strconv.FormatInt(sc.ProjectID, 10), payload)
	}
	if err != nil {
		s.logWarn(ctx, "推送导演台场景事件失败", sc.ProjectID, err)
	}
}
