package app

import (
	"context"
	"errors"
	"strconv"

	"github.com/li904338203/tabtv/backend/internal/smartcut/domain"
	timelinedomain "github.com/li904338203/tabtv/backend/internal/timeline/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// Agent / MCP 的智能剪辑（docs/agent/smartcut SCT-41，SC-F-160~166）：只读详情、免费的 AI 剪辑直接写入、把脚本时间线绑到节点。
// 建节点、连线、按节点取时间线（自愈）在 internal/canvasgen，与网页同一套规则；这里只有时间线这一侧。

// BindTimeline 把一条还没绑节点的时间线绑到画布节点上（Agent 的 timeline_assemble，SCD-39）：已经绑在这个节点上的算成功。
func (s *Service) BindTimeline(ctx context.Context, workspaceID, projectID, timelineID int64, nodeID string) error {
	if err := s.projectWritable(ctx, workspaceID, projectID, "绑定智能剪辑"); err != nil {
		return err
	}
	return s.timelines.BindNode(ctx, workspaceID, projectID, timelineID, nodeID)
}

// SummaryOf 一条绑定节点的时间线的摘要（Agent / MCP 写入之后回报版本、段数、时长）。不是智能剪辑的时间线回 404。
func (s *Service) SummaryOf(ctx context.Context, workspaceID, projectID, timelineID int64) (Summary, error) {
	return s.summaryOf(ctx, workspaceID, projectID, timelineID)
}

// Detail 智能剪辑的只读详情（smartcut_get，SC-F-161）：摘要（时长、过期、导出状态、进行中的 AI 剪辑）、素材条、片段清单。
type Detail struct {
	Summary Summary
	Inputs  []domain.Input
	Omitted int
	Clips   []ClipBrief
}

// ClipBrief 片段清单的一行（SC-F-161）：哪条轨、第几段（每条轨从 1 起）、什么素材、来源节点、成片起点与时长。
type ClipBrief struct {
	ID        string
	TrackKind string
	TrackName string
	// TrackRole 音频轨的用途（music / voice / sfx），其余为空。
	TrackRole    string
	No           int
	MediaID      int64
	Origin       string
	SourceNodeID string
	StartMs      int
	DurationMs   int
	// Hidden 跟随画面的音效被裁到看不见了（不进成片，SC-F-56）。
	Hidden bool
}

// Detail 取一条绑定节点的时间线的只读详情。回收站里的项目也能读；不是智能剪辑的时间线回 404。
func (s *Service) Detail(ctx context.Context, workspaceID, projectID, timelineID int64) (Detail, error) {
	sum, err := s.summaryOf(ctx, workspaceID, projectID, timelineID)
	if err != nil {
		return Detail{}, err
	}
	inputs, err := s.Inputs(ctx, workspaceID, projectID, timelineID)
	if err != nil {
		return Detail{}, err
	}
	content, err := s.timelines.Content(ctx, workspaceID, timelineID)
	if err != nil {
		return Detail{}, err
	}
	return Detail{Summary: sum, Inputs: inputs.Items, Omitted: inputs.Omitted, Clips: clipBriefs(content)}, nil
}

// clipBriefs 片段清单：成片起点与时长用时间线域的同一套算法（Layout，SCK-15），画面轨的落点由服务端算。
func clipBriefs(c timelinedomain.Content) []ClipBrief {
	lay := timelinedomain.Layout(c)
	out := []ClipBrief{}
	for _, t := range c.Tracks {
		for i, cl := range t.Clips {
			pl := lay.Clips[cl.ID]
			b := ClipBrief{ID: cl.ID, TrackKind: t.Kind, TrackName: t.Name, TrackRole: t.Role, No: i + 1,
				MediaID: cl.MediaID, Origin: cl.Origin, SourceNodeID: cl.SourceNodeID, StartMs: pl.StartMs,
				DurationMs: pl.DurationMs}
			if pl.Follow != nil && pl.Follow.Hidden {
				b.Hidden = true
			}
			out = append(out, b)
		}
	}
	return out
}

// PlanAndApply 是 Agent / MCP 发起的 AI 剪辑（SC-F-162）：以此刻的版本为底剪，结果直接写入并存快照，来源记 agent / mcp。
//
// 写入时版本冲突（模型思考的这几分钟里用户在剪辑台里改过）：**重读、按刚出的同一套方案重新编译写入一次**，
// 不再调模型（方案是对要求的回答，与时间线版本无关；同导演台 BuildAndApply）；仍冲突就把冲突返回给调用方，
// 不强行覆盖用户的改动。发起时版本就对不上（读版本与发起之间被改过，窗口很小）同样重读、重发一次。
func (s *Service) PlanAndApply(ctx context.Context, req PlanRequest) (PlanResult, error) {
	if req.Actor.AgentSessionID == 0 && req.Actor.McpGrantID == 0 {
		return PlanResult{}, httpx.Errf(errcode.InvalidArgument, "只有 Agent / MCP 才能直接写入 AI 剪辑的结果")
	}
	if err := s.planReady(); err != nil {
		return PlanResult{}, err
	}
	req.Force = false
	for attempt := 0; ; attempt++ {
		base, err := s.plan.Timelines.Content(ctx, req.WorkspaceID, req.TimelineID)
		if err != nil {
			return PlanResult{}, err
		}
		req.BaseVersion = base.Version
		res, err := s.Plan(ctx, req)
		if err == nil || !isVersionConflict(err) || attempt > 0 {
			return res, err
		}
		callID, variant, ok := conflictCall(err)
		if !ok {
			continue // 发起时版本就对不上：模型还没调，重读之后重发一次
		}
		cur, rerr := s.plan.Timelines.Content(ctx, req.WorkspaceID, req.TimelineID)
		if rerr != nil {
			return PlanResult{}, rerr
		}
		return s.ApplyVariant(ctx, ApplyRequest{WorkspaceID: req.WorkspaceID, ProjectID: req.ProjectID,
			TimelineID: req.TimelineID, CallID: callID, Variant: variant, BaseVersion: cur.Version, Actor: req.Actor})
	}
}

// conflictCall 取写入冲突里的调用记录与本来要写入的那一套（Plan 在模型出完方案之后撞上冲突时带上的）。
func conflictCall(err error) (int64, int, bool) {
	var be *httpx.BizError
	if !errors.As(err, &be) {
		return 0, 0, false
	}
	raw, _ := be.Details["callId"].(string)
	callID, perr := strconv.ParseInt(raw, 10, 64)
	variant, ok := be.Details["variant"].(int)
	if perr != nil || callID <= 0 || !ok {
		return 0, 0, false
	}
	return callID, variant, true
}
