package transport

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/li904338203/tabtv/backend/internal/canvas/app"
	"github.com/li904338203/tabtv/backend/internal/canvas/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// mountDirectorScenes 注册 3D 导演台场景的路由（docs/agent/director，DRT-02）。
//
// 挂在 Mount 的工作区分组里：GuardByMethod 给的默认闸（GET → view，其余 → edit）正好就是场景要的权限。
// ⚠️ 不标 generate（DRD-32）：导演台免费，`generate` 是「会花用户的钱」的标记，标在这里会让
// 只有 edit 的角色被误拦，也让「哪些接口花钱」的代码检索失真。
func (h *Handler) mountDirectorScenes(r chi.Router) {
	r.Get("/projects/{projectId}/director-scenes", h.listDirectorScenes)
	r.Post("/projects/{projectId}/director-scenes", h.createDirectorScene)
	r.Get("/projects/{projectId}/director-scenes/{sceneId}", h.getDirectorScene)
	r.Put("/projects/{projectId}/director-scenes/{sceneId}", h.updateDirectorScene)
	// 媒体（DRT-03）：截图 / 控制图、预演视频、泼溅文件，以及记录的隐藏开关。
	r.Post("/projects/{projectId}/director-scenes/{sceneId}/captures", h.uploadDirectorCapture)
	r.Post("/projects/{projectId}/director-scenes/{sceneId}/previews", h.uploadDirectorPreview)
	r.Post("/projects/{projectId}/director-scenes/{sceneId}/assets", h.uploadDirectorAsset)
	r.Patch("/projects/{projectId}/director-scenes/{sceneId}/records/{recordId}", h.updateDirectorRecord)
}

// maxDirectorBody 是场景请求体的读取上限。正文本身的上限是 512 KiB（超出回 41301，由 app 层判），
// 这里只挡住离谱的请求体，免得把几百 MB 读进内存才发现它超了。
const maxDirectorBody = 2 << 20

type directorSceneSummaryResp struct {
	ID                string  `json:"id"`
	CanvasNodeID      string  `json:"canvasNodeId"`
	Version           int     `json:"version"`
	RenderFingerprint string  `json:"renderFingerprint"`
	OutputCameraID    *string `json:"outputCameraId"`
	LastEditorName    string  `json:"lastEditorName"`
	EditorKind        string  `json:"editorKind"`
	UpdatedAt         string  `json:"updatedAt"`
}

type directorMediaResp struct {
	ID                string          `json:"id"`
	SceneID           string          `json:"sceneId"`
	Kind              string          `json:"kind"`
	MediaID           string          `json:"mediaId"`
	Width             *int            `json:"width"`
	Height            *int            `json:"height"`
	DurationMs        *int            `json:"durationMs"`
	FPS               *int            `json:"fps"`
	CameraID          *string         `json:"cameraId"`
	CameraName        *string         `json:"cameraName"`
	Camera            json.RawMessage `json:"camera"`
	SceneVersion      *int            `json:"sceneVersion"`
	RenderFingerprint *string         `json:"renderFingerprint"`
	Hidden            bool            `json:"hidden"`
	CreatedAt         string          `json:"createdAt"`
}

type directorSceneResp struct {
	ID                string              `json:"id"`
	ProjectID         string              `json:"projectId"`
	CanvasNodeID      string              `json:"canvasNodeId"`
	SchemaVersion     int                 `json:"schemaVersion"`
	Version           int                 `json:"version"`
	RenderFingerprint string              `json:"renderFingerprint"`
	Scene             json.RawMessage     `json:"scene"`
	Records           []directorMediaResp `json:"records"`
	LastEditorID      *string             `json:"lastEditorId"`
	LastEditorName    string              `json:"lastEditorName"`
	EditorKind        string              `json:"editorKind"`
	CreatedAt         string              `json:"createdAt"`
	UpdatedAt         string              `json:"updatedAt"`
}

