//go:build integration

// 用户端营销接口的真库测试（MKT-T-04）：在投谓词、游标分页、公开封面判定。
//
// ⚠️ 时段**现算**：这里改完时间立刻再查，中间不跑任何作业 —— 靠作业翻开关的实现
// 在这里必红（MKT-AC-09 用户端部分）。
package integration

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/li904338203/tabtv/backend/internal/marketing/app"
	"github.com/li904338203/tabtv/backend/internal/marketing/domain"
	"github.com/li904338203/tabtv/backend/internal/marketing/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

type usableCatalog []app.ModelRef

func (u usableCatalog) UsableModels(context.Context, string) ([]app.ModelRef, error) { return u, nil }

func userSvc(r *repo.Repo, now time.Time, usable ...app.ModelRef) *app.Service {
	s := app.NewService(r, usableCatalog(usable), nil, app.Settings{MaxProductImages: 20, MaxVariants: 4, AgentEnabled: true})
	s.SetClock(func() time.Time { return now })
	return s
}

func enable(t *testing.T, r *repo.Repo, id int64) {
	t.Helper()
	if _, _, err := r.SetOptionEnabled(context.Background(), id, true, 1); err != nil {
		t.Fatal(err)
	}
}

func codes(items []app.PublicOption) []string {
	out := []string{}
	for _, p := range items {
		out = append(out, p.Code)
	}
	return out
}

func isNotFound(err error) bool {
	var be *httpx.BizError
	return errors.As(err, &be) && be.Code == errcode.NotFound
}

