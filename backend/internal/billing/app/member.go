package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/li904338203/tabtv/backend/internal/billing/domain"
	"github.com/li904338203/tabtv/backend/internal/billing/repo"
	paymentapp "github.com/li904338203/tabtv/backend/internal/payment/app"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 会员、商品与订单（P11-01）。
//
// ⚠️ 这一层只做**编排**：怎么算钱、算到哪天，全在 `billing/domain`
// 的纯函数里（不碰库、不取当前时间）。算钱的逻辑散在 service 里的话
// 就只能靠真库测试去验，而那种测试跑一轮要几秒、边界根本盖不全。

// OrderTTL 待支付订单多久作废。
//
// ⚠️ 必须有：订单占着幂等键，不作废的话用户再下同一单会命中那张僵尸订单，
// 看到的是一张早就付不了的单。具体时长是**设计建议值**（PARAM-03 定稿前）。
const OrderTTL = 30 * time.Minute

// PurchaseKind 下单意图。
const (
	KindMembership = "membership"
	KindPackage    = "credit_package"
)

// Storefront 是购买页要的一整份数据。
type Storefront struct {
	Plans    []repo.Plan
	Packages []repo.Package
	// Current 当前有效会员（nil 表示没有）。
	Current *repo.Membership
	Balance int
}

func (s *Service) Storefront(ctx context.Context, payerID int64) (Storefront, error) {
	plans, err := s.repo.ListPlans(ctx, true)
	if err != nil {
		return Storefront{}, httpx.Wrap(errcode.Internal, err)
	}
	pkgs, err := s.repo.ListPackages(ctx, true)
	if err != nil {
		return Storefront{}, httpx.Wrap(errcode.Internal, err)
	}
	out := Storefront{Plans: plans, Packages: pkgs}
	if m, err := s.repo.CurrentMembership(ctx, payerID); err == nil {
		out.Current = &m
	} else if !errors.Is(err, repo.ErrNotFound) {
		return Storefront{}, httpx.Wrap(errcode.Internal, err)
	}
	if bal, err := s.repo.Balance(ctx, payerID); err == nil {
		out.Balance = bal
	}
	return out, nil
}

// CurrentMembership 当前有效会员（供权益解析与"我的会员"页用）。
func (s *Service) CurrentMembership(ctx context.Context, payerID int64) (
	*repo.Membership, error) {
	m, err := s.repo.CurrentMembership(ctx, payerID)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return &m, nil
}

// MemberLevelID 当前等级（0 表示无会员）。
//
// ⚠️ 这是模型权益判定的唯一入口（`min_member_level_id`）。
// 取不到时返回 **0 而不是报错**：一次数据库抖动不该让所有生成都失败 ——
// 降级成"按免费档算"，用户最多看到几个模型暂时不可用，
// 而那比整个站点报错好得多。
func (s *Service) MemberLevelID(ctx context.Context, payerID int64) int64 {
	m, err := s.repo.CurrentMembership(ctx, payerID)
	if err != nil {
		return 0
	}
	return m.LevelID
}

// PurchaseInput 下单。
type PurchaseInput struct {
	PayerID int64
	UserID  int64
	Kind    string
	// PlanID / PackageID 二选一。
	PlanID         int64
	PackageID      int64
	IdempotencyKey string
	// DryRun 只报价不建单。
	//
	// ⚠️ 购买页必须能**先看价再决定**：升级的差价由服务端按剩余天数算，
	// 前端按"套餐差价"自己估出来的根本不是一回事。
	DryRun bool
}

// PurchaseQuote 下单报价。
type PurchaseQuote struct {
	// Intent new / renew / upgrade —— 让界面说清"这次买是什么性质"。
	Intent      string
	AmountCents int64
	Credits     int
	// ExpireAt 买完之后会员到期到哪天（积分包单为 nil）。
	ExpireAt *time.Time
	// RemainingDays 升级折算用到的剩余天数（仅升级单有意义）。
	RemainingDays int
	Title         string
}

