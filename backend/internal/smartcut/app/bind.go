package app

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/li904338203/tabtv/backend/internal/smartcut/domain"
	timelinedomain "github.com/li904338203/tabtv/backend/internal/timeline/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 智能剪辑节点 ↔ 时间线（docs/agent/smartcut SCT-07，SCD-20 / SCD-21）。

// Service 智能剪辑的应用服务。
type Service struct {
	timelines TimelineStore
	projects  ProjectAccess
	media     MediaBriefs
	// inputs 输入同步的端口（SCT-25）。
	inputs InputPorts
	// plan AI 剪辑的端口（SCT-27）。
	plan PlanPorts
	// transcribe 语音识别的端口（SCT-33）。
	transcribe TranscribePorts
	// clipTools 片段级生成（SCT-37~39）。
	clipTools ClipToolPorts
	// now 可在测试里替换。
	now func() time.Time
}

// New 装出服务（端口由 wiring 适配）。
func New(timelines TimelineStore, projects ProjectAccess, media MediaBriefs) *Service {
	return &Service{timelines: timelines, projects: projects, media: media, now: time.Now}
}

// Summary 一个智能剪辑节点的状态（契约 SmartcutSummary，SCK-13）：节点卡、下游取输入、整图运行都看它。
type Summary struct {
	TimelineID   int64
	CanvasNodeID string
	Version      int
	IsArranged   bool
	TotalMs      int
	EffectiveMs  int
	// ClipCount 画面轨的段数（节点卡上的「N 段」）。
	ClipCount    int
	CoverMediaID int64
	// InputsStale 素材有变化：剪辑依据的素材指纹与最近一次同步看到的不同（SCT-30）。
	InputsStale bool
	RenderStale bool
	// LatestRender 最近一次导出；OutputPosterID 是成片的封面帧。
	LatestRender   *RenderBrief
	OutputPosterID int64
	// ActiveCall 进行中的 AI 调用（AI 剪辑 / 追加指令 / 翻译，SCT-27）。
	ActiveCall *ActiveCall
	Strip      []domain.StripItem
	UpdatedAt  time.Time
}

// ActiveCall 进行中的 AI 剪辑调用。
type ActiveCall struct {
	ID        int64
	Kind      string
	Stage     string
	StartedAt time.Time
}

// Create 为画布节点建时间线并绑定（POST /projects/{pid}/smartcuts）。按（项目，节点）幂等：已有时返回那一条与 false。
//
// ⚠️ 顺序照导演台（DRD-17）：先判项目可写（回收站里的 40905）→ 已有就直接返回（不再校验分叉来源：
// 网络重试、两个标签页都会带着同一个来源再来一次）→ 校验来源（同工作区、所在项目可读，否则 404）→ 建。
func (s *Service) Create(ctx context.Context, workspaceID, projectID int64, nodeID string,
	sourceTimelineID int64) (Summary, bool, error) {
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" || utf8.RuneCountInString(nodeID) > domain.MaxNodeIDChars {
		return Summary{}, false, httpx.Errf(errcode.InvalidArgument, "canvasNodeId 必填，且不超过 64 个字符").
			WithDetails(map[string]any{"path": "canvasNodeId"})
	}
	if err := s.projectWritable(ctx, workspaceID, projectID, "新建智能剪辑"); err != nil {
		return Summary{}, false, err
	}
	if id, ok, err := s.timelines.NodeTimelineID(ctx, workspaceID, projectID, nodeID); err != nil {
		return Summary{}, false, err
	} else if ok {
		sum, err := s.summaryOf(ctx, workspaceID, projectID, id)
		return sum, false, err
	}
	if sourceTimelineID != 0 {
		srcProject, err := s.timelines.SourceProject(ctx, workspaceID, sourceTimelineID)
		if err != nil {
			return Summary{}, false, err
		}
		// 来源所在项目要可读（回收站里的也算：复制一份出来不改变源项目的任何东西）。
		if _, err := s.projects.ProjectStatus(ctx, workspaceID, srcProject); err != nil {
			if errors.Is(err, ErrProjectNotFound) {
				return Summary{}, false, httpx.Errf(errcode.NotFound, "要复制的时间线不存在")
			}
			return Summary{}, false, err
		}
	}
	id, created, err := s.timelines.CreateForNode(ctx, workspaceID, projectID, nodeID, sourceTimelineID)
	if err != nil {
		return Summary{}, false, err
	}
	sum, err := s.summaryOf(ctx, workspaceID, projectID, id)
	return sum, created, err
}

// List 本项目全部智能剪辑节点的摘要（GET /projects/{pid}/smartcuts）。回收站里的项目也能读。
func (s *Service) List(ctx context.Context, workspaceID, projectID int64) ([]Summary, error) {
	if err := s.projectReadable(ctx, workspaceID, projectID); err != nil {
		return nil, err
	}
	list, err := s.timelines.NodeTimelines(ctx, workspaceID, projectID)
	if err != nil {
		return nil, err
	}
	return s.summarize(ctx, workspaceID, list)
}