func toDirectorMedia(m repo.DirectorMedia) directorMediaResp {
	camera := m.Camera
	if len(camera) == 0 {
		camera = json.RawMessage("null")
	}
	return directorMediaResp{
		ID: idStr(m.ID), SceneID: idStr(m.SceneID), Kind: m.Kind, MediaID: idStr(m.MediaID),
		Width: m.Width, Height: m.Height, DurationMs: m.DurationMs, FPS: m.FPS,
		CameraID: m.CameraID, CameraName: m.CameraName, Camera: camera,
		SceneVersion: m.SceneVersion, RenderFingerprint: m.RenderFingerprint, Hidden: m.Hidden,
		CreatedAt: m.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func toDirectorScene(v app.DirectorSceneView) directorSceneResp {
	sc := v.Scene
	out := directorSceneResp{
		ID: idStr(sc.ID), ProjectID: idStr(sc.ProjectID), CanvasNodeID: sc.CanvasNodeID,
		SchemaVersion: sc.SchemaVersion, Version: sc.Version, RenderFingerprint: sc.RenderFingerprint,
		Scene: sc.Scene, Records: make([]directorMediaResp, 0, len(v.Records)),
		LastEditorName: sc.EditorName, EditorKind: sc.Source,
		CreatedAt: sc.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: sc.UpdatedAt.UTC().Format(time.RFC3339),
	}
	for _, m := range v.Records {
		out.Records = append(out.Records, toDirectorMedia(m))
	}
	if sc.EditorID != 0 {
		s := idStr(sc.EditorID)
		out.LastEditorID = &s
	}
	return out
}

func toDirectorSummary(s repo.DirectorSceneSummary) directorSceneSummaryResp {
	out := directorSceneSummaryResp{
		ID: idStr(s.ID), CanvasNodeID: s.CanvasNodeID, Version: s.Version,
		RenderFingerprint: s.RenderFingerprint, LastEditorName: s.EditorName, EditorKind: s.Source,
		UpdatedAt: s.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if s.OutputCameraID != "" {
		id := s.OutputCameraID
		out.OutputCameraID = &id
	}
	return out
}

// sceneSummaryOf 把保存后的整份场景压成摘要（PUT 的响应：正文就是请求里那份，不再回传）。
func sceneSummaryOf(sc repo.DirectorScene) repo.DirectorSceneSummary {
	var head struct {
		OutputCameraID *string `json:"outputCameraId"`
	}
	_ = json.Unmarshal(sc.Scene, &head)
	out := repo.DirectorSceneSummary{
		ID: sc.ID, CanvasNodeID: sc.CanvasNodeID, Version: sc.Version,
		RenderFingerprint: sc.RenderFingerprint, EditorName: sc.EditorName, Source: sc.Source,
		UpdatedAt: sc.UpdatedAt,
	}
	if head.OutputCameraID != nil {
		out.OutputCameraID = *head.OutputCameraID
	}
	return out
}

// decodeDirector 解码场景请求体：超出读取上限回 41301 而不是「不是合法 JSON」——
// 后者会让用户去查自己的正文哪里写错了，而问题其实是太大。
func decodeDirector(w http.ResponseWriter, r *http.Request, v any) bool {
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxDirectorBody)).Decode(v)
	if err == nil {
		return true
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		httpx.Fail(w, r, httpx.Errf(errcode.PayloadTooLarge, "场景正文超过 512 KiB 上限"))
		return false
	}
	httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "请求体不是合法 JSON"))
	return false
}

