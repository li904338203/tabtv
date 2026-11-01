package transport

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	billingapp "github.com/li904338203/tabtv/backend/internal/billing/app"
	"github.com/li904338203/tabtv/backend/internal/billing/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 商品管理（P11-01）：会员等级 / 套餐 / 积分包。
//
// ⚠️ 商品是**配置不是代码**：改价、调赠分、上下架都不该发版
// （与图片预设、提示词模板、渲染水印同一条）。

// AdminHandler 商品管理接口。
type AdminHandler struct {
	svc     *billingapp.Service
	adminID func(*http.Request) int64
}

func NewAdminHandler(svc *billingapp.Service,
	adminID func(*http.Request) int64) *AdminHandler {
	return &AdminHandler{svc: svc, adminID: adminID}
}

// MountAdmin 挂载商品管理。
//
// ⚠️ 走 `member:read` / `member:write` 而不是 `system:*`：
// 改商品价格是**运营**的日常工作，而 `system:*` 是给运维的 ——
// 合在一起的话，要么运营拿到了改系统配置的权限，要么运营改不了价。
// 这两个权限位 seed 里早就有，不要新造一对（新造的话已有角色不会自动长出来，
// 症状是运营 403 而看着像鉴权代码写错，基线里记过这条）。
func (h *AdminHandler) MountAdmin(r chi.Router, requireAdmin func(http.Handler) http.Handler,
	requirePerm func(string) func(http.Handler) http.Handler) {
	r.Route("/admin/billing", func(r chi.Router) {
		r.Use(requireAdmin)
		r.With(requirePerm("member:read")).Get("/catalog", h.catalog)
		r.With(requirePerm("member:write")).Put("/levels", h.saveLevel)
		r.With(requirePerm("member:write")).Put("/plans", h.savePlan)
		r.With(requirePerm("member:write")).Put("/packages", h.savePackage)
	})
}

// catalog 一次拿齐三张表 —— 商品管理页本来就要同时看等级与它的套餐。
func (h *AdminHandler) catalog(w http.ResponseWriter, r *http.Request) {
	levels, plans, pkgs, err := h.svc.AdminCatalog(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	lv := make([]map[string]any, 0, len(levels))
	for _, l := range levels {
		lv = append(lv, map[string]any{
			"id": strconv.FormatInt(l.ID, 10), "code": l.Code, "name": l.Name,
			"sort": l.Sort, "isEnabled": l.IsEnabled,
		})
	}
	pl := make([]map[string]any, 0, len(plans))
	for _, p := range plans {
		pl = append(pl, map[string]any{
			"id": strconv.FormatInt(p.ID, 10), "levelId": strconv.FormatInt(p.LevelID, 10),
			"levelCode": p.LevelCode, "levelName": p.LevelName,
			"period": p.Period, "priceCents": p.PriceCents,
			"giftCredits": p.GiftCredits, "sort": p.Sort, "isEnabled": p.IsEnabled,
		})
	}
	pk := make([]map[string]any, 0, len(pkgs))
	for _, p := range pkgs {
		pk = append(pk, map[string]any{
			"id": strconv.FormatInt(p.ID, 10), "code": p.Code, "name": p.Name,
			"credits": p.Credits, "priceCents": p.PriceCents,
			"sort": p.Sort, "isEnabled": p.IsEnabled,
		})
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"levels": lv, "plans": pl, "packages": pk,
	})
}

func (h *AdminHandler) saveLevel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code      string `json:"code"`
		Name      string `json:"name"`
		Sort      int    `json:"sort"`
		IsEnabled *bool  `json:"isEnabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Fail(w, r, httpx.Err(errcode.InvalidJSON))
		return
	}
	if err := h.svc.SaveLevel(r.Context(), h.adminID(r), repo.Level{
		Code: req.Code, Name: req.Name, Sort: req.Sort,
		IsEnabled: req.IsEnabled == nil || *req.IsEnabled,
	}); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	h.catalog(w, r)
}

func (h *AdminHandler) savePlan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LevelID     string `json:"levelId"`
		Period      string `json:"period"`
		PriceCents  int64  `json:"priceCents"`
		GiftCredits int    `json:"giftCredits"`
		Sort        int    `json:"sort"`
		IsEnabled   *bool  `json:"isEnabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Fail(w, r, httpx.Err(errcode.InvalidJSON))
		return
	}
	levelID, _ := strconv.ParseInt(req.LevelID, 10, 64)
	if err := h.svc.SavePlan(r.Context(), h.adminID(r), repo.Plan{
		LevelID: levelID, Period: req.Period, PriceCents: req.PriceCents,
		GiftCredits: req.GiftCredits, Sort: req.Sort,
		IsEnabled: req.IsEnabled == nil || *req.IsEnabled,
	}); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	h.catalog(w, r)
}

