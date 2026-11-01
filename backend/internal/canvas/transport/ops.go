package transport

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/li904338203/tabtv/backend/internal/canvas/app"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// opBatchResp 是一批画布操作的对外形状（与 canvas.ops_applied 事件里的字段同名）。
type opBatchResp struct {
	BatchID     string          `json:"batchId"`
	FromVersion int             `json:"fromVersion"`
	ToVersion   int             `json:"toVersion"`
	Actor       app.OpActor     `json:"actor"`
	Ops         json.RawMessage `json:"ops"`
	CreatedAt   string          `json:"createdAt"`
}

// canvasOps 补取 sinceVersion 之后的操作批次（AGT-10）。
//
// ⚠️ 只读、view 即可；**不回画布内容**（几百 KB 到几 MB），补不全时回 needsReload=true，
// 由前端自己去 GET /projects/{id} 整页重载。
func (h *Handler) canvasOps(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := h.ctx(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "projectId")
	if !ok {
		return
	}
	since, err := strconv.Atoi(r.URL.Query().Get("sinceVersion"))
	if err != nil || since < 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "sinceVersion 必填且为非负整数"))
		return
	}
	res, err := h.svc.OpsSince(r.Context(), ws.WorkspaceID, id, ws.Role, since)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	batches := make([]opBatchResp, 0, len(res.Batches))
	for _, b := range res.Batches {
		batches = append(batches, opBatchResp{
			BatchID: b.BatchID, FromVersion: b.FromVersion, ToVersion: b.ToVersion,
			Actor: app.NewOpActor(b), Ops: b.Ops,
			CreatedAt: b.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"currentVersion": res.CurrentVersion,
		"needsReload":    res.NeedsReload,
		"batches":        batches,
	})
}

// optionalIDString 把可选 ID 转成对外的 string；没有时给空串（与 lastEditorId 同一约定）。
func optionalIDString(v int64) string {
	if v == 0 {
		return ""
	}
	return strconv.FormatInt(v, 10)
}
