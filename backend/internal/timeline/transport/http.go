// Package transport 暴露时间线的用户端接口。
package transport

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	collabdomain "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	collabtransport "github.com/li904338203/tabtv/backend/internal/collaboration/transport"
	timelineapp "github.com/li904338203/tabtv/backend/internal/timeline/app"
	"github.com/li904338203/tabtv/backend/internal/timeline/domain"
	"github.com/li904338203/tabtv/backend/internal/timeline/repo"
	"github.com/li904338203/tabtv/backend/internal/timeline/typeset"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

type Handler struct {
	svc         *timelineapp.Service
	userID      func(*http.Request) int64
	memberLevel func(*http.Request) int64
	// typesetter 文字排版（SCD-28）：字体懒加载，第一次排版时才读字体目录。
	typesetter *typeset.Typesetter
}

func NewHandler(svc *timelineapp.Service, userID func(*http.Request) int64) *Handler {
	return &Handler{
		svc: svc, userID: userID,
		memberLevel: func(*http.Request) int64 { return 0 },
		typesetter:  typeset.New(typeset.Options{}),
	}
}

// SetMemberLevelResolver 注入会员等级解析器（P11 接入）。
func (h *Handler) SetMemberLevelResolver(f func(*http.Request) int64) {
	h.memberLevel = f
}

// editor 这次写入的编辑者：当前用户本人亲手（HTTP 请求都是从界面来的）。
func (h *Handler) editor(r *http.Request) repo.Editor {
	return repo.Editor{UserID: h.userID(r), Kind: repo.EditorUser}
}

// maxContentBytes 整份保存的请求体上限：500 段片段 + 2000 条字幕 + 100 个文字层，按每条几百字节算
// 也在 2 MiB 以内；留到 8 MiB，挡住的只是异常的请求。
const maxContentBytes = 8 << 20

// saveContent 整份保存（SC-I-02）。临时 ID（`tmp-` 开头）在响应的 idMap 里换成正式 ID。
func (h *Handler) saveContent(w http.ResponseWriter, r *http.Request) {
	ws, id, ok := scope(w, r, "timelineID")
	if !ok {
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxContentBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			httpx.Fail(w, r, httpx.Errf(errcode.PayloadTooLarge, "时间线内容超过 8 MiB"))
			return
		}
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidJSON, "请求体读取失败"))
		return
	}
	content, err := domain.DecodeContent(raw)
	if err != nil {
		httpx.Fail(w, r, timelineapp.ContentError(err))
		return
	}
	res, err := h.svc.SaveContent(r.Context(), timelineapp.SaveContentInput{
		WorkspaceID: ws, TimelineID: id, Editor: h.editor(r), Content: content,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"timeline": toResp(res.View), "idMap": res.IDMap})
}

