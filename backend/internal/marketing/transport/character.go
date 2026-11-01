package transport

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/li904338203/tabtv/backend/internal/marketing/app"
	"github.com/li904338203/tabtv/backend/internal/marketing/domain"
	"github.com/li904338203/tabtv/backend/internal/marketing/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 角色库与营销角色用户端接口（MKT-T-34；dev/05 §3.1 角色 5 个 + 营销角色 4 个）。
//
// 工作区角色 5 个 + adopt 挂在 MountLibrary 的分组里（要登录 + 工作区闸：GET → view，其余 → edit）；
// 营销角色的列表 / 图片 / 音频挂在 MountPublic（**公开**、受营销总开关，同选项封面）。
// ⚠️ 公开的营销角色 DTO **不含任何媒体 ID**：取图只按 角色 ID + 下标，按媒体 ID 取的路由等于让
// 任何人拿一个 ID 取任何人的图（TestPresetPublic_停用404且无媒体ID 钉住）。

// ---------------------------------------------------------------- 视图

// characterResp 是工作区角色详情（含全部图片 ID —— 都是本工作区的媒体）。
type characterResp struct {
	ID                  string   `json:"id"`
	Name                string   `json:"name"`
	ImageMediaIDs       []string `json:"imageMediaIds"`
	MainMediaID         *string  `json:"mainMediaId"`
	ImageCount          int      `json:"imageCount"`
	Appearance          string   `json:"appearance"`
	StyleCode           *string  `json:"styleCode"`
	VoiceText           string   `json:"voiceText"`
	VoiceMediaID        *string  `json:"voiceMediaId"`
	Source              string   `json:"source"`
	SourcePresetID      *string  `json:"sourcePresetId"`
	SourcePresetVersion *int     `json:"sourcePresetVersion"`
	Version             int      `json:"version"`
	UpdatedByName       *string  `json:"updatedByName"`
	CreatedAt           string   `json:"createdAt"`
	UpdatedAt           string   `json:"updatedAt"`
}

// characterSummaryResp 是列表项（网格只要主图、张数、来源）。
type characterSummaryResp struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	ImageCount  int     `json:"imageCount"`
	MainMediaID *string `json:"mainMediaId"`
	HasVoice    bool    `json:"hasVoice"`
	Source      string  `json:"source"`
	Version     int     `json:"version"`
	CreatedAt   string  `json:"createdAt"`
	UpdatedAt   string  `json:"updatedAt"`
}

// publicPresetResp 是公开的营销角色列表项。⚠️ **没有媒体 ID**（含音频）。
type publicPresetResp struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Summary    string `json:"summary"`
	ImageCount int    `json:"imageCount"`
	HasVoice   bool   `json:"hasVoice"`
	Version    int    `json:"version"`
}

func libOptStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func libOptID(v int64) *string {
	if v <= 0 {
		return nil
	}
	s := idStr(v)
	return &s
}

func toCharacterResp(c repo.Character) characterResp {
	imgs := make([]string, 0, len(c.ImageMediaIDs))
	for _, id := range c.ImageMediaIDs {
		imgs = append(imgs, idStr(id))
	}
	var ver *int
	if c.SourcePresetVersion > 0 {
		v := c.SourcePresetVersion
		ver = &v
	}
	return characterResp{ID: idStr(c.ID), Name: c.Name, ImageMediaIDs: imgs, MainMediaID: mainMedia(c.ImageMediaIDs),
		ImageCount: len(c.ImageMediaIDs), Appearance: c.Appearance, StyleCode: libOptStr(c.StyleCode),
		VoiceText: c.VoiceText, VoiceMediaID: libOptID(c.VoiceMediaID), Source: c.Source,
		SourcePresetID: libOptID(c.SourcePresetID), SourcePresetVersion: ver, Version: c.Version,
		UpdatedByName: libOptStr(c.UpdatedByName), CreatedAt: ts(c.CreatedAt), UpdatedAt: ts(c.UpdatedAt)}
}

