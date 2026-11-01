package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/li904338203/tabtv/backend/internal/smartcut/domain"
	timelinedomain "github.com/li904338203/tabtv/backend/internal/timeline/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 片段级生成（docs/agent/smartcut SCT-37~39，SC-F-130~139，SCD-33）。
//
// 都走生成域的同一个受理入口（报价、能力、扣费、幂等都在那里）：来源 smartcut、不挂画布节点、不登记作品、
// 不发站内通知；`smartcut_target` 记下产物落到哪里与发起时的版本，完成后由 BindSmartcut 回填（clipbind.go）。
// **只换剪辑台里的这一段，不改上游节点**：画布上的节点、它的产物与参数都不动（SC-AC-54）。

// ClipToolPorts 片段级生成要的端口（wiring 适配，本包不碰生成域与时间线域的仓储类型）。
type ClipToolPorts struct {
	Gen ClipGenerations
	// Timelines 读时间线；Save 是回填（整份保存的同一条写路径：校验、快照、乐观锁、事件）。
	Timelines interface {
		Content(ctx context.Context, workspaceID, timelineID int64) (timelinedomain.Content, error)
		Save(ctx context.Context, in SavePlanInput) (SavedPlan, error)
	}
	Pending ClipPendingMarks
	Media   ClipMedia
	// Models 片段级工具可选的模型（`/smartcut/config`）；只有 API 进程要（nil = 不列）。
	Models ClipModelCatalog
	Log    *slog.Logger
}

// ClipModelCatalog 目录域：某个操作此刻可用的模型（绑定了、按给定输入报得出价的，与受理同一个报价函数）。
type ClipModelCatalog interface {
	ClipModels(ctx context.Context, operation string, memberLevelID int64, roles map[string]int) ([]ModelChoice, error)
}

// clipModelAsk 一个工具按哪个操作、哪些输入列模型。
type clipModelAsk struct {
	op    string
	roles map[string]int
}

// clipToolModels `/smartcut/config` 的 models：工具 → 按什么列。补镜两种操作的模型合在一起（同一个模型只列一次，
// 选了哪种操作由补镜请求说，报价那时再按那种操作报）。
//
// ⚠️ 输入角色与各工具受理时**解析出来的**一致：提示词在参数里、不算输入（配乐、文生视频都是空的），去字幕与音效是
// 一段视频，图生视频是一张图。按别的角色列的话，清单说能用的模型点下去报「输入不符合能力矩阵」，或者反过来。
var clipToolModels = map[string][]clipModelAsk{
	"video_desubtitle": {{op: OpDesubtitle, roles: map[string]int{"video": 1}}},
	"text2music":       {{op: "text2music", roles: map[string]int{}}},
	"video2audio":      {{op: "video2audio", roles: map[string]int{"video": 1}}},
	"fill": {{op: "text2video", roles: map[string]int{}},
		{op: "img2video", roles: map[string]int{"image": 1}}},
}

// clipModelChoices 按工具列可选模型（去重，先列的优先）。
func (s *Service) clipModelChoices(ctx context.Context, tool string, memberLevelID int64) ([]ModelChoice, error) {
	out := []ModelChoice{}
	seen := map[string]bool{}
	for _, ask := range clipToolModels[tool] {
		list, err := s.clipTools.Models.ClipModels(ctx, ask.op, memberLevelID, ask.roles)
		if err != nil {
			return nil, err
		}
		for _, m := range list {
			if !seen[m.Code] {
				seen[m.Code] = true
				out = append(out, m)
			}
		}
	}
	return out, nil
}

// SetClipToolPorts 装上片段级生成的端口。
func (s *Service) SetClipToolPorts(p ClipToolPorts) {
	if p.Log == nil {
		p.Log = slog.Default()
	}
	s.clipTools = p
	if s.now == nil {
		s.now = time.Now
	}
}

// ClipSource 产出某份素材的那次成功生成（片段重拍以它为模板，生成信息也读它）。
type ClipSource struct {
	GenerationID  int64
	ProjectID     int64
	OperationCode string
	ModelCode     string
	ModelName     string
	Prompt        string
	// Params 用户参数（不含 `_snapshot`）。
	Params json.RawMessage
	// Inputs 输入快照原样（受理时还原成声明形态、重新解析）。
	Inputs json.RawMessage
	// Origin 来源（smartcut = 片段级生成的产物）；SmartcutKind 那次的回写种类（replace / insert / desubtitle …）。
	Origin       string
	SmartcutKind string
	SmartcutTool string
}

// ClipAccept 一次片段级生成受理（或报价）。FromGenerationID 非 0 时以那次生成为模板（模型、操作、参数、输入：
// 片段重拍），Prompt 换掉模板里的提示词；否则按给的模型、操作、参数与输入。
type ClipAccept struct {
	WorkspaceID, PayerAccountID, UserID, ProjectID, MemberLevelID int64
	FromGenerationID                                              int64
	ModelCode, OperationCode                                      string
	Prompt                                                        string
	// Params 额外参数（覆盖模板里的同名键）。
	Params map[string]any
	// Inputs 声明形态的输入（ID 一律字符串）；以模板受理时为空 = 用模板的。
	Inputs         json.RawMessage
	DurationSec    int
	IdempotencyKey string
	Target         json.RawMessage
}

// ClipQuote 报价（dryRun）。Blocker 非空表示现在受理不了（原因原样给用户）。
type ClipQuote struct {
	Credits   int
	UnitPrice int
	Unit      string
	// DurationSec 计费时长（按区间计价的工具给：去字幕、配音效；向上取整到秒，与受理时一致）。
	DurationSec int
	ModelCode   string
	Blocker     *QuoteBlocker
}

// ClipAccepted 受理结果。
type ClipAccepted struct {
	GenerationID int64
	Status       string
	Credits      int
	CreatedAt    time.Time
	Replayed     bool
}