func (h *Handler) listDirectorScenes(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := h.ctx(w, r)
	if !ok {
		return
	}
	pid, ok := pathID(w, r, "projectId")
	if !ok {
		return
	}
	list, err := h.svc.ListDirectorScenes(r.Context(), ws.WorkspaceID, pid, ws.Role)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]directorSceneSummaryResp, 0, len(list))
	for _, s := range list {
		items = append(items, toDirectorSummary(s))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) createDirectorScene(w http.ResponseWriter, r *http.Request) {
	ws, uid, ok := h.ctx(w, r)
	if !ok {
		return
	}
	pid, ok := pathID(w, r, "projectId")
	if !ok {
		return
	}
	var req struct {
		CanvasNodeID  string `json:"canvasNodeId"`
		SourceSceneID string `json:"sourceSceneId"`
	}
	if !decodeDirector(w, r, &req) {
		return
	}
	var source int64
	if req.SourceSceneID != "" {
		id, err := strconv.ParseInt(req.SourceSceneID, 10, 64)
		if err != nil || id <= 0 {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "sourceSceneId 非法"))
			return
		}
		source = id
	}
	v, created, err := h.svc.CreateDirectorScene(r.Context(), ws.WorkspaceID, pid, app.DirectorActor{UserID: uid},
		ws.Role, app.CreateDirectorSceneInput{CanvasNodeID: req.CanvasNodeID, SourceSceneID: source})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	// ⚠️ 已有时回 200 而不是 201（DRD-17）：前端据此区分「新建」与「收敛到已有那份」，两种都要写回 sceneId。
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpx.JSON(w, status, toDirectorScene(v))
}

func (h *Handler) getDirectorScene(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := h.ctx(w, r)
	if !ok {
		return
	}
	pid, ok := pathID(w, r, "projectId")
	if !ok {
		return
	}
	sid, ok := pathID(w, r, "sceneId")
	if !ok {
		return
	}
	v, err := h.svc.GetDirectorScene(r.Context(), ws.WorkspaceID, pid, sid, ws.Role)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toDirectorScene(v))
}

func (h *Handler) updateDirectorScene(w http.ResponseWriter, r *http.Request) {
	ws, uid, ok := h.ctx(w, r)
	if !ok {
		return
	}
	pid, ok := pathID(w, r, "projectId")
	if !ok {
		return
	}
	sid, ok := pathID(w, r, "sceneId")
	if !ok {
		return
	}
	var req struct {
		// RawMessage：入库的是请求里的原始正文（未知字段原样保留，GO-35），不经 Go 结构体过一遍。
		Scene   json.RawMessage `json:"scene"`
		Version int             `json:"version"`
	}
	if !decodeDirector(w, r, &req) {
		return
	}
	// 缺省与 null 一样拒绝：都不是「一份场景」，不能被当成「清空场景」。
	if len(req.Scene) == 0 || string(req.Scene) == "null" {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "scene 必填"))
		return
	}
	saved, err := h.svc.UpdateDirectorScene(r.Context(), ws.WorkspaceID, pid, sid, app.DirectorActor{UserID: uid},
		ws.Role, req.Version, req.Scene)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toDirectorSummary(sceneSummaryOf(saved)))
}

// ---------------------------------------------------------------- 媒体上传（DRT-03）

// directorMultipartMemory 是 multipart 解析放在内存里的上限，超出的部分落临时文件（同参考图上传）。
const directorMultipartMemory = 8 << 20

// 请求体的外层上限：这一类媒体的默认上限再加 1 MiB 表单开销。只是挡住离谱的请求体 ——
// 不设的话 ParseMultipartForm 会把任意大的请求体都落进临时文件，读完才轮到媒体域按上限拒绝。
// 精确的上限（含配置收紧的预演 / 泼溅上限）由媒体域按实际字节判。
const (
	directorCaptureBodyMax = 10<<20 + 1<<20
	directorLargeBodyMax   = 100<<20 + 1<<20
)

// directorUploadForm 解析 multipart 并取出 file 字段。调用方负责关闭返回的文件。
func directorUploadForm(w http.ResponseWriter, r *http.Request, bodyMax int64) (app.DirectorFile, func(), bool) {
	r.Body = http.MaxBytesReader(w, r.Body, bodyMax)
	if err := r.ParseMultipartForm(directorMultipartMemory); err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.PayloadTooLarge, "请求体过大或格式非法"))
		return app.DirectorFile{}, nil, false
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "缺少 file 字段"))
		return app.DirectorFile{}, nil, false
	}
	return app.DirectorFile{DeclaredMIME: header.Header.Get("Content-Type"), FileName: header.Filename,
		Size: header.Size, Body: file}, func() { _ = file.Close() }, true
}

