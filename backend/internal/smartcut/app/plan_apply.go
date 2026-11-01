package app

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/li904338203/tabtv/backend/internal/smartcut/domain"
	"github.com/li904338203/tabtv/backend/internal/smartcut/repo"
	timelinedomain "github.com/li904338203/tabtv/backend/internal/timeline/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 方案切换、「仍然应用」、配置、恢复扫描与阶段事件（SCT-27 / SCT-29）。

// ApplyRequest 切换到某一套方案（POST .../plan/{callId}/apply）。
type ApplyRequest struct {
	WorkspaceID int64
	ProjectID   int64
	TimelineID  int64
	CallID      int64
	Variant     int
	BaseVersion int
	Force       bool
	Actor       Actor
}

// ApplyVariant 把调用记录里的某一套方案写进时间线：**按当前素材重新编译**（两次之间素材可能换过，SCT-29），
// 写入前存快照。也是「仍然应用」的入口（剪辑期间被改过时，方案已经存下，不重新调模型）。不占限流额度：不调模型。
func (s *Service) ApplyVariant(ctx context.Context, req ApplyRequest) (PlanResult, error) {
	if err := s.planReady(); err != nil {
		return PlanResult{}, err
	}
	if err := s.projectWritable(ctx, req.WorkspaceID, req.ProjectID, "切换 AI 剪辑的方案"); err != nil {
		return PlanResult{}, err
	}
	if _, err := s.boundNode(ctx, req.WorkspaceID, req.ProjectID, req.TimelineID); err != nil {
		return PlanResult{}, err
	}
	rec, err := s.plan.Calls.Get(ctx, req.WorkspaceID, req.TimelineID, req.CallID)
	if errors.Is(err, repo.ErrNotFound) {
		return PlanResult{}, httpx.Errf(errcode.NotFound, "这次 AI 剪辑的记录不存在")
	}
	if err != nil {
		return PlanResult{}, httpx.Wrap(errcode.Internal, err)
	}
	if rec.Status == repo.StatusRunning {
		return PlanResult{}, httpx.Errf(errcode.InvalidStateTransition, "这次 AI 剪辑还没结束")
	}
	var stored storedResult
	if len(rec.Result) == 0 || json.Unmarshal(rec.Result, &stored) != nil {
		return PlanResult{}, httpx.Errf(errcode.InvalidStateTransition, "这次 AI 剪辑没有可用的方案")
	}
	if req.Variant < 0 || req.Variant >= len(stored.Variants) {
		return PlanResult{}, httpx.Errf(errcode.NotFound, "没有第 "+strconv.Itoa(req.Variant+1)+" 套方案")
	}
	v := stored.Variants[req.Variant]
	if v.Status != repo.StatusSuccess || (v.Draft == nil && !v.Fallback) {
		return PlanResult{}, httpx.Errf(errcode.InvalidStateTransition, "这一套方案没有成功，不能切换："+v.Error)
	}
	editor := Editor{UserID: req.Actor.UserID, Kind: req.Actor.editorKind()}
	base, err := s.plan.Timelines.Content(ctx, req.WorkspaceID, req.TimelineID)
	if err != nil {
		return PlanResult{}, err
	}
	if !req.Force && base.Version != req.BaseVersion {
		return PlanResult{}, withCallID(s.plan.Timelines.Conflict(ctx, req.TimelineID, editor), req.CallID)
	}
	var spec specRecord
	_ = json.Unmarshal(rec.Spec, &spec)
	inputs, err := s.Inputs(ctx, req.WorkspaceID, req.ProjectID, req.TimelineID)
	if err != nil {
		return PlanResult{}, err
	}
	mode := rec.Mode
	if rec.Kind == repo.KindEdit {
		// 追加指令只要素材卡片（替换、插入要的编号），不按剪辑方式准备（按混剪准备的话，没有画面素材的时间线会被误拒）。
		mode = domain.ModeEdit
	}
	facts, err := s.gatherFacts(ctx, req.WorkspaceID, req.ProjectID, mode, inputs.Items, spec.Spec, rec.ModelCode)
	if err != nil {
		return PlanResult{}, err
	}
	reason, snap := "plan", "ai_variant"
	switch {
	case rec.Kind == repo.KindEdit:
		reason, snap = "edit", "ai_edit"
	case rec.AppliedVariant == nil:
		// 剪辑期间被改过、方案从没写进去过：这一次是那轮 AI 的落地，不是切换。
		snap = "ai_plan"
	}
	r := &planRun{callID: rec.ID, ws: req.WorkspaceID, pid: req.ProjectID, tid: req.TimelineID, editor: editor,
		actor: req.Actor}
	content, err := s.recompile(facts, v, base)
	if err != nil {
		return PlanResult{}, err
	}
	content.Version = base.Version
	saved, err := s.plan.Timelines.Save(ctx, SavePlanInput{WorkspaceID: req.WorkspaceID, TimelineID: req.TimelineID,
		Editor: editor, Content: content, Reason: reason, SnapshotReason: snap, CallID: rec.ID,
		Fingerprint: inputs.Fingerprint})
	if err != nil && req.Force && isVersionConflict(err) {
		saved, err = s.forceSave(ctx, r, facts, v, reason, snap, inputs.Fingerprint)
	}
	if err != nil {
		if isVersionConflict(err) {
			return PlanResult{}, withCallID(err, rec.ID)
		}
		return PlanResult{}, err
	}
	if rec.Status == repo.StatusSuccess {
		if err := s.plan.Calls.SetApplied(ctx, rec.ID, req.Variant); err != nil {
			s.plan.Log.WarnContext(ctx, "AI 剪辑的当前方案没记下来", "callId", rec.ID, "error", err)
		}
	}
	applied := req.Variant
	out := PlanResult{CallID: rec.ID, AppliedVariant: &applied, Timeline: saved.Timeline, SnapshotID: saved.SnapshotID,
		Changes: stored.Changes}
	for _, sv := range stored.Variants {
		out.Variants = append(out.Variants, sv.VariantResult)
	}
	return out, nil
}

