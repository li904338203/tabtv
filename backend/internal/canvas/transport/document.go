package transport

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/li904338203/tabtv/backend/internal/canvas/app"
	"github.com/li904338203/tabtv/backend/internal/canvas/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// mountDocuments 注册项目文档的四条路由（AGT-09）。
//
// 挂在 Mount 的工作区分组里：GuardByMethod 给的默认闸（GET → view，其余 → edit）
// 正好就是文档要的权限，app 层再判一次给 Agent 端口用。
func (h *Handler) mountDocuments(r chi.Router) {
	r.Get("/projects/{projectId}/documents", h.listDocuments)
	r.Post("/projects/{projectId}/documents", h.createDocument)
	r.Get("/projects/{projectId}/documents/{documentId}", h.getDocument)
	r.Put("/projects/{projectId}/documents/{documentId}", h.updateDocument)
}

type documentSummaryResp struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	Version   int    `json:"version"`
	Source    string `json:"source"`
	Bytes     int    `json:"bytes"`
	UpdatedAt string `json:"updatedAt"`
}

type documentResp struct {
	ID             string  `json:"id"`
	ProjectID      string  `json:"projectId"`
	Path           string  `json:"path"`
	Content        string  `json:"content"`
	Version        int     `json:"version"`
	Source         string  `json:"source"`
	LastEditorID   *string `json:"lastEditorId"`
	LastEditorName string  `json:"lastEditorName"`
	CreatedAt      string  `json:"createdAt"`
	UpdatedAt      string  `json:"updatedAt"`
}

func toDocument(d repo.Document) documentResp {
	out := documentResp{
		ID: idStr(d.ID), ProjectID: idStr(d.ProjectID), Path: d.Path, Content: d.Content,
		Version: d.Version, Source: d.Source, LastEditorName: d.EditorName,
		CreatedAt: d.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: d.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if d.EditorID != 0 {
		s := idStr(d.EditorID)
		out.LastEditorID = &s
	}
	return out
}

func (h *Handler) listDocuments(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := h.ctx(w, r)
	if !ok {
		return
	}
	pid, ok := pathID(w, r, "projectId")
	if !ok {
		return
	}
	list, err := h.svc.ListDocuments(r.Context(), ws.WorkspaceID, pid, ws.Role)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]documentSummaryResp, 0, len(list))
	for _, d := range list {
		items = append(items, documentSummaryResp{
			ID: idStr(d.ID), Path: d.Path, Version: d.Version, Source: d.Source, Bytes: d.Bytes,
			UpdatedAt: d.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) createDocument(w http.ResponseWriter, r *http.Request) {
	ws, uid, ok := h.ctx(w, r)
	if !ok {
		return
	}
	pid, ok := pathID(w, r, "projectId")
	if !ok {
		return
	}
	var req struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if !decode(w, r, &req) {
		return
	}
	d, err := h.svc.CreateDocument(r.Context(), ws.WorkspaceID, pid, app.DocumentActor{UserID: uid},
		ws.Role, app.CreateDocumentInput{Path: req.Path, Content: req.Content})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, toDocument(d))
}

func (h *Handler) getDocument(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := h.ctx(w, r)
	if !ok {
		return
	}
	pid, ok := pathID(w, r, "projectId")
	if !ok {
		return
	}
	did, ok := pathID(w, r, "documentId")
	if !ok {
		return
	}
	d, err := h.svc.GetDocument(r.Context(), ws.WorkspaceID, pid, did, ws.Role)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toDocument(d))
}

func (h *Handler) updateDocument(w http.ResponseWriter, r *http.Request) {
	ws, uid, ok := h.ctx(w, r)
	if !ok {
		return
	}
	pid, ok := pathID(w, r, "projectId")
	if !ok {
		return
	}
	did, ok := pathID(w, r, "documentId")
	if !ok {
		return
	}
	var req struct {
		// 指针：缺省与空串是两件事 —— 不传 content 不能被当成"清空正文"。
		Content *string `json:"content"`
		Version int     `json:"version"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Content == nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "content 必填（清空请传空串）"))
		return
	}
	d, err := h.svc.UpdateDocument(r.Context(), ws.WorkspaceID, pid, did, app.DocumentActor{UserID: uid},
		ws.Role, req.Version, *req.Content)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toDocument(d))
}
