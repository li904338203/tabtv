package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	billingapp "github.com/li904338203/tabtv/backend/internal/billing/app"
	"github.com/li904338203/tabtv/backend/internal/billing/domain"
	"github.com/li904338203/tabtv/backend/internal/billing/repo"
	paymentapp "github.com/li904338203/tabtv/backend/internal/payment/app"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 会员、商品与订单的用户端接口（P11-01）。

// MountMember 挂载会员相关端点。
func (h *Handler) MountMember(r chi.Router, requireUser,
	requireWorkspace func(http.Handler) http.Handler) {
	r.Group(func(r chi.Router) {
		r.Use(requireUser)
		r.Use(requireWorkspace)
		// 购买页一次拿齐：套餐 + 积分包 + 当前会员 + 余额。
		//
		// ⚠️ 合成一个端点而不是让前端发四个请求：四个请求会各自到达，
		// 页面上会出现"套餐已经画出来了但当前会员还没回来"的半截状态，
		// 而升级差价恰恰取决于当前会员是什么。
		r.Get("/storefront", h.storefront)
		r.Get("/membership", h.membership)
		r.Post("/orders", h.createOrder)
		r.Get("/orders", h.listOrders)
		r.Get("/orders/{orderID}", h.getOrder)
		r.Post("/orders/{orderID}/cancel", h.cancelOrder)
		// 开发支付：**仅开发环境**，见 SetDevPay 的注释。
		r.Post("/orders/{orderID}/dev-pay", h.devPay)
		// 真实渠道（P11-02）：拉起支付 + 主动查单。
		r.Post("/orders/{orderID}/pay", h.createPayment)
		r.Post("/orders/{orderID}/sync", h.syncOrder)
		// 退款（P11-03）。⚠️ `refund-check` 只读、可反复调 ——
		// 退款不可逆，必须能先看再决定（尤其"会收回多少积分"）。
		r.Get("/orders/{orderID}/refund-check", h.refundCheck)
		r.Post("/orders/{orderID}/refund", h.refundOrder)
		r.Get("/orders/{orderID}/refunds", h.listRefunds)
	})
}

// SetDevPay 打开开发支付。
//
// ⚠️ **只在 `SEEAITV_DEV_FAKE_PAYMENT=true` 且非 production 时注入**，
// production 下 config 会 fail-fast（`platform/config`）。没注入时这个端点
// 一律 404 —— 返回 403 会暴露"这里有个后门"，404 什么都不说。
//
// ⚠️ 它调的是**与真实渠道回调同一段履约代码**（`FulfilOrder`）。
// 另写一份"开发版履约"的话，P11-02 接真实渠道时两条路会出现差异，
// 而差异只会在真实支付上暴露 —— 那时已经收了用户的钱。
func (h *Handler) SetDevPay(on bool) { h.devPayOn = on }

func (h *Handler) storefront(w http.ResponseWriter, r *http.Request) {
	payer, ok := h.workspace(w, r)
	if !ok {
		return
	}
	sf, err := h.svc.Storefront(r.Context(), payer)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	plans := make([]map[string]any, 0, len(sf.Plans))
	for _, p := range sf.Plans {
		plans = append(plans, map[string]any{
			"id": strconv.FormatInt(p.ID, 10), "levelId": strconv.FormatInt(p.LevelID, 10),
			"levelCode": p.LevelCode, "levelName": p.LevelName, "levelSort": p.LevelSort,
			"period": p.Period, "priceCents": p.PriceCents, "giftCredits": p.GiftCredits,
		})
	}
	pkgs := make([]map[string]any, 0, len(sf.Packages))
	for _, p := range sf.Packages {
		pkgs = append(pkgs, map[string]any{
			"id": strconv.FormatInt(p.ID, 10), "code": p.Code, "name": p.Name,
			"credits": p.Credits, "priceCents": p.PriceCents,
		})
	}
	body := map[string]any{"plans": plans, "packages": pkgs, "balance": sf.Balance}
	// ⚠️ **开发支付开没开由服务端说**，不让前端去探一个 404。
	// 探测的做法每次打开页面都会打一个必然失败的请求，日志里全是它；
	// 而且"404 就是没开"这个推断在网关挡掉请求时会得出错的结论。
	body["devPayEnabled"] = h.devPayOn
	// 可用的真实渠道。⚠️ 由服务端说 —— 前端硬编码一份的话，
	// 后台停用一个渠道之后界面上它还亮着，用户点了才发现不能用。
	if h.payGw != nil {
		body["channels"] = h.payGw.Channels()
	} else {
		body["channels"] = []string{}
	}
	if sf.Current != nil {
		body["membership"] = membershipResp(*sf.Current)
	}
	httpx.JSON(w, http.StatusOK, body)
}

