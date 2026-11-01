package wiring

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	canvasapp "github.com/li904338203/tabtv/backend/internal/canvas/app"
	canvasdomain "github.com/li904338203/tabtv/backend/internal/canvas/domain"
	"github.com/li904338203/tabtv/backend/internal/canvasgen"
	genrepo "github.com/li904338203/tabtv/backend/internal/generation/repo"
	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	smartcutapp "github.com/li904338203/tabtv/backend/internal/smartcut/app"
	smartcutdomain "github.com/li904338203/tabtv/backend/internal/smartcut/domain"
	timelineapp "github.com/li904338203/tabtv/backend/internal/timeline/app"
	timelinedomain "github.com/li904338203/tabtv/backend/internal/timeline/domain"
	timelinerepo "github.com/li904338203/tabtv/backend/internal/timeline/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
	workflowapp "github.com/li904338203/tabtv/backend/internal/workflow/app"
)

// 智能剪辑的装配（docs/agent/smartcut SCT-07）：时间线、项目、媒体都经端口接给 smartcut/app，
// 智能剪辑不 import 别的域的 app / repo（08 §1）。适配器写在组装根。

// SmartcutInput 是 NewSmartcutService 的入参。
type SmartcutInput struct {
	Timeline *timelineapp.Service
	Canvas   *canvasapp.Service
	Media    *mediaapp.Service
	// Generations 回查素材条里的生成（SCT-25）：同项目、真实状态、全部产物。
	Generations *genrepo.Repo
	Log         *slog.Logger
	// Plan AI 剪辑的装配（SCT-27）。为 nil 时 /plan 回「AI 剪辑链路未接入」。
	Plan *SmartcutPlanInput
	// Speech 语音识别的装配（SCT-33）。为 nil 时 /transcribe 回「语音识别链路未接入」。
	Speech *SmartcutSpeechInput
	// Clip 片段级生成的装配（SCT-37）。为 nil 时重拍、补镜回「片段级生成链路未接入」，回写跳过。
	Clip *SmartcutClipInput
}

// NewSmartcutService 装出智能剪辑服务。
func NewSmartcutService(in SmartcutInput) *smartcutapp.Service {
	svc := smartcutapp.New(smartcutTimelines{in.Timeline}, smartcutProjects{in.Canvas}, smartcutMedia{in.Media})
	svc.SetInputPorts(smartcutapp.InputPorts{
		Canvas:    smartcutCanvas{in.Canvas},
		Facts:     smartcutFacts{gens: in.Generations, media: in.Media},
		Timelines: smartcutSyncTimelines{in.Timeline},
		Analysis:  in.Media,
		Log:       in.Log,
	})
	if in.Plan != nil {
		svc.SetPlanPorts(SmartcutPlanPorts(*in.Plan))
	}
	if in.Speech != nil {
		sp := SmartcutSpeechPorts(*in.Speech)
		sp.Log = in.Log
		svc.SetTranscribePorts(sp)
	}
	if in.Clip != nil {
		cp := SmartcutClipPorts(*in.Clip)
		cp.Log = in.Log
		svc.SetClipToolPorts(cp)
	}
	return svc
}

// ---------------------------------------------------------------- 输入同步（SCT-25）

type smartcutCanvas struct{ svc *canvasapp.Service }

func (a smartcutCanvas) Sources(ctx context.Context, ws, projectID int64, nodeID string) ([]smartcutdomain.Source, error) {
	raw, err := a.svc.SavedCanvas(ctx, ws, projectID)
	if err != nil {
		return nil, err
	}
	sources, err := SmartcutSources(raw, nodeID)
	if err != nil {
		return nil, fmt.Errorf("%w：%v", smartcutapp.ErrCanvasUnreadable, err)
	}
	return sources, nil
}

type smartcutFacts struct {
	gens  *genrepo.Repo
	media *mediaapp.Service
}

func (a smartcutFacts) Generations(ctx context.Context, ws, projectID int64, ids []int64) (
	map[int64]smartcutapp.GenerationInfo, error) {
	rows, err := a.gens.InputFacts(ctx, ws, projectID, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]smartcutapp.GenerationInfo, len(rows))
	for id, f := range rows {
		out[id] = smartcutapp.GenerationInfo{Status: f.Status, Operation: f.Operation, ErrorMessage: f.ErrorMessage,
			OutputMediaIDs: f.OutputMediaIDs}
	}
	return out, nil
}

