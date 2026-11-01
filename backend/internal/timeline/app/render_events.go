package app

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/li904338203/tabtv/backend/internal/timeline/repo"
)

// 导出进度事件 render.update（docs/agent/smartcut SCT-15，SCB-6：以前 schema 有定义、没有任何发布方）。
//
// 投到 project:{projectId}（render_job 没有 project_id，经时间线取）。只是对账信号：
// 前端以 GET /renders/{id} 为准，断了实时通道靠轮询照样看得到终态（SC-AC-24）。

// RenderUpdateType 是事件的 type。
const RenderUpdateType = "render.update"

// RenderUpdate 是 api/events/render-update.schema.json 的 Go 形状（测试直接读 schema 核对）。
type RenderUpdate struct {
	Type            string              `json:"type"`
	OccurredAt      string              `json:"occurredAt"`
	RenderJobID     string              `json:"renderJobId"`
	WorkspaceID     string              `json:"workspaceId"`
	ProjectID       string              `json:"projectId"`
	TimelineID      string              `json:"timelineId"`
	TimelineVersion int                 `json:"timelineVersion"`
	Status          string              `json:"status"`
	Progress        int                 `json:"progress"`
	Stage           *string             `json:"stage"`
	Output          *RenderUpdateOutput `json:"output,omitempty"`
	Error           *RenderUpdateError  `json:"error,omitempty"`
}

// RenderUpdateOutput 成片。url 是受保护的媒体端点（不是签名直链：事件会广播给项目里的每个连接）。
type RenderUpdateOutput struct {
	MediaID     string `json:"mediaId"`
	URL         string `json:"url"`
	ContentType string `json:"contentType"`
}

// RenderUpdateError 失败原因。
type RenderUpdateError struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// NewRenderUpdate 由任务当前的样子构造事件。
func NewRenderUpdate(j repo.RenderJob, now time.Time) RenderUpdate {
	ev := RenderUpdate{
		Type: RenderUpdateType, OccurredAt: now.UTC().Format(time.RFC3339Nano),
		RenderJobID: strconv.FormatInt(j.ID, 10), WorkspaceID: strconv.FormatInt(j.WorkspaceID, 10),
		ProjectID: strconv.FormatInt(j.ProjectID, 10), TimelineID: strconv.FormatInt(j.TimelineID, 10),
		TimelineVersion: max(j.TimelineVersion, 1), Status: j.Status, Progress: min(max(j.Progress, 0), 100),
		Stage: optString(j.Stage),
	}
	if j.Status == "success" && j.OutputMediaID != 0 {
		id := strconv.FormatInt(j.OutputMediaID, 10)
		ev.Output = &RenderUpdateOutput{MediaID: id, URL: "/api/v1/media/" + id, ContentType: "video/mp4"}
	}
	if j.Status == "failed" {
		reason := j.ErrorCode
		if reason == "" {
			reason = "render_failed"
		}
		ev.Error = &RenderUpdateError{Reason: reason, Message: j.ErrorMessage}
	}
	return ev
}

// publishRenderEvent 发一条 render.update。时间线已被删（项目取不到）时不发；失败只告警。
func publishRenderEvent(ctx context.Context, p EventPublisher, j repo.RenderJob, warn func(string, ...any)) {
	if p == nil || j.ProjectID == 0 {
		return
	}
	payload, err := json.Marshal(NewRenderUpdate(j, time.Now()))
	if err == nil {
		err = p.Publish(ctx, "project:"+strconv.FormatInt(j.ProjectID, 10), payload)
	}
	if err != nil && warn != nil {
		warn("推送导出进度事件失败", "renderId", j.ID, "projectId", j.ProjectID, "err", err)
	}
}

// publishRender 受理与取消（API 进程）发事件。
func (s *Service) publishRender(ctx context.Context, j repo.RenderJob) {
	publishRenderEvent(ctx, s.events, j, func(msg string, args ...any) { s.warn(ctx, msg, args...) })
}