// ClipGenerationRow 生成记录里的一条（契约 SmartcutGeneration）。
type ClipGenerationRow struct {
	GenerationID  int64
	Target        json.RawMessage
	Status        string
	Progress      int
	Credits       int
	OutputMediaID int64
	Error         string
	CreatedAt     time.Time
}

// ClipGenerations 生成域：素材的来源、受理与报价、任务状态、按时间线列出、记下已回填。
type ClipGenerations interface {
	Sources(ctx context.Context, workspaceID int64, mediaIDs []int64) (map[int64]ClipSource, error)
	Preview(ctx context.Context, in ClipAccept) (ClipQuote, error)
	Accept(ctx context.Context, in ClipAccept) (ClipAccepted, error)
	Status(ctx context.Context, workspaceID, generationID int64) (string, error)
	List(ctx context.Context, workspaceID, timelineID int64, cursor string, limit int) ([]ClipGenerationRow, string, error)
	MarkLanded(ctx context.Context, generationID int64) error
}

// ClipPendingMarks 时间线域：「这一段在生成」的标记（不推进版本，见 timeline/repo/pending.go）。
type ClipPendingMarks interface {
	ClipPending(ctx context.Context, workspaceID, timelineID, clipID int64) (int64, error)
	SetClipPending(ctx context.Context, workspaceID, timelineID, clipID, generationID int64) (bool, error)
	ClearClipPending(ctx context.Context, workspaceID, timelineID, clipID, generationID int64) (bool, error)
}

// ClipMediaInfo 一份素材的类型（video / image / audio）、时长、末帧与用途（生成信息分类用）。
type ClipMediaInfo struct {
	Kind       string
	DurationMs int
	// LastFrameID 视频的末帧（Worker 截的 video_lastframe）；还没截出来为 0。
	// ⚠️ 不是封面帧：封面取第一帧，拿它补镜会让新镜头从上一段的开头重新开始。
	LastFrameID int64
	Purpose     string
}

// ClipMedia 媒体域。
type ClipMedia interface {
	Info(ctx context.Context, workspaceID int64, ids []int64) (map[int64]ClipMediaInfo, error)
}

// clipTarget 片段级生成的回写目标（`generation.smartcut_target`）。ID 一律字符串（索引按 `->>'timelineId'` 取）。
type clipTarget struct {
	TimelineID int64 `json:"timelineId,string"`
	// Kind 产物落到哪：replace 替换这一段、insert 插在 AfterClipID 之后（为空插在最前面）。
	Kind string `json:"kind"`
	// Tool 哪个工具（生成记录里的 kind）：regen / fill / desubtitle。
	Tool        string `json:"tool"`
	ClipID      int64  `json:"clipId,string,omitempty"`
	AfterClipID int64  `json:"afterClipId,string,omitempty"`
	// AtMs / DurationMs 配乐的落点与想要的长度（成片时间；模型单次最短比想要的长时，落下去裁到想要的长度）。
	AtMs       int `json:"atMs,omitempty"`
	DurationMs int `json:"durationMs,omitempty"`
	// RangeMs 音效生成时那一段的素材区间（锚点按它记，SC-F-56）。
	RangeMs []int `json:"rangeMs,omitempty"`
	// Version 发起时的时间线版本（只作记录：回填按当时读到的版本 CAS）。
	Version int   `json:"version"`
	UserID  int64 `json:"userId,string,omitempty"`
	Landed  bool  `json:"landed,omitempty"`
}

// 片段级生成的工具（生成记录的 kind）与落点。
const (
	clipKindReplace = "replace"
	clipKindInsert  = "insert"
	clipKindMusic   = "music"
	clipKindSfx     = "sfx"
	toolRegen       = "regen"
	toolFill        = "fill"
	toolDesubtitle  = "desubtitle"
	toolMusic       = "music"
	toolSfx         = "sfx"
	// MaxClipPromptChars 片段重拍与补镜的提示词上限（契约 maxLength）。
	MaxClipPromptChars = 4000
)

func (s *Service) clipToolsReady() error {
	p := s.clipTools
	if p.Gen == nil || p.Timelines == nil || p.Pending == nil || p.Media == nil {
		return httpx.Errf(errcode.Internal, "片段级生成链路未接入")
	}
	return nil
}

// ClipToolRequest 片段级生成请求的公共部分。
type ClipToolRequest struct {
	WorkspaceID, PayerAccountID, ProjectID, TimelineID int64
	UserID, MemberLevelID                              int64
	DryRun                                             bool
	// IdempotencyKey 客户端给的（一次确认一个键，失败后的重试复用同一个键）。
	IdempotencyKey string
}

// ClipToolResult dryRun 时是报价，否则是受理结果。
type ClipToolResult struct {
	Quote    *ClipQuote
	Accepted *SmartcutGenerationView
}

// clipCommon 校验与载入：项目可写（报价只要可读）、节点绑定、时间线内容。
func (s *Service) clipCommon(ctx context.Context, req ClipToolRequest, what string) (timelinedomain.Content, error) {
	if err := s.clipToolsReady(); err != nil {
		return timelinedomain.Content{}, err
	}
	if !req.DryRun && (len(req.IdempotencyKey) < 8 || len(req.IdempotencyKey) > 128) {
		return timelinedomain.Content{}, httpx.Errf(errcode.MissingIdempotencyKey, what+"会扣费，请求要带 Idempotency-Key（8~128 位）")
	}
	if req.DryRun {
		if err := s.projectReadable(ctx, req.WorkspaceID, req.ProjectID); err != nil {
			return timelinedomain.Content{}, err
		}
	} else if err := s.projectWritable(ctx, req.WorkspaceID, req.ProjectID, what); err != nil {
		return timelinedomain.Content{}, err
	}
	if _, err := s.boundNode(ctx, req.WorkspaceID, req.ProjectID, req.TimelineID); err != nil {
		return timelinedomain.Content{}, err
	}
	return s.clipTools.Timelines.Content(ctx, req.WorkspaceID, req.TimelineID)
}

