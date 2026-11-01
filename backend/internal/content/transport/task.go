package transport

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	collabdomain "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	collabtransport "github.com/li904338203/tabtv/backend/internal/collaboration/transport"
	"github.com/li904338203/tabtv/backend/internal/content/domain"
	"github.com/li904338203/tabtv/backend/internal/content/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// MountTasks 挂载任务中心与作品（P13-02；GAP-05、AD-09 的用户侧）。
//
// ⚠️ 提交作品要 **export**：把自己的产出发到公开墙候选里，是"把内容取出去"
// 那一档，不是日常编辑。只读成员不该能替团队投稿。
func (h *Handler) MountTasks(r chi.Router, requireUser, guard, wsCtx func(http.Handler) http.Handler) {
	r.Group(func(r chi.Router) {
		r.Use(requireUser, guard)
		r.Get("/tasks", h.tasks)
		// 「我的作品」页 = 全部成功产出（老系统的语义）；/works 是「提交去审核的作品」。
		r.Get("/creations", h.creations)
		r.Get("/works", h.myWorks)
		// ⚠️ 作者看自己作品的图**另开一条路**，不复用公开取图：
		// 刚提交还没过审的那一件在公开路径上取不到（那正是它该有的样子），
		// 而作者必须看得见 —— 否则"我的作品"页上全是破图。
		r.Get("/works/{workID}/media", h.ownWorkMedia)
		// 我的资产（迁移 00074）：默认闸按方法给权限 —— 看要 view，保存 / 改 / 删 / 打分要 edit
		h.mountLibrary(r)
	})
	r.Group(func(r chi.Router) {
		r.Use(requireUser, wsCtx,
			collabtransport.RequireAction(collabdomain.ActionExport))
		r.Post("/works", h.submitWork)
	})
}

func (h *Handler) tasks(w http.ResponseWriter, r *http.Request) {
	ws, _ := collabtransport.WorkspaceFrom(r.Context())
	q := r.URL.Query()
	limit := 20
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	var cursorAt *time.Time
	var cursorID int64
	if v := q.Get("cursorAt"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			cursorAt = &t
		}
	}
	if v := q.Get("cursorId"); v != "" {
		cursorID, _ = strconv.ParseInt(v, 10, 64)
	}

	list, counts, err := h.svc.Tasks(r.Context(), ws.WorkspaceID,
		q.Get("kind"), q.Get("state"), limit, cursorAt, cursorID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(list))
	for _, t := range list {
		items = append(items, map[string]any{
			"id": itoa(t.ID), "kind": string(t.Kind), "state": string(t.State),
			"title": t.Title, "credits": t.Credits, "error": t.Error,
			"refId":     itoa(t.RefID),
			"createdAt": t.CreatedAt.UTC().Format(time.RFC3339),
			// ⚠️ 终态由**服务端**给（`domain.TerminalState`），不让前端
			// 按状态串自己判 —— 新增一个终态时前端会漏掉，
			// 而漏掉的表现是它一直轮询一个永远不再变的任务。
			"terminal": domain.TerminalState(t.State),
		})
	}
	byState := map[string]int{}
	for k, v := range counts {
		byState[string(k)] = v
	}
	resp := map[string]any{
		"items": items, "counts": byState, "nextCursorAt": "", "nextCursorId": "",
	}
	if len(list) == limit && len(list) > 0 {
		last := list[len(list)-1]
		resp["nextCursorAt"] = last.CreatedAt.UTC().Format(time.RFC3339)
		resp["nextCursorId"] = itoa(last.ID)
	}
	httpx.JSON(w, http.StatusOK, resp)
}

