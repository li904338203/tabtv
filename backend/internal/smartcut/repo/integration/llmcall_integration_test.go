//go:build integration

// AI 剪辑调用记录与限流（docs/agent/smartcut SCT-27，SCD-24）的真库测试。
//
// ⚠️ 必须真库、必须真并发：限流的正确性全在「advisory lock + 计数 + 插入」同一个事务里，
// 顺序调用永远先看到上一条已经插进去，把锁整条删掉照样绿（CLAUDE.md §5.7）。
package integration

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
	"github.com/li904338203/tabtv/backend/internal/smartcut/repo"
)

const ws = int64(9201)

func setup(t *testing.T) (*repo.Repo, *pgxpool.Pool, func() int64) {
	t.Helper()
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	return repo.New(pool), pool, gen.Next
}

func call(next func() int64, user, timeline int64) repo.Call {
	return repo.Call{ID: next(), WorkspaceID: ws, ProjectID: 1, TimelineID: timeline, CanvasNodeID: "sc", UserID: user,
		Source: "web", Kind: repo.KindPlan, Mode: "mix", VariantCount: 3, ModelCode: "m", BaseVersion: 2,
		Spec: json.RawMessage(`{"targetMs":20000}`)}
}

// warm 先把连接池里的 n 个连接建好：池是懒建连接的，第一个请求拿现成的连接跑完了，
// 后面的还在拨号 —— 那样「并发」其实是串行，测不出没加锁的竞态。
func warm(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	conns := make([]*pgxpool.Conn, 0, n)
	for i := 0; i < n; i++ {
		c, err := pool.Acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		c.Release()
	}
}

func TestInsertWithLimits_真并发只放行一个(t *testing.T) {
	r, pool, next := setup(t)
	const n = 8
	warm(t, pool, n)
	// 竞态不是每次都撞得上：跑几轮（每轮一个新用户），没加锁的实现几乎必然在某一轮放行多个。
	for round := int64(1); round <= 5; round++ {
		user := 300 + round
		var wg sync.WaitGroup
		decisions := make([]repo.LimitDecision, n)
		errs := make([]error, n)
		calls := make([]repo.Call, n)
		for i := range calls {
			calls[i] = call(next, user, 10)
		}
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				decisions[i], errs[i] = r.InsertWithLimits(context.Background(), calls[i], repo.Limits{Concurrency: 1, PerHour: 20})
			}(i)
		}
		close(start)
		wg.Wait()
		allowed := 0
		for i := range decisions {
			if errs[i] != nil {
				t.Fatalf("第 %d 轮第 %d 个出错：%v", round, i, errs[i])
			}
			switch decisions[i].Reject {
			case repo.RejectNone:
				allowed++
			case repo.RejectConcurrency:
			default:
				t.Fatalf("并发超限应判 concurrency，得到 %q", decisions[i].Reject)
			}
		}
		if allowed != 1 {
			t.Fatalf("第 %d 轮：同一用户并发 %d 次、上限 1，应恰好放行 1 个，得到 %d", round, n, allowed)
		}
		var rows int
		_ = pool.QueryRow(context.Background(), `select count(*) from smartcut_llm_call where user_id = $1`, user).Scan(&rows)
		if rows != 1 {
			t.Fatalf("第 %d 轮只该插入 1 条记录，得到 %d", round, rows)
		}
	}
}

func TestInsertWithLimits_小时窗口与多方案只计一次(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	lim := repo.Limits{Concurrency: 5, PerHour: 3}
	for i := 0; i < 3; i++ {
		// 每一次都是 3 套方案：一次请求只插一行、计 1 次（SC-AC-35）。
		d, err := r.InsertWithLimits(ctx, call(next, 7, 10), lim)
		if err != nil || d.Reject != repo.RejectNone {
			t.Fatalf("第 %d 次应放行：%+v %v", i+1, d, err)
		}
	}
	var rows int
	_ = pool.QueryRow(ctx, `select count(*) from smartcut_llm_call where user_id = 7`).Scan(&rows)
	if rows != 3 {
		t.Fatalf("3 次 3 套方案应只有 3 行：%d", rows)
	}
	// 让最早的一条看起来是 59 分钟前的：第 4 次被拒，约 60 秒后可再试。
	_, _ = pool.Exec(ctx, `update smartcut_llm_call set created_at = now() - interval '59 minutes', status = 'success'
		where id = (select id from smartcut_llm_call where user_id = 7 order by created_at limit 1)`)
	d, err := r.InsertWithLimits(ctx, call(next, 7, 10), lim)
	if err != nil || d.Reject != repo.RejectHourly {
		t.Fatalf("超过每小时上限应判 hourly：%+v %v", d, err)
	}
	if d.RetryAfter < 30*time.Second || d.RetryAfter > 70*time.Second {
		t.Fatalf("最早那条再过约 1 分钟滑出窗口：%s", d.RetryAfter)
	}
	// 失败的也算次数（上游的额度照样用掉了）。
	_, _ = pool.Exec(ctx, `update smartcut_llm_call set status = 'failed' where user_id = 7`)
	if d, _ := r.InsertWithLimits(ctx, call(next, 7, 10), lim); d.Reject != repo.RejectHourly {
		t.Fatalf("失败的调用也计入每小时次数：%+v", d)
	}
}

