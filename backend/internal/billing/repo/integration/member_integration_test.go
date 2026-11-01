//go:build integration

// 会员、商品与订单履约的真库测试（P11-01）。
//
// ⚠️ 履约的铁律**只能在真库里证明**：重复回调不重复发权益靠
// `where pay_status='pending'` 的 CAS，一张订单只发一次积分靠
// `uk_grant_source`。内存假实现测的是"我写的 Go 有没有按我想的调用"，
// 而这里要证明的恰恰是数据库在并发下做了什么。
package integration

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/billing/repo"
)

// seedCatalog 造一个等级 + 两个套餐 + 一个积分包。
func seedCatalog(t *testing.T, r *repo.Repo, pool *pgxpool.Pool) (
	basic, pro repo.Plan, pack repo.Package) {
	t.Helper()
	ctx := context.Background()
	for _, l := range []struct {
		code string
		sort int
	}{{"t-basic", 10}, {"t-pro", 20}} {
		if err := r.SaveLevel(ctx, repo.Level{
			Code: l.code, Name: l.code, Sort: l.sort, IsEnabled: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	levels, err := r.ListLevels(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	byCode := map[string]int64{}
	for _, l := range levels {
		byCode[l.Code] = l.ID
	}
	for _, p := range []struct {
		level  string
		price  int64
		credit int
	}{{"t-basic", 3000, 300}, {"t-pro", 9000, 1200}} {
		if err := r.SavePlan(ctx, repo.Plan{
			LevelID: byCode[p.level], Period: "month", PriceCents: p.price,
			GiftCredits: p.credit, IsEnabled: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.SavePackage(ctx, repo.Package{
		Code: "t-pack", Name: "测试积分包", Credits: 5000,
		PriceCents: 4500, IsEnabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if basic, err = r.PlanFor(ctx, byCode["t-basic"], "month"); err != nil {
		t.Fatal(err)
	}
	if pro, err = r.PlanFor(ctx, byCode["t-pro"], "month"); err != nil {
		t.Fatal(err)
	}
	if pack, err = r.Package(ctx, packID(t, pool)); err != nil {
		t.Fatal(err)
	}
	return
}

func packID(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`select id from credit_package where code='t-pack'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func mkOrder(t *testing.T, r *repo.Repo, in repo.CreateOrderInput) repo.Order {
	t.Helper()
	in.PayerID = payer
	in.PlacedBy = 1
	if in.IdempotencyKey == "" {
		in.IdempotencyKey = "k-" + time.Now().Format("150405.000000000")
	}
	o, err := r.CreateOrder(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// ⚠️ 单行配置表的教训同理：商品目录的 upsert 要**更新同一行**，
// 不能保存一次多一行 —— 多出来的行会让 `PlanFor` 取到哪一行取决于排序。
func TestIntegration_商品目录保存是幂等的(t *testing.T) {
	r, pool := setup(t)
	ctx := context.Background()
	seedCatalog(t, r, pool)
	seedCatalog(t, r, pool) // 再来一遍

	var levels, plans, packs int
	_ = pool.QueryRow(ctx, `select count(*) from member_level where code like 't-%'`).Scan(&levels)
	_ = pool.QueryRow(ctx, `select count(*) from member_plan p
		join member_level l on l.id=p.level_id where l.code like 't-%'`).Scan(&plans)
	_ = pool.QueryRow(ctx, `select count(*) from credit_package where code='t-pack'`).Scan(&packs)
	if levels != 2 || plans != 2 || packs != 1 {
		t.Fatalf("重复保存不该新增：levels=%d plans=%d packs=%d", levels, plans, packs)
	}
}

// ⚠️ 履约三件事必须**一个事务**：分开做的话中间崩一次就会出现
// "订单显示已付款但没有会员"或"发了积分但订单还是待支付"。
func TestIntegration_履约同时落订单会员与积分(t *testing.T) {
	r, pool := setup(t)
	ctx := context.Background()
	basic, _, _ := seedCatalog(t, r, pool)

	start := time.Now().UTC()
	expire := start.AddDate(0, 1, 0)
	o := mkOrder(t, r, repo.CreateOrderInput{
		Type: repo.OrderMembership, RefID: basic.ID, AmountCents: basic.PriceCents,
		Credits: basic.GiftCredits, LevelID: basic.LevelID,
		MembershipStartAt: &start, MembershipExpireAt: &expire,
	})
	if o.PayStatus != repo.PayPending {
		t.Fatalf("新建订单应为待支付，得到 %s", o.PayStatus)
	}

	res, err := r.Fulfil(ctx, payer, o.ID, "dev", "txn-1", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if res.Order.PayStatus != repo.PayPaid || res.GrantID == 0 || res.MembershipID == 0 {
		t.Fatalf("三件事都要落：%+v", res)
	}
	bal, err := r.Balance(ctx, payer)
	if err != nil {
		t.Fatal(err)
	}
	if bal != basic.GiftCredits {
		t.Fatalf("余额应为赠分 %d，得到 %d", basic.GiftCredits, bal)
	}
	m, err := r.CurrentMembership(ctx, payer)
	if err != nil {
		t.Fatal(err)
	}
	if m.LevelID != basic.LevelID {
		t.Fatalf("会员等级不对：%+v", m)
	}

	// ⚠️ 会员赠分**随会员到期**（不是永久）。写反了的话用户买的永久积分
	// 会在会员到期那天凭空消失，而他完全不知道为什么。
	var grantExpire *time.Time
	if err := pool.QueryRow(ctx,
		`select expire_at from credit_grant where id=$1`, res.GrantID).
		Scan(&grantExpire); err != nil {
		t.Fatal(err)
	}
	if grantExpire == nil {
		t.Fatal("会员赠分应有到期时间（随会员到期），不该是永久批次")
	}
}

// ⚠️ **重复回调不得重复发放权益**（GO-04 原文）。靠 CAS 不靠"先查再写"。
func TestIntegration_重复履约不重复发权益(t *testing.T) {
	r, pool := setup(t)
	ctx := context.Background()
	basic, _, _ := seedCatalog(t, r, pool)
	start := time.Now().UTC()
	expire := start.AddDate(0, 1, 0)
	o := mkOrder(t, r, repo.CreateOrderInput{
		Type: repo.OrderMembership, RefID: basic.ID, AmountCents: basic.PriceCents,
		Credits: basic.GiftCredits, LevelID: basic.LevelID,
		MembershipStartAt: &start, MembershipExpireAt: &expire,
	})
	if _, err := r.Fulfil(ctx, payer, o.ID, "dev", "txn-1", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	res2, err := r.Fulfil(ctx, payer, o.ID, "dev", "txn-1", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !res2.AlreadyDone {
		t.Fatal("第二次履约应报告已经做过")
	}
	if res2.GrantID != 0 || res2.MembershipID != 0 {
		t.Fatalf("第二次不该再发：%+v", res2)
	}
	bal, _ := r.Balance(ctx, payer)
	if bal != basic.GiftCredits {
		t.Fatalf("余额应仍是 %d，得到 %d（重复发了）", basic.GiftCredits, bal)
	}
	var members int
	_ = pool.QueryRow(ctx, `select count(*) from membership where payer_account_id=$1`,
		payer).Scan(&members)
	if members != 1 {
		t.Fatalf("会员资格应只有一条，得到 %d", members)
	}
}

// ⚠️ 并发的两个回调同时到 —— 只能有一个真的履约。
// 顺序调用永远先撞上状态检查，那种测试把 CAS 条件整条删掉照样绿。
func TestIntegration_并发履约只有一次生效(t *testing.T) {
	r, pool := setup(t)
	ctx := context.Background()
	_, _, pack := seedCatalog(t, r, pool)
	o := mkOrder(t, r, repo.CreateOrderInput{
		Type: repo.OrderPackage, RefID: pack.ID, AmountCents: pack.PriceCents,
		Credits: pack.Credits,
	})

	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			res, err := r.Fulfil(ctx, payer, o.ID, "dev", "txn-c", time.Now().UTC())
			if err == nil && res.GrantID != 0 {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if granted != 1 {
		t.Fatalf("只应有一次真的发放，实际 %d 次", granted)
	}
	bal, _ := r.Balance(ctx, payer)
	if bal != pack.Credits {
		t.Fatalf("余额应为 %d，得到 %d", pack.Credits, bal)
	}
}

// ⚠️ **积分包发永久积分**（GO-04）：跟会员赠分相反。
func TestIntegration_积分包发永久积分(t *testing.T) {
	r, pool := setup(t)
	ctx := context.Background()
	_, _, pack := seedCatalog(t, r, pool)
	o := mkOrder(t, r, repo.CreateOrderInput{
		Type: repo.OrderPackage, RefID: pack.ID, AmountCents: pack.PriceCents,
		Credits: pack.Credits,
	})
	res, err := r.Fulfil(ctx, payer, o.ID, "dev", "txn-p", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	var expireAt *time.Time
	if err := pool.QueryRow(ctx, `select expire_at from credit_grant where id=$1`,
		res.GrantID).Scan(&expireAt); err != nil {
		t.Fatal(err)
	}
	if expireAt != nil {
		t.Fatalf("积分包应发永久批次，得到到期时间 %v", expireAt)
	}
}

// ⚠️ 升级保持原到期时间 → 旧那条会员的 expire_at 仍在**未来**。
// 标成 expired 的话「当前有效会员」会同时命中两行，取到哪一行取决于排序。
func TestIntegration_升级后旧会员标superseded(t *testing.T) {
	r, pool := setup(t)
	ctx := context.Background()
	basic, pro, _ := seedCatalog(t, r, pool)

	start := time.Now().UTC()
	expire := start.AddDate(0, 1, 0)
	o1 := mkOrder(t, r, repo.CreateOrderInput{
		Type: repo.OrderMembership, RefID: basic.ID, AmountCents: basic.PriceCents,
		Credits: basic.GiftCredits, LevelID: basic.LevelID,
		MembershipStartAt: &start, MembershipExpireAt: &expire,
	})
	if _, err := r.Fulfil(ctx, payer, o1.ID, "dev", "t1", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// 升级：到期时间**不变**。
	o2 := mkOrder(t, r, repo.CreateOrderInput{
		Type: repo.OrderMembership, RefID: pro.ID, AmountCents: 3000,
		Credits: 900, LevelID: pro.LevelID,
		MembershipStartAt: &start, MembershipExpireAt: &expire,
	})
	if _, err := r.Fulfil(ctx, payer, o2.ID, "dev", "t2", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	m, err := r.CurrentMembership(ctx, payer)
	if err != nil {
		t.Fatal(err)
	}
	if m.LevelID != pro.LevelID {
		t.Fatalf("当前等级应为 pro，得到 %s", m.LevelCode)
	}
	var superseded int
	_ = pool.QueryRow(ctx, `select count(*) from membership
		where payer_account_id=$1 and status='superseded'`, payer).Scan(&superseded)
	if superseded != 1 {
		t.Fatalf("旧会员应标 superseded，实际 %d 条", superseded)
	}
	// 历史要留着 —— 客服要查"这次升级补了多少钱"。
	hist, err := r.ListMemberships(ctx, payer, 10)
	if err != nil || len(hist) != 2 {
		t.Fatalf("历史应有两条：%v %d", err, len(hist))
	}
}

// ⚠️ 幂等键重复时把**已有的那张**回给调用方，而不是报错：
// 报错的话用户重复点提交会看到"下单失败"，而订单其实建好了。
func TestIntegration_下单幂等键重复返回同一张单(t *testing.T) {
	r, pool := setup(t)
	_, _, pack := seedCatalog(t, r, pool)
	in := repo.CreateOrderInput{
		Type: repo.OrderPackage, RefID: pack.ID, AmountCents: pack.PriceCents,
		Credits: pack.Credits, IdempotencyKey: "same-key",
	}
	a := mkOrder(t, r, in)
	b := mkOrder(t, r, in)
	if a.ID != b.ID {
		t.Fatalf("同一幂等键应返回同一张单：%d vs %d", a.ID, b.ID)
	}
}

// ⚠️ 已经付过款的订单**不能被取消**，且要如实报告"没改成"——
// 静默成功的话用户以为退了钱而其实没有。
func TestIntegration_已付款订单不能取消(t *testing.T) {
	r, pool := setup(t)
	ctx := context.Background()
	_, _, pack := seedCatalog(t, r, pool)
	o := mkOrder(t, r, repo.CreateOrderInput{
		Type: repo.OrderPackage, RefID: pack.ID, AmountCents: pack.PriceCents,
		Credits: pack.Credits,
	})
	changed, err := r.CancelOrder(ctx, payer, o.ID)
	if err != nil || !changed {
		t.Fatalf("待支付订单应能取消：%v %v", err, changed)
	}
	// 再取消一次 —— 已经不是 pending 了。
	changed, err = r.CancelOrder(ctx, payer, o.ID)
	if err != nil || changed {
		t.Fatalf("重复取消应报告没改成：%v %v", err, changed)
	}
}

// ⚠️ 订单超时**必须清**：它占着幂等键，不清掉的话用户再下同一单
// 会命中那张僵尸订单，看到的是一张早就付不了的单。
func TestIntegration_订单超时被关闭(t *testing.T) {
	r, pool := setup(t)
	ctx := context.Background()
	_, _, pack := seedCatalog(t, r, pool)
	past := time.Now().Add(-time.Hour)
	o := mkOrder(t, r, repo.CreateOrderInput{
		Type: repo.OrderPackage, RefID: pack.ID, AmountCents: pack.PriceCents,
		Credits: pack.Credits, ExpireAt: &past,
	})
	n, err := r.ExpireOrders(ctx, 100)
	if err != nil || n != 1 {
		t.Fatalf("应关掉 1 张：%v %d", err, n)
	}
	got, _ := r.Order(ctx, payer, o.ID)
	if got.PayStatus != repo.PayExpired {
		t.Fatalf("应为已超时关闭，得到 %s", got.PayStatus)
	}
	// 超时的单不能再履约 —— 否则用户可以拿着过期的单去付款。
	res, err := r.Fulfil(ctx, payer, o.ID, "dev", "late", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if res.GrantID != 0 || res.Order.PayStatus != repo.PayExpired {
		t.Fatalf("超时订单不该履约：%+v", res)
	}
}

// ⚠️ 权益判定按 `expire_at > now()` 算，**不依赖过期作业**：
// 作业停一天就有一整天的人在用已经过期的等级权益。
func TestIntegration_过期会员立刻失去权益(t *testing.T) {
	r, pool := setup(t)
	ctx := context.Background()
	basic, _, _ := seedCatalog(t, r, pool)
	start := time.Now().UTC().AddDate(0, -2, 0)
	expired := time.Now().UTC().Add(-time.Hour)
	o := mkOrder(t, r, repo.CreateOrderInput{
		Type: repo.OrderMembership, RefID: basic.ID, AmountCents: basic.PriceCents,
		Credits: basic.GiftCredits, LevelID: basic.LevelID,
		MembershipStartAt: &start, MembershipExpireAt: &expired,
	})
	if _, err := r.Fulfil(ctx, payer, o.ID, "dev", "t", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// 作业还没跑 —— status 仍是 active。
	var status string
	_ = pool.QueryRow(ctx, `select status from membership
		where payer_account_id=$1`, payer).Scan(&status)
	if status != "active" {
		t.Fatalf("前置条件：作业还没跑时应仍是 active，得到 %s", status)
	}
	if _, err := r.CurrentMembership(ctx, payer); err == nil {
		t.Fatal("已过期的会员不该被当作当前有效 —— 权益判定不能依赖过期作业")
	}
	// 作业跑完之后状态字面值才跟上。
	if n, err := r.ExpireMemberships(ctx, 10); err != nil || n != 1 {
		t.Fatalf("应标掉 1 条：%v %d", err, n)
	}
}

// ------------------------------------------------- 退款与权益回收（P11-03）

// ⚠️ 回收三件事必须**一个事务**：建 refund 行、收积分、回收会员。
// 分开做的话中间崩一次就是"积分收了但会员还在"或反过来。
func TestIntegration_退款一次回收积分与会员(t *testing.T) {
	r, pool := setup(t)
	ctx := context.Background()
	basic, _, _ := seedCatalog(t, r, pool)
	start := time.Now().UTC()
	expire := start.AddDate(0, 1, 0)
	o := mkOrder(t, r, repo.CreateOrderInput{
		Type: repo.OrderMembership, RefID: basic.ID, AmountCents: basic.PriceCents,
		Credits: basic.GiftCredits, LevelID: basic.LevelID,
		MembershipStartAt: &start, MembershipExpireAt: &expire,
	})
	if _, err := r.Fulfil(ctx, payer, o.ID, "wechat", "t1", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	row, err := r.RevokeForRefund(ctx, repo.RevokeInput{
		OrderID: o.ID, PayerID: payer, UserID: 1,
		AmountCents: basic.PriceCents, Reason: "测试退款",
		RevokeCredits: basic.GiftCredits, RevokeMembership: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if row.RevokedCredits != int64(basic.GiftCredits) || row.RevokedGrantID == 0 {
		t.Fatalf("应收回积分：%+v", row)
	}
	if row.RevokedMembershipID == 0 {
		t.Fatalf("应回收会员：%+v", row)
	}
	// 余额归零。
	bal, _ := r.Balance(ctx, payer)
	if bal != 0 {
		t.Fatalf("回收后余额应为 0，得到 %d", bal)
	}
	// 会员失效。
	if _, err := r.CurrentMembership(ctx, payer); err == nil {
		t.Fatal("回收后不该还有有效会员")
	}
	// ⚠️ 批次要标 `revoked` 并记下**被哪次退款收回** ——
	// 用户问「我的积分怎么没了」时没有这一列就只能翻流水猜。
	var status string
	var byRefund int64
	_ = pool.QueryRow(ctx, `select status, coalesce(revoked_by_refund_id,0)
		from credit_grant where id=$1`, row.RevokedGrantID).Scan(&status, &byRefund)
	if status != "revoked" || byRefund != row.ID {
		t.Fatalf("批次应标 revoked 并记下退款单：%s %d", status, byRefund)
	}
}

// ⚠️ **升级/续费会把上一条会员标成 superseded**。退掉这一单时如果不恢复，
// 用户退的是升级差价、结果连**原来的基础会员也没了**。
func TestIntegration_退升级单要恢复上一段会员(t *testing.T) {
	r, pool := setup(t)
	ctx := context.Background()
	basic, pro, _ := seedCatalog(t, r, pool)
	start := time.Now().UTC()
	expire := start.AddDate(0, 1, 0)

	o1 := mkOrder(t, r, repo.CreateOrderInput{
		Type: repo.OrderMembership, RefID: basic.ID, AmountCents: basic.PriceCents,
		Credits: 0, LevelID: basic.LevelID,
		MembershipStartAt: &start, MembershipExpireAt: &expire,
	})
	if _, err := r.Fulfil(ctx, payer, o1.ID, "wechat", "t1", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// 升级（保持原到期日）。
	o2 := mkOrder(t, r, repo.CreateOrderInput{
		Type: repo.OrderMembership, RefID: pro.ID, AmountCents: 3000,
		Credits: 0, LevelID: pro.LevelID,
		MembershipStartAt: &start, MembershipExpireAt: &expire,
	})
	if _, err := r.Fulfil(ctx, payer, o2.ID, "wechat", "t2", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if m, _ := r.CurrentMembership(ctx, payer); m.LevelID != pro.LevelID {
		t.Fatal("前置条件：应已升到 pro")
	}

	row, err := r.RevokeForRefund(ctx, repo.RevokeInput{
		OrderID: o2.ID, PayerID: payer, AmountCents: 3000,
		Reason: "退升级单", RevokeMembership: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if row.RestoredMembershipID == 0 {
		t.Fatal("应恢复被顶替的那条会员 —— 否则用户连基础会员也没了")
	}
	m, err := r.CurrentMembership(ctx, payer)
	if err != nil {
		t.Fatalf("退掉升级单之后应仍有基础会员：%v", err)
	}
	if m.LevelID != basic.LevelID {
		t.Fatalf("应恢复到 basic，得到 %s", m.LevelCode)
	}
}

// ⚠️ **用掉一分就不许退**，而且这一条要靠**带条件的更新**兜底 ——
// 申请与回收之间用户可能刚好花掉一笔，前面那次判定挡不住。
func TestIntegration_积分被用掉后回收中止(t *testing.T) {
	r, pool := setup(t)
	ctx := context.Background()
	_, _, pack := seedCatalog(t, r, pool)
	o := mkOrder(t, r, repo.CreateOrderInput{
		Type: repo.OrderPackage, RefID: pack.ID, AmountCents: pack.PriceCents,
		Credits: pack.Credits,
	})
	if _, err := r.Fulfil(ctx, payer, o.ID, "wechat", "t", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// 花掉一分。
	if _, err := r.Consume(ctx, repo.ConsumeInput{
		PayerID: payer, Amount: 1, RefType: "generation", RefID: "g-1",
	}); err != nil {
		t.Fatal(err)
	}
	_, err := r.RevokeForRefund(ctx, repo.RevokeInput{
		OrderID: o.ID, PayerID: payer, AmountCents: pack.PriceCents,
		Reason: "应该退不了", RevokeCredits: pack.Credits,
	})
	if !errors.Is(err, repo.ErrRefundRaced) {
		t.Fatalf("用掉一分就该中止，得到 %v", err)
	}
	// ⚠️ **整笔回滚**：不能留下一个 pending 的退款行。
	var refunds int
	_ = pool.QueryRow(ctx, `select count(*) from refund where order_id=$1`,
		o.ID).Scan(&refunds)
	if refunds != 0 {
		t.Fatalf("中止时不该留下退款行，实际 %d 行", refunds)
	}
	// 余额也不该被动过。
	bal, _ := r.Balance(ctx, payer)
	if bal != pack.Credits-1 {
		t.Fatalf("余额不该被动过，得到 %d", bal)
	}
}

// ⚠️ 一张订单**只能有一笔进行中的退款**，靠部分唯一索引兜底 ——
// 并发的两次申请各自查一遍都会说"还没退过"。
func TestIntegration_并发退款只有一笔生效(t *testing.T) {
	r, pool := setup(t)
	ctx := context.Background()
	_, _, pack := seedCatalog(t, r, pool)
	o := mkOrder(t, r, repo.CreateOrderInput{
		Type: repo.OrderPackage, RefID: pack.ID, AmountCents: pack.PriceCents,
		Credits: pack.Credits,
	})
	if _, err := r.Fulfil(ctx, payer, o.ID, "wechat", "t", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, err := r.RevokeForRefund(ctx, repo.RevokeInput{
				OrderID: o.ID, PayerID: payer, AmountCents: pack.PriceCents,
				Reason: "并发", RevokeCredits: pack.Credits,
			}); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("只应有一笔成功，实际 %d 笔", ok)
	}
	var refunds int
	_ = pool.QueryRow(ctx, `select count(*) from refund where order_id=$1`,
		o.ID).Scan(&refunds)
	if refunds != 1 {
		t.Fatalf("库里只该有一笔退款，实际 %d 笔", refunds)
	}
}

// ⚠️ 退款失败时订单**仍是 paid**（钱没退回去），否则对账会认为这笔已经退了。
func TestIntegration_退款失败订单不转refunded(t *testing.T) {
	r, pool := setup(t)
	ctx := context.Background()
	_, _, pack := seedCatalog(t, r, pool)
	o := mkOrder(t, r, repo.CreateOrderInput{
		Type: repo.OrderPackage, RefID: pack.ID, AmountCents: pack.PriceCents,
		Credits: pack.Credits,
	})
	if _, err := r.Fulfil(ctx, payer, o.ID, "wechat", "t", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	row, err := r.RevokeForRefund(ctx, repo.RevokeInput{
		OrderID: o.ID, PayerID: payer, AmountCents: pack.PriceCents,
		Reason: "会失败", RevokeCredits: pack.Credits,
	})
	if err != nil {
		t.Fatal(err)
	}
	changed, err := r.FinishRefund(ctx, row.ID, false, "", "渠道拒绝")
	if err != nil || !changed {
		t.Fatalf("应记下失败：%v %v", changed, err)
	}
	got, _ := r.Order(ctx, payer, o.ID)
	if got.PayStatus != repo.PayPaid {
		t.Fatalf("退款失败时订单应仍是 paid，得到 %s", got.PayStatus)
	}
	// ⚠️ 重复写结果不能把已经落定的状态再改一次。
	changed, _ = r.FinishRefund(ctx, row.ID, true, "x", "又成功了")
	if changed {
		t.Fatal("已经落定的退款不该被再改一次")
	}
	f, _ := r.OrderRefund(ctx, row.ID)
	if f.Status != "failed" {
		t.Fatalf("状态应停在 failed，得到 %s", f.Status)
	}
}

// 成功时订单转 refunded。
func TestIntegration_退款成功订单转refunded(t *testing.T) {
	r, pool := setup(t)
	ctx := context.Background()
	_, _, pack := seedCatalog(t, r, pool)
	o := mkOrder(t, r, repo.CreateOrderInput{
		Type: repo.OrderPackage, RefID: pack.ID, AmountCents: pack.PriceCents,
		Credits: pack.Credits,
	})
	if _, err := r.Fulfil(ctx, payer, o.ID, "wechat", "t", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	row, _ := r.RevokeForRefund(ctx, repo.RevokeInput{
		OrderID: o.ID, PayerID: payer, AmountCents: pack.PriceCents,
		Reason: "ok", RevokeCredits: pack.Credits,
	})
	if _, err := r.FinishRefund(ctx, row.ID, true, "ch-1", "SUCCESS"); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Order(ctx, payer, o.ID)
	if got.PayStatus != repo.PayRefunded {
		t.Fatalf("退款成功后订单应为 refunded，得到 %s", got.PayStatus)
	}
}

// ⚠️ 对账候选**必须包含 pending 的订单**：「渠道收了钱而回调丢了」
// 正藏在那里；只对已支付的单对账的话，最该被发现的那一类永远发现不了。
func TestIntegration_对账候选含待支付单(t *testing.T) {
	r, pool := setup(t)
	ctx := context.Background()
	_, _, pack := seedCatalog(t, r, pool)
	mk := func(channel string) repo.Order {
		o := mkOrder(t, r, repo.CreateOrderInput{
			Type: repo.OrderPackage, RefID: pack.ID, AmountCents: pack.PriceCents,
			Credits: pack.Credits,
		})
		if channel != "" {
			if err := r.MarkChannel(ctx, o.ID, channel); err != nil {
				t.Fatal(err)
			}
		}
		return o
	}
	pendingWithChannel := mk("wechat")
	mk("") // 没拉起过支付 —— 不该进候选
	mk("dev")

	cands, err := r.ReconcileCandidates(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range cands {
		if c.ID == pendingWithChannel.ID {
			found = true
		}
		if c.Channel == "" || c.Channel == "dev" {
			t.Fatalf("没拉起过支付 / 开发支付的单不该进候选：%+v", c)
		}
	}
	if !found {
		t.Fatal("待支付且拉起过支付的单必须进候选 —— 回调丢了正藏在这里")
	}

	// 落一条差异并能查出来。
	if err := r.MarkReconciled(ctx, pendingWithChannel.ID, "diff",
		"渠道已收款 1000 分但本地未履约"); err != nil {
		t.Fatal(err)
	}
	diffs, err := r.ReconcileDiffs(ctx, 50)
	if err != nil || len(diffs) != 1 {
		t.Fatalf("应查到 1 条差异：%v %d", err, len(diffs))
	}
	if diffs[0].ReconcileDetail == "" {
		t.Fatal("差异的具体内容要留着 —— 只标一个 diff 等于只说'有问题'")
	}
	_ = pool
}

// ------------------------------------------------- 后台检索与调账（P11-04）

// ⚠️ 汇总要与列表**同一套过滤条件**：分开算的话运营看到的
// "共 132 笔 / ¥8,900" 与下面列出来的不是同一批，而两个数字都"没错"。
func TestIntegration_后台检索与汇总同一套条件(t *testing.T) {
	r, pool := setup(t)
	ctx := context.Background()
	_, _, pack := seedCatalog(t, r, pool)

	// 造三张单：一张已付、一张待付、一张已付但换个渠道。
	paid := mkOrder(t, r, repo.CreateOrderInput{
		Type: repo.OrderPackage, RefID: pack.ID, AmountCents: 1000, Credits: 100,
	})
	if _, err := r.Fulfil(ctx, payer, paid.ID, "wechat", "t1", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	pending := mkOrder(t, r, repo.CreateOrderInput{
		Type: repo.OrderPackage, RefID: pack.ID, AmountCents: 2000, Credits: 200,
	})
	if err := r.MarkChannel(ctx, pending.ID, "alipay"); err != nil {
		t.Fatal(err)
	}

	// 不筛：两张都在，汇总只算已支付的金额。
	all, err := r.SearchOrders(ctx, repo.OrderFilter{})
	if err != nil {
		t.Fatal(err)
	}
	sum, err := r.SummarizeOrders(ctx, repo.OrderFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || sum.Count != 2 {
		t.Fatalf("应有 2 张单：list=%d sum=%d", len(all), sum.Count)
	}
	// ⚠️ 只算**已支付**的：把待支付的也算进收入会让报表凭空多出一截。
	if sum.PaidCount != 1 || sum.PaidCents != 1000 {
		t.Fatalf("汇总应只算已支付：%+v", sum)
	}

	// 按渠道筛 —— 列表与汇总必须一致。
	f := repo.OrderFilter{Channel: "alipay"}
	list, err := r.SearchOrders(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := r.SummarizeOrders(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || s2.Count != 1 {
		t.Fatalf("按渠道筛后列表与汇总应一致：list=%d sum=%d", len(list), s2.Count)
	}
	if list[0].ID != pending.ID {
		t.Fatalf("筛出来的应是 alipay 那张：%d", list[0].ID)
	}
}

// ⚠️ 关键词共用一个输入框：客服手上拿到的可能是订单号、付费主体或渠道交易号。
func TestIntegration_关键词三种都能查到(t *testing.T) {
	r, pool := setup(t)
	ctx := context.Background()
	_, _, pack := seedCatalog(t, r, pool)
	o := mkOrder(t, r, repo.CreateOrderInput{
		Type: repo.OrderPackage, RefID: pack.ID, AmountCents: 1000, Credits: 100,
	})
	if _, err := r.Fulfil(ctx, payer, o.ID, "wechat", "txn-abc-123",
		time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	for name, kw := range map[string]string{
		"订单号":   itoaTest(o.ID),
		"付费主体":  itoaTest(payer),
		"渠道交易号": "abc",
	} {
		list, err := r.SearchOrders(ctx, repo.OrderFilter{Keyword: kw})
		if err != nil {
			t.Fatalf("%s 查询出错：%v", name, err)
		}
		if len(list) == 0 {
			t.Fatalf("按%s（%q）应查得到", name, kw)
		}
	}
	// ⚠️ 关键词是后台输入框来的 —— 拼进 SQL 的话就是注入口。
	// 这一条证明参数化生效：带引号的输入不会让查询崩掉。
	if _, err := r.SearchOrders(ctx, repo.OrderFilter{
		Keyword: "'; drop table purchase_order; --",
	}); err != nil {
		t.Fatalf("恶意关键词不该让查询出错（参数化应生效）：%v", err)
	}
	var still int
	_ = pool.QueryRow(ctx, `select count(*) from purchase_order`).Scan(&still)
	if still == 0 {
		t.Fatal("表没了 —— 关键词被拼进了 SQL")
	}
}

// ⚠️ 订单详情要**含已回收的批次**：只列 active 的话，一张退过款的订单
// 在详情页上看起来"从来没发过积分"，而客服正是来查这个的。
func TestIntegration_订单详情含已回收批次与回调数(t *testing.T) {
	r, pool := setup(t)
	ctx := context.Background()
	_, _, pack := seedCatalog(t, r, pool)
	o := mkOrder(t, r, repo.CreateOrderInput{
		Type: repo.OrderPackage, RefID: pack.ID, AmountCents: pack.PriceCents,
		Credits: pack.Credits,
	})
	if _, err := r.Fulfil(ctx, payer, o.ID, "wechat", "t", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	row, err := r.RevokeForRefund(ctx, repo.RevokeInput{
		OrderID: o.ID, PayerID: payer, AmountCents: pack.PriceCents,
		Reason: "退", RevokeCredits: pack.Credits,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.FinishRefund(ctx, row.ID, true, "ch", "SUCCESS"); err != nil {
		t.Fatal(err)
	}

	d, err := r.AdminOrderDetail(ctx, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Grants) != 1 {
		t.Fatalf("应列出那个批次（哪怕已回收），实际 %d 个", len(d.Grants))
	}
	if d.Grants[0].Status != "revoked" {
		t.Fatalf("批次状态应为 revoked，得到 %s", d.Grants[0].Status)
	}
	if len(d.Refunds) != 1 {
		t.Fatalf("应有 1 笔退款，实际 %d 笔", len(d.Refunds))
	}
	// ⚠️ 回调条数是「回调丢了」最直接的证据。
	if d.CallbackCount != 0 {
		t.Fatalf("这一单没有回调，应为 0，得到 %d", d.CallbackCount)
	}
	_ = pool
}

func itoaTest(v int64) string {
	s := ""
	if v == 0 {
		return "0"
	}
	for v > 0 {
		s = string(rune('0'+v%10)) + s
		v /= 10
	}
	return s
}

// ⚠️ **加与减是两条不同的路**：加 = 发新批次，减 = 按消费顺序从现有批次扣。
// 写成"直接改某个批次的 remaining"的话，逐批明细与流水就对不上了。
func TestIntegration_人工调账加与减(t *testing.T) {
	r, pool := setup(t)
	ctx := context.Background()

	// 加。
	res, err := r.AdjustCredits(ctx, repo.AdjustInput{
		PayerID: payer, AdminID: 7, Amount: 500, Reason: "补偿",
		IdempotencyKey: "adj-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.GrantID == 0 || res.BalanceAfter != 500 {
		t.Fatalf("加积分应发新批次：%+v", res)
	}
	// 减。
	res, err = r.AdjustCredits(ctx, repo.AdjustInput{
		PayerID: payer, AdminID: 7, Amount: -200, Reason: "扣回",
		IdempotencyKey: "adj-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.GrantID != 0 {
		t.Fatal("扣减不该发新批次")
	}
	if res.BalanceAfter != 300 {
		t.Fatalf("余额应为 300，得到 %d", res.BalanceAfter)
	}
	// 流水要带操作者与原因 —— 事后审计只有这两列能回答"谁、为什么"。
	var admin int64
	var reason string
	_ = pool.QueryRow(ctx, `select coalesce(operator_admin_id,0), coalesce(reason,'')
		from credit_transaction where ref_type='admin_adjust' and ref_id='adj-2'`).
		Scan(&admin, &reason)
	if admin != 7 || reason != "扣回" {
		t.Fatalf("流水应记操作者与原因：%d %q", admin, reason)
	}
}

// ⚠️ 幂等号重复只生效一次 —— 后台按钮一慢就会被点第二次，而这一下是直接改钱。
func TestIntegration_调账幂等(t *testing.T) {
	r, _ := setup(t)
	ctx := context.Background()
	in := repo.AdjustInput{
		PayerID: payer, AdminID: 7, Amount: 500, Reason: "补偿",
		IdempotencyKey: "adj-same",
	}
	if _, err := r.AdjustCredits(ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AdjustCredits(ctx, in); !errors.Is(err, repo.ErrAdjustDuplicate) {
		t.Fatalf("同一个单号应只生效一次，得到 %v", err)
	}
	bal, _ := r.Balance(ctx, payer)
	if bal != 500 {
		t.Fatalf("余额应仍是 500，得到 %d", bal)
	}
}

// ⚠️ 并发下也只能生效一次 —— 顺序调用永远先撞上"已存在"，
// 那种测试把唯一索引删掉照样绿。
func TestIntegration_并发调账只生效一次(t *testing.T) {
	r, _ := setup(t)
	ctx := context.Background()
	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, err := r.AdjustCredits(ctx, repo.AdjustInput{
				PayerID: payer, AdminID: 7, Amount: 100, Reason: "并发",
				IdempotencyKey: "adj-concurrent",
			}); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("只应生效一次，实际 %d 次", ok)
	}
	bal, _ := r.Balance(ctx, payer)
	if bal != 100 {
		t.Fatalf("余额应为 100，得到 %d", bal)
	}
}

// ⚠️ 余额不足**整笔拒绝**，不扣到 0：扣到 0 的话管理员以为扣了 500
// 而实际只扣了 120，审计里记的又是 500 —— 三方对不上。
func TestIntegration_调账扣减余额不足整笔拒绝(t *testing.T) {
	r, _ := setup(t)
	ctx := context.Background()
	if _, err := r.AdjustCredits(ctx, repo.AdjustInput{
		PayerID: payer, AdminID: 7, Amount: 120, Reason: "先加一点",
		IdempotencyKey: "adj-base",
	}); err != nil {
		t.Fatal(err)
	}
	_, err := r.AdjustCredits(ctx, repo.AdjustInput{
		PayerID: payer, AdminID: 7, Amount: -500, Reason: "扣太多",
		IdempotencyKey: "adj-too-much",
	})
	if err == nil {
		t.Fatal("余额不足应整笔拒绝")
	}
	bal, _ := r.Balance(ctx, payer)
	if bal != 120 {
		t.Fatalf("余额不该被动过（不能扣到 0），得到 %d", bal)
	}
}

// ⚠️ 挂在同一个付费主体下的工作区**可能不止一个** ——
// 那意味着一个团队的消费会记到个人账上。后台要能一眼看出来。
func TestIntegration_付费主体全貌(t *testing.T) {
	r, pool := setup(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		insert into payer_account (id, kind, owner_id) values ($1,'user',$2)
		on conflict (kind, owner_id) do nothing`, payer, 999); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AdjustCredits(ctx, repo.AdjustInput{
		PayerID: payer, AdminID: 7, Amount: 300, Reason: "x",
		IdempotencyKey: "adj-ov",
	}); err != nil {
		t.Fatal(err)
	}
	v, err := r.PayerOverview(ctx, payer)
	if err != nil {
		t.Fatal(err)
	}
	if v.Kind != "user" || v.Balance != 300 {
		t.Fatalf("全貌不对：%+v", v)
	}
	if _, err := r.PayerOverview(ctx, 999999999); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("不存在的付费主体应报 NotFound，得到 %v", err)
	}
}
