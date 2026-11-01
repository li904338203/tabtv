package transport

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/li904338203/tabtv/backend/internal/catalog/app"
	"github.com/li904338203/tabtv/backend/internal/catalog/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// MountMaterial 挂载素材与音色接口。
func (h *Handler) MountMaterial(r chi.Router, requireUser func(http.Handler) http.Handler,
	requireAdmin func(http.Handler) http.Handler,
	requirePerm func(string) func(http.Handler) http.Handler) {

	// 用户端：浏览、筛选、收藏、试听。
	r.Group(func(r chi.Router) {
		r.Use(requireUser)
		r.Get("/canvas/styles", h.listStyles)
		r.Get("/canvas/effects", h.listEffects)
		r.Get("/canvas/cameras", h.listCameras)
		r.Post("/canvas/materials/{type}/{code}/favorite", h.toggleFavorite)
		r.Get("/voices", h.listVoices)
		r.Post("/voices/{code}/favorite", h.toggleVoiceFavorite)
	})

	// 管理端。
	r.Group(func(r chi.Router) {
		r.Use(requireAdmin)
		r.With(requirePerm("canvas:read")).Get("/admin/materials/{type}", h.adminListMaterials)
		r.With(requirePerm("canvas:write")).Put("/admin/materials", h.saveMaterial)
		r.With(requirePerm("voice:read")).Get("/admin/voices", h.adminListVoices)
		r.With(requirePerm("voice:write")).Put("/admin/voices", h.saveVoice)
	})
}

type materialResp struct {
	Code         string `json:"code"`
	Name         string `json:"name"`
	Category     string `json:"category,omitempty"`
	Author       string `json:"author,omitempty"`
	IsCommercial bool   `json:"isCommercial"`
	CoverURL     string `json:"coverUrl,omitempty"`
	IsFavorited  bool   `json:"isFavorited"`
	// ⚠️ prompt 不下发给用户端：拼接在服务端完成，
	// 下发了前端就会想自己拼，拼出来的词会落进 canvas_data。
	Prompt    string `json:"prompt,omitempty"`
	IsEnabled bool   `json:"isEnabled,omitempty"`
	// 以下只在管理端下发：保存是整条替换，编辑表单要能原样带回封面与排序，
	// 否则改一个名字就把封面和排序清掉了。
	CoverMediaID string `json:"coverMediaId,omitempty"`
	Sort         *int   `json:"sort,omitempty"`
}

func (h *Handler) listMaterialsBy(w http.ResponseWriter, r *http.Request, mt domain.MaterialType) {
	uid := h.userID(r)
	q := r.URL.Query()
	list, err := h.svc.ListMaterials(r.Context(), mt, uid, app.MaterialFilter{
		Keyword: q.Get("keyword"), Category: q.Get("category"),
		CommercialOnly: q.Get("commercial") == "true",
		FavoritedOnly:  q.Get("favorited") == "true",
	}, true)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := make([]materialResp, 0, len(list))
	for _, m := range list {
		out = append(out, materialResp{
			Code: m.Code, Name: m.Name, Category: m.Category, Author: m.Author,
			IsCommercial: m.IsCommercial, IsFavorited: m.IsFavorited,
			CoverURL: mediaURL(m.CoverMediaID),
		})
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) listStyles(w http.ResponseWriter, r *http.Request) {
	h.listMaterialsBy(w, r, domain.MaterialStyle)
}

func (h *Handler) listEffects(w http.ResponseWriter, r *http.Request) {
	h.listMaterialsBy(w, r, domain.MaterialEffect)
}

// listCameras 运镜广场（视频节点「运镜」）。与风格、特效同一套筛选与收藏，提示词同样不下发。
func (h *Handler) listCameras(w http.ResponseWriter, r *http.Request) {
	h.listMaterialsBy(w, r, domain.MaterialCamera)
}

func (h *Handler) toggleFavorite(w http.ResponseWriter, r *http.Request) {
	h.doToggleFavorite(w, r, domain.MaterialType(chi.URLParam(r, "type")), chi.URLParam(r, "code"))
}

func (h *Handler) toggleVoiceFavorite(w http.ResponseWriter, r *http.Request) {
	h.doToggleFavorite(w, r, domain.MaterialVoice, chi.URLParam(r, "code"))
}

func (h *Handler) doToggleFavorite(w http.ResponseWriter, r *http.Request, mt domain.MaterialType, code string) {
	var req struct {
		Favorite bool `json:"favorite"`
	}
	if !decode(w, r, &req) {
		return
	}
	uid := h.userID(r)
	if uid == 0 {
		httpx.Fail(w, r, httpx.Err(errcode.Unauthenticated))
		return
	}
	got, err := h.svc.ToggleFavorite(r.Context(), uid, mt, code, req.Favorite)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]bool{"isFavorited": got})
}