func TestFinish_CAS与阶段(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	c := call(next, 8, 20)
	if d, err := r.InsertWithLimits(ctx, c, repo.Limits{Concurrency: 1, PerHour: 20}); err != nil || d.Reject != "" {
		t.Fatal(d, err)
	}
	if ok, err := r.SetStage(ctx, c.ID, repo.StagePlan); err != nil || !ok {
		t.Fatalf("进行中的能推进阶段：%v %v", ok, err)
	}
	active, err := r.ActiveCalls(ctx, ws, []int64{20, 21})
	if err != nil || active[20].ID != c.ID || active[20].Stage != repo.StagePlan || len(active) != 1 {
		t.Fatalf("摘要里的进行中调用：%+v %v", active, err)
	}
	v := 1
	long := make([]rune, 2000)
	for i := range long {
		long[i] = '错'
	}
	if ok, err := r.Finish(ctx, c.ID, repo.FinishInput{Status: repo.StatusSuccess, Result: json.RawMessage(`{"variants":[]}`),
		AppliedVariant: &v, UsedFallback: true, ErrorMessage: string(long), DurationMs: 12}); err != nil || !ok {
		t.Fatalf("结束：%v %v", ok, err)
	}
	if ok, _ := r.Finish(ctx, c.ID, repo.FinishInput{Status: repo.StatusFailed}); ok {
		t.Fatal("已结束的不能再改（CAS）")
	}
	rec, err := r.Get(ctx, ws, 20, c.ID)
	if err != nil || rec.Status != repo.StatusSuccess || rec.Stage != repo.StageDone || rec.AppliedVariant == nil ||
		*rec.AppliedVariant != 1 || !rec.UsedFallback || len([]rune(rec.ErrorMessage)) != 1024 || rec.VariantCount != 3 {
		t.Fatalf("记录：%+v %v", rec, err)
	}
	if _, err := r.Get(ctx, ws, 21, c.ID); err != repo.ErrNotFound {
		t.Fatalf("别的时间线取不到这条：%v", err)
	}
	if err := r.SetApplied(ctx, c.ID, 2); err != nil {
		t.Fatal(err)
	}
	if rec, _ := r.Get(ctx, ws, 20, c.ID); *rec.AppliedVariant != 2 {
		t.Fatal("切换方案后记下当前方案")
	}
	if active, _ := r.ActiveCalls(ctx, ws, []int64{20}); len(active) != 0 {
		t.Fatal("结束之后摘要里不再有进行中调用")
	}
	var detail int
	_ = pool.QueryRow(ctx, `select duration_ms from smartcut_llm_call where id = $1`, c.ID).Scan(&detail)
	if detail != 12 {
		t.Fatalf("耗时：%d", detail)
	}
}

func TestRecoverStale_与保留期清理(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	old, fresh := call(next, 9, 30), call(next, 10, 30)
	for _, c := range []repo.Call{old, fresh} {
		if _, err := r.InsertWithLimits(ctx, c, repo.Limits{}); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = pool.Exec(ctx, `update smartcut_llm_call set created_at = now() - interval '21 minutes' where id = $1`, old.ID)
	n, err := r.RecoverStale(ctx, 20*time.Minute, 100)
	if err != nil || n != 1 {
		t.Fatalf("只判超过 20 分钟的那条：%d %v", n, err)
	}
	rec, _ := r.Get(ctx, ws, 30, old.ID)
	if rec.Status != repo.StatusFailed || rec.ErrorMessage != repo.StaleMessage {
		t.Fatalf("%+v", rec)
	}
	// 判了失败就不再占并发：同一用户可以再发起。
	if d, _ := r.InsertWithLimits(ctx, call(next, 9, 30), repo.Limits{Concurrency: 1}); d.Reject != "" {
		t.Fatalf("恢复扫描之后释放并发额度：%+v", d)
	}
	_, _ = pool.Exec(ctx, `update smartcut_llm_call set created_at = now() - interval '31 days' where id in ($1, $2)`, old.ID, fresh.ID)
	deleted, err := r.DeleteOlderThan(ctx, time.Now().Add(-30*24*time.Hour), 1000)
	if err != nil || deleted != 1 {
		t.Fatalf("进行中的不删、结束的删：%d %v", deleted, err)
	}
}