func toCharacterSummaryResp(c repo.Character) characterSummaryResp {
	return characterSummaryResp{ID: idStr(c.ID), Name: c.Name, ImageCount: len(c.ImageMediaIDs),
		MainMediaID: mainMedia(c.ImageMediaIDs), HasVoice: c.VoiceMediaID > 0, Source: c.Source,
		Version: c.Version, CreatedAt: ts(c.CreatedAt), UpdatedAt: ts(c.UpdatedAt)}
}

func toPublicPresetResp(p repo.PresetCharacter) publicPresetResp {
	return publicPresetResp{ID: idStr(p.ID), Name: p.Name, Summary: p.Summary, ImageCount: len(p.ImageMediaIDs),
		HasVoice: p.VoiceMediaID > 0, Version: p.Version}
}

// characterReq 是新建 / 更新的请求体。`version` 只在更新时必填。
//
// ⚠️ source / sourcePresetId / sourcePresetVersion 是**事实不是内容**（由 adopt 写下），
// 带了即 40001 并点名字段 —— 不能让 DisallowUnknownFields 的笼统报错替它说话，也不能静默忽略
// （静默忽略的话前端以为改成功了）。
type characterReq struct {
	Name                string           `json:"name"`
	ImageMediaIDs       []string         `json:"imageMediaIds"`
	Appearance          *string          `json:"appearance"`
	StyleCode           *string          `json:"styleCode"`
	VoiceText           *string          `json:"voiceText"`
	VoiceMediaID        *string          `json:"voiceMediaId"`
	Version             *int             `json:"version"`
	Source              *json.RawMessage `json:"source"`
	SourcePresetID      *json.RawMessage `json:"sourcePresetId"`
	SourcePresetVersion *json.RawMessage `json:"sourcePresetVersion"`
}

func decodeCharacter(w http.ResponseWriter, r *http.Request) (characterReq, domain.CharacterInput, bool) {
	var req characterReq
	dec := json.NewDecoder(io.LimitReader(r.Body, maxLibraryBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, fmt.Sprintf("请求体非法：%v", err)))
		return characterReq{}, domain.CharacterInput{}, false
	}
	var ro []domain.Issue
	for _, f := range []struct {
		name string
		v    *json.RawMessage
	}{{"source", req.Source}, {"sourcePresetId", req.SourcePresetID}, {"sourcePresetVersion", req.SourcePresetVersion}} {
		if f.v != nil {
			ro = append(ro, domain.Issue{Field: f.name, Code: domain.IssueBadValue, Level: domain.LevelError,
				Message: f.name + " 由服务端记录，不可在请求里指定或修改"})
		}
	}
	if len(ro) > 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "角色来源不可修改").
			WithDetails(map[string]any{"issues": ro, "warnings": []domain.Issue{}}))
		return characterReq{}, domain.CharacterInput{}, false
	}
	in := domain.CharacterInput{Name: req.Name, ImageMediaIDs: make([]int64, 0, len(req.ImageMediaIDs))}
	if req.Appearance != nil {
		in.Appearance = *req.Appearance
	}
	if req.StyleCode != nil {
		in.StyleCode = *req.StyleCode
	}
	if req.VoiceText != nil {
		in.VoiceText = *req.VoiceText
	}
	if req.VoiceMediaID != nil && *req.VoiceMediaID != "" {
		id, err := strconv.ParseInt(*req.VoiceMediaID, 10, 64)
		if err != nil || id <= 0 {
			id = -1 // 交给领域校验报 voiceMediaId，与其它问题一次报完
		}
		in.VoiceMediaID = id
	}
	for _, s := range req.ImageMediaIDs {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			id = 0
		}
		in.ImageMediaIDs = append(in.ImageMediaIDs, id)
	}
	return req, in, true
}

func libPathID(w http.ResponseWriter, r *http.Request, param, notFound string) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, param), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.NotFound, notFound))
		return 0, false
	}
	return id, true
}

func libListParams(w http.ResponseWriter, r *http.Request) (app.ProductQuery, bool) {
	q := r.URL.Query()
	f := app.ProductQuery{Q: q.Get("q"), Sort: q.Get("sort"), Cursor: q.Get("cursor")}
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "limit 非法"))
			return app.ProductQuery{}, false
		}
		f.Limit = n
	}
	return f, true
}

