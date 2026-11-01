package transport

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/marketing/app"
	"github.com/li904338203/tabtv/backend/internal/marketing/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 管理端营销角色（平台预置，MKT-T-36；dev/05 §3.2，7 个端点）。
//
// ⚠️ 管理端 DTO 与用户端（MKT-T-34 的预置角色列表）**分开定义**：管理端要媒体 ID 与平台媒体地址，
// 用户端**不下发媒体 ID**（公开取图走按下标的公开端点）。共用一个结构体的话，总有一天会有人
// 往里加一个字段，而它同时出现在公开接口上（dev/02 §4.4 同一条）。

type adminPresetResp struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Summary       string         `json:"summary"`
	ImageMediaIDs []string       `json:"imageMediaIds"`
	ImageURLs     []string       `json:"imageUrls"`
	Appearance    string         `json:"appearance"`
	VoiceText     string         `json:"voiceText"`
	VoiceMediaID  *string        `json:"voiceMediaId"`
	VoiceURL      *string        `json:"voiceUrl"`
	Sort          int            `json:"sort"`
	IsEnabled     bool           `json:"isEnabled"`
	Version       int            `json:"version"`
	Warnings      []domain.Issue `json:"warnings"`
	CreatedAt     string         `json:"createdAt"`
	UpdatedAt     string         `json:"updatedAt"`
}

// adminMediaURL 是平台素材的管理端读取地址（`GET /admin/media/{id}`，要带 admin token fetch 成 blob）。
func adminMediaURL(id int64) string { return "/api/v1/admin/media/" + idStr(id) }

func toAdminPresetResp(v app.AdminPresetView) adminPresetResp {
	p := v.AdminPreset
	ids := make([]string, 0, len(p.ImageMediaIDs))
	urls := make([]string, 0, len(p.ImageMediaIDs))
	for _, id := range p.ImageMediaIDs {
		ids = append(ids, idStr(id))
		urls = append(urls, adminMediaURL(id))
	}
	var voiceID, voiceURL *string
	if p.VoiceMediaID != nil {
		s, u := idStr(*p.VoiceMediaID), adminMediaURL(*p.VoiceMediaID)
		voiceID, voiceURL = &s, &u
	}
	warnings := v.Warnings
	if warnings == nil {
		warnings = []domain.Issue{}
	}
	return adminPresetResp{ID: idStr(p.ID), Name: p.Name, Summary: p.Summary, ImageMediaIDs: ids, ImageURLs: urls,
		Appearance: p.Appearance, VoiceText: p.VoiceText, VoiceMediaID: voiceID, VoiceURL: voiceURL, Sort: p.Sort,
		IsEnabled: p.IsEnabled, Version: p.Version, Warnings: warnings, CreatedAt: ts(p.CreatedAt),
		UpdatedAt: ts(p.UpdatedAt)}
}

type adminPresetBody struct {
	Name          string   `json:"name"`
	Summary       string   `json:"summary"`
	ImageMediaIDs []string `json:"imageMediaIds"`
	Appearance    string   `json:"appearance"`
	VoiceText     string   `json:"voiceText"`
	VoiceMediaID  *string  `json:"voiceMediaId"`
}

// input 解析 ID；格式问题一次报完（details.issues），与服务层的校验同一形状。
func (b adminPresetBody) input(w http.ResponseWriter, r *http.Request) (domain.PresetInput, bool) {
	var issues []domain.Issue
	bad := func(field, msg string) {
		issues = append(issues, domain.Issue{Field: field, Code: domain.IssueBadFormat, Message: msg,
			Level: domain.LevelError})
	}
	in := domain.PresetInput{Name: b.Name, Summary: b.Summary, Appearance: b.Appearance, VoiceText: b.VoiceText,
		ImageMediaIDs: make([]int64, 0, len(b.ImageMediaIDs))}
	for i, s := range b.ImageMediaIDs {
		v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil || v <= 0 {
			bad("imageMediaIds["+strconv.Itoa(i)+"]", "图片媒体 ID 非法")
			continue
		}
		in.ImageMediaIDs = append(in.ImageMediaIDs, v)
	}
	if b.VoiceMediaID != nil && strings.TrimSpace(*b.VoiceMediaID) != "" {
		v, err := strconv.ParseInt(strings.TrimSpace(*b.VoiceMediaID), 10, 64)
		if err != nil || v <= 0 {
			bad("voiceMediaId", "参考音频媒体 ID 非法")
		} else {
			in.VoiceMediaID = &v
		}
	}
	if len(issues) > 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "请求字段格式有误："+issues[0].Message).
			WithDetails(map[string]any{"issues": issues, "warnings": []domain.Issue{}}))
		return domain.PresetInput{}, false
	}
	return in, true
}

