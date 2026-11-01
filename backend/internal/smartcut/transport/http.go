// Package transport 暴露智能剪辑的用户端接口（docs/agent/smartcut dev/02 §4.1）。
package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	collabdomain "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	collabtransport "github.com/li904338203/tabtv/backend/internal/collaboration/transport"
	smartcutapp "github.com/li904338203/tabtv/backend/internal/smartcut/app"
	smartcutdomain "github.com/li904338203/tabtv/backend/internal/smartcut/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// Handler 智能剪辑的路由。
type Handler struct {
	svc *smartcutapp.Service
	// userIDs 由身份域注入（本域不该知道 JWT claims 长什么样）。
	userIDs func(ctx context.Context) (int64, bool)
	// memberLevel 与生成受理同一个解析器（cmd/api 的 memberLevelOf）：选模型时看的会员等级只有一份。
	memberLevel func(*http.Request) int64
}

func NewHandler(svc *smartcutapp.Service) *Handler {
	return &Handler{svc: svc, userIDs: func(context.Context) (int64, bool) { return 0, false },
		memberLevel: func(*http.Request) int64 { return 0 }}
}

// SetUserIDs 接上当前用户的解析（AI 剪辑按用户限流）。
func (h *Handler) SetUserIDs(f func(ctx context.Context) (int64, bool)) {
	if f != nil {
		h.userIDs = f
	}
}

// SetMemberLevelResolver 接上会员等级解析器。
func (h *Handler) SetMemberLevelResolver(f func(*http.Request) int64) {
	if f != nil {
		h.memberLevel = f
	}
}

// Mount 挂在业务路由组：`guard` 是工作区上下文 + 默认动作闸（GET → view，其余 → edit，P12-03）。
// 建时间线是写入（edit），摘要只读（view）——都落在默认闸上，不另标。
func (h *Handler) Mount(r chi.Router, requireUser, guard func(http.Handler) http.Handler) {
	r.Group(func(r chi.Router) {
		r.Use(requireUser, guard)
		r.Get("/projects/{projectId}/smartcuts", h.list)
		r.Post("/projects/{projectId}/smartcuts", h.create)
		// 输入同步会写时间线（连线即入轨），落在默认的 edit；素材条只读（view）。
		r.Post("/projects/{projectId}/smartcuts/{timelineId}/sync", h.sync)
		r.Get("/projects/{projectId}/smartcuts/{timelineId}/inputs", h.inputs)
		// AI 剪辑与方案切换写时间线（edit）；**不标 generate**（SCD-40）：AI 剪辑免费，generate 是「会花用户的钱」的标记。
		r.Post("/projects/{projectId}/smartcuts/{timelineId}/plan", h.plan)
		r.Post("/projects/{projectId}/smartcuts/{timelineId}/plan/{callId}/apply", h.apply)
		// 追加指令（SCT-29）：同样免费、写时间线（edit），与 AI 剪辑共用限流。
		r.Post("/projects/{projectId}/smartcuts/{timelineId}/edit", h.edit)
		// 字幕翻译（SCT-36）：免费、与 AI 剪辑共用限流，写时间线（edit）。
		r.Post("/projects/{projectId}/smartcuts/{timelineId}/translate", h.translate)
		// 可用性与额度（只读，view）。
		r.Get("/smartcut/config", h.config)
		// 语音识别（SCT-33）：按模型收费 → generate（SCD-40）。dryRun 只报价，也要 generate：报价本身不花钱，
		// 但只读成员看到报价之后点不动「确认」，不如在入口就说清楚。
		r.With(collabtransport.RequireAction(collabdomain.ActionGenerate)).
			Post("/projects/{projectId}/smartcuts/{timelineId}/transcribe", h.transcribe)
		// 片段级生成（SCT-37）：按模型收费 → generate；生成记录与生成信息只读（view）。
		r.With(collabtransport.RequireAction(collabdomain.ActionGenerate)).
			Post("/projects/{projectId}/smartcuts/{timelineId}/clips/{clipId}/regenerate", h.regenerate)
		r.With(collabtransport.RequireAction(collabdomain.ActionGenerate)).
			Post("/projects/{projectId}/smartcuts/{timelineId}/fill", h.fill)
		r.With(collabtransport.RequireAction(collabdomain.ActionGenerate)).
			Post("/projects/{projectId}/smartcuts/{timelineId}/clips/{clipId}/desubtitle", h.desubtitle)
		// 生成配乐 / 按画面生成音效（SCT-39）：按模型收费 → generate。
		r.With(collabtransport.RequireAction(collabdomain.ActionGenerate)).
			Post("/projects/{projectId}/smartcuts/{timelineId}/music", h.music)
		r.With(collabtransport.RequireAction(collabdomain.ActionGenerate)).
			Post("/projects/{projectId}/smartcuts/{timelineId}/clips/{clipId}/sfx", h.sfx)
		r.Get("/projects/{projectId}/smartcuts/{timelineId}/generations", h.generations)
		r.Get("/projects/{projectId}/smartcuts/{timelineId}/clips/{clipId}/origin", h.clipOrigin)
	})
}

