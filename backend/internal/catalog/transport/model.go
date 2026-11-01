package transport

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/li904338203/tabtv/backend/internal/catalog/app"
	"github.com/li904338203/tabtv/backend/internal/catalog/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// MountModelAdmin 挂载模型/Provider 管理接口。
func (h *Handler) MountModelAdmin(r chi.Router, requireAdmin func(http.Handler) http.Handler,
	requirePerm func(string) func(http.Handler) http.Handler) {
	r.Route("/admin/models", func(r chi.Router) {
		r.Use(requireAdmin)
		read := requirePerm("model:read")
		write := requirePerm("model:write")

		// 协议目录来自**代码注册表**，管理端下拉直接读它（修 GAP-09）。
		r.With(read).Get("/protocols", h.listProtocols)
		r.With(read).Get("/providers", h.listProviders)
		r.With(write).Put("/providers", h.saveProvider)
		// 连接测试（P03-02 的挂账，P06-01 补上）。
		// ⚠️ 用 POST：它会真的向上游发一次请求，不是幂等的读操作。
		r.With(write).Post("/providers/{code}/probe", h.probeProvider)
		r.With(read).Get("/", h.listModels)
		r.With(write).Put("/", h.saveModel)
		r.With(read).Get("/{code}/publishable", h.publishable)
		r.With(write).Put("/{code}/operations", h.saveModelOperation)
		r.With(read).Get("/{code}/capabilities", h.listCapabilities)
		r.With(write).Put("/{code}/capabilities", h.saveCapability)
		r.With(write).Delete("/{code}/capabilities/{operation}/{role}", h.deleteCapability)
	})
	// 操作目录只读：后台的计价与模型绑定表单要能选到**还没有任何价格/绑定**的操作 ——
	// 此前只能从已有的价格里凑操作码，新操作永远配不上价。
	r.With(requireAdmin, requirePerm("model:read")).Get("/admin/operations", h.listOperations)
}

