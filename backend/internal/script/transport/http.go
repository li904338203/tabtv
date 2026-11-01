// Package transport 是脚本域的 HTTP 层（薄：仅校验 + 组装）。
package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	collabdomain "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	collabtransport "github.com/li904338203/tabtv/backend/internal/collaboration/transport"
	"github.com/li904338203/tabtv/backend/internal/script/app"
	"github.com/li904338203/tabtv/backend/internal/script/domain"
	"github.com/li904338203/tabtv/backend/internal/script/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

type Handler struct {
	svc    *app.Service
	userID func(context.Context) (int64, bool)
}

func NewHandler(svc *app.Service, userID func(context.Context) (int64, bool)) *Handler {
	return &Handler{svc: svc, userID: userID}
}

func (h *Handler) uid(r *http.Request) int64 {
	id, _ := h.userID(r.Context())
	return id
}

// Mount 挂载用户端接口。
func (h *Handler) Mount(r chi.Router, requireUser func(http.Handler) http.Handler,
	requireWorkspace func(http.Handler) http.Handler) {
	r.Group(func(r chi.Router) {
		r.Use(requireUser, requireWorkspace)
		// ⚠️ `gen` 是**会扣费**的那一档（P12-03）。默认闸按 HTTP 方法推出的是
		// edit，对只读成员已经够用；单独标出来是为了让"哪些接口会花用户的钱"
		// 在代码里一眼可见 —— 脚本域的扣费点分散在分镜、资产、提示词、组执行
		// 四处，不标的话要读完整个文件才数得清。
		gen := r.With(collabtransport.RequireAction(collabdomain.ActionGenerate))

		r.Post("/scripts", h.create)
		r.Get("/scripts/{scriptID}", h.get)
		r.Patch("/scripts/{scriptID}", h.patch)
		r.Get("/scripts/{scriptID}/llm-calls", h.calls)
		// ⚠️ 按节点查要注册在 `/{scriptID}` 之前的静态段上，
		// 否则 `by-node` 会被当成一个 scriptID。
		r.Get("/projects/{projectID}/nodes/{nodeID}/script", h.byNode)

		// ⚠️ 分镜走**同步**文本模型调用，费用记在 `script_llm_call` —— 一样是花钱。
		gen.Post("/scripts/{scriptID}/storyboard", h.storyboard)

		r.Post("/scripts/{scriptID}/shots", h.addShot)
		r.Put("/scripts/{scriptID}/shots/{shotID}", h.saveShot)
		r.Delete("/scripts/{scriptID}/shots/{shotID}", h.deleteShot)
		r.Post("/scripts/{scriptID}/shots/{shotID}/duplicate", h.duplicateShot)
		r.Post("/scripts/{scriptID}/shots/{shotID}/move", h.moveShot)
		r.Put("/scripts/{scriptID}/shots/order", h.reorderShots)

		// 资产（P07-02）。
		r.Post("/scripts/{scriptID}/assets/reconcile", h.reconcile)
		r.Get("/scripts/{scriptID}/assets", h.listAssets)
		gen.Post("/scripts/{scriptID}/assets/generate", h.generateAssets)
		r.Post("/scripts/{scriptID}/shots/{shotID}/assets/{assetID}", h.attachAsset)
		r.Put("/script-assets/{assetID}", h.saveAsset)
		r.Put("/script-assets/{assetID}/media", h.attachMedia)
		// ⚠️ 资产库是**工作区级**的，不挂在脚本下：它服务的是
		// "跨脚本手动挑选"（AV-20），而自动识别永远只在本脚本内归并。
		r.Get("/script-assets", h.library)

		// 提示词（P07-03）。
		r.Get("/scripts/{scriptID}/prompts", h.listPrompts)
		// ⚠️ 合成的 `ai` 模式收费、`template` 模式 0 积分，但入口是同一个 ——
		// 按**可能扣费**的那一档设闸（少扣一次不如多挡一次）。
		gen.Post("/scripts/{scriptID}/prompts/compose", h.compose)
		r.Put("/scripts/{scriptID}/shots/{shotID}/prompts/{kind}", h.savePrompt)

		// 组任务（P07-04）。
		r.Post("/scripts/{scriptID}/groups", h.createGroup)
		r.Get("/scripts/{scriptID}/groups", h.listGroups)
		r.Get("/groups/{groupID}", h.getGroup)
		r.Get("/projects/{projectID}/nodes/{nodeID}/group", h.groupByNode)
		gen.Post("/groups/{groupID}/execute", h.executeGroup)
		gen.Post("/groups/{groupID}/items/{itemID}/retry", h.retryItem)
		r.Put("/groups/{groupID}/items/{itemID}", h.saveGroupItem)
		r.Post("/groups/{groupID}/cancel", h.cancelGroup)
		r.Post("/groups/{groupID}/ungroup", h.ungroup)

		// 脚本导出（P07-05）。
		r.Get("/scripts/{scriptID}/export", h.export)
	})
}

