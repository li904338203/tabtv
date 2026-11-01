package transport

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/li904338203/tabtv/backend/internal/content/app"
	contentdomain "github.com/li904338203/tabtv/backend/internal/content/domain"
	"github.com/li904338203/tabtv/backend/internal/content/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

type AdminHandler struct {
	svc      *app.Service
	adminIDs func(r *http.Request) int64
}

func NewAdminHandler(svc *app.Service, adminIDs func(r *http.Request) int64) *AdminHandler {
	return &AdminHandler{svc: svc, adminIDs: adminIDs}
}

// MountAdmin 挂载运营接口（AD-02 首页运营、AD-03 消息公告）。
//
// 权限位走 seed 里早就有的 `content:read` / `content:write`，**不新造一对** ——
// 新造的话已有角色不会自动长出来，症状是运营 403 而看着像鉴权代码写错。
func (h *AdminHandler) MountAdmin(r chi.Router, requireAdmin func(http.Handler) http.Handler,
	requirePerm func(string) func(http.Handler) http.Handler) {
	// ⚠️ 按**资源**各开一个 `r.Route`，不要共用 `r.Route("/admin", ...)`：
	// chi 的 Route 在同一个 Mux 上重复挂载同一个前缀会 **panic**，
	// 而那时进程直接起不来（实测踩过：日志里只有一段 chi 的调用栈，
	// 看着像路由写错了）。本仓库其余管理端都是 `/admin/<资源>` 这种写法。
	r.Route("/admin/banners", func(r chi.Router) {
		r.Use(requireAdmin)
		r.With(requirePerm("content:read")).Get("/", h.listBanners)
		r.With(requirePerm("content:write")).Post("/", h.createBanner)
		r.With(requirePerm("content:write")).Put("/{id}", h.updateBanner)
		r.With(requirePerm("content:write")).Delete("/{id}", h.deleteBanner)
	})
	r.Route("/admin/announcements", func(r chi.Router) {
		r.Use(requireAdmin)
		r.With(requirePerm("content:read")).Get("/", h.listAnnouncements)
		r.With(requirePerm("content:write")).Post("/", h.createAnnouncement)
		r.With(requirePerm("content:write")).Put("/{id}", h.updateAnnouncement)
		// ⚠️ 发布与下线是**独立的动作**，不是 update 的一个字段。
		// 混进 update 的话，运营改个错字保存一下就等于给全站重新发了一次公告 ——
		// 而那个动作不可撤销。
		r.With(requirePerm("content:write")).Post("/{id}/publish", h.publishAnnouncement)
		r.With(requirePerm("content:write")).Post("/{id}/offline", h.offlineAnnouncement)
		r.With(requirePerm("content:write")).Delete("/{id}", h.deleteAnnouncement)
	})
}

func (h *AdminHandler) pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "id 非法"))
		return 0, false
	}
	return id, true
}

// ---------------------------------------------------------------- Banner

type bannerReq struct {
	Position      string  `json:"position"`
	Title         string  `json:"title"`
	MediaObjectID string  `json:"mediaObjectId"`
	CoverMediaID  string  `json:"coverMediaId"`
	LinkURL       string  `json:"linkUrl"`
	Sort          int     `json:"sort"`
	IsEnabled     bool    `json:"isEnabled"`
	StartAt       *string `json:"startAt"`
	EndAt         *string `json:"endAt"`
}

// toInput 解析请求体。
//
// ⚠️ 时间解析失败**明确报错**而不是当成没传。与后台订单检索那边相反 ——
// 那边少一个筛选条件顶多结果多一些，这边少一个排期端点意味着
// **一条本该定时下线的 Banner 会一直挂在首页上**。
func (r bannerReq) toInput(id int64) (app.BannerInput, error) {
	in := app.BannerInput{
		ID: id, Position: r.Position, Title: r.Title, LinkURL: r.LinkURL,
		Sort: r.Sort, IsEnabled: r.IsEnabled,
	}
	in.MediaObjectID, _ = strconv.ParseInt(r.MediaObjectID, 10, 64)
	in.CoverMediaID, _ = strconv.ParseInt(r.CoverMediaID, 10, 64)
	for _, p := range []struct {
		raw   *string
		dst   **time.Time
		field string
	}{{r.StartAt, &in.StartAt, "startAt"}, {r.EndAt, &in.EndAt, "endAt"}} {
		if p.raw == nil || *p.raw == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, *p.raw)
		if err != nil {
			return app.BannerInput{}, httpx.Errf(errcode.InvalidArgument,
				p.field+" 需要 RFC3339 时间")
		}
		*p.dst = &t
	}
	return in, nil
}

