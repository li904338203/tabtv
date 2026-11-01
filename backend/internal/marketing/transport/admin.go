// Package transport 是营销域的 HTTP 入口。
package transport

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/li904338203/tabtv/backend/internal/marketing/app"
	"github.com/li904338203/tabtv/backend/internal/marketing/domain"
	"github.com/li904338203/tabtv/backend/internal/marketing/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 管理端营销选项与分类（MKT-F-70~77；dev/02 §4.2，12 个端点）。
//
// ⚠️ 权限沿用 agent:read / agent:write（MKT-F-74、MKD-20，不新造权限位），读写分开挂：
// 只读管理员能进页面看全部配置，写接口一律 403（前端禁用按钮只是体验，这里才是边界）。
// ⚠️ 不受 SEEAITV_MARKETING_ENABLED 影响：运营要能在功能上线前把选项配好（同技能管理）。
// ⚠️ 没有 DELETE：历史快照按 code 引用，选项与分类都只停用。

const maxBody = 1 << 20

// AdminHandler 是管理端入口。
type AdminHandler struct {
	svc      *app.Admin
	adminIDs func(r *http.Request) int64
	// presets 是营销角色（平台预置）管理（MKT-T-36）；未注入时那 7 个端点回 500（装配漏了，不是 404）。
	presets *app.PresetAdmin
}

// SetPresets 注入营销角色管理服务（MKT-T-36）。
func (h *AdminHandler) SetPresets(p *app.PresetAdmin) { h.presets = p }

// NewAdminHandler 构造。
func NewAdminHandler(svc *app.Admin, adminIDs func(r *http.Request) int64) *AdminHandler {
	return &AdminHandler{svc: svc, adminIDs: adminIDs}
}

// MountAdmin 挂载 /admin/marketing/…。
func (h *AdminHandler) MountAdmin(r chi.Router, requireAdmin func(http.Handler) http.Handler,
	requirePerm func(string) func(http.Handler) http.Handler) {
	r.Route("/admin/marketing", func(r chi.Router) {
		r.Use(requireAdmin)
		read := requirePerm("agent:read")
		write := requirePerm("agent:write")

		r.With(read).Get("/options", h.list)
		r.With(write).Post("/options", h.create)
		// 静态段 sort 与 {optionId} 同级：chi 静态段优先匹配，「sort」不会被当成 ID。
		r.With(write).Put("/options/sort", h.sort)
		r.With(read).Get("/options/{optionId}", h.get)
		r.With(write).Put("/options/{optionId}", h.update)
		r.With(write).Post("/options/{optionId}/enable", h.enable)
		r.With(write).Post("/options/{optionId}/disable", h.disable)
		r.With(read).Get("/options/{optionId}/versions", h.versions)
		r.With(read).Get("/categories", h.categories)
		r.With(write).Post("/categories", h.createCategory)
		r.With(write).Put("/categories/sort", h.sortCategories)
		r.With(write).Put("/categories/{categoryId}", h.updateCategory)

		// 营销角色（平台预置，MKT-T-36）：同一对权限位；没有 DELETE（MKT-F-120，只停用）。
		r.With(read).Get("/preset-characters", h.listPresets)
		r.With(write).Post("/preset-characters", h.createPreset)
		r.With(write).Put("/preset-characters/sort", h.sortPresets)
		r.With(read).Get("/preset-characters/{presetId}", h.getPreset)
		r.With(write).Put("/preset-characters/{presetId}", h.updatePreset)
		r.With(write).Post("/preset-characters/{presetId}/enable", h.enablePreset)
		r.With(write).Post("/preset-characters/{presetId}/disable", h.disablePreset)
	})
}

// ---------------------------------------------------------------- 视图

type optionResp struct {
	ID                string            `json:"id"`
	Kind              string            `json:"kind"`
	Code              string            `json:"code"`
	Name              string            `json:"name"`
	CategoryCode      string            `json:"categoryCode"`
	Description       string            `json:"description"`
	CoverMediaIDs     []string          `json:"coverMediaIds"`
	PreviewMediaID    *string           `json:"previewMediaId"`
	Prompt            string            `json:"prompt"`
	OutputTypes       []string          `json:"outputTypes"`
	InputRequirements map[string]string `json:"inputRequirements"`
	IsAudioRequired   bool              `json:"isAudioRequired"`
	RecommendedModels []string          `json:"recommendedModels"`
	RecommendedParams json.RawMessage   `json:"recommendedParams"`
	DefaultSkillCode  *string           `json:"defaultSkillCode"`
	Tags              []string          `json:"tags"`
	StartAt           *string           `json:"startAt"`
	EndAt             *string           `json:"endAt"`
	Sort              int               `json:"sort"`
	IsEnabled         bool              `json:"isEnabled"`
	Version           int               `json:"version"`
	Lifecycle         string            `json:"lifecycle"`
	Warnings          []domain.Issue    `json:"warnings"`
	CreatedAt         string            `json:"createdAt"`
	UpdatedAt         string            `json:"updatedAt"`
}

