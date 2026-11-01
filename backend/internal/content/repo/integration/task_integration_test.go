//go:build integration

// 任务中心与作品审核的真库测试（P13-02；GAP-05、AD-09）。
package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/content/domain"
	"github.com/li904338203/tabtv/backend/internal/content/repo"
)

const taskWS = int64(880001)

// seedTasks 在四张表里各造几条任务。
func seedTasks(t *testing.T, pool *pgxpool.Pool, next func() int64) {
	t.Helper()
	ctx := context.Background()
	payer := next()
	if _, err := pool.Exec(ctx, `
		insert into payer_account (id, kind, owner_id) values ($1,'workspace',$2)`,
		payer, taskWS); err != nil {
		t.Fatal(err)
	}
	proj := next()
	if _, err := pool.Exec(ctx, `
		insert into project (id, workspace_id, created_by_user_id, name, canvas_data, status)
		values ($1,$2,1,'任务项目','{}'::jsonb,'active')`, proj, taskWS); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Hour)
	at := func(i int) time.Time { return base.Add(time.Duration(i) * time.Minute) }

	// generation：每个状态一条。
	for i, st := range domain.AllRawStates()[domain.KindGeneration] {
		if _, err := pool.Exec(ctx, `
			insert into generation (id, workspace_id, payer_account_id, created_by_user_id,
				project_id, operation_id, model_id, result_type, params_snapshot,
				inputs_snapshot, request_count, credits_cost, idempotency_key, status, created_at)
			values ($1,$2,$3,1,$4,1,1,'image','{}'::jsonb,'[]'::jsonb,1,3,$5,$6,$7)`,
			next(), taskWS, payer, proj, "gen-"+st, st, at(i)); err != nil {
			t.Fatal(err)
		}
	}
	// workflow_run：含 pending 与 partial_success —— 这两个正是最容易漏的。
	for i, st := range domain.AllRawStates()[domain.KindWorkflow] {
		if _, err := pool.Exec(ctx, `
			insert into workflow_run (id, workspace_id, payer_account_id, created_by_user_id,
				project_id, graph_snapshot, status, created_at)
			values ($1,$2,$3,1,$4,'{}'::jsonb,$5,$6)`,
			next(), taskWS, payer, proj, st, at(10+i)); err != nil {
			t.Fatal(err)
		}
	}
	// render_job。
	tl := next()
	if _, err := pool.Exec(ctx, `
		insert into timeline (id, workspace_id, project_id, name, aspect_ratio, width, height)
		values ($1,$2,$3,'线','16:9',1920,1080)`, tl, taskWS, proj); err != nil {
		t.Fatal(err)
	}
	for i, st := range domain.AllRawStates()[domain.KindRender] {
		if _, err := pool.Exec(ctx, `
			insert into render_job (id, workspace_id, payer_account_id, created_by_user_id,
				timeline_id, timeline_version, snapshot, status, credits_cost, created_at)
			values ($1,$2,$3,1,$4,1,'{}'::jsonb,$5,4,$6)`,
			next(), taskWS, payer, tl, st, at(20+i)); err != nil {
			t.Fatal(err)
		}
	}
	// export_task。
	for i, st := range domain.AllRawStates()[domain.KindExport] {
		if _, err := pool.Exec(ctx, `
			insert into export_task (id, workspace_id, user_id, items, status,
				expires_at, created_at)
			values ($1,$2,1,'[]'::jsonb,$3,now() + interval '1 day',$4)`,
			next(), taskWS, st, at(30+i)); err != nil {
			t.Fatal(err)
		}
	}
}

