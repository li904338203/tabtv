package transport

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 管理端技能配置（AGT-60；AG-13）。
//
// ⚠️ 权限位 agent:read / agent:write，读写分开挂：只读管理员能进页面看全部配置，
// 写接口一律 403（前端禁用按钮只是体验，这里才是边界，CLAUDE.md §5.23）。
// ⚠️ 不受 SEEAITV_AGENT_ENABLED 总开关影响：运营要能在功能上线前把技能配好。

// AdminHandler 是技能配置的管理端入口。
type AdminHandler struct {
	svc      *app.SkillAdmin
	adminIDs func(r *http.Request) int64
}

// NewAdminHandler 构造。
func NewAdminHandler(svc *app.SkillAdmin, adminIDs func(r *http.Request) int64) *AdminHandler {
	return &AdminHandler{svc: svc, adminIDs: adminIDs}
}

// MountAdmin 挂载 /admin/agent/…。
func (h *AdminHandler) MountAdmin(r chi.Router, requireAdmin func(http.Handler) http.Handler,
	requirePerm func(string) func(http.Handler) http.Handler) {
	r.Route("/admin/agent", func(r chi.Router) {
		r.Use(requireAdmin)
		read := requirePerm("agent:read")
		write := requirePerm("agent:write")

		r.With(read).Get("/tools", h.tools)
		r.With(read).Get("/model-options", h.modelOptions)
		r.With(read).Get("/skills", h.list)
		r.With(write).Post("/skills", h.create)
		r.With(read).Get("/skills/{skillId}", h.get)
		r.With(write).Put("/skills/{skillId}", h.update)
		r.With(write).Post("/skills/{skillId}/enable", h.enable)
		r.With(write).Post("/skills/{skillId}/disable", h.disable)
		r.With(write).Post("/skills/{skillId}/restore-builtin", h.restoreBuiltin)
		r.With(read).Get("/skills/{skillId}/references", h.listRefs)
		r.With(write).Post("/skills/{skillId}/references", h.createRef)
		r.With(write).Put("/skills/{skillId}/references/{refId}", h.updateRef)
		r.With(write).Delete("/skills/{skillId}/references/{refId}", h.deleteRef)
	})
}

// ---------------------------------------------------------------- 视图

type skillBindingsResp struct {
	Video     string `json:"video"`
	Image     string `json:"image"`
	Reasoning string `json:"reasoning"`
}

type adminSkillResp struct {
	ID            string            `json:"id"`
	Code          string            `json:"code"`
	Name          string            `json:"name"`
	Category      string            `json:"category"`
	Description   string            `json:"description"`
	Tags          json.RawMessage   `json:"tags"`
	CoverMediaID  *string           `json:"coverMediaId"`
	Body          *string           `json:"body,omitempty"`
	Steps         json.RawMessage   `json:"steps"`
	Tools         json.RawMessage   `json:"tools"`
	ModelBindings skillBindingsResp `json:"modelBindings"`
	IsQuickEntry  bool              `json:"isQuickEntry"`
	Sort          int               `json:"sort"`
	IsEnabled     bool              `json:"isEnabled"`
	Version       int               `json:"version"`
	Scenes        json.RawMessage   `json:"scenes"`
	InputSlots    json.RawMessage   `json:"inputSlots"`
	PrefillText   string            `json:"prefillText"`
	IsBatch       bool              `json:"isBatch"`
	// IsBuiltin / IsUserModified：设置页的「内置 / 已修改」标识（TT-T-19）。
	IsBuiltin      bool               `json:"isBuiltin"`
	IsUserModified bool               `json:"isUserModified"`
	Warnings       []skillWarningDTO  `json:"warnings,omitempty"`
	RefCount       *int               `json:"refCount,omitempty"`
	References     []adminSkillRefDTO `json:"references,omitempty"`
	CreatedAt      string             `json:"createdAt"`
	UpdatedAt      string             `json:"updatedAt"`
}

// skillWarningDTO 是保存时不拦截的提示（缺口播提示句、合规风险用语，MKT-F-76）。
type skillWarningDTO struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func withWarnings(v adminSkillResp, ws []app.CopyIssue) adminSkillResp {
	v.Warnings = make([]skillWarningDTO, 0, len(ws))
	for _, w := range ws {
		v.Warnings = append(v.Warnings, skillWarningDTO(w))
	}
	return v
}

type adminSkillRefDTO struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Body      string `json:"body"`
	Sort      int    `json:"sort"`
	UpdatedAt string `json:"updatedAt"`
}

