package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/li904338203/tabtv/backend/internal/script/domain"
	"github.com/li904338203/tabtv/backend/internal/script/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 画布 Agent（AGT-50）在付费步骤之前要先报价：资产批量出图、分镜图组（建组即提交）、组执行
// 这三处没有 dryRun 入口。这里给出「真正提交时会发出去的那些请求」，**只读不写**，
// 报价由调用方逐项问目录 —— 与受理同一组参数，报的价与扣的钱是同一个口径。
//
// ⚠️ 不新增业务规则：跳过哪些项（PlanGroupRun / PlanBatch）、每项的操作码 / 时长 / 输入
// （itemGenRequest）都是提交路径上的同一个函数。

// GroupItemPlan 是一项会被提交的子项：受理请求（幂等键是基础键）与镜号。
type GroupItemPlan struct {
	ItemID  int64
	ShotNo  int
	Request GroupGenRequest
}

// GroupExecutionPlan 是一次组执行（或建组即提交）会提交哪些项、跳过哪些项（镜号 + 原因）。
type GroupExecutionPlan struct {
	Items   []GroupItemPlan
	Skipped []string
}

// PlanGroupExecution 预演一次 ExecuteGroup：不改组状态、不受理、不扣费。
func (s *Service) PlanGroupExecution(ctx context.Context, in ExecuteGroupInput) (GroupExecutionPlan, error) {
	g, err := s.repo.GetGroup(ctx, in.WorkspaceID, in.GroupID)
	if err != nil {
		return GroupExecutionPlan{}, translateGroup(err)
	}
	if err := domain.CanExecuteGroup(domain.GroupType(g.GroupType),
		domain.GroupStatus(g.Status)); err != nil {
		return GroupExecutionPlan{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	if in.ModelCode == "" {
		return GroupExecutionPlan{}, httpx.Errf(errcode.InvalidArgument, "缺少模型")
	}
	items, err := s.repo.ListItems(ctx, g.ID)
	if err != nil {
		return GroupExecutionPlan{}, httpx.Wrap(errcode.Internal, err)
	}
	picked := filterItems(items, in.ItemIDs)
	return s.planItems(ctx, g, picked, domain.PlanGroupRun(toBatchItems(picked), in.Force), in)
}

// PlanStoryboardImageGroup 预演「建分镜图组」会立即提交的那些项（建组即提交，AV-23）。
// 该画布节点上已经有组时（CreateGroup 会直接返回那一个、不再提交）返回空计划。
func (s *Service) PlanStoryboardImageGroup(ctx context.Context, in CreateGroupInput) (GroupExecutionPlan, error) {
	if in.GroupType != domain.GroupStoryboardImage {
		return GroupExecutionPlan{}, nil
	}
	if in.CanvasNodeID == "" {
		return GroupExecutionPlan{}, httpx.Errf(errcode.InvalidArgument, "缺少画布节点 ID")
	}
	sc, err := s.repo.Get(ctx, in.WorkspaceID, in.ScriptID)
	if err != nil {
		return GroupExecutionPlan{}, translate(err)
	}
	if _, err := s.repo.GetGroupByNode(ctx, in.WorkspaceID, sc.ProjectID, in.CanvasNodeID); err == nil {
		return GroupExecutionPlan{}, nil
	} else if !errors.Is(err, repo.ErrGroupNotFound) {
		return GroupExecutionPlan{}, httpx.Wrap(errcode.Internal, err)
	}
	// 与 CreateGroup 同一个 shotItems：只收提示词已就绪、引用资产都有设定图的镜头。
	items, err := s.shotItems(ctx, 0, in)
	if err != nil {
		return GroupExecutionPlan{}, err
	}
	g := repo.GroupNode{WorkspaceID: in.WorkspaceID, ProjectID: sc.ProjectID, CanvasNodeID: in.CanvasNodeID,
		GroupType: string(in.GroupType), ScriptID: in.ScriptID, Status: string(domain.GroupDraft)}
	planned := domain.PlanGroupRun(toBatchItems(items), false)
	return s.planItems(ctx, g, items, planned, ExecuteGroupInput{
		WorkspaceID: in.WorkspaceID, PayerAccountID: in.PayerAccountID, UserID: in.UserID,
		ModelCode: in.ModelCode, DurationSec: in.DurationSec,
	})
}

func (s *Service) planItems(ctx context.Context, g repo.GroupNode, picked []repo.GroupItem,
	planned []domain.BatchItem, in ExecuteGroupInput) (GroupExecutionPlan, error) {
	shotNo := map[int64]int{}
	if shots, err := s.repo.ListShots(ctx, g.ScriptID); err == nil {
		for _, sh := range shots {
			shotNo[sh.ID] = sh.ShotNo
		}
	}
	byID := map[int64]repo.GroupItem{}
	for _, it := range picked {
		byID[it.ID] = it
	}
	var out GroupExecutionPlan
	for _, p := range planned {
		it := byID[p.ItemID]
		label := fmt.Sprintf("第 %d 项", it.ItemNo)
		if n := shotNo[it.ShotID]; n > 0 {
			label = fmt.Sprintf("第 %d 镜", n)
		}
		if p.Skip {
			out.Skipped = append(out.Skipped, label+"："+p.Reason)
			continue
		}
		req, err := s.itemGenRequest(ctx, g, it, in)
		if err != nil {
			// 提交时这一项会被标 failed 而不扣费；报价时同样不计入，并把原因说出来。
			out.Skipped = append(out.Skipped, label+"："+err.Error())
			continue
		}
		out.Items = append(out.Items, GroupItemPlan{ItemID: it.ID, ShotNo: shotNo[it.ShotID], Request: req})
	}
	return out, nil
}

// PendingAsset 是一个会被提交出图的资产与它**真正会发出去的**提示词（报价按它算，per_kchar 时才对得上）。
type PendingAsset struct {
	Asset  repo.Asset
	Prompt string
}

// PendingAssets 批量出图会真正提交的资产（已就绪的跳过，同 PlanBatch 的判定）。AssetIDs 为空 = 全部。
func (s *Service) PendingAssets(ctx context.Context, workspaceID, scriptID int64, assetIDs []int64) (
	[]PendingAsset, error) {
	if _, err := s.repo.Get(ctx, workspaceID, scriptID); err != nil {
		return nil, translate(err)
	}
	all, err := s.repo.ListAssetsByScript(ctx, scriptID)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	var out []PendingAsset
	for _, a := range filterAssets(all, assetIDs) {
		d := toDomainAsset(a)
		if !d.IsReady() {
			out = append(out, PendingAsset{Asset: a, Prompt: domain.AssetPrompt(d)})
		}
	}
	return out, nil
}