func (h *Handler) ctx(w http.ResponseWriter, r *http.Request) (
	collabtransport.WorkspaceContext, bool) {
	ws, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
		return collabtransport.WorkspaceContext{}, false
	}
	return ws, true
}

func pathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, name), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, name+" 非法"))
		return 0, false
	}
	return id, true
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidJSON, "请求体不是合法 JSON"))
		return false
	}
	return true
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ctx(w, r)
	if !ok {
		return
	}
	var req struct {
		ProjectID    string `json:"projectId"`
		CanvasNodeID string `json:"canvasNodeId"`
		SourceType   string `json:"sourceType"`
		RawInput     string `json:"rawInput"`
		GlobalStyle  string `json:"globalStyle"`
	}
	if !decode(w, r, &req) {
		return
	}
	projectID, _ := strconv.ParseInt(req.ProjectID, 10, 64)
	out, err := h.svc.Create(r.Context(), app.CreateInput{
		WorkspaceID: ws.WorkspaceID, ProjectID: projectID,
		CanvasNodeID: req.CanvasNodeID, SourceType: req.SourceType,
		RawInput: req.RawInput, GlobalStyle: req.GlobalStyle,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, toDetail(out))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ctx(w, r)
	if !ok {
		return
	}
	id, ok2 := pathID(w, r, "scriptID")
	if !ok2 {
		return
	}
	out, err := h.svc.Get(r.Context(), ws.WorkspaceID, id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toDetail(out))
}

