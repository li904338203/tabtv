package transport

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/li904338203/tabtv/backend/internal/identity/app"
	"github.com/li904338203/tabtv/backend/internal/identity/domain"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

func (h *Handler) listAdmins(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.ListAdmins(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	// ⚠️ 此前不带创建与最近登录时间：管理端「创建时间」一列永远是「—」（P15-05 发现）。
	type resp struct {
		ID          string   `json:"id"`
		Username    string   `json:"username"`
		Status      string   `json:"status"`
		Roles       []string `json:"roles"`
		CreatedAt   string   `json:"createdAt"`
		LastLoginAt string   `json:"lastLoginAt,omitempty"`
	}
	out := make([]resp, 0, len(list))
	for _, a := range list {
		last := ""
		if a.LastLoginAt != nil {
			last = a.LastLoginAt.UTC().Format(time.RFC3339)
		}
		out = append(out, resp{itoa(a.ID), a.Username, a.Status, a.Roles, a.CreatedAt.UTC().Format(time.RFC3339), last})
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) createAdmin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string   `json:"username"`
		Password string   `json:"password"`
		Roles    []string `json:"roles"`
	}
	if !decode(w, r, &req) {
		return
	}
	operator, _ := ClaimsFrom(r.Context()).SubjectID()
	id, err := h.svc.CreateAdmin(r.Context(), operator, app.CreateAdminInput{
		Username: req.Username, Password: req.Password, Roles: req.Roles,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]string{"id": itoa(id)})
}

func (h *Handler) setAdminStatus(w http.ResponseWriter, r *http.Request) {
	adminID, err := pathID(r, "adminID")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	var req struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if !decode(w, r, &req) {
		return
	}
	operator, _ := ClaimsFrom(r.Context()).SubjectID()
	if err := h.svc.SetAdminStatus(r.Context(), operator, adminID, req.Status, req.Reason); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) setAdminRoles(w http.ResponseWriter, r *http.Request) {
	adminID, err := pathID(r, "adminID")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	var req struct {
		Roles []string `json:"roles"`
	}
	if !decode(w, r, &req) {
		return
	}
	operator, _ := ClaimsFrom(r.Context()).SubjectID()
	if err := h.svc.SetAdminRoles(r.Context(), operator, adminID, req.Roles); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listRoles(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.ListRoles(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	type resp struct {
		Code        string   `json:"code"`
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Permissions []string `json:"permissions"`
		// Editable=false 的角色前端应禁用编辑，但后端仍会独立拦截。
		Editable bool `json:"editable"`
	}
	out := make([]resp, 0, len(list))
	for _, x := range list {
		out = append(out, resp{x.Code, x.Name, x.Description, x.Permissions, x.Code != "super_admin"})
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) listPermissions(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.ListPermissions(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	type resp struct {
		Code   string `json:"code"`
		Name   string `json:"name"`
		Module string `json:"module"`
	}
	out := make([]resp, 0, len(list))
	for _, p := range list {
		out = append(out, resp{p.Code, p.Name, p.Module})
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) setRolePermissions(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "roleCode")
	var req struct {
		Permissions []string `json:"permissions"`
	}
	if !decode(w, r, &req) {
		return
	}
	operator, _ := ClaimsFrom(r.Context()).SubjectID()
	if err := h.svc.SetRolePermissions(r.Context(), operator, code, req.Permissions); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) searchUsers(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := h.svc.SearchUsers(r.Context(),
		r.URL.Query().Get("keyword"), r.URL.Query().Get("status"), limit)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	// ⚠️ 字段以契约 `AdminUser` 为准。此前少了 isAiWatermarkOff 与 createdAt ——
	// 管理端的「AI 水印」列于是对**所有人**都显示「开启」（缺字段被当成 false），
	// 而那正是 P13-05 要让后台看清的一件事（P15-05 发现）。
	type resp struct {
		ID               string `json:"id"`
		Nickname         string `json:"nickname"`
		Phone            string `json:"phone,omitempty"`
		Email            string `json:"email,omitempty"`
		Status           string `json:"status"`
		IsAiWatermarkOff bool   `json:"isAiWatermarkOff"`
		CreatedAt        string `json:"createdAt"`
	}
	out := make([]resp, 0, len(list))
	for _, u := range list {
		// ⚠️ 账号一律脱敏：管理端列表同样是个人信息面。
		out = append(out, resp{
			ID: itoa(u.ID), Nickname: u.Nickname, Status: u.Status,
			Phone:            maskIfSet(u.Phone, domain.MaskPhone),
			Email:            maskIfSet(u.Email, domain.MaskEmail),
			IsAiWatermarkOff: u.WatermarkOff,
			CreatedAt:        u.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) getAuthSetting(w http.ResponseWriter, r *http.Request) {
	s, err := h.svc.GetAuthSetting(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, s)
}

func (h *Handler) updateAuthSetting(w http.ResponseWriter, r *http.Request) {
	var req app.AuthSettingUpdate
	if !decode(w, r, &req) {
		return
	}
	s, err := h.svc.UpdateAuthSetting(r.Context(), req)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	operator, _ := ClaimsFrom(r.Context()).SubjectID()
	// 配置发布是高影响操作，写审计但**不记录任何密钥内容**。
	h.svc.AuditConfigChange(r.Context(), operator, s.Version)
	httpx.JSON(w, http.StatusOK, s)
}

func (h *Handler) auditLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	objectID, _ := strconv.ParseInt(q.Get("objectId"), 10, 64)
	actorID, _ := strconv.ParseInt(q.Get("actorId"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	cursor, _ := strconv.ParseInt(q.Get("cursor"), 10, 64)
	// ⚠️ 时间参数解析失败**当成没传**而不是报错：为一个格式问题把整个
	// 审计列表挡掉不值（同后台订单检索，CD-56）。
	var from, to time.Time
	if v, err := time.Parse(time.RFC3339, q.Get("from")); err == nil {
		from = v
	}
	if v, err := time.Parse(time.RFC3339, q.Get("to")); err == nil {
		to = v
	}
	list, err := h.svc.AuditLog(r.Context(), audit.Query{
		ObjectType: q.Get("objectType"), ObjectID: objectID,
		ActorType: q.Get("actorType"), ActorID: actorID,
		Action: q.Get("action"), From: from, To: to,
		CursorID: cursor, Limit: limit,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	next := ""
	if limit > 0 && len(list) == limit && len(list) > 0 {
		next = list[len(list)-1].ID
	}
	// ⚠️ 从裸数组改成带游标的对象：没有游标的话「翻到上个月」只能靠
	// 一直调大 limit，而那会把整张表拖出来。
	httpx.JSON(w, http.StatusOK, map[string]any{
		"items": list, "nextCursor": next,
	})
}

// profile 是用户端接口，不在 admin 路由下。
func (h *Handler) updateProfile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Nickname         string `json:"nickname"`
		IsAiWatermarkOff *bool  `json:"isAiWatermarkOff"`
	}
	if !decode(w, r, &req) {
		return
	}
	uid, ok := UserIDFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.Unauthenticated))
		return
	}
	if err := h.svc.UpdateProfile(r.Context(), uid, app.ProfileInput{
		Nickname: req.Nickname, WatermarkOff: req.IsAiWatermarkOff,
	}); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