type voiceResp struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	Language    string `json:"language,omitempty"`
	Gender      string `json:"gender,omitempty"`
	IconURL     string `json:"iconUrl,omitempty"`
	PreviewURL  string `json:"previewUrl,omitempty"`
	IsFavorited bool   `json:"isFavorited"`
	// UpstreamVoiceID 只在管理端返回：它是上游的内部标识，
	// 暴露给用户端没有意义，反而多一处信息泄漏面。
	UpstreamVoiceID string `json:"upstreamVoiceId,omitempty"`
	IsEnabled       bool   `json:"isEnabled,omitempty"`
	// 只在管理端下发（同素材：编辑要能原样带回图标、试听与排序）。
	IconMediaID    string `json:"iconMediaId,omitempty"`
	PreviewMediaID string `json:"previewMediaId,omitempty"`
	Sort           *int   `json:"sort,omitempty"`
}

func (h *Handler) listVoices(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	list, err := h.svc.ListVoices(r.Context(), h.userID(r), app.VoiceFilter{
		Keyword: q.Get("keyword"), Language: q.Get("language"), Gender: q.Get("gender"),
		FavoritedOnly: q.Get("favorited") == "true",
	}, true)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := make([]voiceResp, 0, len(list))
	for _, v := range list {
		out = append(out, voiceResp{
			Code: v.Code, Name: v.Name, Language: v.Language, Gender: v.Gender,
			IconURL: mediaURL(v.IconMediaID), PreviewURL: mediaURL(v.PreviewMediaID),
			IsFavorited: v.IsFavorited,
		})
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) adminListMaterials(w http.ResponseWriter, r *http.Request) {
	mt := domain.MaterialType(chi.URLParam(r, "type"))
	list, err := h.svc.ListMaterials(r.Context(), mt, 0, app.MaterialFilter{
		Keyword: r.URL.Query().Get("keyword"),
	}, false)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := make([]materialResp, 0, len(list))
	for _, m := range list {
		sort := m.Sort
		out = append(out, materialResp{
			Code: m.Code, Name: m.Name, Category: m.Category, Author: m.Author,
			IsCommercial: m.IsCommercial, CoverURL: mediaURL(m.CoverMediaID),
			Prompt: m.Prompt, IsEnabled: m.IsEnabled,
			CoverMediaID: idString(m.CoverMediaID), Sort: &sort,
		})
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) saveMaterial(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Type         string `json:"type"`
		Code         string `json:"code"`
		Name         string `json:"name"`
		Category     string `json:"category"`
		Prompt       string `json:"prompt"`
		Author       string `json:"author"`
		IsCommercial bool   `json:"isCommercial"`
		CoverMediaID *int64 `json:"coverMediaId,string"`
		Sort         int    `json:"sort"`
		IsEnabled    bool   `json:"isEnabled"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := h.svc.SaveMaterial(r.Context(), h.adminID(r), domain.Material{
		Type: domain.MaterialType(req.Type), Code: req.Code, Name: req.Name,
		Category: req.Category, Prompt: req.Prompt, Author: req.Author,
		IsCommercial: req.IsCommercial, CoverMediaID: req.CoverMediaID,
		Sort: req.Sort, IsEnabled: req.IsEnabled,
	}); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) adminListVoices(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.ListVoices(r.Context(), 0, app.VoiceFilter{
		Keyword: r.URL.Query().Get("keyword"),
	}, false)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := make([]voiceResp, 0, len(list))
	for _, v := range list {
		sort := v.Sort
		out = append(out, voiceResp{
			Code: v.Code, Name: v.Name, Language: v.Language, Gender: v.Gender,
			IconURL: mediaURL(v.IconMediaID), PreviewURL: mediaURL(v.PreviewMediaID),
			UpstreamVoiceID: v.UpstreamVoiceID, IsEnabled: v.IsEnabled,
			IconMediaID: idString(v.IconMediaID), PreviewMediaID: idString(v.PreviewMediaID), Sort: &sort,
		})
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) saveVoice(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code            string `json:"code"`
		Name            string `json:"name"`
		UpstreamVoiceID string `json:"upstreamVoiceId"`
		Language        string `json:"language"`
		Gender          string `json:"gender"`
		IconMediaID     *int64 `json:"iconMediaId,string"`
		PreviewMediaID  *int64 `json:"previewMediaId,string"`
		Sort            int    `json:"sort"`
		IsEnabled       bool   `json:"isEnabled"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := h.svc.SaveVoice(r.Context(), h.adminID(r), domain.Voice{
		Code: req.Code, Name: req.Name, UpstreamVoiceID: req.UpstreamVoiceID,
		Language: req.Language, Gender: req.Gender,
		IconMediaID: req.IconMediaID, PreviewMediaID: req.PreviewMediaID,
		Sort: req.Sort, IsEnabled: req.IsEnabled,
	}); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// idString 媒体 ID 对外一律 string（防 JS 精度丢失）；没有时给空串。
func idString(id *int64) string {
	if id == nil || *id == 0 {
		return ""
	}
	return itoa64(*id)
}

// mediaURL 拼受保护媒体端点。
// ⚠️ 不是对象存储直链：直链绕过归属校验，且过期后已归档内容会失效。
func mediaURL(id *int64) string {
	if id == nil || *id == 0 {
		return ""
	}
	return "/api/v1/media/" + itoa64(*id)
}