// QuotePurchase 报价：会员单按新购/续费/升级三种情况分别算。
func (s *Service) QuotePurchase(ctx context.Context, in PurchaseInput) (
	PurchaseQuote, repo.CreateOrderInput, error) {
	now := time.Now().UTC()
	switch in.Kind {
	case KindPackage:
		p, err := s.repo.Package(ctx, in.PackageID)
		if errors.Is(err, repo.ErrNotFound) {
			return PurchaseQuote{}, repo.CreateOrderInput{},
				httpx.Errf(errcode.NotFound, "积分包不存在")
		}
		if err != nil {
			return PurchaseQuote{}, repo.CreateOrderInput{}, httpx.Wrap(errcode.Internal, err)
		}
		if !p.IsEnabled {
			// ⚠️ 已下架的商品**明确拒绝**：用户可能停在一个旧页面上。
			// 静默放行的话他会买到一个我们已经不卖的东西。
			return PurchaseQuote{}, repo.CreateOrderInput{},
				httpx.Errf(errcode.InvalidArgument, "该积分包已下架")
		}
		q := PurchaseQuote{
			Intent: repo.IntentNew, AmountCents: p.PriceCents,
			Credits: p.Credits, Title: p.Name,
		}
		exp := now.Add(OrderTTL)
		return q, repo.CreateOrderInput{
			PayerID: in.PayerID, PlacedBy: in.UserID, Type: repo.OrderPackage,
			RefID: p.ID, AmountCents: p.PriceCents,
			// ⚠️ 积分包发**永久**积分（`MembershipExpireAt` 留空即永久）。
			Credits: p.Credits, IdempotencyKey: in.IdempotencyKey, ExpireAt: &exp,
		}, nil

	case KindMembership:
		return s.quoteMembership(ctx, in, now)
	}
	return PurchaseQuote{}, repo.CreateOrderInput{},
		httpx.Errf(errcode.InvalidArgument, "下单类型只能是 membership 或 credit_package")
}

func (s *Service) quoteMembership(ctx context.Context, in PurchaseInput,
	now time.Time) (PurchaseQuote, repo.CreateOrderInput, error) {
	fail := func(e error) (PurchaseQuote, repo.CreateOrderInput, error) {
		return PurchaseQuote{}, repo.CreateOrderInput{}, e
	}
	plan, err := s.repo.Plan(ctx, in.PlanID)
	if errors.Is(err, repo.ErrNotFound) {
		return fail(httpx.Errf(errcode.NotFound, "套餐不存在"))
	}
	if err != nil {
		return fail(httpx.Wrap(errcode.Internal, err))
	}
	if !plan.IsEnabled {
		return fail(httpx.Errf(errcode.InvalidArgument, "该套餐已下架"))
	}

	var current *domain.Membership
	var curRow *repo.Membership
	if m, err := s.repo.CurrentMembership(ctx, in.PayerID); err == nil {
		curRow = &m
		current = &domain.Membership{
			LevelID: m.LevelID, StartAt: m.StartAt, ExpireAt: m.ExpireAt,
		}
	} else if !errors.Is(err, repo.ErrNotFound) {
		return fail(httpx.Wrap(errcode.Internal, err))
	}

	orderExp := now.Add(OrderTTL)
	base := repo.CreateOrderInput{
		PayerID: in.PayerID, PlacedBy: in.UserID, Type: repo.OrderMembership,
		RefID: plan.ID, LevelID: plan.LevelID,
		IdempotencyKey: in.IdempotencyKey, ExpireAt: &orderExp,
	}

	// 新购 / 同等级续费：从当前有效期顺延。
	if current == nil || current.LevelID == plan.LevelID {
		r, err := domain.PlanRenew(current, plan.Period, now)
		if err != nil {
			return fail(httpx.Errf(errcode.InvalidArgument, err.Error()))
		}
		intent := repo.IntentNew
		if r.Extended {
			intent = repo.IntentRenew
		}
		base.AmountCents = plan.PriceCents
		base.Credits = plan.GiftCredits
		base.MembershipStartAt = &r.StartAt
		base.MembershipExpireAt = &r.ExpireAt
		return PurchaseQuote{
			Intent: intent, AmountCents: plan.PriceCents, Credits: plan.GiftCredits,
			ExpireAt: &r.ExpireAt, Title: plan.LevelName + " · " + periodName(plan.Period),
		}, base, nil
	}

	// 升级：要拿**当前等级的同周期套餐**作基准算日价差。
	fromPlan, err := s.repo.PlanFor(ctx, current.LevelID, plan.Period)
	if errors.Is(err, repo.ErrNotFound) {
		// 当前等级没有这个周期的套餐 —— 没法算日价差。
		// ⚠️ 明说而不是按新购收全款：按全款收的话用户已经付过的那段时间白白作废。
		return fail(httpx.Errf(errcode.InvalidArgument,
			fmt.Sprintf("当前等级没有%s套餐，无法按该周期折算升级差价", periodName(plan.Period))))
	}
	if err != nil {
		return fail(httpx.Wrap(errcode.Internal, err))
	}
	up, err := domain.PlanUpgrade(*current,
		repo.UpgradePlanOf(fromPlan), repo.UpgradePlanOf(plan), now)
	switch {
	case errors.Is(err, domain.ErrDowngrade):
		return fail(httpx.Errf(errcode.InvalidArgument, err.Error()))
	case errors.Is(err, domain.ErrSameLevelUpgrade), errors.Is(err, domain.ErrPeriodInvalid):
		return fail(httpx.Errf(errcode.InvalidArgument, err.Error()))
	case err != nil:
		return fail(httpx.Wrap(errcode.Internal, err))
	}
	base.AmountCents = up.AmountCents
	base.Credits = up.GiftCredits
	base.MembershipStartAt = &curRow.StartAt
	base.MembershipExpireAt = &up.ExpireAt
	return PurchaseQuote{
		Intent: repo.IntentUpgrade, AmountCents: up.AmountCents,
		Credits: up.GiftCredits, ExpireAt: &up.ExpireAt,
		RemainingDays: up.RemainingDays,
		Title:         "升级至 " + plan.LevelName,
	}, base, nil
}

