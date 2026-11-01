package wiring

import (
	"context"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	smartcutapp "github.com/li904338203/tabtv/backend/internal/smartcut/app"
	timelineapp "github.com/li904338203/tabtv/backend/internal/timeline/app"
	timelinedomain "github.com/li904338203/tabtv/backend/internal/timeline/domain"
	timelinerepo "github.com/li904338203/tabtv/backend/internal/timeline/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// Agent / MCP 的智能剪辑端口（docs/agent/smartcut SCT-41）：只做类型转换，规则在 smartcut/app 与 internal/canvasgen。

var _ agentapp.Smartcuts = AgentSmartcuts{}

// errSmartcutUnwired 智能剪辑服务没有装上（零值装配）。
func errSmartcutUnwired() error { return httpx.Errf(errcode.Internal, "智能剪辑服务未接入") }

// AgentSmartcuts 把智能剪辑服务接成 agentapp.Smartcuts（Agent 在 Worker 里跑，用 Worker 装的那一份）。
// Timeline 只用来改新时间线的画幅（按节点拼时给的画幅）。
type AgentSmartcuts struct {
	Svc      *smartcutapp.Service
	Timeline *timelineapp.Service
}

func (a AgentSmartcuts) CreateTimeline(ctx context.Context, ws, projectID int64, nodeID string, source int64) (int64, error) {
	if a.Svc == nil {
		return 0, errSmartcutUnwired()
	}
	sum, _, err := a.Svc.Create(ctx, ws, projectID, nodeID, source)
	return sum.TimelineID, err
}

func (a AgentSmartcuts) TimelineNode(ctx context.Context, ws, projectID, timelineID int64) (string, error) {
	if a.Svc == nil {
		return "", errSmartcutUnwired()
	}
	node, ok, err := a.Svc.TimelineNode(ctx, ws, projectID, timelineID)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", agentapp.ErrSmartcutTimelineNotFound
	}
	return node, nil
}

func (a AgentSmartcuts) BindTimeline(ctx context.Context, ws, projectID, timelineID int64, nodeID string) error {
	if a.Svc == nil {
		return errSmartcutUnwired()
	}
	return a.Svc.BindTimeline(ctx, ws, projectID, timelineID, nodeID)
}

func (a AgentSmartcuts) SetAspectRatio(ctx context.Context, ws, projectID, timelineID int64, aspect string) error {
	if a.Svc == nil || a.Timeline == nil {
		return errSmartcutUnwired()
	}
	w, h, ok := timelinedomain.DefaultSize(aspect)
	if !ok {
		return httpx.Errf(errcode.InvalidArgument, "画幅只能是 16:9 / 9:16 / 1:1 这类固定比例").
			WithDetails(map[string]any{"path": "aspectRatio"})
	}
	if _, err := a.Svc.SummaryOf(ctx, ws, projectID, timelineID); err != nil {
		return err // 不是本项目里绑节点的时间线
	}
	v, err := a.Timeline.Get(ctx, ws, timelineID)
	if err != nil {
		return err
	}
	if v.Timeline.AspectRatio == aspect {
		return nil
	}
	for _, tr := range v.Tracks {
		if len(tr.Clips) > 0 {
			return httpx.Errf(errcode.InvalidStateTransition, "时间线上已经有片段，不在这里改画幅（用 smartcut_plan 指定画幅重新剪）")
		}
	}
	// 编辑者记 sync：只是按要求定下新时间线的画幅，不算「剪过」—— 记成 agent 的话之后连线即入轨就不铺了。
	_, err = a.Timeline.UpdateMeta(ctx, ws, timelineID, v.Timeline.Version,
		timelinerepo.Editor{Kind: timelinerepo.EditorSync}, "", aspect, 0, w, h)
	return err
}

func (a AgentSmartcuts) Sync(ctx context.Context, ws, projectID, timelineID int64) (agentapp.SmartcutSyncOutcome, error) {
	if a.Svc == nil {
		return agentapp.SmartcutSyncOutcome{}, errSmartcutUnwired()
	}
	res, err := a.Svc.Sync(ctx, ws, projectID, timelineID, smartcutapp.SyncAutofill)
	if err != nil {
		return agentapp.SmartcutSyncOutcome{}, err
	}
	sum, err := a.Svc.SummaryOf(ctx, ws, projectID, timelineID)
	if err != nil {
		return agentapp.SmartcutSyncOutcome{}, err
	}
	return agentapp.SmartcutSyncOutcome{Appended: len(res.AppendedClipIDs), Unused: res.UnusedCount,
		Summary: agentSmartcutSummary(sum)}, nil
}

