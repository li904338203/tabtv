//go:build integration

// 请求日志与统计口径的真库测试（P13-04；AD-07、GAP-24）。
//
// ⚠️ 必须用真库：这一组要守的全部由 **SQL** 决定 —— 活跃用户的并集口径、
// 积分只认账本、取消不进成功率分母、折线补零、以及「折线各天之和 == 概览总数」。
package integration

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/observability/domain"
	"github.com/li904338203/tabtv/backend/internal/observability/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
)

const (
	obsWS    = int64(770001)
	obsUserA = int64(770101)
	obsUserB = int64(770102)
)

func setup(t *testing.T) (*repo.Repo, *pgxpool.Pool, func() int64) {
	t.Helper()
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	return repo.New(pool, gen.Next), pool, gen.Next
}

// seedStats 造一批可预测的数据：昨天与今天各有产出。
func seedStats(t *testing.T, pool *pgxpool.Pool, next func() int64) (payer, proj int64) {
	t.Helper()
	ctx := context.Background()
	payer = next()
	if _, err := pool.Exec(ctx, `
		insert into payer_account (id, kind, owner_id) values ($1,'workspace',$2)`,
		payer, obsWS); err != nil {
		t.Fatal(err)
	}
	proj = next()
	if _, err := pool.Exec(ctx, `
		insert into project (id, workspace_id, created_by_user_id, name, canvas_data, status)
		values ($1,$2,$3,'统计项目','{}'::jsonb,'active')`, proj, obsWS, obsUserA); err != nil {
		t.Fatal(err)
	}
	yesterday := time.Now().Add(-24 * time.Hour)
	today := time.Now().Add(-time.Hour)

	// A 昨天两条：一成一败。B 今天一条：取消。
	mk := func(uid int64, status string, at time.Time, cost int) {
		if _, err := pool.Exec(ctx, `
			insert into generation (id, workspace_id, payer_account_id, created_by_user_id,
				project_id, operation_id, model_id, result_type, params_snapshot,
				inputs_snapshot, request_count, credits_cost, idempotency_key, status, created_at)
			values ($1,$2,$3,$4,$5,1,1,'image','{}'::jsonb,'[]'::jsonb,1,$6,$7,$8,$9)`,
			next(), obsWS, payer, uid, proj, cost,
			"obs-"+status+"-"+at.Format("150405.000000"), status, at); err != nil {
			t.Fatal(err)
		}
	}
	mk(obsUserA, "success", yesterday, 5)
	mk(obsUserA, "failed", yesterday, 5)
	mk(obsUserB, "cancelled", today, 5)

	// 账本：两笔消费（各 5）+ 一笔退款（5）。change 有符号。
	tx := func(kind string, change int, at time.Time, ref string) {
		if _, err := pool.Exec(ctx, `
			insert into credit_transaction (id, payer_account_id, type, change,
				balance_after, ref_type, ref_id, created_at)
			values ($1,$2,$3,$4,0,'generation',$5,$6)`,
			next(), payer, kind, change, ref, at); err != nil {
			t.Fatal(err)
		}
	}
	tx("consume", -5, yesterday, "g1")
	tx("consume", -5, yesterday, "g2")
	tx("refund", 5, yesterday, "g2")

	// Provider 日志：3 条，其中 1 条失败。
	for i, et := range []string{"none", "none", "timeout"} {
		if _, err := pool.Exec(ctx, `
			insert into provider_request_log (id, provider_code, url, error_type,
				duration_ms, created_at)
			values ($1,'fake','http://x','`+et+`',$2,$3)`,
			next(), 100*(i+1), yesterday); err != nil {
			t.Fatal(err)
		}
	}
	return payer, proj
}

