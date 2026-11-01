package transport

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/li904338203/tabtv/backend/internal/marketing/app"
	"github.com/li904338203/tabtv/backend/internal/marketing/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 用户端营销接口（MKT-T-04；dev/02 §4.1 的 5 个端点，技能封面在 agent 域）。
//
// ⚠️ 全部**公开**、不挂 requireUser：未登录也要能浏览模板墙（MKT-F-03），
// 门面上一屏破图正是他决定要不要注册的那一眼。
// ⚠️ 总开关 SEEAITV_MARKETING_ENABLED=false 时全部 404（用户端侧栏据此隐藏入口）。
// ⚠️ 封面 / 示例视频**只按 code + 下标取**：公开取图不做归属校验，按媒体 ID 取的路由
//    等于让任何人拿一个 ID 取任何人的图。

// MediaServer 由媒体域实现：把一份媒体吐给客户端，**不做归属校验**（调用方已判过可见）。
// 端口定义在这里：marketing 不 import 媒体域。
type MediaServer interface {
	ServePublic(w http.ResponseWriter, r *http.Request, mediaID int64, width int)
	// RedirectPublic 同 ServePublic，但 302 到对象存储的短时效签名地址、不经 API 转发字节（2026-11-02）。
	// 只给 <img> / <video> 直接加载的匿名公开端点用。
	RedirectPublic(w http.ResponseWriter, r *http.Request, mediaID int64, width int)
}

// Handler 是用户端入口。
type Handler struct {
	svc     *app.Service
	enabled bool
	media   MediaServer
	// library 是商品库 / 角色库（MKT-T-33，见 library.go）；为 nil 时 MountLibrary 不挂路由。
	library *app.Library
	userIDs func(*http.Request) int64
	// charGen / memberLevel 是生成角色（MKT-T-35，见 chargen.go）；为 nil 时 MountCharacterGen 不挂路由。
	charGen     *app.CharacterGen
	memberLevel func(*http.Request) int64
}

// NewHandler 构造。enabled=false 时全部路由 404。
func NewHandler(svc *app.Service, enabled bool) *Handler {
	return &Handler{svc: svc, enabled: enabled}
}

// SetMedia 注入公开取图。未注入时封面端点回 500「媒体服务未接入」。
func (h *Handler) SetMedia(m MediaServer) { h.media = m }

// MountPublic 挂载 /marketing/…（不挂任何鉴权中间件）。
func (h *Handler) MountPublic(r chi.Router) {
	r.Route("/marketing", func(r chi.Router) {
		r.Use(h.gate)
		r.Get("/config", h.config)
		r.Get("/options", h.options)
		r.Get("/options/{code}", h.option)
		r.Get("/options/{code}/covers/{index}", h.cover)
		r.Get("/options/{code}/preview", h.preview)
		// 营销角色（平台预置，MKT-T-34）：公开、只认启用的；adopt 要登录，挂在 MountLibrary。
		if h.library != nil {
			r.Get("/preset-characters", h.listPresets)
			r.Get("/preset-characters/{presetId}/images/{index}", h.presetImage)
			r.Get("/preset-characters/{presetId}/voice", h.presetVoice)
		}
	})
}