func periodName(p string) string {
	switch p {
	case domain.PeriodMonth:
		return "月付"
	case domain.PeriodQuarter:
		return "季付"
	case domain.PeriodYear:
		return "年付"
	}
	return p
}

// Purchase 下单（或只报价）。
func (s *Service) Purchase(ctx context.Context, in PurchaseInput) (
	PurchaseQuote, *repo.Order, error) {
	q, create, err := s.QuotePurchase(ctx, in)
	if err != nil {
		return PurchaseQuote{}, nil, err
	}
	if in.DryRun {
		return q, nil, nil
	}
	if in.IdempotencyKey == "" {
		// ⚠️ 下单必须带幂等键：不带的话用户连点两次就是两张单、两次扣款。
		return PurchaseQuote{}, nil, httpx.Errf(errcode.InvalidArgument,
			"下单必须带 Idempotency-Key")
	}
	o, err := s.repo.CreateOrder(ctx, create)
	if err != nil {
		return PurchaseQuote{}, nil, httpx.Wrap(errcode.Internal, err)
	}
	return q, &o, nil
}

func (s *Service) Order(ctx context.Context, payerID, id int64) (repo.Order, error) {
	o, err := s.repo.Order(ctx, payerID, id)
	if errors.Is(err, repo.ErrNotFound) {
		return repo.Order{}, httpx.Err(errcode.NotFound)
	}
	if err != nil {
		return repo.Order{}, httpx.Wrap(errcode.Internal, err)
	}
	return o, nil
}

func (s *Service) Orders(ctx context.Context, payerID int64, limit int,
	cursor int64) ([]repo.Order, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	out, err := s.repo.ListOrders(ctx, payerID, limit, cursor)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return out, nil
}

func (s *Service) CancelOrder(ctx context.Context, payerID, id int64) (repo.Order, error) {
	changed, err := s.repo.CancelOrder(ctx, payerID, id)
	if err != nil {
		return repo.Order{}, httpx.Wrap(errcode.Internal, err)
	}
	if !changed {
		o, err := s.Order(ctx, payerID, id)
		if err != nil {
			return repo.Order{}, err
		}
		// ⚠️ 已经不是待支付的订单要**明确拒绝**，不是静默成功 ——
		// 静默的话用户以为取消了（甚至以为退了钱），而其实什么都没发生。
		return repo.Order{}, httpx.Errf(errcode.InvalidStateTransition,
			"订单当前是"+orderStatusName(o.PayStatus)+"，不能取消")
	}
	return s.Order(ctx, payerID, id)
}