func workResp(v repo.Work) map[string]any {
	out := map[string]any{
		"id": itoa(v.ID), "sourceType": v.SourceType, "sourceId": itoa(v.SourceID),
		"title": v.Title, "reviewStatus": v.ReviewStatus,
		"isFeatured": v.IsFeatured, "isOffline": v.IsOffline,
		"reviewReason": v.ReviewReason, "submittedAt": "",
		"createdAt": v.CreatedAt.UTC().Format(time.RFC3339),
		// ⚠️ 与公开墙一致：**不下发 mediaObjectId**。作者本人要看图走
		// 受保护的产物端点（他有归属），公开取图走 `/showcase/{id}/media`。
		"hasMedia": v.MediaObjectID != 0,
	}
	if v.SubmittedAt != nil {
		out["submittedAt"] = v.SubmittedAt.UTC().Format(time.RFC3339)
	}
	return out
}

func (h *Handler) myWorks(w http.ResponseWriter, r *http.Request) {
	ws, _ := collabtransport.WorkspaceFrom(r.Context())
	limit := 24
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	var cursor int64
	if v := r.URL.Query().Get("cursor"); v != "" {
		cursor, _ = strconv.ParseInt(v, 10, 64)
	}
	list, err := h.svc.MyWorks(r.Context(), ws.WorkspaceID, limit, cursor)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(list))
	for _, v := range list {
		items = append(items, workResp(v))
	}
	next := ""
	if len(list) == limit && len(list) > 0 {
		next = itoa(list[len(list)-1].ID)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": next})
}

func (h *Handler) ownWorkMedia(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "workID"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "workID 非法"))
		return
	}
	ws, _ := collabtransport.WorkspaceFrom(r.Context())
	mediaID, err := h.svc.OwnMediaOf(r.Context(), id, ws.WorkspaceID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if h.media == nil {
		httpx.Fail(w, r, httpx.Errf(errcode.Internal, "媒体服务未接入"))
		return
	}
	width := 320
	if v := r.URL.Query().Get("w"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			width = n
		}
	}
	// 归属已经在上一行按 workspaceID 判过了，这里只负责把字节吐出去。
	h.media.ServePublic(w, r, mediaID, width)
}

// WorkSourceGuard 由生成/时间线域实现：校验来源任务归属且已成功。
//
// ⚠️ 这一步**不能省，也不能由内容域自己查**：内容域不知道 generation
// 那张表的语义（什么算成功、产出在哪一项）。少了它，任何人都能拿一个
// 别人的 generationId 提交成自己的作品 —— 而作品墙上会出现他没做过的东西。
type WorkSourceGuard interface {
	// Resolve 回作品标题与主产物媒体 ID；不属于该工作区或未成功时返回错误。
	Resolve(r *http.Request, workspaceID int64, sourceType string, sourceID int64) (
		title string, mediaID int64, err error)
}

func (h *Handler) SetWorkSource(g WorkSourceGuard) { h.sources = g }

func (h *Handler) submitWork(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SourceType string `json:"sourceType"`
		SourceID   string `json:"sourceId"`
		Title      string `json:"title"`
	}
	if !decode(w, r, &req) {
		return
	}
	sourceID, err := strconv.ParseInt(req.SourceID, 10, 64)
	if err != nil || sourceID <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "sourceId 非法"))
		return
	}
	ws, _ := collabtransport.WorkspaceFrom(r.Context())
	if h.sources == nil {
		httpx.Fail(w, r, httpx.Errf(errcode.Internal, "来源校验未接入"))
		return
	}
	title, mediaID, err := h.sources.Resolve(r, ws.WorkspaceID, req.SourceType, sourceID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if req.Title != "" {
		title = req.Title
	}
	out, err := h.svc.SubmitWork(r.Context(), repo.Work{
		WorkspaceID: ws.WorkspaceID, SourceType: req.SourceType, SourceID: sourceID,
		MediaObjectID: mediaID, Title: title,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	// ⚠️ 提交成功 ≠ 已公开：响应里的 `reviewStatus` 是 pending，
	// 前端必须照着它说"已提交，等待审核"而不是"已发布"。
	httpx.JSON(w, http.StatusCreated, workResp(out))
}