func (h *Handler) byNode(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ctx(w, r)
	if !ok {
		return
	}
	projectID, ok2 := pathID(w, r, "projectID")
	if !ok2 {
		return
	}
	out, err := h.svc.GetByNode(r.Context(), ws.WorkspaceID, projectID,
		chi.URLParam(r, "nodeID"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toDetail(out))
}

func (h *Handler) patch(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ctx(w, r)
	if !ok {
		return
	}
	id, ok2 := pathID(w, r, "scriptID")
	if !ok2 {
		return
	}
	var req struct {
		Version     int     `json:"version"`
		RawInput    *string `json:"rawInput"`
		GlobalStyle *string `json:"globalStyle"`
		CurrentStep *int    `json:"currentStep"`
	}
	if !decode(w, r, &req) {
		return
	}
	out, err := h.svc.Patch(r.Context(), app.PatchInput{
		WorkspaceID: ws.WorkspaceID, ScriptID: id, ExpectedVersion: req.Version,
		RawInput: req.RawInput, GlobalStyle: req.GlobalStyle, CurrentStep: req.CurrentStep,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toDetail(out))
}

func (h *Handler) storyboard(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ctx(w, r)
	if !ok {
		return
	}
	id, ok2 := pathID(w, r, "scriptID")
	if !ok2 {
		return
	}
	var req struct {
		ModelCode    string `json:"modelCode"`
		ShotCount    int    `json:"shotCount"`
		ExtraContext string `json:"extraContext"`
	}
	if !decode(w, r, &req) {
		return
	}
	out, err := h.svc.GenerateStoryboard(r.Context(), app.StoryboardInput{
		WorkspaceID: ws.WorkspaceID, PayerAccountID: ws.PayerAccountID,
		UserID: h.uid(r), ScriptID: id, ModelCode: req.ModelCode,
		ShotCount: req.ShotCount, ExtraContext: req.ExtraContext,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	resp := toDetail(out.Detail)
	resp["llmCallId"] = strconv.FormatInt(out.CallID, 10)
	resp["creditsCost"] = out.Credits
	httpx.JSON(w, http.StatusOK, resp)
}

func (h *Handler) addShot(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ctx(w, r)
	if !ok {
		return
	}
	id, ok2 := pathID(w, r, "scriptID")
	if !ok2 {
		return
	}
	var req struct {
		AfterShotID string `json:"afterShotId"`
		DurationSec int    `json:"durationSec"`
		Description string `json:"description"`
	}
	if !decode(w, r, &req) {
		return
	}
	after, _ := strconv.ParseInt(req.AfterShotID, 10, 64)
	out, err := h.svc.AddShot(r.Context(), app.AddShotInput{
		WorkspaceID: ws.WorkspaceID, ScriptID: id, AfterShotID: after,
		DurationSec: req.DurationSec, Description: req.Description,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toDetail(out))
}

func (h *Handler) saveShot(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ctx(w, r)
	if !ok {
		return
	}
	scriptID, ok2 := pathID(w, r, "scriptID")
	shotID, ok3 := pathID(w, r, "shotID")
	if !ok2 || !ok3 {
		return
	}
	var req struct {
		Version     int             `json:"version"`
		DurationSec *int            `json:"durationSec"`
		Description *string         `json:"description"`
		ShotSize    *string         `json:"shotSize"`
		Lighting    *string         `json:"lighting"`
		Dialogue    *string         `json:"dialogue"`
		SoundEffect *string         `json:"soundEffect"`
		CameraMove  *string         `json:"cameraMove"`
		StartState  *string         `json:"startState"`
		EndState    *string         `json:"endState"`
		Entities    json.RawMessage `json:"entities"`
	}
	if !decode(w, r, &req) {
		return
	}
	shot, err := h.svc.SaveShot(r.Context(), app.SaveShotInput{
		WorkspaceID: ws.WorkspaceID, ScriptID: scriptID, ShotID: shotID,
		ExpectedVersion: req.Version, DurationSec: req.DurationSec,
		Description: req.Description, ShotSize: req.ShotSize, Lighting: req.Lighting,
		Dialogue: req.Dialogue, SoundEffect: req.SoundEffect, CameraMove: req.CameraMove,
		StartState: req.StartState, EndState: req.EndState, Entities: req.Entities,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toShot(shot))
}

func (h *Handler) deleteShot(w http.ResponseWriter, r *http.Request) {
	h.shotAction(w, r, func(ws collabtransport.WorkspaceContext, scriptID, shotID int64) (
		app.Detail, error) {
		return h.svc.DeleteShot(r.Context(), ws.WorkspaceID, scriptID, shotID)
	})
}

func (h *Handler) duplicateShot(w http.ResponseWriter, r *http.Request) {
	h.shotAction(w, r, func(ws collabtransport.WorkspaceContext, scriptID, shotID int64) (
		app.Detail, error) {
		return h.svc.DuplicateShot(r.Context(), ws.WorkspaceID, scriptID, shotID)
	})
}

func (h *Handler) moveShot(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("direction")
	h.shotAction(w, r, func(ws collabtransport.WorkspaceContext, scriptID, shotID int64) (
		app.Detail, error) {
		return h.svc.MoveShot(r.Context(), ws.WorkspaceID, scriptID, shotID, dir)
	})
}

func (h *Handler) shotAction(w http.ResponseWriter, r *http.Request,
	fn func(collabtransport.WorkspaceContext, int64, int64) (app.Detail, error)) {
	ws, ok := h.ctx(w, r)
	if !ok {
		return
	}
	scriptID, ok2 := pathID(w, r, "scriptID")
	shotID, ok3 := pathID(w, r, "shotID")
	if !ok2 || !ok3 {
		return
	}
	out, err := fn(ws, scriptID, shotID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toDetail(out))
}

func (h *Handler) reorderShots(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ctx(w, r)
	if !ok {
		return
	}
	scriptID, ok2 := pathID(w, r, "scriptID")
	if !ok2 {
		return
	}
	var req struct {
		ShotIDs []string `json:"shotIds"`
	}
	if !decode(w, r, &req) {
		return
	}
	ids := make([]int64, 0, len(req.ShotIDs))
	for _, s := range req.ShotIDs {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "shotIds 含非法 ID"))
			return
		}
		ids = append(ids, id)
	}
	out, err := h.svc.ReorderShots(r.Context(), ws.WorkspaceID, scriptID, ids)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toDetail(out))
}

func (h *Handler) calls(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ctx(w, r)
	if !ok {
		return
	}
	id, ok2 := pathID(w, r, "scriptID")
	if !ok2 {
		return
	}
	list, err := h.svc.Calls(r.Context(), ws.WorkspaceID, id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	type item struct {
		ID           string `json:"id"`
		Purpose      string `json:"purpose"`
		Status       string `json:"status"`
		CreditsCost  int    `json:"creditsCost"`
		ErrorMessage string `json:"errorMessage,omitempty"`
	}
	out := make([]item, 0, len(list))
	for _, c := range list {
		out = append(out, item{
			ID: strconv.FormatInt(c.ID, 10), Purpose: c.Purpose, Status: c.Status,
			CreditsCost: c.CreditsCost, ErrorMessage: c.ErrorMessage,
		})
	}
	httpx.JSON(w, http.StatusOK, out)
}

// toDetail / toShot：entity ≠ dto，ID 一律转字符串（防 JS 精度丢失）。
func toDetail(d app.Detail) map[string]any {
	shots := make([]map[string]any, 0, len(d.Shots))
	for _, s := range d.Shots {
		m := toShot(s)
		// 详情里**每一镜都给**（没有引用就是空数组）：前端据此区分「没有引用」与「这份响应不带引用」。
		ids := make([]string, 0, len(d.AssetIDs[s.ID]))
		for _, id := range d.AssetIDs[s.ID] {
			ids = append(ids, strconv.FormatInt(id, 10))
		}
		m["assetIds"] = ids
		shots = append(shots, m)
	}
	return map[string]any{
		"id":            strconv.FormatInt(d.Script.ID, 10),
		"projectId":     strconv.FormatInt(d.Script.ProjectID, 10),
		"canvasNodeId":  d.Script.CanvasNodeID,
		"sourceType":    d.Script.SourceType,
		"currentStep":   d.Script.CurrentStep,
		"status":        d.Script.Status,
		"rawInput":      d.Script.RawInput,
		"globalStyle":   d.Script.GlobalStyle,
		"assetProfiles": d.Script.AssetProfiles,
		"version":       d.Script.Version,
		"shots":         shots,
	}
}

func toShot(s repo.Shot) map[string]any {
	return map[string]any{
		"id":                strconv.FormatInt(s.ID, 10),
		"shotNo":            s.ShotNo,
		"durationSec":       s.DurationSec,
		"description":       s.Description,
		"shotSize":          s.ShotSize,
		"lighting":          s.Lighting,
		"dialogue":          s.Dialogue,
		"soundEffect":       s.SoundEffect,
		"cameraMove":        s.CameraMove,
		"startState":        s.StartState,
		"endState":          s.EndState,
		"entities":          s.Entities,
		"imagePrompt":       s.ImagePrompt,
		"imagePromptStatus": s.ImagePromptStatus,
		"imagePromptSource": s.ImagePromptSource,
		"videoPrompt":       s.VideoPrompt,
		"videoPromptStatus": s.VideoPromptStatus,
		"videoPromptSource": s.VideoPromptSource,
		"version":           s.Version,
	}
}

// ---------------------------------------------------------------- 资产

func (h *Handler) reconcile(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ctx(w, r)
	if !ok {
		return
	}
	id, ok2 := pathID(w, r, "scriptID")
	if !ok2 {
		return
	}
	out, err := h.svc.ReconcileAssets(r.Context(), ws.WorkspaceID, id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"assets": toAssets(out.Assets),
		// ⚠️ 缺档案的名单要回给前端：这些资产退回了模板描述，
		// 用户该知道哪几个需要自己补一句外观。
		"unmatched": out.Unmatched,
		// ⚠️ 不再出现的资产**只报不删**：用户可能刚删掉某一镜、正准备加回来。
		"orphan": toAssets(out.Orphan),
	})
}

func (h *Handler) listAssets(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ctx(w, r)
	if !ok {
		return
	}
	id, ok2 := pathID(w, r, "scriptID")
	if !ok2 {
		return
	}
	list, lints, err := h.svc.Assets(r.Context(), ws.WorkspaceID, id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := toAssets(list)
	for i, a := range list {
		if l, ok := lints[a.ID]; ok {
			items[i]["lints"] = toLints(l)
		}
	}
	httpx.JSON(w, http.StatusOK, items)
}

func (h *Handler) saveAsset(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ctx(w, r)
	if !ok {
		return
	}
	id, ok2 := pathID(w, r, "assetID")
	if !ok2 {
		return
	}
	var req struct {
		Type        *string         `json:"type"`
		Name        *string         `json:"name"`
		Description *string         `json:"description"`
		Profile     json.RawMessage `json:"profile"`
	}
	if !decode(w, r, &req) {
		return
	}
	asset, lints, err := h.svc.SaveAsset(r.Context(), app.SaveAssetInput{
		WorkspaceID: ws.WorkspaceID, AssetID: id, Type: req.Type, Name: req.Name,
		Description: req.Description, Profile: req.Profile,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	item := toAsset(asset)
	// ⚠️ 质量提示**随响应回去而不是拦住请求**：名字没语义、描述里写了
	// 别人的名字都是质量问题不是错误，拦下来用户就走不下去了。
	item["lints"] = toLints(lints)
	httpx.JSON(w, http.StatusOK, item)
}

func (h *Handler) attachMedia(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ctx(w, r)
	if !ok {
		return
	}
	id, ok2 := pathID(w, r, "assetID")
	if !ok2 {
		return
	}
	var req struct {
		MediaID     string `json:"mediaId"`
		ContentType string `json:"contentType"`
		SizeBytes   int64  `json:"sizeBytes"`
	}
	if !decode(w, r, &req) {
		return
	}
	mediaID, err := strconv.ParseInt(req.MediaID, 10, 64)
	if err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "mediaId 非法"))
		return
	}
	asset, err := h.svc.AttachMedia(r.Context(), ws.WorkspaceID, id, mediaID,
		req.ContentType, req.SizeBytes)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toAsset(asset))
}

func (h *Handler) attachAsset(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ctx(w, r)
	if !ok {
		return
	}
	scriptID, ok2 := pathID(w, r, "scriptID")
	shotID, ok3 := pathID(w, r, "shotID")
	assetID, ok4 := pathID(w, r, "assetID")
	if !ok2 || !ok3 || !ok4 {
		return
	}
	out, err := h.svc.AttachExisting(r.Context(), ws.WorkspaceID, scriptID, shotID, assetID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toDetail(out))
}

func (h *Handler) library(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ctx(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := h.svc.Library(r.Context(), ws.WorkspaceID,
		r.URL.Query().Get("type"), r.URL.Query().Get("keyword"), limit)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toAssets(list))
}

func (h *Handler) generateAssets(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ctx(w, r)
	if !ok {
		return
	}
	id, ok2 := pathID(w, r, "scriptID")
	if !ok2 {
		return
	}
	var req struct {
		ModelCode string   `json:"modelCode"`
		AssetIDs  []string `json:"assetIds"`
		// Params 画幅 / 画质；服务端只认 app.AssetParamKeys 里的键。
		Params map[string]any `json:"params"`
	}
	if !decode(w, r, &req) {
		return
	}
	ids := make([]int64, 0, len(req.AssetIDs))
	for _, s := range req.AssetIDs {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "assetIds 含非法 ID"))
			return
		}
		ids = append(ids, v)
	}
	items, err := h.svc.GenerateAssets(r.Context(), app.BatchAssetInput{
		WorkspaceID: ws.WorkspaceID, PayerAccountID: ws.PayerAccountID,
		UserID: h.uid(r), ScriptID: id, ModelCode: req.ModelCode, AssetIDs: ids,
		Params: req.Params,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		m := map[string]any{
			"assetId": strconv.FormatInt(it.AssetID, 10), "name": it.Name,
			"skipped": it.Skipped,
		}
		if it.GenerationID != 0 {
			m["generationId"] = strconv.FormatInt(it.GenerationID, 10)
		}
		if it.Reason != "" {
			m["reason"] = it.Reason
		}
		out = append(out, m)
	}
	// ⚠️ 逐项结果，**允许部分成功**（AV-13）：余额不足会停掉后续项，
	// 前面已经提交的照常跑。整批当成一个成功/失败会把这件事抹掉。
	httpx.JSON(w, http.StatusOK, out)
}