func (a smartcutFacts) Media(ctx context.Context, ws int64, ids []int64) (map[int64]smartcutapp.MediaInfo, error) {
	rows, err := a.media.InputFacts(ctx, ws, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]smartcutapp.MediaInfo, len(rows))
	for id, f := range rows {
		out[id] = smartcutapp.MediaInfo{Kind: f.Kind, DurationMs: f.DurationMs, PosterID: f.PosterID,
			HasAudio: f.HasAudio, Invalid: f.Invalid}
	}
	return out, nil
}

type smartcutSyncTimelines struct{ svc *timelineapp.Service }

func (a smartcutSyncTimelines) SyncState(ctx context.Context, ws, timelineID int64) (smartcutapp.SyncState, error) {
	v, err := a.svc.Get(ctx, ws, timelineID)
	if err != nil {
		return smartcutapp.SyncState{}, err
	}
	return syncStateOf(v), nil
}

func (a smartcutSyncTimelines) AppendSynced(ctx context.Context, ws, timelineID int64, in smartcutapp.SyncAppend) (
	smartcutapp.SyncState, []int64, error) {
	items := make([]timelineapp.SyncItem, len(in.Items))
	for i, it := range in.Items {
		items[i] = timelineapp.SyncItem{MediaID: it.MediaID, Kind: it.Kind, DurationMs: it.DurationMs,
			AudioRole: it.AudioRole, SourceNodeID: it.SourceNodeID}
	}
	res, err := a.svc.AppendSynced(ctx, timelineapp.SyncAppendInput{WorkspaceID: ws, TimelineID: timelineID,
		BaseVersion: in.BaseVersion, Items: items, Fingerprint: in.Fingerprint, Planned: in.Planned})
	if err != nil {
		return smartcutapp.SyncState{}, nil, err
	}
	return syncStateOf(res.View), res.Appended, nil
}

func syncStateOf(v timelineapp.View) smartcutapp.SyncState {
	st := smartcutapp.SyncState{Version: v.Timeline.Version, IsArranged: v.Timeline.IsArranged}
	for _, tv := range v.Tracks {
		for _, c := range tv.Clips {
			st.Clips = append(st.Clips, smartcutapp.SyncClip{ID: c.ID, SourceNodeID: c.SourceNodeID, MediaID: c.MediaID})
		}
	}
	return st
}

type smartcutTimelines struct{ svc *timelineapp.Service }

func (a smartcutTimelines) NodeTimelineID(ctx context.Context, ws, projectID int64, nodeID string) (int64, bool, error) {
	return a.svc.NodeTimelineID(ctx, ws, projectID, nodeID)
}

func (a smartcutTimelines) CreateForNode(ctx context.Context, ws, projectID int64, nodeID string,
	sourceTimelineID int64) (int64, bool, error) {
	t, created, err := a.svc.CreateForNode(ctx, timelineapp.NodeTimelineInput{
		WorkspaceID: ws, ProjectID: projectID, NodeID: nodeID, SourceTimelineID: sourceTimelineID})
	return t.ID, created, err
}

func (a smartcutTimelines) SourceProject(ctx context.Context, ws, timelineID int64) (int64, error) {
	return a.svc.SourceProject(ctx, ws, timelineID)
}

func (a smartcutTimelines) BindNode(ctx context.Context, ws, projectID, timelineID int64, nodeID string) error {
	return a.svc.BindNode(ctx, ws, projectID, timelineID, nodeID)
}

func (a smartcutTimelines) Content(ctx context.Context, ws, timelineID int64) (timelinedomain.Content, error) {
	v, err := a.svc.Get(ctx, ws, timelineID)
	if err != nil {
		return timelinedomain.Content{}, err
	}
	return timelineapp.ContentFromView(v), nil
}

func (a smartcutTimelines) TimelineNode(ctx context.Context, ws, projectID, timelineID int64) (string, bool, error) {
	return a.svc.TimelineNode(ctx, ws, projectID, timelineID)
}

func (a smartcutTimelines) NodeTimelines(ctx context.Context, ws, projectID int64) ([]smartcutapp.NodeTimeline, error) {
	list, clips, err := a.svc.NodeTimelines(ctx, ws, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]smartcutapp.NodeTimeline, 0, len(list))
	for _, t := range list {
		n := smartcutapp.NodeTimeline{
			ID: t.ID, CanvasNodeID: t.CanvasNodeID, Version: t.Version, IsArranged: t.IsArranged,
			UpdatedAt: t.UpdatedAt, Video: clips[t.ID].Video, Audio: clips[t.ID].Audio,
			SuccessVersion: t.SuccessVersion, SuccessMediaID: t.SuccessMediaID,
			PlannedFingerprint: t.PlannedFingerprint, SyncedFingerprint: t.SyncedFingerprint,
		}
		if b := t.LatestRender; b != nil {
			n.LatestRender = &smartcutapp.RenderBrief{ID: b.ID, Status: b.Status, Progress: b.Progress, Stage: b.Stage,
				OutputMediaID: b.OutputMediaID, TimelineVersion: b.TimelineVersion, Spec: b.Spec,
				ErrorMessage: b.ErrorMessage}
		}
		out = append(out, n)
	}
	return out, nil
}

