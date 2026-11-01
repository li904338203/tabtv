//go:build integration

// 营销选项数据层（MKT-T-03）的真库测试。
//
// ⚠️ 乐观锁必须真并发测：顺序调用永远先撞上版本检查，那种测试把 `version = $2`
// 整条删掉照样绿（CLAUDE.md §5.7）。
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/li904338203/tabtv/backend/internal/marketing/domain"
	"github.com/li904338203/tabtv/backend/internal/marketing/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
)

func setup(t *testing.T) (*repo.Repo, func() int64) {
	t.Helper()
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	return repo.New(pool), gen.Next
}

func opt(kind domain.Kind, code string) domain.Option {
	return domain.Option{Kind: kind, Code: code, Name: "名称", CategoryCode: "cat", Prompt: "片段 v1",
		OutputTypes: []string{"video"}, CoverMediaIDs: []int64{}, Tags: []string{}}
}

func snap(o domain.Option, v int) json.RawMessage {
	b, _ := json.Marshal(domain.Snapshot{Pick: domain.Pick{Kind: o.Kind, Code: o.Code, Version: v},
		Name: o.Name, Prompt: o.Prompt})
	return b
}

func mustInsert(t *testing.T, r *repo.Repo, next func() int64, o domain.Option) repo.Option {
	t.Helper()
	row, err := r.InsertOption(context.Background(), next(), next(), 1, o, snap(o, 1))
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func TestMarketingOption_保存版本加一且历史同事务(t *testing.T) {
	r, next := setup(t)
	ctx := context.Background()
	o := opt(domain.KindCreative, "giant_drop")
	row := mustInsert(t, r, next, o)
	if row.Version != 1 || row.IsEnabled {
		t.Fatalf("新建应 version=1 且停用：%+v", row)
	}
	o.Prompt = "片段 v2"
	up, err := r.UpdateOption(ctx, row.ID, 1, next(), 1, o, snap(o, 2))
	if err != nil || up.Version != 2 || up.Prompt != "片段 v2" {
		t.Fatalf("保存应 version+1：%v %+v", err, up)
	}
	vs, _ := r.ListVersions(ctx, row.ID)
	if len(vs) != 2 || vs[0].Version != 2 {
		t.Fatalf("历史应恰好多一行：%+v", vs)
	}

	// 同事务：版本行插不进去（人为占住 version=3）时，选项本身也不能前进。
	if _, err := r.UpdateOption(ctx, row.ID, 2, vs[0].ID, 1, o, snap(o, 3)); err == nil {
		t.Fatal("版本历史主键冲突时整笔应失败")
	}
	cur, _ := r.GetOption(ctx, row.ID)
	if cur.Version != 2 {
		t.Fatalf("历史写失败后选项不应前进，实际 version=%d", cur.Version)
	}
}

func TestMarketingOption_乐观锁并发只成功一次(t *testing.T) {
	r, next := setup(t)
	ctx := context.Background()
	o := opt(domain.KindHook, "boom_sound")
	row := mustInsert(t, r, next, o)

	const n = 8
	ids := make([][2]int64, n)
	for i := range ids {
		ids[i] = [2]int64{next(), 0}
	}
	var ok, stale atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := r.UpdateOption(ctx, row.ID, 1, ids[i][0], 1, o, snap(o, 2))
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, repo.ErrStale):
				stale.Add(1)
			default:
				t.Errorf("意外错误：%v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if ok.Load() != 1 || stale.Load() != n-1 {
		t.Fatalf("同一 version 并发保存应恰好成功一次：ok=%d stale=%d", ok.Load(), stale.Load())
	}
	cur, _ := r.GetOption(ctx, row.ID)
	vs, _ := r.ListVersions(ctx, row.ID)
	if cur.Version != 2 || len(vs) != 2 {
		t.Fatalf("版本只应进位一次：version=%d 历史 %d 行", cur.Version, len(vs))
	}
}

func TestMarketingOption_版本历史只增(t *testing.T) {
	r, next := setup(t)
	ctx := context.Background()
	o := opt(domain.KindCreative, "split_screen")
	row := mustInsert(t, r, next, o)
	for v := 1; v <= 3; v++ {
		o.Prompt = "片段第" + string(rune('A'+v)) + "版"
		if _, err := r.UpdateOption(ctx, row.ID, v, next(), 1, o, snap(o, v+1)); err != nil {
			t.Fatal(err)
		}
	}
	vs, _ := r.ListVersions(ctx, row.ID)
	if len(vs) != 4 {
		t.Fatalf("改三次后应有 4 个版本，实际 %d", len(vs))
	}
	old, err := r.GetVersionByCode(ctx, "split_screen", 1)
	if err != nil {
		t.Fatal(err)
	}
	var s domain.Snapshot
	_ = json.Unmarshal(old.Snapshot, &s)
	if s.Prompt != "片段 v1" || s.Version != 1 {
		t.Fatalf("旧版本原文应取得回：%+v", s)
	}
	if _, err := r.GetVersionByCode(ctx, "split_screen", 9); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("不存在的版本应 ErrNotFound：%v", err)
	}
}

func TestMarketingOption_code唯一跨kind(t *testing.T) {
	r, next := setup(t)
	mustInsert(t, r, next, opt(domain.KindCreative, "same_code"))
	o := opt(domain.KindStyle, "same_code")
	if _, err := r.InsertOption(context.Background(), next(), next(), 1, o, snap(o, 1)); !errors.Is(err, repo.ErrDuplicate) {
		t.Fatalf("跨 kind 重复 code 应 ErrDuplicate：%v", err)
	}
}

