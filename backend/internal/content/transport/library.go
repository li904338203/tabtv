package transport

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	collabtransport "github.com/li904338203/tabtv/backend/internal/collaboration/transport"
	"github.com/li904338203/tabtv/backend/internal/content/app"
	"github.com/li904338203/tabtv/backend/internal/content/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// mountLibrary 我的资产（迁移 00074）。挂在带默认闸的组里：GET 要查看权限，写操作（保存 / 改 / 删 / 打分）要编辑权限。
func (h *Handler) mountLibrary(r chi.Router) {
	r.Get("/library/items", h.libraryItems)
	r.Post("/library/items", h.saveLibraryItem)
	r.Post("/library/items/batch", h.batchLibraryItems)
	r.Get("/library/items/{itemID}", h.libraryItem)
	r.Patch("/library/items/{itemID}", h.updateLibraryItem)
	r.Get("/library/folders", h.libraryFolders)
	r.Post("/library/folders", h.createLibraryFolder)
	r.Patch("/library/folders/{folderID}", h.renameLibraryFolder)
	r.Delete("/library/folders/{folderID}", h.deleteLibraryFolder)
	r.Get("/library/tags", h.libraryTags)
	r.Post("/library/tags", h.createLibraryTag)
	r.Delete("/library/tags/{name}", h.deleteLibraryTag)
	r.Get("/library/marks", h.generationMarks)
	r.Put("/library/ratings/{generationID}", h.rateGeneration)
}

func pathID(r *http.Request, key string) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, key), 10, 64)
	return id, err == nil && id > 0
}

func parseIDList(s string) ([]int64, bool) {
	out := []int64{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		id, err := strconv.ParseInt(p, 10, 64)
		if err != nil || id <= 0 {
			return nil, false
		}
		out = append(out, id)
	}
	return out, true
}

func libraryItemResp(it repo.LibraryItem) map[string]any {
	idOrNil := func(v int64) any {
		if v == 0 {
			return nil
		}
		return itoa(v)
	}
	out := map[string]any{
		"id": itoa(it.ID), "folderId": idOrNil(it.FolderID),
		"generationId": itoa(it.GenerationID), "outputIndex": it.OutputIdx,
		"kind": it.Kind, "title": it.Title, "tags": it.Tags, "rating": it.Rating,
		"projectId": idOrNil(it.ProjectID), "projectName": it.ProjectName, "nodeId": it.NodeID,
		"creatorName": it.CreatorName, "modelCode": it.ModelCode, "modelName": it.ModelName,
		"prompt":      it.Prompt,
		"generatedAt": it.GeneratedAt.UTC().Format(time.RFC3339),
		"savedAt":     it.SavedAt.UTC().Format(time.RFC3339),
	}
	if it.Kind == "text" {
		out["text"] = it.Text
	} else if it.MediaID != 0 {
		// 受保护端点（不是对象存储直链）：前端要 fetch 成 blob（同 /creations）
		id := itoa(it.MediaID)
		out["mediaId"], out["url"] = id, "/api/v1/media/"+id
	}
	if it.PosterMediaID != 0 {
		out["posterMediaId"] = itoa(it.PosterMediaID)
	}
	return out
}

func (h *Handler) libraryItems(w http.ResponseWriter, r *http.Request) {
	ws, _ := collabtransport.WorkspaceFrom(r.Context())
	q := r.URL.Query()
	query := app.LibraryQuery{Query: q.Get("q"), Kind: q.Get("kind")}
	// folderId：不传 = 不限；root = 只要顶层；其余 = 那个文件夹
	if v := q.Get("folderId"); v != "" {
		var id int64
		if v != "root" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n <= 0 {
				httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "folderId 非法"))
				return
			}
			id = n
		}
		query.FolderID = &id
	}
	if v := q.Get("tags"); v != "" {
		for _, t := range strings.Split(v, ",") {
			if t = strings.TrimSpace(t); t != "" {
				query.Tags = append(query.Tags, t)
			}
		}
	}
	query.MinRating, _ = strconv.Atoi(q.Get("minRating"))
	query.Limit, _ = strconv.Atoi(q.Get("limit"))
	query.GenerationID, _ = strconv.ParseInt(q.Get("generationId"), 10, 64)
	query.CursorID, _ = strconv.ParseInt(q.Get("cursor"), 10, 64)
	page, err := h.svc.LibraryItems(r.Context(), ws.WorkspaceID, query)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(page.Items))
	for _, it := range page.Items {
		items = append(items, libraryItemResp(it))
	}
	var next *string
	if page.HasMore && len(page.Items) > 0 {
		s := itoa(page.Items[len(page.Items)-1].ID)
		next = &s
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": next, "hasMore": page.HasMore, "total": page.Total})
}