func validClipPrompt(prompt string) (string, error) {
	p := strings.TrimSpace(prompt)
	switch {
	case p == "":
		return "", httpx.Errf(errcode.InvalidArgument, "先写下要生成什么").WithDetails(map[string]any{"path": "prompt"})
	case utf8.RuneCountInString(p) > MaxClipPromptChars:
		return "", httpx.Errf(errcode.InvalidArgument, "提示词不能超过 "+strconv.Itoa(MaxClipPromptChars)+" 字").
			WithDetails(map[string]any{"path": "prompt"})
	}
	return p, nil
}

// clipBusy 这一段正在被一次片段级生成替换时报错（同一段同时只能有一次）；那次早已结束但标记没清掉（回写失败）时清掉放行。
func (s *Service) clipBusy(ctx context.Context, ws, tid, clipID int64) error {
	pending, err := s.clipTools.Pending.ClipPending(ctx, ws, tid, clipID)
	if err != nil || pending == 0 {
		return err
	}
	status, err := s.clipTools.Gen.Status(ctx, ws, pending)
	if err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	if status == "queued" || status == "running" {
		return httpx.Errf(errcode.InvalidStateTransition, "这一段正在重新生成，等它完成再试")
	}
	_, _ = s.clipTools.Pending.ClearClipPending(ctx, ws, tid, clipID, pending)
	return nil
}

// ---------------------------------------------------------------- 片段重拍（SC-F-130）

// RegenerateRequest 片段重拍（POST .../clips/{clipId}/regenerate）。
type RegenerateRequest struct {
	ClipToolRequest
	ClipID int64
	Prompt string
}

// RegenerateClip 沿用这一段素材那次生成的模型、操作、参数与输入，换上新提示词整段重新生成；完成后替换这一段，
// 原素材进备选。按那个模型定价（SCK-4）。
func (s *Service) RegenerateClip(ctx context.Context, req RegenerateRequest) (ClipToolResult, error) {
	prompt, err := validClipPrompt(req.Prompt)
	if err != nil {
		return ClipToolResult{}, err
	}
	content, err := s.clipCommon(ctx, req.ClipToolRequest, "重新生成片段")
	if err != nil {
		return ClipToolResult{}, err
	}
	clip, src, err := s.regenSource(ctx, req.WorkspaceID, content, req.ClipID)
	if err != nil {
		return ClipToolResult{}, err
	}
	in := ClipAccept{WorkspaceID: req.WorkspaceID, PayerAccountID: req.PayerAccountID, UserID: req.UserID,
		ProjectID: req.ProjectID, MemberLevelID: req.MemberLevelID, FromGenerationID: src.GenerationID,
		ModelCode: src.ModelCode, OperationCode: src.OperationCode, Prompt: prompt, IdempotencyKey: req.IdempotencyKey}
	if req.DryRun {
		q, err := s.clipTools.Gen.Preview(ctx, in)
		if err != nil {
			return ClipToolResult{}, err
		}
		return ClipToolResult{Quote: &q}, nil
	}
	if err := s.clipBusy(ctx, req.WorkspaceID, req.TimelineID, req.ClipID); err != nil {
		return ClipToolResult{}, err
	}
	in.Target, _ = json.Marshal(clipTarget{TimelineID: req.TimelineID, Kind: clipKindReplace, Tool: toolRegen,
		ClipID: req.ClipID, Version: content.Version, UserID: req.UserID})
	acc, err := s.clipTools.Gen.Accept(ctx, in)
	if err != nil {
		return ClipToolResult{}, err
	}
	s.markPending(ctx, req.WorkspaceID, req.TimelineID, req.ClipID, acc)
	view := clipView(acc, toolRegen)
	view.ClipID, view.MediaID = strconv.FormatInt(req.ClipID, 10), clip.MediaID
	return ClipToolResult{Accepted: &view}, nil
}

// regenSource 能不能重拍、以哪次生成为模板：画面轨上的、生成出来的素材（上传的没有生成信息）；
// 去字幕的结果要先切回原片（以它为模板重拍的话「重拍」的是去字幕那一步）。
func (s *Service) regenSource(ctx context.Context, ws int64, c timelinedomain.Content, clipID int64) (
	timelinedomain.ContentClip, ClipSource, error) {
	clip, trackKind, ok := domain.FindClip(c, strconv.FormatInt(clipID, 10))
	if !ok {
		return clip, ClipSource{}, httpx.Errf(errcode.NotFound, "这条时间线上没有这一段").WithDetails(map[string]any{"path": "clipId"})
	}
	if trackKind != timelinedomain.TrackVideo {
		return clip, ClipSource{}, regenUnsuitable("只有画面轨上的片段可以重新生成")
	}
	sources, err := s.clipTools.Gen.Sources(ctx, ws, []int64{clip.MediaID})
	if err != nil {
		return clip, ClipSource{}, httpx.Wrap(errcode.Internal, err)
	}
	src, ok := sources[clip.MediaID]
	if !ok {
		return clip, ClipSource{}, regenUnsuitable("这是上传的素材，没有生成信息：重新生成只能用于生成出来的素材")
	}
	if why := regenBlocked(src); why != "" {
		return clip, ClipSource{}, regenUnsuitable(why)
	}
	return clip, src, nil
}

// regenBlocked 以这次生成为模板重拍不成立的原因（生成信息的 canRegenerate 与受理共用这一个判断）。
func regenBlocked(src ClipSource) string {
	switch {
	case src.SmartcutTool == "desubtitle":
		return "这一段是去字幕的结果：要重新生成，先在备选里切回原片"
	case src.ModelCode == "":
		return "生成这段素材的模型已经下线，不能按原模型重新生成"
	}
	return ""
}