// ---------------------------------------------------------------- 片段级生成（SCT-37）

// clipToolRequest 片段级生成请求的公共部分（工作区、付费主体、项目、时间线、用户、会员等级、幂等键）。
func (h *Handler) clipToolRequest(w http.ResponseWriter, r *http.Request, dryRun bool) (smartcutapp.ClipToolRequest, bool) {
	wc, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
		return smartcutapp.ClipToolRequest{}, false
	}
	_, pid, ok := projectScope(w, r)
	if !ok {
		return smartcutapp.ClipToolRequest{}, false
	}
	tid, ok := timelineParam(w, r)
	if !ok {
		return smartcutapp.ClipToolRequest{}, false
	}
	actor, ok := h.actor(w, r)
	if !ok {
		return smartcutapp.ClipToolRequest{}, false
	}
	return smartcutapp.ClipToolRequest{WorkspaceID: wc.WorkspaceID, PayerAccountID: wc.PayerAccountID, ProjectID: pid,
		TimelineID: tid, UserID: actor.UserID, MemberLevelID: h.memberLevel(r), DryRun: dryRun,
		IdempotencyKey: r.Header.Get("Idempotency-Key")}, true
}

func clipParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "clipId"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "clipId 非法"))
		return 0, false
	}
	return id, true
}

// optionalID 请求体里可选的 ID（字符串）；空串 = 没给。
func optionalID(w http.ResponseWriter, r *http.Request, raw, path string) (int64, bool) {
	if strings.TrimSpace(raw) == "" {
		return 0, true
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, path+" 非法").WithDetails(map[string]any{"path": path}))
		return 0, false
	}
	return id, true
}

func (h *Handler) regenerate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Prompt string `json:"prompt"`
		DryRun bool   `json:"dryRun"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidJSON, "请求体不是合法 JSON"))
		return
	}
	base, ok := h.clipToolRequest(w, r, req.DryRun)
	if !ok {
		return
	}
	clipID, ok := clipParam(w, r)
	if !ok {
		return
	}
	res, err := h.svc.RegenerateClip(r.Context(), smartcutapp.RegenerateRequest{ClipToolRequest: base, ClipID: clipID,
		Prompt: req.Prompt})
	writeClipTool(w, r, res, err)
}

func (h *Handler) fill(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AfterClipID   string `json:"afterClipId"`
		Prompt        string `json:"prompt"`
		ModelCode     string `json:"modelCode"`
		Operation     string `json:"operation"`
		DurationSec   int    `json:"durationSec"`
		RefFromClipID string `json:"refFromClipId"`
		DryRun        bool   `json:"dryRun"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidJSON, "请求体不是合法 JSON"))
		return
	}
	base, ok := h.clipToolRequest(w, r, req.DryRun)
	if !ok {
		return
	}
	after, ok := optionalID(w, r, req.AfterClipID, "afterClipId")
	if !ok {
		return
	}
	ref, ok := optionalID(w, r, req.RefFromClipID, "refFromClipId")
	if !ok {
		return
	}
	res, err := h.svc.FillGap(r.Context(), smartcutapp.FillRequest{ClipToolRequest: base, AfterClipID: after, Prompt: req.Prompt,
		ModelCode: req.ModelCode, Operation: req.Operation, DurationSec: req.DurationSec, RefFromClipID: ref})
	writeClipTool(w, r, res, err)
}