func (a AgentSmartcuts) Detail(ctx context.Context, ws, projectID, timelineID int64) (agentapp.SmartcutDetail, error) {
	if a.Svc == nil {
		return agentapp.SmartcutDetail{}, errSmartcutUnwired()
	}
	d, err := a.Svc.Detail(ctx, ws, projectID, timelineID)
	if err != nil {
		return agentapp.SmartcutDetail{}, err
	}
	out := agentapp.SmartcutDetail{SmartcutSummary: agentSmartcutSummary(d.Summary), Omitted: d.Omitted,
		Inputs: make([]agentapp.SmartcutInput, 0, len(d.Inputs)), Clips: make([]agentapp.SmartcutClip, 0, len(d.Clips))}
	for _, it := range d.Inputs {
		in := agentapp.SmartcutInput{No: it.No, Kind: it.Kind, Status: it.Status, Title: it.Title,
			SourceNodeID: it.SourceNodeID, MediaID: it.MediaID, Used: it.Used, Reason: it.Reason}
		if it.DurationMs != nil {
			in.DurationMs = *it.DurationMs
		}
		out.Inputs = append(out.Inputs, in)
	}
	for _, c := range d.Clips {
		out.Clips = append(out.Clips, agentapp.SmartcutClip{TrackKind: c.TrackKind, TrackName: c.TrackName,
			TrackRole: c.TrackRole, No: c.No, MediaID: c.MediaID, Origin: c.Origin, SourceNodeID: c.SourceNodeID,
			StartMs: c.StartMs, DurationMs: c.DurationMs, Hidden: c.Hidden})
	}
	return out, nil
}

func (a AgentSmartcuts) Plan(ctx context.Context, in agentapp.SmartcutPlanRequest) (agentapp.SmartcutPlanOutcome, error) {
	if a.Svc == nil {
		return agentapp.SmartcutPlanOutcome{}, errSmartcutUnwired()
	}
	res, err := a.Svc.PlanAndApply(ctx, smartcutapp.PlanRequest{WorkspaceID: in.WorkspaceID, ProjectID: in.ProjectID,
		TimelineID: in.TimelineID, Actor: smartcutapp.Actor{UserID: in.UserID, AgentSessionID: in.SessionID},
		MemberLevelID: in.MemberLevelID, Mode: in.Mode, Instruction: in.Instruction, TargetSec: in.TargetSec,
		AspectRatio: in.AspectRatio, Variants: in.Variants})
	if err != nil {
		return agentapp.SmartcutPlanOutcome{}, err
	}
	out := agentapp.SmartcutPlanOutcome{CallID: res.CallID, AppliedVariant: -1, SnapshotID: res.SnapshotID}
	if res.AppliedVariant != nil {
		out.AppliedVariant = *res.AppliedVariant
	}
	for _, v := range res.Variants {
		out.Variants = append(out.Variants, agentapp.SmartcutVariant{Index: v.Index, Status: v.Status,
			Summary: v.Summary, TotalMs: v.TotalMs, Warnings: v.Warnings, UsedFallback: v.UsedFallback, Error: v.Error})
	}
	// 写入之后的摘要：取不到只是少几个数字，方案已经写进去了 —— 不能因此回一个错误让模型再剪一遍。
	if sum, err := a.Svc.SummaryOf(ctx, in.WorkspaceID, in.ProjectID, in.TimelineID); err == nil {
		out.Summary = agentSmartcutSummary(sum)
	}
	return out, nil
}

func agentSmartcutSummary(s smartcutapp.Summary) agentapp.SmartcutSummary {
	out := agentapp.SmartcutSummary{TimelineID: s.TimelineID, Version: s.Version, IsArranged: s.IsArranged,
		ClipCount: s.ClipCount, TotalMs: s.TotalMs, EffectiveMs: s.EffectiveMs, InputsStale: s.InputsStale,
		RenderStale: s.RenderStale}
	if r := s.LatestRender; r != nil {
		out.LatestRender = &agentapp.SmartcutRender{ID: r.ID, Status: r.Status, Progress: r.Progress, Stage: r.Stage,
			OutputMediaID: r.OutputMediaID, Error: r.ErrorMessage}
	}
	if c := s.ActiveCall; c != nil {
		out.ActiveCall = &agentapp.SmartcutActiveCall{ID: c.ID, Kind: c.Kind, Stage: c.Stage}
	}
	return out
}