func regenUnsuitable(detail string) error {
	return httpx.Errf(errcode.SmartcutInputUnsuitable, detail).
		WithDetails(map[string]any{"reason": "not_regenerable", "options": []string{}})
}

func (s *Service) markPending(ctx context.Context, ws, tid, clipID int64, acc ClipAccepted) {
	if acc.Status != "queued" && acc.Status != "running" {
		return
	}
	if _, err := s.clipTools.Pending.SetClipPending(ctx, ws, tid, clipID, acc.GenerationID); err != nil {
		s.clipTools.Log.WarnContext(ctx, "片段级生成已受理但「生成中」标记没写上", "clipId", clipID,
			"generationId", acc.GenerationID, "error", err)
	}
}

func clipView(acc ClipAccepted, tool string) SmartcutGenerationView {
	return SmartcutGenerationView{GenerationID: acc.GenerationID, Kind: tool, Status: acc.Status, Credits: acc.Credits,
		CreatedAt: acc.CreatedAt}
}

// ---------------------------------------------------------------- 补镜（SC-F-131）

// FillRequest 补镜（POST .../fill）。
type FillRequest struct {
	ClipToolRequest
	// AfterClipID 插在这一段之后（0 = 最前面）。
	AfterClipID   int64
	Prompt        string
	ModelCode     string
	Operation     string
	DurationSec   int
	RefFromClipID int64
}

// fillOperations 补镜能用的操作（契约 enum）。
var fillOperations = map[string]bool{"text2video": true, "img2video": true}

// FillGap 在指定位置插入一段新生成的视频；按所选模型定价。img2video 用相邻片段的末帧（封面帧）作参考图。
// 完成后插在发起时记下的位置；那一段已经不在时产物只进生成记录。
func (s *Service) FillGap(ctx context.Context, req FillRequest) (ClipToolResult, error) {
	prompt, err := validClipPrompt(req.Prompt)
	if err != nil {
		return ClipToolResult{}, err
	}
	switch {
	case strings.TrimSpace(req.ModelCode) == "":
		return ClipToolResult{}, httpx.Errf(errcode.InvalidArgument, "请选择模型").WithDetails(map[string]any{"path": "modelCode"})
	case !fillOperations[req.Operation]:
		return ClipToolResult{}, httpx.Errf(errcode.InvalidArgument, "operation 只能是 text2video / img2video").
			WithDetails(map[string]any{"path": "operation"})
	case req.Operation == "img2video" && req.RefFromClipID == 0:
		return ClipToolResult{}, httpx.Errf(errcode.InvalidArgument, "图生视频要选一段作参考（取它的末帧）").
			WithDetails(map[string]any{"path": "refFromClipId"})
	case req.DurationSec < 0 || req.DurationSec > 60:
		return ClipToolResult{}, httpx.Errf(errcode.InvalidArgument, "时长只能是 1~60 秒").WithDetails(map[string]any{"path": "durationSec"})
	}
	if req.DurationSec == 0 {
		req.DurationSec = 5
	}
	content, err := s.clipCommon(ctx, req.ClipToolRequest, "补镜")
	if err != nil {
		return ClipToolResult{}, err
	}
	if req.AfterClipID != 0 {
		if _, kind, ok := domain.FindClip(content, strconv.FormatInt(req.AfterClipID, 10)); !ok || kind != timelinedomain.TrackVideo {
			return ClipToolResult{}, httpx.Errf(errcode.NotFound, "画面轨上没有这一段").WithDetails(map[string]any{"path": "afterClipId"})
		}
	}
	params := map[string]any{"duration": req.DurationSec}
	// 补出来的一段跟着成片的画幅（模型不支持时报价会说清楚）。
	if ar := content.AspectRatio; ar == "16:9" || ar == "9:16" || ar == "1:1" {
		params["aspect_ratio"] = ar
	}
	in := ClipAccept{WorkspaceID: req.WorkspaceID, PayerAccountID: req.PayerAccountID, UserID: req.UserID,
		ProjectID: req.ProjectID, MemberLevelID: req.MemberLevelID, ModelCode: strings.TrimSpace(req.ModelCode),
		OperationCode: req.Operation, Prompt: prompt, Params: params, DurationSec: req.DurationSec,
		IdempotencyKey: req.IdempotencyKey}
	if req.Operation == "img2video" {
		ref, err := s.fillReference(ctx, req.WorkspaceID, content, req.RefFromClipID)
		if err != nil {
			return ClipToolResult{}, err
		}
		in.Inputs, _ = json.Marshal([]map[string]any{{"kind": "reference", "order": 1, "referenceId": strconv.FormatInt(ref, 10)}})
	}
	if req.DryRun {
		q, err := s.clipTools.Gen.Preview(ctx, in)
		if err != nil {
			return ClipToolResult{}, err
		}
		return ClipToolResult{Quote: &q}, nil
	}
	in.Target, _ = json.Marshal(clipTarget{TimelineID: req.TimelineID, Kind: clipKindInsert, Tool: toolFill,
		AfterClipID: req.AfterClipID, Version: content.Version, UserID: req.UserID})
	acc, err := s.clipTools.Gen.Accept(ctx, in)
	if err != nil {
		return ClipToolResult{}, err
	}
	view := clipView(acc, toolFill)
	return ClipToolResult{Accepted: &view}, nil
}

