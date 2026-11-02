package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/li904338203/tabtv/backend/internal/generation/domain"
	"github.com/li904338203/tabtv/backend/internal/generation/repo"
)

// 状态事件经进程内事件总线推给前端（TT-T-06）。事件不是事实来源，前端断开重连后要用 HTTP 对账（GO-26）。

// GenerationUpdate 是推给浏览器的任务状态事件，形状与 api/events/generation-update.schema.json 一致
// （events_test 拿 schema 的 required / properties 逐项核，改了一边另一边就红）。
//
// ⚠️ 此前推的是 map 拼出来的六个字段，缺 occurredAt / workspaceId / outputs / error ——
// 失败事件里**没有原因**。前端只把事件当「该去对账了」的信号（GO-26），但契约是唯一来源，这里照样给全。
type GenerationUpdate struct {
	Type         string         `json:"type"`
	OccurredAt   string         `json:"occurredAt"`
	GenerationID string         `json:"generationId"`
	WorkspaceID  string         `json:"workspaceId"`
	ProjectID    string         `json:"projectId"`
	NodeID       *string        `json:"nodeId"`
	GroupItemID  *string        `json:"groupItemId"`
	Status       string         `json:"status"`
	Progress     int            `json:"progress"`
	Outputs      []UpdateOutput `json:"outputs"`
	Error        *UpdateError   `json:"error"`
}

// UpdateOutput 是事件里的一个产物。
type UpdateOutput struct {
	Index      int    `json:"index"`
	OutputType string `json:"outputType"`
	// URL 是媒体产物的受保护端点（与产物列表同一个，见 domain.OutputMediaURL）；
	// 文本产物没有 —— 正文只在 `GET /generations/{id}/outputs` 上。
	URL string `json:"url,omitempty"`
}

// UpdateError 是失败与取消的原因：reason 与任务行上的 error_code 同一套词。
type UpdateError struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// NewGenerationUpdate 拼一条终态事件。outputs 只在成功时有；code / msg 只在失败与取消时有。
func NewGenerationUpdate(g repo.Generation, status string, outputs []repo.Output,
	code, msg string, now time.Time) GenerationUpdate {
	u := GenerationUpdate{
		Type:         "generation.update",
		OccurredAt:   now.UTC().Format(time.RFC3339Nano),
		GenerationID: itoa(g.ID),
		WorkspaceID:  itoa(g.WorkspaceID),
		ProjectID:    itoa(g.ProjectID),
		Status:       status,
		Progress:     100,
		// ⚠️ 空也要是 []：契约要的是数组，null 会让按数组遍历的消费方直接抛错。
		Outputs: make([]UpdateOutput, 0, len(outputs)),
	}
	// 组子项与脚本任务没有画布节点：给 null，不给空串（空串会被当成一个叫 "" 的节点）。
	if g.NodeID != "" {
		node := g.NodeID
		u.NodeID = &node
	}
	if g.GroupItemID != 0 {
		item := itoa(g.GroupItemID)
		u.GroupItemID = &item
	}
	for _, o := range outputs {
		out := UpdateOutput{Index: o.Idx, OutputType: o.OutputType}
		if o.MediaObjectID != 0 {
			out.URL = domain.OutputMediaURL(o.MediaObjectID)
		}
		u.Outputs = append(u.Outputs, out)
	}
	if status != domain.StatusSuccess {
		u.Error = &UpdateError{Reason: code, Message: msg}
	}
	return u
}

// publishUpdate 推一条终态事件。Worker（成功 / 失败）与 API（取消 / 运维补偿）共用。
//
// ⚠️ 每一条进入终态的路径都要推：此前只有 Worker 那两条推了，用户在任务中心取消、运维补偿
//
//	都不推 —— 前端轮询耗尽之后还在等的那个节点，就只能等用户手动刷新。
//
// ⚠️ 推送失败只告警：任务结果已经落库，前端本来就要用 HTTP 对账（GO-26）。
func publishUpdate(ctx context.Context, p EventPublisher, log *slog.Logger, g repo.Generation,
	status string, outputs []repo.Output, code, msg string) {
	if p == nil {
		return
	}
	payload, err := json.Marshal(NewGenerationUpdate(g, status, outputs, code, msg, time.Now()))
	if err == nil {
		err = p.PublishState(ctx, g.WorkspaceID, payload)
	}
	if err != nil && log != nil {
		log.WarnContext(ctx, "推送状态事件失败", "generationId", g.ID, "error", err)
	}
}

// BusEventPublisher 发布生成状态事件（执行器的成功 / 失败，取消 / 补偿）。
//
// topic = `workspace:{id}`，payload 原样 —— 前端在 `workspace:{id}` 上收到的仍是裸的
// `{"type":"generation.update",...}`。按工作区投递而不是按用户（与来源系统一致）。
type BusEventPublisher struct {
	bus interface {
		Publish(ctx context.Context, topic string, payload []byte) error
	}
}

// NewBusEventPublisher 用事件总线（eventbus.Local）构造发布器。
func NewBusEventPublisher(bus interface {
	Publish(ctx context.Context, topic string, payload []byte) error
}) *BusEventPublisher {
	return &BusEventPublisher{bus: bus}
}

func (p *BusEventPublisher) PublishState(ctx context.Context, workspaceID int64,
	payload json.RawMessage) error {
	return p.bus.Publish(ctx, "workspace:"+itoa(workspaceID), payload)
}
