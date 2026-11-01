package app

import (
	"encoding/json"
	"strconv"
	"time"
)

// 画布 Agent 的四类实时事件（dev/02 §7；契约 api/events/agent-*.schema.json，AGT-26）。
//
// ⚠️ 事件只是**对账信号**（AG-16、GO-26）：前端收到 state / step / card 后一律走 HTTP 拉
// 会话详情；delta 是纯展示，丢了无所谓。所以结构里只放「去拉什么」需要的 ID 与少量
// 展示字段，不放完整消息体。全部投递到 `project:{projectId}`（topic 已说明是哪个项目，
// 载荷里不再重复）。OccurredAt 由发布方在发出前填（Runner.publish / Service.publish）。
// ⚠️ 事件没有代码生成：events_test.go 直接读 schema 文件核 required / properties，
// 改了任何一边另一边就红。API（service.go）与 Worker（runner / loop）共用这里的结构，不各拼一份。

// 事件 type。
const (
	EventAgentState = "agent.state"
	EventAgentDelta = "agent.delta"
	EventAgentStep  = "agent.step"
	EventAgentCard  = "agent.card"
)

// ProjectTopic 是 Agent 事件的投递 topic。
func ProjectTopic(projectID int64) string { return "project:" + strconv.FormatInt(projectID, 10) }

// AgentStateEvent：会话状态或额度变了（发消息、提交卡片、一轮开始 / 结束、取消）。
// TurnID / PendingCardID 为空串时不出现在载荷里。
type AgentStateEvent struct {
	Type           string `json:"type"`
	OccurredAt     string `json:"occurredAt"`
	SessionID      string `json:"sessionId"`
	State          string `json:"state"`
	TurnID         string `json:"turnId,omitempty"`
	ApprovedBudget int    `json:"approvedBudget"`
	Spent          int    `json:"spent"`
	PendingCardID  string `json:"pendingCardId,omitempty"`
	// Reason 是这次变化的原因码（insufficient_credits / stopped / cancelled …），没有就不出现。
	Reason string `json:"reason,omitempty"`
}

// AgentDeltaEvent：助手文本的流式增量（Worker 按 50ms 或 64 字符合并后发）。
type AgentDeltaEvent struct {
	Type       string `json:"type"`
	OccurredAt string `json:"occurredAt"`
	SessionID  string `json:"sessionId"`
	TurnID     string `json:"turnId"`
	// MessageID 是这段文本最终落库的那条助手消息（Worker 预先分配）。
	MessageID string `json:"messageId"`
	Text      string `json:"text"`
}

// AgentStepEvent：一次工具调用开始或结束。Display 为空时不出现。
type AgentStepEvent struct {
	Type       string          `json:"type"`
	OccurredAt string          `json:"occurredAt"`
	SessionID  string          `json:"sessionId"`
	TurnID     string          `json:"turnId"`
	StepID     string          `json:"stepId"`
	Tool       string          `json:"tool"`
	Status     string          `json:"status"`
	Display    json.RawMessage `json:"display,omitempty"`
}

// AgentCardEvent：卡片发出、提交、作废或过期。
type AgentCardEvent struct {
	Type       string `json:"type"`
	OccurredAt string `json:"occurredAt"`
	SessionID  string `json:"sessionId"`
	CardID     string `json:"cardId"`
	Kind       string `json:"kind"`
	Status     string `json:"status"`
}

// idStr 把内部 int64 ID 转成对外的十进制字符串（§5.4）。
func idStr(v int64) string { return strconv.FormatInt(v, 10) }

// nowStr 是事件的 occurredAt（UTC，RFC3339 带纳秒）。
func nowStr(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