func (h *Handler) desubtitle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ModelCode string `json:"modelCode"`
		Mode      string `json:"mode"`
		DryRun    bool   `json:"dryRun"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidJSON, "请求体不是合法 JSON"))
		return
	}
	base, ok := h.clipToolRequest(w, r, req.DryRun)
	if !ok {
		return
	}
	clipID, ok := clipParam(w, r)
	if !ok {
		return
	}
	res, err := h.svc.DesubtitleClip(r.Context(), smartcutapp.DesubtitleRequest{ClipToolRequest: base, ClipID: clipID,
		ModelCode: req.ModelCode, Mode: req.Mode})
	writeClipTool(w, r, res, err)
}

func (h *Handler) music(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Prompt       string `json:"prompt"`
		ModelCode    string `json:"modelCode"`
		DurationSec  int    `json:"durationSec"`
		AtMs         int    `json:"atMs"`
		Instrumental *bool  `json:"instrumental"`
		DryRun       bool   `json:"dryRun"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidJSON, "请求体不是合法 JSON"))
		return
	}
	base, ok := h.clipToolRequest(w, r, req.DryRun)
	if !ok {
		return
	}
	res, err := h.svc.GenerateMusic(r.Context(), smartcutapp.MusicRequest{ClipToolRequest: base, Prompt: req.Prompt,
		ModelCode: req.ModelCode, DurationSec: req.DurationSec, AtMs: req.AtMs, Instrumental: req.Instrumental})
	writeClipTool(w, r, res, err)
}