func membershipResp(m repo.Membership) map[string]any {
	return map[string]any{
		"id":        strconv.FormatInt(m.ID, 10),
		"levelId":   strconv.FormatInt(m.LevelID, 10),
		"levelCode": m.LevelCode, "levelName": m.LevelName, "levelSort": m.LevelSort,
		"startAt":  m.StartAt.UTC().Format(time.RFC3339),
		"expireAt": m.ExpireAt.UTC().Format(time.RFC3339),
		"status":   m.Status,
	}
}

func (h *Handler) membership(w http.ResponseWriter, r *http.Request) {
	payer, ok := h.workspace(w, r)
	if !ok {
		return
	}
	cur, err := h.svc.CurrentMembership(r.Context(), payer)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	hist, err := h.svc.Memberships(r.Context(), payer, 20)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(hist))
	for _, m := range hist {
		items = append(items, membershipResp(m))
	}
	body := map[string]any{"history": items}
	if cur != nil {
		body["current"] = membershipResp(*cur)
	}
	httpx.JSON(w, http.StatusOK, body)
}

func (h *Handler) createOrder(w http.ResponseWriter, r *http.Request) {
	payer, ok := h.workspace(w, r)
	if !ok {
		return
	}
	var req struct {
		Kind      string `json:"kind"`
		PlanID    string `json:"planId"`
		PackageID string `json:"packageId"`
		DryRun    bool   `json:"dryRun"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Fail(w, r, httpx.Err(errcode.InvalidJSON))
		return
	}
	planID, _ := strconv.ParseInt(req.PlanID, 10, 64)
	pkgID, _ := strconv.ParseInt(req.PackageID, 10, 64)
	q, order, err := h.svc.Purchase(r.Context(), billingapp.PurchaseInput{
		PayerID: payer, UserID: h.userID(r), Kind: req.Kind,
		PlanID: planID, PackageID: pkgID, DryRun: req.DryRun,
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	body := map[string]any{
		"intent": q.Intent, "amountCents": q.AmountCents,
		"credits": q.Credits, "title": q.Title,
	}
	if q.ExpireAt != nil {
		body["membershipExpireAt"] = q.ExpireAt.UTC().Format(time.RFC3339)
	}
	if q.RemainingDays > 0 {
		// 升级时把折算天数说出来 —— 只给一个差价数字的话，
		// 用户没法判断这个价是怎么来的，也就没法信它。
		body["remainingDays"] = q.RemainingDays
	}
	if req.DryRun {
		httpx.JSON(w, http.StatusOK, body)
		return
	}
	body["order"] = orderResp(*order)
	httpx.JSON(w, http.StatusCreated, body)
}

func orderResp(o repo.Order) map[string]any {
	out := map[string]any{
		"id": strconv.FormatInt(o.ID, 10), "type": o.Type,
		"amountCents": o.AmountCents, "currency": o.Currency,
		"payStatus": o.PayStatus, "credits": o.Credits,
		"createdAt": o.CreatedAt.UTC().Format(time.RFC3339),
	}
	if o.RefID != 0 {
		out["refId"] = strconv.FormatInt(o.RefID, 10)
	}
	if o.Channel != "" {
		out["channel"] = o.Channel
	}
	if o.PaidAt != nil {
		out["paidAt"] = o.PaidAt.UTC().Format(time.RFC3339)
	}
	if o.ExpireAt != nil {
		out["expireAt"] = o.ExpireAt.UTC().Format(time.RFC3339)
	}
	return out
}

func (h *Handler) listOrders(w http.ResponseWriter, r *http.Request) {
	payer, ok := h.workspace(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	cursor, _ := strconv.ParseInt(r.URL.Query().Get("cursor"), 10, 64)
	list, err := h.svc.Orders(r.Context(), payer, limit, cursor)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(list))
	for _, o := range list {
		items = append(items, orderResp(o))
	}
	httpx.JSON(w, http.StatusOK, items)
}

func (h *Handler) orderID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "orderID"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "orderID 非法"))
		return 0, false
	}
	return id, true
}

func (h *Handler) getOrder(w http.ResponseWriter, r *http.Request) {
	payer, ok := h.workspace(w, r)
	if !ok {
		return
	}
	id, ok := h.orderID(w, r)
	if !ok {
		return
	}
	o, err := h.svc.Order(r.Context(), payer, id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, orderResp(o))
}

func (h *Handler) cancelOrder(w http.ResponseWriter, r *http.Request) {
	payer, ok := h.workspace(w, r)
	if !ok {
		return
	}
	id, ok := h.orderID(w, r)
	if !ok {
		return
	}
	o, err := h.svc.CancelOrder(r.Context(), payer, id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, orderResp(o))
}

// devPay 开发支付：直接把订单当作已付款并履约。
//
// ⚠️ **不是真实支付**（GO-04 原文：开发支付必须仅限明确开启的开发环境）。
// 真实渠道在 P11-02；它会走同一个 `FulfilOrder`，只是前面多了验签与查单。
func (h *Handler) devPay(w http.ResponseWriter, r *http.Request) {
	if !h.devPayOn {
		// ⚠️ 404 而不是 403：403 等于告诉外面"这里有个后门，只是你没权限"。
		httpx.Fail(w, r, httpx.Err(errcode.NotFound))
		return
	}
	payer, ok := h.workspace(w, r)
	if !ok {
		return
	}
	id, ok := h.orderID(w, r)
	if !ok {
		return
	}
	res, err := h.svc.FulfilOrder(r.Context(), payer, id, "dev",
		"dev-"+strconv.FormatInt(id, 10))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	body := map[string]any{"order": orderResp(res.Order)}
	// ⚠️ 明确告诉调用方"这次是重复履约"：不说的话联调脚本会把
	// 第二次调用的成功当成"又发了一次权益"，而它其实什么都没做。
	body["alreadyFulfilled"] = res.AlreadyDone
	if res.GrantID != 0 {
		body["grantId"] = strconv.FormatInt(res.GrantID, 10)
	}
	if res.MembershipID != 0 {
		body["membershipId"] = strconv.FormatInt(res.MembershipID, 10)
	}
	httpx.JSON(w, http.StatusOK, body)
}

// ---------------------------------------------------------------- 真实支付

// PayGateway 由支付域实现：向渠道下单、主动查单。
type PayGateway interface {
	CreatePayment(ctx context.Context, channel string, ref paymentapp.OrderRef) (
		paymentapp.PayParams, error)
	SyncOrder(ctx context.Context, channel string, order paymentapp.OrderInfo,
		outTradeNo string) (bool, error)
	Channels() []string
}

// SetPayGateway 注入支付域。没注入时 `/pay` 与 `/sync` 一律 404。
func (h *Handler) SetPayGateway(g PayGateway) { h.payGw = g }

// pay 向渠道下单，拿回拉起支付所需的参数。
//
// ⚠️ **这一步不改订单状态**：拿到二维码不等于付了钱。
// 顺手把订单标成"支付中"的话，状态机会多出一个没人能推进的中间态 ——
// 用户关掉页面之后它永远停在那儿。
func (h *Handler) createPayment(w http.ResponseWriter, r *http.Request) {
	if h.payGw == nil {
		httpx.Fail(w, r, httpx.Err(errcode.NotFound))
		return
	}
	payer, ok := h.workspace(w, r)
	if !ok {
		return
	}
	id, ok := h.orderID(w, r)
	if !ok {
		return
	}
	var req struct {
		Channel string `json:"channel"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	o, err := h.svc.Order(r.Context(), payer, id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if o.PayStatus != repo.PayPending {
		// ⚠️ 已经不是待支付的单不给支付参数：给了的话用户会去付第二笔，
		// 而那笔钱进来时订单已经是 paid —— 只能人工退。
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidStateTransition,
			"订单当前是"+payStatusName(o.PayStatus)+"，不能支付"))
		return
	}
	expire := time.Now().Add(30 * time.Minute)
	if o.ExpireAt != nil {
		expire = *o.ExpireAt
	}
	// ⚠️ 先记下渠道再去下单：下单成功后再记的话，下单请求超时的那次
	// （上游其实已经建了单）就丢了渠道，`/sync` 不知道该问哪一家。
	if err := h.svc.MarkOrderChannel(r.Context(), o.ID, req.Channel); err != nil {
		httpx.Fail(w, r, httpx.Wrap(errcode.Internal, err))
		return
	}
	params, err := h.payGw.CreatePayment(r.Context(), req.Channel, paymentapp.OrderRef{
		// ⚠️ 商户订单号**就用订单 ID**，不另造编号：另造的话对账时
		// 要在两套号之间来回映射，而映射表本身会丢。
		OutTradeNo: strconv.FormatInt(o.ID, 10), AmountCents: o.AmountCents,
		Subject: subjectOf(o), ExpireAt: expire,
	})
	if err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.Internal, err.Error()))
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"channel": params.Channel, "kind": params.Kind,
		"values":   params.Values,
		"expireAt": params.ExpireAt.UTC().Format(time.RFC3339),
		// ⚠️ **明确告诉前端"跳转完成不等于支付成功"**：不说的话
		// 前端会在用户从渠道页面回来时直接显示"支付成功"（GAP-14 的同类）。
		"note": "拉起支付不代表已付款。付款结果以渠道回调为准；" +
			"回调迟迟不到时调 /orders/{id}/sync 主动查单。",
	})
}