// ---------------------------------------------------------------- 配置

// PlanConfigView 是 `GET /smartcut/config`（契约 SmartcutConfig）：入口置灰与说明都靠它（SC-F-88）。只读，不占额度。
type PlanConfigView struct {
	Available    bool
	Reason       string
	UserFixable  bool
	UsedThisHour int
	PerHour      int
	Concurrency  int
	Limits       map[string]int
	// Models 剪辑台各操作可选的模型（识别 / 去字幕 / 配乐 / 音效 / 补镜），由 SCT-33、37~39 补齐。
	Models         map[string][]ModelChoice
	TranslateLangs []string
}

// ModelChoice 一个可选模型（契约 SmartcutModelChoice）。
type ModelChoice struct {
	Code        string
	Name        string
	Granularity string
	MinSec      int
	MaxSec      int
	UnitPrice   int
	Unit        string
}

// TranslateLangs 字幕翻译支持的语种（SC-PARAM-18）。
var TranslateLangs = []string{"zh-CN", "en", "ja", "ko"}

// PlanConfig 回答「现在能不能 AI 剪辑、本小时用了几次、各种上限」。
//
// ⚠️ 可用性与 /plan 是**同一个函数**（Catalog.SmartcutPlanChoice）：各写一份的话会出现「入口说能用、点下去 422」。
// 限流的计数只用于展示，是否放行仍以 /plan 那一刻为准。
func (s *Service) PlanConfig(ctx context.Context, userID, memberLevelID int64) (PlanConfigView, error) {
	if s.plan.Catalog == nil || s.plan.Calls == nil {
		return PlanConfigView{}, httpx.Errf(errcode.Internal, "AI 剪辑链路未接入")
	}
	cfg := s.plan.Config
	out := PlanConfigView{PerHour: cfg.PerHour, Concurrency: cfg.Concurrency, TranslateLangs: TranslateLangs,
		Limits: map[string]int{
			"inputs": domain.MaxInputItems, "outputsPerSource": domain.MaxOutputsPerSource,
			"clips": timelinedomain.MaxClips, "audioTracks": timelinedomain.MaxAudioTracks,
			"subtitleTracks": timelinedomain.MaxSubtitleTracks, "textTracks": timelinedomain.MaxTextTracks,
			"textLayers": timelinedomain.MaxTextLayers, "subtitles": timelinedomain.MaxSubtitles,
			"timelineMs": timelinedomain.MaxTimelineMs, "targetSec": MaxTargetSec, "variants": MaxPlanVariants,
			"instructionChars": MaxInstructionChars,
		},
		Models: map[string][]ModelChoice{"speech2text": {}, "video_desubtitle": {}, "text2music": {}, "video2audio": {},
			"fill": {}},
	}
	_, unavailable, err := s.plan.Catalog.SmartcutPlanChoice(ctx, memberLevelID, 0)
	if err != nil {
		return PlanConfigView{}, httpx.Wrap(errcode.Internal, err)
	}
	if unavailable != nil {
		out.Reason, out.UserFixable = unavailable.Reason, unavailable.UserFixable
	} else {
		out.Available = true
	}
	_, used, err := s.plan.Calls.Usage(ctx, userID)
	if err != nil {
		return PlanConfigView{}, httpx.Wrap(errcode.Internal, err)
	}
	out.UsedThisHour = used
	// 识别模型（SCT-33）：与报价同一个目录函数；能力（字级 / 词级 / 只有文字）决定能不能用于口播精剪。
	if s.transcribe.Catalog != nil {
		models, err := s.transcribe.Catalog.SpeechModels(ctx, memberLevelID)
		if err != nil {
			return PlanConfigView{}, httpx.Wrap(errcode.Internal, err)
		}
		if models != nil {
			out.Models["speech2text"] = models
		}
	}
	// 片段级工具的模型（SCT-38~40）：同样与受理同一个报价函数，按各工具要的输入报价；报不出价的不列。
	if s.clipTools.Models != nil {
		for tool := range clipToolModels {
			models, err := s.clipModelChoices(ctx, tool, memberLevelID)
			if err != nil {
				return PlanConfigView{}, httpx.Wrap(errcode.Internal, err)
			}
			out.Models[tool] = models
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- 恢复扫描与保留期

// RecoverStaleCalls 把「进行中」超过 StaleAfter 的调用判失败，释放它们占着的并发额度（API 的恢复扫描调用）。
// 免费，所以没有退款分支。多个 API 实例同时扫也安全（判失败是 CAS）。
func (s *Service) RecoverStaleCalls(ctx context.Context) (int, error) {
	if s.plan.Calls == nil {
		return 0, nil
	}
	total := 0
	for {
		n, err := s.plan.Calls.RecoverStale(ctx, s.plan.Config.StaleAfter, 100)
		total += n
		if err != nil || n < 100 {
			return total, err
		}
	}
}

// CleanupCalls 分批删除超过保留期的调用记录（SC-PARAM-22），返回删了几条。
func (s *Service) CleanupCalls(ctx context.Context) (int, error) {
	if s.plan.Calls == nil {
		return 0, nil
	}
	before := s.now().Add(-s.plan.Config.Retention)
	total := 0
	for {
		n, err := s.plan.Calls.DeleteOlderThan(ctx, before, 1000)
		total += n
		if err != nil || n < 1000 {
			return total, err
		}
		// 批间让出：连续大批删除会把锁一直占着，堵住正在写调用记录的请求。
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// ---------------------------------------------------------------- 阶段事件

// SmartcutUpdatedType 是事件的 type。
const SmartcutUpdatedType = "smartcut.updated"

// SmartcutUpdated 是 api/events/smartcut-updated.schema.json 的 Go 形状（测试直接读 schema 核对）。
// 只是对账信号：节点卡收到后刷新摘要。
type SmartcutUpdated struct {
	Type         string  `json:"type"`
	OccurredAt   string  `json:"occurredAt"`
	ProjectID    string  `json:"projectId"`
	TimelineID   string  `json:"timelineId"`
	CanvasNodeID string  `json:"canvasNodeId"`
	CallID       *string `json:"callId"`
	Stage        *string `json:"stage"`
	Status       *string `json:"status"`
	// Analysis 素材分析有了结果（识别的完成 / 失败，SCT-33）。只在这类事件里带。
	Analysis *SmartcutAnalysisEvent `json:"analysis,omitempty"`
}

// SmartcutAnalysisEvent 事件里的分析段（schema 的 analysis）。
type SmartcutAnalysisEvent struct {
	MediaID string `json:"mediaId"`
	Kind    string `json:"kind"`
	Status  string `json:"status"`
}

// publishCall 推送调用的阶段到 project:{id}。失败只告警：阶段只是进度信号，请求的响应才是结果。
func (s *Service) publishCall(ctx context.Context, r *planRun, stage, status string) {
	if s.plan.Events == nil || r.nodeID == "" {
		return
	}
	id := strconv.FormatInt(r.callID, 10)
	payload, err := json.Marshal(SmartcutUpdated{Type: SmartcutUpdatedType, OccurredAt: s.now().UTC().Format(time.RFC3339Nano),
		ProjectID: strconv.FormatInt(r.pid, 10), TimelineID: strconv.FormatInt(r.tid, 10), CanvasNodeID: r.nodeID,
		CallID: &id, Stage: &stage, Status: &status})
	if err == nil {
		err = s.plan.Events.Publish(ctx, "project:"+strconv.FormatInt(r.pid, 10), payload)
	}
	if err != nil {
		s.plan.Log.WarnContext(ctx, "推送 AI 剪辑阶段失败", "callId", r.callID, "stage", stage, "error", err)
	}
}

// activeCalls 摘要里的进行中调用（SCD-21）。
func (s *Service) activeCalls(ctx context.Context, workspaceID int64, list []NodeTimeline) map[int64]*ActiveCall {
	out := map[int64]*ActiveCall{}
	if s.plan.Calls == nil || len(list) == 0 {
		return out
	}
	ids := make([]int64, len(list))
	for i, t := range list {
		ids[i] = t.ID
	}
	got, err := s.plan.Calls.ActiveCalls(ctx, workspaceID, ids)
	if err != nil {
		// 摘要不该因为进度读不到而失败：节点卡只是暂时看不到阶段。
		s.plan.Log.WarnContext(ctx, "摘要里的 AI 剪辑进度没读到", "error", err)
		return out
	}
	for tid, a := range got {
		out[tid] = &ActiveCall{ID: a.ID, Kind: a.Kind, Stage: a.Stage, StartedAt: a.StartedAt}
	}
	return out
}