func (h *Handler) sfx(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Prompt    string `json:"prompt"`
		ModelCode string `json:"modelCode"`
		DryRun    bool   `json:"dryRun"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidJSON, "请求体不是合法 JSON"))
		return
	}
	base, ok := h.clipToolRequest(w, r, req.DryRun)
	if !ok {
		return
	}
	clipID, ok := clipParam(w, r)
	if !ok {
		return
	}
	res, err := h.svc.GenerateSfx(r.Context(), smartcutapp.SfxRequest{ClipToolRequest: base, ClipID: clipID,
		Prompt: req.Prompt, ModelCode: req.ModelCode})
	writeClipTool(w, r, res, err)
}

// writeClipTool dryRun 回报价（200），受理回 SmartcutGeneration（202）。
func writeClipTool(w http.ResponseWriter, r *http.Request, res smartcutapp.ClipToolResult, err error) {
	switch {
	case err != nil:
		httpx.Fail(w, r, err)
	case res.Quote != nil:
		httpx.JSON(w, http.StatusOK, clipQuoteResp(*res.Quote))
	case res.Accepted != nil:
		httpx.JSON(w, http.StatusAccepted, generationResp(*res.Accepted))
	}
}

func clipQuoteResp(q smartcutapp.ClipQuote) map[string]any {
	blockers := []map[string]any{}
	items := 1
	if q.Blocker != nil {
		blockers = append(blockers, map[string]any{"code": q.Blocker.Code, "message": q.Blocker.Message,
			"userFixable": q.Blocker.UserFixable})
		items = 0
	}
	out := map[string]any{"credits": q.Credits, "modelCode": q.ModelCode, "items": items, "blockers": blockers}
	if q.Unit != "" {
		out["unitPrice"], out["unit"] = q.UnitPrice, q.Unit
	}
	if q.DurationSec > 0 {
		out["durationSec"] = q.DurationSec
	}
	return out
}

func generationResp(g smartcutapp.SmartcutGenerationView) map[string]any {
	out := map[string]any{"generationId": strconv.FormatInt(g.GenerationID, 10), "kind": g.Kind, "status": g.Status,
		"progress": g.Progress, "credits": g.Credits, "createdAt": g.CreatedAt.UTC().Format(time.RFC3339)}
	if g.ClipID != "" {
		out["clipId"] = g.ClipID
	}
	if g.MediaID != 0 {
		out["mediaId"] = strconv.FormatInt(g.MediaID, 10)
	}
	return out
}

func (h *Handler) generations(w http.ResponseWriter, r *http.Request) {
	ws, pid, ok := projectScope(w, r)
	if !ok {
		return
	}
	tid, ok := timelineParam(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, next, err := h.svc.Generations(r.Context(), ws, pid, tid, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(list))
	for _, g := range list {
		item := generationResp(g.SmartcutGenerationView)
		item["landed"] = g.Landed
		if g.OutputMediaID != 0 {
			item["outputMediaId"] = strconv.FormatInt(g.OutputMediaID, 10)
		}
		if g.Error != "" {
			item["error"] = g.Error
		}
		items = append(items, item)
	}
	out := map[string]any{"items": items}
	if next != "" {
		out["nextCursor"] = next
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) clipOrigin(w http.ResponseWriter, r *http.Request) {
	ws, pid, ok := projectScope(w, r)
	if !ok {
		return
	}
	tid, ok := timelineParam(w, r)
	if !ok {
		return
	}
	clipID, ok := clipParam(w, r)
	if !ok {
		return
	}
	o, err := h.svc.ClipOriginOf(r.Context(), ws, pid, tid, clipID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := map[string]any{"kind": o.Kind, "canRegenerate": o.CanRegenerate}
	if o.GenerationID != 0 {
		out["generationId"] = strconv.FormatInt(o.GenerationID, 10)
		out["operationCode"], out["modelCode"], out["modelName"], out["prompt"] = o.OperationCode, o.ModelCode, o.ModelName, o.Prompt
		out["params"], out["inputs"] = o.Params, o.Inputs
	}
	if o.Reason != "" {
		out["reason"] = o.Reason
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	ws, pid, ok := projectScope(w, r)
	if !ok {
		return
	}
	list, err := h.svc.List(r.Context(), ws, pid)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(list))
	for _, s := range list {
		items = append(items, summaryResp(s))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	ws, pid, ok := projectScope(w, r)
	if !ok {
		return
	}
	var req struct {
		CanvasNodeID     string `json:"canvasNodeId"`
		SourceTimelineID string `json:"sourceTimelineId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidJSON, "请求体不是合法 JSON"))
		return
	}
	var source int64
	if s := strings.TrimSpace(req.SourceTimelineID); s != "" {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil || id <= 0 {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "sourceTimelineId 非法").
				WithDetails(map[string]any{"path": "sourceTimelineId"}))
			return
		}
		source = id
	}
	sum, created, err := h.svc.Create(r.Context(), ws, pid, req.CanvasNodeID, source)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	// 已有时回 200 而不是 201（同导演台 DRD-17）：前端据此区分「新建」与「收敛到已有那条」，两种都写回 timelineId。
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpx.JSON(w, status, summaryResp(sum))
}

