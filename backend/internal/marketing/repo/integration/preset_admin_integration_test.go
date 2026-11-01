//go:build integration

// 营销角色管理端（MKT-T-36）的真库测试：版本只随内容推进、乐观锁**真并发**、排序整笔回滚、启用闸。
//
// ⚠️ 乐观锁必须真并发测：顺序调用永远先撞上版本检查，把 `version = $2` 整条删掉照样绿（CLAUDE.md §5.7 末条）。
package integration

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/marketing/app"
	"github.com/li904338203/tabtv/backend/internal/marketing/domain"
	"github.com/li904338203/tabtv/backend/internal/marketing/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

func presetIn(name string, imgs ...int64) domain.PresetInput {
	if imgs == nil {
		imgs = []int64{}
	}
	return domain.PresetInput{Name: name, Summary: "说明", Appearance: "形象", ImageMediaIDs: imgs}
}

func newPreset(t *testing.T, r *repo.Repo, next func() int64, name string, imgs ...int64) repo.AdminPreset {
	t.Helper()
	// 操作者 ID 用雪花号：0 会被 nullif 吃掉，测不到 bigint 参数的类型推断。
	p, err := r.InsertPreset(context.Background(), next(), next(), presetIn(name, imgs...))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPresetAdmin_版本只随内容推进(t *testing.T) {
	r, next := setup(t)
	ctx := context.Background()
	voice := next()
	p := newPreset(t, r, next, "柚子君", next(), next())
	if p.IsEnabled || p.Version != 1 || p.Sort < 1 {
		t.Fatalf("新建应停用、version=1、排在末尾：%+v", p)
	}
	if _, changed, err := r.SetPresetEnabled(ctx, p.ID, true, next()); err != nil || !changed {
		t.Fatalf("启用：%v %v", changed, err)
	}
	if _, changed, err := r.SetPresetEnabled(ctx, p.ID, true, next()); err != nil || changed {
		t.Fatalf("重复启用应幂等不改：%v %v", changed, err)
	}
	if err := r.SortPresets(ctx, []int64{p.ID}); err != nil {
		t.Fatal(err)
	}
	if cur, _ := r.AdminGetPreset(ctx, p.ID); cur.Version != 1 || !cur.IsEnabled || cur.Sort != 1 {
		t.Fatalf("启停与排序不推进版本：%+v", cur)
	}
	for v := 1; v <= 3; v++ {
		in := presetIn("柚子君", p.ImageMediaIDs...)
		in.VoiceMediaID = &voice
		in.Summary = "改" + string(rune('0'+v))
		if _, err := r.UpdatePreset(ctx, p.ID, v, next(), in); err != nil {
			t.Fatalf("第 %d 次更新：%v", v, err)
		}
	}
	cur, err := r.AdminGetPreset(ctx, p.ID)
	if err != nil || cur.Version != 4 || cur.Summary != "改3" || cur.VoiceMediaID == nil || *cur.VoiceMediaID != voice {
		t.Fatalf("改三次后 version=4 且内容回读一致：%v %+v", err, cur)
	}
	if len(cur.ImageMediaIDs) != 2 || cur.ImageMediaIDs[0] != p.ImageMediaIDs[0] {
		t.Fatalf("图片顺序回读：%v", cur.ImageMediaIDs)
	}
	if _, err := r.UpdatePreset(ctx, 424242, 1, 0, presetIn("x")); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("不存在应 ErrNotFound（不是 ErrStale）：%v", err)
	}
	// 名称不区分大小写唯一。
	newPreset(t, r, next, "Momo")
	if _, err := r.InsertPreset(ctx, next(), 0, presetIn("momo")); !errors.Is(err, repo.ErrDuplicate) {
		t.Fatalf("同名（大小写不同）应 ErrDuplicate：%v", err)
	}
}

func TestPresetAdmin_乐观锁真并发(t *testing.T) {
	r, next := setup(t)
	ctx := context.Background()
	p := newPreset(t, r, next, "并发角色", next())
	const n = 8
	var ok, stale atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := r.UpdatePreset(ctx, p.ID, 1, next(), presetIn("并发角色", int64(100+i)))
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
		t.Fatalf("同一 version 并发更新应恰好成功一次：ok=%d stale=%d", ok.Load(), stale.Load())
	}
	if cur, _ := r.AdminGetPreset(ctx, p.ID); cur.Version != 2 {
		t.Fatalf("版本只应进位一次：%d", cur.Version)
	}
}