func toAdminSkill(s repo.Skill, withBody bool) adminSkillResp {
	var b domain.SkillBindings
	_ = json.Unmarshal(s.ModelBindings, &b)
	out := adminSkillResp{
		ID: idStr(s.ID), Code: s.Code, Name: s.Name, Category: s.Category, Description: s.Description,
		Tags: rawOr(s.Tags, "[]"), CoverMediaID: optID(s.CoverMediaID), Steps: rawOr(s.Steps, "[]"),
		Tools: rawOr(s.Tools, "[]"), ModelBindings: skillBindingsResp(b), IsQuickEntry: s.IsQuickEntry,
		Sort: s.Sort, IsEnabled: s.IsEnabled, Version: s.Version,
		Scenes: rawOr(s.Scenes, "[]"), InputSlots: rawOr(s.InputSlots, "[]"), PrefillText: s.PrefillText,
		IsBatch: s.IsBatch, IsBuiltin: s.IsBuiltin, IsUserModified: s.IsUserModified, CreatedAt: ts(s.CreatedAt), UpdatedAt: ts(s.UpdatedAt),
	}
	if withBody {
		body := s.Body
		out.Body = &body
	}
	return out
}

func toRefDTOs(refs []repo.SkillReference) []adminSkillRefDTO {
	out := make([]adminSkillRefDTO, 0, len(refs))
	for _, f := range refs {
		out = append(out, adminSkillRefDTO{ID: idStr(f.ID), Name: f.Name, Body: f.Body, Sort: f.Sort,
			UpdatedAt: ts(f.UpdatedAt)})
	}
	return out
}

// ---------------------------------------------------------------- 只读

func (h *AdminHandler) tools(w http.ResponseWriter, r *http.Request) {
	opts, err := h.svc.Options(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	type toolResp struct {
		Name      string `json:"name"`
		Paid      bool   `json:"paid"`
		Writes    bool   `json:"canvasWrite"`
		Action    string `json:"action"`
		Planning  bool   `json:"planning"`
		Executing bool   `json:"executing"`
	}
	items := make([]toolResp, 0, len(opts.Tools))
	for _, t := range opts.Tools {
		items = append(items, toolResp(t))
	}
	vars := opts.Vars
	if vars == nil {
		vars = []string{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items, "vars": vars})
}

func (h *AdminHandler) modelOptions(w http.ResponseWriter, r *http.Request) {
	opts, err := h.svc.Options(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	type modelResp struct {
		Code         string `json:"code"`
		Name         string `json:"name"`
		Domain       string `json:"domain"`
		Status       string `json:"status"`
		AgentCapable bool   `json:"agentCapable"`
	}
	items := make([]modelResp, 0, len(opts.Models))
	for _, m := range opts.Models {
		items = append(items, modelResp(m))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *AdminHandler) list(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.List(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]adminSkillResp, 0, len(rows))
	for _, s := range rows {
		v := toAdminSkill(s.Skill, false)
		n := s.RefCount
		v.RefCount = &n
		items = append(items, v)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *AdminHandler) get(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "skillId")
	if !ok {
		return
	}
	d, err := h.svc.Get(r.Context(), id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	v := toAdminSkill(d.Skill, true)
	v.References = toRefDTOs(d.Refs)
	n := len(d.Refs)
	v.RefCount = &n
	httpx.JSON(w, http.StatusOK, v)
}

func (h *AdminHandler) listRefs(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "skillId")
	if !ok {
		return
	}
	d, err := h.svc.Get(r.Context(), id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": toRefDTOs(d.Refs), "skillVersion": d.Skill.Version})
}

// ---------------------------------------------------------------- 写

// skillBody 是新建 / 修改共用的字段。新建多一个 code、修改多一个 version（各自的请求结构）。
type skillBody struct {
	Name          string               `json:"name"`
	Category      string               `json:"category"`
	Description   string               `json:"description"`
	Tags          []string             `json:"tags"`
	CoverMediaID  *string              `json:"coverMediaId"`
	Body          string               `json:"body"`
	Steps         []domain.SkillStep   `json:"steps"`
	Tools         []string             `json:"tools"`
	ModelBindings domain.SkillBindings `json:"modelBindings"`
	IsQuickEntry  bool                 `json:"isQuickEntry"`
	Sort          int                  `json:"sort"`
	// 场景扩展（MKT-T-05）：不带 = 修改时保留原值（见 app.SkillInput）。
	Scenes      []string        `json:"scenes"`
	InputSlots  json.RawMessage `json:"inputSlots"`
	PrefillText *string         `json:"prefillText"`
	// 批量标识（MKT-T-32）：不带 = 修改时保留原值。
	IsBatch *bool `json:"isBatch"`
}

func (b skillBody) input(w http.ResponseWriter, r *http.Request) (app.SkillInput, bool) {
	var cover int64
	if b.CoverMediaID != nil && strings.TrimSpace(*b.CoverMediaID) != "" {
		v, err := strconv.ParseInt(strings.TrimSpace(*b.CoverMediaID), 10, 64)
		if err != nil || v <= 0 {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "coverMediaId 非法"))
			return app.SkillInput{}, false
		}
		cover = v
	}
	return app.SkillInput{
		Name: b.Name, Category: b.Category, Description: b.Description, Tags: b.Tags,
		CoverMediaID: cover, Body: b.Body, Steps: b.Steps, Tools: b.Tools, Bindings: b.ModelBindings,
		IsQuickEntry: b.IsQuickEntry, Sort: b.Sort,
		Scenes: b.Scenes, InputSlots: b.InputSlots, PrefillText: b.PrefillText, IsBatch: b.IsBatch,
	}, true
}

func (h *AdminHandler) create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
		skillBody
	}
	if !decode(w, r, &req) {
		return
	}
	in, ok := req.skillBody.input(w, r)
	if !ok {
		return
	}
	in.Code = req.Code
	s, err := h.svc.Create(r.Context(), h.adminIDs(r), in)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, withWarnings(toAdminSkill(s.Skill, true), s.Warnings))
}

