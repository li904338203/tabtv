// Package transport 暴露媒体上传与受保护读取。
package transport

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	collabdomain "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	collabtransport "github.com/li904338203/tabtv/backend/internal/collaboration/transport"
	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	"github.com/li904338203/tabtv/backend/internal/media/domain"
	"github.com/li904338203/tabtv/backend/internal/media/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

type Handler struct {
	svc     *mediaapp.Service
	userID  func(r *http.Request) int64
	adminID func(r *http.Request) int64
}

func NewHandler(svc *mediaapp.Service, userID, adminID func(r *http.Request) int64) *Handler {
	return &Handler{svc: svc, userID: userID, adminID: adminID}
}

// maxUploadBytes 是 multipart 解析的硬上限。
// ⚠️ 上限要在反向代理、HTTP server、领域校验器**三层**各配一次（08 §4.3）：
// 只配一层的话，绕过那一层就等于没限制。
const maxUploadBytes = 100 << 20

func (h *Handler) Mount(r chi.Router, requireUser, requireAdmin func(http.Handler) http.Handler,
	requireWorkspace func(http.Handler) http.Handler) {
	// 受保护读取与用户上传：**必须带工作区上下文**。
	// ⚠️ 不带工作区的话就只能按 mediaID 查，那正是 P03-04 留下的越权口子：
	// 任何登录用户按 id 就能取到别人工作区的文件。
	r.Group(func(r chi.Router) {
		r.Use(requireUser)
		r.Use(requireWorkspace)
		r.Get("/media", h.listMine)
		r.Get("/media/{mediaID}", h.download)
		r.Delete("/media/{mediaID}", h.deleteMedia)
		r.Get("/media/{mediaID}/usages", h.usages)
		// 元信息（尺寸/类型）。⚠️ 蒙版编辑器必须先知道**原图像素尺寸**：
		// 按显示尺寸导出的蒙版服务端一律拒（CD-46），而前端只有从这里
		// 才拿得到那两个数字 —— 图片本身 decode 出来的是浏览器缩放后的尺寸。
		r.Get("/media/{mediaID}/info", h.mediaInfo)
		// 剪辑台的预览素材（SCT-16，SCD-30）：预览版、拼图、波形、节拍。只读（view），还没生成好时 status 说明进度。
		r.Get("/media/{mediaID}/preview-assets", h.previewAssets)
		r.Post("/media/references", h.uploadReference)
		// 宫格切分（NEW-14）：切块是独立素材，切完就能作为下一次生成的输入。
		r.Post("/media/{mediaID}/grid-split", h.splitGrid)
		r.Get("/media/{mediaID}/grid-tiles", h.gridTiles)
		// 批量导出（NEW-15、GAP-12、GAP-18）：服务端异步打包，
		// 下载仍走上面那个受保护端点 —— 前端不再有任何裸 `a.href`。
		// ⚠️ 打包导出是**把内容取出去**，标 export（P12-03）。
		// 它不扣积分（只是把已经属于用户的字节重新搬一次），但"能不能取出去"
		// 是一个独立于"能不能编辑"的授权维度 —— 共享授权里 view/export 分开
		// 正是为了表达「只给看不给下」。
		r.With(collabtransport.RequireAction(collabdomain.ActionExport)).Post("/exports", h.createExport)
		r.Get("/exports", h.listExports)
		r.Get("/exports/{exportID}", h.getExport)
	})
	// 管理端上传与读取**平台级**素材（Banner 图、素材封面、音色试听、水印图）。
	r.Group(func(r chi.Router) {
		r.Use(requireAdmin)
		r.Post("/admin/media", h.adminUpload)
		r.Get("/admin/media/{mediaID}", h.adminDownload)
	})
}

// adminPurposes 管理端上传允许的用途（都是平台级素材）。
var adminPurposes = map[string]bool{"banner": true, "material_cover": true, "avatar": true}

