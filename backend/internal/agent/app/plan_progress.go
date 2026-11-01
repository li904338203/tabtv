package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
)

// 批量变体的执行进度（MKT-T-27、dev/01 §3.6）：会话详情的 planProgress，按步骤现算。

// PlanStepsOf 把 agent_step 投影成计算用的计划步骤（保持顺序）。
func PlanStepsOf(steps []repo.Step) []domain.PlanStep {
	out := make([]domain.PlanStep, 0, len(steps))
	for _, s := range steps {
		out = append(out, domain.PlanStep{Key: s.PlanItemKey, Status: s.Status, ErrorCode: s.ErrorCode})
	}
	return out
}

// PlanProgressDetail 是会话详情里的计划进度：那张已批准规划卡 + 现算的汇总。
type PlanProgressDetail struct {
	CardID int64
	domain.PlanProgressView
}

// planProgress 现算营销会话最近一张已批准计划的进度。通用会话、还没有已批准计划、计划里没有条目键
// 时返回 nil（详情里是 null）。
func (s *Service) planProgress(ctx context.Context, sess repo.Session) (*PlanProgressDetail, error) {
	if sess.Scene != domain.SceneMarketing {
		return nil, nil
	}
	card, err := s.repo.LatestApprovedPlan(ctx, sess.ID)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p domain.PlanConfirmPayload
	if err := json.Unmarshal(card.Payload, &p); err != nil {
		return nil, fmt.Errorf("已批准的规划卡内容损坏：%w", err)
	}
	since := card.CreatedAt
	if card.SubmittedAt != nil {
		since = *card.SubmittedAt
	}
	steps, err := s.repo.PlanSteps(ctx, sess.ID, since)
	if err != nil {
		return nil, err
	}
	v, ok := domain.PlanProgress(p.Items, PlanStepsOf(steps), domain.SessionOpen(sess.State))
	if !ok {
		return nil, nil
	}
	return &PlanProgressDetail{CardID: card.ID, PlanProgressView: v}, nil
}
