//go:build integration

// 工作区用量审计与多租户并发负向测试（P12-04；NEW-20、FULL-06、AC-02）。
//
// ⚠️ 这一组必须用真库：要守的每条都由 **SQL 与事务**决定 ——
// 退款按原消费的操作者归集、无操作者那一行不能丢、
// 以及"两个人同时消费同一个团队账户，余额只够一个"。
// 最后一条尤其：mock 仓储里永远不会发生真正的并发。
package integration

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	billingdomain "github.com/li904338203/tabtv/backend/internal/billing/domain"
	"github.com/li904338203/tabtv/backend/internal/billing/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
)

func usageSetup(t *testing.T) (*repo.Repo, *pgxpool.Pool, func() int64) {
	t.Helper()
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	return repo.New(pool, gen.Next), pool, gen.Next
}

func mkPayer(t *testing.T, pool *pgxpool.Pool, next func() int64) int64 {
	t.Helper()
	id := next()
	if _, err := pool.Exec(context.Background(), `
		insert into payer_account (id, kind, owner_id) values ($1, 'workspace', $2)`,
		id, next()); err != nil {
		t.Fatal(err)
	}
	return id
}

func mkActor(t *testing.T, pool *pgxpool.Pool, next func() int64, nickname, phone string) int64 {
	t.Helper()
	id := next()
	if _, err := pool.Exec(context.Background(), `
		insert into app_user (id, phone, nickname, password_hash)
		values ($1,$2,$3,'x')`, id, phone, nickname); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestWorkspaceUsage_按操作者汇总且退款归回原消费者(t *testing.T) {
	r, pool, next := usageSetup(t)
	ctx := context.Background()
	payer := mkPayer(t, pool, next)
	alice := mkActor(t, pool, next, "小 A", "13200000001")
	bob := mkActor(t, pool, next, "小 B", "13200000002")

	if _, err := r.Grant(ctx, repo.GrantInput{
		PayerID: payer, Source: "admin_adjust", Amount: 1000, Reason: "测试",
	}); err != nil {
		t.Fatal(err)
	}

	consume := func(user int64, amount int, ref string) {
		t.Helper()
		if _, err := r.Consume(ctx, repo.ConsumeInput{
			PayerID: payer, Amount: amount, RefType: "generation", RefID: ref,
			UserID: user, Reason: "text2img",
		}); err != nil {
			t.Fatal(err)
		}
	}
	consume(alice, 100, "g1")
	consume(alice, 50, "g2")
	consume(bob, 30, "g3")

	// 任务失败自动退款：那条流水**自己没有操作者**。
	if _, err := r.Refund(ctx, repo.RefundInput{
		PayerID: payer, RefType: "generation", RefID: "g2", Reason: "任务失败",
	}); err != nil {
		t.Fatal(err)
	}

	members, sum, err := r.WorkspaceUsage(ctx, payer, repo.UsageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	byUser := map[int64]repo.MemberUsage{}
	for _, m := range members {
		byUser[m.UserID] = m
	}

	// ⚠️ 退款必须归回**原消费**的操作者。按退款流水自己的 operator 分组的话，
	// 扣费算在小 A 头上、退款算在"未知"头上 —— 两边对不齐，
	// 而小 A 看起来花了 150 而实际只花了 100。
	if got := byUser[alice]; got.Consumed != 150 || got.Refunded != 50 {
		t.Fatalf("小 A 应是 消耗150/退回50，得到 %+v", got)
	}
	if got := byUser[bob]; got.Consumed != 30 || got.Refunded != 0 {
		t.Fatalf("小 B 应是 消耗30/退回0，得到 %+v", got)
	}
	// ⚠️ 任务数按 ref 去重，**不是流水条数**：退款会再加一条流水，
	// 数流水的话"任务数"会随失败率上涨。
	if got := byUser[alice].Tasks; got != 2 {
		t.Fatalf("小 A 应是 2 个任务，得到 %d", got)
	}
	// 消耗与退回**分开给，不合并成净值** —— 合并之后对不上账本流水。
	if sum.Consumed != 180 || sum.Refunded != 50 {
		t.Fatalf("汇总应是 消耗180/退回50，得到 %+v", sum)
	}
	if sum.Members != 2 {
		t.Fatalf("应有 2 个有用量的人，得到 %d", sum.Members)
	}
	if !byUser[alice].LastAt.After(time.Time{}) {
		t.Fatal("应给出最近一次用量时间")
	}
}

// ⚠️ 没有操作者的流水（过期扣减、系统调账）要**单独成行不能丢**：
// 丢掉的话各成员之和对不上总额，而那个差额没有任何人解释得了。
func TestWorkspaceUsage_无操作者的流水单独成行(t *testing.T) {
	r, pool, next := usageSetup(t)
	ctx := context.Background()
	payer := mkPayer(t, pool, next)
	alice := mkActor(t, pool, next, "小 A", "13200000011")

	if _, err := r.Grant(ctx, repo.GrantInput{
		PayerID: payer, Source: "admin_adjust", Amount: 500,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Consume(ctx, repo.ConsumeInput{
		PayerID: payer, Amount: 100, RefType: "generation", RefID: "g1",
		UserID: alice,
	}); err != nil {
		t.Fatal(err)
	}
	// 一笔没有操作者的消费（例如系统发起的清理）。
	if _, err := r.Consume(ctx, repo.ConsumeInput{
		PayerID: payer, Amount: 20, RefType: "system", RefID: "s1",
	}); err != nil {
		t.Fatal(err)
	}

	members, sum, err := r.WorkspaceUsage(ctx, payer, repo.UsageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var unknown *repo.MemberUsage
	total := 0
	for i := range members {
		total += members[i].Consumed
		if members[i].UserID == 0 {
			unknown = &members[i]
		}
	}
	if unknown == nil || unknown.Consumed != 20 {
		t.Fatalf("无操作者的流水应单独成行（20），得到 %+v", members)
	}
	// 各行之和必须等于总额 —— 这正是"不能丢"的可验证形式。
	if total != sum.Consumed {
		t.Fatalf("各成员之和 %d 应等于总额 %d", total, sum.Consumed)
	}
}

// 时间窗过滤：只统计窗内的流水。
func TestWorkspaceUsage_时间窗(t *testing.T) {
	r, pool, next := usageSetup(t)
	ctx := context.Background()
	payer := mkPayer(t, pool, next)
	alice := mkActor(t, pool, next, "小 A", "13200000021")
	if _, err := r.Grant(ctx, repo.GrantInput{
		PayerID: payer, Source: "admin_adjust", Amount: 500,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Consume(ctx, repo.ConsumeInput{
		PayerID: payer, Amount: 100, RefType: "generation", RefID: "g1", UserID: alice,
	}); err != nil {
		t.Fatal(err)
	}
	// 把它挪到一天前。
	if _, err := pool.Exec(ctx, `
		update credit_transaction set created_at = now() - interval '1 day'
		where payer_account_id = $1 and ref_id = 'g1'`, payer); err != nil {
		t.Fatal(err)
	}
	from := time.Now().Add(-time.Hour)
	_, sum, err := r.WorkspaceUsage(ctx, payer, repo.UsageFilter{From: &from})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Consumed != 0 {
		t.Fatalf("窗外的流水不该被统计，得到 %+v", sum)
	}
	_, sum, err = r.WorkspaceUsage(ctx, payer, repo.UsageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Consumed != 100 {
		t.Fatalf("不传时间窗应统计全部，得到 %+v", sum)
	}
}

// ⚠️ **多租户隔离**：一个付费主体的用量里不能混进另一个的流水。
func TestWorkspaceUsage_付费主体隔离(t *testing.T) {
	r, pool, next := usageSetup(t)
	ctx := context.Background()
	team := mkPayer(t, pool, next)
	personal := mkPayer(t, pool, next)
	alice := mkActor(t, pool, next, "小 A", "13200000031")

	for _, p := range []int64{team, personal} {
		if _, err := r.Grant(ctx, repo.GrantInput{
			PayerID: p, Source: "admin_adjust", Amount: 500,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 同一个人在两个主体下各花一笔 —— 这正是现实中最容易混的情形。
	if _, err := r.Consume(ctx, repo.ConsumeInput{
		PayerID: team, Amount: 100, RefType: "generation", RefID: "t1", UserID: alice,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Consume(ctx, repo.ConsumeInput{
		PayerID: personal, Amount: 7, RefType: "generation", RefID: "p1", UserID: alice,
	}); err != nil {
		t.Fatal(err)
	}

	_, sum, err := r.WorkspaceUsage(ctx, team, repo.UsageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Consumed != 100 {
		t.Fatalf("团队用量不该混进个人的那 7 分，得到 %+v", sum)
	}
	_, sum, err = r.WorkspaceUsage(ctx, personal, repo.UsageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Consumed != 7 {
		t.Fatalf("个人用量应是 7，得到 %+v", sum)
	}
}

// ⚠️ **多人并发消费同一个团队账户，余额只够一部分** —— 绝不能扣成负数。
//
// 这是协作场景下最直接的账务风险：团队里几个人同时点生成，
// 余额检查与扣减若不在同一个事务里（或没有 `for update`），
// 每个人都会看到"够"，然后一起扣下去。
// 顺序调用永远测不出来 —— 必须真库多 goroutine 抢同一个账户。
func TestConsume_多人并发不超支(t *testing.T) {
	r, pool, next := usageSetup(t)
	ctx := context.Background()
	payer := mkPayer(t, pool, next)

	const unit, budget, racers = 30, 100, 8 // 100 只够 3 次
	if _, err := r.Grant(ctx, repo.GrantInput{
		PayerID: payer, Source: "admin_adjust", Amount: budget,
	}); err != nil {
		t.Fatal(err)
	}

	users := make([]int64, racers)
	for i := range users {
		users[i] = mkActor(t, pool, next, "并发", "1330000"+itoa4(i)+"0")
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, denied, other := 0, 0, 0
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := r.Consume(ctx, repo.ConsumeInput{
				PayerID: payer, Amount: unit, RefType: "generation",
				RefID: "race-" + itoa4(i), UserID: users[i],
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, billingdomain.ErrInsufficient):
				denied++
			default:
				other++
				t.Logf("第 %d 个并发得到非预期错误：%v", i, err)
			}
		}(i)
	}
	wg.Wait()

	if other != 0 {
		t.Fatalf("不该出现非预期错误，%d 次", other)
	}
	if ok != budget/unit {
		t.Fatalf("应恰好成功 %d 次，实际 %d 次（拒绝 %d）", budget/unit, ok, denied)
	}
	// 余额**绝不能为负**。
	bal, err := r.Balance(ctx, payer)
	if err != nil {
		t.Fatal(err)
	}
	if bal < 0 {
		t.Fatalf("余额被扣成负数：%d", bal)
	}
	if bal != budget-ok*unit {
		t.Fatalf("余额应是 %d，实际 %d", budget-ok*unit, bal)
	}
	// 用量审计里也要对得上：成功几次就是几个任务。
	_, sum, err := r.WorkspaceUsage(ctx, payer, repo.UsageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Tasks != ok || sum.Consumed != ok*unit {
		t.Fatalf("审计应与实际一致（%d 任务 / %d 积分），得到 %+v", ok, ok*unit, sum)
	}
}

func itoa4(i int) string {
	const d = "0123456789"
	return string([]byte{d[(i/1000)%10], d[(i/100)%10], d[(i/10)%10], d[i%10]})
}