// fillReference 参考图：这一段是图片就用它本身，是视频就用它的末帧（Worker 截的 video_lastframe）。
func (s *Service) fillReference(ctx context.Context, ws int64, c timelinedomain.Content, clipID int64) (int64, error) {
	clip, _, ok := domain.FindClip(c, strconv.FormatInt(clipID, 10))
	if !ok {
		return 0, httpx.Errf(errcode.NotFound, "这条时间线上没有这一段").WithDetails(map[string]any{"path": "refFromClipId"})
	}
	infos, err := s.clipTools.Media.Info(ctx, ws, []int64{clip.MediaID})
	if err != nil {
		return 0, httpx.Wrap(errcode.Internal, err)
	}
	info, ok := infos[clip.MediaID]
	switch {
	case !ok:
		return 0, httpx.Errf(errcode.NotFound, "这一段的素材不存在").WithDetails(map[string]any{"path": "refFromClipId"})
	case info.Kind == "image":
		return clip.MediaID, nil
	case info.Kind == "video" && info.LastFrameID != 0:
		return info.LastFrameID, nil
	case info.Kind == "video":
		return 0, httpx.Errf(errcode.InvalidArgument, "这一段的末帧还没准备好，稍后再试，或者改用文生视频").
			WithDetails(map[string]any{"path": "refFromClipId"})
	}
	return 0, httpx.Errf(errcode.InvalidArgument, "音频片段不能作参考图").WithDetails(map[string]any{"path": "refFromClipId"})
}

// ---------------------------------------------------------------- 去字幕（SC-F-132）

// OpDesubtitle 去字幕的操作码（不是画布节点的操作，只在剪辑台里用）。
const OpDesubtitle = "video_desubtitle"

// DesubtitleRequest 去字幕（POST .../clips/{clipId}/desubtitle）。
type DesubtitleRequest struct {
	ClipToolRequest
	ClipID    int64
	ModelCode string
	// Mode subtitle（缺省：只去字幕）/ text（去掉画面上的全部文字）。
	Mode string
}

// DesubtitleClip 去掉一段视频里的字幕：只把这一段取用的区间截出来发给上游（SCD-34，截取在 Worker 里做），
// 按这段时长计价；完成后替换这一段（新素材从头取、长度 = 区间长度），原素材连同原区间进备选（SCD-35）。
//
// ⚠️ 不能整段素材发过去：一段 60 秒的素材只用了 3 秒时，整段发等于多付 57 秒的钱，结果还要再裁一次。
func (s *Service) DesubtitleClip(ctx context.Context, req DesubtitleRequest) (ClipToolResult, error) {
	mode := strings.TrimSpace(req.Mode)
	switch {
	case strings.TrimSpace(req.ModelCode) == "":
		return ClipToolResult{}, httpx.Errf(errcode.InvalidArgument, "请选择模型").WithDetails(map[string]any{"path": "modelCode"})
	case mode == "":
		mode = "subtitle"
	case mode != "subtitle" && mode != "text":
		return ClipToolResult{}, httpx.Errf(errcode.InvalidArgument, "mode 只能是 subtitle / text").
			WithDetails(map[string]any{"path": "mode"})
	}
	content, err := s.clipCommon(ctx, req.ClipToolRequest, "去字幕")
	if err != nil {
		return ClipToolResult{}, err
	}
	clip, err := s.videoClip(ctx, req.WorkspaceID, content, req.ClipID, "去字幕")
	if err != nil {
		return ClipToolResult{}, err
	}
	rangeMs := []int{clip.SourceInMs, clip.SourceOutMs}
	in := ClipAccept{WorkspaceID: req.WorkspaceID, PayerAccountID: req.PayerAccountID, UserID: req.UserID,
		ProjectID: req.ProjectID, MemberLevelID: req.MemberLevelID, ModelCode: strings.TrimSpace(req.ModelCode),
		OperationCode: OpDesubtitle, Params: map[string]any{"mode": mode}, IdempotencyKey: req.IdempotencyKey}
	in.Inputs, _ = json.Marshal([]map[string]any{{"kind": "reference", "order": 1,
		"referenceId": strconv.FormatInt(clip.MediaID, 10), "rangeMs": rangeMs}})
	if req.DryRun {
		q, err := s.clipTools.Gen.Preview(ctx, in)
		if err != nil {
			return ClipToolResult{}, err
		}
		q.DurationSec = rangeSeconds(rangeMs)
		return ClipToolResult{Quote: &q}, nil
	}
	if err := s.clipBusy(ctx, req.WorkspaceID, req.TimelineID, req.ClipID); err != nil {
		return ClipToolResult{}, err
	}
	in.Target, _ = json.Marshal(clipTarget{TimelineID: req.TimelineID, Kind: clipKindReplace, Tool: toolDesubtitle,
		ClipID: req.ClipID, Version: content.Version, UserID: req.UserID})
	acc, err := s.clipTools.Gen.Accept(ctx, in)
	if err != nil {
		return ClipToolResult{}, err
	}
	s.markPending(ctx, req.WorkspaceID, req.TimelineID, req.ClipID, acc)
	view := clipView(acc, toolDesubtitle)
	view.ClipID, view.MediaID = strconv.FormatInt(req.ClipID, 10), clip.MediaID
	return ClipToolResult{Accepted: &view}, nil
}

// videoClip 片段级工具要的一段：画面轨上、素材是视频（去字幕、按画面配音效都只认视频）。
// 不合适时回 42203（所选操作不支持这个输入），原因写明是哪一条。
func (s *Service) videoClip(ctx context.Context, ws int64, c timelinedomain.Content, clipID int64, what string) (
	timelinedomain.ContentClip, error) {
	clip, trackKind, ok := domain.FindClip(c, strconv.FormatInt(clipID, 10))
	if !ok {
		return clip, httpx.Errf(errcode.NotFound, "这条时间线上没有这一段").WithDetails(map[string]any{"path": "clipId"})
	}
	if trackKind != timelinedomain.TrackVideo {
		return clip, httpx.Errf(errcode.CapabilityUnsupported, "只有画面轨上的视频片段可以"+what)
	}
	infos, err := s.clipTools.Media.Info(ctx, ws, []int64{clip.MediaID})
	if err != nil {
		return clip, httpx.Wrap(errcode.Internal, err)
	}
	info, ok := infos[clip.MediaID]
	switch {
	case !ok:
		return clip, httpx.Errf(errcode.NotFound, "这一段的素材不存在").WithDetails(map[string]any{"path": "clipId"})
	case info.Kind != "video":
		return clip, httpx.Errf(errcode.CapabilityUnsupported, "这一段是图片，只有视频片段可以"+what)
	}
	return clip, nil
}

