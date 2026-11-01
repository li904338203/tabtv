// Package transport 是整图 DAG 的 HTTP 层（薄：仅校验 + 组装）。
package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	collabdomain "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	collabtransport "github.com/li904338203/tabtv/backend/internal/collaboration/transport"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
	"github.com/li904338203/tabtv/backend/internal/workflow/app"
	"github.com/li904338203/tabtv/backend/internal/workflow/domain"
	"github.com/li904338203/tabtv/backend/internal/workflow/repo"
)

// contextT 只是让 nodeAction 的签名短一点。
type contextT = context.Context

type Handler struct {
	svc *app.Service
	uid func(*http.Request) int64
	// memberLevel 解析当前付费主体的会员等级（P11-01 接入）。
	// 不接的话恒为 0，整图里那些有等级门槛的模型会在**派发时**才失败 ——
	// 而那时前面几层已经跑完、钱已经花了。
	memberLevel func(*http.Request) int64
}

func New(svc *app.Service, uid func(*http.Request) int64) *Handler {
	return &Handler{svc: svc, uid: uid,
		memberLevel: func(*http.Request) int64 { return 0 }}
}

// SetMemberLevelResolver 注入会员等级解析器（P11-01 接入）。
func (h *Handler) SetMemberLevelResolver(f func(*http.Request) int64) {
	if f != nil {
		h.memberLevel = f
	}
}

// Mount 挂载路由。
//
// ⚠️ `user` 与 `workspace` **两个中间件都要**：工作区中间件是在用户上下文
// 之上校验成员资格的，少了前者它取不到 userId，一律 401 ——
// 而那看起来像"token 过期了"。
// Mount 需要两个工作区中间件（P12-03）：
//   - `guard` = 装上下文 + **按 HTTP 方法推断的默认动作闸**（GET→view，其余→edit）；
//   - `wsCtx` = 只装上下文，供**例外路由**自带动作闸。
//
// ⚠️ 例外必须放进**独立分组**，不能用 `r.With(RequireAction(...))` 去"放宽"：
// chi 的 `r.With` 是**追加**在组中间件之后的 —— 默认闸先跑就已经拒了，
// 后面那个更宽松的闸根本没机会执行。实测踩过（E2E 第 149 步第一次跑红）。
func (h *Handler) Mount(r chi.Router, user, guard, wsCtx func(http.Handler) http.Handler) {
	// ⚠️ 这一组**只放只读接口**。运行前预览是真的只读（NEW-01：不建 run、
	// 不扣费、不发队列，可反复点）—— 按方法推断出的 edit 会把只读成员挡在
	// 一个免费且无副作用的接口外面，而他连"这张图要花多少钱"都看不到。
	// 往这一组里加写路由的话，那条路由就只需要 view 权限了。
	r.Group(func(r chi.Router) {
		r.Use(user, wsCtx, collabtransport.RequireAction(collabdomain.ActionView))
		r.Post("/projects/{projectID}/workflow/plan", h.plan)
	})
	r.Group(func(r chi.Router) {
		r.Use(user, guard)
		// 跑整张图会逐节点扣费 —— 与发起单个生成同一档。
		r.With(collabtransport.RequireAction(collabdomain.ActionGenerate)).
			Post("/projects/{projectID}/workflow/runs", h.start)
		// 重试一个节点＝重新受理、重新扣费（幂等键随 attempt 变）。
		r.With(collabtransport.RequireAction(collabdomain.ActionGenerate)).
			Post("/workflow/runs/{runID}/nodes/{nodeID}/retry", h.retryNode)
		r.Get("/projects/{projectID}/workflow/runs", h.history)
		r.Get("/workflow/runs/{runID}", h.run)
		// 跳过与取消都是**停掉**已有的运行，不花钱 —— 默认的 edit 即可。
		r.Post("/workflow/runs/{runID}/nodes/{nodeID}/skip", h.skipNode)
		r.Post("/workflow/runs/{runID}/cancel", h.cancel)
	})
}