func (h *Handler) adminUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.PayloadTooLarge, "请求体过大或格式非法"))
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "缺少 file 字段"))
		return
	}
	defer file.Close()

	purpose := r.FormValue("purpose")
	if purpose == "" {
		purpose = "material_cover"
	}
	// 只收平台级用途：upload_reference / generation_output 这类属于用户工作区，
	// 落在工作区 0 上没有意义；不在这里拦的话会撞上表的 CHECK 约束，表现为 500。
	if !adminPurposes[purpose] {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "purpose 取值非法（只允许 banner / material_cover / avatar）"))
		return
	}
	var expect domain.Kind
	switch r.FormValue("expectKind") {
	case "image":
		expect = domain.KindImage
	case "audio":
		expect = domain.KindAudio
	case "video":
		expect = domain.KindVideo
	}

	up, err := h.svc.Upload(r.Context(), mediaapp.UploadInput{
		// 管理端上传的是平台级素材，不属于任何工作区。
		WorkspaceID:  0,
		UserID:       h.adminID(r),
		Purpose:      purpose,
		DeclaredMIME: header.Header.Get("Content-Type"),
		FileName:     header.Filename,
		Size:         header.Size,
		Body:         http.MaxBytesReader(w, file, maxUploadBytes),
		ExpectKind:   expect,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{
		"id":          strconv.FormatInt(up.ID, 10),
		"contentType": up.MIME,
		"sizeBytes":   up.SizeBytes,
		"url":         "/api/v1/media/" + strconv.FormatInt(up.ID, 10),
	})
}

// download 受保护读取。
//
// ⚠️ `<img src>` 与 `<a href>` **不会带 Authorization 头**（CD-16）：
// 前端必须 fetch 成 blob 再转 object URL，并在卸载时 revokeObjectURL。
func (h *Handler) download(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "mediaID"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "mediaID 非法"))
		return
	}
	ws, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
		return
	}
	width, _ := strconv.Atoi(r.URL.Query().Get("w"))
	body, m, err := h.svc.Download(r.Context(), ws.WorkspaceID, id, width)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	writeMedia(w, body, m)
}

// adminDownload 管理端读取**平台级**素材（Banner 图、素材封面、水印图）。
//
// ⚠️ 工作区固定为 0：仓储的归属条件是「本工作区 或 工作区 0」，传 0 就**只取得到平台级素材**。
// 管理端不能借这条路读用户的私有产物 —— 那需要单独的授权与审计，不能顺手打开。
// 此前管理端只有上传没有读取：传上去的 Banner 图、水印图在后台一张都看不到（P15-05 发现）。
func (h *Handler) adminDownload(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "mediaID"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "mediaID 非法"))
		return
	}
	width, _ := strconv.Atoi(r.URL.Query().Get("w"))
	body, m, err := h.svc.Download(r.Context(), 0, id, width)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	writeMedia(w, body, m)
}

func writeMedia(w http.ResponseWriter, body io.ReadCloser, m repo.MediaObject) {
	defer body.Close()
	w.Header().Set("Content-Type", m.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(m.SizeBytes, 10))
	// 平台级素材（封面/试听）可缓存；用户私有产物由 P04-04 另行处理。
	if m.WorkspaceID == 0 {
		w.Header().Set("Cache-Control", "private, max-age=3600")
	} else {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
}

func (h *Handler) listMine(w http.ResponseWriter, r *http.Request) {
	ws, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	var cursor int64
	if c := q.Get("cursor"); c != "" {
		cursor, _ = strconv.ParseInt(c, 10, 64)
	}
	rows, hasMore, err := h.svc.ListMine(r.Context(), ws.WorkspaceID, q.Get("kind"), limit, cursor)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	type item struct {
		ID        string `json:"id"`
		Kind      string `json:"kind"`
		MIME      string `json:"mime"`
		SizeBytes int64  `json:"sizeBytes"`
		// 契约里一直有这个字段，此前没输出（资产库「我上传的」的日期因此是空的）
		CreatedAt string `json:"createdAt,omitempty"`
	}
	items := make([]item, 0, len(rows))
	for _, m := range rows {
		it := item{
			ID: strconv.FormatInt(m.ID, 10), Kind: mediaapp.KindOf(m.ContentType),
			MIME: m.ContentType, SizeBytes: m.SizeBytes,
		}
		if !m.CreatedAt.IsZero() {
			it.CreatedAt = m.CreatedAt.UTC().Format(time.RFC3339)
		}
		items = append(items, it)
	}
	var next *string
	if hasMore && len(items) > 0 {
		s := items[len(items)-1].ID
		next = &s
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"items": items, "nextCursor": next, "hasMore": hasMore,
	})
}