// rangeSeconds 区间的计费秒数：向上取整（与生成域按区间计价的规则一致）。
func rangeSeconds(r []int) int {
	if len(r) != 2 || r[1] <= r[0] {
		return 0
	}
	return (r[1] - r[0] + 999) / 1000
}

// ---------------------------------------------------------------- 生成配乐 / 生成音效（SC-F-65、SC-F-133）

// 配乐与音效的操作码与上限（契约：配乐描述最多 2000 字、时长 3~300 秒；音效描述最多 1000 字）。
const (
	OpTextToMusic       = "text2music"
	OpVideoToAudio      = "video2audio"
	MaxMusicPromptChars = 2000
	MaxSfxPromptChars   = 1000
	MinMusicSec         = 3
	MaxMusicSec         = 300
	// defaultMusicSec 时间线还是空的时候配乐的缺省时长。
	defaultMusicSec = 30
)

// MusicRequest 生成配乐（POST .../music）。
type MusicRequest struct {
	ClipToolRequest
	Prompt    string
	ModelCode string
	// DurationSec 想要多长（0 = 从 AtMs 到成片结尾）；AtMs 放在成片的哪里（发起时的播放头）。
	DurationSec int
	AtMs        int
	// Instrumental 纯音乐（缺省 true）。
	Instrumental *bool
}

// GenerateMusic 按描述生成一段配乐，放进配乐轨、起点为发起时的播放头（SC-F-65）。按所选模型定价。
//
// 时长缺省取「从播放头到成片结尾」（dev/01 R-20 #8：播放头在 0 时就是成片时长；按整段成片长度从播放头往后放的话
// 配乐会越过成片结尾，导出时被画面轨的长度生硬截断），播放头已在结尾或之后时取成片时长。**报价之前**就按所选
// 模型的单次范围截（SCT-39 风险第 4 条）：超出上限让上游报错再退款是白等一轮；报价的 durationSec 是计费秒数
// （向上取整），确认框据此说明「按 60 秒生成」。模型单次最短比想要的长时按最短生成、落下去按毫秒裁到想要的长度。
func (s *Service) GenerateMusic(ctx context.Context, req MusicRequest) (ClipToolResult, error) {
	prompt := strings.TrimSpace(req.Prompt)
	switch {
	case prompt == "":
		return ClipToolResult{}, httpx.Errf(errcode.InvalidArgument, "先写下想要什么样的配乐").WithDetails(map[string]any{"path": "prompt"})
	case utf8.RuneCountInString(prompt) > MaxMusicPromptChars:
		return ClipToolResult{}, httpx.Errf(errcode.InvalidArgument, "配乐描述不能超过 "+strconv.Itoa(MaxMusicPromptChars)+" 字").
			WithDetails(map[string]any{"path": "prompt"})
	case strings.TrimSpace(req.ModelCode) == "":
		return ClipToolResult{}, httpx.Errf(errcode.InvalidArgument, "请选择模型").WithDetails(map[string]any{"path": "modelCode"})
	case req.DurationSec != 0 && (req.DurationSec < MinMusicSec || req.DurationSec > MaxMusicSec):
		return ClipToolResult{}, httpx.Errf(errcode.InvalidArgument, fmt.Sprintf("时长只能是 %d~%d 秒", MinMusicSec, MaxMusicSec)).
			WithDetails(map[string]any{"path": "durationSec"})
	case req.AtMs < 0:
		return ClipToolResult{}, httpx.Errf(errcode.InvalidArgument, "起点不能为负").WithDetails(map[string]any{"path": "atMs"})
	}
	content, err := s.clipCommon(ctx, req.ClipToolRequest, "生成配乐")
	if err != nil {
		return ClipToolResult{}, err
	}
	wantMs := musicWantMs(timelinedomain.Layout(content).EffectiveMs, req.AtMs, req.DurationSec)
	sec := (wantMs + 999) / 1000
	if lo, hi, ok := s.clipModelRange(ctx, OpTextToMusic, clipToolModels["text2music"][0].roles, req.ModelCode,
		req.MemberLevelID); ok {
		if lo > 0 {
			sec = max(sec, lo)
		}
		if hi > 0 {
			sec = min(sec, hi)
		}
	}
	instrumental := req.Instrumental == nil || *req.Instrumental
	in := ClipAccept{WorkspaceID: req.WorkspaceID, PayerAccountID: req.PayerAccountID, UserID: req.UserID,
		ProjectID: req.ProjectID, MemberLevelID: req.MemberLevelID, ModelCode: strings.TrimSpace(req.ModelCode),
		OperationCode: OpTextToMusic, Prompt: prompt, Params: map[string]any{"duration": sec, "instrumental": instrumental},
		DurationSec: sec, IdempotencyKey: req.IdempotencyKey}
	if req.DryRun {
		q, err := s.clipTools.Gen.Preview(ctx, in)
		if err != nil {
			return ClipToolResult{}, err
		}
		q.DurationSec = sec
		return ClipToolResult{Quote: &q}, nil
	}
	in.Target, _ = json.Marshal(clipTarget{TimelineID: req.TimelineID, Kind: clipKindMusic, Tool: toolMusic, AtMs: req.AtMs,
		DurationMs: min(wantMs, sec*1000), Version: content.Version, UserID: req.UserID})
	acc, err := s.clipTools.Gen.Accept(ctx, in)
	if err != nil {
		return ClipToolResult{}, err
	}
	view := clipView(acc, toolMusic)
	return ClipToolResult{Accepted: &view}, nil
}

