package app

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/li904338203/tabtv/backend/internal/timeline/repo"
)

// 时间线写入事件（docs/agent/smartcut SCD-19）。
//
// ⚠️ 出口只有一个：每条写路径提交成功后都调 publishUpdated —— 逐项接口、整份保存、装配、字幕、
// 快照恢复，以及后续卡复用这些写路径的 AI 落地、输入同步、片段回填、翻译。前端与 Agent 只在这一个
// 出口对账；各写各的话，漏掉的那一条路径改了时间线而别的标签页永远不知道。

// EventPublisher 把一条事件发到实时总线（platform/eventbus.Publisher 满足它）。
//
// 端口定义在时间线域、由组装根接上（API 与 Worker 各接一份：片段回填在 Worker 里发生）。
// 为 nil 时只是没有实时信号，前端靠补取对账。
type EventPublisher interface {
	Publish(ctx context.Context, topic string, payload []byte) error
}

// SetEvents 接上实时事件发布。
func (s *Service) SetEvents(p EventPublisher) { s.events = p }

// TimelineUpdatedType 是事件的 type。
const TimelineUpdatedType = "timeline.updated"

// 写入原因（api/events/timeline-updated.schema.json 的 reason）。
const (
	ReasonSave      = "save"
	ReasonEdit      = "edit"
	ReasonPlan      = "plan"
	ReasonSync      = "sync"
	ReasonRestore   = "restore"
	ReasonClipFill  = "clip_fill"
	ReasonAssemble  = "assemble"
	ReasonSubtitles = "subtitles"
	ReasonTranslate = "translate"
)

// TimelineUpdated 是 api/events/timeline-updated.schema.json 的 Go 形状（测试直接读 schema 核对）。
//
// 只是对账信号：前端本地版本 < Version 时去取正文，不拿事件当正文。
type TimelineUpdated struct {
	Type         string  `json:"type"`
	OccurredAt   string  `json:"occurredAt"`
	ProjectID    string  `json:"projectId"`
	TimelineID   string  `json:"timelineId"`
	CanvasNodeID *string `json:"canvasNodeId"`
	Version      int     `json:"version"`
	Reason       string  `json:"reason"`
	EditorKind   string  `json:"editorKind"`
	EditorID     *string `json:"editorId"`
	EditorName   *string `json:"editorName"`
}

// NewTimelineUpdated 由刚写完的时间线构造事件。
//
// 编辑者说不清（Agent / MCP 的写路径接入编辑者之前，两列为空）时 editorKind 记 system：
// schema 要求一个确定的来源，而「system」正是「不是某个人亲手改的」。
func NewTimelineUpdated(t repo.Timeline, reason string, now time.Time) TimelineUpdated {
	kind := t.LastEditorKind
	if kind == "" {
		kind = repo.EditorSystem
	}
	return TimelineUpdated{
		Type:         TimelineUpdatedType,
		OccurredAt:   now.UTC().Format(time.RFC3339Nano),
		ProjectID:    strconv.FormatInt(t.ProjectID, 10),
		TimelineID:   strconv.FormatInt(t.ID, 10),
		CanvasNodeID: optString(t.CanvasNodeID),
		Version:      t.Version,
		Reason:       reason,
		EditorKind:   kind,
		EditorID:     optInt64(t.LastEditorUserID),
		EditorName:   optString(t.LastEditorName),
	}
}

// publishUpdated 在写入提交成功之后发 timeline.updated 到 project:{id}。失败只告警：数据已经落库，
// 前端按版本号对账（同画布与导演台的事件）。
func (s *Service) publishUpdated(ctx context.Context, t repo.Timeline, reason string) {
	if s.events == nil {
		return
	}
	payload, err := json.Marshal(NewTimelineUpdated(t, reason, time.Now()))
	if err == nil {
		err = s.events.Publish(ctx, "project:"+strconv.FormatInt(t.ProjectID, 10), payload)
	}
	if err != nil {
		s.warn(ctx, "推送时间线事件失败", "timelineId", t.ID, "projectId", t.ProjectID, "err", err)
	}
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func optInt64(v int64) *string {
	if v == 0 {
		return nil
	}
	s := strconv.FormatInt(v, 10)
	return &s
}