func summaryResp(s smartcutapp.Summary) map[string]any {
	strip := make([]map[string]any, 0, len(s.Strip))
	for _, it := range s.Strip {
		item := map[string]any{"kind": it.Kind, "startMs": it.StartMs, "durationMs": it.DurationMs}
		if it.PosterMediaID != 0 {
			item["posterMediaId"] = strconv.FormatInt(it.PosterMediaID, 10)
		}
		if it.MediaID != 0 {
			item["mediaId"] = strconv.FormatInt(it.MediaID, 10)
		}
		strip = append(strip, item)
	}
	out := map[string]any{
		"timelineId": strconv.FormatInt(s.TimelineID, 10), "canvasNodeId": s.CanvasNodeID,
		"version": s.Version, "isArranged": s.IsArranged, "totalMs": s.TotalMs, "effectiveMs": s.EffectiveMs,
		"clipCount": s.ClipCount, "inputsStale": s.InputsStale, "renderStale": s.RenderStale,
		"strip": strip, "updatedAt": s.UpdatedAt.UTC().Format(time.RFC3339), "activeCall": nil,
	}
	if s.CoverMediaID != 0 {
		out["coverMediaId"] = strconv.FormatInt(s.CoverMediaID, 10)
	}
	if b := s.LatestRender; b != nil {
		lr := map[string]any{
			"id": strconv.FormatInt(b.ID, 10), "status": b.Status, "progress": b.Progress,
			"timelineVersion": b.TimelineVersion, "stage": nil,
		}
		if b.Stage != "" {
			lr["stage"] = b.Stage
		}
		if b.OutputMediaID != 0 {
			lr["outputMediaId"] = strconv.FormatInt(b.OutputMediaID, 10)
		}
		if s.OutputPosterID != 0 {
			lr["outputPosterId"] = strconv.FormatInt(s.OutputPosterID, 10)
		}
		if len(b.Spec) > 0 {
			lr["spec"] = b.Spec
		}
		if b.ErrorMessage != "" {
			lr["errorMessage"] = b.ErrorMessage
		}
		out["latestRender"] = lr
	}
	if c := s.ActiveCall; c != nil {
		out["activeCall"] = map[string]any{"id": strconv.FormatInt(c.ID, 10), "kind": c.Kind, "stage": c.Stage,
			"startedAt": c.StartedAt.UTC().Format(time.RFC3339)}
	}
	return out
}

func projectScope(w http.ResponseWriter, r *http.Request) (int64, int64, bool) {
	wc, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
		return 0, 0, false
	}
	pid, err := strconv.ParseInt(chi.URLParam(r, "projectId"), 10, 64)
	if err != nil || pid <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "projectId 非法"))
		return 0, 0, false
	}
	return wc.WorkspaceID, pid, true
}