// musicWantMs 想要的配乐长度（毫秒）：显式给了秒数就用它；否则从 atMs 到成片结尾，atMs 已在结尾或之后时取成片
// 时长，时间线还是空的时候取缺省 30 秒；最后夹到 3~300 秒。前端 smartcutMusicDuration 是同一条规则。
func musicWantMs(effectiveMs, atMs, durationSec int) int {
	if durationSec > 0 {
		return durationSec * 1000
	}
	want := effectiveMs - atMs
	if want <= 0 {
		want = effectiveMs
	}
	if want <= 0 {
		want = defaultMusicSec * 1000
	}
	return min(max(want, MinMusicSec*1000), MaxMusicSec*1000)
}

// SfxRequest 按画面生成音效（POST .../clips/{clipId}/sfx）。
type SfxRequest struct {
	ClipToolRequest
	ClipID    int64
	Prompt    string
	ModelCode string
}

// GenerateSfx 按选中的画面片段生成音效：只把这一段取用的区间截出来发给上游（SCD-34），按这段时长计价；
// 完成后放进音效轨并跟随这一段（SC-F-56）。
//
// 片段长度不在所选模型的单次范围内（可灵视频配音效只收 3~20 秒）时回 42203 并说明，不受理（SC-F-133）——
// 按毫秒判：报价侧拿到的是向上取整的秒数，19.4 秒的片段报 20 秒放行没错，2.6 秒报 3 秒就漏过去了。
func (s *Service) GenerateSfx(ctx context.Context, req SfxRequest) (ClipToolResult, error) {
	prompt := strings.TrimSpace(req.Prompt)
	switch {
	case strings.TrimSpace(req.ModelCode) == "":
		return ClipToolResult{}, httpx.Errf(errcode.InvalidArgument, "请选择模型").WithDetails(map[string]any{"path": "modelCode"})
	case utf8.RuneCountInString(prompt) > MaxSfxPromptChars:
		return ClipToolResult{}, httpx.Errf(errcode.InvalidArgument, "音效描述不能超过 "+strconv.Itoa(MaxSfxPromptChars)+" 字").
			WithDetails(map[string]any{"path": "prompt"})
	}
	content, err := s.clipCommon(ctx, req.ClipToolRequest, "生成音效")
	if err != nil {
		return ClipToolResult{}, err
	}
	clip, err := s.videoClip(ctx, req.WorkspaceID, content, req.ClipID, "生成音效")
	if err != nil {
		return ClipToolResult{}, err
	}
	rangeMs := []int{clip.SourceInMs, clip.SourceOutMs}
	if lo, hi, ok := s.clipModelRange(ctx, OpVideoToAudio, clipToolModels["video2audio"][0].roles, req.ModelCode,
		req.MemberLevelID); ok {
		if n := rangeMs[1] - rangeMs[0]; (lo > 0 && n < lo*1000) || (hi > 0 && n > hi*1000) {
			return ClipToolResult{}, httpx.Errf(errcode.CapabilityUnsupported,
				fmt.Sprintf("这个模型只收 %d~%d 秒的片段，这一段取用了 %.1f 秒：先把它裁到这个范围里再生成音效", lo, hi,
					float64(n)/1000)).WithDetails(map[string]any{"path": "clipId", "minSec": lo, "maxSec": hi, "clipMs": n})
		}
	}
	in := ClipAccept{WorkspaceID: req.WorkspaceID, PayerAccountID: req.PayerAccountID, UserID: req.UserID,
		ProjectID: req.ProjectID, MemberLevelID: req.MemberLevelID, ModelCode: strings.TrimSpace(req.ModelCode),
		OperationCode: OpVideoToAudio, Prompt: prompt, IdempotencyKey: req.IdempotencyKey}
	in.Inputs, _ = json.Marshal([]map[string]any{{"kind": "reference", "order": 1,
		"referenceId": strconv.FormatInt(clip.MediaID, 10), "rangeMs": rangeMs}})
	if req.DryRun {
		q, err := s.clipTools.Gen.Preview(ctx, in)
		if err != nil {
			return ClipToolResult{}, err
		}
		q.DurationSec = rangeSeconds(rangeMs)
		return ClipToolResult{Quote: &q}, nil
	}
	in.Target, _ = json.Marshal(clipTarget{TimelineID: req.TimelineID, Kind: clipKindSfx, Tool: toolSfx, ClipID: req.ClipID,
		RangeMs: rangeMs, Version: content.Version, UserID: req.UserID})
	acc, err := s.clipTools.Gen.Accept(ctx, in)
	if err != nil {
		return ClipToolResult{}, err
	}
	view := clipView(acc, toolSfx)
	view.ClipID, view.MediaID = strconv.FormatInt(req.ClipID, 10), clip.MediaID
	return ClipToolResult{Accepted: &view}, nil
}

// clipModelRange 所选模型在这个操作下的单次时长范围（秒，取自模型清单，与报价同一份声明）。
// 清单没接（Worker 侧）或模型不在清单里（不可用：报价会给出原因）时 ok=false，不在这里拦。
func (s *Service) clipModelRange(ctx context.Context, op string, roles map[string]int, modelCode string,
	memberLevelID int64) (int, int, bool) {
	if s.clipTools.Models == nil {
		return 0, 0, false
	}
	list, err := s.clipTools.Models.ClipModels(ctx, op, memberLevelID, roles)
	if err != nil {
		return 0, 0, false
	}
	for _, m := range list {
		if m.Code == strings.TrimSpace(modelCode) {
			return m.MinSec, m.MaxSec, m.MinSec > 0 || m.MaxSec > 0
		}
	}
	return 0, 0, false
}

// ---------------------------------------------------------------- 生成记录与生成信息

// ClipGenerationView 生成记录的一条（契约 SmartcutGeneration）。
type ClipGenerationView struct {
	SmartcutGenerationView
	OutputMediaID int64
	Landed        bool
	Error         string
}