func (h *AdminHandler) presetsReady(w http.ResponseWriter, r *http.Request) bool {
	if h.presets == nil {
		httpx.Fail(w, r, httpx.Errf(errcode.Internal, "服务端未接入营销角色管理"))
		return false
	}
	return true
}

func (h *AdminHandler) listPresets(w http.ResponseWriter, r *http.Request) {
	if !h.presetsReady(w, r) {
		return
	}
	q := r.URL.Query()
	f := app.AdminPresetFilter{Q: q.Get("q"), Cursor: q.Get("cursor")}
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
	page, err := h.presets.List(r.Context(), f)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]adminPresetResp, 0, len(page.Items))
	for _, v := range page.Items {
		items = append(items, toAdminPresetResp(v))
	}
	var next *string
	if page.NextCursor != "" {
		next = &page.NextCursor
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": next})
}

func (h *AdminHandler) getPreset(w http.ResponseWriter, r *http.Request) {
	if !h.presetsReady(w, r) {
		return
	}
	id, ok := pathID(w, r, "presetId")
	if !ok {
		return
	}
	v, err := h.presets.Get(r.Context(), id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toAdminPresetResp(v))
}

func (h *AdminHandler) createPreset(w http.ResponseWriter, r *http.Request) {
	if !h.presetsReady(w, r) {
		return
	}
	var req adminPresetBody
	if !decode(w, r, &req) {
		return
	}
	in, ok := req.input(w, r)
	if !ok {
		return
	}
	v, err := h.presets.Create(r.Context(), h.adminIDs(r), in)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, toAdminPresetResp(v))
}

func (h *AdminHandler) updatePreset(w http.ResponseWriter, r *http.Request) {
	if !h.presetsReady(w, r) {
		return
	}
	id, ok := pathID(w, r, "presetId")
	if !ok {
		return
	}
	var req struct {
		Version int `json:"version"`
		adminPresetBody
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Version <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "缺少 version（乐观锁）"))
		return
	}
	in, ok := req.adminPresetBody.input(w, r)
	if !ok {
		return
	}
	v, err := h.presets.Update(r.Context(), h.adminIDs(r), id, req.Version, in)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toAdminPresetResp(v))
}

func (h *AdminHandler) enablePreset(w http.ResponseWriter, r *http.Request) {
	h.setPresetEnabled(w, r, true)
}
func (h *AdminHandler) disablePreset(w http.ResponseWriter, r *http.Request) {
	h.setPresetEnabled(w, r, false)
}

func (h *AdminHandler) setPresetEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	if !h.presetsReady(w, r) {
		return
	}
	id, ok := pathID(w, r, "presetId")
	if !ok {
		return
	}
	var v app.AdminPresetView
	var err error
	if enabled {
		v, err = h.presets.Enable(r.Context(), h.adminIDs(r), id)
	} else {
		v, err = h.presets.Disable(r.Context(), h.adminIDs(r), id)
	}
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toAdminPresetResp(v))
}

func (h *AdminHandler) sortPresets(w http.ResponseWriter, r *http.Request) {
	if !h.presetsReady(w, r) {
		return
	}
	var req struct {
		IDs []string `json:"ids"`
	}
	if !decode(w, r, &req) {
		return
	}
	ids, ok := parseIDs(w, r, req.IDs)
	if !ok {
		return
	}
	if err := h.presets.Sort(r.Context(), h.adminIDs(r), ids); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