func toAssets(list []repo.Asset) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, a := range list {
		out = append(out, toAsset(a))
	}
	return out
}

func toAsset(a repo.Asset) map[string]any {
	m := map[string]any{
		"id":          strconv.FormatInt(a.ID, 10),
		"type":        a.Type,
		"name":        a.Name,
		"description": a.Description,
		"profile":     a.Profile,
		"source":      a.Source,
		"status":      a.Status,
	}
	if a.MediaObjectID != 0 {
		m["mediaId"] = strconv.FormatInt(a.MediaObjectID, 10)
		// 字节走媒体域的受保护端点（CD-33）。
		m["url"] = "/api/v1/media/" + strconv.FormatInt(a.MediaObjectID, 10)
	}
	return m
}

func toLints(list []domain.Lint) []map[string]string {
	out := make([]map[string]string, 0, len(list))
	for _, l := range list {
		out = append(out, map[string]string{"code": l.Code, "message": l.Message})
	}
	return out
}

// ---------------------------------------------------------------- 提示词

func (h *Handler) listPrompts(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ctx(w, r)
	if !ok {
		return
	}
	id, ok2 := pathID(w, r, "scriptID")
	if !ok2 {
		return
	}
	list, err := h.svc.Prompts(r.Context(), ws.WorkspaceID, id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, v := range list {
		out = append(out, map[string]any{
			"shotId": strconv.FormatInt(v.ShotID, 10), "shotNo": v.ShotNo,
			"kind": string(v.Kind), "text": v.Text,
			// ⚠️ status 是**算出来的**有效状态，不是库里存的那个 ——
			// 库里可能还停在 ready，而上游早就改过了。
			"status": string(v.Status), "source": string(v.Source),
		})
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) compose(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ctx(w, r)
	if !ok {
		return
	}
	id, ok2 := pathID(w, r, "scriptID")
	if !ok2 {
		return
	}
	var req struct {
		Source    string   `json:"source"`
		ModelCode string   `json:"modelCode"`
		ShotIDs   []string `json:"shotIds"`
		Kinds     []string `json:"kinds"`
		Force     bool     `json:"force"`
		// DryRun 只报价不执行 —— 前端据此渲染"本次将花费 N 积分"（GAP-21）。
		DryRun bool `json:"dryRun"`
	}
	if !decode(w, r, &req) {
		return
	}
	shotIDs := make([]int64, 0, len(req.ShotIDs))
	for _, s := range req.ShotIDs {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "shotIds 含非法 ID"))
			return
		}
		shotIDs = append(shotIDs, v)
	}
	kinds := make([]domain.PromptKind, 0, len(req.Kinds))
	for _, k := range req.Kinds {
		kinds = append(kinds, domain.PromptKind(k))
	}
	out, err := h.svc.Compose(r.Context(), app.ComposeInput{
		WorkspaceID: ws.WorkspaceID, PayerAccountID: ws.PayerAccountID,
		UserID: h.uid(r), ScriptID: id, ShotIDs: shotIDs, Kinds: kinds,
		Source: domain.PromptSource(req.Source), ModelCode: req.ModelCode,
		Force: req.Force, DryRun: req.DryRun,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(out.Items))
	for _, it := range out.Items {
		m := map[string]any{
			"shotId": strconv.FormatInt(it.ShotID, 10), "shotNo": it.ShotNo,
			"kind": string(it.Kind), "status": string(it.Status), "skipped": it.Skipped,
		}
		if it.Reason != "" {
			m["reason"] = it.Reason
		}
		items = append(items, m)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"quote": map[string]any{
			"aiCount": out.Quote.AICount, "templateCount": out.Quote.TemplateCount,
			"skipCount": out.Quote.SkipCount, "unitCost": out.Quote.UnitCost,
			"total": out.Quote.Total,
		},
		"items": items,
	})
}