func (h *Handler) sync(w http.ResponseWriter, r *http.Request) {
	ws, pid, ok := projectScope(w, r)
	if !ok {
		return
	}
	tid, ok := timelineParam(w, r)
	if !ok {
		return
	}
	var req struct {
		Mode        string `json:"mode"`
		BaseVersion int    `json:"baseVersion"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidJSON, "请求体不是合法 JSON"))
		return
	}
	var res smartcutapp.SyncResult
	var err error
	if req.Mode == smartcutapp.SyncReplaceChanged {
		// 原位替换（SCT-30）：写时间线、存快照，要操作者与 baseVersion。
		actor, ok := h.actor(w, r)
		if !ok {
			return
		}
		res, err = h.svc.ReplaceChanged(r.Context(), smartcutapp.ReplaceRequest{WorkspaceID: ws, ProjectID: pid,
			TimelineID: tid, BaseVersion: req.BaseVersion, Actor: actor})
	} else {
		res, err = h.svc.Sync(r.Context(), ws, pid, tid, req.Mode)
	}
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	shortened := make([]map[string]any, 0, len(res.Shortened))
	for _, sh := range res.Shortened {
		shortened = append(shortened, map[string]any{"clipId": strconv.FormatInt(sh.ClipID, 10), "fromMs": sh.FromMs,
			"toMs": sh.ToMs})
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"inputs": inputsOrEmpty(res.Inputs), "omitted": res.Omitted,
		"appendedClipIds": idStrings(res.AppendedClipIDs), "disconnectedClipIds": idStrings(res.DisconnectedClipIDs),
		"replacedClipIds": idStrings(res.ReplacedClipIDs), "shortened": shortened,
		"unusedCount": res.UnusedCount, "fingerprint": res.Fingerprint, "timelineVersion": res.TimelineVersion,
	})
}

func (h *Handler) inputs(w http.ResponseWriter, r *http.Request) {
	ws, pid, ok := projectScope(w, r)
	if !ok {
		return
	}
	tid, ok := timelineParam(w, r)
	if !ok {
		return
	}
	res, err := h.svc.Inputs(r.Context(), ws, pid, tid)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": inputsOrEmpty(res.Items), "omitted": res.Omitted,
		"fingerprint": res.Fingerprint})
}

func timelineParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "timelineId"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "timelineId 非法"))
		return 0, false
	}
	return id, true
}

func inputsOrEmpty(items []smartcutdomain.Input) []smartcutdomain.Input {
	if items == nil {
		return []smartcutdomain.Input{}
	}
	return items
}

func idStrings(ids []int64) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, strconv.FormatInt(id, 10))
	}
	return out
}

// ---------------------------------------------------------------- AI 剪辑（SCT-27）

func (h *Handler) actor(w http.ResponseWriter, r *http.Request) (smartcutapp.Actor, bool) {
	uid, ok := h.userIDs(r.Context())
	if !ok || uid <= 0 {
		httpx.Fail(w, r, httpx.Err(errcode.Unauthenticated))
		return smartcutapp.Actor{}, false
	}
	return smartcutapp.Actor{UserID: uid}, true
}

func (h *Handler) plan(w http.ResponseWriter, r *http.Request) {
	ws, pid, ok := projectScope(w, r)
	if !ok {
		return
	}
	tid, ok := timelineParam(w, r)
	if !ok {
		return
	}
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	var req struct {
		Mode        string `json:"mode"`
		Instruction string `json:"instruction"`
		Spec        struct {
			AspectRatio string `json:"aspectRatio"`
			Resolution  string `json:"resolution"`
			TargetSec   int    `json:"targetSec"`
		} `json:"spec"`
		Variants      int   `json:"variants"`
		BaseVersion   int   `json:"baseVersion"`
		AllowSlow     bool  `json:"allowSlow"`
		AllowRepeat   bool  `json:"allowRepeat"`
		WithSubtitles *bool `json:"withSubtitles"`
		IgnoreBeat    bool  `json:"ignoreBeat"`
		KeepFillers   bool  `json:"keepFillers"`
		Force         bool  `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidJSON, "请求体不是合法 JSON"))
		return
	}
	res, err := h.svc.Plan(r.Context(), smartcutapp.PlanRequest{
		WorkspaceID: ws, ProjectID: pid, TimelineID: tid, Actor: actor, MemberLevelID: h.memberLevel(r),
		Mode: req.Mode, Instruction: req.Instruction, TargetSec: req.Spec.TargetSec, AspectRatio: req.Spec.AspectRatio,
		Resolution: req.Spec.Resolution, Variants: req.Variants, BaseVersion: req.BaseVersion, AllowSlow: req.AllowSlow,
		AllowRepeat: req.AllowRepeat, WithSubtitles: req.WithSubtitles, IgnoreBeat: req.IgnoreBeat,
		KeepFillers: req.KeepFillers, Force: req.Force,
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, planResp(res))
}

func (h *Handler) apply(w http.ResponseWriter, r *http.Request) {
	ws, pid, ok := projectScope(w, r)
	if !ok {
		return
	}
	tid, ok := timelineParam(w, r)
	if !ok {
		return
	}
	callID, err := strconv.ParseInt(chi.URLParam(r, "callId"), 10, 64)
	if err != nil || callID <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "callId 非法"))
		return
	}
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	var req struct {
		Variant     int  `json:"variant"`
		BaseVersion int  `json:"baseVersion"`
		Force       bool `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidJSON, "请求体不是合法 JSON"))
		return
	}
	res, err := h.svc.ApplyVariant(r.Context(), smartcutapp.ApplyRequest{WorkspaceID: ws, ProjectID: pid, TimelineID: tid,
		CallID: callID, Variant: req.Variant, BaseVersion: req.BaseVersion, Force: req.Force, Actor: actor})
	if err != nil {
		fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, planResp(res))
}

func (h *Handler) edit(w http.ResponseWriter, r *http.Request) {
	ws, pid, ok := projectScope(w, r)
	if !ok {
		return
	}
	tid, ok := timelineParam(w, r)
	if !ok {
		return
	}
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	var req struct {
		Instruction string `json:"instruction"`
		BaseVersion int    `json:"baseVersion"`
		Force       bool   `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidJSON, "请求体不是合法 JSON"))
		return
	}
	res, err := h.svc.Edit(r.Context(), smartcutapp.EditRequest{WorkspaceID: ws, ProjectID: pid, TimelineID: tid,
		Actor: actor, MemberLevelID: h.memberLevel(r), Instruction: req.Instruction, BaseVersion: req.BaseVersion,
		Force: req.Force})
	if err != nil {
		fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, planResp(res))
}

