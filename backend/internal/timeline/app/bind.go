package app

import (
	"context"
	"errors"

	"github.com/li904338203/tabtv/backend/internal/timeline/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 智能剪辑节点的时间线（docs/agent/smartcut SCT-07）：建、分叉、按节点查。项目可写与否由调用方（smartcut）判。

// 新建智能剪辑时间线的默认格式：与节点参数的默认画幅一致（seed：9:16、1080P，dev/02 R-5）。
const (
	smartcutDefaultName   = "智能剪辑"
	smartcutDefaultAspect = "9:16"
	smartcutDefaultFPS    = 30
)

// NodeTimelineInput 为画布节点建时间线。SourceTimelineID 非 0 时分叉那一条（复制、粘贴出来的节点）。
type NodeTimelineInput struct {
	WorkspaceID      int64
	ProjectID        int64
	NodeID           string
	SourceTimelineID int64
}

// CreateForNode 按（项目，节点）幂等：已有有效时间线时返回它与 false。
//
// 分叉时沿用来源的名称、画幅、剪过与否与素材指纹：复制出来的节点与原节点「剪过的同一份东西」，
// 新节点的输入不一样时会被判成「素材有变化」，由用户决定替不替换，而不是被自动铺轨冲掉。
// 来源必须在同一工作区（查询带工作区条件），不存在与不是你的回同一个 404。
func (s *Service) CreateForNode(ctx context.Context, in NodeTimelineInput) (repo.Timeline, bool, error) {
	if existing, err := s.repo.GetByNode(ctx, in.WorkspaceID, in.ProjectID, in.NodeID); err == nil {
		return existing, false, nil
	} else if !errors.Is(err, repo.ErrNotFound) {
		return repo.Timeline{}, false, httpx.Wrap(errcode.Internal, err)
	}
	w, h := defaultSize(smartcutDefaultAspect)
	t := repo.Timeline{
		WorkspaceID: in.WorkspaceID, ProjectID: in.ProjectID, Name: smartcutDefaultName,
		AspectRatio: smartcutDefaultAspect, FPS: smartcutDefaultFPS, Width: w, Height: h,
		CanvasNodeID: in.NodeID, IsArranged: false,
	}
	if in.SourceTimelineID == 0 {
		got, created, err := s.repo.CreateForNode(ctx, t)
		if err != nil {
			return repo.Timeline{}, false, httpx.Wrap(errcode.Internal, err)
		}
		return got, created, nil
	}
	src, err := s.repo.Get(ctx, in.WorkspaceID, in.SourceTimelineID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return repo.Timeline{}, false, httpx.Errf(errcode.NotFound, "要复制的时间线不存在")
		}
		return repo.Timeline{}, false, httpx.Wrap(errcode.Internal, err)
	}
	t.Name, t.AspectRatio, t.FPS, t.Width, t.Height = src.Name, src.AspectRatio, src.FPS, src.Width, src.Height
	t.IsArranged, t.PlannedFingerprint, t.SyncedFingerprint = src.IsArranged, src.PlannedFingerprint, src.SyncedFingerprint
	got, created, err := s.repo.ForkForNode(ctx, src.ID, t)
	if err != nil {
		return repo.Timeline{}, false, httpx.Wrap(errcode.Internal, err)
	}
	return got, created, nil
}

// SourceProject 分叉来源所在的项目（调用方据此判来源项目是否还可读）。
func (s *Service) SourceProject(ctx context.Context, workspaceID, timelineID int64) (int64, error) {
	t, err := s.repo.Get(ctx, workspaceID, timelineID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return 0, httpx.Errf(errcode.NotFound, "要复制的时间线不存在")
		}
		return 0, httpx.Wrap(errcode.Internal, err)
	}
	return t.ProjectID, nil
}

// TimelineNode 本项目里一条有效时间线绑定的节点 ID（按节点自愈用）。ok=false 表示不在本项目或没绑节点。
func (s *Service) TimelineNode(ctx context.Context, workspaceID, projectID, timelineID int64) (string, bool, error) {
	node, err := s.repo.TimelineNodeIn(ctx, workspaceID, projectID, timelineID)
	if errors.Is(err, repo.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, httpx.Wrap(errcode.Internal, err)
	}
	return node, true, nil
}

// NodeTimelines 本项目全部绑定节点的时间线（含最近导出）与它们的画面轨、第一条音频轨片段（摘要用）。
func (s *Service) NodeTimelines(ctx context.Context, workspaceID, projectID int64) (
	[]repo.NodeTimeline, map[int64]repo.StripClips, error) {
	list, err := s.repo.NodeTimelines(ctx, workspaceID, projectID)
	if err != nil {
		return nil, nil, httpx.Wrap(errcode.Internal, err)
	}
	ids := make([]int64, 0, len(list))
	for _, t := range list {
		ids = append(ids, t.ID)
	}
	clips, err := s.repo.StripClipsOf(ctx, ids)
	if err != nil {
		return nil, nil, httpx.Wrap(errcode.Internal, err)
	}
	return list, clips, nil
}

// BindNode 把一条还没绑节点的时间线绑到画布节点上（SCT-41，SCD-39：Agent 装配出来的脚本时间线要在画布上看得到）。
// 已经绑在这个节点上的算成功；绑在别的节点上、或节点已有时间线回 40902；不在本项目回 404。
func (s *Service) BindNode(ctx context.Context, workspaceID, projectID, timelineID int64, nodeID string) error {
	err := s.repo.BindNode(ctx, workspaceID, projectID, timelineID, nodeID)
	switch {
	case errors.Is(err, repo.ErrNotFound):
		return httpx.Errf(errcode.NotFound, "时间线不存在")
	case errors.Is(err, repo.ErrBindConflict):
		return httpx.Errf(errcode.VersionConflict, "这条时间线已经属于别的智能剪辑节点，或这个节点已经有自己的时间线")
	case err != nil:
		return httpx.Wrap(errcode.Internal, err)
	}
	return nil
}

// NodeTimelineID 节点已绑定的有效时间线；没有时 ok=false。
func (s *Service) NodeTimelineID(ctx context.Context, workspaceID, projectID int64, nodeID string) (int64, bool, error) {
	t, err := s.repo.GetByNode(ctx, workspaceID, projectID, nodeID)
	if errors.Is(err, repo.ErrNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, httpx.Wrap(errcode.Internal, err)
	}
	return t.ID, true, nil
}