func (h *Handler) plan(w http.ResponseWriter, r *http.Request) {
	ws, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
		return
	}
	projectID, ok2 := pathID(w, r, "projectID")
	if !ok2 {
		return
	}
	var req struct {
		TargetNodeIDs []string `json:"targetNodeIds"`
		Force         bool     `json:"force"`
	}
	if r.Body != nil {
		// 空 body 合法：整图预览不需要任何参数。
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	plan, err := h.svc.Plan(r.Context(), app.PlanInput{
		WorkspaceID: ws.WorkspaceID, ProjectID: projectID,
		TargetNodeIDs: req.TargetNodeIDs, Force: req.Force,
		MemberLevelID: h.memberLevel(r),
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toPlan(plan))
}

func toPlan(p domain.Plan) map[string]any {
	issues := make([]map[string]any, 0, len(p.Issues))
	for _, i := range p.Issues {
		issues = append(issues, map[string]any{
			"code": string(i.Code), "nodes": orEmpty(i.Nodes), "message": i.Message,
		})
	}
	nodes := make([]map[string]any, 0, len(p.Nodes))
	for _, n := range p.Nodes {
		m := map[string]any{
			"nodeId": n.ID, "kind": string(n.Kind), "action": string(n.Action),
			"reason": n.Reason, "level": n.Level, "credits": n.Credits,
			"estimatedSeconds": n.EstimatedSeconds,
			"dependsOn":        orEmpty(n.DependsOn),
		}
		if n.Operation != "" {
			m["operation"] = n.Operation
		}
		if n.ModelCode != "" {
			m["modelCode"] = n.ModelCode
		}
		if len(n.Blockers) > 0 {
			m["blockers"] = n.Blockers
		}
		nodes = append(nodes, m)
	}
	levels := make([][]string, 0, len(p.Levels))
	for _, l := range p.Levels {
		levels = append(levels, orEmpty(l))
	}
	return map[string]any{
		"executable": p.Executable(),
		"summary":    p.Summary(),
		"issues":     issues,
		"levels":     levels,
		"nodes":      nodes,
		"runCount":   p.RunCount, "reuseCount": p.ReuseCount,
		"skipCount": p.SkipCount, "blockedCount": p.BlockedCount,
		"totalCredits": p.TotalCredits, "estimatedSeconds": p.EstimatedSeconds,
	}
}

// orEmpty 让空列表序列化成 `[]` 而不是 `null`。
//
// ⚠️ `null` 与 `[]` 在前端不是一回事：前者会让 `.map()` 直接抛异常，
// 而这类字段十有八九是空的（没有问题、没有依赖）。
func orEmpty(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// ---------------------------------------------------------------- 运行

func (h *Handler) start(w http.ResponseWriter, r *http.Request) {
	ws, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
		return
	}
	projectID, ok2 := pathID(w, r, "projectID")
	if !ok2 {
		return
	}
	var req struct {
		TargetNodeIDs []string `json:"targetNodeIds"`
		Force         bool     `json:"force"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	out, err := h.svc.Start(r.Context(), app.StartInput{
		WorkspaceID: ws.WorkspaceID, PayerAccountID: ws.PayerAccountID,
		UserID: h.uid(r), ProjectID: projectID,
		TargetNodeIDs: req.TargetNodeIDs, Force: req.Force,
		MemberLevelID: h.memberLevel(r),
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toRun(out))
}

func (h *Handler) run(w http.ResponseWriter, r *http.Request) {
	ws, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
		return
	}
	runID, ok2 := pathID(w, r, "runID")
	if !ok2 {
		return
	}
	out, err := h.svc.Run(r.Context(), ws.WorkspaceID, runID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toRun(out))
}

func (h *Handler) history(w http.ResponseWriter, r *http.Request) {
	ws, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
		return
	}
	projectID, ok2 := pathID(w, r, "projectID")
	if !ok2 {
		return
	}
	limit, _ := strconv.ParseInt(r.URL.Query().Get("limit"), 10, 64)
	runs, err := h.svc.History(r.Context(), ws.WorkspaceID, projectID, limit)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(runs))
	for _, v := range runs {
		out = append(out, map[string]any{
			"id": strconv.FormatInt(v.ID, 10), "status": v.Status,
			"totalCredits": v.TotalCredits,
		})
	}
	httpx.JSON(w, http.StatusOK, out)
}

func toRun(v app.RunView) map[string]any {
	nodes := make([]map[string]any, 0, len(v.Nodes))
	for _, n := range v.Nodes {
		m := map[string]any{
			"nodeId": n.NodeID, "status": n.Status, "attempt": n.Attempt,
			"creditsCost": n.CreditsCost, "isReused": n.IsReused,
		}
		if n.GenerationID != 0 {
			m["generationId"] = strconv.FormatInt(n.GenerationID, 10)
		}
		if n.ErrorCode != "" {
			m["errorCode"] = n.ErrorCode
		}
		if n.ErrorMessage != "" {
			m["errorMessage"] = n.ErrorMessage
		}
		nodes = append(nodes, m)
	}
	cost := domain.AggregateCost(costNodes(v.Nodes))
	failures := make([]map[string]any, 0, len(cost.Failures))
	for _, f := range cost.Failures {
		m := map[string]any{"nodeId": f.NodeID, "message": f.Message}
		if f.Code != "" {
			m["code"] = f.Code
		}
		failures = append(failures, m)
	}
	return map[string]any{
		"id":        strconv.FormatInt(v.Run.ID, 10),
		"projectId": strconv.FormatInt(v.Run.ProjectID, 10),
		"status":    v.Run.Status, "totalCredits": v.Run.TotalCredits,
		"nodes": nodes,
		// 逐项对账：花掉的与退回的分开 —— 从花掉的里减掉会让用户
		// 对不上账本流水（NEW-06）。
		"cost": map[string]any{
			"spent": cost.Spent, "refunded": cost.Refunded, "reused": cost.Reused,
			"succeeded": cost.Succeeded, "failed": cost.Failed, "running": cost.Running,
			"failures": failures,
		},
	}
}

func costNodes(rows []repo.NodeRun) []domain.CostNode {
	out := make([]domain.CostNode, 0, len(rows))
	for _, n := range rows {
		out = append(out, domain.CostNode{
			NodeID: n.NodeID, Status: domain.NodeStatus(n.Status),
			CreditsCost: n.CreditsCost, IsReused: n.IsReused,
			ErrorCode: n.ErrorCode, ErrorMessage: n.ErrorMessage,
		})
	}
	return out
}

func pathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, name), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, name+" 非法"))
		return 0, false
	}
	return id, true
}

func (h *Handler) retryNode(w http.ResponseWriter, r *http.Request) {
	h.nodeAction(w, r, h.svc.RetryNode)
}

func (h *Handler) skipNode(w http.ResponseWriter, r *http.Request) {
	h.nodeAction(w, r, h.svc.SkipNode)
}

func (h *Handler) nodeAction(w http.ResponseWriter, r *http.Request,
	fn func(ctx contextT, ws, runID int64, nodeID string) (app.RunView, error)) {
	ws, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
		return
	}
	runID, ok2 := pathID(w, r, "runID")
	if !ok2 {
		return
	}
	out, err := fn(r.Context(), ws.WorkspaceID, runID, chi.URLParam(r, "nodeID"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toRun(out))
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	ws, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
		return
	}
	runID, ok2 := pathID(w, r, "runID")
	if !ok2 {
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	if req.Reason == "" {
		req.Reason = "用户取消整图运行"
	}
	out, err := h.svc.Cancel(r.Context(), ws.WorkspaceID, runID, req.Reason)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toRun(out))
}
