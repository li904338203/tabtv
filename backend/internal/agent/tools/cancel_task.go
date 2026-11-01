package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
)

// cancel_task（AGT-24）：取消本会话提交的生成任务。
//
// ⚠️ 只能取消**本会话某个步骤**受理的任务（按 agent_step.generation_id 反查）：
// 否则模型拿着画布上看到的任意 generationId 就能取消用户自己点的生成。
// ⚠️ 退款与额度归还都不在这里：退款由生成域按原批次完成（只退一次，AG-11），
// 额度条目 charged → refunded 由扫描循环做（AGT-25）。本工具不碰账本。

// GenerationSteps 按生成任务反查步骤（agent/repo.Repo 实现）。
type GenerationSteps interface {
	StepsByGeneration(ctx context.Context, generationID int64) ([]repo.Step, error)
}

// CancelTask 是 cancel_task 工具。
type CancelTask struct {
	Generator app.Generator
	Steps     GenerationSteps
}

func (CancelTask) Name() domain.ToolName { return domain.ToolCancelTask }
func (CancelTask) Paid() bool            { return false }
func (CancelTask) Action() collab.Action { return collab.ActionGenerate }

func (CancelTask) Description() string {
	return "取消本会话提交的、还没结束的生成任务（已扣的积分会自动退回）。只能取消你自己用 generate 提交的任务。"
}

func (CancelTask) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"generationId":{"type":"string","description":"generate 返回的 generationId"},` +
		`"reason":{"type":"string"}},"required":["generationId"]}`)
}

// ParseCancelTaskArgs 取出任务 ID（字符串或数字都收：ID 对外是字符串，但模型可能写成数字）。
func ParseCancelTaskArgs(raw json.RawMessage) (int64, string, error) {
	var a struct {
		GenerationID json.Number `json:"generationId"`
		Reason       string      `json:"reason"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		// json.Number 不收带引号以外的非数字；字符串形式的数字也能解进来。
		return 0, "", fmt.Errorf("参数不合法：%v", err)
	}
	id, err := strconv.ParseInt(strings.TrimSpace(a.GenerationID.String()), 10, 64)
	if err != nil || id <= 0 {
		return 0, "", errors.New("generationId 必须是 generate 返回的任务 ID")
	}
	return id, strings.TrimSpace(a.Reason), nil
}

func (t CancelTask) Run(ctx context.Context, rc app.RunContext, raw json.RawMessage) (app.ToolResult, error) {
	if t.Generator == nil || t.Steps == nil {
		return app.ToolResult{}, errors.New("生成服务未接入")
	}
	id, reason, err := ParseCancelTaskArgs(raw)
	if err != nil {
		return app.ToolResult{}, err
	}
	steps, err := t.Steps.StepsByGeneration(ctx, id)
	if err != nil {
		return app.ToolResult{}, fmt.Errorf("查询任务归属失败：%w", err)
	}
	owned := false
	for _, s := range steps {
		if s.SessionID == rc.SessionID {
			owned = true
			break
		}
	}
	if !owned {
		return app.ToolResult{}, errors.New("该任务不是本会话提交的，不能取消")
	}
	if reason == "" {
		reason = "Agent 取消"
	}
	idStr := strconv.FormatInt(id, 10)
	err = t.Generator.Cancel(ctx, rc.WorkspaceID, id, reason)
	if errors.Is(err, app.ErrTaskFinished) {
		content, _ := json.Marshal(map[string]any{"generationId": idStr, "cancelled": false,
			"message": "任务已经结束，无需取消"})
		return app.ToolResult{Content: content, Display: content}, nil
	}
	if err != nil {
		return app.ToolResult{}, fmt.Errorf("取消失败：%w", err)
	}
	content, _ := json.Marshal(map[string]any{"generationId": idStr, "cancelled": true})
	return app.ToolResult{Content: content, Display: content}, nil
}

var _ app.Tool = CancelTask{}