func (h *Handler) translate(w http.ResponseWriter, r *http.Request) {
	ws, pid, ok := projectScope(w, r)
	if !ok {
		return
	}
	tid, ok := timelineParam(w, r)
	if !ok {
		return
	}
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	var req struct {
		SubtitleTrackID string `json:"subtitleTrackId"`
		TargetLang      string `json:"targetLang"`
		Mode            string `json:"mode"`
		BaseVersion     int    `json:"baseVersion"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidJSON, "请求体不是合法 JSON"))
		return
	}
	track, err := strconv.ParseInt(req.SubtitleTrackID, 10, 64)
	if err != nil || track <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "subtitleTrackId 非法").
			WithDetails(map[string]any{"path": "subtitleTrackId"}))
		return
	}
	res, err := h.svc.Translate(r.Context(), smartcutapp.TranslateRequest{WorkspaceID: ws, ProjectID: pid, TimelineID: tid,
		Actor: actor, MemberLevelID: h.memberLevel(r), SubtitleTrackID: track, TargetLang: strings.TrimSpace(req.TargetLang),
		Mode: strings.TrimSpace(req.Mode), BaseVersion: req.BaseVersion})
	if err != nil {
		fail(w, r, err)
		return
	}
	failed := res.Failed
	if failed == nil {
		failed = []smartcutapp.TranslateFailure{}
	}
	out := map[string]any{"trackId": strconv.FormatInt(res.TrackID, 10), "translated": res.Translated, "failed": failed}
	if len(res.Timeline) > 0 {
		out["timeline"] = json.RawMessage(res.Timeline)
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) config(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	c, err := h.svc.PlanConfig(r.Context(), actor.UserID, h.memberLevel(r))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	models := map[string]any{}
	for k, list := range c.Models {
		items := make([]map[string]any, 0, len(list))
		for _, m := range list {
			item := map[string]any{"code": m.Code, "name": m.Name}
			if m.Granularity != "" {
				item["granularity"] = m.Granularity
			}
			if m.MinSec > 0 {
				item["minSec"] = m.MinSec
			}
			if m.MaxSec > 0 {
				item["maxSec"] = m.MaxSec
			}
			if m.Unit != "" {
				item["unitPrice"], item["unit"] = m.UnitPrice, m.Unit
			}
			items = append(items, item)
		}
		models[k] = items
	}
	out := map[string]any{"available": c.Available, "userFixable": c.UserFixable, "usedThisHour": c.UsedThisHour,
		"perHour": c.PerHour, "concurrency": c.Concurrency, "limits": c.Limits, "models": models,
		"translateLangs": c.TranslateLangs}
	if c.Reason != "" {
		out["reason"] = c.Reason
	}
	httpx.JSON(w, http.StatusOK, out)
}

func planResp(res smartcutapp.PlanResult) map[string]any {
	variants := make([]smartcutapp.VariantResult, 0, len(res.Variants))
	variants = append(variants, res.Variants...)
	out := map[string]any{"callId": strconv.FormatInt(res.CallID, 10), "appliedVariant": res.AppliedVariant,
		"variants": variants, "changes": nonNilStrings(res.Changes)}
	if len(res.Timeline) > 0 {
		out["timeline"] = json.RawMessage(res.Timeline)
	}
	if res.SnapshotID != 0 {
		out["snapshotId"] = strconv.FormatInt(res.SnapshotID, 10)
	}
	return out
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// fail 回错误；限流时另设 Retry-After 头（同导演台与 MCP 的限流）。
func fail(w http.ResponseWriter, r *http.Request, err error) {
	var be *httpx.BizError
	if errors.As(err, &be) && (be.Code == errcode.RateLimited || be.Code == errcode.ConcurrencyLimited) {
		if secs, ok := be.Details["retry_after_seconds"].(int); ok && secs > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(secs))
		}
	}
	httpx.Fail(w, r, err)
}

// transcribe 语音识别（SCT-33）：dryRun 回 200 报价，确认后回 202 受理结果。
func (h *Handler) transcribe(w http.ResponseWriter, r *http.Request) {
	wc, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
		return
	}
	_, pid, ok := projectScope(w, r)
	if !ok {
		return
	}
	tid, ok := timelineParam(w, r)
	if !ok {
		return
	}
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	var req struct {
		ClipIDs     []string `json:"clipIds"`
		ModelCode   string   `json:"modelCode"`
		KeepFillers bool     `json:"keepFillers"`
		Language    string   `json:"language"`
		Subtitles   bool     `json:"subtitles"`
		DryRun      bool     `json:"dryRun"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidJSON, "请求体不是合法 JSON"))
		return
	}
	in := smartcutapp.TranscribeRequest{WorkspaceID: wc.WorkspaceID, PayerAccountID: wc.PayerAccountID, ProjectID: pid,
		TimelineID: tid, UserID: actor.UserID, MemberLevelID: h.memberLevel(r), ClipIDs: req.ClipIDs,
		ModelCode: strings.TrimSpace(req.ModelCode), KeepFillers: req.KeepFillers, Language: strings.TrimSpace(req.Language),
		Subtitles: req.Subtitles, DryRun: req.DryRun, IdempotencyKey: r.Header.Get("Idempotency-Key")}
	if req.DryRun {
		q, err := h.svc.QuoteTranscribe(r.Context(), in)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		httpx.JSON(w, http.StatusOK, transcribeQuoteResp(q))
		return
	}
	res, err := h.svc.Transcribe(r.Context(), in)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusAccepted, transcribeResp(res))
}

