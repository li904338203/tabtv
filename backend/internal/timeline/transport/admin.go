package transport

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	timelineapp "github.com/li904338203/tabtv/backend/internal/timeline/app"
	"github.com/li904338203/tabtv/backend/internal/timeline/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 管理端：渲染水印配置与成片追溯（P10-04）。

// MountAdmin 挂载管理端接口。
//
// ⚠️ 水印配置走 `system:write`**不走 `content:write`**：它影响每一条
// 成片的画面，与"审一件作品"完全不是一个量级的决定。读也单开
// `system:read` —— 与请求日志同一条：谁能看、谁能改，一律按权限位分。
func (h *AdminHandler) MountAdmin(r chi.Router, requireAdmin func(http.Handler) http.Handler,
	requirePerm func(string) func(http.Handler) http.Handler) {
	r.Route("/admin/render", func(r chi.Router) {
		r.Use(requireAdmin)
		r.With(requirePerm("system:read")).Get("/settings", h.getSetting)
		r.With(requirePerm("system:write")).Put("/settings", h.saveSetting)
		// 成片追溯：一次渲染用了哪条时间线、哪些素材、成片进了哪件作品。
		//
		// ⚠️ 走 `content:read`：这是**排障与审核**用的（"这个片子怎么不对"），
		// 与改水印配置不是一件事。
		r.With(requirePerm("content:read")).Get("/renders/{renderID}", h.trace)
	})
}

type AdminHandler struct {
	svc     *timelineapp.Service
	adminID func(*http.Request) int64
}

func NewAdminHandler(svc *timelineapp.Service,
	adminID func(*http.Request) int64) *AdminHandler {
	return &AdminHandler{svc: svc, adminID: adminID}
}

func (h *AdminHandler) getSetting(w http.ResponseWriter, r *http.Request) {
	set, err := h.svc.RenderSetting(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"watermarkMediaId": strconv.FormatInt(set.WatermarkMediaID, 10),
		"watermarkMissing": set.WatermarkMissing,
		"position":         set.Position,
		"opacity":          set.Opacity,
		"scale":            set.Scale,
		"version":          set.Version,
	})
}

func (h *AdminHandler) saveSetting(w http.ResponseWriter, r *http.Request) {
	var req struct {
		// ⚠️ 用**字符串**收 ID（对外一律 string，防 JS 精度丢失）；
		// 空串与 "0" 都表示"不配水印"。
		WatermarkMediaID string `json:"watermarkMediaId"`
		Position         string `json:"position"`
		Opacity          int    `json:"opacity"`
		Scale            int    `json:"scale"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Fail(w, r, httpx.Err(errcode.InvalidJSON))
		return
	}
	mediaID, _ := strconv.ParseInt(req.WatermarkMediaID, 10, 64)
	if err := h.svc.SaveRenderSetting(r.Context(), h.adminID(r), repo.RenderSetting{
		WatermarkMediaID: mediaID, Position: req.Position,
		Opacity: req.Opacity, Scale: req.Scale,
	}); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	h.getSetting(w, r)
}

func (h *AdminHandler) trace(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "renderID"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "renderID 非法"))
		return
	}
	tr, err := h.svc.TraceRender(r.Context(), id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	mediaIDs := make([]string, 0, len(tr.MediaIDs))
	for _, m := range tr.MediaIDs {
		mediaIDs = append(mediaIDs, strconv.FormatInt(m, 10))
	}
	body := map[string]any{
		"render": renderResp(tr.Job),
		// ⚠️ 快照**原样给**：它就是"当时渲的是什么"的唯一证据。
		// 重新组装一份可读结构会引入第二种解释，而排障恰恰需要原文。
		"snapshot": json.RawMessage(tr.Job.Snapshot),
		"mediaIds": mediaIDs,
	}
	if tr.Timeline.ID != 0 {
		// ⚠️ 同时给**当前**时间线：与快照的差别才说明问题
		// —— 用户很可能在渲完之后又改过。
		body["timeline"] = map[string]any{
			"id":      strconv.FormatInt(tr.Timeline.ID, 10),
			"name":    tr.Timeline.Name,
			"version": tr.Timeline.Version,
			"status":  tr.Timeline.Status,
		}
	}
	if tr.WorkID != 0 {
		body["workId"] = strconv.FormatInt(tr.WorkID, 10)
	}
	httpx.JSON(w, http.StatusOK, body)
}
