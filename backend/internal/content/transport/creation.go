package transport

import (
	"net/http"
	"strconv"
	"time"

	collabtransport "github.com/li904338203/tabtv/backend/internal/collaboration/transport"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// creations 「我的作品」：工作区里成功的生成（老系统「作品」的语义）。
//
// 只收图片、视频、音频（repo.CreationTypes），文本不在「创作资产」里。
// ⚠️ 给的是**媒体 ID 与受保护端点地址**，不是对象存储直链（CD-16）：
// `<img src>` 不带鉴权头，前端要 fetch 成 blob（且要带 X-Workspace-Id）。
func (h *Handler) creations(w http.ResponseWriter, r *http.Request) {
	ws, _ := collabtransport.WorkspaceFrom(r.Context())
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	var cursor int64
	if v := q.Get("cursor"); v != "" {
		cursor, _ = strconv.ParseInt(v, 10, 64)
	}
	projectID, _ := strconv.ParseInt(q.Get("projectId"), 10, 64)
	page, err := h.svc.Creations(r.Context(), ws.WorkspaceID, q.Get("resultType"), limit, cursor, projectID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(page.Items))
	for _, c := range page.Items {
		var cover map[string]any
		if c.CoverType != "" && c.CoverMedia != 0 {
			id := itoa(c.CoverMedia)
			cover = map[string]any{"type": c.CoverType, "mediaId": id, "url": "/api/v1/media/" + id}
		}
		items = append(items, map[string]any{
			"id": itoa(c.ID), "projectId": itoa(c.ProjectID),
			// 空串 = 项目已被彻底删除（产出仍然保留，是用户花钱得来的）
			"projectName": c.ProjectName, "nodeId": c.NodeID,
			"resultType": c.ResultType, "modelCode": c.ModelCode, "modelName": c.ModelName,
			"outputCount": c.OutputCount, "cover": cover,
			"createdAt": c.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	var next *string
	if page.HasMore && len(page.Items) > 0 {
		s := itoa(page.Items[len(page.Items)-1].ID)
		next = &s
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"items": items, "nextCursor": next, "hasMore": page.HasMore, "total": page.Total,
	})
}