func orderStatusName(s string) string {
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

// FulfilOrder 履约一张订单。
//
// ⚠️ **这个方法本身不判断"付没付钱"** —— 那是支付适配器的事（P11-02）。
// 它只负责"已经确认收到钱之后要做什么"，所以真实渠道回调与开发支付
// 走的是同一段代码。把验签塞进来的话，P11-02 会变成改这里而不是加一个适配器。
func (s *Service) FulfilOrder(ctx context.Context, payerID, orderID int64,
	channel, channelTxn string) (repo.FulfilResult, error) {
	res, err := s.repo.Fulfil(ctx, payerID, orderID, channel, channelTxn,
		time.Now().UTC())
	if errors.Is(err, repo.ErrNotFound) {
		return repo.FulfilResult{}, httpx.Err(errcode.NotFound)
	}
	if err != nil {
		return repo.FulfilResult{}, httpx.Wrap(errcode.Internal, err)
	}
	if res.Order.PayStatus != repo.PayPaid {
		return repo.FulfilResult{}, httpx.Errf(errcode.InvalidStateTransition,
			"订单当前是"+orderStatusName(res.Order.PayStatus)+"，不能支付")
	}
	return res, nil
}

// Memberships 会员历史。
func (s *Service) Memberships(ctx context.Context, payerID int64, limit int) (
	[]repo.Membership, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	out, err := s.repo.ListMemberships(ctx, payerID, limit)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return out, nil
}

// ---------------------------------------------------------------- 商品管理

// AdminCatalog 后台看全部（含停用的）。
func (s *Service) AdminCatalog(ctx context.Context) ([]repo.Level, []repo.Plan,
	[]repo.Package, error) {
	levels, err := s.repo.ListLevels(ctx, false)
	if err != nil {
		return nil, nil, nil, httpx.Wrap(errcode.Internal, err)
	}
	plans, err := s.repo.ListPlans(ctx, false)
	if err != nil {
		return nil, nil, nil, httpx.Wrap(errcode.Internal, err)
	}
	pkgs, err := s.repo.ListPackages(ctx, false)
	if err != nil {
		return nil, nil, nil, httpx.Wrap(errcode.Internal, err)
	}
	return levels, plans, pkgs, nil
}

// SaveLevel 保存等级。
//
// ⚠️ 校验在这里做一遍**不是**因为库里没有约束，而是因为约束冲突
// 给运营的是一句 23514/23505，说不出是哪个字段越界。
func (s *Service) SaveLevel(ctx context.Context, adminID int64, l repo.Level) error {
	l.Name = strings.TrimSpace(l.Name)
	if err := domain.ValidateCatalogCode(l.Code); err != nil {
		return httpx.Errf(errcode.InvalidArgument, "等级"+err.Error())
	}
	if err := domain.ValidateCatalogName(l.Name); err != nil {
		return httpx.Errf(errcode.InvalidArgument, "等级"+err.Error())
	}
	// ⚠️ 等级高低**只按 sort 比**（升级/降级的判定、差价折算都靠它）。两个等级 sort 相同的话
	// 谁高谁低说不清：PlanUpgrade 会把它们之间的切换当成「升级」而差价按 0 算 ——
	// 用户花 0 元就能在两个等级之间来回换。这是配置错误，保存时就拒。
	levels, err := s.repo.ListLevels(ctx, false)
	if err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	for _, other := range levels {
		if other.Code != l.Code && other.Sort == l.Sort {
			return httpx.Errf(errcode.InvalidArgument, "排序值与「"+other.Name+
				"」相同：等级高低按排序比较，相同的话说不清谁高谁低")
		}
	}
	if err := s.repo.SaveLevel(ctx, l); err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	s.auditCatalog(ctx, adminID, "member_level.save", l)
	return nil
}

// SavePlan 保存套餐。
func (s *Service) SavePlan(ctx context.Context, adminID int64, p repo.Plan) error {
	if _, err := domain.PeriodMonths(p.Period); err != nil {
		return httpx.Errf(errcode.InvalidArgument, "套餐周期只能是 month/quarter/year")
	}
	if p.LevelID == 0 {
		return httpx.Errf(errcode.InvalidArgument, "套餐必须挂在一个等级上")
	}
	// 等级是逻辑外键：不查的话挂在一个不存在的等级上也能存进去，而用户端永远列不出它。
	levels, err := s.repo.ListLevels(ctx, false)
	if err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	found := false
	for _, l := range levels {
		found = found || l.ID == p.LevelID
	}
	if !found {
		return httpx.Errf(errcode.InvalidArgument, "套餐挂的等级不存在")
	}
	if err := domain.ValidateCatalogCredits(p.GiftCredits); err != nil {
		return httpx.Errf(errcode.InvalidArgument, "赠送"+err.Error())
	}
	// ⚠️ **金额与赠分不能为负**：负价等于下单就给用户打钱，
	// 负赠分会让履约时发一个负数批次（`ck_grant_amount` 会拒，
	// 但那时订单已经建好了、用户已经在付款页上）。
	if p.PriceCents < 0 || p.GiftCredits < 0 {
		return httpx.Errf(errcode.InvalidArgument, "价格与赠分不能为负")
	}
	if err := s.repo.SavePlan(ctx, p); err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	s.auditCatalog(ctx, adminID, "member_plan.save", p)
	return nil
}

// SavePackage 保存积分包。
func (s *Service) SavePackage(ctx context.Context, adminID int64, p repo.Package) error {
	p.Name = strings.TrimSpace(p.Name)
	if err := domain.ValidateCatalogCode(p.Code); err != nil {
		return httpx.Errf(errcode.InvalidArgument, "积分包"+err.Error())
	}
	if err := domain.ValidateCatalogName(p.Name); err != nil {
		return httpx.Errf(errcode.InvalidArgument, "积分包"+err.Error())
	}
	if err := domain.ValidateCatalogCredits(p.Credits); err != nil {
		return httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	// ⚠️ 积分包的积分必须 > 0（`ck_package_credits`）：0 分的包
	// 用户付了钱什么都拿不到，而订单是"成功"的。
	if p.Credits <= 0 {
		return httpx.Errf(errcode.InvalidArgument, "积分包的积分必须大于 0")
	}
	if p.PriceCents < 0 {
		return httpx.Errf(errcode.InvalidArgument, "价格不能为负")
	}
	if err := s.repo.SavePackage(ctx, p); err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	s.auditCatalog(ctx, adminID, "credit_package.save", p)
	return nil
}

// ---------------------------------------------------------------- 到期作业

// ExpireSweep 把过期的会员与超时未支付的订单标掉。
//
// ⚠️ 两者的重要性**完全不同**：会员过期只影响列表上的状态字面值
// （权益判定按时间算，见 repo.CurrentMembership）；而订单超时**必须跑** ——
// 它占着幂等键，不清掉的话用户再下同一单会命中那张僵尸订单。
func (s *Service) ExpireSweep(ctx context.Context) (memberships, orders int) {
	if n, err := s.repo.ExpireMemberships(ctx, 500); err == nil {
		memberships = n
	}
	if n, err := s.repo.ExpireOrders(ctx, 500); err == nil {
		orders = n
	}
	return
}

// auditCatalog 商品改动进审计。
//
// ⚠️ 改价必须留痕：「这一单为什么是 99 而不是 199」在半年后只有审计说得清
// —— 订单里存的是快照，但"谁在什么时候把价改成了这个数"不在订单上。
// ⚠️ 商品没有密钥字段，所以整条 after 可以入库；有密钥的配置只记版本号。
func (s *Service) auditCatalog(ctx context.Context, adminID int64, action string, after any) {
	if s.audit == nil {
		return
	}
	s.audit.Write(ctx, audit.Entry{
		ActorType: audit.ActorAdmin, ActorID: adminID,
		Action: action, ObjectType: "billing_catalog", After: after,
	})
}

// ---------------------------------------------------------------- 支付域端口

// FindByOutTradeNo 实现 `payment/app.OrderGateway`。
//
// ⚠️ 这是**回调**走的路径，没有当前用户 —— 归属由订单自己带出来。
func (s *Service) FindByOutTradeNo(ctx context.Context, outTradeNo string) (
	paymentapp.OrderInfo, error) {
	o, err := s.repo.OrderByOutTradeNo(ctx, outTradeNo)
	if errors.Is(err, repo.ErrNotFound) {
		return paymentapp.OrderInfo{}, paymentapp.ErrOrderNotFound
	}
	if err != nil {
		return paymentapp.OrderInfo{}, err
	}
	return paymentapp.OrderInfo{
		ID: o.ID, PayerID: o.PayerID, AmountCents: o.AmountCents,
		PayStatus: o.PayStatus, Channel: o.Channel,
	}, nil
}

// Fulfil 实现 `payment/app.OrderGateway`。
//
// ⚠️ 回调、查单、开发支付**都走这一个方法**。各写一份的话，
// 同一笔钱在三条路上会被做成三件不同的事，而差异只在生产上暴露。
func (s *Service) Fulfil(ctx context.Context, payerID, orderID int64,
	channel, txnID string) (bool, error) {
	res, err := s.repo.Fulfil(ctx, payerID, orderID, channel, txnID,
		time.Now().UTC())
	if err != nil {
		return false, err
	}
	if res.Order.PayStatus != repo.PayPaid {
		// 订单已被取消/超时关闭 —— 钱收到了但这单不能履约。
		// ⚠️ 报错而不是静默：这笔钱要人工退，不报的话没人知道。
		return false, fmt.Errorf("订单当前是%s，不能履约", orderStatusName(res.Order.PayStatus))
	}
	return res.AlreadyDone, nil
}

// MarkSynced 实现 `payment/app.OrderGateway`。
func (s *Service) MarkSynced(ctx context.Context, orderID int64, note string) error {
	return s.repo.MarkSynced(ctx, orderID, note)
}

// MarkOrderChannel 记下最后一次拉起支付用的渠道（见 repo.MarkChannel）。
func (s *Service) MarkOrderChannel(ctx context.Context, orderID int64,
	channel string) error {
	return s.repo.MarkChannel(ctx, orderID, channel)
}
