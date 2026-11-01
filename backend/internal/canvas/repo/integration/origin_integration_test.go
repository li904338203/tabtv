//go:build integration

// 项目来源 origin（MKT-T-16、MKD-3、MKT-AC-21）的真库测试。
//
// ⚠️ 必须用真库：kind=marketing 与游标分页在**同一条 SQL** 里完成、CHECK 约束只在库里，
// mock 仓储都测不出来。
package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/li904338203/tabtv/backend/internal/canvas/repo"
)

// mustInsertOrigin 走 Agent 建项目那条路（InsertWithIdemKey）带上来源。
func mustInsertOrigin(t *testing.T, r *repo.Repo, next func() int64, workspace int64,
	name, origin string) int64 {
	t.Helper()
	p := repo.Project{ID: next(), WorkspaceID: workspace, CreatedBy: 1, Name: name, Origin: origin}
	if err := r.InsertWithIdemKey(context.Background(), p, fmt.Sprintf("k-%d", p.ID)); err != nil {
		t.Fatal(err)
	}
	return p.ID
}

func TestProjectList_kind营销Agent画布三种筛选(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()

	plain := mustInsert(t, r, next, ws, "普通画布")
	agentProj := mustInsertOrigin(t, r, next, ws, "通用 Agent", "")
	insertSession(t, pool, next, ws, agentProj, "会话", "idle", time.Now())
	mkt := mustInsertOrigin(t, r, next, ws, "营销项目", repo.OriginMarketing)
	insertSession(t, pool, next, ws, mkt, "营销会话", "idle", time.Now())
	// 别的工作区的营销项目不能混进来。
	other := mustInsertOrigin(t, r, next, ws+1, "别人的营销", repo.OriginMarketing)
	insertSession(t, pool, next, ws+1, other, "会话", "idle", time.Now())

	list := func(kind repo.ProjectKind) map[int64]repo.Project {
		rows, err := r.Page(ctx, repo.PageQuery{WorkspaceID: ws, Limit: 20, Kind: kind})
		if err != nil {
			t.Fatal(err)
		}
		m := map[int64]repo.Project{}
		for _, p := range rows {
			m[p.ID] = p
		}
		return m
	}

	m := list(repo.KindMarketing)
	if len(m) != 1 || m[mkt].ID == 0 {
		t.Fatalf("kind=marketing 应只返回营销项目，得到 %v", m)
	}
	if m[mkt].Origin != repo.OriginMarketing || m[mkt].Agent == nil {
		t.Errorf("营销项目应带 origin 与会话，得到 %+v", m[mkt])
	}

	a := list(repo.KindAgent)
	if len(a) != 2 || a[mkt].ID == 0 || a[agentProj].ID == 0 {
		t.Errorf("kind=agent 应同时含营销项目与通用 Agent 项目，得到 %v", a)
	}
	c := list(repo.KindCanvas)
	if len(c) != 1 || c[plain].ID == 0 {
		t.Errorf("kind=canvas 应只含普通画布，营销项目不得出现，得到 %v", c)
	}
	all := list(repo.KindAll)
	if len(all) != 3 {
		t.Errorf("全部应为 3 个，得到 %d", len(all))
	}
	// 历史 / 非营销项目 origin 为空。
	if all[plain].Origin != "" || all[agentProj].Origin != "" {
		t.Errorf("非营销项目 origin 应为空，得到 %q / %q", all[plain].Origin, all[agentProj].Origin)
	}

	// 单项查询也读得出来源（详情接口用的是 GetWithCanvas）。
	got, err := r.GetWithCanvas(ctx, ws, mkt)
	if err != nil || got.Origin != repo.OriginMarketing {
		t.Errorf("详情应带 origin=marketing，得到 %q %v", got.Origin, err)
	}

	// 改名不碰来源：任何更新路径都不写 origin。
	name := "改过名"
	upd, err := r.UpdateMeta(ctx, ws, mkt, &name, nil, nil)
	if err != nil || upd.Origin != repo.OriginMarketing {
		t.Errorf("改名后 origin 应保持 marketing，得到 %q %v", upd.Origin, err)
	}
}

// MKT-AC-21④：50 条营销 + 50 条普通项目交错，按 kind=marketing 翻页不重不漏、页页满额。
//
// ⚠️ 拦的是「先分页再过滤」：那样每页条数不固定、还会出现空页而 hasMore=true。
func TestProjectList_营销筛选游标分页不重不漏(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()

	want := map[int64]bool{}
	for i := 0; i < 100; i++ {
		if i%2 == 0 {
			id := mustInsertOrigin(t, r, next, ws, fmt.Sprintf("营销-%d", i), repo.OriginMarketing)
			insertSession(t, pool, next, ws, id, "会话", "idle", time.Now())
			want[id] = true
		} else {
			mustInsert(t, r, next, ws, fmt.Sprintf("普通-%d", i))
		}
		// 一部分项目同一毫秒建（不 sleep），验证 id 作为第二排序键打破平局。
		if i%7 == 0 {
			time.Sleep(time.Millisecond)
		}
	}

	seen := map[int64]int{}
	q := repo.PageQuery{WorkspaceID: ws, Limit: 7, Kind: repo.KindMarketing}
	for pages := 0; ; pages++ {
		if pages > 20 {
			t.Fatal("翻页不收敛")
		}
		rows, err := r.Page(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range rows {
			seen[p.ID]++
		}
		if len(rows) < q.Limit {
			break
		}
		last := rows[len(rows)-1]
		q.CursorTime, q.CursorID = last.UpdatedAt, last.ID
	}
	if len(seen) != len(want) {
		t.Errorf("应返回 %d 个营销项目，得到 %d", len(want), len(seen))
	}
	for id := range want {
		if seen[id] != 1 {
			t.Errorf("营销项目 %d 出现 %d 次（应恰好 1 次）", id, seen[id])
		}
	}
	for id := range seen {
		if !want[id] {
			t.Errorf("混进了非营销项目 %d", id)
		}
	}
}

// ck_project_origin 只允许 null 或 'marketing'：其余入口的来源不落值（MKD-3）。
func TestProjectOrigin_CHECK只允许marketing(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()

	for _, bad := range []string{"canvas", "mcp", "Marketing"} {
		_, err := pool.Exec(ctx, `insert into project (id, workspace_id, created_by_user_id, name,
			canvas_data, status, origin) values ($1, $2, 1, 'x', '{}', 'active', $3)`, next(), ws, bad)
		if err == nil || !strings.Contains(err.Error(), "ck_project_origin") {
			t.Errorf("origin=%q 应被 ck_project_origin 拒绝，得到 %v", bad, err)
		}
	}
	ok := mustInsertOrigin(t, r, next, ws, "营销", repo.OriginMarketing)
	none := mustInsertOrigin(t, r, next, ws, "空来源", "")
	var got *string
	if err := pool.QueryRow(ctx, `select origin from project where id = $1`, none).Scan(&got); err != nil || got != nil {
		t.Errorf("空来源应落 null，得到 %v %v", got, err)
	}
	if err := pool.QueryRow(ctx, `select origin from project where id = $1`, ok).Scan(&got); err != nil ||
		got == nil || *got != "marketing" {
		t.Errorf("营销来源应落 marketing，得到 %v %v", got, err)
	}
}
