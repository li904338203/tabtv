//go:build integration

// AI 搭景调用记录与限流（docs/agent/director，DRT-19，DRD-23）的真库测试。
//
// ⚠️ 必须真库、必须真并发：限流的正确性全在「advisory lock + 计数 + 插入」同一个事务里，
// 顺序调用永远先看到上一条已经插进去，把锁整条删掉照样绿（CLAUDE.md §5.7）。
package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/director/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
)

func setup(t *testing.T) (*repo.Repo, *pgxpool.Pool, func() int64) {
	t.Helper()
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	return repo.New(pool), pool, gen.Next
}

func call(next func() int64, user int64) repo.LLMCall {
	return repo.LLMCall{ID: next(), WorkspaceID: 9001, ProjectID: 1, SceneID: 2, UserID: user,
		Source: "web", ModelCode: "m", PromptChars: 10}
}

func status(t *testing.T, pool *pgxpool.Pool, id int64) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), `select status from director_llm_call where id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// warm 先把连接池里的 n 个连接建好：池是懒建连接的，第一个请求拿现成的连接跑完了，
// 后面的还在拨号 —— 那样「并发」其实是串行，测不出没加锁的竞态（实测：删掉锁这条测试照样绿）。
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
		user := 100 + round
		var wg sync.WaitGroup
		decisions := make([]repo.LimitDecision, n)
		errs := make([]error, n)
		calls := make([]repo.LLMCall, n)
		for i := range calls {
			calls[i] = call(next, user)
		}
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				decisions[i], errs[i] = r.InsertWithLimits(context.Background(), calls[i],
					repo.Limits{Concurrency: 1, PerHour: 20})
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
		_ = pool.QueryRow(context.Background(), `select count(*) from director_llm_call where user_id = $1`, user).Scan(&rows)
		if rows != 1 {
			t.Fatalf("第 %d 轮只该插入 1 条记录，得到 %d", round, rows)
		}
	}
}

func TestInsertWithLimits_小时窗口与retryAfter(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	lim := repo.Limits{Concurrency: 5, PerHour: 3}
	var ids []int64
	for i := 0; i < 3; i++ {
		c := call(next, 7)
		d, err := r.InsertWithLimits(ctx, c, lim)
		if err != nil || d.Reject != repo.RejectNone {
			t.Fatalf("第 %d 次应放行：%+v %v", i+1, d, err)
		}
		ids = append(ids, c.ID)
		if _, err := r.Finish(ctx, c.ID, repo.FinishInput{Status: repo.StatusSucceeded, ObjectCount: 3}); err != nil {
			t.Fatal(err)
		}
	}
	// 让最早那条落在 50 分钟前：它 10 分钟后滑出窗口。
	if _, err := pool.Exec(ctx, `update director_llm_call set created_at = now() - interval '50 minutes' where id = $1`,
		ids[0]); err != nil {
		t.Fatal(err)
	}
	d, err := r.InsertWithLimits(ctx, call(next, 7), lim)
	if err != nil || d.Reject != repo.RejectHourly {
		t.Fatalf("第 4 次应判小时额度用完，得到 %+v %v", d, err)
	}
	if d.RetryAfter < 9*time.Minute || d.RetryAfter > 10*time.Minute+time.Second {
		t.Fatalf("应在约 10 分钟后可再发起，得到 %s", d.RetryAfter)
	}
	if d.UsedThisHour != 3 {
		t.Errorf("近 1 小时已用 3 次，得到 %d", d.UsedThisHour)
	}
	// 一小时前的不算。
	if _, err := pool.Exec(ctx, `update director_llm_call set created_at = now() - interval '61 minutes' where id = $1`,
		ids[0]); err != nil {
		t.Fatal(err)
	}
	if d, err := r.InsertWithLimits(ctx, call(next, 7), lim); err != nil || d.Reject != repo.RejectNone {
		t.Fatalf("最早那条滑出窗口后应放行，得到 %+v %v", d, err)
	}
}

func TestInsertWithLimits_跨用户互不影响(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	lim := repo.Limits{Concurrency: 1, PerHour: 1}
	if d, err := r.InsertWithLimits(ctx, call(next, 7), lim); err != nil || d.Reject != repo.RejectNone {
		t.Fatalf("用户 7 第一次应放行：%+v %v", d, err)
	}
	if d, err := r.InsertWithLimits(ctx, call(next, 8), lim); err != nil || d.Reject != repo.RejectNone {
		t.Fatalf("用户 8 不受用户 7 影响：%+v %v", d, err)
	}
	running, used, err := r.Usage(ctx, 7)
	if err != nil || running != 1 || used != 1 {
		t.Fatalf("用户 7 的计数应为进行中 1、近 1 小时 1，得到 %d %d %v", running, used, err)
	}
}

func TestFinish_只结束进行中的(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	c := call(next, 7)
	if _, err := r.InsertWithLimits(ctx, c, repo.Limits{}); err != nil {
		t.Fatal(err)
	}
	long := ""
	for i := 0; i < 800; i++ {
		long += "错"
	}
	changed, err := r.Finish(ctx, c.ID, repo.FinishInput{Status: repo.StatusFailed, ErrorCode: "draft_invalid",
		ErrorMessage: long, DurationMs: 1200})
	if err != nil || !changed {
		t.Fatalf("超长原文要截断而不是让结束失败：changed=%v err=%v", changed, err)
	}
	if s := status(t, pool, c.ID); s != repo.StatusFailed {
		t.Fatalf("应为 failed，得到 %s", s)
	}
	// 已经结束的不能再被改（晚到的结局不能把恢复扫描判的失败改回成功）。
	changed, err = r.Finish(ctx, c.ID, repo.FinishInput{Status: repo.StatusSucceeded})
	if err != nil || changed {
		t.Fatalf("已结束的记录不能再改，得到 changed=%v err=%v", changed, err)
	}
	if s := status(t, pool, c.ID); s != repo.StatusFailed {
		t.Fatalf("应仍为 failed，得到 %s", s)
	}
}

func TestRecoverStale_进行中超时判失败(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	old, fresh := call(next, 7), call(next, 8)
	for _, c := range []repo.LLMCall{old, fresh} {
		if _, err := r.InsertWithLimits(ctx, c, repo.Limits{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `update director_llm_call set created_at = now() - interval '25 minutes' where id = $1`,
		old.ID); err != nil {
		t.Fatal(err)
	}
	n, err := r.RecoverStale(ctx, 20*time.Minute, 100)
	if err != nil || n != 1 {
		t.Fatalf("应判失败 1 条，得到 %d %v", n, err)
	}
	if s := status(t, pool, old.ID); s != repo.StatusFailed {
		t.Fatalf("超时的应为 failed，得到 %s", s)
	}
	if s := status(t, pool, fresh.ID); s != repo.StatusRunning {
		t.Fatalf("没超时的不动，得到 %s", s)
	}
	// 判失败之后，并发额度随即释放。
	if d, err := r.InsertWithLimits(ctx, call(next, 7), repo.Limits{Concurrency: 1}); err != nil || d.Reject != repo.RejectNone {
		t.Fatalf("恢复之后用户 7 应能再发起：%+v %v", d, err)
	}
}

func TestDeleteOlderThan_分批(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		c := call(next, 7)
		if _, err := r.InsertWithLimits(ctx, c, repo.Limits{}); err != nil {
			t.Fatal(err)
		}
		if i < 4 {
			if _, err := r.Finish(ctx, c.ID, repo.FinishInput{Status: repo.StatusSucceeded}); err != nil {
				t.Fatal(err)
			}
		}
	}
	// 全部挪到 31 天前；最后一条仍是 running（不能删：它还占着并发额度）。
	if _, err := pool.Exec(ctx, `update director_llm_call set created_at = now() - interval '31 days'`); err != nil {
		t.Fatal(err)
	}
	before := time.Now().Add(-30 * 24 * time.Hour)
	if n, err := r.DeleteOlderThan(ctx, before, 3); err != nil || n != 3 {
		t.Fatalf("第一批应删 3 条，得到 %d %v", n, err)
	}
	if n, err := r.DeleteOlderThan(ctx, before, 3); err != nil || n != 1 {
		t.Fatalf("第二批应删剩下的 1 条（进行中的不删），得到 %d %v", n, err)
	}
	var left int
	_ = pool.QueryRow(ctx, `select count(*) from director_llm_call`).Scan(&left)
	if left != 1 {
		t.Fatalf("应只剩那条进行中的，得到 %d", left)
	}
}