func TestPresetAdmin_排序整笔回滚(t *testing.T) {
	r, next := setup(t)
	ctx := context.Background()
	a := newPreset(t, r, next, "甲", next())
	b := newPreset(t, r, next, "乙", next())
	c := newPreset(t, r, next, "丙", next())
	before := map[int64]int{a.ID: a.Sort, b.ID: b.Sort, c.ID: c.Sort}
	if err := r.SortPresets(ctx, []int64{c.ID, a.ID, 424242}); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("含不存在 ID 应 ErrNotFound：%v", err)
	}
	for id, s := range before {
		if cur, _ := r.AdminGetPreset(ctx, id); cur.Sort != s {
			t.Fatalf("回滚后 sort 不应变：%d %d→%d", id, s, cur.Sort)
		}
	}
	if err := r.SortPresets(ctx, []int64{c.ID, a.ID, b.ID}); err != nil {
		t.Fatal(err)
	}
	// 启用后按 (sort, id) 读出的顺序 = 排序请求的顺序（用户端列表同一排序键，MKT-AC-38 管理端部分）。
	for _, id := range []int64{a.ID, b.ID, c.ID} {
		if _, _, err := r.SetPresetEnabled(ctx, id, true, 0); err != nil {
			t.Fatal(err)
		}
	}
	on := true
	list, _, err := r.AdminListPresets(ctx, repo.PresetAdminFilter{Enabled: &on})
	if err != nil || len(list) != 3 || list[0].ID != c.ID || list[1].ID != a.ID || list[2].ID != b.ID {
		t.Fatalf("排序后顺序应为 丙 甲 乙：%v %+v", err, list)
	}
	// 游标翻页：limit=2 → 下一页从 (sort, id) 之后接上。
	page, more, err := r.AdminListPresets(ctx, repo.PresetAdminFilter{Limit: 2})
	if err != nil || !more || len(page) != 2 {
		t.Fatalf("第一页：%v %v %d", err, more, len(page))
	}
	rest, more, err := r.AdminListPresets(ctx, repo.PresetAdminFilter{Limit: 2, CursorSort: page[1].Sort,
		CursorID: page[1].ID})
	if err != nil || more || len(rest) != 1 || rest[0].ID != b.ID {
		t.Fatalf("第二页：%v %v %+v", err, more, rest)
	}
	// 搜索转义：% 不匹配全部。
	if got, _, _ := r.AdminListPresets(ctx, repo.PresetAdminFilter{Q: "%"}); len(got) != 0 {
		t.Fatalf("q=%% 不应匹配全部：%d", len(got))
	}
}

type platformMedia map[int64]app.MediaFacts

func (m platformMedia) LookupMedia(_ context.Context, id int64) (app.MediaFacts, bool, error) {
	f, ok := m[id]
	return f, ok, nil
}

// 启用闸走真实仓储 + 服务：无图草稿可存、启用 40905 点名；补图后启用成功；用户工作区媒体任何时候拦。
func TestPresetAdmin_启用闸(t *testing.T) {
	r, next := setup(t)
	ctx := context.Background()
	cover, userImg := next(), next()
	svc := app.NewPresetAdmin(r, platformMedia{
		cover:   {ID: cover, Purpose: "material_cover", ContentType: "image/webp"},
		userImg: {ID: userImg, WorkspaceID: 77, Purpose: "upload_reference", ContentType: "image/png"},
	}, next)
	v, err := svc.Create(ctx, next(), presetIn("小青"))
	if err != nil {
		t.Fatalf("无图草稿应可存：%v", err)
	}
	_, err = svc.Enable(ctx, next(), v.ID)
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.InvalidStateTransition {
		t.Fatalf("无图启用应 40905：%v", err)
	}
	if cur, _ := r.AdminGetPreset(ctx, v.ID); cur.IsEnabled {
		t.Fatal("启用闸没拦住")
	}
	if _, err := svc.Update(ctx, next(), v.ID, 1, presetIn("小青", userImg)); !errors.As(err, &be) ||
		be.Code != errcode.InvalidArgument {
		t.Fatalf("用户工作区媒体应任何时候拦：%v", err)
	}
	if _, err := svc.Update(ctx, next(), v.ID, 1, presetIn("小青", cover)); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Enable(ctx, next(), v.ID)
	if err != nil || !got.IsEnabled || got.Version != 2 {
		t.Fatalf("补图后应能启用且版本不因启用推进：%v %+v", err, got.AdminPreset)
	}
}