// Generations 这条时间线名下的识别与片段级生成，按时间倒序（SC-F-139）。
func (s *Service) Generations(ctx context.Context, ws, pid, tid int64, cursor string, limit int) ([]ClipGenerationView, string, error) {
	if err := s.clipToolsReady(); err != nil {
		return nil, "", err
	}
	if err := s.projectReadable(ctx, ws, pid); err != nil {
		return nil, "", err
	}
	if _, err := s.boundNode(ctx, ws, pid, tid); err != nil {
		return nil, "", err
	}
	rows, next, err := s.clipTools.Gen.List(ctx, ws, tid, cursor, limit)
	if err != nil {
		return nil, "", httpx.Wrap(errcode.Internal, err)
	}
	out := make([]ClipGenerationView, 0, len(rows))
	for _, r := range rows {
		var t struct {
			Kind        string `json:"kind"`
			Tool        string `json:"tool"`
			ClipID      string `json:"clipId"`
			MediaID     string `json:"mediaId"`
			Landed      bool   `json:"landed"`
			AfterClipID string `json:"afterClipId"`
		}
		_ = json.Unmarshal(r.Target, &t)
		kind := t.Tool
		if t.Kind == "asr" {
			kind = "asr"
		}
		v := ClipGenerationView{SmartcutGenerationView: SmartcutGenerationView{GenerationID: r.GenerationID, Kind: kind,
			Status: r.Status, Progress: r.Progress, Credits: r.Credits, ClipID: t.ClipID, CreatedAt: r.CreatedAt},
			OutputMediaID: r.OutputMediaID, Landed: t.Landed, Error: r.Error}
		if id, err := strconv.ParseInt(t.MediaID, 10, 64); err == nil {
			v.MediaID = id
		}
		out = append(out, v)
	}
	return out, next, nil
}

// ClipOrigin 片段编辑「生成信息」（契约 SmartcutClipOrigin，SC-F-41）。
type ClipOrigin struct {
	Kind          string
	GenerationID  int64
	OperationCode string
	ModelCode     string
	ModelName     string
	Prompt        string
	Params        map[string]any
	Inputs        []ClipOriginInput
	CanRegenerate bool
	Reason        string
}

// ClipOriginInput 生成用到的一条输入（只给种类与顺序：参考图的 ID 指向别的节点的产物，给出去也打不开）。
type ClipOriginInput struct {
	Kind  string `json:"kind"`
	Order int    `json:"order"`
}

// originParams 生成信息里给哪些参数（白名单：时长、分辨率、画幅、模式、风格类 —— 别的是内部字段或太长）。
var originParams = []string{"duration", "resolution", "aspect_ratio", "video_mode", "quality", "style", "voice", "speed"}

// ClipOriginOf 这一段素材的来历：生成出来的给模型、操作、提示词与主要参数，以及能不能重拍；
// 渲染的成片、导演台截图、上传的各给一个种类。
func (s *Service) ClipOriginOf(ctx context.Context, ws, pid, tid, clipID int64) (ClipOrigin, error) {
	if err := s.clipToolsReady(); err != nil {
		return ClipOrigin{}, err
	}
	if err := s.projectReadable(ctx, ws, pid); err != nil {
		return ClipOrigin{}, err
	}
	if _, err := s.boundNode(ctx, ws, pid, tid); err != nil {
		return ClipOrigin{}, err
	}
	content, err := s.clipTools.Timelines.Content(ctx, ws, tid)
	if err != nil {
		return ClipOrigin{}, err
	}
	clip, trackKind, ok := domain.FindClip(content, strconv.FormatInt(clipID, 10))
	if !ok {
		return ClipOrigin{}, httpx.Errf(errcode.NotFound, "这条时间线上没有这一段")
	}
	sources, err := s.clipTools.Gen.Sources(ctx, ws, []int64{clip.MediaID})
	if err != nil {
		return ClipOrigin{}, httpx.Wrap(errcode.Internal, err)
	}
	if src, ok := sources[clip.MediaID]; ok {
		o := ClipOrigin{Kind: "generation", GenerationID: src.GenerationID, OperationCode: src.OperationCode,
			ModelCode: src.ModelCode, ModelName: src.ModelName, Prompt: src.Prompt, Params: map[string]any{},
			Inputs: []ClipOriginInput{}}
		var params map[string]any
		_ = json.Unmarshal(src.Params, &params)
		for _, k := range originParams {
			if v, ok := params[k]; ok {
				o.Params[k] = v
			}
		}
		var inputs []ClipOriginInput
		if json.Unmarshal(src.Inputs, &inputs) == nil {
			o.Inputs = append(o.Inputs, inputs...)
		}
		switch why := regenBlocked(src); {
		case trackKind != timelinedomain.TrackVideo:
			o.Reason = "只有画面轨上的片段可以重新生成"
		case why != "":
			o.Reason = why
		default:
			o.CanRegenerate = true
		}
		return o, nil
	}
	infos, err := s.clipTools.Media.Info(ctx, ws, []int64{clip.MediaID})
	if err != nil {
		return ClipOrigin{}, httpx.Wrap(errcode.Internal, err)
	}
	o := ClipOrigin{Kind: "unknown", Reason: "找不到这段素材的生成信息，不能重新生成"}
	switch purpose := infos[clip.MediaID].Purpose; {
	case purpose == "render_output":
		o.Kind, o.Reason = "render", "这是导出的成片：要改内容，回到它的剪辑台里改"
	case strings.HasPrefix(purpose, "director_"):
		o.Kind, o.Reason = "director", "这是 3D 导演台的截图：回到导演台里重新取景"
	case purpose == "upload_reference" || purpose == "script_asset":
		o.Kind, o.Reason = "upload", "这是上传的素材，没有生成信息：重新生成只能用于生成出来的素材"
	}
	return o, nil
}
