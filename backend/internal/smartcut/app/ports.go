// Package app 是智能剪辑的应用层（docs/agent/smartcut dev/01 §2.1）：节点绑定与摘要、输入同步、AI 剪辑……
//
// 时间线、项目、媒体都经端口取（wiring 适配）：智能剪辑不碰别的域的表（08 §1）。
package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	timelinedomain "github.com/li904338203/tabtv/backend/internal/timeline/domain"
)

// ErrProjectNotFound 项目不在本工作区（不存在与不是你的不区分）。
var ErrProjectNotFound = errors.New("项目不存在")

// ProjectAccess 项目的存在与状态（wiring 适配 canvas/app.ProjectStatus）。
type ProjectAccess interface {
	// ProjectStatus 返回 active / deleted；不在本工作区时返回 ErrProjectNotFound。
	ProjectStatus(ctx context.Context, workspaceID, projectID int64) (string, error)
}

// TimelineStore 时间线域（wiring 适配 timeline/app）。
type TimelineStore interface {
	// NodeTimelineID 节点已绑定的有效时间线；没有时 ok=false。
	NodeTimelineID(ctx context.Context, workspaceID, projectID int64, nodeID string) (int64, bool, error)
	// CreateForNode 为节点建（或分叉 sourceTimelineID）时间线，按（项目，节点）幂等。返回时间线 ID 与「这次真的新建了」。
	CreateForNode(ctx context.Context, workspaceID, projectID int64, nodeID string, sourceTimelineID int64) (int64, bool, error)
	// SourceProject 分叉来源所在的项目；来源不在本工作区时返回 404 业务错误。
	SourceProject(ctx context.Context, workspaceID, timelineID int64) (int64, error)
	// TimelineNode 本项目里一条有效时间线绑定的节点；不在本项目或没绑节点时 ok=false。
	TimelineNode(ctx context.Context, workspaceID, projectID, timelineID int64) (string, bool, error)
	// NodeTimelines 本项目全部绑定节点的时间线（摘要用，一次取全）。
	NodeTimelines(ctx context.Context, workspaceID, projectID int64) ([]NodeTimeline, error)
	// BindNode 把一条还没绑节点的时间线绑到节点上（SCT-41）：已经绑在这个节点上的算成功；冲突回 40902、不在本项目回 404。
	BindNode(ctx context.Context, workspaceID, projectID, timelineID int64, nodeID string) error
	// Content 当前的完整内容（与整份保存同形，片段清单用，SCT-41）。
	Content(ctx context.Context, workspaceID, timelineID int64) (timelinedomain.Content, error)
}

// NodeTimeline 一条绑定节点的时间线：版本、剪过与否、画面轨与第一条音频轨的片段、最近一次导出与最近一次成功导出。
type NodeTimeline struct {
	ID             int64
	CanvasNodeID   string
	Version        int
	IsArranged     bool
	UpdatedAt      time.Time
	Video          []timelinedomain.Clip
	Audio          []timelinedomain.Clip
	LatestRender   *RenderBrief
	SuccessVersion int
	SuccessMediaID int64
	// PlannedFingerprint / SyncedFingerprint 剪辑依据的素材与最近一次同步看到的素材（「素材有变化」，SCT-30）。
	PlannedFingerprint string
	SyncedFingerprint  string
}

// RenderBrief 最近一次导出的概况。
type RenderBrief struct {
	ID              int64
	Status          string
	Progress        int
	Stage           string
	OutputMediaID   int64
	TimelineVersion int
	Spec            json.RawMessage
	ErrorMessage    string
}

// MediaBriefs 媒体的类型与封面帧（wiring 适配 media/app.Briefs）。查不到的不在结果里。
type MediaBriefs interface {
	Briefs(ctx context.Context, workspaceID int64, ids []int64) (map[int64]MediaBrief, error)
}

// MediaBrief 类型（image / video / audio）与封面帧。
type MediaBrief struct {
	Kind     string
	PosterID int64
}
