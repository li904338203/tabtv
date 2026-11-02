package transport

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/li904338203/tabtv/backend/internal/catalog/app"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// MountPrompt 挂载提示词运营台（旧系统缺此页面，GAP-08）。
func (h *Handler) MountPrompt(r chi.Router, requireAdmin func(http.Handler) http.Handler,
	requirePerm func(string) func(http.Handler) http.Handler) {
	r.Route("/admin/prompts", func(r chi.Router) {
		r.Use(requireAdmin)
		read := requirePerm("prompt:read")
		write := requirePerm("prompt:write")

		r.With(read).Get("/specs", h.promptSpecs)
		r.With(read).Get("/", h.listPrompts)
		r.With(read).Get("/versions", h.promptVersions)
		r.With(write).Put("/draft", h.savePromptDraft)
		r.With(write).Post("/publish", h.publishPrompt)
		r.With(write).Post("/rollback", h.rollbackPrompt)
		r.With(write).Post("/archive", h.archivePrompt)
		r.With(read).Post("/preview", h.previewPrompt)
	})
}

// promptSpecs 下发用途契约，让管理端知道每个用途**有哪些占位符可用**。
// 没有它运营只能靠猜，猜错了会把字面量发给模型。
func (h *Handler) promptSpecs(w http.ResponseWriter, r *http.Request) {
	specs := h.svc.ListPromptSpecs()
	type resp struct {
		Code     string   `json:"code"`
		Name     string   `json:"name"`
		Required []string `json:"required"`
		Optional []string `json:"optional"`
		MaxChars int      `json:"maxChars"`
		// Builtin 代码内置的兜底原文：没有覆盖行时实际生效的就是它（设置页对照显示）。
		Builtin    string `json:"builtin"`
		HasBuiltin bool   `json:"hasBuiltin"`
	}
	out := make([]resp, 0, len(specs))
	for _, s := range specs {
		// ⚠️ 两个都要兜底成空数组：此前只兜了 optional，没有必需变量的用途回 `required: null`，
		// 管理端按数组展开时直接抛错，整个编辑表单渲染失败（P15-05 发现）。
		req, opt := s.Required, s.Optional
		if req == nil {
			req = []string{}
		}
		if opt == nil {
			opt = []string{}
		}
		fb, ok := h.svc.BuiltinPrompt(s.Code)
		out = append(out, resp{s.Code, s.Name, req, opt, s.MaxChars, fb, ok})
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) listPrompts(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.ListPrompts(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	type resp struct {
		Code      string `json:"code"`
		ModelCode string `json:"modelCode,omitempty"`
		Status    string `json:"status"`
		Version   int    `json:"version"`
		Content   string `json:"content"`
		Draft     string `json:"draftContent,omitempty"`
		// HasUnpublishedDraft 让管理端能提示"有未发布的修改"。
		HasUnpublishedDraft bool   `json:"hasUnpublishedDraft"`
		Note                string `json:"note,omitempty"`
	}
	out := make([]resp, 0, len(list))
	for _, p := range list {
		out = append(out, resp{
			Code: p.Code, ModelCode: p.ModelCode, Status: string(p.Status),
			Version: p.Version, Content: p.Content, Draft: p.DraftContent,
			HasUnpublishedDraft: p.DraftContent != "" && p.DraftContent != p.Content,
			Note:                p.Note,
		})
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) savePromptDraft(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code      string `json:"code"`
		ModelCode string `json:"modelCode"`
		Content   string `json:"content"`
		Note      string `json:"note"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := h.svc.SaveDraft(r.Context(), h.adminID(r), app.SaveDraftInput{
		Code: req.Code, ModelCode: req.ModelCode, Content: req.Content, Note: req.Note,
	}); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) publishPrompt(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code      string `json:"code"`
		ModelCode string `json:"modelCode"`
		Note      string `json:"note"`
	}
	if !decode(w, r, &req) {
		return
	}
	v, err := h.svc.PublishPrompt(r.Context(), h.adminID(r), req.Code, req.ModelCode, req.Note)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]int{"version": v})
}

func (h *Handler) rollbackPrompt(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code          string `json:"code"`
		ModelCode     string `json:"modelCode"`
		TargetVersion int    `json:"targetVersion"`
		Note          string `json:"note"`
	}
	if !decode(w, r, &req) {
		return
	}
	v, err := h.svc.RollbackPrompt(r.Context(), h.adminID(r),
		req.Code, req.ModelCode, req.TargetVersion, req.Note)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	// 回滚产生的是**新版本号**，不是回到旧号。
	httpx.JSON(w, http.StatusOK, map[string]int{"version": v, "rollbackOf": req.TargetVersion})
}

func (h *Handler) archivePrompt(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code      string `json:"code"`
		ModelCode string `json:"modelCode"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := h.svc.ArchivePrompt(r.Context(), h.adminID(r), req.Code, req.ModelCode); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) promptVersions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := h.svc.PromptVersions(r.Context(), q.Get("code"), q.Get("modelCode"), limit)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	// 显式 DTO：仓储结构体直接序列化会把字段名原样暴露成 PascalCase，
	// 且以后加一列就悄悄出现在对外契约里。
	type resp struct {
		Version           int    `json:"version"`
		Content           string `json:"content"`
		Note              string `json:"note,omitempty"`
		PublishedBy       string `json:"publishedByAdminId,omitempty"`
		PublishedAt       string `json:"publishedAt"`
		RollbackOfVersion *int   `json:"rollbackOfVersion,omitempty"`
	}
	out := make([]resp, 0, len(list))
	for _, v := range list {
		var by string
		if v.PublishedBy != 0 {
			// ID 对外序列化为 string，防 JS 精度丢失。
			by = strconv.FormatInt(v.PublishedBy, 10)
		}
		out = append(out, resp{v.Version, v.Content, v.Note, by, v.PublishedAt, v.RollbackOfVersion})
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) previewPrompt(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code      string            `json:"code"`
		ModelCode string            `json:"modelCode"`
		Content   string            `json:"content"`
		Vars      map[string]string `json:"vars"`
	}
	if !decode(w, r, &req) {
		return
	}
	out, err := h.svc.PreviewPrompt(r.Context(), req.Code, req.ModelCode, req.Content, req.Vars)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"rendered": out})
}