func (h *Handler) deleteMedia(w http.ResponseWriter, r *http.Request) {
	ws, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "mediaID"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "mediaID 非法"))
		return
	}
	if err := h.svc.Delete(r.Context(), ws.WorkspaceID, id); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) usages(w http.ResponseWriter, r *http.Request) {
	ws, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "mediaID"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "mediaID 非法"))
		return
	}
	list, err := h.svc.Usages(r.Context(), ws.WorkspaceID, id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	type resp struct {
		UsageType string `json:"usageType"`
		UsageRef  string `json:"usageRefId"`
		CreatedAt string `json:"createdAt"`
	}
	out := make([]resp, 0, len(list))
	for _, u := range list {
		out = append(out, resp{u.UsageType, strconv.FormatInt(u.RefID, 10),
			u.CreatedAt.UTC().Format(time.RFC3339)})
	}
	httpx.JSON(w, http.StatusOK, out)
}

// splitGrid 宫格切分。
//
// ⚠️ **不走生成链路**：没有模型、没有上游、没有费用，是一次本地像素搬运。
// 同步返回而不是建任务：几毫秒的事建一条任务只会让任务列表变脏。
func (h *Handler) splitGrid(w http.ResponseWriter, r *http.Request) {
	ws, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
		return
	}
	id, ok2 := mediaPathID(w, r)
	if !ok2 {
		return
	}
	var req struct {
		Rows int `json:"rows"`
		Cols int `json:"cols"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "请求体不是合法 JSON"))
		return
	}
	tiles, err := h.svc.SplitGrid(r.Context(), mediaapp.SplitInput{
		WorkspaceID: ws.WorkspaceID, UserID: h.userID(r),
		SourceMediaID: id, Rows: req.Rows, Cols: req.Cols,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{
		"sourceMediaId": strconv.FormatInt(id, 10),
		"rows":          req.Rows, "cols": req.Cols,
		"tiles": tileResp(tiles),
	})
}

func (h *Handler) gridTiles(w http.ResponseWriter, r *http.Request) {
	ws, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
		return
	}
	id, ok2 := mediaPathID(w, r)
	if !ok2 {
		return
	}
	rows, _ := strconv.Atoi(r.URL.Query().Get("rows"))
	cols, _ := strconv.Atoi(r.URL.Query().Get("cols"))
	tiles, err := h.svc.ListGridTiles(r.Context(), ws.WorkspaceID, id, rows, cols)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"tiles": tileResp(tiles)})
}

func tileResp(tiles []mediaapp.SplitTile) []map[string]any {
	out := make([]map[string]any, 0, len(tiles))
	for _, t := range tiles {
		out = append(out, map[string]any{
			"mediaId": strconv.FormatInt(t.MediaID, 10),
			"index":   t.Index, "row": t.Row, "col": t.Col,
			"width": t.Width, "height": t.Height, "label": t.Label,
			// url 指向受保护端点 —— 前端一律走 useAuthenticatedMedia。
			"url": "/api/v1/media/" + strconv.FormatInt(t.MediaID, 10),
		})
	}
	return out
}

func (h *Handler) createExport(w http.ResponseWriter, r *http.Request) {
	ws, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
		return
	}
	var req struct {
		Items []struct {
			MediaID string `json:"mediaId"`
			Name    string `json:"name"`
		} `json:"items"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "请求体不是合法 JSON"))
		return
	}
	items := make([]domain.ExportItem, 0, len(req.Items))
	for _, it := range req.Items {
		id, err := strconv.ParseInt(it.MediaID, 10, 64)
		if err != nil {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "清单里有非法的 mediaId"))
			return
		}
		items = append(items, domain.ExportItem{MediaID: id, Name: it.Name})
	}
	task, err := h.svc.CreateExport(r.Context(), mediaapp.CreateExportInput{
		WorkspaceID: ws.WorkspaceID, UserID: h.userID(r), Items: items,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusAccepted, exportResp(task))
}