func (h *AdminHandler) update(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "skillId")
	if !ok {
		return
	}
	var req struct {
		Version int `json:"version"`
		skillBody
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Version <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "缺少 version（乐观锁）"))
		return
	}
	in, ok := req.skillBody.input(w, r)
	if !ok {
		return
	}
	s, err := h.svc.Update(r.Context(), h.adminIDs(r), id, req.Version, in)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, withWarnings(toAdminSkill(s.Skill, true), s.Warnings))
}

func (h *AdminHandler) enable(w http.ResponseWriter, r *http.Request)  { h.setEnabled(w, r, true) }
func (h *AdminHandler) disable(w http.ResponseWriter, r *http.Request) { h.setEnabled(w, r, false) }

func (h *AdminHandler) setEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	id, ok := pathID(w, r, "skillId")
	if !ok {
		return
	}
	s, err := h.svc.SetEnabled(r.Context(), h.adminIDs(r), id, enabled)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toAdminSkill(s, false))
}

// restoreBuiltin 恢复内置版本（TT-T-19）。回带正文的完整技能，编辑器直接替换。
func (h *AdminHandler) restoreBuiltin(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "skillId")
	if !ok {
		return
	}
	s, err := h.svc.RestoreBuiltin(r.Context(), h.adminIDs(r), id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toAdminSkill(s, true))
}

type refBody struct {
	SkillVersion int    `json:"skillVersion"`
	Name         string `json:"name"`
	Body         string `json:"body"`
	Sort         int    `json:"sort"`
}

func (h *AdminHandler) saveRef(w http.ResponseWriter, r *http.Request, refID int64) {
	skillID, ok := pathID(w, r, "skillId")
	if !ok {
		return
	}
	var req refBody
	if !decode(w, r, &req) {
		return
	}
	if req.SkillVersion <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "缺少 skillVersion（乐观锁）"))
		return
	}
	v, err := h.svc.SaveRef(r.Context(), h.adminIDs(r), skillID, refID, req.SkillVersion,
		app.SkillRefInput{Name: req.Name, Body: req.Body, Sort: req.Sort})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	status := http.StatusOK
	if refID == 0 {
		status = http.StatusCreated
	}
	httpx.JSON(w, status, map[string]any{"skillVersion": v})
}

func (h *AdminHandler) createRef(w http.ResponseWriter, r *http.Request) { h.saveRef(w, r, 0) }

func (h *AdminHandler) updateRef(w http.ResponseWriter, r *http.Request) {
	refID, ok := pathID(w, r, "refId")
	if !ok {
		return
	}
	h.saveRef(w, r, refID)
}

func (h *AdminHandler) deleteRef(w http.ResponseWriter, r *http.Request) {
	skillID, ok := pathID(w, r, "skillId")
	if !ok {
		return
	}
	refID, ok := pathID(w, r, "refId")
	if !ok {
		return
	}
	ver, err := strconv.Atoi(r.URL.Query().Get("skillVersion"))
	if err != nil || ver <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "缺少 skillVersion（乐观锁）"))
		return
	}
	v, err := h.svc.DeleteRef(r.Context(), h.adminIDs(r), skillID, refID, ver)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"skillVersion": v})
}