func skipsResp(list []smartcutapp.TranscribeSkip) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, s := range list {
		out = append(out, map[string]any{"mediaId": strconv.FormatInt(s.MediaID, 10), "reason": s.Reason})
	}
	return out
}

func transcribeQuoteResp(q smartcutapp.TranscribeQuote) map[string]any {
	blockers := make([]map[string]any, 0, len(q.Blockers))
	for _, b := range q.Blockers {
		blockers = append(blockers, map[string]any{"code": b.Code, "message": b.Message, "userFixable": b.UserFixable})
	}
	out := map[string]any{"credits": q.Credits, "modelCode": q.ModelCode, "durationSec": q.DurationSec,
		"items": q.Items, "skipped": skipsResp(q.Skipped), "blockers": blockers}
	if q.Unit != "" {
		out["unitPrice"], out["unit"] = q.UnitPrice, q.Unit
	}
	return out
}

func transcribeResp(res smartcutapp.TranscribeResult) map[string]any {
	items := make([]map[string]any, 0, len(res.Items))
	for _, g := range res.Items {
		items = append(items, map[string]any{"generationId": strconv.FormatInt(g.GenerationID, 10), "kind": g.Kind,
			"status": g.Status, "progress": g.Progress, "credits": g.Credits, "mediaId": strconv.FormatInt(g.MediaID, 10),
			"createdAt": g.CreatedAt.UTC().Format(time.RFC3339)})
	}
	out := map[string]any{"items": items, "waiting": skipsResp(res.Waiting), "skipped": skipsResp(res.Skipped),
		"failed": skipsResp(res.Failed)}
	if res.SubtitleError != "" {
		out["subtitleError"] = res.SubtitleError
	}
	return out
}