func (h *Handler) getExport(w http.ResponseWriter, r *http.Request) {
	ws, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "exportID"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "exportID 非法"))
		return
	}
	task, err := h.svc.GetExport(r.Context(), ws.WorkspaceID, id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, exportResp(task))
}

func (h *Handler) listExports(w http.ResponseWriter, r *http.Request) {
	ws, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	var cursor int64
	if c := r.URL.Query().Get("cursor"); c != "" {
		cursor, _ = strconv.ParseInt(c, 10, 64)
	}
	rows, hasMore, err := h.svc.ListExports(r.Context(), ws.WorkspaceID, limit, cursor)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(rows))
	for _, t := range rows {
		items = append(items, exportResp(t))
	}
	var next *string
	if hasMore && len(rows) > 0 {
		s := strconv.FormatInt(rows[len(rows)-1].ID, 10)
		next = &s
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"items": items, "nextCursor": next, "hasMore": hasMore,
	})
}

func exportResp(t repo.ExportTask) map[string]any {
	// ⚠️ 状态是**算出来的**：库里存的 success 在包被清理之后仍是 success，
	// 直接下发的话界面上会有一个点下去 404 的下载按钮。
	status := domain.EffectiveExportStatus(t.Status, t.ExpiresAt, time.Now())
	out := map[string]any{
		"id":        strconv.FormatInt(t.ID, 10),
		"kind":      t.Kind,
		"status":    status,
		"itemCount": t.ItemCount,
		"sizeBytes": t.SizeBytes,
		"expiresAt": t.ExpiresAt.UTC().Format(time.RFC3339),
		"createdAt": t.CreatedAt.UTC().Format(time.RFC3339),
		"updatedAt": t.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if t.ErrorCode != "" {
		out["errorCode"] = t.ErrorCode
		out["errorMessage"] = t.ErrorMsg
	}
	// 只有还能下载时才给地址：过期后给了地址就是给一个 404。
	if status == domain.ExportSuccess && t.MediaID != 0 {
		out["mediaId"] = strconv.FormatInt(t.MediaID, 10)
		out["url"] = "/api/v1/media/" + strconv.FormatInt(t.MediaID, 10)
	}
	return out
}

func mediaPathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "mediaID"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "mediaID 非法"))
		return 0, false
	}
	return id, true
}

// mediaInfo 媒体元信息。
//
// ⚠️ 与下载端点分开：下载出的是字节，前端拿不到宽高（浏览器解出来的是
// 缩放后的显示尺寸）。而尺寸是蒙版能不能对齐的唯一依据。
//
// ⚠️ 仍然**不回 bucket / objectKey**：调用方拿不到定位信息，
// 就不可能绕过受保护读取去直接取对象（GO-27）。
func (h *Handler) mediaInfo(w http.ResponseWriter, r *http.Request) {
	ws, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
		return
	}
	id, ok2 := mediaPathID(w, r)
	if !ok2 {
		return
	}
	m, err := h.svc.Info(r.Context(), ws.WorkspaceID, id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := map[string]any{
		"id":        strconv.FormatInt(m.ID, 10),
		"mime":      m.ContentType,
		"kind":      mediaapp.KindOf(m.ContentType),
		"sizeBytes": m.SizeBytes,
		"width":     m.Width,
		"height":    m.Height,
		// ⚠️ 时长也要给：时间线的裁切校验靠它，而前端从视频本身
		// 只能等 loadedmetadata 之后才拿得到 —— 那已经是下载完一段之后了。
		"durationMs": m.DurationMs,
	}
	// 派生溯源：切块要能说清自己是从哪来的（NEW-14）。
	// ⚠️ 源图可能已被删除 —— 那不是错误，只是溯源到此为止。
	if m.SourceMediaID != 0 {
		out["sourceMediaId"] = strconv.FormatInt(m.SourceMediaID, 10)
		out["deriveKind"] = m.DeriveKind
		out["deriveIndex"] = m.DeriveIndex
	}
	// 视频的封面帧（design/03 R-5）：前端靠它显示画面、按真实比例摆卡片 —— 否则要先把整段视频
	// 取回来才知道长什么样（一段 30 秒的 480p 产出就有 17MB）。还没截出来（Worker 异步截）时不给。
	// ⚠️ 查不到封面帧不是错误，查询失败也只告警：元信息的其余部分照常返回。
	if mediaapp.KindOf(m.ContentType) == "video" {
		p, ok, err := h.svc.PosterOf(r.Context(), ws.WorkspaceID, m.ID)
		switch {
		case err != nil:
			slog.WarnContext(r.Context(), "查询视频封面帧失败", "mediaId", m.ID, "error", err)
		case ok:
			out["posterMediaId"] = strconv.FormatInt(p.ID, 10)
			// 视频自己的宽高上传时没探测（只对图片读头部），封面帧是按原尺寸截的，比例就是视频的比例。
			if (m.Width == 0 || m.Height == 0) && p.Width > 0 && p.Height > 0 {
				out["width"], out["height"] = p.Width, p.Height
			}
		}
	}
	httpx.JSON(w, http.StatusOK, out)
}

