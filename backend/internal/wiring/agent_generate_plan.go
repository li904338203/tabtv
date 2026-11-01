package wiring

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	agentdomain "github.com/li904338203/tabtv/backend/internal/agent/domain"
	agentrepo "github.com/li904338203/tabtv/backend/internal/agent/repo"
	marketingapp "github.com/li904338203/tabtv/backend/internal/marketing/app"
	mktdomain "github.com/li904338203/tabtv/backend/internal/marketing/domain"
	scriptapp "github.com/li904338203/tabtv/backend/internal/script/app"
)

// AgentPlanSource 把会话、已批准的规划卡与脚本资产接成 agentapp.PlanSource（MKT-T-22）：
// 营销会话里的 generate 按条目键取已批准的条目、把角色换成要发的图。
type AgentPlanSource struct {
	Repo *agentrepo.Repo
	// Script 角色槽里的资产引用取它的图；为 nil 时资产角色一律报错（不悄悄少发一张参考图）。
	Script *scriptapp.Service
	// Marketing 取创意的推荐画幅（CreativeRatio）：新建的营销视频节点模型没写画幅时按它补。
	// 为 nil 时不给推荐（由创作偏好 / 节点默认兜底）。
	Marketing *marketingapp.Service
}

// SessionPlan 读会话场景；营销会话再读 scene_input 与最近一张已批准的规划卡。
//
// ⚠️ 条目没写操作码时按**报价同一个映射**补成实际操作码（defaultPlanOperation，PlanQuoteRequest 用的那份）：
// generate 拿它与模型给的操作码比、再交给受理 —— 各补一份的话「卡片按 text2video 报的价」与
// 「实际按别的操作受理」会悄悄分叉。
func (s AgentPlanSource) SessionPlan(ctx context.Context, sessionID int64) (agentapp.SessionPlan, error) {
	sess, err := s.Repo.GetByID(ctx, sessionID)
	if err != nil {
		return agentapp.SessionPlan{}, err
	}
	out := agentapp.SessionPlan{Scene: sess.Scene}
	if sess.Scene != agentdomain.SceneMarketing {
		return out, nil
	}
	if out.Input, err = agentapp.ParseSceneInput(sess.SceneInput); err != nil {
		return agentapp.SessionPlan{}, err
	}
	card, err := s.Repo.LatestApprovedPlan(ctx, sessionID)
	if errors.Is(err, agentrepo.ErrNotFound) {
		return out, nil
	}
	if err != nil {
		return agentapp.SessionPlan{}, err
	}
	var p agentdomain.PlanConfirmPayload
	if err := json.Unmarshal(card.Payload, &p); err != nil {
		return agentapp.SessionPlan{}, fmt.Errorf("已批准的规划卡内容损坏：%w", err)
	}
	for i := range p.Items {
		if p.Items[i].OperationCode == "" {
			p.Items[i].OperationCode = defaultPlanOperation[p.Items[i].Kind]
		}
	}
	out.Items, out.Approved = p.Items, true
	out.CardID, out.Title = card.ID, p.Title
	// 这张计划批准之后的 generate 步骤：停后续（MKT-T-27）按它现算。以批准时刻划界 ——
	// 条目键在不同计划里会重复，上一张计划里的余额不足不能停掉这一张。
	since := card.CreatedAt
	if card.SubmittedAt != nil {
		since = *card.SubmittedAt
	}
	steps, err := s.Repo.PlanSteps(ctx, sessionID, since)
	if err != nil {
		return agentapp.SessionPlan{}, err
	}
	out.Steps = agentapp.PlanStepsOf(steps)
	return out, nil
}

// CharacterMedia 把角色引用换成图片 mediaId：媒体原样（归属与类型由受理的输入解析按工作区校验）；
// 资产按工作区取它当前的图，还没有图时报错。
func (s AgentPlanSource) CharacterMedia(ctx context.Context, ws int64, ref agentapp.SceneRef) (int64, error) {
	id, err := strconv.ParseInt(ref.ID, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("角色引用 %q 不合法", ref.ID)
	}
	switch ref.Kind {
	case agentapp.RefMedia:
		return id, nil
	case agentapp.RefAsset:
		if s.Script == nil {
			return 0, errors.New("角色资产暂时无法解析")
		}
		a, err := s.Script.AssetInWorkspace(ctx, ws, id)
		if err != nil {
			return 0, err
		}
		if a.MediaObjectID == 0 {
			return 0, fmt.Errorf("角色资产「%s」还没有图片，请先为它出图", a.Name)
		}
		return a.MediaObjectID, nil
	}
	return 0, fmt.Errorf("角色引用类型 %q 不受支持", ref.Kind)
}

// CreativeRatio 实现 agentapp.CreativeRatioSource：创意**当前版本**的推荐画幅（与「营销输入」里写给模型的
// 推荐同一份，见 AgentSceneContext.summaryOption）。已下线 / 不在时段时 GetOption 报错，由调用方兜底。
func (s AgentPlanSource) CreativeRatio(ctx context.Context, code string) (string, error) {
	if s.Marketing == nil || code == "" {
		return "", nil
	}
	o, err := s.Marketing.GetOption(ctx, code)
	if err != nil {
		return "", err
	}
	rp, err := mktdomain.ParseRecommendedParams(o.RecommendedParams)
	if err != nil {
		return "", err
	}
	return rp.AspectRatio, nil
}

var (
	_ agentapp.PlanSource          = AgentPlanSource{}
	_ agentapp.CreativeRatioSource = AgentPlanSource{}
)
