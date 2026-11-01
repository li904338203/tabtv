//go:build integration

// 任务运维的真库测试（P13-03；GAP-07、AD-08）。
//
// ⚠️ 必须用真库：这一组要守的全部由 **SQL** 决定 ——
// 死信的"同一条只留一条待处理"靠部分唯一索引、处置靠 CAS、
// 项目列表的节点数靠 jsonb 当场算、恢复/删除靠带状态条件的更新。
package integration

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/ops/domain"
	"github.com/li904338203/tabtv/backend/internal/ops/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
)

func setup(t *testing.T) (*repo.Repo, *pgxpool.Pool, func() int64) {
	t.Helper()
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	return repo.New(pool, gen.Next), pool, gen.Next
}

// ⚠️ 死信投递也是 at-least-once：排空器可能看到同一条两次。
// 去重靠 `(queue, md5(payload))` 的**部分**唯一索引（只在 pending 时唯一）——
// 无条件唯一的话，一条被重投之后**再次失败**会被静默吞掉，
// 而那正是"重投没解决问题"的唯一证据。
func TestRecordDead_待处理去重而处置后可再记(t *testing.T) {
	r, _, _ := setup(t)
	ctx := context.Background()
	// 业务引用由 `domain.SniffAggregate` 在编排层认出来（那是纯解析，
	// 单测在 domain 里），仓储只负责存 —— 这里直接给。
	d := repo.DeadLetter{
		Queue: "generation.jobs", RoutingKey: "gen.job",
		Payload:       `{"aggregateType":"generation","aggregateId":"123"}`,
		AggregateType: "generation", AggregateID: 123,
		Reason: "rejected",
	}

	first, created, err := r.Record(ctx, d)
	if err != nil || !created {
		t.Fatalf("第一条应新建：created=%v err=%v", created, err)
	}
	if first.AggregateID != 123 || first.AggregateType != "generation" {
		t.Fatalf("业务引用应原样存下：%+v", first)
	}

	// 同一条再来一次：不新建、也不报错。
	_, created, err = r.Record(ctx, d)
	if err != nil {
		t.Fatalf("重复投递不该报错：%v", err)
	}
	if created {
		t.Fatal("重复投递不该新建第二条")
	}

	// 处置掉之后，**同样的消息再次死信是一次新的故障**，必须能再记一条。
	if _, err := r.MarkDead(ctx, first.ID, domain.DeadReplayed, 1, "重投", true); err != nil {
		t.Fatal(err)
	}
	second, created, err := r.Record(ctx, d)
	if err != nil || !created {
		t.Fatalf("处置后再次死信应能记下：created=%v err=%v", created, err)
	}
	if second.ID == first.ID {
		t.Fatal("应是新的一行")
	}
}

// ⚠️ 死信按定义就**可能是解不开的字节**。用 jsonb 存的话 PG 直接拒收，
// 于是最需要人看的那一条恰恰是唯一存不进来的。
// 含 NUL 的负载也要能落库（`pgtext.Safe` 清洗）。
func TestRecordDead_解不开的负载也要存得下(t *testing.T) {
	r, _, _ := setup(t)
	ctx := context.Background()
	d := repo.DeadLetter{
		Queue: "render.jobs", RoutingKey: "render.job",
		Payload: "这不是 JSON\x00里面还有个 NUL",
		Reason:  "rejected",
	}
	out, created, err := r.Record(ctx, d)
	if err != nil {
		t.Fatalf("解不开的负载也必须存得下：%v", err)
	}
	if !created {
		t.Fatal("应新建一条")
	}
	if strings.ContainsRune(out.Payload, 0) {
		t.Fatal("NUL 应被清掉（PG 的 text 列拒收它）")
	}
	// 认不出业务引用是**正常的**，不是数据缺陷 —— 那正是运维要看到的信号。
	if out.AggregateID != 0 || out.AggregateType != "" {
		t.Fatalf("解不开时不该编出业务引用：%+v", out)
	}
}