func (s *Service) summaryOf(ctx context.Context, workspaceID, projectID, timelineID int64) (Summary, error) {
	all, err := s.List(ctx, workspaceID, projectID)
	if err != nil {
		return Summary{}, err
	}
	for _, sm := range all {
		if sm.TimelineID == timelineID {
			return sm, nil
		}
	}
	return Summary{}, httpx.Errf(errcode.Internal, "刚建好的时间线读不到")
}

// summarize 把时间线排成摘要：落点用时间线域的同一套算法（成片时间，SCK-15），媒体类型与封面帧一次批量查。
func (s *Service) summarize(ctx context.Context, workspaceID int64, list []NodeTimeline) ([]Summary, error) {
	var ids []int64
	for _, t := range list {
		for _, c := range t.Video {
			ids = append(ids, c.MediaID)
		}
		for _, c := range t.Audio {
			ids = append(ids, c.MediaID)
		}
		if t.SuccessMediaID != 0 {
			ids = append(ids, t.SuccessMediaID)
		}
		if t.LatestRender != nil && t.LatestRender.OutputMediaID != 0 {
			ids = append(ids, t.LatestRender.OutputMediaID)
		}
	}
	briefs := map[int64]MediaBrief{}
	if len(ids) > 0 && s.media != nil {
		got, err := s.media.Briefs(ctx, workspaceID, ids)
		if err != nil {
			return nil, httpx.Wrap(errcode.Internal, err)
		}
		briefs = got
	}
	active := s.activeCalls(ctx, workspaceID, list)
	out := make([]Summary, 0, len(list))
	for _, t := range list {
		video := timelinedomain.Reflow(t.Video)
		starts, effective := timelinedomain.VideoPlacement(video)
		videoStrip := make([]domain.StripItem, len(video))
		for i, c := range video {
			b := briefs[c.MediaID]
			kind, poster := "video", b.PosterID
			if b.Kind == "image" {
				kind, poster = "image", c.MediaID
			}
			videoStrip[i] = domain.StripItem{Kind: kind, StartMs: starts[i], DurationMs: c.PlayLen(), PosterMediaID: poster,
				MediaID: c.MediaID}
		}
		audio := timelinedomain.PlaceByStart(t.Audio)
		audioStrip := make([]domain.StripItem, len(audio))
		for i, c := range audio {
			audioStrip[i] = domain.StripItem{Kind: "audio", StartMs: c.StartMs, DurationMs: c.PlayLen(), MediaID: c.MediaID}
		}
		var firstMedia int64
		if len(video) > 0 {
			firstMedia = video[0].MediaID
		}
		sm := Summary{
			TimelineID: t.ID, CanvasNodeID: t.CanvasNodeID, Version: t.Version, IsArranged: t.IsArranged,
			TotalMs: timelinedomain.TotalDuration(video), EffectiveMs: effective, ClipCount: len(video),
			CoverMediaID: domain.Cover(briefs[t.SuccessMediaID].PosterID, videoStrip, firstMedia),
			RenderStale:  domain.RenderStale(t.SuccessVersion, t.Version),
			InputsStale:  domain.InputsStale(t.PlannedFingerprint, t.SyncedFingerprint),
			LatestRender: t.LatestRender, Strip: domain.Strip(videoStrip, audioStrip), UpdatedAt: t.UpdatedAt,
		}
		if t.LatestRender != nil {
			sm.OutputPosterID = briefs[t.LatestRender.OutputMediaID].PosterID
		}
		sm.ActiveCall = active[t.ID]
		out = append(out, sm)
	}
	return out, nil
}

// projectWritable 建时间线要求项目在本工作区且没进回收站（回收站里的报 40905：项目明明还在，说不存在会让他以为数据丢了）。
// projectWritable 项目可写（不在回收站）。action 用在提示里（「回收站中的项目不能<action>」）。
func (s *Service) projectWritable(ctx context.Context, workspaceID, projectID int64, action string) error {
	status, err := s.projects.ProjectStatus(ctx, workspaceID, projectID)
	if err != nil {
		return projectErr(err)
	}
	if status != "active" {
		return httpx.Errf(errcode.InvalidStateTransition, "回收站中的项目不能"+action+"，请先恢复")
	}
	return nil
}

func (s *Service) projectReadable(ctx context.Context, workspaceID, projectID int64) error {
	_, err := s.projects.ProjectStatus(ctx, workspaceID, projectID)
	return projectErr(err)
}

func projectErr(err error) error {
	if errors.Is(err, ErrProjectNotFound) {
		return httpx.Errf(errcode.NotFound, "项目不存在")
	}
	return err
}

// TimelineNode 本项目里一条有效时间线绑定的节点（按节点自愈用，canvasgen.SmartcutTimelineOfNode）。
func (s *Service) TimelineNode(ctx context.Context, workspaceID, projectID, timelineID int64) (string, bool, error) {
	return s.timelines.TimelineNode(ctx, workspaceID, projectID, timelineID)
}
