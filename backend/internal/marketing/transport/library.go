package transport

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	collabtransport "github.com/li904338203/tabtv/backend/internal/collaboration/transport"
	"github.com/li904338203/tabtv/backend/internal/marketing/app"
	"github.com/li904338203/tabtv/backend/internal/marketing/domain"
	"github.com/li904338203/tabtv/backend/internal/marketing/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 商品库用户端接口（MKT-T-33；dev/05 §3.1 的商品 5 个端点）。
//
// ⚠️ 与 MountPublic 不同，这一组**要登录 + 工作区闸**（MKD-32）：GET → view，其余 → edit，
// 由 wsGuard（RequireWorkspace + GuardByMethod）在路由层给 —— 不在 handler 里手写角色判断。
// ⚠️ 仍在营销总开关 gate 之下（SEEAITV_MARKETING_ENABLED=false → 404）。
// ⚠️ 路由用完整路径挂在根路由的独立分组里，不进 MountPublic 的 `r.Route("/marketing")` 子路由：
//    那一组不挂鉴权（模板墙是公开的），混进去的话要么公开接口被迫登录、要么库接口漏掉闸。
//    chi 静态段优先于 `/marketing/*` 的挂载点，两组互不干扰（TestLibraryRoutes_与公开路由共存 钉住）。

// maxLibraryBody 是库接口请求体上限：30 + 300 + 300 字 + 十几个 ID，64 KiB 绰绰有余。
const maxLibraryBody = 64 << 10

// SetLibrary 注入商品库服务与「当前用户」解析（来自登录态）。
func (h *Handler) SetLibrary(lib *app.Library, userIDs func(*http.Request) int64) {
	h.library, h.userIDs = lib, userIDs
}

// MountLibrary 挂载 /marketing/products*、/marketing/characters* 与营销角色 adopt。未注入 SetLibrary 时不挂。
func (h *Handler) MountLibrary(r chi.Router, requireUser, wsGuard func(http.Handler) http.Handler) {
	if h.library == nil {
		return
	}
	r.Group(func(r chi.Router) {
		r.Use(h.gate)
		r.Use(requireUser)
		r.Use(wsGuard)
		r.Get("/marketing/products", h.listProducts)
		r.Post("/marketing/products", h.createProduct)
		r.Get("/marketing/products/{productId}", h.getProduct)
		r.Put("/marketing/products/{productId}", h.updateProduct)
		r.Delete("/marketing/products/{productId}", h.deleteProduct)
		// 角色库与营销角色添加（MKT-T-34，见 character.go）。adopt 是 POST → edit。
		r.Get("/marketing/characters", h.listCharacters)
		r.Post("/marketing/characters", h.createCharacter)
		r.Get("/marketing/characters/{characterId}", h.getCharacter)
		r.Put("/marketing/characters/{characterId}", h.updateCharacter)
		r.Delete("/marketing/characters/{characterId}", h.deleteCharacter)
		r.Post("/marketing/preset-characters/{presetId}/adopt", h.adoptPreset)
	})
}

// ---------------------------------------------------------------- 视图

// productResp 是商品详情（含全部图片 ID）。
type productResp struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	SellingPoints string   `json:"sellingPoints"`
	Description   string   `json:"description"`
	ImageMediaIDs []string `json:"imageMediaIds"`
	MainMediaID   *string  `json:"mainMediaId"`
	ImageCount    int      `json:"imageCount"`
	Version       int      `json:"version"`
	// UpdatedByName 最后编辑者昵称；未知时为 null（不猜）。
	UpdatedByName *string `json:"updatedByName"`
	CreatedAt     string  `json:"createdAt"`
	UpdatedAt     string  `json:"updatedAt"`
}

// productSummaryResp 是列表项。⚠️ 列表**不回全部图片 ID**（详情才回）：网格只要主图与张数。
type productSummaryResp struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	ImageCount  int     `json:"imageCount"`
	MainMediaID *string `json:"mainMediaId"`
	Version     int     `json:"version"`
	CreatedAt   string  `json:"createdAt"`
	UpdatedAt   string  `json:"updatedAt"`
}

func mainMedia(ids []int64) *string {
	if m := domain.MainImage(ids); m > 0 {
		s := idStr(m)
		return &s
	}
	return nil
}

func toProductResp(p repo.Product) productResp {
	imgs := make([]string, 0, len(p.ImageMediaIDs))
	for _, id := range p.ImageMediaIDs {
		imgs = append(imgs, idStr(id))
	}
	var editor *string
	if p.UpdatedByName != "" {
		n := p.UpdatedByName
		editor = &n
	}
	return productResp{ID: idStr(p.ID), Name: p.Name, SellingPoints: p.SellingPoints, Description: p.Description,
		ImageMediaIDs: imgs, MainMediaID: mainMedia(p.ImageMediaIDs), ImageCount: len(p.ImageMediaIDs),
		Version: p.Version, UpdatedByName: editor, CreatedAt: ts(p.CreatedAt), UpdatedAt: ts(p.UpdatedAt)}
}

