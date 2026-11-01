//go:build integration

// 后台任务运营台的真库测试（P13-03；GAP-07）。
package integration

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/content/domain"
	"github.com/li904338203/tabtv/backend/internal/content/repo"
)

const otherWS = int64(880099)

// ⚠️ 运营台与用户侧任务中心**共用同一个 union**（只换租户条件）。
// 这条测试钉住「换了条件之后两边算出来的状态仍然一致」——
// 各写一份 union 的话，新增一个状态词只会改一处，而漏掉的那一处的症状是
// 某一类任务在那个筛选下整体消失（CD-62）。
func TestListOpsTasks_跨租户且状态与用户侧一致(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	seedTasks(t, pool, next)
	// 另一个工作区也造一条：运营台要看得到它，用户侧不该看到。
	seedOtherWorkspaceTask(t, pool, next)

	mine, err := r.ListTasks(ctx, taskWS, repo.TaskFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	all, err := r.ListOpsTasks(ctx, repo.OpsTaskFilter{Limit: 200}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) <= len(mine) {
		t.Fatalf("运营台应看到更多（含别的工作区）：ops=%d user=%d", len(all), len(mine))
	}

	// 同一条任务在两边算出来的统一状态必须逐字一致。
	byID := map[int64]domain.TaskState{}
	for _, t0 := range all {
		byID[t0.ID] = t0.State
	}
	for _, m := range mine {
		if byID[m.ID] != m.State {
			t.Fatalf("同一条任务两边状态不一致：id=%d user=%s ops=%s",
				m.ID, m.State, byID[m.ID])
		}
	}

	// 用户侧看不到别的工作区那一条。
	var leaked bool
	for _, m := range mine {
		if m.Title == "别人的渲染" {
			leaked = true
		}
	}
	if leaked {
		t.Fatal("用户侧不该看到别的工作区的任务")
	}
}

// ⚠️ 关键词一个框查三样（任务 ID / 工作区 / 付费主体）：运维手上拿到的
// 可能是任意一个，分成三个框的话他得先判断"这串数字是什么" ——
// 而那正是他来查的原因。
func TestListOpsTasks_关键词三选一(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	seedTasks(t, pool, next)

	all, err := r.ListOpsTasks(ctx, repo.OpsTaskFilter{Limit: 200}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 {
		t.Fatal("应有任务")
	}
	one := all[0]

	// 按任务 ID。
	got, err := r.ListOpsTasks(ctx, repo.OpsTaskFilter{
		Keyword: itoa64(one.ID), Limit: 50}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != one.ID {
		t.Fatalf("按任务 ID 应恰好命中一条，得到 %d", len(got))
	}
	// 按工作区 ID：整批都该出来。
	got, err = r.ListOpsTasks(ctx, repo.OpsTaskFilter{
		Keyword: itoa64(taskWS), Limit: 200}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(all) {
		t.Fatalf("按工作区应捞到全部 %d 条，得到 %d", len(all), len(got))
	}
	// ⚠️ 非数字关键词**不报错也不当条件**：为一个格式问题把整个列表挡掉不值。
	got, err = r.ListOpsTasks(ctx, repo.OpsTaskFilter{
		Keyword: "'; drop table generation; --", Limit: 200}, time.Now())
	if err != nil {
		t.Fatalf("非法关键词不该让查询出错：%v", err)
	}
	if len(got) != len(all) {
		t.Fatalf("非数字关键词应被忽略，得到 %d 条", len(got))
	}
}

// ⚠️ 「卡住」= 还没结束 **且** 死线已过。只看"还没结束"的话筛出来的是整个
// 待办队列，而运维要找的是**本该结束却没结束**的那几条。
func TestListOpsTasks_卡住筛选与汇总(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	seedTasks(t, pool, next)
	now := time.Now()

	// 先确认：什么都没超时的时候，卡住是 0 条。
	stuck, err := r.ListOpsTasks(ctx, repo.OpsTaskFilter{StuckOnly: true, Limit: 50}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(stuck) != 0 {
		t.Fatalf("还没有超时任务时应为 0 条，得到 %d", len(stuck))
	}

	// 给一条 running 的生成任务补一个过去的死线。
	if _, err := pool.Exec(ctx, `
		update generation set deadline_at = now() - interval '1 hour'
		where workspace_id = $1 and status = 'running'`, taskWS); err != nil {
		t.Fatal(err)
	}
	stuck, err = r.ListOpsTasks(ctx, repo.OpsTaskFilter{StuckOnly: true, Limit: 50}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(stuck) != 1 {
		t.Fatalf("应恰好 1 条卡住，得到 %d", len(stuck))
	}
	if stuck[0].State != domain.StateRunning {
		t.Fatalf("卡住的应是 running，得到 %s", stuck[0].State)
	}

	// ⚠️ 汇总与列表**同一套条件**：分开算的话两个数字都"没错"而它们不一致。
	sum, err := r.SummarizeOpsTasks(ctx, repo.OpsTaskFilter{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Stuck != 1 {
		t.Fatalf("汇总里的卡住数应为 1，得到 %d", sum.Stuck)
	}
	total := 0
	for _, n := range sum.ByState {
		total += n
	}
	all, err := r.ListOpsTasks(ctx, repo.OpsTaskFilter{Limit: 500}, now)
	if err != nil {
		t.Fatal(err)
	}
	if total != len(all) {
		t.Fatalf("各状态之和应等于列表总数：%d vs %d", total, len(all))
	}
}

// ⚠️ 排障要看的是**库里到底是什么**（原始状态），不是"给用户怎么显示"。
// 两者都要给：只给统一状态的话，整图运行的 pending 与生成的 queued 在
// 运维台上长得一模一样，而它们的处置完全不同。
func TestListOpsTasks_同时给统一状态与原始状态(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	seedTasks(t, pool, next)

	all, err := r.ListOpsTasks(ctx, repo.OpsTaskFilter{
		Kind: domain.KindWorkflow, Limit: 50}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var sawPending bool
	for _, t0 := range all {
		if t0.RawStatus == "pending" {
			sawPending = true
			if t0.State != domain.StateQueued {
				t.Fatalf("整图的 pending 应映射成 queued，得到 %s", t0.State)
			}
		}
	}
	if !sawPending {
		t.Fatal("应能看到整图运行的原始状态 pending")
	}
}

// seedOtherWorkspaceTask 在**另一个工作区**造一条渲染任务。
//
// 它的作用是让"运营台看得到全站 / 用户侧只看得到自己的"这条断言真的有东西
// 可验 —— 只造一个工作区的数据的话，租户条件写错也测不出来。
func seedOtherWorkspaceTask(t *testing.T, pool *pgxpool.Pool, next func() int64) {
	t.Helper()
	ctx := context.Background()
	payer := next()
	if _, err := pool.Exec(ctx, `
		insert into payer_account (id, kind, owner_id) values ($1,'workspace',$2)`,
		payer, otherWS); err != nil {
		t.Fatal(err)
	}
	proj := next()
	if _, err := pool.Exec(ctx, `
		insert into project (id, workspace_id, created_by_user_id, name, canvas_data, status)
		values ($1,$2,2,'别人的项目','{}'::jsonb,'active')`, proj, otherWS); err != nil {
		t.Fatal(err)
	}
	tl := next()
	if _, err := pool.Exec(ctx, `
		insert into timeline (id, workspace_id, project_id, name, aspect_ratio, width, height)
		values ($1,$2,$3,'别人的渲染','16:9',1920,1080)`, tl, otherWS, proj); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into render_job (id, workspace_id, payer_account_id, created_by_user_id,
			timeline_id, timeline_version, snapshot, status, credits_cost)
		values ($1,$2,$3,2,$4,1,'{}'::jsonb,'success',9)`,
		next(), otherWS, payer, tl); err != nil {
		t.Fatal(err)
	}
}
