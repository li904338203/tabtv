//go:build integration

// 积分账本的真库测试。
//
// ⚠️ 账本的几条铁律**只能在真库里证明**：并发扣费不超卖靠 `for update`，
// 一次性靠唯一索引，跨批扣款靠 SQL 的排序。mock 仓储测的是"我写的 Go
// 有没有按我想的调用"，而这里要证明的恰恰是数据库做了什么。
package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/billing/domain"
	"github.com/li904338203/tabtv/backend/internal/billing/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
)

const payer = int64(4001)

func setup(t *testing.T) (*repo.Repo, *pgxpool.Pool) {
	t.Helper()
	// 每个包共用一个容器，用例之间靠清表隔离（见 testsupport 包注释）。
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	return repo.New(pool, gen.Next), pool
}

func grantN(t *testing.T, r *repo.Repo, amount int, expireDays int, source string) int64 {
	t.Helper()
	var exp *time.Time
	if expireDays > 0 {
		e := time.Now().AddDate(0, 0, expireDays)
		exp = &e
	}
	gid, err := r.Grant(context.Background(), repo.GrantInput{
		PayerID: payer, Source: source, Amount: amount, ExpireAt: exp,
		SourceRefID: fmt.Sprintf("%s-%d-%d", source, amount, time.Now().UnixNano()),
	})
	if err != nil {
		t.Fatal(err)
	}
	return gid
}