func subjectOf(o repo.Order) string {
	if o.Type == repo.OrderMembership {
		return "会员套餐"
	}
	return "积分包"
}

// sync 主动查单。
//
// ⚠️ 这是「用户说付了但回调没来」的出口，也是 GAP-14 的同类：
// 浏览器那边的"跳转回来了"证明不了任何事 —— 唯一可信的是渠道自己说的话。
//
// ⚠️ 查单走**与回调同一段履约代码**：另写一份的话，两条路会对同一笔钱
// 做出不同的事，而差异只在生产上暴露。
func (h *Handler) syncOrder(w http.ResponseWriter, r *http.Request) {
	if h.payGw == nil {
		httpx.Fail(w, r, httpx.Err(errcode.NotFound))
		return
	}
	payer, ok := h.workspace(w, r)
	if !ok {
		return
	}
	id, ok := h.orderID(w, r)
	if !ok {
		return
	}
	o, err := h.svc.Order(r.Context(), payer, id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if o.PayStatus == repo.PayPaid {
		// 已经付过了 —— 直接回，不去打扰渠道。
		httpx.JSON(w, http.StatusOK, map[string]any{
			"paid": true, "order": orderResp(o),
		})
		return
	}
	channel := o.Channel
	if channel == "" {
		channel = r.URL.Query().Get("channel")
	}
	if channel == "" {
		// ⚠️ 说清是"还没选渠道"而不是"渠道没配置"：后者会让运维
		// 去翻后台配置，而真实原因是这张单根本还没拉起过支付。
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument,
			"这张订单还没有拉起过支付，无从查单"))
		return
	}
	paid, err := h.payGw.SyncOrder(r.Context(), channel, paymentapp.OrderInfo{
		ID: o.ID, PayerID: payer, AmountCents: o.AmountCents,
		PayStatus: o.PayStatus, Channel: channel,
	}, strconv.FormatInt(o.ID, 10))
	if err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.Internal, err.Error()))
		return
	}
	fresh, err := h.svc.Order(r.Context(), payer, id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"paid": paid, "order": orderResp(fresh),
	})
}