func (h *Handler) listOperations(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.ListOperations(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	type resp struct {
		Code        string `json:"code"`
		Name        string `json:"name"`
		Domain      string `json:"domain"`
		InNodeTypes string `json:"inNodeTypes"`
		OutNodeType string `json:"outNodeType"`
		Status      string `json:"status"`
		// Modelless 不绑模型、只靠兜底价计费（时间线渲染）：它只能配兜底价。
		Modelless bool `json:"modelless"`
	}
	out := make([]resp, 0, len(list))
	for _, o := range list {
		out = append(out, resp{o.Code, o.Name, o.Domain, o.InNodeTypes, o.OutNodeType, o.Status,
			domain.IsModelless(o.Code)})
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) listProtocols(w http.ResponseWriter, r *http.Request) {
	ps := h.svc.Protocols()
	type resp struct {
		Code              string `json:"code"`
		Name              string `json:"name"`
		IsAsync           bool   `json:"isAsync"`
		RequiresQueryPath bool   `json:"requiresQueryPath"`
		// ChatCapable 能否作 Agent 推理模型（AGT-04）。
		ChatCapable bool `json:"chatCapable"`
	}
	out := make([]resp, 0, len(ps))
	for _, p := range ps {
		out = append(out, resp{p.Code, p.Name, p.IsAsync, p.RequiresQueryPath, p.ChatCapable})
	}
	httpx.JSON(w, http.StatusOK, out)
}

// probeProvider 测试一个 Provider 是否可达。
//
// ⚠️ 需要 write 权限而不是 read：它会真的向上游发请求、
// 会在 Provider 那边留下访问记录，也可能触发对方的限流。
func (h *Handler) probeProvider(w http.ResponseWriter, r *http.Request) {
	if h.prober == nil {
		httpx.Fail(w, r, httpx.Errf(errcode.Internal, "连接测试未接入"))
		return
	}
	res, err := h.prober.Probe(r.Context(), chi.URLParam(r, "code"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, res)
}

func (h *Handler) listProviders(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.ListProviders(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, list)
}

func (h *Handler) saveProvider(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code             string           `json:"code"`
		Name             string           `json:"name"`
		AdapterType      string           `json:"adapterType"`
		BaseURL          string           `json:"baseUrl"`
		AuthType         string           `json:"authType"`
		APIKey           *app.SecretField `json:"apiKey"`
		Headers          json.RawMessage  `json:"headers"`
		TestPath         string           `json:"testPath"`
		ConnectTimeoutMS int              `json:"connectTimeoutMs"`
		RequestTimeoutMS int              `json:"requestTimeoutMs"`
		IdleTimeoutMS    int              `json:"idleTimeoutMs"`
		MaxRetries       int              `json:"maxRetries"`
		MaxConcurrent    int              `json:"maxConcurrent"`
		Status           string           `json:"status"`
		Version          int              `json:"version"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := h.svc.SaveProvider(r.Context(), h.adminID(r), app.ProviderInput{
		Code: req.Code, Name: req.Name, AdapterType: req.AdapterType, BaseURL: req.BaseURL,
		AuthType: req.AuthType, APIKey: req.APIKey, Headers: req.Headers,
		TestPath: req.TestPath, ConnectTimeoutMS: req.ConnectTimeoutMS,
		RequestTimeoutMS: req.RequestTimeoutMS, IdleTimeoutMS: req.IdleTimeoutMS,
		MaxRetries: req.MaxRetries, MaxConcurrent: req.MaxConcurrent,
		Status: req.Status, Version: req.Version,
	}); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listModels(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.ListModels(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	bindings, err := h.svc.ModelBindings(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	type binding struct {
		Operation  string `json:"operation"`
		Enabled    bool   `json:"enabled"`
		EstSeconds *int   `json:"estSeconds,omitempty"`
	}
	type resp struct {
		Code              string `json:"code"`
		Name              string `json:"name"`
		ProviderCode      string `json:"providerCode"`
		Domain            string `json:"domain"`
		UpstreamModelCode string `json:"upstreamModelCode"`
		APIProtocol       string `json:"apiProtocol"`
		EstSeconds        int    `json:"estSeconds"`
		Status            string `json:"status"`
		// ⚠️ 下面几项此前不下发，而保存是**整条替换**：照着列表回填的编辑表单一保存，
		// 提交路径、查询路径、输入上限、排序就被清空 —— 模型从此调不通而配置页看不出哪里错。
		SubType       string `json:"subType"`
		SubmitPath    string `json:"submitPath"`
		QueryPath     string `json:"queryPath"`
		MaxInputChars *int   `json:"maxInputChars"`
		Sort          int    `json:"sort"`
		// MinMemberLevelID 会员门槛（等级 ID），空串 = 不限。
		MinMemberLevelID string `json:"minMemberLevelId"`
		// Operations 这个模型绑了哪些操作（含停用的）。
		Operations []binding `json:"operations"`
		// 视频能力（AGT-05）。maxDurationSec 为 null = 未配置；非视频模型恒为 null/false/false。
		MaxDurationSec *int `json:"maxDurationSec"`
		IsMultiShot    bool `json:"isMultiShot"`
		HasNativeAudio bool `json:"hasNativeAudio"`
		// AgentCapable 可作推理模型（AGT-04）。
		AgentCapable bool `json:"agentCapable"`
	}
	out := make([]resp, 0, len(list))
	for _, m := range list {
		ops := make([]binding, 0, len(bindings[m.Code]))
		for _, b := range bindings[m.Code] {
			ops = append(ops, binding{b.OperationCode, b.IsEnabled, b.EstSeconds})
		}
		level := ""
		if m.MinMemberLevelID != nil && *m.MinMemberLevelID > 0 {
			level = itoa64(*m.MinMemberLevelID)
		}
		out = append(out, resp{m.Code, m.Name, m.ProviderCode, m.Domain,
			m.UpstreamModelCode, m.APIProtocol, m.EstSeconds, m.Status,
			m.SubType, m.SubmitPath, m.QueryPath, m.MaxInputChars, m.Sort, level, ops,
			m.MaxDurationSec, m.IsMultiShot, m.HasNativeAudio, m.IsAgentCapable})
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) saveModel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code              string `json:"code"`
		Name              string `json:"name"`
		ProviderCode      string `json:"providerCode"`
		Domain            string `json:"domain"`
		SubType           string `json:"subType"`
		UpstreamModelCode string `json:"upstreamModelCode"`
		APIProtocol       string `json:"apiProtocol"`
		SubmitPath        string `json:"submitPath"`
		QueryPath         string `json:"queryPath"`
		EstSeconds        int    `json:"estSeconds"`
		MaxInputChars     *int   `json:"maxInputChars"`
		Status            string `json:"status"`
		Sort              int    `json:"sort"`
		// 三态：缺省 = 保留库里的值，"" = 清除，等级 ID = 设置（见 app.ModelInput）。
		MinMemberLevelID *string `json:"minMemberLevelId"`
		// 视频能力（AGT-05）：缺省 = 保留库里的值。maxDurationSec 显式传 null = 清除。
		MaxDurationSec optionalInt `json:"maxDurationSec"`
		IsMultiShot    *bool       `json:"isMultiShot"`
		HasNativeAudio *bool       `json:"hasNativeAudio"`
		// 支持 Agent（AGT-04）：缺省 = 保留库里的值。
		AgentCapable *bool `json:"agentCapable"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := h.svc.SaveModel(r.Context(), h.adminID(r), app.ModelInput{
		Code: req.Code, Name: req.Name, ProviderCode: req.ProviderCode,
		Domain: req.Domain, SubType: req.SubType, UpstreamModelCode: req.UpstreamModelCode,
		APIProtocol: req.APIProtocol, SubmitPath: req.SubmitPath, QueryPath: req.QueryPath,
		EstSeconds: req.EstSeconds, MaxInputChars: req.MaxInputChars,
		Status: req.Status, Sort: req.Sort, MinMemberLevel: req.MinMemberLevelID,
		MaxDurationSec: req.MaxDurationSec.Value, ClearMaxDurationSec: req.MaxDurationSec.Set && req.MaxDurationSec.Value == nil,
		IsMultiShot: req.IsMultiShot, HasNativeAudio: req.HasNativeAudio,
		IsAgentCapable: req.AgentCapable,
	}); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// publishable 返回阻塞原因列表。空列表即可发布（FE-07）。
func (h *Handler) publishable(w http.ResponseWriter, r *http.Request) {
	blockers, err := h.svc.Publishable(r.Context(), chi.URLParam(r, "code"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"publishable": len(blockers) == 0,
		"blockers":    blockers,
	})
}

func (h *Handler) saveModelOperation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Operation  string `json:"operation"`
		IsEnabled  bool   `json:"isEnabled"`
		EstSeconds *int   `json:"estSeconds"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := h.svc.SaveModelOperation(r.Context(), h.adminID(r),
		chi.URLParam(r, "code"), req.Operation, req.IsEnabled, req.EstSeconds); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listCapabilities(w http.ResponseWriter, r *http.Request) {
	caps, err := h.svc.Capabilities(r.Context(), chi.URLParam(r, "code"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	type capResp struct {
		Role        string   `json:"role"`
		MinCount    int      `json:"minCount"`
		MaxCount    int      `json:"maxCount"`
		AllowedMIME []string `json:"allowedMime,omitempty"`
	}
	out := map[string][]capResp{}
	for op, list := range caps {
		for _, c := range list {
			out[op] = append(out[op], capResp{string(c.Role), c.MinCount, c.MaxCount, c.AllowedMIME})
		}
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) saveCapability(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Operation   string   `json:"operation"`
		Role        string   `json:"role"`
		MinCount    int      `json:"minCount"`
		MaxCount    int      `json:"maxCount"`
		AllowedMIME []string `json:"allowedMime"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := h.svc.SaveCapability(r.Context(), h.adminID(r),
		chi.URLParam(r, "code"), req.Operation, domain.Capability{
			Role: domain.InputRole(req.Role), MinCount: req.MinCount,
			MaxCount: req.MaxCount, AllowedMIME: req.AllowedMIME,
		}); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) deleteCapability(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.DeleteCapability(r.Context(), h.adminID(r),
		chi.URLParam(r, "code"), chi.URLParam(r, "operation"), chi.URLParam(r, "role")); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// optionalInt 区分「没传」「传了 null」「传了数字」三态。
//
// ⚠️ 用 *int 的话「没传」与「null」解出来都是 nil：要么永远清不掉已配的时长，
// 要么不带这个字段的老调用方一保存就把它清掉 —— 两种都不报错。
// encoding/json 对实现了 Unmarshaler 的类型在遇到 null 时也会调用 UnmarshalJSON，靠这一点分出 Set。
type optionalInt struct {
	Set   bool
	Value *int
}

func (o *optionalInt) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Value = nil
		return nil
	}
	var v int
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	o.Value = &v
	return nil
}
