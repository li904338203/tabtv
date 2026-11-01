package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/li904338203/tabtv/backend/internal/smartcut/domain"
	"github.com/li904338203/tabtv/backend/internal/smartcut/repo"
	timelinedomain "github.com/li904338203/tabtv/backend/internal/timeline/domain"
)

// AI 剪辑的端口（SCT-27，dev/01 §3.4）。模型、目录、提示词、时间线写入、素材分析都经端口接进来，
// 适配器在 wiring（智能剪辑不 import 别的域的 app / repo，08 §1）。
//
// ⚠️ AI 剪辑**免费**（SCK-2）：这些端口里没有账本。免费靠配置成立（smartcut_plan 的兜底价 0 积分），
// 运营改成非 0 时目录域显式拒绝（catalog/app.SmartcutPlanChoice），不悄悄忽略。

// Actor 发起 AI 剪辑的人：网页（只有 UserID）、Agent（AgentSessionID 非 0）或外部 MCP 客户端（McpGrantID 非 0）。
// 后两者的 UserID 是「代谁剪的」那个人：限流按他算。
type Actor struct {
	UserID         int64
	AgentSessionID int64
	McpGrantID     int64
}

// source 调用记录的来源（ck_sc_call_source）。
func (a Actor) source() string {
	switch {
	case a.AgentSessionID != 0:
		return "agent"
	case a.McpGrantID != 0:
		return "mcp"
	}
	return "web"
}

// editorKind 写进时间线的编辑者来源：网页发起的是「AI 剪辑」（ai），Agent / MCP 发起的记它们自己
// （冲突提示要分得出是谁改的，SCD-17）。
func (a Actor) editorKind() string {
	switch {
	case a.AgentSessionID != 0:
		return "agent"
	case a.McpGrantID != 0:
		return "mcp"
	}
	return "ai"
}

// PlanChoice 这次 AI 剪辑要用的模型；MaxImages 是拼图张数上限（模型的 image 上限与 SC-PARAM-24 取小）。
type PlanChoice struct {
	ModelCode string
	MaxImages int
}

// Unavailable 说明 AI 剪辑现在为什么不可用（42218）。UserFixable：用户自己能解决的（会员等级），其余要运营处理。
type Unavailable struct {
	Reason      string
	UserFixable bool
}

// PlanCatalog 目录域：smartcut_plan 此刻可用的第一个模型（/plan 与 /smartcut/config 共用这一个判断，SC-F-88）。
type PlanCatalog interface {
	SmartcutPlanChoice(ctx context.Context, memberLevelID int64, imageCount int) (PlanChoice, *Unavailable, error)
}

// TextRequest 一次同步文本调用。Images 是内联好的 `data:` 地址，不发外链（SC-AC-37）。
type TextRequest struct {
	ModelCode string
	System    string
	User      string
	Images    []string
	// Operation 记进请求日志（provider_request_log 记 smartcut_plan，SC-AC-30）。
	Operation string
}

// TextModel Provider 域的同步文本调用（provider/app.HTTPProvider.CompleteText）。
type TextModel interface {
	Complete(ctx context.Context, in TextRequest) (string, error)
}

// Prompts 目录域：渲染 `smartcut.{mode}.system`（DB 行优先，代码兜底在目录域）。
type Prompts interface {
	RenderPrompt(ctx context.Context, code, modelCode string, vars map[string]string) (string, error)
}

// Editor 写进时间线的编辑者。
type Editor struct {
	UserID int64
	Kind   string
}

// SavePlanInput 把编译结果写进时间线：同一事务里先存快照、再以 Content.Version 做 CAS（SCD-18）。
type SavePlanInput struct {
	WorkspaceID int64
	TimelineID  int64
	Editor      Editor
	Content     timelinedomain.Content
	// Reason 事件里的写入原因（plan / edit）。
	Reason string
	// SnapshotReason 快照原因（ai_plan / ai_edit / ai_variant / replace_changed）。
	SnapshotReason string
	CallID         int64
	// Fingerprint 非空时把它记成「剪辑依据的素材」（planned_fingerprint）与最近一次同步看到的指纹。
	Fingerprint string
}