func (h *Handler) savePrompt(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ctx(w, r)
	if !ok {
		return
	}
	scriptID, ok2 := pathID(w, r, "scriptID")
	shotID, ok3 := pathID(w, r, "shotID")
	if !ok2 || !ok3 {
		return
	}
	var req struct {
		Text    string `json:"text"`
		Version int    `json:"version"`
	}
	if !decode(w, r, &req) {
		return
	}
	shot, err := h.svc.SavePrompt(r.Context(), app.SavePromptInput{
		WorkspaceID: ws.WorkspaceID, ScriptID: scriptID, ShotID: shotID,
		Kind: domain.PromptKind(chi.URLParam(r, "kind")), Text: req.Text,
		ExpectedVersion: req.Version,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toShot(shot))
}

// ---------------------------------------------------------------- 组任务

func toGroup(v app.GroupView) map[string]any {
	items := make([]map[string]any, 0, len(v.Items))
	for _, it := range v.Items {
		m := map[string]any{
			"id": strconv.FormatInt(it.ID, 10), "itemNo": it.ItemNo,
			"status": it.Status, "progress": it.Progress, "creditsCost": it.CreditsCost,
			"params": rawOrEmpty(it.ParamsSnapshot, "{}"),
			"inputs": rawOrEmpty(it.InputRefs, "[]"),
		}
		if it.ShotID != 0 {
			m["shotId"] = strconv.FormatInt(it.ShotID, 10)
		}
		if it.ScriptAssetID != 0 {
			m["assetId"] = strconv.FormatInt(it.ScriptAssetID, 10)
		}
		if it.GenerationID != 0 {
			m["generationId"] = strconv.FormatInt(it.GenerationID, 10)
		}
		if it.ErrorMessage != "" {
			m["errorMessage"] = it.ErrorMessage
		}
		items = append(items, m)
	}
	return map[string]any{
		"id":           strconv.FormatInt(v.Group.ID, 10),
		"projectId":    strconv.FormatInt(v.Group.ProjectID, 10),
		"canvasNodeId": v.Group.CanvasNodeID, "groupType": v.Group.GroupType,
		"scriptId": strconv.FormatInt(v.Group.ScriptID, 10),
		"name":     v.Group.Name, "status": v.Group.Status, "version": v.Group.Version,
		"items": items,
	}
}

func rawOrEmpty(raw json.RawMessage, fallback string) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(fallback)
	}
	return raw
}