// ⚠️ 处置用 CAS：两个运维同时点重投时只有一个成功 ——
// 不带的话两条消息都会被投出去，而其中一类死信重投两次就是让上游计两次费。
func TestMarkDead_CAS只允许处置一次(t *testing.T) {
	r, _, _ := setup(t)
	ctx := context.Background()
	d, _, err := r.Record(ctx, repo.DeadLetter{
		Queue: "export.jobs", RoutingKey: "export.job", Payload: `{"a":1}`})
	if err != nil {
		t.Fatal(err)
	}

	out, err := r.MarkDead(ctx, d.ID, domain.DeadReplayed, 7, "第一个人", true)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != domain.DeadReplayed || out.ReplayCount != 1 || out.HandledBy != 7 {
		t.Fatalf("处置结果不对：%+v", out)
	}
	// 第二个人来晚了。
	if _, err := r.MarkDead(ctx, d.ID, domain.DeadDiscarded, 8, "第二个人", false); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("CAS 应落空，得到 %v", err)
	}
	// 终态不被覆盖。
	cur, err := r.GetDead(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Status != domain.DeadReplayed || cur.HandledBy != 7 {
		t.Fatalf("终态不该被后来者覆盖：%+v", cur)
	}

	// ⚠️ 追加备注是**追加事实不是改判**：状态不回退。
	if err := r.AppendDeadNote(ctx, d.ID, "重投时投递失败：connection refused"); err != nil {
		t.Fatal(err)
	}
	cur, _ = r.GetDead(ctx, d.ID)
	if cur.Status != domain.DeadReplayed {
		t.Fatalf("追加备注不该改状态：%s", cur.Status)
	}
	if !strings.Contains(cur.Note, "投递失败") || !strings.Contains(cur.Note, "第一个人") {
		t.Fatalf("备注应保留原文并追加：%q", cur.Note)
	}
}

