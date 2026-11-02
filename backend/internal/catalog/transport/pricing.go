package transport

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/li904338203/tabtv/backend/internal/catalog/app"
	"github.com/li904338203/tabtv/backend/internal/catalog/domain"
	"github.com/li904338203/tabtv/backend/internal/catalog/repo"
	imagetooldomain "github.com/li904338203/tabtv/backend/internal/imagetool/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// MountPricing 挂载计价管理与用户端报价。
func (h *Handler) MountPricing(r chi.Router, requireUser func(http.Handler) http.Handler,
	requireAdmin func(http.Handler) http.Handler,
	requirePerm func(string) func(http.Handler) http.Handler) {

	r.Route("/admin/pricing", func(r chi.Router) {
		r.Use(requireAdmin)
		r.With(requirePerm("model:read")).Get("/", h.listPrices)
		r.With(requirePerm("model:write")).Put("/", h.savePrice)
		r.With(requirePerm("model:write")).Delete("/{operation}", h.deleteFallbackPrice)
		r.With(requirePerm("model:write")).Delete("/{operation}/{model}", h.deletePrice)
	})

	h.MountQuote(r, requireUser)
}

// MountQuote 只挂用户端那一半：报价（能力校验）、图片工具栏、可用模型与模型参数。
//
// ⚠️ 桌面版去掉了计价管理（TT-T-09），但这四个端点**不是**计价功能：模型下拉、图片工具的可用性、
// 发送前的能力校验都走它们。整组随 MountPricing 一起不挂的话，画布上没有模型可选、工具栏全灰，
// 而后端测试照样全绿（TT-T-10 的契约对照测试抓出来的）。
func (h *Handler) MountQuote(r chi.Router, requireUser func(http.Handler) http.Handler) {
	r.Group(func(r chi.Router) {
		r.Use(requireUser)
		r.Post("/generations/estimate", h.estimate)
		// 图片工具栏（P09-04）：可用性与单价同源于报价路径，不另判一次。
		r.Get("/image-tools", h.imageTools)
		r.Get("/models", h.usableModels)
		r.Get("/models/{code}/params", h.modelParams)
	})
}

func (h *Handler) listPrices(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.ListPrices(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	type resp struct {
		Operation string `json:"operation"`
		Model     string `json:"model,omitempty"`
		Kind      string `json:"kind"`
		Credits   int    `json:"credits"`
		Unit      string `json:"unit"`
		Enabled   bool   `json:"enabled"`
		// InUseBy 去掉（或停用）这条价格后会报不出价的模型（不绑模型的能力为空串）。
		InUseBy []string `json:"inUseBy"`
	}
	out := make([]resp, 0, len(list))
	for _, p := range list {
		kind := "exact"
		if p.IsFallback() {
			kind = "fallback"
		}
		inUse := p.InUseBy
		if inUse == nil {
			inUse = []string{}
		}
		out = append(out, resp{p.OperationCode, p.ModelCode, kind, p.Credits, string(p.Unit), p.Enabled, inUse})
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) savePrice(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Operation string `json:"operation"`
		Model     string `json:"model"`
		Credits   int    `json:"credits"`
		Unit      string `json:"unit"`
		Enabled   bool   `json:"enabled"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := h.svc.SavePrice(r.Context(), h.adminID(r), domain.Price{
		OperationCode: req.Operation, ModelCode: req.Model,
		Credits: req.Credits, Unit: domain.Unit(req.Unit), Enabled: req.Enabled,
	}); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) deleteFallbackPrice(w http.ResponseWriter, r *http.Request) {
	h.doDeletePrice(w, r, chi.URLParam(r, "operation"), "")
}

func (h *Handler) deletePrice(w http.ResponseWriter, r *http.Request) {
	h.doDeletePrice(w, r, chi.URLParam(r, "operation"), chi.URLParam(r, "model"))
}

func (h *Handler) doDeletePrice(w http.ResponseWriter, r *http.Request, op, model string) {
	if err := h.svc.DeletePrice(r.Context(), h.adminID(r), op, model); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// estimate 报价。**扣费前的全部校验都在这里跑一遍**，
// 让用户在点生成之前就知道能不能跑、要花多少（CV-20）。
func (h *Handler) estimate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ModelCode   string         `json:"modelCode"`
		Operation   string         `json:"operation"`
		Count       int            `json:"count"`
		DurationSec int            `json:"durationSec"`
		Text        string         `json:"text"`
		Inputs      map[string]int `json:"inputs"`
		// 图片节点工具条（docs/agent/imagetools R-5）：带了 tool 时按图生图报价，张数由工具选项定。
		Tool        string `json:"tool"`
		ToolOptions any    `json:"toolOptions"`
	}
	if !decode(w, r, &req) {
		return
	}
	counts := domain.RoleCount{}
	for role, n := range req.Inputs {
		counts[domain.InputRole(role)] = n
	}
	if imagetooldomain.IsTool(req.Tool) {
		req.Operation = imagetooldomain.OpImg2Img
		req.Count = imagetooldomain.CountOf(req.Tool, req.ToolOptions)
	}
	q, err := h.svc.Quote(r.Context(), app.QuoteRequest{
		ModelCode: req.ModelCode, OperationCode: req.Operation,
		Count: req.Count, DurationSec: req.DurationSec, Text: req.Text,
		MemberLevelID: h.memberLevel(r), RoleCounts: counts,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, q)
}

// imageTools 图片工具栏：每个工具能不能点、不能点是为什么、能点要多少积分。
//
// ⚠️ **只读接口**。`imageCount` / `hasText` 由前端报当前节点上连着什么，
// 仅用于给提示 —— 客户端谎报只会误导它自己，真正的校验仍在受理链路上
// （"角色计数不信客户端"那条规则约束的是报价与受理，不是这个提示接口）。
func (h *Handler) imageTools(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	n, _ := strconv.Atoi(q.Get("imageCount"))
	list, err := h.svc.ImageTools(r.Context(), app.ImageToolAsk{
		ModelCode: q.Get("modelCode"), MemberLevelID: h.memberLevel(r),
		ImageCount: n, HasText: q.Get("hasText") == "true",
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, list)
}

// usableModels 用户端可选模型。
//
// ⚠️ 只回 code / name / domain / resultType / 会员门槛。
// provider code、提交地址、协议**不下发**：那些是运维信息，
// 给到浏览器等于把上游拓扑公开出去。
func (h *Handler) usableModels(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// agentCapable=true 只列推理模型（AGT-04）；其它取值一律当没传，不报错。
	list, err := h.svc.UsableModels(r.Context(), q.Get("resultType"), q.Get("agentCapable") == "true")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if list == nil {
		list = []repo.UsableModel{}
	}
	httpx.JSON(w, http.StatusOK, list)
}

// modelParams 下发一个模型的型号级参数覆盖（合并的第三层，P15-01）。
func (h *Handler) modelParams(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")
	override, err := h.svc.ModelParams(r.Context(), code)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, struct {
		ModelCode string                    `json:"modelCode"`
		Override  map[string]map[string]any `json:"override"`
	}{code, override})
}