// SavedPlan 写入之后的时间线。Timeline 是与 `GET /timelines/{id}` 同形的 JSON（由时间线域的传输层渲染）。
type SavedPlan struct {
	Version    int
	SnapshotID int64
	Timeline   json.RawMessage
	// IDMap 临时 ID → 正式 ID（新建的轨道与条目；翻译要用它说出新字幕轨的 ID）。
	IDMap map[string]string
}

// PlanTimelines 时间线域：读当前内容、整份写入、冲突详情。
type PlanTimelines interface {
	// Content 当前的完整内容（与整份保存同形，ID 都是正式 ID）。
	Content(ctx context.Context, workspaceID, timelineID int64) (timelinedomain.Content, error)
	// Save 整份写入。版本对不上时返回时间线域的 40902（details 里有编辑者），调用方补上 callId。
	Save(ctx context.Context, in SavePlanInput) (SavedPlan, error)
	// Conflict 时间线当前的冲突详情（发起时 baseVersion 就对不上，用它说清是谁改的）。
	Conflict(ctx context.Context, timelineID int64, editor Editor) error
}

// MosaicItem 要进拼图的一段素材。
type MosaicItem struct {
	No      string
	MediaID int64
	Kind    string
}

// Mosaic 一张拼图：内联好的 `data:` 地址，以及每一行是哪段素材（按行）。
type Mosaic struct {
	DataURI string
	Rows    []string
}

// MaterialFacts 素材的描述与分析结果（wiring：生成域、媒体域）。查不到的不在结果里。
type MaterialFacts interface {
	// Descriptions 素材描述：这个模型写过的看图描述优先，其次是产出它的那次生成的提示词。按媒体 ID。
	Descriptions(ctx context.Context, workspaceID, projectID int64, mediaIDs []int64, modelCode string) (map[int64]string, error)
	// Mosaics 上传素材的缩略图拼图（每张三行、每行一段素材的首 / 中 / 尾三帧，SC-PARAM-24），最多 maxSheets 张。
	// 没有缩略图（还在分析）的素材不进拼图。
	Mosaics(ctx context.Context, workspaceID int64, items []MosaicItem, maxSheets int) ([]Mosaic, error)
	// Beats 配乐的节拍。
	Beats(ctx context.Context, workspaceID int64, mediaIDs []int64) (map[int64]domain.Beats, error)
	// Peaks 波形（口播的能量校正）。
	Peaks(ctx context.Context, workspaceID int64, mediaIDs []int64) (map[int64]domain.Peaks, error)
	// Transcripts 识别结果（口播的视频、解说的配音）：按用途挑能用的那一份；不能用的给出原因（按媒体 ID，没有识别过的不在里面）。
	Transcripts(ctx context.Context, workspaceID int64, mediaIDs []int64, need TranscriptNeed) (
		map[int64]domain.Transcript, map[int64]string, error)
	// VoiceScripts 配音的原稿：产出这份音频的那次文字转语音实际念的文字（从生成快照还原：按连线顺序的文本输入 +
	// 节点自己的提示词）。不是文字转语音产出的（上传的音频）不在结果里。按媒体 ID。
	VoiceScripts(ctx context.Context, workspaceID int64, mediaIDs []int64) (map[int64]string, error)
	// SaveCaptions 写回模型对看图素材的描述（media_analysis kind=caption、engine=模型码），失败只告警。
	SaveCaptions(ctx context.Context, workspaceID int64, modelCode string, captions map[int64]string) error
}

// TranscriptNeed 用途对识别结果的要求。
type TranscriptNeed struct {
	// KeepFillers 识别时保留了口癖（口播精剪要靠识别结果判断口癖，开了顺滑的结果里口癖已经没了）。
	KeepFillers bool
	// Fine 有字级或词级时间（口播的剪点要落在字与字之间；只有句或整段文字的不行）。
	Fine bool
}