func (h *Handler) createGroup(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ctx(w, r)
	if !ok {
		return
	}
	scriptID, ok2 := pathID(w, r, "scriptID")
	if !ok2 {
		return
	}
	var req struct {
		GroupType    string   `json:"groupType"`
		CanvasNodeID string   `json:"canvasNodeId"`
		Name         string   `json:"name"`
		ShotIDs      []string `json:"shotIds"`
		ModelCode    string   `json:"modelCode"`
		Operation    string   `json:"operation"`
		DurationSec  int      `json:"durationSec"`
		// Params 整组参数（服务端只认 app.GroupParamKeys 里的键）；Durations 视频组逐镜时长（镜头 ID → 秒）。
		Params    map[string]any `json:"params"`
		Durations map[string]int `json:"durations"`
	}
	if !decode(w, r, &req) {
		return
	}
	shotIDs, ok3 := parseIDs(w, r, req.ShotIDs, "shotIds")
	if !ok3 {
		return
	}
	durations := make(map[int64]int, len(req.Durations))
	for k, v := range req.Durations {
		id, err := strconv.ParseInt(k, 10, 64)
		if err != nil || v < 0 {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "durations 含非法镜头 ID 或时长"))
			return
		}
		durations[id] = v
	}
	out, err := h.svc.CreateGroup(r.Context(), app.CreateGroupInput{
		WorkspaceID: ws.WorkspaceID, PayerAccountID: ws.PayerAccountID,
		UserID: h.uid(r), ScriptID: scriptID, CanvasNodeID: req.CanvasNodeID,
		GroupType: domain.GroupType(req.GroupType), Name: req.Name, ShotIDs: shotIDs,
		ModelCode: req.ModelCode, Operation: req.Operation, DurationSec: req.DurationSec,
		Params: req.Params, Durations: durations,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toGroup(out))
}