func (h *Handler) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.enabled {
			httpx.Fail(w, r, httpx.Errf(errcode.NotFound, "营销 Agent 暂未开放"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- 视图

type dimensionResp struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

type modelRefResp struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

type configResp struct {
	Available            bool                      `json:"available"`
	UnavailableReason    *string                   `json:"unavailableReason"`
	Slots                []domain.SlotDecl         `json:"slots"`
	MaxProductImages     int                       `json:"maxProductImages"`
	MaxVariants          int                       `json:"maxVariants"`
	Dimensions           []dimensionResp           `json:"dimensions"`
	Categories           map[string][]categoryResp `json:"categories"`
	CreativeModelFilters []modelRefResp            `json:"creativeModelFilters"`
	// 商品库 / 角色库（MKT-T-37；dev/05 §3.3）。
	MaxProductsPerSession  int               `json:"maxProductsPerSession"`
	MaxCharacterRefs       int               `json:"maxCharacterRefs"`
	MaxCharacterImages     int               `json:"maxCharacterImages"`
	MaxCharacterGenCount   int               `json:"maxCharacterGenCount"`
	Quota                  quotaResp         `json:"quota"`
	CharacterNameForbidden []string          `json:"characterNameForbidden"`
	AllowedImageMimes      []string          `json:"allowedImageMimes"`
	TextLimits             libraryTextLimits `json:"textLimits"`
}

type quotaResp struct {
	Products   int `json:"products"`
	Characters int `json:"characters"`
}

// libraryTextLimits 是商品 / 角色文本字段的长度上限（码点）。
type libraryTextLimits struct {
	ProductName        int `json:"productName"`
	SellingPoints      int `json:"sellingPoints"`
	ProductDescription int `json:"productDescription"`
	CharacterName      int `json:"characterName"`
	Appearance         int `json:"appearance"`
	VoiceText          int `json:"voiceText"`
}

type recommendedModelResp struct {
	Code      string `json:"code"`
	Name      string `json:"name"`
	Available bool   `json:"available"`
}

// publicOptionResp 是用户端的一条选项。⚠️ **没有 prompt 字段，也没有媒体 ID**（MKT-AC-06）；
// `TestPublicOptionResp_无prompt字段` 钉住这一条。
type publicOptionResp struct {
	Kind              string                 `json:"kind"`
	Code              string                 `json:"code"`
	Name              string                 `json:"name"`
	CategoryCode      string                 `json:"categoryCode"`
	Description       string                 `json:"description"`
	Lifecycle         string                 `json:"lifecycle"`
	CoverCount        int                    `json:"coverCount"`
	HasPreview        bool                   `json:"hasPreview"`
	IsAudioRequired   bool                   `json:"isAudioRequired"`
	OutputTypes       []string               `json:"outputTypes"`
	InputRequirements map[string]string      `json:"inputRequirements"`
	Tags              []string               `json:"tags"`
	RecommendedModels []recommendedModelResp `json:"recommendedModels"`
	RecommendedParams json.RawMessage        `json:"recommendedParams"`
	DefaultSkillCode  *string                `json:"defaultSkillCode"`
}

func toPublicOptionResp(p app.PublicOption) publicOptionResp {
	models := make([]recommendedModelResp, 0, len(p.RecommendedModels))
	for _, m := range p.RecommendedModels {
		models = append(models, recommendedModelResp{Code: m.Code, Name: m.Name, Available: m.Available})
	}
	var skill *string
	if p.DefaultSkillCode != "" {
		s := p.DefaultSkillCode
		skill = &s
	}
	return publicOptionResp{
		Kind: string(p.Kind), Code: p.Code, Name: p.Name, CategoryCode: p.CategoryCode, Description: p.Description,
		Lifecycle: string(p.Lifecycle), CoverCount: p.CoverCount, HasPreview: p.HasPreview,
		IsAudioRequired: p.IsAudioRequired, OutputTypes: nonNilStrings(p.OutputTypes),
		InputRequirements: nonNilMap(p.InputRequirements), Tags: nonNilStrings(p.Tags), RecommendedModels: models,
		RecommendedParams: rawOr(p.RecommendedParams, "{}"), DefaultSkillCode: skill,
	}
}

// ---------------------------------------------------------------- 路由

func (h *Handler) config(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.Config(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	resp := configResp{Available: v.Available, Slots: v.Slots, MaxProductImages: v.MaxProductImages,
		MaxVariants: v.MaxVariants, Categories: map[string][]categoryResp{},
		CreativeModelFilters: make([]modelRefResp, 0, len(v.CreativeModelFilters))}
	lc := v.Library
	resp.MaxProductsPerSession, resp.MaxCharacterRefs = lc.MaxProductsPerSession, lc.MaxCharacterRefs
	resp.MaxCharacterImages, resp.MaxCharacterGenCount = lc.MaxCharacterImages, lc.MaxCharacterGenCount
	resp.Quota = quotaResp{Products: lc.QuotaProducts, Characters: lc.QuotaCharacters}
	resp.CharacterNameForbidden, resp.AllowedImageMimes = nonNilStrings(lc.CharacterNameForbidden),
		nonNilStrings(lc.AllowedImageMimes)
	resp.TextLimits = libraryTextLimits{ProductName: lc.MaxProductName, SellingPoints: lc.MaxSellingPoints,
		ProductDescription: lc.MaxProductDescription, CharacterName: lc.MaxCharacterName,
		Appearance: lc.MaxAppearance, VoiceText: lc.MaxVoiceText}
	if v.UnavailableReason != "" {
		s := v.UnavailableReason
		resp.UnavailableReason = &s
	}
	if resp.Slots == nil {
		resp.Slots = []domain.SlotDecl{}
	}
	for _, d := range v.Dimensions {
		resp.Dimensions = append(resp.Dimensions, dimensionResp{Key: d.Key, Label: d.Label})
	}
	for _, k := range domain.Kinds {
		list := make([]categoryResp, 0, len(v.Categories[k]))
		for _, c := range v.Categories[k] {
			list = append(list, toCategoryResp(c))
		}
		resp.Categories[string(k)] = list
	}
	for _, m := range v.CreativeModelFilters {
		resp.CreativeModelFilters = append(resp.CreativeModelFilters, modelRefResp{Code: m.Code, Name: m.Name})
	}
	httpx.JSON(w, http.StatusOK, resp)
}

func (h *Handler) options(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := app.OptionsQuery{Kind: domain.Kind(q.Get("kind")), Category: q.Get("category"), Model: q.Get("model"),
		Q: q.Get("q"), Cursor: q.Get("cursor")}
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "limit 非法"))
			return
		}
		f.Limit = n
	}
	page, err := h.svc.ListOptions(r.Context(), f)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]publicOptionResp, 0, len(page.Items))
	for _, p := range page.Items {
		items = append(items, toPublicOptionResp(p))
	}
	var next *string
	if page.NextCursor != "" {
		next = &page.NextCursor
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": next})
}