func toOptionResp(v app.OptionView) optionResp {
	o := v.Option
	covers := make([]string, 0, len(o.CoverMediaIDs))
	for _, id := range o.CoverMediaIDs {
		covers = append(covers, idStr(id))
	}
	var preview *string
	if o.PreviewMediaID != nil {
		s := idStr(*o.PreviewMediaID)
		preview = &s
	}
	var skill *string
	if o.DefaultSkillCode != "" {
		s := o.DefaultSkillCode
		skill = &s
	}
	warnings := v.Warnings
	if warnings == nil {
		warnings = []domain.Issue{}
	}
	return optionResp{
		ID: idStr(o.ID), Kind: string(o.Kind), Code: o.Code, Name: o.Name, CategoryCode: o.CategoryCode,
		Description: o.Description, CoverMediaIDs: covers, PreviewMediaID: preview, Prompt: o.Prompt,
		OutputTypes: nonNilStrings(o.OutputTypes), InputRequirements: nonNilMap(o.InputRequirements),
		IsAudioRequired: o.IsAudioRequired, RecommendedModels: nonNilStrings(o.RecommendedModels),
		RecommendedParams: rawOr(o.RecommendedParams, "{}"), DefaultSkillCode: skill,
		Tags: nonNilStrings(o.Tags), StartAt: optTS(o.StartAt), EndAt: optTS(o.EndAt), Sort: o.Sort,
		IsEnabled: o.IsEnabled, Version: o.Version, Lifecycle: string(v.Lifecycle), Warnings: warnings,
		CreatedAt: ts(o.CreatedAt), UpdatedAt: ts(o.UpdatedAt),
	}
}

type categoryResp struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Code      string `json:"code"`
	Name      string `json:"name"`
	Sort      int    `json:"sort"`
	IsEnabled bool   `json:"isEnabled"`
}

func toCategoryResp(c repo.Category) categoryResp {
	return categoryResp{ID: idStr(c.ID), Kind: string(c.Kind), Code: c.Code, Name: c.Name, Sort: c.Sort,
		IsEnabled: c.IsEnabled}
}

// ---------------------------------------------------------------- 选项

func (h *AdminHandler) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := app.ListFilter{Kind: domain.Kind(q.Get("kind")), Category: q.Get("category"),
		Lifecycle: domain.LifecycleState(q.Get("lifecycle")), Q: q.Get("q"), Cursor: q.Get("cursor")}
	if s := q.Get("enabled"); s != "" {
		b, err := strconv.ParseBool(s)
		if err != nil {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "enabled 只能是 true / false"))
			return
		}
		f.Enabled = &b
	}
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "limit 非法"))
			return
		}
		f.Limit = n
	}
	page, err := h.svc.List(r.Context(), f)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]optionResp, 0, len(page.Items))
	for _, v := range page.Items {
		items = append(items, toOptionResp(v))
	}
	var next *string
	if page.NextCursor != "" {
		next = &page.NextCursor
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": next})
}

func (h *AdminHandler) get(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "optionId")
	if !ok {
		return
	}
	v, err := h.svc.Get(r.Context(), id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toOptionResp(v))
}

// optionBody 是新建 / 修改共用的字段。新建与修改都允许带 kind / code（修改时与原值不同即拒）。
type optionBody struct {
	Kind              string            `json:"kind"`
	Code              string            `json:"code"`
	Name              string            `json:"name"`
	CategoryCode      string            `json:"categoryCode"`
	Description       string            `json:"description"`
	CoverMediaIDs     []string          `json:"coverMediaIds"`
	PreviewMediaID    *string           `json:"previewMediaId"`
	Prompt            string            `json:"prompt"`
	OutputTypes       []string          `json:"outputTypes"`
	InputRequirements map[string]string `json:"inputRequirements"`
	IsAudioRequired   bool              `json:"isAudioRequired"`
	RecommendedModels []string          `json:"recommendedModels"`
	RecommendedParams json.RawMessage   `json:"recommendedParams"`
	DefaultSkillCode  *string           `json:"defaultSkillCode"`
	Tags              []string          `json:"tags"`
	StartAt           *string           `json:"startAt"`
	EndAt             *string           `json:"endAt"`
	Sort              int               `json:"sort"`
}