// uploadReference 用户上传参考文件。
//
// ⚠️ 校验**按实际文件头**而不是声明 MIME（GO-28），上限在
// 反向代理、HTTP server 与领域校验器三层各配一次 —— 少一层就有一条绕过路径。
func (h *Handler) uploadReference(w http.ResponseWriter, r *http.Request) {
	ws, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
		return
	}
	uid := h.userID(r)
	if uid == 0 {
		httpx.Fail(w, r, httpx.Err(errcode.Unauthenticated))
		return
	}
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.PayloadTooLarge, "请求体过大或格式非法"))
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "缺少 file 字段"))
		return
	}
	defer file.Close()

	expect, ok := referenceExpectKind(r.FormValue("mediaType"))
	if !ok {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "mediaType 取值非法"))
		return
	}

	up, err := h.svc.Upload(r.Context(), mediaapp.UploadInput{
		WorkspaceID: ws.WorkspaceID, UserID: uid, Purpose: "upload_reference",
		DeclaredMIME: header.Header.Get("Content-Type"), FileName: header.Filename,
		Size: header.Size, Body: file, ExpectKind: expect,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{
		"id":        strconv.FormatInt(up.ID, 10),
		"mime":      up.MIME,
		"kind":      string(up.Kind),
		"sizeBytes": up.SizeBytes,
		"checksum":  up.Checksum,
	})
}

// referenceExpectKind 把参考上传的可选 `mediaType` 换成允许的媒体大类。
//
// 空串表示不限定（按实际文件头识别）—— 契约里 `mediaType` 与 `projectId` / `nodeId`
// 一样是可选的（MKT-T-01）：创造页与营销页都在建项目之前上传。
// `projectId` / `nodeId` 这里**不读**，归属按工作区登记。
func referenceExpectKind(v string) (domain.Kind, bool) {
	switch v {
	case "image":
		return domain.KindImage, true
	case "audio":
		return domain.KindAudio, true
	case "video":
		return domain.KindVideo, true
	case "":
		return "", true
	default:
		return "", false
	}
}

// ServePublic 输出一份**已经被调用方判定为公开**的媒体（P13-01）。
//
// ⚠️ 它**不做归属校验** —— 这是它与 `download` 唯一的区别，也是唯一的风险点。
// 调用方（内容域）必须先按作品的公开可见性（`approved ∧ featured ∧ ¬offline`）
// 判过，那一步在 SQL 里，所以取消加精或下架之后这里**立刻**取不到。
//
// ⚠️ 正因为不校验归属，这个方法**绝不能**直接挂成一条按 mediaID 取图的路由 ——
// 那样任何人拿一个 ID 就能取到别人的私有产物。它只能被内容域在判过之后调用。
func (h *Handler) ServePublic(w http.ResponseWriter, r *http.Request, mediaID int64, width int) {
	// workspaceID 传 0：`GetScoped` 对 0 的语义是"平台级素材"，
	// 用它会把用户产物挡在外面。这里用不带租户条件的读取。
	body, m, err := h.svc.OpenPublic(r.Context(), mediaID, width)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	writePublic(w, body, m)
}

