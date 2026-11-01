// Package transport 暴露任务运维的管理端接口（P13-03）。
package transport

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/li904338203/tabtv/backend/internal/ops/app"
	"github.com/li904338203/tabtv/backend/internal/ops/domain"
	"github.com/li904338203/tabtv/backend/internal/ops/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

type Handler struct {
	svc      *app.Service
	adminIDs func(r *http.Request) int64
}

func NewHandler(svc *app.Service, adminIDs func(r *http.Request) int64) *Handler {
	return &Handler{svc: svc, adminIDs: adminIDs}
}

// Mount 挂载运维接口。
//
// ⚠️ 权限位走 seed 里早就有的 `ops:read` / `ops:write`，**不新造一对** ——
// 新造的话已有角色不会自动长出这个权限，症状是运维 403 而看着像鉴权
// 代码写错（CD-53 记过一次）。
//
// ⚠️ 按**资源**各开一个 `r.Route`：chi 在同一个 Mux 上重复挂载同一前缀
// 会 panic，进程直接起不来（基线 §2.90）。
func (h *Handler) Mount(r chi.Router, requireAdmin func(http.Handler) http.Handler,
	requirePerm func(string) func(http.Handler) http.Handler) {
	r.Route("/admin/dead-letters", func(r chi.Router) {
		r.Use(requireAdmin)
		r.With(requirePerm("ops:read")).Get("/", h.listDead)
		r.With(requirePerm("ops:write")).Post("/{id}/replay", h.replayDead)
		r.With(requirePerm("ops:write")).Post("/{id}/discard", h.discardDead)
	})
	r.Route("/admin/ops", func(r chi.Router) {
		r.Use(requireAdmin)
		// ⚠️ 补偿与重试都**改钱**，一律 `ops:write`。
		r.With(requirePerm("ops:write")).Post("/tasks/{kind}/{id}/compensate", h.compensate)
		r.With(requirePerm("ops:write")).Post("/tasks/generation/{id}/retry", h.retry)
	})
	r.Route("/admin/projects", func(r chi.Router) {
		r.Use(requireAdmin)
		// ⚠️ 项目查看走 `ops:read` 而不是 `user:read`：它会吐出**画布原文**，
		// 那是用户的创作内容。能看用户资料的人不该顺手就能翻他的画布。
		r.With(requirePerm("ops:read")).Get("/", h.listProjects)
		r.With(requirePerm("ops:read")).Get("/{id}", h.projectDetail)
		r.With(requirePerm("ops:write")).Post("/{id}/actions", h.projectAction)
	})
}

// ---------------------------------------------------------------- 死信

func deadResp(d repo.DeadLetter) map[string]any {
	out := map[string]any{
		"id": itoa(d.ID), "queue": d.Queue, "routingKey": d.RoutingKey,
		"aggregateType": d.AggregateType, "reason": d.Reason,
		"status": string(d.Status), "replayCount": d.ReplayCount,
		"note": d.Note, "handledAt": "",
		"createdAt": d.CreatedAt.UTC().Format(time.RFC3339),
		// ⚠️ payload 截断后下发：一条死信的消息体可能是几 MB 的内联 base64，
		// 整个列表带上它会把管理端卡死。要看全文走详情（这里给出长度让人
		// 知道后面还有多少 —— 只截不说的话运维会以为消息就这么长）。
		"payloadPreview": preview(d.Payload),
		"payloadLength":  len([]rune(d.Payload)),
	}
	if d.AggregateID != 0 {
		out["aggregateId"] = itoa(d.AggregateID)
	} else {
		// ⚠️ 空串不是"没有这个字段"：解不开的消息**本来就没有**业务引用，
		// 而那恰恰是运维要看到的信号。
		out["aggregateId"] = ""
	}
	if d.HandledBy != 0 {
		out["handledByAdminId"] = itoa(d.HandledBy)
	}
	if d.HandledAt != nil {
		out["handledAt"] = d.HandledAt.UTC().Format(time.RFC3339)
	}
	return out
}

const previewRunes = 2000

func preview(s string) string {
	r := []rune(s)
	if len(r) <= previewRunes {
		return s
	}
	return string(r[:previewRunes]) + "…"
}