func (h *AdminHandler) savePackage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code       string `json:"code"`
		Name       string `json:"name"`
		Credits    int    `json:"credits"`
		PriceCents int64  `json:"priceCents"`
		Sort       int    `json:"sort"`
		IsEnabled  *bool  `json:"isEnabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Fail(w, r, httpx.Err(errcode.InvalidJSON))
		return
	}
	if err := h.svc.SavePackage(r.Context(), h.adminID(r), repo.Package{
		Code: req.Code, Name: req.Name, Credits: req.Credits,
		PriceCents: req.PriceCents, Sort: req.Sort,
		IsEnabled: req.IsEnabled == nil || *req.IsEnabled,
	}); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	h.catalog(w, r)
}

// ---------------------------------------------------------------- 退款与对账

// MountOrderAdmin 挂载订单退款与对账（P11-03）。
//
// ⚠️ 走 `order:read` / `order:write` 而不是 `member:*`：
// 处理一笔退款是**客服/财务**动作，改商品价格是运营动作 ——
// 合在一起的话，改价的人顺手就能退款。
func (h *AdminHandler) MountOrderAdmin(r chi.Router,
	requireAdmin func(http.Handler) http.Handler,
	requirePerm func(string) func(http.Handler) http.Handler) {
	r.Route("/admin/orders", func(r chi.Router) {
		r.Use(requireAdmin)
		r.With(requirePerm("order:read")).Get("/{orderID}/refund-check", h.refundCheck)
		r.With(requirePerm("order:write")).Post("/{orderID}/refund", h.refund)
		// 对账。⚠️ 跑一轮是**写**操作（会改 reconcile_status），
		// 所以走 `order:write`；看差异只要 read。
		r.With(requirePerm("order:write")).Post("/reconcile", h.reconcile)
		r.With(requirePerm("order:read")).Get("/reconcile/diffs", h.reconcileDiffs)
	})
}

// adminOrderScope 后台按订单取付费主体。
//
// ⚠️ 后台**没有工作区上下文**：客服处理的是别人的订单。
// 所以付费主体要从订单自己身上取，而不是从请求里 ——
// 从请求里取的话客服得先知道 payerId，而他手上只有订单号。
func (h *AdminHandler) adminOrderScope(w http.ResponseWriter, r *http.Request) (
	payerID, orderID int64, ok bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "orderID"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "orderID 非法"))
		return 0, 0, false
	}
	payer, err := h.svc.PayerOfOrder(r.Context(), id)
	if err != nil {
		httpx.Fail(w, r, err)
		return 0, 0, false
	}
	return payer, id, true
}

func (h *AdminHandler) refundCheck(w http.ResponseWriter, r *http.Request) {
	payer, id, ok := h.adminOrderScope(w, r)
	if !ok {
		return
	}
	el, err := h.svc.CheckRefund(r.Context(), payer, id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"refundable": el.Refundable, "code": el.Code, "reason": el.Reason,
		"amountCents": el.AmountCents, "revokeCredits": el.RevokeCredits,
		"revokeMembership": el.RevokeMembership,
	})
}

func (h *AdminHandler) refund(w http.ResponseWriter, r *http.Request) {
	payer, id, ok := h.adminOrderScope(w, r)
	if !ok {
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Reason == "" {
		// ⚠️ 后台代办**必须写原因**：事后对账时"这笔为什么退了"
		// 只有这一行能回答。给个兜底而不是拒绝 —— 拒绝会让客服
		// 在用户等着的时候卡在一个表单校验上。
		req.Reason = "客服代办退款"
	}
	row, err := h.svc.RefundOrder(r.Context(), billingapp.RefundInput{
		PayerID: payer, OrderID: id, AdminID: h.adminID(r), Reason: req.Reason,
	})
	if err != nil {
		if row.ID != 0 {
			httpx.JSON(w, http.StatusAccepted, map[string]any{
				"refundId": strconv.FormatInt(row.ID, 10), "error": err.Error(),
			})
			return
		}
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"refundId": strconv.FormatInt(row.ID, 10), "status": row.Status,
		"amountCents": row.AmountCents, "revokedCredits": row.RevokedCredits,
	})
}

func (h *AdminHandler) reconcile(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	res, err := h.svc.Reconcile(r.Context(), limit)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"checked": res.Checked, "matched": res.Matched, "diff": res.Diff,
		// ⚠️ `skipped` 要单独给：把"查不动"混进 diff 的话，
		// 一次网络抖动会在对账页上刷出一屏红字而真正的差异淹没在里面。
		"skipped": res.Skipped,
	})
}

func (h *AdminHandler) reconcileDiffs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := h.svc.ReconcileDiffs(r.Context(), limit)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(rows))
	for _, o := range rows {
		items = append(items, map[string]any{
			"id": strconv.FormatInt(o.ID, 10), "type": o.Type,
			"amountCents": o.AmountCents, "payStatus": o.PayStatus,
			"channel": o.Channel, "reconcileStatus": o.ReconcileStatus,
			"reconcileDetail": o.ReconcileDetail,
			"createdAt":       o.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	httpx.JSON(w, http.StatusOK, items)
}

// ---------------------------------------------------------------- 订单检索与调账

// MountOrderSearch 挂载后台订单检索、详情与人工调账（AD-06、GAP-07、P11-04）。
func (h *AdminHandler) MountOrderSearch(r chi.Router,
	requireAdmin func(http.Handler) http.Handler,
	requirePerm func(string) func(http.Handler) http.Handler) {
	r.Route("/admin/billing-ops", func(r chi.Router) {
		r.Use(requireAdmin)
		r.With(requirePerm("order:read")).Get("/orders", h.searchOrders)
		r.With(requirePerm("order:read")).Get("/orders/{orderID}", h.orderDetail)
		// 人工标记对账结论 —— 覆盖自动结论是财务的正当权力。
		r.With(requirePerm("order:write")).Post("/orders/{orderID}/reconcile",
			h.markReconcile)
		// ⚠️ 人工调账单开权限位 **`credits:adjust`**，不随 `order:write` 放开：
		// 它是后台唯一能**直接改用户资产**的操作，处理退款的客服
		// 不该顺手就能给任意账户加积分。
		r.With(requirePerm("credits:adjust")).Post("/payers/{payerID}/adjust",
			h.adjustCredits)
		r.With(requirePerm("order:read")).Get("/payers/{payerID}", h.payerOverview)
	})
}

func (h *AdminHandler) searchOrders(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	cursor, _ := strconv.ParseInt(q.Get("cursor"), 10, 64)
	f := repo.OrderFilter{
		Keyword: q.Get("keyword"), Type: q.Get("type"), Status: q.Get("status"),
		Channel: q.Get("channel"), Reconcile: q.Get("reconcile"),
		Limit: repo.OrderPageSize(limit), CursorID: cursor,
	}
	// ⚠️ 时间解析失败时**当成没传**而不是报错：运营从别处粘来的时间格式
	// 五花八门，为一个格式问题把整个列表挡掉不值 —— 少一个筛选条件
	// 顶多是结果多一些，而报错是什么都看不到。
	if t, err := time.Parse(time.RFC3339, q.Get("from")); err == nil {
		f.From = &t
	}
	if t, err := time.Parse(time.RFC3339, q.Get("to")); err == nil {
		f.To = &t
	}
	list, sum, err := h.svc.SearchOrders(r.Context(), f)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(list))
	for _, o := range list {
		items = append(items, adminOrderResp(o))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"items": items,
		// ⚠️ 此前收 `cursor` 却从不回下一页的游标 —— 只看得到最新的一页，
		// 更早的订单在后台**根本翻不到**（客服拿着订单号来查时要靠关键词碰运气）。
		"nextCursor": orderNextCursor(list, f.Limit),
		// ⚠️ 汇总与列表**同一套条件**算出来（见 service 注释）。
		"summary": map[string]any{
			"count": sum.Count, "paidCount": sum.PaidCount,
			"paidCents": sum.PaidCents, "refundCount": sum.RefundCount,
			"refundCents": sum.RefundCents, "diffCount": sum.DiffCount,
		},
	})
}

// orderNextCursor 满页时给出下一页的游标（按 id 倒序翻页）；不满一页说明没有下一页。
func orderNextCursor(list []repo.Order, limit int) string {
	if limit <= 0 || len(list) < limit {
		return ""
	}
	return strconv.FormatInt(list[len(list)-1].ID, 10)
}

func adminOrderResp(o repo.Order) map[string]any {
	out := map[string]any{
		"id":      strconv.FormatInt(o.ID, 10),
		"payerId": strconv.FormatInt(o.PayerID, 10),
		"type":    o.Type, "amountCents": o.AmountCents, "payStatus": o.PayStatus,
		"credits": o.Credits, "reconcileStatus": o.ReconcileStatus,
		"createdAt": o.CreatedAt.UTC().Format(time.RFC3339),
	}
	if o.Channel != "" {
		out["channel"] = o.Channel
	}
	if o.ChannelTxn != "" {
		out["channelTxnId"] = o.ChannelTxn
	}
	if o.ReconcileDetail != "" {
		out["reconcileDetail"] = o.ReconcileDetail
	}
	if o.PaidAt != nil {
		out["paidAt"] = o.PaidAt.UTC().Format(time.RFC3339)
	}
	return out
}

func (h *AdminHandler) orderDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "orderID"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "orderID 非法"))
		return
	}
	d, err := h.svc.AdminOrderDetail(r.Context(), id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	refunds := make([]map[string]any, 0, len(d.Refunds))
	for _, f := range d.Refunds {
		refunds = append(refunds, map[string]any{
			"id": strconv.FormatInt(f.ID, 10), "status": f.Status,
			"amountCents": f.AmountCents, "reason": f.Reason,
			"revokedCredits":  f.RevokedCredits,
			"channelRefundId": f.ChannelRefundID,
			"channelResult":   f.ChannelResult,
			"createdAt":       f.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	grants := make([]map[string]any, 0, len(d.Grants))
	for _, g := range d.Grants {
		item := map[string]any{
			"id": strconv.FormatInt(g.ID, 10), "source": g.Source,
			"amount": g.Amount, "remaining": g.Remaining, "status": g.Status,
		}
		if g.ExpireAt != nil {
			item["expireAt"] = g.ExpireAt.UTC().Format(time.RFC3339)
		}
		grants = append(grants, item)
	}
	body := map[string]any{
		"order": adminOrderResp(d.Order), "refunds": refunds, "grants": grants,
		// ⚠️ 回调条数是「回调丢了」最直接的证据：0 条 + 订单待支付就是那一类。
		// 详情里不给的话客服得再去翻回调留痕页。
		"callbackCount": d.CallbackCount,
	}
	if d.Membership != nil {
		body["membership"] = map[string]any{
			"id":        strconv.FormatInt(d.Membership.ID, 10),
			"levelCode": d.Membership.LevelCode, "status": d.Membership.Status,
			"expireAt": d.Membership.ExpireAt.UTC().Format(time.RFC3339),
		}
	}
	httpx.JSON(w, http.StatusOK, body)
}

func (h *AdminHandler) markReconcile(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "orderID"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "orderID 非法"))
		return
	}
	var req struct {
		Status string `json:"status"`
		Note   string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Fail(w, r, httpx.Err(errcode.InvalidJSON))
		return
	}
	if err := h.svc.MarkReconcileManual(r.Context(), h.adminID(r), id,
		req.Status, req.Note); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	h.orderDetail(w, r)
}

func (h *AdminHandler) adjustCredits(w http.ResponseWriter, r *http.Request) {
	payer, err := strconv.ParseInt(chi.URLParam(r, "payerID"), 10, 64)
	if err != nil || payer <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "payerID 非法"))
		return
	}
	var req struct {
		Amount   int    `json:"amount"`
		Reason   string `json:"reason"`
		ExpireAt string `json:"expireAt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Fail(w, r, httpx.Err(errcode.InvalidJSON))
		return
	}
	in := billingapp.AdjustInput{
		PayerID: payer, AdminID: h.adminID(r), Amount: req.Amount,
		Reason: req.Reason,
		// ⚠️ 幂等号从 header 取（与下单同一条约定），不从 body ——
		// 放 body 里的话前端可能在重试时重新生成一个。
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
	}
	if t, err := time.Parse(time.RFC3339, req.ExpireAt); err == nil {
		in.ExpireAt = &t
	}
	res, err := h.svc.AdjustCredits(r.Context(), in)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := map[string]any{
		"transactionId": strconv.FormatInt(res.TransactionID, 10),
		"balanceAfter":  res.BalanceAfter,
	}
	if res.GrantID != 0 {
		out["grantId"] = strconv.FormatInt(res.GrantID, 10)
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *AdminHandler) payerOverview(w http.ResponseWriter, r *http.Request) {
	payer, err := strconv.ParseInt(chi.URLParam(r, "payerID"), 10, 64)
	if err != nil || payer <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "payerID 非法"))
		return
	}
	v, err := h.svc.PayerOverview(r.Context(), payer)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	ws := make([]string, 0, len(v.WorkspaceIDs))
	for _, id := range v.WorkspaceIDs {
		ws = append(ws, strconv.FormatInt(id, 10))
	}
	body := map[string]any{
		"payerId": strconv.FormatInt(v.PayerID, 10), "kind": v.Kind,
		"ownerId": strconv.FormatInt(v.OwnerID, 10), "balance": v.Balance,
		// ⚠️ 挂在这个付费主体下的工作区**可能不止一个**：个人与团队各有自己的
		// payer，但误配会让两个工作区指向同一个 —— 那意味着一个团队的消费
		// 会记到个人账上。后台要能一眼看出来。
		"workspaceIds": ws,
	}
	if v.Membership != nil {
		body["membership"] = map[string]any{
			"levelCode": v.Membership.LevelCode,
			"expireAt":  v.Membership.ExpireAt.UTC().Format(time.RFC3339),
		}
	}
	httpx.JSON(w, http.StatusOK, body)
}