// ⚠️ 真实活跃 = 当天**产生过消费或产出**的人，**不是"登录过的人"**。
// 按 last_login_at 算的话（旧系统的做法，GAP-24），一个把页面挂在后台的
// 浏览器每天都算一个活跃用户，而这个数只会涨。
func TestOverview_活跃按产出算且取消不进成功率(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	seedStats(t, pool, next)
	rg := domain.Range{From: time.Now().Add(-48 * time.Hour), To: time.Now()}

	tot, err := r.Overview(ctx, rg)
	if err != nil {
		t.Fatal(err)
	}
	if tot.ActiveUsers != 2 {
		t.Fatalf("活跃应是 2 人（A 与 B），得到 %d", tot.ActiveUsers)
	}
	if tot.Generations != 3 || tot.GenerationSuccess != 1 ||
		tot.GenerationFailed != 1 || tot.GenerationCancelled != 1 {
		t.Fatalf("生成分布不对：%+v", tot)
	}
	// ⚠️ 取消**不进分母**：并进去的话用户多取消几次会让成功率看起来在下降，
	// 而系统一切正常。1 成 1 败 → 50%，而不是 1/3。
	if got := domain.SuccessRate(tot.GenerationSuccess, tot.GenerationFailed); got != 0.5 {
		t.Fatalf("成功率应是 0.5（取消不进分母），得到 %v", got)
	}
}

// ⚠️ 积分口径**只认账本**，不按各业务表的 credits_cost 相加 —— 那一列是
// **报价**，任务失败退款之后它不会变，于是一个失败又退款的任务会被算成花了钱。
// ⚠️ 消耗与退还**分开给，不合并成净值**（第五次）。
func TestOverview_积分只认账本且消耗退还分开(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	seedStats(t, pool, next)
	rg := domain.Range{From: time.Now().Add(-48 * time.Hour), To: time.Now()}

	tot, err := r.Overview(ctx, rg)
	if err != nil {
		t.Fatal(err)
	}
	if tot.CreditsConsumed != 10 {
		t.Fatalf("消耗应是 10（两笔账本流水），得到 %d", tot.CreditsConsumed)
	}
	if tot.CreditsRefunded != 5 {
		t.Fatalf("退还应是 5 且单列，得到 %d", tot.CreditsRefunded)
	}
	// 业务表的 credits_cost 之和是 15（三条任务各 5）—— 与账本口径**不同**，
	// 这条断言就是在钉住"不能拿那个数当消耗"。
	var costSum int64
	if err := pool.QueryRow(ctx,
		`select coalesce(sum(credits_cost),0) from generation where workspace_id=$1`,
		obsWS).Scan(&costSum); err != nil {
		t.Fatal(err)
	}
	if costSum == tot.CreditsConsumed {
		t.Fatal("消耗口径不该等于业务表 credits_cost 之和（那一列是报价，退款后不会变）")
	}
}

// ⚠️ 这是**调用次数不是成本**：真实成本要各 Provider 的单价，
// 而那是 PARAM-04 未定稿的内容。
func TestOverview_供应商调用量与失败数(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	seedStats(t, pool, next)
	rg := domain.Range{From: time.Now().Add(-48 * time.Hour), To: time.Now()}

	tot, err := r.Overview(ctx, rg)
	if err != nil {
		t.Fatal(err)
	}
	if tot.ProviderCalls != 3 || tot.ProviderErrors != 1 {
		t.Fatalf("上游调用统计不对：calls=%d errors=%d", tot.ProviderCalls, tot.ProviderErrors)
	}
	by, err := r.ByProvider(ctx, rg)
	if err != nil {
		t.Fatal(err)
	}
	if len(by) != 1 || by[0].ProviderCode != "fake" || by[0].Calls != 3 || by[0].Errors != 1 {
		t.Fatalf("按供应商分组不对：%+v", by)
	}
	if by[0].AvgDurationMs != 200 {
		t.Fatalf("平均耗时应是 200（100/200/300），得到 %d", by[0].AvgDurationMs)
	}
}

// ⚠️ 折线**补零**：没有数据的那天要出现且值为 0。不补的话折线会把两个相隔
// 一周的点直接连起来，看上去像"平稳增长" —— 而中间那一周其实一条记录都没有
// （服务挂了也长这样）。
func TestSeries_补零且与概览总数一致(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	seedStats(t, pool, next)
	rg := domain.Range{From: time.Now().Add(-5 * 24 * time.Hour), To: time.Now()}

	pts, err := r.Series(ctx, domain.MetricGenerations, rg)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) < 5 {
		t.Fatalf("应补齐每一天，得到 %d 个点", len(pts))
	}
	var zero, sum int64
	for _, p := range pts {
		if p.Value == 0 {
			zero++
		}
		sum += p.Value
	}
	if zero == 0 {
		t.Fatal("没有数据的那几天应补零")
	}
	// ⚠️ **折线各天之和必须等于概览上的总数**：两处口径不一致的表现是
	// 看板上两个数字对不上，而看的人只会觉得"这个看板不准"、说不出哪里不准。
	tot, err := r.Overview(ctx, rg)
	if err != nil {
		t.Fatal(err)
	}
	if sum != tot.Generations {
		t.Fatalf("折线之和(%d)应等于概览总数(%d)", sum, tot.Generations)
	}
}