func TestListDead_按状态筛选与待处理计数(t *testing.T) {
	r, _, _ := setup(t)
	ctx := context.Background()
	for i, q := range []string{"generation.jobs", "render.jobs", "export.jobs"} {
		if _, _, err := r.Record(ctx, repo.DeadLetter{
			Queue: q, RoutingKey: q, Payload: `{"i":` + string(rune('0'+i)) + `}`}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := r.CountDeadPending(ctx)
	if err != nil || n != 3 {
		t.Fatalf("应有 3 条待处理，得到 %d（%v）", n, err)
	}
	only, err := r.ListDead(ctx, repo.DeadFilter{Queue: "render.jobs", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(only) != 1 {
		t.Fatalf("按队列筛应 1 条，得到 %d", len(only))
	}
	// 处置一条之后，待处理数要跟着降 —— 它是运维台顶上那个"还有几条要管"。
	if _, err := r.MarkDead(ctx, only[0].ID, domain.DeadDiscarded, 1, "确认无需处理", false); err != nil {
		t.Fatal(err)
	}
	if n, _ = r.CountDeadPending(ctx); n != 2 {
		t.Fatalf("处置后应剩 2 条待处理，得到 %d", n)
	}
}

// ---------------------------------------------------------------- 项目运维

const opsWS = int64(990001)

func seedProject(t *testing.T, pool *pgxpool.Pool, next func() int64, name string) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		insert into workspace (id, kind, name, owner_user_id, payer_account_id)
		values ($1,'team',$2,1,1) on conflict (id) do nothing`,
		opsWS, "运维测试组"); err != nil {
		t.Fatal(err)
	}
	pid := next()
	if _, err := pool.Exec(ctx, `
		insert into project (id, workspace_id, created_by_user_id, name, canvas_data, status)
		values ($1,$2,1,$3,$4::jsonb,'active')`, pid, opsWS, name,
		`{"schemaVersion":1,"nodes":[{"id":"a"},{"id":"b"},{"id":"c"}],"edges":[{"id":"e1"}]}`,
	); err != nil {
		t.Fatal(err)
	}
	return pid
}

// ⚠️ 节点/边条数**从 canvasData 当场算**，不另存两列计数：
// 那两列要在每条保存路径上同步更新，而漏掉任何一条都不会报错 ——
// 症状是后台显示的与用户看到的对不上，且没人说得清哪个是对的。
func TestListProjects_计数当场算且空画布不报错(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	pid := seedProject(t, pool, next, "有节点的项目")
	// 一个刚建的、canvas_data 是 {} 的项目：不兜底的话整个列表打不开。
	empty := next()
	if _, err := pool.Exec(ctx, `
		insert into project (id, workspace_id, created_by_user_id, name, canvas_data, status)
		values ($1,$2,1,'空项目','{}'::jsonb,'active')`, empty, opsWS); err != nil {
		t.Fatal(err)
	}

	list, err := r.ListProjects(ctx, repo.ProjectFilter{Keyword: "990001", Limit: 50})
	if err != nil {
		t.Fatalf("空 canvasData 不该让列表打不开：%v", err)
	}
	got := map[int64]repo.ProjectRow{}
	for _, p := range list {
		got[p.ID] = p
	}
	if got[pid].NodeCount != 3 || got[pid].EdgeCount != 1 {
		t.Fatalf("节点/边计数不对：%+v", got[pid])
	}
	if got[empty].NodeCount != 0 || got[empty].EdgeCount != 0 {
		t.Fatalf("空画布应为 0：%+v", got[empty])
	}
}

// ⚠️ 名字里的 `%` 要转义：不转的话一个 `%` 会把全表拖出来。
func TestListProjects_按名字模糊查且转义通配符(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	seedProject(t, pool, next, "西游记分镜")
	seedProject(t, pool, next, "100%真实")

	hit, err := r.ListProjects(ctx, repo.ProjectFilter{Name: "西游", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(hit) != 1 {
		t.Fatalf("按名字应命中 1 条，得到 %d", len(hit))
	}
	// 用 `%` 查：它是字面量而不是通配符，所以只命中那一条名字里真有 % 的。
	hit, err = r.ListProjects(ctx, repo.ProjectFilter{Name: "%", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(hit) != 1 || hit[0].Name != "100%真实" {
		t.Fatalf("%% 应当字面量处理，得到 %d 条", len(hit))
	}
}

// ⚠️ 恢复/删除都带状态条件：返回 false 表示"它本来就是那样"。
// 静默成功的话客服会告诉用户"我已经帮你恢复了"，而用户那边什么都没变。
func TestProjectLifecycle_恢复删除都看本次是否真的改了(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	pid := seedProject(t, pool, next, "会被删的项目")

	// 还没删就恢复 → false。
	if ok, err := r.RestoreProject(ctx, pid); err != nil || ok {
		t.Fatalf("没删过的项目恢复应回 false：ok=%v err=%v", ok, err)
	}
	if ok, err := r.DeleteProject(ctx, pid); err != nil || !ok {
		t.Fatalf("删除应生效：ok=%v err=%v", ok, err)
	}
	// 重复删 → false（不刷新 deleted_at，否则保留期永远重新开始）。
	if ok, _ := r.DeleteProject(ctx, pid); ok {
		t.Fatal("重复删除不该再回 true")
	}

	before, err := r.GetProject(ctx, pid)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := r.RestoreProject(ctx, pid); err != nil || !ok {
		t.Fatalf("恢复应生效：ok=%v err=%v", ok, err)
	}
	after, err := r.GetProject(ctx, pid)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "active" || after.DeletedAt != nil {
		t.Fatalf("恢复后应是 active 且清掉 deleted_at：%+v", after)
	}
	// ⚠️ **不刷 version**：那是画布的乐观锁。恢复动了它的话，用户那个还开着的
	// 标签页下一次保存会撞冲突 —— 而他什么都没做错。
	if after.Version != before.Version {
		t.Fatalf("恢复不该动乐观锁版本：%d → %d", before.Version, after.Version)
	}
}

// ⚠️ 后台默认**包含回收站里的**：来查项目十有八九正是因为
// "用户说他的项目不见了"。
func TestListProjects_默认含回收站(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	pid := seedProject(t, pool, next, "删掉的")
	if _, err := r.DeleteProject(ctx, pid); err != nil {
		t.Fatal(err)
	}
	all, err := r.ListProjects(ctx, repo.ProjectFilter{Keyword: "990001", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, p := range all {
		if p.ID == pid {
			found = true
			if p.Status != "deleted" || p.DeletedAt == nil {
				t.Fatalf("应看得出它在回收站里：%+v", p)
			}
		}
	}
	if !found {
		t.Fatal("后台默认应列出回收站里的项目")
	}
	// 显式筛 active 时就不该出现。
	only, err := r.ListProjects(ctx, repo.ProjectFilter{
		Keyword: "990001", Status: "active", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range only {
		if p.ID == pid {
			t.Fatal("筛 active 时不该出现回收站里的")
		}
	}
}