func (h *Handler) listGroups(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ctx(w, r)
	if !ok {
		return
	}
	scriptID, ok2 := pathID(w, r, "scriptID")
	if !ok2 {
		return
	}
	list, err := h.svc.Groups(r.Context(), ws.WorkspaceID, scriptID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, g := range list {
		out = append(out, map[string]any{
			"id": strconv.FormatInt(g.ID, 10), "canvasNodeId": g.CanvasNodeID,
			"groupType": g.GroupType, "name": g.Name,
			"status": g.Status, "version": g.Version,
		})
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) getGroup(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ctx(w, r)
	if !ok {
		return
	}
	id, ok2 := pathID(w, r, "groupID")
	if !ok2 {
		return
	}
	out, err := h.svc.Group(r.Context(), ws.WorkspaceID, id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toGroup(out))
}

func (h *Handler) groupByNode(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ctx(w, r)
	if !ok {
		return
	}
	projectID, ok2 := pathID(w, r, "projectID")
	if !ok2 {
		return
	}
	out, err := h.svc.GroupByNode(r.Context(), ws.WorkspaceID, projectID,
		chi.URLParam(r, "nodeID"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toGroup(out))
}

func (h *Handler) executeGroup(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ctx(w, r)
	if !ok {
		return
	}
	id, ok2 := pathID(w, r, "groupID")
	if !ok2 {
		return
	}
	var req struct {
		ModelCode   string   `json:"modelCode"`
		Operation   string   `json:"operation"`
		DurationSec int      `json:"durationSec"`
		Version     int      `json:"version"`
		Force       bool     `json:"force"`
		ItemIDs     []string `json:"itemIds"`
	}
	if !decode(w, r, &req) {
		return
	}
	itemIDs, ok3 := parseIDs(w, r, req.ItemIDs, "itemIds")
	if !ok3 {
		return
	}
	out, err := h.svc.ExecuteGroup(r.Context(), app.ExecuteGroupInput{
		WorkspaceID: ws.WorkspaceID, PayerAccountID: ws.PayerAccountID,
		UserID: h.uid(r), GroupID: id, ModelCode: req.ModelCode,
		Operation: req.Operation, DurationSec: req.DurationSec,
		ExpectedVersion: req.Version, Force: req.Force, ItemIDs: itemIDs,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toGroup(out))
}

func (h *Handler) retryItem(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ctx(w, r)
	if !ok {
		return
	}
	groupID, ok2 := pathID(w, r, "groupID")
	itemID, ok3 := pathID(w, r, "itemID")
	if !ok2 || !ok3 {
		return
	}
	var req struct {
		ModelCode   string `json:"modelCode"`
		DurationSec int    `json:"durationSec"`
		Version     int    `json:"version"`
	}
	if !decode(w, r, &req) {
		return
	}
	out, err := h.svc.RetryItem(r.Context(), app.ExecuteGroupInput{
		WorkspaceID: ws.WorkspaceID, PayerAccountID: ws.PayerAccountID,
		UserID: h.uid(r), GroupID: groupID, ModelCode: req.ModelCode,
		DurationSec: req.DurationSec, ExpectedVersion: req.Version,
	}, itemID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toGroup(out))
}

func (h *Handler) saveGroupItem(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ctx(w, r)
	if !ok {
		return
	}
	groupID, ok2 := pathID(w, r, "groupID")
	itemID, ok3 := pathID(w, r, "itemID")
	if !ok2 || !ok3 {
		return
	}
	var req struct {
		Params json.RawMessage `json:"params"`
		Inputs json.RawMessage `json:"inputs"`
	}
	if !decode(w, r, &req) {
		return
	}
	it, err := h.svc.SaveGroupItem(r.Context(), app.SaveGroupItemInput{
		WorkspaceID: ws.WorkspaceID, GroupID: groupID, ItemID: itemID,
		Params: req.Params, InputRefs: req.Inputs,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"id": strconv.FormatInt(it.ID, 10), "itemNo": it.ItemNo, "status": it.Status,
		"params": rawOrEmpty(it.ParamsSnapshot, "{}"),
		"inputs": rawOrEmpty(it.InputRefs, "[]"),
	})
}

func (h *Handler) cancelGroup(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ctx(w, r)
	if !ok {
		return
	}
	id, ok2 := pathID(w, r, "groupID")
	if !ok2 {
		return
	}
	var req struct {
		ItemIDs []string `json:"itemIds"`
	}
	if !decode(w, r, &req) {
		return
	}
	itemIDs, ok3 := parseIDs(w, r, req.ItemIDs, "itemIds")
	if !ok3 {
		return
	}
	out, err := h.svc.CancelGroup(r.Context(), ws.WorkspaceID, id, itemIDs)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toGroup(out))
}

func (h *Handler) ungroup(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ctx(w, r)
	if !ok {
		return
	}
	id, ok2 := pathID(w, r, "groupID")
	if !ok2 {
		return
	}
	var req struct {
		Version int `json:"version"`
	}
	if !decode(w, r, &req) {
		return
	}
	out, err := h.svc.Ungroup(r.Context(), ws.WorkspaceID, id, req.Version)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	nodes := make([]map[string]any, 0, len(out.Nodes))
	for _, n := range out.Nodes {
		m := map[string]any{
			"itemId": strconv.FormatInt(n.ItemID, 10), "itemNo": n.ItemNo,
			"nodeId": n.NodeID, "status": n.Status,
			"params": rawOrEmpty(n.Params, "{}"),
			"inputs": rawOrEmpty(n.InputRefs, "[]"),
		}
		if n.ShotID != 0 {
			m["shotId"] = strconv.FormatInt(n.ShotID, 10)
		}
		if n.GenerationID != 0 {
			m["generationId"] = strconv.FormatInt(n.GenerationID, 10)
		}
		nodes = append(nodes, m)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"id": strconv.FormatInt(out.Group.ID, 10), "status": out.Group.Status,
		"version": out.Group.Version, "nodes": nodes,
	})
}

// parseIDs 把字符串 ID 数组转成 int64。
func parseIDs(w http.ResponseWriter, r *http.Request, in []string, field string) (
	[]int64, bool) {
	out := make([]int64, 0, len(in))
	for _, s := range in {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, field+" 含非法 ID"))
			return nil, false
		}
		out = append(out, v)
	}
	return out, true
}

// ---------------------------------------------------------------- 导出

func (h *Handler) export(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ctx(w, r)
	if !ok {
		return
	}
	id, ok2 := pathID(w, r, "scriptID")
	if !ok2 {
		return
	}
	out, err := h.svc.Export(r.Context(), ws.WorkspaceID, id,
		time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	// ⚠️ 带 Content-Disposition：浏览器直接另存而不是当成一个页面渲染。
	// 文件名用脚本 ID 而不是标题 —— 标题里可能有斜杠、引号、换行，
	// 拼进 header 轻则文件名乱码、重则被当成 header 注入。
	w.Header().Set("Content-Disposition",
		`attachment; filename="script-`+strconv.FormatInt(id, 10)+`.json"`)
	httpx.JSON(w, http.StatusOK, out)
}