// ShotGroup 素材条里的一个镜头组（脚本或视频组）。
type ShotGroup struct {
	No           string
	SourceNodeID string
	Title        string
	ScriptID     int64
	// GroupID 视频组节点时只取这个组的子项（同一脚本可能有好几个视频组，CLAUDE.md §5.10）。
	GroupID int64
}

// BoardGap 装配时的一处缺口：哪个来源、第几镜、为什么。
type BoardGap struct {
	Source string
	ShotNo int
	Reason string
}

// BoardShots 分镜成片的装配（SCT-28，wiring：脚本域）。按给出的顺序逐组装配，缺素材与失败的镜头逐条报出。
type BoardShots interface {
	Assemble(ctx context.Context, workspaceID int64, groups []ShotGroup) ([]domain.BoardShot, []BoardGap, error)
}

// PlanEvents 实时事件（platform/eventbus.Publisher 满足它）。为 nil 时只是没有阶段推送，前端靠响应与摘要对账。
type PlanEvents interface {
	Publish(ctx context.Context, topic string, payload []byte) error
}

// PlanCalls 调用记录（smartcut/repo 实现）。
type PlanCalls interface {
	InsertWithLimits(ctx context.Context, c repo.Call, lim repo.Limits) (repo.LimitDecision, error)
	SetStage(ctx context.Context, id int64, stage string) (bool, error)
	SetModel(ctx context.Context, id int64, modelCode, fingerprint string) error
	Finish(ctx context.Context, id int64, in repo.FinishInput) (bool, error)
	SetApplied(ctx context.Context, id int64, variant int) error
	Get(ctx context.Context, workspaceID, timelineID, id int64) (repo.Record, error)
	Usage(ctx context.Context, userID int64) (running, usedThisHour int, err error)
	ActiveCalls(ctx context.Context, workspaceID int64, timelineIDs []int64) (map[int64]repo.Active, error)
	RecoverStale(ctx context.Context, olderThan time.Duration, limit int) (int, error)
	DeleteOlderThan(ctx context.Context, before time.Time, limit int) (int, error)
}

// PlanConfig 可配置的几项（dev/01 §10，SC-PARAM-19 / 22 / 29，设计建议值）。
type PlanConfig struct {
	Concurrency int
	PerHour     int
	// CallTimeout 单次模型调用上限（默认 3 分钟）；带拼图时用 VisionTimeout（默认 5 分钟）。
	CallTimeout   time.Duration
	VisionTimeout time.Duration
	// StaleAfter 「进行中」超过它判失败（默认 20 分钟，必须大于长路由上限 15 分钟）。
	StaleAfter time.Duration
	// Retention 调用记录保留期（默认 30 天）。
	Retention time.Duration
}

func (c PlanConfig) withDefaults() PlanConfig {
	if c.Concurrency <= 0 {
		c.Concurrency = 1
	}
	if c.PerHour <= 0 {
		c.PerHour = 20
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = 3 * time.Minute
	}
	if c.VisionTimeout <= 0 {
		c.VisionTimeout = 5 * time.Minute
	}
	if c.StaleAfter <= 0 {
		c.StaleAfter = 20 * time.Minute
	}
	if c.Retention <= 0 {
		c.Retention = 30 * 24 * time.Hour
	}
	return c
}

// PlanPorts AI 剪辑要的全部端口。
type PlanPorts struct {
	Calls     PlanCalls
	Catalog   PlanCatalog
	Text      TextModel
	Prompts   Prompts
	Timelines PlanTimelines
	Material  MaterialFacts
	Board     BoardShots
	Events    PlanEvents
	NextID    func() int64
	Config    PlanConfig
	Log       *slog.Logger
}

// SetPlanPorts 装上 AI 剪辑的端口。
func (s *Service) SetPlanPorts(p PlanPorts) {
	p.Config = p.Config.withDefaults()
	if p.Log == nil {
		p.Log = slog.New(slog.DiscardHandler)
	}
	s.plan = p
	if s.now == nil {
		s.now = time.Now
	}
}