func libCursor(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ---------------------------------------------------------------- 工作区角色

func (h *Handler) listCharacters(w http.ResponseWriter, r *http.Request) {
	a, ok := h.libraryActor(w, r)
	if !ok {
		return
	}
	f, ok := libListParams(w, r)
	if !ok {
		return
	}
	page, err := h.library.ListCharacters(r.Context(), a, f)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]characterSummaryResp, 0, len(page.Items))
	for _, c := range page.Items {
		items = append(items, toCharacterSummaryResp(c))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": libCursor(page.NextCursor)})
}

func (h *Handler) createCharacter(w http.ResponseWriter, r *http.Request) {
	a, ok := h.libraryActor(w, r)
	if !ok {
		return
	}
	req, in, ok := decodeCharacter(w, r)
	if !ok {
		return
	}
	if req.Version != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "新建角色不接受 version"))
		return
	}
	c, err := h.library.CreateCharacter(r.Context(), a, in)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, toCharacterResp(c))
}

func (h *Handler) getCharacter(w http.ResponseWriter, r *http.Request) {
	a, ok := h.libraryActor(w, r)
	if !ok {
		return
	}
	id, ok := libPathID(w, r, "characterId", "角色不存在")
	if !ok {
		return
	}
	c, err := h.library.GetCharacter(r.Context(), a, id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toCharacterResp(c))
}

func (h *Handler) updateCharacter(w http.ResponseWriter, r *http.Request) {
	a, ok := h.libraryActor(w, r)
	if !ok {
		return
	}
	id, ok := libPathID(w, r, "characterId", "角色不存在")
	if !ok {
		return
	}
	req, in, ok := decodeCharacter(w, r)
	if !ok {
		return
	}
	version := 0
	if req.Version != nil {
		version = *req.Version
	}
	c, err := h.library.UpdateCharacter(r.Context(), a, id, version, in)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toCharacterResp(c))
}

func (h *Handler) deleteCharacter(w http.ResponseWriter, r *http.Request) {
	a, ok := h.libraryActor(w, r)
	if !ok {
		return
	}
	id, ok := libPathID(w, r, "characterId", "角色不存在")
	if !ok {
		return
	}
	if err := h.library.DeleteCharacter(r.Context(), a, id); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// adoptPreset 把营销角色复制进本工作区：新建 201，同版本已添加过 200（回那一份）。
func (h *Handler) adoptPreset(w http.ResponseWriter, r *http.Request) {
	a, ok := h.libraryActor(w, r)
	if !ok {
		return
	}
	id, ok := libPathID(w, r, "presetId", "营销角色不存在或已下线")
	if !ok {
		return
	}
	c, created, err := h.library.Adopt(r.Context(), a, id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpx.JSON(w, status, toCharacterResp(c))
}

// ---------------------------------------------------------------- 营销角色（公开）

func (h *Handler) listPresets(w http.ResponseWriter, r *http.Request) {
	f, ok := libListParams(w, r)
	if !ok {
		return
	}
	page, err := h.library.ListPresets(r.Context(), f.Q, f.Cursor, f.Limit)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]publicPresetResp, 0, len(page.Items))
	for _, p := range page.Items {
		items = append(items, toPublicPresetResp(p))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": libCursor(page.NextCursor)})
}

func (h *Handler) presetImage(w http.ResponseWriter, r *http.Request) {
	id, ok := libPathID(w, r, "presetId", "营销角色不存在或已下线")
	if !ok {
		return
	}
	index, err := strconv.Atoi(chi.URLParam(r, "index"))
	if err != nil || index < 0 {
		// 下标非法与越界、不可见同一个 404。
		httpx.Fail(w, r, httpx.Errf(errcode.NotFound, "营销角色不存在或已下线"))
		return
	}
	width := 320
	if v := r.URL.Query().Get("w"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "w 只接受 160 / 320 / 640"))
			return
		}
		width = n
	}
	mediaID, err := h.library.PresetImageMedia(r.Context(), id, index)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	h.serve(w, r, mediaID, width)
}

func (h *Handler) presetVoice(w http.ResponseWriter, r *http.Request) {
	id, ok := libPathID(w, r, "presetId", "营销角色不存在或已下线")
	if !ok {
		return
	}
	mediaID, err := h.library.PresetVoiceMedia(r.Context(), id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	h.serve(w, r, mediaID, 0)
}