type smartcutProjects struct{ svc *canvasapp.Service }

// ProjectStatus 项目在本工作区的状态；不存在与不是你的都回 ErrProjectNotFound（由 smartcut 说成 404）。
func (a smartcutProjects) ProjectStatus(ctx context.Context, ws, projectID int64) (string, error) {
	st, err := a.svc.ProjectStatus(ctx, ws, projectID)
	var be *httpx.BizError
	if errors.As(err, &be) && be.Code == errcode.NotFound {
		return "", smartcutapp.ErrProjectNotFound
	}
	return st, err
}

type smartcutMedia struct{ svc *mediaapp.Service }

func (a smartcutMedia) Briefs(ctx context.Context, ws int64, ids []int64) (map[int64]smartcutapp.MediaBrief, error) {
	got, err := a.svc.Briefs(ctx, ws, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]smartcutapp.MediaBrief, len(got))
	for id, b := range got {
		out[id] = smartcutapp.MediaBrief{Kind: b.Kind, PosterID: b.PosterID}
	}
	return out, nil
}

// CanvasgenSmartcuts 把智能剪辑服务接成 canvasgen 的端口（SCT-41 的 Agent / MCP 工具建节点、按节点自愈用）。
type CanvasgenSmartcuts struct{ Svc *smartcutapp.Service }

func (a CanvasgenSmartcuts) Create(ctx context.Context, c canvasgen.Caller, nodeID string, sourceTimelineID int64) (int64, error) {
	sum, _, err := a.Svc.Create(ctx, c.WorkspaceID, c.ProjectID, nodeID, sourceTimelineID)
	return sum.TimelineID, err
}

func (a CanvasgenSmartcuts) TimelineNode(ctx context.Context, c canvasgen.Caller, timelineID int64) (string, error) {
	node, ok, err := a.Svc.TimelineNode(ctx, c.WorkspaceID, c.ProjectID, timelineID)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", canvasgen.ErrSmartcutTimelineNotFound
	}
	return node, nil
}

// smartcutOutputs 把时间线仓储接成整图的成片查询（SCT-09）：与摘要同一条查询（最近一次成功导出 + 当前版本）。
//
// 直接用仓储而不经 smartcut/app：整图服务装在 BuildCore 里（API 与 Worker 的 Agent 共用），那里还没有时间线服务；
// 这里只读一条 SQL 的结果，不涉及任何业务规则（「过期」的判定在 workflow 的计划器里）。
type smartcutOutputs struct{ repo *timelinerepo.Repo }

func (a smartcutOutputs) Outputs(ctx context.Context, ws, projectID int64) (map[string]workflowapp.SmartcutOutput, error) {
	list, err := a.repo.NodeTimelines(ctx, ws, projectID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]workflowapp.SmartcutOutput, len(list))
	for _, t := range list {
		out[t.CanvasNodeID] = workflowapp.SmartcutOutput{
			MediaID: t.SuccessMediaID, RenderVersion: t.SuccessVersion, CurrentVersion: t.Version}
	}
	return out, nil
}

// SmartcutSources 把画布里某个节点的入边上游换成智能剪辑的素材来源（SCT-25，SCD-22）。
//
// 服务端的输入同步与共享 fixture 的测试走**同一个函数**：解析规则在 canvas/domain.InboundSources，
// 这里只做字段搬运（智能剪辑不 import 画布域）。
func SmartcutSources(raw json.RawMessage, nodeID string) ([]smartcutdomain.Source, error) {
	in, err := canvasdomain.InboundSources(raw, nodeID)
	if err != nil {
		return nil, err
	}
	out := make([]smartcutdomain.Source, 0, len(in))
	for _, x := range in {
		r := x.Result
		out = append(out, smartcutdomain.Source{
			Order: x.Order, NodeID: x.NodeID, Type: x.Type, Title: x.Title, Params: x.Params,
			FirstInboundOperation: x.FirstInboundOperation,
			Result: smartcutdomain.SourceResult{ID: r.ID, SourceGenerationID: r.SourceGenerationID, Status: r.Status,
				MediaID: r.MediaID, OutputURL: r.OutputURL, OutputType: r.OutputType, OutputText: r.OutputText,
				Error: r.Error},
		})
	}
	return out, nil
}