func (h *Handler) libraryItem(w http.ResponseWriter, r *http.Request) {
	ws, _ := collabtransport.WorkspaceFrom(r.Context())
	id, ok := pathID(r, "itemID")
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.NotFound))
		return
	}
	it, err := h.svc.LibraryItem(r.Context(), ws.WorkspaceID, id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, libraryItemResp(it))
}

func (h *Handler) saveLibraryItem(w http.ResponseWriter, r *http.Request) {
	ws, _ := collabtransport.WorkspaceFrom(r.Context())
	var req struct {
		GenerationID string   `json:"generationId"`
		OutputIndex  int      `json:"outputIndex"`
		Title        string   `json:"title"`
		Text         string   `json:"text"`
		FolderID     string   `json:"folderId"`
		Tags         []string `json:"tags"`
	}
	if !decode(w, r, &req) {
		return
	}
	genID, _ := strconv.ParseInt(req.GenerationID, 10, 64)
	folderID, _ := strconv.ParseInt(req.FolderID, 10, 64)
	userID, _ := h.userIDs(r.Context())
	it, created, err := h.svc.SaveLibraryItem(r.Context(), ws.WorkspaceID, userID, app.SaveLibraryItemRequest{
		GenerationID: genID, OutputIdx: req.OutputIndex, Title: req.Title, Text: req.Text, FolderID: folderID, Tags: req.Tags,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpx.JSON(w, status, libraryItemResp(it))
}

func (h *Handler) updateLibraryItem(w http.ResponseWriter, r *http.Request) {
	ws, _ := collabtransport.WorkspaceFrom(r.Context())
	id, ok := pathID(r, "itemID")
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.NotFound))
		return
	}
	// folderId 区分「没传」（不改）与 null / ""（挪到顶层）：用原始 JSON 判断有没有这个键
	var raw map[string]json.RawMessage
	if !decode(w, r, &raw) {
		return
	}
	var in app.UpdateLibraryItemRequest
	if v, ok := raw["title"]; ok {
		var t string
		if err := json.Unmarshal(v, &t); err != nil {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "title 非法"))
			return
		}
		in.Title = &t
	}
	if v, ok := raw["folderId"]; ok {
		var folder int64
		var s *string
		if err := json.Unmarshal(v, &s); err != nil {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "folderId 非法"))
			return
		}
		if s != nil && *s != "" {
			n, err := strconv.ParseInt(*s, 10, 64)
			if err != nil || n <= 0 {
				httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "folderId 非法"))
				return
			}
			folder = n
		}
		in.FolderID = &folder
	}
	if v, ok := raw["tags"]; ok {
		var tags []string
		if err := json.Unmarshal(v, &tags); err != nil {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "tags 非法"))
			return
		}
		if tags == nil {
			tags = []string{}
		}
		in.Tags = &tags
	}
	it, err := h.svc.UpdateLibraryItem(r.Context(), ws.WorkspaceID, id, in)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, libraryItemResp(it))
}

func (h *Handler) batchLibraryItems(w http.ResponseWriter, r *http.Request) {
	ws, _ := collabtransport.WorkspaceFrom(r.Context())
	var req struct {
		Action   string   `json:"action"`
		IDs      []string `json:"ids"`
		FolderID string   `json:"folderId"`
	}
	if !decode(w, r, &req) {
		return
	}
	ids, ok := parseIDList(strings.Join(req.IDs, ","))
	if !ok {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "ids 非法"))
		return
	}
	folderID, _ := strconv.ParseInt(req.FolderID, 10, 64)
	n, err := h.svc.BatchLibraryItems(r.Context(), ws.WorkspaceID, req.Action, ids, folderID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"affected": n})
}