func (h *Handler) option(w http.ResponseWriter, r *http.Request) {
	p, err := h.svc.GetOption(r.Context(), chi.URLParam(r, "code"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toPublicOptionResp(p))
}

func (h *Handler) cover(w http.ResponseWriter, r *http.Request) {
	index, err := strconv.Atoi(chi.URLParam(r, "index"))
	if err != nil || index < 0 {
		// 下标非法与越界、不可见同一个 404：不给探测「这个 code 存不存在」留缝。
		httpx.Fail(w, r, httpx.Errf(errcode.NotFound, "选项不存在或已下线"))
		return
	}
	// ⚠️ 默认给缩略图（模板墙一屏几十张）；宽度只接受媒体域档位，其余由媒体域回 400。
	width := 320
	if v := r.URL.Query().Get("w"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "w 只接受 160 / 320 / 640"))
			return
		}
		width = n
	}
	mediaID, err := h.svc.CoverMedia(r.Context(), chi.URLParam(r, "code"), index)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	h.serve(w, r, mediaID, width)
}

func (h *Handler) preview(w http.ResponseWriter, r *http.Request) {
	mediaID, err := h.svc.PreviewMedia(r.Context(), chi.URLParam(r, "code"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	// 视频不缩（width=0 原样输出）。
	h.serve(w, r, mediaID, 0)
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, mediaID int64, width int) {
	if h.media == nil {
		httpx.Fail(w, r, httpx.Errf(errcode.Internal, "媒体服务未接入"))
		return
	}
	// 302 到对象存储的短时效签名地址（2026-11-02）；跳转的 Cache-Control: public, max-age=300 由 RedirectPublic 设：
	// 缓存期即「撤不下来」的窗口（CD-61）。
	h.media.RedirectPublic(w, r, mediaID, width)
}