func bannerResp(b repo.Banner) map[string]any {
	out := map[string]any{
		"id": itoa(b.ID), "position": string(b.Position), "title": b.Title,
		"mediaObjectId": idOrEmpty(b.MediaObjectID),
		"coverMediaId":  idOrEmpty(b.CoverMediaID),
		"linkUrl":       b.LinkURL, "sort": b.Sort, "isEnabled": b.IsEnabled,
		"lifecycle": string(contentdomain.BannerSchedule{StartAt: b.StartAt, EndAt: b.EndAt}.
			Lifecycle(b.IsEnabled, time.Now())),
		"startAt": "", "endAt": "",
		"createdAt": b.CreatedAt.UTC().Format(time.RFC3339),
		"updatedAt": b.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if b.StartAt != nil {
		out["startAt"] = b.StartAt.UTC().Format(time.RFC3339)
	}
	if b.EndAt != nil {
		out["endAt"] = b.EndAt.UTC().Format(time.RFC3339)
	}
	return out
}

func (h *AdminHandler) listBanners(w http.ResponseWriter, r *http.Request) {
	// ⚠️ 管理端**不过滤排期**：运营要看得到没生效与已过期的。
	// 过滤掉的话，他排好的明天那条会从列表里消失，于是他会再配一条。
	list, err := h.svc.ListBanners(r.Context(), r.URL.Query().Get("position"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, b := range list {
		out = append(out, bannerResp(b))
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *AdminHandler) createBanner(w http.ResponseWriter, r *http.Request) {
	var req bannerReq
	if !decode(w, r, &req) {
		return
	}
	in, err := req.toInput(0)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out, err := h.svc.CreateBanner(r.Context(), h.adminIDs(r), in)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, bannerResp(out))
}

func (h *AdminHandler) updateBanner(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r)
	if !ok {
		return
	}
	var req bannerReq
	if !decode(w, r, &req) {
		return
	}
	in, err := req.toInput(id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out, err := h.svc.UpdateBanner(r.Context(), h.adminIDs(r), in)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, bannerResp(out))
}

func (h *AdminHandler) deleteBanner(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r)
	if !ok {
		return
	}
	if err := h.svc.DeleteBanner(r.Context(), h.adminIDs(r), id); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------- 公告

func annResp(a repo.Announcement) map[string]any {
	out := map[string]any{
		"id": itoa(a.ID), "title": a.Title, "content": a.Content,
		"status": string(a.Status), "linkUrl": a.LinkURL, "publishedAt": "",
		"createdAt": a.CreatedAt.UTC().Format(time.RFC3339),
		"updatedAt": a.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if a.PublishedAt != nil {
		out["publishedAt"] = a.PublishedAt.UTC().Format(time.RFC3339)
	}
	return out
}

func (h *AdminHandler) listAnnouncements(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.ListAnnouncements(r.Context(), r.URL.Query().Get("status"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, a := range list {
		out = append(out, annResp(a))
	}
	httpx.JSON(w, http.StatusOK, out)
}

type annReq struct {
	Title   string `json:"title"`
	Content string `json:"content"`
	LinkURL string `json:"linkUrl"`
}

func (h *AdminHandler) createAnnouncement(w http.ResponseWriter, r *http.Request) {
	var req annReq
	if !decode(w, r, &req) {
		return
	}
	out, err := h.svc.CreateAnnouncement(r.Context(), h.adminIDs(r), app.AnnouncementInput{
		Title: req.Title, Content: req.Content, LinkURL: req.LinkURL,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	// 新建一律落草稿 —— 响应里的 status 就说明了这一点，前端据此显示"发布"按钮。
	httpx.JSON(w, http.StatusCreated, annResp(out))
}

func (h *AdminHandler) updateAnnouncement(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r)
	if !ok {
		return
	}
	var req annReq
	if !decode(w, r, &req) {
		return
	}
	out, err := h.svc.UpdateAnnouncement(r.Context(), h.adminIDs(r), app.AnnouncementInput{
		ID: id, Title: req.Title, Content: req.Content, LinkURL: req.LinkURL,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, annResp(out))
}

func (h *AdminHandler) publishAnnouncement(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r)
	if !ok {
		return
	}
	out, err := h.svc.PublishAnnouncement(r.Context(), h.adminIDs(r), id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, annResp(out))
}

func (h *AdminHandler) offlineAnnouncement(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r)
	if !ok {
		return
	}
	out, err := h.svc.OfflineAnnouncement(r.Context(), h.adminIDs(r), id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, annResp(out))
}

func (h *AdminHandler) deleteAnnouncement(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r)
	if !ok {
		return
	}
	if err := h.svc.DeleteAnnouncement(r.Context(), h.adminIDs(r), id); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------- 作品审核（AD-09）

// MountReview 挂载后台审核。权限位走 seed 里早就有的 `review:read`/`review:write`。
//
// ⚠️ 与内容运营（`content:*`）**分开**：审核是"这东西能不能放出去"，
// 内容运营是"首页放什么"。合在一起的话，配 Banner 的人顺手就能通过审核。
func (h *AdminHandler) MountReview(r chi.Router, requireAdmin func(http.Handler) http.Handler,
	requirePerm func(string) func(http.Handler) http.Handler) {
	r.Route("/admin/works", func(r chi.Router) {
		r.Use(requireAdmin)
		r.With(requirePerm("review:read")).Get("/", h.listWorks)
		r.With(requirePerm("review:read")).Get("/{id}", h.workDetail)
		// ⚠️ 四个动作走**同一个端点**（body 里带 action）而不是四条路由：
		// 它们共享同一条"读状态 → 校验 → 算新状态 → 一个事务落地 → 通知"的路径，
		// 拆成四条的话那条路径会被抄四遍，而抄漏一次就是一处不写留痕。
		r.With(requirePerm("review:write")).Post("/{id}/review", h.review)
	})
}

func (h *AdminHandler) listWorks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := repo.AdminWorkFilter{Status: q.Get("status"), Limit: 20}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			f.Limit = n
		}
	}
	if v := q.Get("cursor"); v != "" {
		f.CursorID, _ = strconv.ParseInt(v, 10, 64)
	}
	if v := q.Get("featured"); v != "" {
		b := v == "true"
		f.Featured = &b
	}
	if v := q.Get("offline"); v != "" {
		b := v == "true"
		f.Offline = &b
	}
	list, err := h.svc.AdminWorks(r.Context(), f)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(list))
	for _, v := range list {
		item := workResp(v)
		// 后台要看得到归属，才能判断"这是谁的东西"。
		item["workspaceId"] = itoa(v.WorkspaceID)
		items = append(items, item)
	}
	next := ""
	if len(list) == f.Limit && len(list) > 0 {
		next = itoa(list[len(list)-1].ID)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": next})
}

func (h *AdminHandler) workDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r)
	if !ok {
		return
	}
	v, history, err := h.svc.WorkDetail(r.Context(), id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	hist := make([]map[string]any, 0, len(history))
	for _, rec := range history {
		hist = append(hist, map[string]any{
			"id": itoa(rec.ID), "action": rec.Action,
			"adminId": itoa(rec.AdminID), "reason": rec.Reason,
			"createdAt": rec.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	item := workResp(v)
	item["workspaceId"] = itoa(v.WorkspaceID)
	item["authorUserId"] = itoa(v.AuthorUserID)
	// ⚠️ **审核历史是这个页面的重点**（"后台可追踪发布原因"）：
	// 只看当前状态回答不了"它为什么现在是这样"—— 而那正是排查时要问的。
	item["history"] = hist
	httpx.JSON(w, http.StatusOK, item)
}

func (h *AdminHandler) review(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r)
	if !ok {
		return
	}
	var req struct {
		Action string `json:"action"`
		Reason string `json:"reason"`
	}
	if !decode(w, r, &req) {
		return
	}
	out, err := h.svc.Review(r.Context(), h.adminIDs(r), id, req.Action, req.Reason)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, workResp(out))
}

// MountOpsTasks 挂载后台任务运营台（P13-03；GAP-07）。
//
// ⚠️ 它在内容域，是因为「四类任务的统一读模型」本来就在这里
// （P13-02 的 union）。运维台是这个读模型的**第二个消费者**，不是第二份
// 实现 —— 另写一份的话新增一个状态词会漏掉一处，而漏掉的症状是
// **某一类任务在那个筛选下整体消失**（CD-62）。
//
// ⚠️ 处置动作（重投、补偿、重试）**不在这里**，在 `internal/ops`：
// 那些会改钱和状态，不属于内容域。
//
// ⚠️ 权限位走 `ops:read`：这条查询**跨全部工作区**，而 `content:read`
// 是给运营看首页配置的 —— 配 Banner 的人不该顺手就能翻全站的任务。
func (h *AdminHandler) MountOpsTasks(r chi.Router,
	requireAdmin func(http.Handler) http.Handler,
	requirePerm func(string) func(http.Handler) http.Handler) {
	r.Route("/admin/tasks", func(r chi.Router) {
		r.Use(requireAdmin, requirePerm("ops:read"))
		r.Get("/", h.opsTasks)
	})
}

func (h *AdminHandler) opsTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := repo.OpsTaskFilter{
		Kind:         contentdomain.TaskKind(q.Get("kind")),
		State:        contentdomain.TaskState(q.Get("state")),
		Keyword:      q.Get("keyword"),
		ProviderCode: q.Get("provider"),
		StuckOnly:    q.Get("stuck") == "true",
		Limit:        20,
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			f.Limit = n
		}
	}
	if v := q.Get("cursorAt"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.CursorAt = &t
		}
	}
	if v := q.Get("cursorId"); v != "" {
		f.CursorID, _ = strconv.ParseInt(v, 10, 64)
	}

	now := time.Now()
	list, summary, err := h.svc.OpsTasks(r.Context(), f, now)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(list))
	for _, t := range list {
		item := map[string]any{
			"id": itoa(t.ID), "kind": string(t.Kind), "state": string(t.State),
			"title": t.Title, "credits": t.Credits, "error": t.Error,
			"refId":           itoa(t.RefID),
			"workspaceId":     itoa(t.WorkspaceID),
			"createdByUserId": itoa(t.CreatedByUser),
			// ⚠️ **原始状态**也给：统一状态回答"给用户怎么显示"，
			// 原始状态回答"库里到底是什么"——排障问的是后者。
			"rawStatus": t.RawStatus,
			"attempt":   t.Attempt,
			"provider":  t.ProviderCode, "model": t.ModelCode,
			"errorCode": t.ErrorCode,
			// 上游原话只在运维台出现：用户侧只看到归类后的说法（platform/usererr）。
			"errorDetail": t.ErrorDetail,
			"createdAt":   t.CreatedAt.UTC().Format(time.RFC3339),
			"deadlineAt":  "", "finishedAt": "",
			// 「本该结束却没结束」—— 运维台上最该盯的那一位。
			// ⚠️ 服务端算，不让前端拿 deadline 去跟本地时钟比：
			// 用户机器的时钟偏几分钟，这一列就会整片显示错。
			"stuck": isStuck(t, now),
		}
		if t.PayerAccountID != 0 {
			item["payerAccountId"] = itoa(t.PayerAccountID)
		}
		if t.DeadlineAt != nil {
			item["deadlineAt"] = t.DeadlineAt.UTC().Format(time.RFC3339)
		}
		if t.FinishedAt != nil {
			item["finishedAt"] = t.FinishedAt.UTC().Format(time.RFC3339)
		}
		items = append(items, item)
	}
	byState := map[string]int{}
	for k, v := range summary.ByState {
		byState[string(k)] = v
	}
	resp := map[string]any{
		"items": items, "counts": byState, "stuck": summary.Stuck,
		"nextCursorAt": "", "nextCursorId": "",
	}
	if len(list) == f.Limit && len(list) > 0 {
		last := list[len(list)-1]
		resp["nextCursorAt"] = last.CreatedAt.UTC().Format(time.RFC3339)
		resp["nextCursorId"] = itoa(last.ID)
	}
	httpx.JSON(w, http.StatusOK, resp)
}

func isStuck(t repo.OpsTask, now time.Time) bool {
	if t.State != contentdomain.StateQueued && t.State != contentdomain.StateRunning {
		return false
	}
	return t.DeadlineAt != nil && t.DeadlineAt.Before(now)
}