// input 解析 ID 与时间；格式问题一次报完（details.issues），与服务层的校验同一形状。
func (b optionBody) input(w http.ResponseWriter, r *http.Request) (app.OptionInput, bool) {
	var issues []domain.Issue
	bad := func(field, msg string) {
		issues = append(issues, domain.Issue{Field: field, Code: domain.IssueBadFormat, Message: msg,
			Level: domain.LevelError})
	}
	in := app.OptionInput{Kind: domain.Kind(strings.TrimSpace(b.Kind)), Code: b.Code, Name: b.Name,
		CategoryCode: b.CategoryCode, Description: b.Description, Prompt: b.Prompt, OutputTypes: b.OutputTypes,
		InputRequirements: b.InputRequirements, IsAudioRequired: b.IsAudioRequired,
		RecommendedModels: b.RecommendedModels, RecommendedParams: b.RecommendedParams, Tags: b.Tags,
		Sort: b.Sort}
	for i, s := range b.CoverMediaIDs {
		v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil || v <= 0 {
			bad("coverMediaIds["+strconv.Itoa(i)+"]", "封面媒体 ID 非法")
			continue
		}
		in.CoverMediaIDs = append(in.CoverMediaIDs, v)
	}
	if b.PreviewMediaID != nil && strings.TrimSpace(*b.PreviewMediaID) != "" {
		v, err := strconv.ParseInt(strings.TrimSpace(*b.PreviewMediaID), 10, 64)
		if err != nil || v <= 0 {
			bad("previewMediaId", "示例视频媒体 ID 非法")
		} else {
			in.PreviewMediaID = &v
		}
	}
	if b.DefaultSkillCode != nil {
		in.DefaultSkillCode = *b.DefaultSkillCode
	}
	parse := func(field string, s *string) *time.Time {
		if s == nil || strings.TrimSpace(*s) == "" {
			return nil
		}
		// ⚠️ 必须带时区（RFC3339）：管理端按北京时间录入、提交 +08:00（MK-18）；
		// 不带时区的话服务端只能猜，猜错就整整偏 8 小时。
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(*s))
		if err != nil {
			bad(field, "时间须为带时区的 RFC3339（如 2026-11-01T00:00:00+08:00）")
			return nil
		}
		return &t
	}
	in.StartAt = parse("startAt", b.StartAt)
	in.EndAt = parse("endAt", b.EndAt)
	if len(issues) > 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "请求字段格式有误："+issues[0].Message).
			WithDetails(map[string]any{"issues": issues, "warnings": []domain.Issue{}}))
		return app.OptionInput{}, false
	}
	return in, true
}

func (h *AdminHandler) create(w http.ResponseWriter, r *http.Request) {
	var req optionBody
	if !decode(w, r, &req) {
		return
	}
	in, ok := req.input(w, r)
	if !ok {
		return
	}
	v, err := h.svc.Create(r.Context(), h.adminIDs(r), in)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, toOptionResp(v))
}

func (h *AdminHandler) update(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "optionId")
	if !ok {
		return
	}
	var req struct {
		Version int `json:"version"`
		optionBody
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Version <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "缺少 version（乐观锁）"))
		return
	}
	in, ok := req.optionBody.input(w, r)
	if !ok {
		return
	}
	v, err := h.svc.Update(r.Context(), h.adminIDs(r), id, req.Version, in)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toOptionResp(v))
}

func (h *AdminHandler) enable(w http.ResponseWriter, r *http.Request)  { h.setEnabled(w, r, true) }
func (h *AdminHandler) disable(w http.ResponseWriter, r *http.Request) { h.setEnabled(w, r, false) }

func (h *AdminHandler) setEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	id, ok := pathID(w, r, "optionId")
	if !ok {
		return
	}
	var v app.OptionView
	var err error
	if enabled {
		v, err = h.svc.Enable(r.Context(), h.adminIDs(r), id)
	} else {
		v, err = h.svc.Disable(r.Context(), h.adminIDs(r), id)
	}
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toOptionResp(v))
}