func TestMarketingOption_时段CHECK拒倒置与零长度(t *testing.T) {
	r, next := setup(t)
	s := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for name, e := range map[string]time.Time{"倒置": s.Add(-time.Hour), "零长度": s} {
		o := opt(domain.KindCreative, "window_"+map[string]string{"倒置": "rev", "零长度": "zero"}[name])
		end := e
		o.StartAt, o.EndAt = &s, &end
		if _, err := r.InsertOption(context.Background(), next(), next(), 1, o, snap(o, 1)); err == nil {
			t.Errorf("%s的时段应被 CHECK 拒绝", name)
		}
	}
}

func TestMarketingOption_列表不按时段过滤(t *testing.T) {
	r, next := setup(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	future, past, farPast := now.Add(time.Hour), now.Add(-time.Hour), now.Add(-48*time.Hour)

	mk := func(code string, start, end *time.Time, enabled bool) int64 {
		o := opt(domain.KindCreative, code)
		o.StartAt, o.EndAt = start, end
		row := mustInsert(t, r, next, o)
		if enabled {
			if _, _, err := r.SetOptionEnabled(ctx, row.ID, true, 1); err != nil {
				t.Fatal(err)
			}
		}
		return row.ID
	}
	mk("not_started", &future, nil, true)
	mk("ended_one", &farPast, &past, true)
	mk("live_one", nil, nil, true)
	mk("off_one", nil, nil, false)

	all, _, err := r.ListOptions(ctx, repo.OptionFilter{Now: now})
	if err != nil || len(all) != 4 {
		t.Fatalf("管理端列表应返回全部 4 条（不按时段过滤）：%v %d", err, len(all))
	}
	for st, want := range map[domain.LifecycleState]string{
		domain.LifecycleNotStarted: "not_started", domain.LifecycleEnded: "ended_one",
		domain.LifecycleLive: "live_one", domain.LifecycleDisabled: "off_one",
	} {
		got, _, err := r.ListOptions(ctx, repo.OptionFilter{Now: now, Lifecycle: st})
		if err != nil || len(got) != 1 || got[0].Code != want {
			t.Errorf("lifecycle=%s 应只命中 %s：%v %+v", st, want, err, got)
			continue
		}
		if domain.Lifecycle(got[0].Option, now) != st {
			t.Errorf("SQL 谓词与 domain.Lifecycle 不一致：%s", want)
		}
	}

	// 游标分页：每页 3 条，第二页接得上且不重不漏。
	p1, more, _ := r.ListOptions(ctx, repo.OptionFilter{Now: now, Limit: 3})
	if len(p1) != 3 || !more {
		t.Fatalf("第一页应 3 条且还有下一页：%d %v", len(p1), more)
	}
	last := p1[2]
	p2, more, _ := r.ListOptions(ctx, repo.OptionFilter{Now: now, Limit: 3, CursorSort: last.Sort, CursorID: last.ID})
	if len(p2) != 1 || more {
		t.Fatalf("第二页应 1 条且结束：%d %v", len(p2), more)
	}

	// 启停不推进内容版本。
	cur, _ := r.GetOptionByCode(ctx, "live_one")
	if cur.Version != 1 {
		t.Fatalf("启用不应推进 version，实际 %d", cur.Version)
	}
}

func TestMarketingCategory_排序与code不可改(t *testing.T) {
	r, next := setup(t)
	ctx := context.Background()
	mk := func(kind domain.Kind, code string) repo.Category {
		c, err := r.InsertCategory(ctx, repo.Category{ID: next(), Category: domain.Category{Kind: kind, Code: code,
			Name: code, IsEnabled: true}})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	a, b := mk(domain.KindCreative, "aaa"), mk(domain.KindCreative, "bbb")
	h := mk(domain.KindHook, "aaa") // 同 code 不同 kind 允许
	if _, err := r.InsertCategory(ctx, repo.Category{ID: next(), Category: domain.Category{Kind: domain.KindCreative,
		Code: "aaa", Name: "x"}}); !errors.Is(err, repo.ErrDuplicate) {
		t.Fatalf("同 kind 重复 code 应 ErrDuplicate：%v", err)
	}

	if err := r.SortCategories(ctx, domain.KindCreative, []int64{b.ID, a.ID}); err != nil {
		t.Fatal(err)
	}
	cs, _ := r.ListCategories(ctx, domain.KindCreative)
	if len(cs) != 2 || cs[0].Code != "bbb" || cs[1].Code != "aaa" {
		t.Fatalf("排序后应 bbb 在前：%+v", cs)
	}
	// 混进别的 kind 的分类：整笔回滚。
	if err := r.SortCategories(ctx, domain.KindCreative, []int64{a.ID, h.ID}); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("跨 kind 排序应拒绝：%v", err)
	}
	cs, _ = r.ListCategories(ctx, domain.KindCreative)
	if cs[0].Code != "bbb" {
		t.Fatal("被拒的排序不应生效")
	}

	up, err := r.UpdateCategory(ctx, a.ID, "改名", false)
	if err != nil || up.Code != "aaa" || up.Name != "改名" || up.IsEnabled {
		t.Fatalf("改名 / 停用不改 code：%v %+v", err, up)
	}
}
