package transport

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/li904338203/tabtv/backend/internal/catalog/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// MountPresetAdmin 挂载图片预设的管理端（TT-T-20）：列表（含已停用）、新建、更新、启停、恢复内置版本。
// 权限位与画布配置同一组（canvas:read / canvas:write）。
func (h *Handler) MountPresetAdmin(r chi.Router, requireAdmin func(http.Handler) http.Handler,
	requirePerm func(string) func(http.Handler) http.Handler) {
	r.Route("/admin/image-presets", func(r chi.Router) {
		r.Use(requireAdmin)
		read, write := requirePerm("canvas:read"), requirePerm("canvas:write")
		r.With(read).Get("/", h.listPresetsAdmin)
		r.With(write).Post("/", h.createPreset)
		r.With(read).Post("/preview", h.previewPreset)
		r.With(write).Put("/{code}", h.updatePreset)
		r.With(write).Post("/{code}/enable", h.enablePreset(true))
		r.With(write).Post("/{code}/disable", h.enablePreset(false))
		r.With(write).Post("/{code}/restore-builtin", h.restorePreset)
	})
}

func (h *Handler) listPresetsAdmin(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.AdminListPresets(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": list})
}

func (h *Handler) createPreset(w http.ResponseWriter, r *http.Request) {
	var p repo.AdminImagePreset
	if !decode(w, r, &p) {
		return
	}
	if err := h.svc.SavePreset(r.Context(), h.adminID(r), p, true); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	h.writePreset(w, r, p.Code, http.StatusCreated)
}

func (h *Handler) updatePreset(w http.ResponseWriter, r *http.Request) {
	var p repo.AdminImagePreset
	if !decode(w, r, &p) {
		return
	}
	// 编码以路径为准：请求体里的 code 被忽略，改不了编码（历史节点按 code 找配方）。
	p.Code = chi.URLParam(r, "code")
	if err := h.svc.SavePreset(r.Context(), h.adminID(r), p, false); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	h.writePreset(w, r, p.Code, http.StatusOK)
}

func (h *Handler) enablePreset(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		code := chi.URLParam(r, "code")
		if err := h.svc.SetPresetEnabled(r.Context(), h.adminID(r), code, enabled); err != nil {
			httpx.Fail(w, r, err)
			return
		}
		h.writePreset(w, r, code, http.StatusOK)
	}
}

func (h *Handler) restorePreset(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")
	if err := h.svc.RestorePreset(r.Context(), h.adminID(r), code); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	h.writePreset(w, r, code, http.StatusOK)
}

// previewPreset 只读：按给定的配方与维度拼出追加的提示词（不落库）。
func (h *Handler) previewPreset(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Prompt     string            `json:"prompt"`
		Dimensions json.RawMessage   `json:"dimensions"`
		Selected   map[string]string `json:"selected"`
	}
	if !decode(w, r, &req) {
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"rendered": h.svc.PreviewPreset(req.Prompt, req.Dimensions, req.Selected)})
}

func (h *Handler) writePreset(w http.ResponseWriter, r *http.Request, code string, status int) {
	p, err := h.svc.AdminGetPreset(r.Context(), code)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, status, p)
}
