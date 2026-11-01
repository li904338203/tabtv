package app

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// TurnEstimate 是「下一轮对话费」的预估（MCPT-17：外部客户端委托站内 Agent 时，每轮按站内规则扣 agent_chat，
// 不逐轮确认（M-8），但结果里要如实说出每轮多少钱）。
type TurnEstimate struct {
	// ModelCode 是这一轮会用的推理模型（会话偏好里选的；没选时为平台默认推理模型）。
	ModelCode string
	Credits   int
}

// EstimateTurn 按**运行器扣费的同一条路**估一轮对话费：resolveModel（会话偏好）→ 平台默认推理模型 →
// Quoter.QuoteTurn（operation=agent_chat，会员等级取调用者的，与生成受理同一个解析器）。只读：不建轮、不扣费。
//
// sessionID 为 0 时按 prefs 估（还没建会话时）。⚠️ 实际扣费以 Worker 跑这一轮时的报价为准
// （两次之间价格可能变）；系统唤醒的轮不收费（TurnTrigger.Billable）—— 这里估的是用户触发的一轮。
// 报价端口未接入或报不出价时返回错误（调用方不编数字）。
func (s *Service) EstimateTurn(ctx context.Context, a SessionActor, sessionID int64,
	prefs json.RawMessage) (TurnEstimate, error) {
	op := s.p.Optimize
	if op == nil || op.Quoter == nil {
		return TurnEstimate{}, httpx.Errf(errcode.Internal, "推理模型报价未接入")
	}
	sess := repo.Session{Prefs: prefs}
	if sessionID > 0 {
		got, err := s.load(ctx, a, sessionID)
		if err != nil {
			return TurnEstimate{}, err
		}
		sess = got
	}
	model := resolveModel(repo.Turn{}, sess)
	if model == "" && op.Reasoning != nil {
		code, err := op.Reasoning.DefaultReasoningModel(ctx, a.MemberLevelID)
		if err != nil {
			return TurnEstimate{}, httpx.Wrap(errcode.Internal, err)
		}
		model = code
	}
	if strings.TrimSpace(model) == "" {
		return TurnEstimate{}, httpx.Errf(errcode.ModelUnavailable, "还没有可用的推理模型")
	}
	q, err := op.Quoter.QuoteTurn(ctx, model, a.MemberLevelID)
	if err != nil {
		return TurnEstimate{}, err
	}
	return TurnEstimate{ModelCode: model, Credits: q.Credits}, nil
}