func TestListOptions_时段现算不跑作业(t *testing.T) {
	r, next := setup(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	svc := userSvc(r, now)

	o := opt(domain.KindCreative, "flash_sale")
	o.CoverMediaIDs = []int64{501}
	row := mustInsert(t, r, next, o)
	enable(t, r, row.ID)

	list := func() []string {
		t.Helper()
		page, err := svc.ListOptions(ctx, app.OptionsQuery{Kind: domain.KindCreative})
		if err != nil {
			t.Fatal(err)
		}
		return codes(page.Items)
	}
	if got := list(); !reflect.DeepEqual(got, []string{"flash_sale"}) {
		t.Fatalf("在投应可见：%v", got)
	}

	// 结束时间设为过去 → 立即不再返回（列表、详情、封面同一个判定）。
	past := now.Add(-time.Minute)
	o.EndAt = &past
	if _, err := r.UpdateOption(ctx, row.ID, 1, next(), 1, o, snap(o, 2)); err != nil {
		t.Fatal(err)
	}
	if got := list(); len(got) != 0 {
		t.Fatalf("已结束应立即消失：%v", got)
	}
	if _, err := svc.GetOption(ctx, "flash_sale"); !isNotFound(err) {
		t.Fatalf("已结束的详情应 404：%v", err)
	}
	if _, err := svc.CoverMedia(ctx, "flash_sale", 0); !isNotFound(err) {
		t.Fatalf("已结束的封面应 404：%v", err)
	}

	// 边界 [start, end)：结束时间恰为此刻 → 已结束。
	o.EndAt = &now
	if _, err := r.UpdateOption(ctx, row.ID, 2, next(), 1, o, snap(o, 3)); err != nil {
		t.Fatal(err)
	}
	if got := list(); len(got) != 0 {
		t.Fatalf("end == now 应已结束：%v", got)
	}

	// 开始时间设为将来 → 同样不返回；开始时间恰为此刻 → 在投。
	future := now.Add(time.Hour)
	o.StartAt, o.EndAt = &future, nil
	if _, err := r.UpdateOption(ctx, row.ID, 3, next(), 1, o, snap(o, 4)); err != nil {
		t.Fatal(err)
	}
	if got := list(); len(got) != 0 {
		t.Fatalf("未开始不应出现：%v", got)
	}
	o.StartAt = &now
	if _, err := r.UpdateOption(ctx, row.ID, 4, next(), 1, o, snap(o, 5)); err != nil {
		t.Fatal(err)
	}
	if got := list(); !reflect.DeepEqual(got, []string{"flash_sale"}) {
		t.Fatalf("start == now 应在投：%v", got)
	}
	// 停用 → 不返回（时段无关）。
	if _, _, err := r.SetOptionEnabled(ctx, row.ID, false, 1); err != nil {
		t.Fatal(err)
	}
	if got := list(); len(got) != 0 {
		t.Fatalf("停用不应出现：%v", got)
	}
	// 管理端列表仍可见（两套口径）。
	all, _, err := r.ListOptions(ctx, repo.OptionFilter{Kind: domain.KindCreative, Now: now})
	if err != nil || len(all) != 1 {
		t.Fatalf("管理端列表不按时段过滤：%v %d", err, len(all))
	}
}

func TestListOptions_游标分页不重不漏(t *testing.T) {
	r, next := setup(t)
	ctx := context.Background()
	now := time.Now().UTC()
	svc := userSvc(r, now, app.ModelRef{Code: "m_a", Name: "模型A"})

	var want []string
	for i := 0; i < 8; i++ {
		o := opt(domain.KindCreative, fmt.Sprintf("tpl_%02d", i))
		o.Sort = i / 3 // 有并列的 sort：游标必须带 id 才能不重不漏
		if i%2 == 0 {
			o.RecommendedModels = []string{"m_a"}
		}
		row := mustInsert(t, r, next, o)
		enable(t, r, row.ID)
		want = append(want, o.Code)
	}
	// 一条 Hook 与一条停用的创意不应混进来。
	mustInsert(t, r, next, opt(domain.KindHook, "hook_x"))
	mustInsert(t, r, next, opt(domain.KindCreative, "draft_x"))

	var got []string
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("翻页不收敛")
		}
		page, err := svc.ListOptions(ctx, app.OptionsQuery{Kind: domain.KindCreative, Cursor: cursor, Limit: 3})
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, codes(page.Items)...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("翻页结果应不重不漏且按 (sort,id)：\n得 %v\n要 %v", got, want)
	}

	// 按模型筛选：服务端筛，只要推荐了 m_a 的。
	page, err := svc.ListOptions(ctx, app.OptionsQuery{Kind: domain.KindCreative, Model: "m_a", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if got := codes(page.Items); !reflect.DeepEqual(got, []string{"tpl_00", "tpl_02", "tpl_04", "tpl_06"}) {
		t.Fatalf("按模型筛选不对：%v", got)
	}
	if m := page.Items[0].RecommendedModels; len(m) != 1 || m[0].Name != "模型A" || !m[0].Available {
		t.Fatalf("推荐模型应带名称：%+v", m)
	}
	// 「按模型」筛选项由在投创意的推荐模型派生。
	recs, err := r.VisibleRecommendedModels(ctx, domain.KindCreative, now)
	if err != nil || !reflect.DeepEqual(recs, []string{"m_a"}) {
		t.Fatalf("在投创意推荐的模型：%v %v", err, recs)
	}
	cfg, err := svc.Config(ctx)
	if err != nil || len(cfg.CreativeModelFilters) != 1 || cfg.CreativeModelFilters[0].Code != "m_a" {
		t.Fatalf("配置里的按模型筛选项：%v %+v", err, cfg.CreativeModelFilters)
	}
}

func TestOpenCover_不可见404(t *testing.T) {
	r, next := setup(t)
	ctx := context.Background()
	svc := userSvc(r, time.Now().UTC())

	o := opt(domain.KindStyle, "film_look")
	o.CoverMediaIDs = []int64{701}
	preview := int64(702)
	o.PreviewMediaID = &preview
	row := mustInsert(t, r, next, o)

	// 草稿（停用）：封面、示例视频、详情都是 404 —— 与「不存在」同一个错误。
	for _, f := range []func() error{
		func() error { _, err := svc.CoverMedia(ctx, "film_look", 0); return err },
		func() error { _, err := svc.PreviewMedia(ctx, "film_look"); return err },
		func() error { _, err := svc.CoverMedia(ctx, "not_exist", 0); return err },
	} {
		if err := f(); !isNotFound(err) {
			t.Fatalf("不可见应 404：%v", err)
		}
	}
	enable(t, r, row.ID)
	if id, err := svc.CoverMedia(ctx, "film_look", 0); err != nil || id != 701 {
		t.Fatalf("在投封面：%v %d", err, id)
	}
	if id, err := svc.PreviewMedia(ctx, "film_look"); err != nil || id != 702 {
		t.Fatalf("在投示例视频：%v %d", err, id)
	}
	if _, err := svc.CoverMedia(ctx, "film_look", 1); !isNotFound(err) {
		t.Fatalf("下标越界应 404：%v", err)
	}
	// 下线 → 封面直链立即取不到（列表与取图同一个判定）。
	if _, _, err := r.SetOptionEnabled(ctx, row.ID, false, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CoverMedia(ctx, "film_look", 0); !isNotFound(err) {
		t.Fatalf("下线后封面应 404：%v", err)
	}
	// 但钉住的版本照常取得回原文（Snapshot 不看是否在投）。
	snaps, err := svc.Snapshot(ctx, []domain.Pick{{Kind: domain.KindStyle, Code: "film_look", Version: 1}})
	if err != nil || len(snaps) != 1 || snaps[0].Prompt != "片段 v1" {
		t.Fatalf("下线后钉住的版本应照常返回：%v %+v", err, snaps)
	}
}