// ServePublicCover 输出一份已判定为公开的媒体的**封面图**（图片的缩略图 / 视频的第一帧，见 OpenPublicCover）。
// 不做归属校验，约束同 ServePublic：只能被内容域在判过作品可见性之后调用。
func (h *Handler) ServePublicCover(w http.ResponseWriter, r *http.Request, mediaID int64, width int) {
	body, m, err := h.svc.OpenPublicCover(r.Context(), mediaID, width)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	writePublic(w, body, m)
}

// RedirectPublic 同 ServePublic（调用方已判过可见性、不做归属校验），但**不经 API 转发字节**：
// 302 到对象存储的短时效签名地址，浏览器直接去取（2026-11-02，见 media/app/public_link.go）。
//
// ⚠️ 只给 `<img>` / `<video>` 直接加载的匿名公开端点用；用 fetch 读字节的端点（作者看自己的作品）跳转后
// 读不到跨源响应体，那里继续用 ServePublic。存储签不出直连地址时（本地 local 存储）退回 ServePublic。
func (h *Handler) RedirectPublic(w http.ResponseWriter, r *http.Request, mediaID int64, width int) {
	h.redirectPublic(w, r, mediaID, width, false)
}

// RedirectPublicCover 是 ServePublicCover 的跳转版（图片缩略图 / 视频封面帧），约束同 RedirectPublic。
func (h *Handler) RedirectPublicCover(w http.ResponseWriter, r *http.Request, mediaID int64, width int) {
	h.redirectPublic(w, r, mediaID, width, true)
}

func (h *Handler) redirectPublic(w http.ResponseWriter, r *http.Request, mediaID int64, width int, cover bool) {
	u, err := h.svc.PublicLink(r.Context(), mediaID, width, cover)
	if errors.Is(err, mediaapp.ErrNoDirectLink) {
		if cover {
			h.ServePublicCover(w, r, mediaID, width)
		} else {
			h.ServePublic(w, r, mediaID, width)
		}
		return
	}
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	// ⚠️ 跳转本身可以缓存，时长与原来的公开缓存一致（5 分钟）：这是「撤下来之后多久看不到」的窗口，
	// 不能因为改成跳转就变长。签名地址自己的有效期（15 分钟）只约束已经拿到地址的人。
	w.Header().Set("Cache-Control", "public, max-age=300")
	http.Redirect(w, r, u, http.StatusFound)
}

func writePublic(w http.ResponseWriter, body io.ReadCloser, m repo.MediaObject) {
	defer body.Close()
	w.Header().Set("Content-Type", m.ContentType)
	// ⚠️ 公开媒体可以缓存，但**必须是 public 且时间不能长**：
	// 取消加精之后 CDN/浏览器那份缓存仍会命中，缓存期就是"撤不下来"的窗口。
	// 5 分钟是在"首页别每次都回源"与"撤下来要快"之间取的折中。
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
}

// previewAssets 返回剪辑台用的预览素材（契约 PreviewAssets）。
//
// ⚠️ 只给派生件的 ID，不给签名地址：预览版仍走受保护的下载端点（按工作区卡归属）。
func (h *Handler) previewAssets(w http.ResponseWriter, r *http.Request) {
	ws, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
		return
	}
	id, ok2 := mediaPathID(w, r)
	if !ok2 {
		return
	}
	p, err := h.svc.PreviewAssets(r.Context(), ws.WorkspaceID, id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, previewAssetsResp(p))
}

func previewAssetsResp(p mediaapp.PreviewAssets) map[string]any {
	out := map[string]any{"mediaId": strconv.FormatInt(p.MediaID, 10), "status": p.Status,
		"preview": nil, "filmstrip": nil, "peaks": nil, "beats": nil}
	if v := p.Preview; v != nil {
		out["preview"] = map[string]any{"mediaId": strconv.FormatInt(v.MediaID, 10), "width": v.Width,
			"height": v.Height, "durationMs": v.DurationMs}
	}
	if v := p.Filmstrip; v != nil {
		out["filmstrip"] = v
	}
	if v := p.Peaks; v != nil {
		out["peaks"] = v
	}
	if v := p.Beats; v != nil {
		out["beats"] = v
	}
	return out
}