func (h *AdminHandler) sort(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind     string   `json:"kind"`
		Category string   `json:"category"`
		IDs      []string `json:"ids"`
	}
	if !decode(w, r, &req) {
		return
	}
	ids, ok := parseIDs(w, r, req.IDs)
	if !ok {
		return
	}
	if err := h.svc.Sort(r.Context(), h.adminIDs(r), domain.Kind(req.Kind), req.Category, ids); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *AdminHandler) versions(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "optionId")
	if !ok {
		return
	}
	vs, err := h.svc.Versions(r.Context(), id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	type versionResp struct {
		ID               string          `json:"id"`
		Version          int             `json:"version"`
		Snapshot         json.RawMessage `json:"snapshot"`
		CreatedByAdminID *string         `json:"createdByAdminId"`
		CreatedAt        string          `json:"createdAt"`
	}
	items := make([]versionResp, 0, len(vs))
	for _, v := range vs {
		var by *string
		if v.CreatedByAdminID != 0 {
			s := idStr(v.CreatedByAdminID)
			by = &s
		}
		items = append(items, versionResp{ID: idStr(v.ID), Version: v.Version,
			Snapshot: rawOr(v.Snapshot, "{}"), CreatedByAdminID: by, CreatedAt: ts(v.CreatedAt)})
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
}

// ---------------------------------------------------------------- 分类

func (h *AdminHandler) categories(w http.ResponseWriter, r *http.Request) {
	cs, err := h.svc.Categories(r.Context(), domain.Kind(r.URL.Query().Get("kind")))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]categoryResp, 0, len(cs))
	for _, c := range cs {
		items = append(items, toCategoryResp(c))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
}

type categoryBody struct {
	Kind      string `json:"kind"`
	Code      string `json:"code"`
	Name      string `json:"name"`
	Sort      int    `json:"sort"`
	IsEnabled *bool  `json:"isEnabled"`
}

func (b categoryBody) input() app.CategoryInput {
	return app.CategoryInput{Kind: domain.Kind(strings.TrimSpace(b.Kind)), Code: b.Code, Name: b.Name,
		Sort: b.Sort, IsEnabled: b.IsEnabled}
}

func (h *AdminHandler) createCategory(w http.ResponseWriter, r *http.Request) {
	var req categoryBody
	if !decode(w, r, &req) {
		return
	}
	c, err := h.svc.CreateCategory(r.Context(), h.adminIDs(r), req.input())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, toCategoryResp(c))
}

func (h *AdminHandler) updateCategory(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "categoryId")
	if !ok {
		return
	}
	var req categoryBody
	if !decode(w, r, &req) {
		return
	}
	c, err := h.svc.UpdateCategory(r.Context(), h.adminIDs(r), id, req.input())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toCategoryResp(c))
}

func (h *AdminHandler) sortCategories(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind string   `json:"kind"`
		IDs  []string `json:"ids"`
	}
	if !decode(w, r, &req) {
		return
	}
	ids, ok := parseIDs(w, r, req.IDs)
	if !ok {
		return
	}
	if err := h.svc.SortCategories(r.Context(), h.adminIDs(r), domain.Kind(req.Kind), ids); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------- 工具

func pathID(w http.ResponseWriter, r *http.Request, key string) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, key), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, key+" 非法"))
		return 0, false
	}
	return id, true
}

func parseIDs(w http.ResponseWriter, r *http.Request, in []string) ([]int64, bool) {
	out := make([]int64, 0, len(in))
	for _, s := range in {
		v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil || v <= 0 {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "ids 里有非法 ID「"+s+"」"))
			return nil, false
		}
		out = append(out, v)
	}
	return out, true
}

// decode 严格解码请求体：未知字段直接拒（键名写错时不能当成「没传」，§5.10）。
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidJSON, "请求体不是合法 JSON 或含未知字段："+err.Error()))
		return false
	}
	return true
}

func idStr(v int64) string { return strconv.FormatInt(v, 10) }

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func optTS(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := ts(*t)
	return &s
}

func rawOr(v json.RawMessage, def string) json.RawMessage {
	if len(v) == 0 {
		return json.RawMessage(def)
	}
	return v
}

func nonNilStrings(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
