//go:build integration

// 项目类型筛选与「最近活跃的 Agent 会话」（AGT-45）的真库测试。
//
// ⚠️ 必须用真库：筛选与游标分页在**同一条 SQL** 里完成是这一卡的全部意义 ——
// 先分页再过滤会让页不满（甚至空页而 hasMore=true），mock 仓储测不出来。
package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/canvas/repo"
)

// insertSession 直接写一行 agent_session（只填必需列）。
func insertSession(t *testing.T, pool *pgxpool.Pool, next func() int64, workspace, projectID int64,
	title, state string, lastActive time.Time) int64 {
	t.Helper()
	id := next()
	if _, err := pool.Exec(context.Background(), `
		insert into agent_session (id, workspace_id, project_id, payer_account_id, created_by_user_id,
			title, state, last_active_at)
		values ($1, $2, $3, 1, 1, $4, $5, $6)`,
		id, workspace, projectID, title, state, lastActive); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestPage_类型筛选与最近会话(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()

	plain := mustInsert(t, r, next, ws, "普通画布")
	time.Sleep(2 * time.Millisecond)
	agentProj := mustInsert(t, r, next, ws, "Agent 项目")
	time.Sleep(2 * time.Millisecond)
	cancelledOnly := mustInsert(t, r, next, ws, "只有已取消会话")

	base := time.Now().Add(-time.Hour)
	insertSession(t, pool, next, ws, agentProj, "旧会话", "idle", base)
	newest := insertSession(t, pool, next, ws, agentProj, "新会话", "awaiting_approval", base.Add(10*time.Minute))
	// 已取消的会话也算历史：项目仍是 Agent 类型。
	cancelled := insertSession(t, pool, next, ws, cancelledOnly, "取消了", "cancelled", base)

	ids := func(rows []repo.Project) map[int64]repo.Project {
		m := map[int64]repo.Project{}
		for _, p := range rows {
			m[p.ID] = p
		}
		return m
	}

	all, err := r.Page(ctx, repo.PageQuery{WorkspaceID: ws, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	m := ids(all)
	if len(m) != 3 {
		t.Fatalf("all 应返回 3 个项目，得到 %d", len(m))
	}
	if m[plain].Agent != nil {
		t.Errorf("没有会话的项目 agent 应为 nil，得到 %+v", m[plain].Agent)
	}
	a := m[agentProj].Agent
	if a == nil || a.SessionID != newest || a.Title != "新会话" || a.State != "awaiting_approval" {
		t.Errorf("应取最近活跃的会话，得到 %+v", a)
	}
	if a != nil && a.LastActiveAt.IsZero() {
		t.Errorf("lastActiveAt 不应为零值")
	}
	if c := m[cancelledOnly].Agent; c == nil || c.SessionID != cancelled || c.State != "cancelled" {
		t.Errorf("只有已取消会话的项目也要带出会话，得到 %+v", c)
	}

	agents, err := r.Page(ctx, repo.PageQuery{WorkspaceID: ws, Limit: 10, Kind: repo.KindAgent})
	if err != nil {
		t.Fatal(err)
	}
	if m := ids(agents); len(m) != 2 || m[agentProj].ID == 0 || m[cancelledOnly].ID == 0 {
		t.Errorf("kind=agent 应只返回两个有会话的项目，得到 %v", m)
	}

	canvas, err := r.Page(ctx, repo.PageQuery{WorkspaceID: ws, Limit: 10, Kind: repo.KindCanvas})
	if err != nil {
		t.Fatal(err)
	}
	if len(canvas) != 1 || canvas[0].ID != plain || canvas[0].Agent != nil {
		t.Errorf("kind=canvas 应只返回普通画布项目，得到 %+v", canvas)
	}

	// 一个会话行出现多次时 lateral 只取一条：列表里同一个项目不能出现两次。
	seen := map[int64]int{}
	for _, p := range all {
		seen[p.ID]++
	}
	if seen[agentProj] != 1 {
		t.Errorf("有两个会话的项目在列表里出现了 %d 次", seen[agentProj])
	}
}

// 筛选下的游标分页：Agent 与普通项目交错排列，按 kind 翻页必须不漏不重、页页满额。
//
// ⚠️ 这条要拦的是「先分页再过滤」：那样每页条数不固定、还会出现空页而 hasMore=true。
func TestPage_筛选下游标翻页不漏不重(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()

	const total = 11
	want := map[repo.ProjectKind]map[int64]bool{repo.KindAgent: {}, repo.KindCanvas: {}}
	for i := 0; i < total; i++ {
		id := mustInsert(t, r, next, ws, fmt.Sprintf("项目-%d", i))
		if i%3 != 0 { // 交错：每三个里两个是 Agent 项目
			insertSession(t, pool, next, ws, id, "会话", "idle", time.Now())
			want[repo.KindAgent][id] = true
		} else {
			want[repo.KindCanvas][id] = true
		}
		time.Sleep(2 * time.Millisecond)
	}
	// 另一个工作区的 Agent 项目不能混进来。
	other := mustInsert(t, r, next, ws+1, "别人的")
	insertSession(t, pool, next, ws+1, other, "会话", "idle", time.Now())

	for kind, expect := range want {
		seen := map[int64]int{}
		q := repo.PageQuery{WorkspaceID: ws, Limit: 3, Kind: kind}
		for pages := 0; ; pages++ {
			if pages > 10 {
				t.Fatalf("%s 翻页不收敛", kind)
			}
			rows, err := r.Page(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range rows {
				seen[p.ID]++
				if (p.Agent != nil) != (kind == repo.KindAgent) {
					t.Errorf("%s 筛选下项目 %d 的 agent=%+v 与类型不符", kind, p.ID, p.Agent)
				}
			}
			if len(rows) < q.Limit {
				break
			}
			last := rows[len(rows)-1]
			q.CursorTime, q.CursorID = last.UpdatedAt, last.ID
		}
		if len(seen) != len(expect) {
			t.Errorf("%s 应返回 %d 个，得到 %d", kind, len(expect), len(seen))
		}
		for id := range expect {
			if seen[id] != 1 {
				t.Errorf("%s 下项目 %d 出现 %d 次（应恰好 1 次）", kind, id, seen[id])
			}
		}
		for id := range seen {
			if !expect[id] {
				t.Errorf("%s 下混进了不该出现的项目 %d", kind, id)
			}
		}
	}
}

// 首页（内容域经端口）用的批量查询：与列表同一条「最近」规则，且受工作区约束。
func TestAgentSummaries_与列表同一规则且隔离工作区(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()

	plain := mustInsert(t, r, next, ws, "普通")
	agentProj := mustInsert(t, r, next, ws, "Agent")
	base := time.Now().Add(-time.Hour)
	insertSession(t, pool, next, ws, agentProj, "旧", "idle", base)
	newest := insertSession(t, pool, next, ws, agentProj, "新", "executing", base.Add(time.Minute))

	m, err := r.AgentSummaries(ctx, ws, []int64{plain, agentProj})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m[plain]; ok {
		t.Errorf("没有会话的项目不应出现")
	}
	if got := m[agentProj]; got.SessionID != newest || got.State != "executing" {
		t.Errorf("应取最近活跃的会话，得到 %+v", got)
	}

	page, err := r.Page(ctx, repo.PageQuery{WorkspaceID: ws, Limit: 10, Kind: repo.KindAgent})
	if err != nil || len(page) != 1 || page[0].Agent == nil || page[0].Agent.SessionID != m[agentProj].SessionID {
		t.Errorf("列表与批量查询应指向同一个会话：page=%+v err=%v", page, err)
	}

	other, err := r.AgentSummaries(ctx, ws+1, []int64{agentProj})
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Errorf("跨工作区不应查到别人的会话，得到 %v", other)
	}
	empty, err := r.AgentSummaries(ctx, ws, nil)
	if err != nil || len(empty) != 0 {
		t.Errorf("空 ID 列表应返回空，得到 %v %v", empty, err)
	}
}
