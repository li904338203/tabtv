package wiring

import (
	"context"
	"errors"

	"github.com/li904338203/tabtv/backend/internal/canvasgen"
	mcpapp "github.com/li904338203/tabtv/backend/internal/mcp/app"
	smartcutapp "github.com/li904338203/tabtv/backend/internal/smartcut/app"
	timelineapp "github.com/li904338203/tabtv/backend/internal/timeline/app"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// MCP 的智能剪辑端口（docs/agent/smartcut SCT-41，SC-F-165）：建节点、连线、按节点取时间线走 internal/canvasgen
// （与 Agent 同一套），AI 剪辑与详情走 smartcut/app，导出走时间线域同一条路径（与网页的「导出」一致）。

var _ mcpapp.Smartcuts = MCPSmartcuts{}

// MCPSmartcuts 实现 mcpapp.Smartcuts（API 进程）。租户、操作者、会员等级一律取 Principal；来源 = 本授权。
type MCPSmartcuts struct {
	// Orch 只用到画布（建节点 / 读节点 / 连线 / 写回），不报价、不受理。
	Orch     *canvasgen.Orchestrator
	Svc      *smartcutapp.Service
	Timeline *timelineapp.Service
}

func (a MCPSmartcuts) timelines() canvasgen.SmartcutTimelines { return CanvasgenSmartcuts{Svc: a.Svc} }

func (a MCPSmartcuts) ready() error {
	if a.Orch == nil || a.Svc == nil || a.Timeline == nil {
		return errSmartcutUnwired()
	}
	return nil
}

func (a MCPSmartcuts) Create(ctx context.Context, p mcpapp.Principal, in mcpapp.SmartcutCreateInput) (
	mcpapp.SmartcutCreated, error) {
	if err := a.ready(); err != nil {
		return mcpapp.SmartcutCreated{}, err
	}
	c := mcpCaller(p, in.ProjectID, in.BatchID)
	// 先校验要连的节点再建节点：给错一个 ID 就留下一个空节点的话，客户端重试时会再建一个。
	if len(in.InputNodeIDs) > 0 {
		v, err := a.Orch.Canvas.Read(ctx, p.WorkspaceID, in.ProjectID)
		if err != nil {
			return mcpapp.SmartcutCreated{}, err
		}
		if err := canvasgen.CheckSmartcutInputs(v, "", in.InputNodeIDs); err != nil {
			return mcpapp.SmartcutCreated{}, smartcutInputErr(err)
		}
	}
	nodeID, tid, err := canvasgen.CreateSmartcutNode(ctx, *a.Orch, c,
		canvasgen.SmartcutNodeInput{Title: in.Title, Position: in.Position}, a.timelines())
	if err != nil {
		return mcpapp.SmartcutCreated{}, err
	}
	out := mcpapp.SmartcutCreated{NodeID: nodeID, TimelineID: tid, Connected: []string{}, Existing: []string{}}
	if len(in.InputNodeIDs) > 0 {
		conn, err := canvasgen.ConnectSmartcutInputs(ctx, *a.Orch, c, nodeID, in.InputNodeIDs)
		if err != nil {
			return mcpapp.SmartcutCreated{}, httpx.Errf(errcode.Internal,
				"智能剪辑节点 "+nodeID+" 已建好，但把素材节点连进来失败："+err.Error()).
				WithDetails(map[string]any{"nodeId": nodeID})
		}
		out.Connected, out.Existing = conn.Added, conn.Existing
		res, err := a.Svc.Sync(ctx, p.WorkspaceID, in.ProjectID, tid, smartcutapp.SyncAutofill)
		if err != nil {
			// 连线已经在画布上了：铺轨失败的话，用户打开剪辑台时会再同步一次，不能让客户端因此再建一个节点。
			out.SyncError = "素材已连上，但铺上时间线失败（用户打开剪辑台时会再铺一次）：" + err.Error()
		} else {
			out.Appended = len(res.AppendedClipIDs)
		}
	}
	if sum, err := a.Svc.SummaryOf(ctx, p.WorkspaceID, in.ProjectID, tid); err == nil {
		out.Summary = mcpSmartcutSummary(sum)
	}
	return out, nil
}

func (a MCPSmartcuts) Get(ctx context.Context, p mcpapp.Principal, projectID int64, nodeID string) (
	mcpapp.SmartcutDetail, error) {
	if err := a.ready(); err != nil {
		return mcpapp.SmartcutDetail{}, err
	}
	tid, ok, err := canvasgen.SmartcutBoundTimeline(ctx, *a.Orch, mcpCaller(p, projectID, ""), nodeID, a.timelines())
	if err != nil {
		return mcpapp.SmartcutDetail{}, smartcutNodeErr(nodeID, err)
	}
	if !ok {
		return mcpapp.SmartcutDetail{NodeID: nodeID}, nil
	}
	d, err := a.Svc.Detail(ctx, p.WorkspaceID, projectID, tid)
	if err != nil {
		return mcpapp.SmartcutDetail{}, err
	}
	out := mcpapp.SmartcutDetail{NodeID: nodeID, HasTimeline: true, SmartcutSummary: mcpSmartcutSummary(d.Summary),
		Omitted: d.Omitted, Inputs: make([]mcpapp.SmartcutInput, 0, len(d.Inputs)),
		Clips: make([]mcpapp.SmartcutClip, 0, len(d.Clips))}
	for _, it := range d.Inputs {
		in := mcpapp.SmartcutInput{No: it.No, Kind: it.Kind, Status: it.Status, Title: it.Title,
			SourceNodeID: it.SourceNodeID, Used: it.Used, Reason: it.Reason}
		if it.DurationMs != nil {
			in.DurationMs = *it.DurationMs
		}
		out.Inputs = append(out.Inputs, in)
	}
	for _, c := range d.Clips {
		out.Clips = append(out.Clips, mcpapp.SmartcutClip{TrackKind: c.TrackKind, TrackRole: c.TrackRole, No: c.No,
			MediaID: c.MediaID, Origin: c.Origin, SourceNodeID: c.SourceNodeID, StartMs: c.StartMs,
			DurationMs: c.DurationMs, Hidden: c.Hidden})
	}
	return out, nil
}

func (a MCPSmartcuts) Plan(ctx context.Context, p mcpapp.Principal, in mcpapp.SmartcutPlanInput) (
	mcpapp.SmartcutPlanResult, error) {
	if err := a.ready(); err != nil {
		return mcpapp.SmartcutPlanResult{}, err
	}
	tid, repair, err := canvasgen.SmartcutTimelineOfNode(ctx, *a.Orch, mcpCaller(p, in.ProjectID, in.BatchID),
		in.NodeID, a.timelines())
	if err != nil {
		return mcpapp.SmartcutPlanResult{}, smartcutNodeErr(in.NodeID, err)
	}
	res, err := a.Svc.PlanAndApply(ctx, smartcutapp.PlanRequest{WorkspaceID: p.WorkspaceID, ProjectID: in.ProjectID,
		TimelineID: tid, Actor: smartcutapp.Actor{UserID: p.UserID, McpGrantID: p.GrantID},
		MemberLevelID: p.MemberLevelID, Mode: in.Mode, Instruction: in.Instruction, TargetSec: in.TargetSec,
		AspectRatio: in.AspectRatio, Variants: in.Variants})
	if err != nil {
		return mcpapp.SmartcutPlanResult{}, err
	}
	out := mcpapp.SmartcutPlanResult{TimelineID: tid, CallID: res.CallID, AppliedVariant: -1,
		SnapshotID: res.SnapshotID, Repair: repair}
	if res.AppliedVariant != nil {
		out.AppliedVariant = *res.AppliedVariant
	}
	for _, v := range res.Variants {
		out.Variants = append(out.Variants, mcpapp.SmartcutVariant{Index: v.Index, Status: v.Status,
			Summary: v.Summary, TotalMs: v.TotalMs, Warnings: v.Warnings, UsedFallback: v.UsedFallback, Error: v.Error})
	}
	// 写入之后的摘要取不到只是少几个数字：方案已经写进去了，不能因此回错误让客户端再剪一遍。
	if sum, err := a.Svc.SummaryOf(ctx, p.WorkspaceID, in.ProjectID, tid); err == nil {
		out.Summary = mcpSmartcutSummary(sum)
	}
	return out, nil
}

// boundTimeline 导出用：节点自己的时间线（只读取，不补建 —— 一条空时间线没有可导出的东西）。
func (a MCPSmartcuts) boundTimeline(ctx context.Context, p mcpapp.Principal, projectID int64, nodeID string) (int64, error) {
	tid, ok, err := canvasgen.SmartcutBoundTimeline(ctx, *a.Orch, mcpCaller(p, projectID, ""), nodeID, a.timelines())
	if err != nil {
		return 0, smartcutNodeErr(nodeID, err)
	}
	if !ok {
		return 0, httpx.Errf(errcode.InvalidStateTransition, "智能剪辑 "+nodeID+" 还没有自己的时间线：先用 smartcut_plan 剪一次，"+
			"或请用户在 seeAitv 里打开剪辑台")
	}
	return tid, nil
}

func (a MCPSmartcuts) renderInput(p mcpapp.Principal, tid int64, spec mcpapp.SmartcutExportSpec, dry bool,
	key string) timelineapp.RenderInput {
	return timelineapp.RenderInput{WorkspaceID: p.WorkspaceID, PayerAccountID: p.PayerAccountID, UserID: p.UserID,
		TimelineID: tid, MemberLevelID: p.MemberLevelID, DryRun: dry, IdempotencyKey: key,
		Resolution: spec.Resolution, FPS: spec.FPS}
}

func (a MCPSmartcuts) PrecheckExport(ctx context.Context, p mcpapp.Principal, projectID int64, nodeID string,
	spec mcpapp.SmartcutExportSpec) (int64, mcpapp.RenderPrecheck, error) {
	if err := a.ready(); err != nil {
		return 0, mcpapp.RenderPrecheck{}, err
	}
	tid, err := a.boundTimeline(ctx, p, projectID, nodeID)
	if err != nil {
		return 0, mcpapp.RenderPrecheck{}, err
	}
	pre, _, _, err := a.Timeline.PrecheckRender(ctx, a.renderInput(p, tid, spec, true, ""))
	if err != nil {
		return 0, mcpapp.RenderPrecheck{}, err
	}
	out := mcpapp.RenderPrecheck{DurationMs: pre.DurationMs, Credits: pre.Credits}
	for _, is := range pre.Issues {
		out.Issues = append(out.Issues, mcpapp.RenderIssue{Code: is.Code, Detail: is.Detail, ClipIdx: is.ClipIdx})
	}
	return tid, out, nil
}

func (a MCPSmartcuts) StartExport(ctx context.Context, p mcpapp.Principal, projectID int64, nodeID string,
	spec mcpapp.SmartcutExportSpec, idemKey string) (mcpapp.RenderView, error) {
	if err := a.ready(); err != nil {
		return mcpapp.RenderView{}, err
	}
	tid, err := a.boundTimeline(ctx, p, projectID, nodeID)
	if err != nil {
		return mcpapp.RenderView{}, err
	}
	j, _, err := a.Timeline.StartRender(ctx, a.renderInput(p, tid, spec, false, idemKey))
	if err != nil {
		return mcpapp.RenderView{}, err
	}
	return mcpRenderView(j.ID, j.TimelineID, j.Status, j.Progress, j.Stage, j.CreditsCost, j.OutputMediaID,
		j.ErrorMessage), nil
}

func mcpSmartcutSummary(s smartcutapp.Summary) mcpapp.SmartcutSummary {
	out := mcpapp.SmartcutSummary{TimelineID: s.TimelineID, Version: s.Version, IsArranged: s.IsArranged,
		ClipCount: s.ClipCount, TotalMs: s.TotalMs, EffectiveMs: s.EffectiveMs, InputsStale: s.InputsStale,
		RenderStale: s.RenderStale}
	if r := s.LatestRender; r != nil {
		out.LatestRender = &mcpapp.SmartcutRender{ID: r.ID, Status: r.Status, Progress: r.Progress, Stage: r.Stage,
			OutputMediaID: r.OutputMediaID, Error: r.ErrorMessage}
	}
	if c := s.ActiveCall; c != nil {
		out.ActiveCallKind, out.ActiveCallStage = c.Kind, c.Stage
	}
	return out
}

// smartcutNodeErr 按节点取时间线的错误说清楚：节点不存在 404、不是智能剪辑 400；其余（含业务错误）原样返回。
func smartcutNodeErr(nodeID string, err error) error {
	var ns *canvasgen.NotSmartcutError
	switch {
	case errors.Is(err, canvasgen.ErrNodeNotFound):
		return httpx.Errf(errcode.NotFound, "画布上没有节点 "+nodeID+"：先用 canvas_read 确认，或用 smartcut_create 新建一个智能剪辑")
	case errors.As(err, &ns):
		return httpx.Errf(errcode.InvalidArgument, "节点 "+nodeID+" 是 "+ns.Type+"，不是智能剪辑")
	}
	return err
}

// smartcutInputErr 要连的节点不合适：400 并带上是哪个节点。
func smartcutInputErr(err error) error {
	var ie *canvasgen.SmartcutInputError
	if errors.As(err, &ie) {
		return httpx.Errf(errcode.InvalidArgument, ie.Reason+"（先用 canvas_read 确认节点 ID 与类型）").
			WithDetails(map[string]any{"path": "inputNodeIds", "nodeId": ie.NodeID})
	}
	return err
}