func (h *Handler) listDead(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := repo.DeadFilter{
		Status: domain.DeadStatus(q.Get("status")),
		Queue:  q.Get("queue"),
		Limit:  20,
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			f.Limit = n
		}
	}
	if v := q.Get("cursor"); v != "" {
		f.CursorID, _ = strconv.ParseInt(v, 10, 64)
	}
	list, pending, err := h.svc.ListDead(r.Context(), f)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(list))
	for _, d := range list {
		items = append(items, deadResp(d))
	}
	next := ""
	if len(list) == f.Limit && len(list) > 0 {
		next = itoa(list[len(list)-1].ID)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"items": items, "nextCursor": next,
		// 待处理条数与筛选无关：它是运维台顶上那个"还有几条要管"的数。
		"pending": pending,
	})
}

func (h *Handler) replayDead(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	if !decode(w, r, &req) {
		return
	}
	out, err := h.svc.ReplayDead(r.Context(), h.adminIDs(r), id, req.Note)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, deadResp(out))
}

func (h *Handler) discardDead(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	if !decode(w, r, &req) {
		return
	}
	out, err := h.svc.DiscardDead(r.Context(), h.adminIDs(r), id, req.Note)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, deadResp(out))
}

// ---------------------------------------------------------------- 补偿与重试

func (h *Handler) compensate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var req struct {
		Reason string `json:"reason"`
		Force  bool   `json:"force"`
	}
	if !decode(w, r, &req) {
		return
	}
	err := h.svc.Compensate(r.Context(), app.CompensateInput{
		Kind:    domain.TaskKind(chi.URLParam(r, "kind")),
		TaskID:  id,
		Reason:  req.Reason,
		Force:   req.Force,
		AdminID: h.adminIDs(r),
	}, time.Now())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) retry(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if !decode(w, r, &req) {
		return
	}
	out, err := h.svc.Retry(r.Context(), h.adminIDs(r), id, req.Reason)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{
		"newTaskId": itoa(out.NewTaskID),
		// ⚠️ 把扣了多少积分**明说**：受控重试是重新扣费的，
		// 不说的话运维会以为后台重试是免费的（而那笔钱记在用户账上）。
		"credits": out.Credits,
	})
}

// ---------------------------------------------------------------- 项目

func projectResp(p repo.ProjectRow) map[string]any {
	out := map[string]any{
		"id": itoa(p.ID), "workspaceId": itoa(p.WorkspaceID),
		"workspaceName": p.WorkspaceName, "ownerUserId": itoa(p.OwnerUserID),
		"ownerName": p.OwnerName, "name": p.Name, "status": p.Status,
		"version": p.Version, "nodeCount": p.NodeCount, "edgeCount": p.EdgeCount,
		"generationCount": p.GenCount, "deletedAt": "",
		"updatedAt": p.UpdatedAt.UTC().Format(time.RFC3339),
		"createdAt": p.CreatedAt.UTC().Format(time.RFC3339),
	}
	if p.DeletedAt != nil {
		out["deletedAt"] = p.DeletedAt.UTC().Format(time.RFC3339)
	}
	return out
}

func (h *Handler) listProjects(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := repo.ProjectFilter{
		Keyword: q.Get("keyword"), Name: q.Get("name"),
		Status: q.Get("status"), Limit: 20,
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			f.Limit = n
		}
	}
	if v := q.Get("cursor"); v != "" {
		f.CursorID, _ = strconv.ParseInt(v, 10, 64)
	}
	list, err := h.svc.ListProjects(r.Context(), f)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(list))
	for _, p := range list {
		items = append(items, projectResp(p))
	}
	next := ""
	if len(list) == f.Limit && len(list) > 0 {
		next = itoa(list[len(list)-1].ID)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": next})
}

func (h *Handler) projectDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	d, err := h.svc.ProjectDetail(r.Context(), id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := projectResp(d.Project)
	// ⚠️ 画布**原文**给出去：排障要看的恰恰是"库里到底存着什么"。
	out["canvasData"] = json.RawMessage(d.Canvas)
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) projectAction(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var req struct {
		Action string `json:"action"`
		Reason string `json:"reason"`
		Name   string `json:"name"`
		// Origin 只为拒绝而声明（MKD-3）：项目来源建后不可改，后台也不例外。
		// 不声明的话 Go 解码静默忽略它，请求 200 而什么都没改。
		Origin json.RawMessage `json:"origin"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Origin != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "origin 建后不可修改"))
		return
	}
	out, err := h.svc.ActOnProject(r.Context(), h.adminIDs(r), id,
		app.ProjectAction(req.Action), req.Reason, req.Name)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, projectResp(out))
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

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(dst); err != nil {
		httpx.Fail(w, r, httpx.Err(errcode.InvalidJSON))
		return false
	}
	return true
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