// ⚠️ 四类任务的状态词表并不一致（`pending` vs `queued`，多一个 `partial_success`）。
// 不统一就混在一个列表里的后果**很安静**：用户点"失败"筛选，某一类整体消失 ——
// 而列表看起来很正常，他只会以为那天没跑过那类任务。
//
// 这条测试用**同一批数据**同时验 SQL 的映射与 `domain.NormalizeState`：
// 两处是同一份映射，改一处漏改另一处就会红。
func TestListTasks_状态映射与domain一致(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	seedTasks(t, pool, next)

	all, err := r.ListTasks(ctx, taskWS, repo.TaskFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	expect := 0
	for _, raws := range domain.AllRawStates() {
		expect += len(raws)
	}
	if len(all) != expect {
		t.Fatalf("应有 %d 条任务，实际 %d", expect, len(all))
	}
	// 时间倒序。
	for i := 1; i < len(all); i++ {
		if all[i].CreatedAt.After(all[i-1].CreatedAt) {
			t.Fatalf("第 %d 条比前一条还新 —— 没有按时间倒序", i)
		}
	}
	// 每一条的统一状态都必须在 domain 的取值域内。
	for _, task := range all {
		switch task.State {
		case domain.StateQueued, domain.StateRunning, domain.StateSuccess,
			domain.StatePartial, domain.StateFailed, domain.StateCancelled:
		default:
			t.Fatalf("%s 的状态 %q 不在统一取值域里", task.Kind, task.State)
		}
	}

	// 逐个统一状态筛选，条数必须与 domain 的映射算出来的一致。
	want := map[domain.TaskState]int{}
	for kind, raws := range domain.AllRawStates() {
		for _, raw := range raws {
			want[domain.NormalizeState(kind, raw)]++
		}
	}
	for state, n := range want {
		got, err := r.ListTasks(ctx, taskWS, repo.TaskFilter{State: state, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != n {
			t.Errorf("筛选 %s 应有 %d 条，实际 %d —— SQL 的 case 与 domain 的映射对不上",
				state, n, len(got))
		}
	}
	// ⚠️ `pending` 必须落进 queued：少写一个 `when 'pending'` 的话，
	// 整图运行的排队任务会全部落进 failed，而用户看到的是一排"失败"的运行。
	queued, _ := r.ListTasks(ctx, taskWS, repo.TaskFilter{
		Kind: domain.KindWorkflow, State: domain.StateQueued, Limit: 10})
	if len(queued) != 1 {
		t.Fatalf("整图的 pending 应落进 queued，实际 %d 条", len(queued))
	}
	// ⚠️ partial_success 不摊平（AV-24）。
	partial, _ := r.ListTasks(ctx, taskWS, repo.TaskFilter{
		State: domain.StatePartial, Limit: 10})
	if len(partial) != 1 || partial[0].Kind != domain.KindWorkflow {
		t.Fatalf("partial_success 应单独存在，实际 %+v", partial)
	}
}

// ⚠️ 计数与列表用**同一个 union**：分开写的话筛选器上的数字与列表内容对不上，
// 而两个数都"没错"（同消息中心的未读数与列表必须同条件）。
func TestCountTasks_与列表一致(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	seedTasks(t, pool, next)

	counts, err := r.CountTasks(ctx, taskWS, "")
	if err != nil {
		t.Fatal(err)
	}
	for state, n := range counts {
		list, err := r.ListTasks(ctx, taskWS, repo.TaskFilter{State: state, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != n {
			t.Errorf("%s 计数 %d 与列表 %d 不一致", state, n, len(list))
		}
	}
}

// ⚠️ 每一段 union 都必须带 workspace_id：少一处就是一个跨租户泄漏，
// 而它不会报错 —— 用户只会在自己的任务列表里看到别人的任务。
func TestListTasks_租户隔离(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	seedTasks(t, pool, next)

	other, err := r.ListTasks(ctx, taskWS+1, repo.TaskFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Fatalf("别的工作区不该看到任何任务，实际 %d 条", len(other))
	}
}

// 跨表分页：翻完不重不漏。
func TestListTasks_跨表分页(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	seedTasks(t, pool, next)

	seen := map[string]bool{}
	var cursorAt *time.Time
	var cursorID int64
	total := 0
	for page := 0; page < 20; page++ {
		list, err := r.ListTasks(ctx, taskWS, repo.TaskFilter{
			Limit: 4, CursorAt: cursorAt, CursorID: cursorID})
		if err != nil {
			t.Fatal(err)
		}
		if len(list) == 0 {
			break
		}
		for _, v := range list {
			key := string(v.Kind) + ":" + itoa64(v.ID)
			if seen[key] {
				t.Fatalf("%s 出现了两次", key)
			}
			seen[key] = true
			total++
		}
		last := list[len(list)-1]
		cursorAt, cursorID = &last.CreatedAt, last.ID
		if len(list) < 4 {
			break
		}
	}
	expect := 0
	for _, raws := range domain.AllRawStates() {
		expect += len(raws)
	}
	if total != expect {
		t.Fatalf("翻完应得到 %d 条，实际 %d", expect, total)
	}
}

// ---------------------------------------------------------------- 作品审核

func TestSubmitWork_幂等且不重置审核状态(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	src := next()
	w := repo.Work{WorkspaceID: taskWS, SourceType: "generation", SourceID: src,
		MediaObjectID: next(), Title: "一张图"}

	first, err := r.SubmitWork(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	if first.ReviewStatus != "pending" {
		t.Fatalf("应落 pending，得到 %q", first.ReviewStatus)
	}
	// ⚠️ 重复提交回的是**已有的那一件**，不是报错也不是新建一件 ——
	// 新建的话同一张图会在审核队列里出现两次，运营得审两遍。
	second, err := r.SubmitWork(ctx, w)
	if err != nil {
		t.Fatalf("重复提交不该报错：%v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("重复提交应回同一件，%d vs %d", first.ID, second.ID)
	}

	// 先驳回，再重复提交 —— ⚠️ **不能把"已驳回"洗回"待审"**（那是绕过审核）。
	before := domain.WorkState{ReviewStatus: "pending"}
	after := domain.ApplyReview(before, domain.ActionReject)
	if _, err := r.ApplyReview(ctx, first.ID, before, after,
		domain.ActionReject, 1, "不合规"); err != nil {
		t.Fatal(err)
	}
	again, err := r.SubmitWork(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	if again.ReviewStatus != "rejected" {
		t.Fatalf("重复提交不该把已驳回洗回待审，得到 %q", again.ReviewStatus)
	}
	_ = pool
}

// ⚠️ 改 work 与写 content_review **必须一个事务**：只改不留痕的话
// "这件作品为什么被下架"就永远查不到了 —— 而那正是"后台可追踪发布原因"。
func TestApplyReview_留痕与CAS(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	src := next()
	w, err := r.SubmitWork(ctx, repo.Work{
		WorkspaceID: taskWS, SourceType: "generation", SourceID: src, Title: "作品"})
	if err != nil {
		t.Fatal(err)
	}

	before := domain.WorkState{ReviewStatus: "pending"}
	after := domain.ApplyReview(before, domain.ActionApprove)
	got, err := r.ApplyReview(ctx, w.ID, before, after, domain.ActionApprove, 7, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.ReviewStatus != "approved" || got.IsFeatured {
		t.Fatalf("通过不该自动加精，得到 %+v", got)
	}

	// ⚠️ CAS：拿**过期的** before 再提交一次必须落空 ——
	// 不带的话后点的那个会把前一个的结论悄悄覆盖掉，
	// 而 content_review 里会留下两条互相矛盾的记录。
	if _, err := r.ApplyReview(ctx, w.ID, before, after,
		domain.ActionApprove, 8, ""); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("过期的 CAS 应落空，得到 %v", err)
	}

	// 加精 → 下架 → 历史正序三条。
	cur := domain.WorkState{ReviewStatus: "approved"}
	nxt := domain.ApplyReview(cur, domain.ActionFeature)
	if _, err := r.ApplyReview(ctx, w.ID, cur, nxt, domain.ActionFeature, 7, ""); err != nil {
		t.Fatal(err)
	}
	cur = nxt
	nxt = domain.ApplyReview(cur, domain.ActionOffline)
	if _, err := r.ApplyReview(ctx, w.ID, cur, nxt, domain.ActionOffline, 7, "活动结束"); err != nil {
		t.Fatal(err)
	}

	hist, err := r.ReviewHistory(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 3 {
		t.Fatalf("应有 3 条留痕，实际 %d", len(hist))
	}
	// ⚠️ **正序**：排查"它为什么现在是这个状态"时人是顺着时间读的。
	want := []string{"approved", "featured", "offline"}
	for i, a := range want {
		if hist[i].Action != a {
			t.Fatalf("第 %d 条应是 %s，实际 %s", i, a, hist[i].Action)
		}
	}
	if hist[2].Reason != "活动结束" {
		t.Fatalf("下架原因应留痕，得到 %q", hist[2].Reason)
	}

	// ⚠️ 下架**不改 review_status**：混成一件的话，重新上架时那件作品
	// 会显示成"被驳回过"。
	final, _ := r.GetWork(ctx, w.ID)
	if final.ReviewStatus != "approved" || !final.IsOffline {
		t.Fatalf("下架只该改 is_offline，得到 %+v", final)
	}
}

func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

// ⚠️ **作者看自己的图与公开取图是两条路，判定条件完全不同**：
// 合成一个方法再加个布尔参数的话，总有一天会有人在公开路径上把它传成
// true —— 而那一下是把整个审核前的队列开放给了所有人。
func TestOwnMediaOf_按工作区限定且不要求公开(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	media := next()
	w, err := r.SubmitWork(ctx, repo.Work{
		WorkspaceID: taskWS, SourceType: "generation", SourceID: next(),
		MediaObjectID: media, Title: "待审作品"})
	if err != nil {
		t.Fatal(err)
	}

	// 作者本人：作品还是 pending，照样取得到 —— 否则"我的作品"页上全是破图。
	got, err := r.OwnMediaOf(ctx, w.ID, taskWS)
	if err != nil {
		t.Fatalf("作者应取得到自己待审作品的媒体：%v", err)
	}
	if got != media {
		t.Fatalf("媒体 ID 不对：%d vs %d", got, media)
	}

	// 同一件作品在**公开**路径上取不到（三条件一个都不满足）。
	if _, err := r.PublicMediaOf(ctx, w.ID); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("待审作品不该在公开路径上取得到：%v", err)
	}

	// 换一个工作区：⚠️ 回的必须是 ErrNotFound 而不是那张图 ——
	// 归属条件写在 SQL 里，不是"先查出来再比一下"。
	if _, err := r.OwnMediaOf(ctx, w.ID, taskWS+1); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("别的工作区不该取得到：%v", err)
	}
}