// payStatusName 订单状态的中文说法（transport 侧展示用）。
func payStatusName(s string) string {
	switch s {
	case repo.PayPaid:
		return "已支付"
	case repo.PayCancelled:
		return "已取消"
	case repo.PayRefunded:
		return "已退款"
	case repo.PayExpired:
		return "已超时关闭"
	}
	return s
}

// ---------------------------------------------------------------- 退款

func refundResp(f repo.RefundRow) map[string]any {
	out := map[string]any{
		"id":          strconv.FormatInt(f.ID, 10),
		"orderId":     strconv.FormatInt(f.OrderID, 10),
		"amountCents": f.AmountCents, "status": f.Status,
		"reason": f.Reason,
		// ⚠️ **回收了什么要说出来**：用户看到"退款成功"而积分没了，
		// 不说的话他会以为系统出错了。
		"revokedCredits": f.RevokedCredits,
		"createdAt":      f.CreatedAt.UTC().Format(time.RFC3339),
	}
	if f.RevokedMembershipID != 0 {
		out["revokedMembership"] = true
	}
	if f.RestoredMembershipID != 0 {
		// ⚠️ 恢复了上一段会员也要说：用户退掉升级单之后
		// 会员并没有消失，不说的话他会再来问一次。
		out["restoredPreviousMembership"] = true
	}
	if f.ChannelRefundID != "" {
		out["channelRefundId"] = f.ChannelRefundID
	}
	return out
}