// ⚠️ 指标名来自 HTTP 查询参数：不认识的一律回**空序列而不是报错**
// （前端可能比后端新），更不能拼进 SQL。
func TestSeries_未知指标回空不报错(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	seedStats(t, pool, next)
	rg := domain.Range{From: time.Now().Add(-2 * 24 * time.Hour), To: time.Now()}

	for _, m := range []domain.Metric{"not_a_metric", "1) union select 1--", ""} {
		pts, err := r.Series(ctx, m, rg)
		if err != nil {
			t.Fatalf("未知指标 %q 不该报错：%v", m, err)
		}
		if len(pts) != 0 {
			t.Fatalf("未知指标应回空序列，得到 %d 个点", len(pts))
		}
	}
}

// ---------------------------------------------------------------- API 日志

func TestAPILog_写入检索与清理(t *testing.T) {
	r, _, _ := setup(t)
	ctx := context.Background()
	now := time.Now()

	write := func(path string, status, biz int, user int64) {
		if err := r.WriteAPILog(ctx, repo.APILog{
			TraceID: "trace-" + path, Method: "POST", Path: path,
			HTTPStatus: status, BizCode: biz, DurationMs: 12,
			UserID: user, IP: "127.0.0.1", RequestBody: `{"a":1}`,
		}); err != nil {
			t.Fatal(err)
		}
	}
	write("/api/v1/generations", 200, 0, obsUserA)
	write("/api/v1/generations", 409, 40903, obsUserA)
	write("/api/v1/projects", 200, 0, obsUserB)

	all, err := r.ListAPILogs(ctx, repo.APILogFilter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("应有 3 条，得到 %d", len(all))
	}

	// ⚠️ `bizCode` 存在的意义是**分类**：40001/40002/40903 在 HTTP 上都只是
	// 一个 400/409，而日志要回答的恰恰是"今天这批错里哪一类最多"。
	failed, err := r.ListAPILogs(ctx, repo.APILogFilter{FailedOnly: true, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 1 || failed[0].BizCode != 40903 {
		t.Fatalf("失败筛选不对：%+v", failed)
	}

	// 按 traceId —— 用户报障时给的就是它。
	byTrace, err := r.ListAPILogs(ctx, repo.APILogFilter{
		TraceID: "trace-/api/v1/projects", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(byTrace) != 1 {
		t.Fatalf("按 traceId 应命中 1 条，得到 %d", len(byTrace))
	}

	// 按路径前缀。
	byPath, err := r.ListAPILogs(ctx, repo.APILogFilter{Path: "/api/v1/gen", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(byPath) != 2 {
		t.Fatalf("按路径前缀应命中 2 条，得到 %d", len(byPath))
	}
	// ⚠️ `%` 按字面量处理：不转义的话一个 `%` 就能拖出全表。
	byPct, err := r.ListAPILogs(ctx, repo.APILogFilter{Path: "%", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(byPct) != 0 {
		t.Fatalf("%% 应当字面量处理（没有路径以它开头），得到 %d 条", len(byPct))
	}

	// 按主体：一个框查 user/admin/workspace 三样。
	byUser, err := r.ListAPILogs(ctx, repo.APILogFilter{
		Keyword: "770102", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(byUser) != 1 {
		t.Fatalf("按用户应命中 1 条，得到 %d", len(byUser))
	}

	// 清理：保留期之内的一条都不删。
	n, err := r.CleanupAPILogsBefore(ctx, now.Add(-time.Hour), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("保留期内不该删，删了 %d 条", n)
	}
	// 把截止时间推到未来 → 全删。
	n, err = r.CleanupAPILogsBefore(ctx, now.Add(time.Hour), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("应删 3 条，删了 %d", n)
	}
}