func folderResp(f repo.LibraryFolder) map[string]any {
	return map[string]any{"id": itoa(f.ID), "name": f.Name, "itemCount": f.ItemCount,
		"createdAt": f.CreatedAt.UTC().Format(time.RFC3339)}
}

func (h *Handler) libraryFolders(w http.ResponseWriter, r *http.Request) {
	ws, _ := collabtransport.WorkspaceFrom(r.Context())
	folders, err := h.svc.LibraryFolders(r.Context(), ws.WorkspaceID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(folders))
	for _, f := range folders {
		items = append(items, folderResp(f))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) createLibraryFolder(w http.ResponseWriter, r *http.Request) {
	ws, _ := collabtransport.WorkspaceFrom(r.Context())
	var req struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	userID, _ := h.userIDs(r.Context())
	f, err := h.svc.CreateLibraryFolder(r.Context(), ws.WorkspaceID, userID, req.Name)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, folderResp(f))
}

func (h *Handler) renameLibraryFolder(w http.ResponseWriter, r *http.Request) {
	ws, _ := collabtransport.WorkspaceFrom(r.Context())
	id, ok := pathID(r, "folderID")
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.NotFound))
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := h.svc.RenameLibraryFolder(r.Context(), ws.WorkspaceID, id, req.Name); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) deleteLibraryFolder(w http.ResponseWriter, r *http.Request) {
	ws, _ := collabtransport.WorkspaceFrom(r.Context())
	id, ok := pathID(r, "folderID")
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.NotFound))
		return
	}
	if err := h.svc.DeleteLibraryFolder(r.Context(), ws.WorkspaceID, id); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) libraryTags(w http.ResponseWriter, r *http.Request) {
	ws, _ := collabtransport.WorkspaceFrom(r.Context())
	preset, custom, err := h.svc.LibraryTags(r.Context(), ws.WorkspaceID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"preset": preset, "custom": custom})
}

func (h *Handler) createLibraryTag(w http.ResponseWriter, r *http.Request) {
	ws, _ := collabtransport.WorkspaceFrom(r.Context())
	var req struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	name, err := h.svc.CreateLibraryTag(r.Context(), ws.WorkspaceID, req.Name)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"name": name})
}

func (h *Handler) deleteLibraryTag(w http.ResponseWriter, r *http.Request) {
	ws, _ := collabtransport.WorkspaceFrom(r.Context())
	if err := h.svc.DeleteLibraryTag(r.Context(), ws.WorkspaceID, chi.URLParam(r, "name")); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) generationMarks(w http.ResponseWriter, r *http.Request) {
	ws, _ := collabtransport.WorkspaceFrom(r.Context())
	ids, ok := parseIDList(r.URL.Query().Get("generationIds"))
	if !ok {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "generationIds 非法"))
		return
	}
	marks, err := h.svc.GenerationMarks(r.Context(), ws.WorkspaceID, ids)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		m := marks[id]
		saved := make([]map[string]any, 0, len(m.SavedItems))
		for idx, itemID := range m.SavedItems {
			saved = append(saved, map[string]any{"outputIndex": idx, "itemId": itoa(itemID)})
		}
		items = append(items, map[string]any{"generationId": itoa(id), "rating": m.Rating, "saved": saved})
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) rateGeneration(w http.ResponseWriter, r *http.Request) {
	ws, _ := collabtransport.WorkspaceFrom(r.Context())
	id, ok := pathID(r, "generationID")
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.NotFound))
		return
	}
	var req struct {
		Rating int `json:"rating"`
	}
	if !decode(w, r, &req) {
		return
	}
	userID, _ := h.userIDs(r.Context())
	if err := h.svc.RateGeneration(r.Context(), ws.WorkspaceID, id, userID, req.Rating); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"generationId": itoa(id), "rating": req.Rating})
}