// 先扣快到期的赠送积分，永久购买积分最后扣。
func TestConsume_先扣将过期的批次(t *testing.T) {
	r, pool := setup(t)
	ctx := context.Background()
	permanent := grantN(t, r, 100, 0, "purchase")
	far := grantN(t, r, 50, 30, "membership")
	near := grantN(t, r, 30, 7, "promotion")

	res, err := r.Consume(ctx, repo.ConsumeInput{
		PayerID: payer, Amount: 60, RefType: "generation", RefID: "g-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Allocations) != 2 {
		t.Fatalf("应跨 2 批，实际 %+v", res.Allocations)
	}
	if res.Allocations[0].GrantID != near || res.Allocations[0].Amount != 30 {
		t.Errorf("应先扣完将过期的批次，实际 %+v", res.Allocations[0])
	}
	if res.Allocations[1].GrantID != far || res.Allocations[1].Amount != 30 {
		t.Errorf("其次扣较远的批次，实际 %+v", res.Allocations[1])
	}

	var remaining int
	_ = pool.QueryRow(ctx, `select remaining from credit_grant where id=$1`, permanent).Scan(&remaining)
	if remaining != 100 {
		t.Errorf("永久批次不该被动，实际剩 %d", remaining)
	}
	if res.BalanceAfter != 120 {
		t.Errorf("余额应为 180-60=120，实际 %d", res.BalanceAfter)
	}

	// 用尽的批次要标 exhausted，否则下次还会被 for update 锁一遍。
	var status string
	_ = pool.QueryRow(ctx, `select status from credit_grant where id=$1`, near).Scan(&status)
	if status != "exhausted" {
		t.Errorf("扣完的批次应为 exhausted，实际 %s", status)
	}
}

// 余额检查与扣减拆成两步就是超卖。
func TestConsume_并发不超卖(t *testing.T) {
	r, pool := setup(t)
	ctx := context.Background()
	grantN(t, r, 100, 0, "purchase") // 只够 10 次 × 10 分

	const workers = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, insufficient := 0, 0
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			_, err := r.Consume(ctx, repo.ConsumeInput{
				PayerID: payer, Amount: 10,
				RefType: "generation", RefID: fmt.Sprintf("concurrent-%d", i),
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, domain.ErrInsufficient):
				insufficient++
			default:
				t.Errorf("意外错误: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if ok != 10 || insufficient != 10 {
		t.Fatalf("100 分只够 10 次：期望成功 10 / 不足 10，实际 %d / %d", ok, insufficient)
	}
	var balance int
	_ = pool.QueryRow(ctx,
		`select coalesce(sum(remaining),0) from credit_grant where payer_account_id=$1`, payer).
		Scan(&balance)
	if balance != 0 {
		t.Fatalf("余额应恰好扣完为 0，实际 %d（负数=超卖）", balance)
	}
}

// 同一业务只能扣一次：靠唯一索引兜底，不依赖 Redis 命中。
func TestConsume_同一业务引用只能扣一次(t *testing.T) {
	r, _ := setup(t)
	ctx := context.Background()
	grantN(t, r, 100, 0, "purchase")

	in := repo.ConsumeInput{PayerID: payer, Amount: 10, RefType: "generation", RefID: "same"}
	if _, err := r.Consume(ctx, in); err != nil {
		t.Fatal(err)
	}
	_, err := r.Consume(ctx, in)
	if !errors.Is(err, repo.ErrDuplicate) {
		t.Fatalf("重复扣费应被唯一索引拒绝，实际 %v", err)
	}
	// ⚠️ 关键：被拒的那次**不能留下扣减**。
	if bal, _ := r.Balance(ctx, payer); bal != 90 {
		t.Fatalf("余额应仍为 90（只扣了一次），实际 %d", bal)
	}
}

// 并发重复提交同一业务：只有一个能成功，其余全部落唯一索引。
func TestConsume_并发重复提交(t *testing.T) {
	r, _ := setup(t)
	ctx := context.Background()
	grantN(t, r, 1000, 0, "purchase")

	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, err := r.Consume(ctx, repo.ConsumeInput{
				PayerID: payer, Amount: 50, RefType: "generation", RefID: "dup",
			})
			if err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("同一业务只应成功一次，实际 %d 次", ok)
	}
	if bal, _ := r.Balance(ctx, payer); bal != 950 {
		t.Fatalf("只该扣 50，实际余额 %d", bal)
	}
}

// 退款按原批次回补，不凭空发永久积分（旧系统把赠送积分洗成永久的历史 bug）。
func TestRefund_按原批次回补(t *testing.T) {
	r, pool := setup(t)
	ctx := context.Background()
	near := grantN(t, r, 30, 7, "promotion")
	perm := grantN(t, r, 100, 0, "purchase")

	if _, err := r.Consume(ctx, repo.ConsumeInput{
		PayerID: payer, Amount: 50, RefType: "generation", RefID: "g-refund",
	}); err != nil {
		t.Fatal(err)
	}
	res, err := r.Refund(ctx, repo.RefundInput{
		PayerID: payer, RefType: "generation", RefID: "g-refund",
		CompensationValidFor: 30 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 50 || res.Compensated != 0 {
		t.Fatalf("应全额回补原批次，实际 %+v", res)
	}

	var nearRemaining, permRemaining int
	_ = pool.QueryRow(ctx, `select remaining from credit_grant where id=$1`, near).Scan(&nearRemaining)
	_ = pool.QueryRow(ctx, `select remaining from credit_grant where id=$1`, perm).Scan(&permRemaining)
	if nearRemaining != 30 || permRemaining != 100 {
		t.Fatalf("应各自回到原值，实际 近=%d 永久=%d", nearRemaining, permRemaining)
	}
	// 用尽后又被回补的批次要变回 active，否则这笔钱等于消失了。
	var status string
	_ = pool.QueryRow(ctx, `select status from credit_grant where id=$1`, near).Scan(&status)
	if status != "active" {
		t.Errorf("回补后应变回 active，实际 %s", status)
	}
	// 不该凭空多出永久批次。
	var permCount int
	_ = pool.QueryRow(ctx,
		`select count(*) from credit_grant where payer_account_id=$1 and expire_at is null`, payer).
		Scan(&permCount)
	if permCount != 1 {
		t.Errorf("退款不该新发永久批次，实际永久批次 %d 个", permCount)
	}
}

// 过期批次不复活：复活后它立刻又会被过期扫描收走，等于没退。
func TestRefund_过期批次改发补偿(t *testing.T) {
	r, pool := setup(t)
	ctx := context.Background()
	near := grantN(t, r, 40, 7, "promotion")
	if _, err := r.Consume(ctx, repo.ConsumeInput{
		PayerID: payer, Amount: 40, RefType: "generation", RefID: "g-exp",
	}); err != nil {
		t.Fatal(err)
	}
	// 消费之后那个批次过期了。
	if _, err := pool.Exec(ctx,
		`update credit_grant set expire_at = now() - interval '1 day' where id=$1`, near); err != nil {
		t.Fatal(err)
	}

	res, err := r.Refund(ctx, repo.RefundInput{
		PayerID: payer, RefType: "generation", RefID: "g-exp",
		CompensationValidFor: 30 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Compensated != 40 {
		t.Fatalf("过期部分应走补偿，实际 %+v", res)
	}

	var remaining int
	_ = pool.QueryRow(ctx, `select remaining from credit_grant where id=$1`, near).Scan(&remaining)
	if remaining != 0 {
		t.Errorf("过期批次不该被复活，实际剩 %d", remaining)
	}

	var amount int
	var expireAt *time.Time
	var compOf *int64
	if err := pool.QueryRow(ctx, `
		select amount, expire_at, compensation_of_grant_id from credit_grant
		where payer_account_id=$1 and source='expiry_compensation'`, payer).
		Scan(&amount, &expireAt, &compOf); err != nil {
		t.Fatalf("应有一条补偿批次: %v", err)
	}
	if amount != 40 {
		t.Errorf("补偿金额应为 40，实际 %d", amount)
	}
	// ⚠️ 补偿批次必须有到期时间，不能变成永久积分。
	if expireAt == nil {
		t.Error("补偿批次不该是永久的 —— 那正是要避免的「赠送变永久」")
	}
	if compOf == nil || *compOf != near {
		t.Errorf("补偿批次应指回原批次 %d，实际 %v", near, compOf)
	}
}

// 退款一次性：同一业务只能退一次。
func TestRefund_只能成功一次(t *testing.T) {
	r, _ := setup(t)
	ctx := context.Background()
	grantN(t, r, 100, 0, "purchase")
	_, _ = r.Consume(ctx, repo.ConsumeInput{
		PayerID: payer, Amount: 30, RefType: "generation", RefID: "once"})

	in := repo.RefundInput{PayerID: payer, RefType: "generation", RefID: "once"}
	if _, err := r.Refund(ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Refund(ctx, in); !errors.Is(err, repo.ErrDuplicate) {
		t.Fatalf("重复退款应被拒，实际 %v", err)
	}
	if bal, _ := r.Balance(ctx, payer); bal != 100 {
		t.Fatalf("只该退一次，余额应为 100，实际 %d", bal)
	}
}

func TestRefund_没有消费记录时报未找到(t *testing.T) {
	r, _ := setup(t)
	_, err := r.Refund(context.Background(), repo.RefundInput{
		PayerID: payer, RefType: "generation", RefID: "never-consumed"})
	if !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("应报未找到，实际 %v", err)
	}
}

// 发放幂等：会员按月发放、支付回调都可能重放。
func TestGrant_同一来源只发一次(t *testing.T) {
	r, _ := setup(t)
	ctx := context.Background()
	in := repo.GrantInput{PayerID: payer, Source: "membership",
		SourceRefID: "sub-2026-09", Amount: 500}
	if _, err := r.Grant(ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Grant(ctx, in); !errors.Is(err, repo.ErrDuplicate) {
		t.Fatalf("重复发放应被拒，实际 %v", err)
	}
	if bal, _ := r.Balance(ctx, payer); bal != 500 {
		t.Fatalf("只该发一次，实际余额 %d", bal)
	}
}

// 看板净消费按 transaction 汇总；按 detail 汇总会把跨批消费重复计算。
func TestConsume_看板口径(t *testing.T) {
	r, pool := setup(t)
	ctx := context.Background()
	grantN(t, r, 30, 7, "promotion")
	grantN(t, r, 100, 0, "purchase")
	if _, err := r.Consume(ctx, repo.ConsumeInput{
		PayerID: payer, Amount: 50, RefType: "generation", RefID: "dash"}); err != nil {
		t.Fatal(err)
	}

	var byTx, byDetail, detailRows int
	_ = pool.QueryRow(ctx, `
		select coalesce(-sum(change),0) from credit_transaction
		where payer_account_id=$1 and type='consume'`, payer).Scan(&byTx)
	_ = pool.QueryRow(ctx, `
		select coalesce(sum(d.amount),0), count(*) from credit_transaction_detail d
		join credit_transaction t on t.id = d.transaction_id
		where t.payer_account_id=$1 and t.type='consume'`, payer).Scan(&byDetail, &detailRows)

	if byTx != 50 {
		t.Fatalf("按流水汇总应为 50，实际 %d", byTx)
	}
	if detailRows != 2 {
		t.Fatalf("跨 2 批应有 2 条明细，实际 %d", detailRows)
	}
	// 这次两者恰好相等，但明细行数是 2 —— 一旦按明细做「笔数」统计就会翻倍。
	if byDetail != 50 {
		t.Fatalf("明细合计也应为 50，实际 %d", byDetail)
	}
}

// 到期扫描要记流水：只改状态不记流水的话，余额凭空变少而账本里找不到解释。
func TestExpireGrants_记流水(t *testing.T) {
	r, pool := setup(t)
	ctx := context.Background()
	gid := grantN(t, r, 60, 7, "promotion")
	if _, err := pool.Exec(ctx,
		`update credit_grant set expire_at = now() - interval '1 hour' where id=$1`, gid); err != nil {
		t.Fatal(err)
	}

	n, err := r.ExpireGrants(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应处理 1 个批次，实际 %d", n)
	}
	var status string
	_ = pool.QueryRow(ctx, `select status from credit_grant where id=$1`, gid).Scan(&status)
	if status != "expired" {
		t.Errorf("应标记为 expired，实际 %s", status)
	}
	var change int
	if err := pool.QueryRow(ctx, `
		select change from credit_transaction
		where type='expire' and ref_id=$1`, fmt.Sprint(gid)).Scan(&change); err != nil {
		t.Fatalf("应有一条 expire 流水: %v", err)
	}
	if change != -60 {
		t.Errorf("流水应记 -60，实际 %d", change)
	}

	// 重复扫描不应再产生流水。
	if n, _ := r.ExpireGrants(ctx, 100); n != 0 {
		t.Errorf("已过期的批次不该被重复处理，实际 %d", n)
	}
}

// 已花完的批次到期不产生流水：余额没有变化，记一条 0 变动的流水只会让账单更难读。
func TestExpireGrants_已用尽的批次不记流水(t *testing.T) {
	r, pool := setup(t)
	ctx := context.Background()
	gid := grantN(t, r, 20, 7, "promotion")
	_, _ = r.Consume(ctx, repo.ConsumeInput{
		PayerID: payer, Amount: 20, RefType: "generation", RefID: "used-up"})
	if _, err := pool.Exec(ctx, `
		update credit_grant set expire_at = now() - interval '1 hour', status='active'
		where id=$1`, gid); err != nil {
		t.Fatal(err)
	}

	if _, err := r.ExpireGrants(ctx, 100); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = pool.QueryRow(ctx,
		`select count(*) from credit_transaction where type='expire'`).Scan(&n)
	if n != 0 {
		t.Errorf("已用尽的批次到期不该记流水，实际 %d 条", n)
	}
}

// 余额由批次推导，且**不含已过期批次** —— 否则用户看到的余额花不出去。
func TestBalance_排除过期批次(t *testing.T) {
	r, pool := setup(t)
	ctx := context.Background()
	gid := grantN(t, r, 50, 7, "promotion")
	grantN(t, r, 100, 0, "purchase")
	if bal, _ := r.Balance(ctx, payer); bal != 150 {
		t.Fatalf("期望 150，实际 %d", bal)
	}
	if _, err := pool.Exec(ctx,
		`update credit_grant set expire_at = now() - interval '1 day' where id=$1`, gid); err != nil {
		t.Fatal(err)
	}
	if bal, _ := r.Balance(ctx, payer); bal != 100 {
		t.Fatalf("过期批次应被排除，期望 100，实际 %d", bal)
	}
}

// CD-82：退款原因超长也要退成。
//
// ⚠️ 起因：退款原因写的是上游报错原文，一段 323 字的 JSON 撞上 `credit_transaction.reason varchar(255)`，
// 整笔退款回滚 —— 而失败路径上的退款失败只留日志，6 个失败任务的积分就这样没退回去。
func TestRefund_原因超长也要退成(t *testing.T) {
	r, pool := setup(t)
	ctx := context.Background()
	grantN(t, r, 100, 0, "purchase")
	if _, err := r.Consume(ctx, repo.ConsumeInput{
		PayerID: payer, Amount: 48, RefType: "generation", RefID: "g-long", Reason: "img2img",
	}); err != nil {
		t.Fatal(err)
	}
	long := `上游返回 HTTP 400: {"error":{"code":"invalid_value","message":"` +
		strings.Repeat("Invalid 'images[0].image_url'. ", 12) + `"}}`
	if _, err := r.Refund(ctx, repo.RefundInput{
		PayerID: payer, RefType: "generation", RefID: "g-long", Reason: long,
		CompensationValidFor: 30 * 24 * time.Hour,
	}); err != nil {
		t.Fatalf("原因超长不该让退款失败：%v", err)
	}
	if bal, _ := r.Balance(ctx, payer); bal != 100 {
		t.Fatalf("应全额退回，余额 %d", bal)
	}
	var reason string
	if err := pool.QueryRow(ctx, `select reason from credit_transaction
		where ref_type='generation' and ref_id='g-long' and type='refund'`).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if n := utf8.RuneCountInString(reason); n != 255 || !strings.HasPrefix(reason, "上游返回 HTTP 400") {
		t.Fatalf("原因应截到 255 字并保留开头，得到 %d 字：%.40s", n, reason)
	}
}