// refundCheck 只读判定，可反复调。
//
// ⚠️ 与下单的 `dryRun` 同一条：退款是不可逆的，必须能**先看再决定** ——
// 尤其要在确认之前告诉他"会收回多少积分"。
func (h *Handler) refundCheck(w http.ResponseWriter, r *http.Request) {
	payer, ok := h.workspace(w, r)
	if !ok {
		return
	}
	id, ok := h.orderID(w, r)
	if !ok {
		return
	}
	el, err := h.svc.CheckRefund(r.Context(), payer, id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	body := map[string]any{
		"refundable": el.Refundable, "amountCents": el.AmountCents,
		"revokeCredits": el.RevokeCredits, "revokeMembership": el.RevokeMembership,
	}
	if !el.Refundable {
		body["code"] = el.Code
		body["reason"] = el.Reason
		// ⚠️ 显式给出"用户自己能不能解决"：前端按**分类**决定语气，
		// 而不是去匹配文案（文案改一个字匹配就失效）。
		body["userFixable"] = domain.UserFixableRefund(el.Code)
	}
	httpx.JSON(w, http.StatusOK, body)
}

func (h *Handler) refundOrder(w http.ResponseWriter, r *http.Request) {
	payer, ok := h.workspace(w, r)
	if !ok {
		return
	}
	id, ok := h.orderID(w, r)
	if !ok {
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Reason == "" {
		req.Reason = "用户申请退款"
	}
	row, err := h.svc.RefundOrder(r.Context(), billingapp.RefundInput{
		PayerID: payer, OrderID: id, UserID: h.userID(r), Reason: req.Reason,
	})
	if err != nil {
		// ⚠️ 出错时**也要把退款单回出去**（如果建出来了）：
		// "已受理并回收权益但调渠道出错"这种情况下，用户需要看到那个单号
		// 才能跟客服对上 —— 只回一句错误的话他手上什么都没有。
		if row.ID != 0 {
			httpx.JSON(w, http.StatusAccepted, map[string]any{
				"refund": refundResp(row), "error": err.Error(),
			})
			return
		}
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"refund": refundResp(row)})
}

func (h *Handler) listRefunds(w http.ResponseWriter, r *http.Request) {
	payer, ok := h.workspace(w, r)
	if !ok {
		return
	}
	id, ok := h.orderID(w, r)
	if !ok {
		return
	}
	rows, err := h.svc.RefundsOfOrder(r.Context(), payer, id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(rows))
	for _, f := range rows {
		items = append(items, refundResp(f))
	}
	httpx.JSON(w, http.StatusOK, items)
}