func toProductSummaryResp(p repo.Product) productSummaryResp {
	return productSummaryResp{ID: idStr(p.ID), Name: p.Name, ImageCount: len(p.ImageMediaIDs),
		MainMediaID: mainMedia(p.ImageMediaIDs), Version: p.Version,
		CreatedAt: ts(p.CreatedAt), UpdatedAt: ts(p.UpdatedAt)}
}

// productReq 是新建 / 更新的请求体。`version` 只在更新时必填。
//
// ⚠️ 指针字段区分「没传」与「传了空串」：卖点 / 描述可选，没传按空串存（整份更新的语义）。
type productReq struct {
	Name          string   `json:"name"`
	SellingPoints *string  `json:"sellingPoints"`
	Description   *string  `json:"description"`
	ImageMediaIDs []string `json:"imageMediaIds"`
	Version       *int     `json:"version"`
}

// ---------------------------------------------------------------- 路由

func (h *Handler) libraryActor(w http.ResponseWriter, r *http.Request) (app.LibraryActor, bool) {
	ws, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		// 工作区闸没跑（中间件顺序写反）时一律拒，不是放行（CD-59）。
		httpx.Fail(w, r, httpx.Err(errcode.NotWorkspaceMember))
		return app.LibraryActor{}, false
	}
	var uid int64
	if h.userIDs != nil {
		uid = h.userIDs(r)
	}
	return app.LibraryActor{WorkspaceID: ws.WorkspaceID, UserID: uid}, true
}

func productID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "productId"), 10, 64)
	if err != nil || id <= 0 {
		// 非法 ID 与不存在同一个 404：不为「格式」单开一种回答。
		httpx.Fail(w, r, httpx.Errf(errcode.NotFound, "商品不存在"))
		return 0, false
	}
	return id, true
}

// decodeProduct 严格解码（未知字段直接拒：键名写错时不能当成「没传」，§5.10）并把 ID 换成 int64。
//
// ID 解析失败的那一张按 0 交给领域校验，由它报 `imageMediaIds[i]`，与其它字段的问题一次报完。
func decodeProduct(w http.ResponseWriter, r *http.Request) (productReq, domain.ProductInput, bool) {
	var req productReq
	dec := json.NewDecoder(io.LimitReader(r.Body, maxLibraryBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, fmt.Sprintf("请求体非法：%v", err)))
		return productReq{}, domain.ProductInput{}, false
	}
	in := domain.ProductInput{Name: req.Name, ImageMediaIDs: make([]int64, 0, len(req.ImageMediaIDs))}
	if req.SellingPoints != nil {
		in.SellingPoints = *req.SellingPoints
	}
	if req.Description != nil {
		in.Description = *req.Description
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

func (h *Handler) listProducts(w http.ResponseWriter, r *http.Request) {
	a, ok := h.libraryActor(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := app.ProductQuery{Q: q.Get("q"), Sort: q.Get("sort"), Cursor: q.Get("cursor")}
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "limit 非法"))
			return
		}
		f.Limit = n
	}
	page, err := h.library.ListProducts(r.Context(), a, f)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]productSummaryResp, 0, len(page.Items))
	for _, p := range page.Items {
		items = append(items, toProductSummaryResp(p))
	}
	var next *string
	if page.NextCursor != "" {
		next = &page.NextCursor
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": next})
}

func (h *Handler) createProduct(w http.ResponseWriter, r *http.Request) {
	a, ok := h.libraryActor(w, r)
	if !ok {
		return
	}
	req, in, ok := decodeProduct(w, r)
	if !ok {
		return
	}
	if req.Version != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "新建商品不接受 version"))
		return
	}
	p, err := h.library.CreateProduct(r.Context(), a, in)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, toProductResp(p))
}

func (h *Handler) getProduct(w http.ResponseWriter, r *http.Request) {
	a, ok := h.libraryActor(w, r)
	if !ok {
		return
	}
	id, ok := productID(w, r)
	if !ok {
		return
	}
	p, err := h.library.GetProduct(r.Context(), a, id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toProductResp(p))
}

func (h *Handler) updateProduct(w http.ResponseWriter, r *http.Request) {
	a, ok := h.libraryActor(w, r)
	if !ok {
		return
	}
	id, ok := productID(w, r)
	if !ok {
		return
	}
	req, in, ok := decodeProduct(w, r)
	if !ok {
		return
	}
	version := 0
	if req.Version != nil {
		version = *req.Version
	}
	p, err := h.library.UpdateProduct(r.Context(), a, id, version, in)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toProductResp(p))
}

func (h *Handler) deleteProduct(w http.ResponseWriter, r *http.Request) {
	a, ok := h.libraryActor(w, r)
	if !ok {
		return
	}
	id, ok := productID(w, r)
	if !ok {
		return
	}
	if err := h.library.DeleteProduct(r.Context(), a, id); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