// formInt 取 multipart 里的整数字段；缺省或非法时回 40001 并点名字段。
func formInt(w http.ResponseWriter, r *http.Request, key string) (int, bool) {
	v, err := strconv.Atoi(r.FormValue(key))
	if err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, key+" 必须是整数"))
		return 0, false
	}
	return v, true
}

func (h *Handler) uploadDirectorCapture(w http.ResponseWriter, r *http.Request) {
	ws, uid, ok := h.ctx(w, r)
	if !ok {
		return
	}
	pid, ok := pathID(w, r, "projectId")
	if !ok {
		return
	}
	sid, ok := pathID(w, r, "sceneId")
	if !ok {
		return
	}
	file, done, ok := directorUploadForm(w, r, directorCaptureBodyMax)
	if !ok {
		return
	}
	defer done()
	version, ok := formInt(w, r, "sceneVersion")
	if !ok {
		return
	}
	rec, err := h.svc.UploadDirectorCapture(r.Context(), ws.WorkspaceID, pid, sid, app.DirectorActor{UserID: uid},
		ws.Role, app.DirectorCaptureInput{Kind: r.FormValue("kind"), CameraID: r.FormValue("cameraId"),
			SceneVersion: version, Camera: json.RawMessage(r.FormValue("camera")), File: file})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, toDirectorMedia(rec))
}

func (h *Handler) uploadDirectorPreview(w http.ResponseWriter, r *http.Request) {
	ws, uid, ok := h.ctx(w, r)
	if !ok {
		return
	}
	pid, ok := pathID(w, r, "projectId")
	if !ok {
		return
	}
	sid, ok := pathID(w, r, "sceneId")
	if !ok {
		return
	}
	file, done, ok := directorUploadForm(w, r, directorLargeBodyMax)
	if !ok {
		return
	}
	defer done()
	version, ok := formInt(w, r, "sceneVersion")
	if !ok {
		return
	}
	duration, ok := formInt(w, r, "durationMs")
	if !ok {
		return
	}
	fps, ok := formInt(w, r, "fps")
	if !ok {
		return
	}
	rec, err := h.svc.UploadDirectorPreview(r.Context(), ws.WorkspaceID, pid, sid, app.DirectorActor{UserID: uid},
		ws.Role, app.DirectorPreviewInput{CameraID: r.FormValue("cameraId"), SceneVersion: version,
			DurationMs: duration, FPS: fps, Camera: json.RawMessage(r.FormValue("camera")), File: file})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, toDirectorMedia(rec))
}

func (h *Handler) uploadDirectorAsset(w http.ResponseWriter, r *http.Request) {
	ws, uid, ok := h.ctx(w, r)
	if !ok {
		return
	}
	pid, ok := pathID(w, r, "projectId")
	if !ok {
		return
	}
	sid, ok := pathID(w, r, "sceneId")
	if !ok {
		return
	}
	file, done, ok := directorUploadForm(w, r, directorLargeBodyMax)
	if !ok {
		return
	}
	defer done()
	rec, err := h.svc.UploadDirectorAsset(r.Context(), ws.WorkspaceID, pid, sid, app.DirectorActor{UserID: uid},
		ws.Role, r.FormValue("kind"), file)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, toDirectorMedia(rec))
}

func (h *Handler) updateDirectorRecord(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := h.ctx(w, r)
	if !ok {
		return
	}
	pid, ok := pathID(w, r, "projectId")
	if !ok {
		return
	}
	sid, ok := pathID(w, r, "sceneId")
	if !ok {
		return
	}
	rid, ok := pathID(w, r, "recordId")
	if !ok {
		return
	}
	var req struct {
		// 指针：不传 hidden 是参数错误，不能被当成「取消隐藏」。
		Hidden *bool `json:"hidden"`
	}
	if !decodeDirector(w, r, &req) {
		return
	}
	if req.Hidden == nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "hidden 必填"))
		return
	}
	rec, err := h.svc.SetDirectorRecordHidden(r.Context(), ws.WorkspaceID, pid, sid, rid, ws.Role, *req.Hidden)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toDirectorMedia(rec))
}