// listSnapshots 快照列表（不含正文）。
func (h *Handler) listSnapshots(w http.ResponseWriter, r *http.Request) {
	ws, id, ok := scope(w, r, "timelineID")
	if !ok {
		return
	}
	list, err := h.svc.ListSnapshots(r.Context(), ws, id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(list))
	for _, sn := range list {
		item := map[string]any{
			"id": strconv.FormatInt(sn.ID, 10), "version": sn.Version, "reason": sn.Reason,
			"editorKind": sn.EditorKind, "createdByName": sn.CreatedByName,
			"createdAt": sn.CreatedAt.UTC().Format(time.RFC3339),
		}
		if sn.CallID != 0 {
			item["callId"] = strconv.FormatInt(sn.CallID, 10)
		}
		if sn.GenerationID != 0 {
			item["generationId"] = strconv.FormatInt(sn.GenerationID, 10)
		}
		items = append(items, item)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
}

// restoreSnapshot 恢复到某份快照（「撤销这轮 AI」也走这里）。
func (h *Handler) restoreSnapshot(w http.ResponseWriter, r *http.Request) {
	ws, id, ok := scope(w, r, "timelineID")
	if !ok {
		return
	}
	snapshotID, ok := pathID(w, r, "snapshotID")
	if !ok {
		return
	}
	var req struct {
		BaseVersion *int `json:"baseVersion"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.BaseVersion == nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "baseVersion 必填").
			WithDetails(map[string]any{"path": "baseVersion"}))
		return
	}
	res, err := h.svc.RestoreSnapshot(r.Context(), timelineapp.RestoreInput{
		WorkspaceID: ws, TimelineID: id, SnapshotID: snapshotID, BaseVersion: *req.BaseVersion,
		Editor: h.editor(r),
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	missing := make([]map[string]any, 0, len(res.Missing))
	for _, m := range res.Missing {
		missing = append(missing, map[string]any{
			"clipId": m.ClipID, "mediaId": strconv.FormatInt(m.MediaID, 10), "trackKind": m.TrackKind,
		})
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"timeline": toResp(res.View), "snapshotId": strconv.FormatInt(res.SnapshotID, 10), "missing": missing,
	})
}

// payer 取付费主体。⚠️ 从**工作区上下文**里取，不从请求体 ——
// 让客户端报付费主体等于让他挑谁付钱。
func (h *Handler) payer(r *http.Request) int64 {
	ws, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		return 0
	}
	return ws.PayerAccountID
}

// Mount 需要两个工作区中间件（P12-03，见 workflow.Mount 的说明）：
// `guard` 带默认动作闸，`wsCtx` 只装上下文、供例外路由自带闸。
func (h *Handler) Mount(r chi.Router, requireUser, guard, wsCtx func(http.Handler) http.Handler) {
	// ⚠️ 渲染端点单独一组：一个端点里免费预检与收费渲染并存，
	// 而区分它们的 `dryRun` 在请求体里、中间件看不到。
	// 路由层放宽到 view（预检报的"第几段缺素材"是**诊断信息**，
	// 只读成员帮忙看问题时需要），真渲染在 startRender 里显式判 export。
	r.Group(func(r chi.Router) {
		r.Use(requireUser, wsCtx, collabtransport.RequireAction(collabdomain.ActionView))
		r.Post("/timelines/{timelineID}/renders", h.startRender)
		// 文字排版（SCD-28）：只读、不扣费、不落库的 POST，放宽到 view（只读成员打开剪辑台也要看得到字幕与文字）。
		r.Post("/smartcut/typeset", typeset.Handler(h.typesetter))
	})
	r.Group(func(r chi.Router) {
		r.Use(requireUser)
		r.Use(guard)
		r.Post("/timelines", h.create)
		r.Get("/timelines", h.list)
		r.Get("/timelines/{timelineID}", h.get)
		r.Patch("/timelines/{timelineID}", h.updateMeta)
		// 按分镜自动装配。⚠️ `dryRun` 让用户**先看再决定** ——
		// 有缺口时他可能想先去把那几镜补出来。
		r.Post("/timelines/{timelineID}/assemble", h.assemble)
		r.Put("/timelines/{timelineID}/subtitles", h.saveSubtitles)
		// 整份保存（SC-I-02）：剪辑台的撤销 / 重做靠它。PUT 落在默认闸的 edit 上。
		r.Put("/timelines/{timelineID}/content", h.saveContent)
		// 快照（SCD-18）：列表只读，恢复是写入（POST 落在默认闸的 edit 上）。
		r.Get("/timelines/{timelineID}/snapshots", h.listSnapshots)
		r.Post("/timelines/{timelineID}/snapshots/{snapshotID}/restore", h.restoreSnapshot)
		r.Patch("/timelines/{timelineID}/tracks/{trackID}", h.updateTrack)
		r.Post("/timelines/{timelineID}/tracks/{trackID}/clips", h.addClip)
		r.Patch("/timelines/{timelineID}/tracks/{trackID}/clips/{clipID}/trim", h.trim)
		r.Post("/timelines/{timelineID}/tracks/{trackID}/clips/{clipID}/split", h.split)
		r.Patch("/timelines/{timelineID}/tracks/{trackID}/clips/{clipID}/move", h.move)
		// 片段属性：变速、音量、淡入淡出、转场（NEW-08、NEW-09）。
		r.Patch("/timelines/{timelineID}/tracks/{trackID}/clips/{clipID}", h.updateClip)
		r.Delete("/timelines/{timelineID}/tracks/{trackID}/clips/{clipID}", h.removeClip)
		// 渲染（NEW-10）的 POST 在上面那一组（权限语义不同）。
		r.Get("/timelines/{timelineID}/renders", h.listRenders)
		r.Get("/renders/{renderID}", h.getRender)
		r.Post("/renders/{renderID}/cancel", h.cancelRender)
		// 成片下载链接（SCK-10）：把内容取出去，要 export（GET 的默认闸只给 view，这里收紧）。
		r.With(collabtransport.RequireAction(collabdomain.ActionExport)).
			Get("/renders/{renderID}/download", h.renderDownload)
	})
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	ws, ok := workspace(w, r)
	if !ok {
		return
	}
	var req struct {
		ProjectID   string `json:"projectId"`
		ScriptID    string `json:"scriptId"`
		Name        string `json:"name"`
		AspectRatio string `json:"aspectRatio"`
		FPS         int    `json:"fps"`
		Width       int    `json:"width"`
		Height      int    `json:"height"`
	}
	if !decode(w, r, &req) {
		return
	}
	projectID, _ := strconv.ParseInt(req.ProjectID, 10, 64)
	scriptID, _ := strconv.ParseInt(req.ScriptID, 10, 64)
	v, err := h.svc.Create(r.Context(), timelineapp.CreateInput{
		WorkspaceID: ws, ProjectID: projectID, ScriptID: scriptID,
		Name: req.Name, AspectRatio: req.AspectRatio, FPS: req.FPS,
		Width: req.Width, Height: req.Height,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, toResp(v))
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	ws, ok := workspace(w, r)
	if !ok {
		return
	}
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("projectId"), 10, 64)
	if projectID == 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "缺少 projectId"))
		return
	}
	list, err := h.svc.List(r.Context(), ws, projectID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, t := range list {
		out = append(out, metaResp(t))
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	ws, id, ok := scope(w, r, "timelineID")
	if !ok {
		return
	}
	v, err := h.svc.Get(r.Context(), ws, id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toResp(v))
}

func (h *Handler) updateMeta(w http.ResponseWriter, r *http.Request) {
	ws, id, ok := scope(w, r, "timelineID")
	if !ok {
		return
	}
	var req struct {
		Version     int    `json:"version"`
		Name        string `json:"name"`
		AspectRatio string `json:"aspectRatio"`
		FPS         int    `json:"fps"`
		Width       int    `json:"width"`
		Height      int    `json:"height"`
	}
	if !decode(w, r, &req) {
		return
	}
	v, err := h.svc.UpdateMeta(r.Context(), ws, id, req.Version, h.editor(r), req.Name,
		req.AspectRatio, req.FPS, req.Width, req.Height)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toResp(v))
}

func (h *Handler) assemble(w http.ResponseWriter, r *http.Request) {
	ws, id, ok := scope(w, r, "timelineID")
	if !ok {
		return
	}
	var req struct {
		Version int  `json:"version"`
		Force   bool `json:"force"`
		DryRun  bool `json:"dryRun"`
	}
	if !decode(w, r, &req) {
		return
	}
	res, err := h.svc.Assemble(r.Context(), timelineapp.AssembleInput{
		Editor:      h.editor(r),
		WorkspaceID: ws, TimelineID: id, Version: req.Version,
		Force: req.Force, DryRun: req.DryRun,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	body := toResp(res.View)
	// ⚠️ 缺口要**原样回给前端**，不能只回一个数字：用户要知道是哪几镜、
	// 为什么没有 —— 否则他只能挨个去翻（NEW-09）。
	body["plan"] = map[string]any{
		"appended": len(res.Plan.Append),
		"skipped":  res.Plan.Skipped,
		"gaps":     res.Plan.Gaps,
		"complete": res.Plan.Complete(),
		"summary":  res.Plan.Summary(),
	}
	httpx.JSON(w, http.StatusOK, body)
}

func (h *Handler) saveSubtitles(w http.ResponseWriter, r *http.Request) {
	ws, id, ok := scope(w, r, "timelineID")
	if !ok {
		return
	}
	var req struct {
		Version int `json:"version"`
		Items   []struct {
			StartMs int             `json:"startMs"`
			EndMs   int             `json:"endMs"`
			Text    string          `json:"text"`
			Style   json.RawMessage `json:"style"`
		} `json:"items"`
	}
	if !decode(w, r, &req) {
		return
	}
	subs := make([]repo.Subtitle, 0, len(req.Items))
	for _, it := range req.Items {
		subs = append(subs, repo.Subtitle{
			StartMs: it.StartMs, EndMs: it.EndMs, Text: it.Text, Style: it.Style,
		})
	}
	v, err := h.svc.SaveSubtitles(r.Context(), ws, id, req.Version, h.editor(r), subs)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toResp(v))
}

func (h *Handler) updateTrack(w http.ResponseWriter, r *http.Request) {
	ws, id, ok := scope(w, r, "timelineID")
	if !ok {
		return
	}
	trackID, ok := pathID(w, r, "trackID")
	if !ok {
		return
	}
	var req struct {
		Version int  `json:"version"`
		IsMuted bool `json:"isMuted"`
		Volume  int  `json:"volume"`
	}
	if !decode(w, r, &req) {
		return
	}
	v, err := h.svc.UpdateTrack(r.Context(), ws, id, trackID, req.Version, h.editor(r),
		req.IsMuted, req.Volume)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toResp(v))
}

func (h *Handler) addClip(w http.ResponseWriter, r *http.Request) {
	ws, id, ok := scope(w, r, "timelineID")
	if !ok {
		return
	}
	trackID, ok := pathID(w, r, "trackID")
	if !ok {
		return
	}
	var req struct {
		Version     int    `json:"version"`
		MediaID     string `json:"mediaId"`
		SourceInMs  int    `json:"sourceInMs"`
		SourceOutMs int    `json:"sourceOutMs"`
		AfterClipID string `json:"afterClipId"`
	}
	if !decode(w, r, &req) {
		return
	}
	mediaID, _ := strconv.ParseInt(req.MediaID, 10, 64)
	after, _ := strconv.ParseInt(req.AfterClipID, 10, 64)
	v, err := h.svc.AddClip(r.Context(), timelineapp.AddClipInput{
		Editor:      h.editor(r),
		WorkspaceID: ws, TimelineID: id, TrackID: trackID, Version: req.Version,
		MediaID: mediaID, SourceInMs: req.SourceInMs, SourceOutMs: req.SourceOutMs,
		AfterClipID: after,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, toResp(v))
}

func (h *Handler) trim(w http.ResponseWriter, r *http.Request) {
	ws, id, trackID, clipID, ok := clipScope(w, r)
	if !ok {
		return
	}
	var req struct {
		Version     int `json:"version"`
		SourceInMs  int `json:"sourceInMs"`
		SourceOutMs int `json:"sourceOutMs"`
	}
	if !decode(w, r, &req) {
		return
	}
	v, err := h.svc.TrimClip(r.Context(), timelineapp.TrimInput{
		Editor:      h.editor(r),
		WorkspaceID: ws, TimelineID: id, TrackID: trackID, ClipID: clipID,
		Version: req.Version, SourceInMs: req.SourceInMs, SourceOutMs: req.SourceOutMs,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toResp(v))
}

func (h *Handler) split(w http.ResponseWriter, r *http.Request) {
	ws, id, trackID, clipID, ok := clipScope(w, r)
	if !ok {
		return
	}
	var req struct {
		Version int `json:"version"`
		AtMs    int `json:"atMs"`
	}
	if !decode(w, r, &req) {
		return
	}
	v, err := h.svc.SplitClip(r.Context(), timelineapp.SplitInput{
		Editor:      h.editor(r),
		WorkspaceID: ws, TimelineID: id, TrackID: trackID, ClipID: clipID,
		Version: req.Version, AtMs: req.AtMs,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toResp(v))
}

func (h *Handler) move(w http.ResponseWriter, r *http.Request) {
	ws, id, trackID, clipID, ok := clipScope(w, r)
	if !ok {
		return
	}
	// 画面轨传 toPosition（排序），音频轨传 startMs（成片时间，SCD-15）；服务端按轨道种类分流。
	var req struct {
		Version    int  `json:"version"`
		ToPosition int  `json:"toPosition"`
		StartMs    *int `json:"startMs"`
	}
	if !decode(w, r, &req) {
		return
	}
	v, err := h.svc.MoveClip(r.Context(), timelineapp.MoveInput{
		Editor:      h.editor(r),
		WorkspaceID: ws, TimelineID: id, TrackID: trackID, ClipID: clipID,
		Version: req.Version, ToPosition: req.ToPosition, StartMs: req.StartMs,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toResp(v))
}

// updateClip 改片段属性。
//
// ⚠️ 每个字段都用**指针**接：`volume: 0` 是静音，是一个正当的取值。
// 用零值当"没传"的话，用户永远调不到静音 —— 他点了没反应，
// 而界面上那个滑块确实滑到了 0（03 §5「不要让 Go 零值悄悄覆盖语义」）。
func (h *Handler) updateClip(w http.ResponseWriter, r *http.Request) {
	ws, id, trackID, clipID, ok := clipScope(w, r)
	if !ok {
		return
	}
	var req struct {
		Version      int  `json:"version"`
		Speed        *int `json:"speed"`
		Volume       *int `json:"volume"`
		FadeInMs     *int `json:"fadeInMs"`
		FadeOutMs    *int `json:"fadeOutMs"`
		TransitionIn *struct {
			Kind       string `json:"kind"`
			DurationMs int    `json:"durationMs"`
		} `json:"transitionIn"`
	}
	if !decode(w, r, &req) {
		return
	}
	props := domain.ClipProps{
		Speed: req.Speed, Volume: req.Volume,
		FadeInMs: req.FadeInMs, FadeOutMs: req.FadeOutMs,
	}
	if req.TransitionIn != nil {
		props.TransitionIn = &domain.Transition{
			Kind: req.TransitionIn.Kind, DurationMs: req.TransitionIn.DurationMs,
		}
	}
	v, err := h.svc.UpdateClip(r.Context(), timelineapp.ClipPropsInput{
		Editor:      h.editor(r),
		WorkspaceID: ws, TimelineID: id, TrackID: trackID, ClipID: clipID,
		Version: req.Version, Props: props,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toResp(v))
}

func (h *Handler) removeClip(w http.ResponseWriter, r *http.Request) {
	ws, id, trackID, clipID, ok := clipScope(w, r)
	if !ok {
		return
	}
	// 删除也要带版本：不带的话两个人同时删不同的段，后一个会把前一个的
	// 删除撤销掉（他手上那份清单还有那一段）。
	version, _ := strconv.Atoi(r.URL.Query().Get("version"))
	v, err := h.svc.RemoveClip(r.Context(), ws, id, trackID, clipID, version, h.editor(r))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toResp(v))
}

// ---------------------------------------------------------------- 响应

func metaResp(t repo.Timeline) map[string]any {
	return map[string]any{
		"id":        strconv.FormatInt(t.ID, 10),
		"projectId": strconv.FormatInt(t.ProjectID, 10),
		"scriptId":  optionalID(t.ScriptID),
		"name":      t.Name,
		// ⚠️ version 是**服务端乐观锁**，前端每次写回都要原样带上。
		// 与 schemaVersion（JSON 结构版本）不是一回事，不能混用。
		"version":     t.Version,
		"aspectRatio": t.AspectRatio,
		"fps":         t.FPS,
		"width":       t.Width,
		"height":      t.Height,
		"status":      t.Status,
		// 智能剪辑（SCD-17、SCD-20）：旧时间线与脚本时间线没有绑定节点、编辑者为空。
		"canvasNodeId":   optionalString(t.CanvasNodeID),
		"isArranged":     t.IsArranged,
		"lastEditorKind": optionalString(t.LastEditorKind),
	}
}

// ViewJSON 是时间线的对外形状（契约 Timeline，与 `GET /timelines/{id}` 同一份渲染）。
// AI 剪辑的结果里带着写入后的时间线（SmartcutPlanResult.timeline），由组装根经它渲染 —— 不另写一份，
// 两份早晚会在某个字段上不一致，而前端用同一个函数解析它们。
func ViewJSON(v timelineapp.View) ([]byte, error) { return json.Marshal(toResp(v)) }

func toResp(v timelineapp.View) map[string]any {
	tracks := make([]map[string]any, 0, len(v.Tracks))
	for _, tr := range v.Tracks {
		clips := make([]map[string]any, 0, len(tr.Clips))
		for _, c := range tr.Clips {
			clips = append(clips, clipResp(c))
		}
		tracks = append(tracks, map[string]any{
			"id": strconv.FormatInt(tr.Track.ID, 10), "kind": tr.Track.Kind,
			"idx": tr.Track.Idx, "name": tr.Track.Name,
			"isMuted": tr.Track.IsMuted, "volume": tr.Track.Volume,
			"role": optionalString(tr.Track.Role), "lang": optionalString(tr.Track.Lang),
			"isHidden": tr.Track.IsHidden, "style": optionalRaw(tr.Track.Style),
			"clips": clips,
		})
	}
	subs := make([]map[string]any, 0, len(v.Subtitles))
	for _, s := range v.Subtitles {
		sub := map[string]any{
			"id": strconv.FormatInt(s.ID, 10), "startMs": s.StartMs,
			"endMs": s.EndMs, "text": s.Text, "style": s.Style,
			"trackId": strconv.FormatInt(s.TrackID, 10),
		}
		// 锚点原样回：未知字段原样保留（GO-35），读出时坏值已当成没有。
		if len(s.Anchor) > 0 {
			sub["anchor"] = s.Anchor
		}
		// 跟随的片段把它所在的区间裁掉了：隐藏，不删除（SC-F-56）。
		if s.Hidden {
			sub["hidden"] = true
		}
		subs = append(subs, sub)
	}
	texts := make([]map[string]any, 0, len(v.Texts))
	for _, t := range v.Texts {
		texts = append(texts, map[string]any{
			"id": strconv.FormatInt(t.ID, 10), "trackId": strconv.FormatInt(t.TrackID, 10),
			"startMs": t.StartMs, "endMs": t.EndMs, "text": t.Text,
			"style": rawOrEmptyObject(t.Style), "box": rawOrEmptyObject(t.Box),
		})
	}
	out := metaResp(v.Timeline)
	out["tracks"] = tracks
	out["subtitles"] = subs
	out["texts"] = texts
	// 总时长由服务端算：前端自己累加的话，变速那一档会算错。
	out["totalMs"] = v.TotalMs
	// ⚠️ 两个数都要给：用户按 totalMs 理解"放了多少素材"，
	// 但配乐与字幕要对齐的是 effectiveMs（算上转场重叠）——
	// 只给一个的话，设了转场之后字幕就会整体偏移。
	out["effectiveMs"] = v.EffectiveMs
	return out
}

func clipResp(c domain.Clip) map[string]any {
	out := map[string]any{
		"id": strconv.FormatInt(c.ID, 10), "idx": c.Idx,
		"mediaId":     strconv.FormatInt(c.MediaID, 10),
		"sourceInMs":  c.SourceInMs,
		"sourceOutMs": c.SourceOutMs,
		"startMs":     c.StartMs,
		"durationMs":  c.DurationMs,
		"speed":       c.Speed,
		"volume":      c.Volume,
		"fadeInMs":    c.FadeInMs,
		"fadeOutMs":   c.FadeOutMs,
		// url 指向受保护端点，前端一律走 useAuthenticatedMedia。
		"mediaUrl": "/api/v1/media/" + strconv.FormatInt(c.MediaID, 10),
	}
	// 溯源只在自动装配出来的段上有。⚠️ 源镜头可能已被删除 ——
	// 那不是错误，只是溯源到此为止。
	if c.SourceShotID != 0 {
		out["sourceShotId"] = strconv.FormatInt(c.SourceShotID, 10)
	}
	// ⚠️ 接缝的转场只记在**后一段的 transitionIn** 上（见 domain/transition.go）。
	// 前端画接缝时读的是右边那一段，不要去看左边那段的 transitionOut。
	if c.TransitionIn.IsActive() {
		out["transitionIn"] = map[string]any{
			"kind": c.TransitionIn.Kind, "durationMs": c.TransitionIn.DurationMs,
		}
	}
	if c.TransitionOut.IsActive() {
		out["transitionOut"] = map[string]any{
			"kind": c.TransitionOut.Kind, "durationMs": c.TransitionOut.DurationMs,
		}
	}
	// 智能剪辑的片段字段（SC-D-03）。变换原样回（未知字段原样保留）；没有变换 = 等比适配、居中。
	if c.Transform != nil {
		out["transform"] = c.Transform
	}
	origin := c.Origin
	if origin == "" {
		origin = domain.OriginUser
	}
	out["origin"] = origin
	out["sourceNodeId"] = optionalString(c.SourceNodeID)
	alternates := c.Alternates
	if alternates == nil {
		alternates = []domain.Alternate{}
	}
	out["alternates"] = alternates
	if c.PendingGenerationID != 0 {
		out["pendingGenerationId"] = strconv.FormatInt(c.PendingGenerationID, 10)
	}
	// 跟随画面轨的一段（音频轨，SC-F-56）：落点与取用区间已按锚点算好；hidden = 那一段被裁到看不见了。
	if len(c.Anchor) > 0 {
		out["anchor"] = c.Anchor
		out["hidden"] = c.Hidden
	}
	return out
}

func optionalString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// optionalRaw 原样 JSON；没有时回 null。
func optionalRaw(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return raw
}

// rawOrEmptyObject 原样 JSON；没有时回 {}（文字层的样式与位置框在契约里是对象，不可为空）。
func rawOrEmptyObject(raw json.RawMessage) any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	return raw
}

func optionalID(id int64) any {
	if id == 0 {
		return nil
	}
	return strconv.FormatInt(id, 10)
}

// ---------------------------------------------------------------- 工具

func workspace(w http.ResponseWriter, r *http.Request) (int64, bool) {
	ws, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
		return 0, false
	}
	return ws.WorkspaceID, true
}

func pathID(w http.ResponseWriter, r *http.Request, key string) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, key), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, key+" 非法"))
		return 0, false
	}
	return id, true
}

func scope(w http.ResponseWriter, r *http.Request, key string) (int64, int64, bool) {
	ws, ok := workspace(w, r)
	if !ok {
		return 0, 0, false
	}
	id, ok := pathID(w, r, key)
	if !ok {
		return 0, 0, false
	}
	return ws, id, true
}

func clipScope(w http.ResponseWriter, r *http.Request) (ws, id, trackID, clipID int64, ok bool) {
	ws, id, ok = scope(w, r, "timelineID")
	if !ok {
		return
	}
	trackID, ok = pathID(w, r, "trackID")
	if !ok {
		return
	}
	clipID, ok = pathID(w, r, "clipID")
	return
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidJSON, "请求体不是合法 JSON"))
		return false
	}
	return true
}

// ---------------------------------------------------------------- 渲染

func (h *Handler) startRender(w http.ResponseWriter, r *http.Request) {
	ws, id, ok := scope(w, r, "timelineID")
	if !ok {
		return
	}
	var req struct {
		DryRun     bool   `json:"dryRun"`
		Resolution string `json:"resolution"`
		FPS        int    `json:"fps"`
	}
	if !decode(w, r, &req) {
		return
	}
	// ⚠️ 真渲染要 export，预检不要（见 Mount 里那段注释）。
	// 判定必须在**调用服务之前** —— 放到 StartRender 里面的话，
	// 预检、报价、扣费的顺序就得为权限再让一次路，而那条顺序本身
	// 是 CD-51 花了力气定下来的。
	if !req.DryRun && !collabtransport.Allows(r, collabdomain.ActionExport) {
		httpx.Fail(w, r, collabtransport.DenyAction(r, collabdomain.ActionExport))
		return
	}
	job, pre, err := h.svc.StartRender(r.Context(), timelineapp.RenderInput{
		WorkspaceID: ws, PayerAccountID: h.payer(r), UserID: h.userID(r),
		TimelineID: id, MemberLevelID: h.memberLevel(r), DryRun: req.DryRun,
		IdempotencyKey: r.Header.Get("Idempotency-Key"), Resolution: req.Resolution, FPS: req.FPS,
	})
	if err != nil {
		failWithRetry(w, r, err)
		return
	}
	body := precheckResp(pre)
	if req.DryRun {
		httpx.JSON(w, http.StatusOK, body)
		return
	}
	body["render"] = renderResp(job)
	httpx.JSON(w, http.StatusAccepted, body)
}

func (h *Handler) listRenders(w http.ResponseWriter, r *http.Request) {
	ws, id, ok := scope(w, r, "timelineID")
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := h.svc.ListRenders(r.Context(), ws, id, limit)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, j := range list {
		out = append(out, renderResp(j))
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) getRender(w http.ResponseWriter, r *http.Request) {
	ws, id, ok := scope(w, r, "renderID")
	if !ok {
		return
	}
	j, err := h.svc.GetRender(r.Context(), ws, id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, renderResp(j))
}

func (h *Handler) cancelRender(w http.ResponseWriter, r *http.Request) {
	ws, id, ok := scope(w, r, "renderID")
	if !ok {
		return
	}
	j, err := h.svc.CancelRender(r.Context(), ws, id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, renderResp(j))
}

// renderDownload 成片的签名下载链接（5 分钟有效）。⚠️ 链接本身就是凭据：不写日志。
func (h *Handler) renderDownload(w http.ResponseWriter, r *http.Request) {
	ws, id, ok := scope(w, r, "renderID")
	if !ok {
		return
	}
	url, exp, name, err := h.svc.RenderDownload(r.Context(), ws, id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	// 不让浏览器或中间代理缓存：过期之后再点要拿到新链接。
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, map[string]any{"url": url, "expiresAt": exp.UTC().Format(time.RFC3339),
		"fileName": name})
}

// failWithRetry 回错误；导出限流（429）时另设 Retry-After 头（同导演台、MCP 的限流）。
func failWithRetry(w http.ResponseWriter, r *http.Request, err error) {
	var be *httpx.BizError
	if errors.As(err, &be) && (be.Code == errcode.RateLimited || be.Code == errcode.ConcurrencyLimited) {
		if secs, ok := be.Details["retry_after_seconds"].(int); ok && secs > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(secs))
		}
	}
	httpx.Fail(w, r, err)
}

func precheckResp(p timelineapp.RenderPrecheck) map[string]any {
	issues := make([]map[string]any, 0, len(p.Issues))
	for _, x := range p.Issues {
		issues = append(issues, map[string]any{
			"code": x.Code, "detail": x.Detail, "clipIdx": x.ClipIdx,
		})
	}
	return map[string]any{
		// ⚠️ **每一条**原因都回给用户，不是一句"无法渲染" ——
		// 他要知道是哪一段缺素材才改得动。
		"issues":     issues,
		"runnable":   p.Runnable(),
		"durationMs": p.DurationMs,
		"credits":    p.Credits,
		"clipCount":  p.ClipCount,
		"subtitles":  p.SubtitleCnt,
	}
}

func renderResp(j repo.RenderJob) map[string]any {
	out := map[string]any{
		"id":         strconv.FormatInt(j.ID, 10),
		"timelineId": strconv.FormatInt(j.TimelineID, 10),
		// ⚠️ 渲染用的是**受理时冻结的**那个版本，不是时间线当前的版本。
		// 用户在排队期间改了一刀不会影响这次成片（NEW-10）。
		"timelineVersion": j.TimelineVersion,
		"status":          j.Status,
		"progress":        j.Progress,
		"creditsCost":     j.CreditsCost,
		// ⚠️ 这次**到底加没加**水印，与"配置里开没开"是两件事：
		// 用户可能自己关了、平台可能还没配图、图也可能取不回来。
		// 只看配置的话事后三种分不出来，而处理完全不同。
		"watermarkApplied": j.WatermarkApplied,
		"createdAt":        j.CreatedAt.UTC().Format(time.RFC3339),
		"updatedAt":        j.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if j.Stage != "" {
		// 阶段让进度条能说"现在在干什么"——只给百分比的话，
		// 卡在 40% 十分钟时用户不知道是死了还是在编码。
		out["stage"] = j.Stage
	}
	if j.FFmpegVersion != "" {
		out["ffmpegVersion"] = j.FFmpegVersion
	}
	if j.ErrorCode != "" {
		out["errorCode"] = j.ErrorCode
		out["errorMessage"] = j.ErrorMessage
	}
	if j.OutputMediaID != 0 {
		out["outputMediaId"] = strconv.FormatInt(j.OutputMediaID, 10)
		out["url"] = "/api/v1/media/" + strconv.FormatInt(j.OutputMediaID, 10)
	}
	if j.WorkID != 0 {
		// ⚠️ 有 workId **不等于**已经公开：落的是 pending，
		// 公开可见性要运营审核 + 加精 + 未下架三个条件都成立。
		out["workId"] = strconv.FormatInt(j.WorkID, 10)
	}
	if j.DeadlineAt != nil {
		out["deadlineAt"] = j.DeadlineAt.UTC().Format(time.RFC3339)
	}
	if len(j.Spec) > 0 {
		out["spec"] = json.RawMessage(j.Spec)
	}
	if len(j.SelfCheck) > 0 {
		// 导出后自检：只警告，不判失败（SC-F-75）。
		out["selfCheck"] = json.RawMessage(j.SelfCheck)
	}
	return out
}
