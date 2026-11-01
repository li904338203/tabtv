//go:build integration

// 商品库数据层（MKT-T-33）的真库测试：工作区隔离、乐观锁 / 配额的**真并发**、搜索转义、
// 游标分页、媒体引用检查器只认未删除。
//
// ⚠️ 乐观锁与配额都必须真并发测：顺序调用永远先撞上检查，那种测试把 `version = $n`
// 或 advisory 锁整条删掉照样绿（CLAUDE.md §5.7 末条、MKR-19）。
package integration

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/marketing/repo"
)

func newProduct(t *testing.T, r *repo.Repo, next func() int64, ws int64, name string, imgs ...int64) repo.Product {
	t.Helper()
	if len(imgs) == 0 {
		imgs = []int64{next()}
	}
	// 操作者 ID 用雪花号：0 会被 nullif 吃掉，测不到 bigint 参数的类型推断（int4 溢出）。
	p, err := r.InsertProduct(context.Background(), repo.Product{ID: next(), WorkspaceID: ws, Name: name,
		ImageMediaIDs: imgs, CreatedBy: next()}, 500)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProduct_工作区隔离(t *testing.T) {
	r, next := setup(t)
	ctx := context.Background()
	const a, b = 1001, 1002
	p := newProduct(t, r, next, a, "A 的商品")
	if got, err := r.GetProduct(ctx, a, p.ID); err != nil || got.Name != "A 的商品" || got.Version != 1 {
		t.Fatalf("本工作区应取得到：%v %+v", err, got)
	}
	if _, err := r.GetProduct(ctx, b, p.ID); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("B 取 A 的商品应 ErrNotFound：%v", err)
	}
	if _, err := r.UpdateProduct(ctx, repo.Product{ID: p.ID, WorkspaceID: b, Name: "改", ImageMediaIDs: []int64{1}}, 1); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("B 改 A 的商品应 ErrNotFound（不是 ErrStale）：%v", err)
	}
	if _, err := r.SoftDeleteProduct(ctx, b, p.ID, 0); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("B 删 A 的商品应 ErrNotFound：%v", err)
	}
	list, _, err := r.ListProducts(ctx, repo.ProductFilter{WorkspaceID: b})
	if err != nil || len(list) != 0 {
		t.Fatalf("B 的列表应为空：%v %d", err, len(list))
	}
	// 软删后本工作区也取不到、列表消失、再删 404。
	if _, err := r.SoftDeleteProduct(ctx, a, p.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetProduct(ctx, a, p.ID); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("软删后应取不到：%v", err)
	}
	if list, _, _ := r.ListProducts(ctx, repo.ProductFilter{WorkspaceID: a}); len(list) != 0 {
		t.Fatalf("软删后列表应消失：%d", len(list))
	}
	if _, err := r.SoftDeleteProduct(ctx, a, p.ID, 0); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("重复删除应 ErrNotFound：%v", err)
	}
}

func TestProduct_乐观锁真并发(t *testing.T) {
	r, next := setup(t)
	ctx := context.Background()
	p := newProduct(t, r, next, 2001, "原名")
	const n = 8
	var ok, stale atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := r.UpdateProduct(ctx, repo.Product{ID: p.ID, WorkspaceID: 2001, Name: "并发改",
				ImageMediaIDs: []int64{int64(100 + i)}, UpdatedBy: next()}, 1)
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
	if cur, _ := r.GetProduct(ctx, 2001, p.ID); cur.Version != 2 {
		t.Fatalf("版本只应进位一次：%d", cur.Version)
	}
}

func TestProduct_配额并发(t *testing.T) {
	r, next := setup(t)
	ctx := context.Background()
	const ws, max, n = 3001, 3, 10
	ids := make([]int64, n)
	for i := range ids {
		ids[i] = next()
	}
	var ok, quota atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := r.InsertProduct(ctx, repo.Product{ID: ids[i], WorkspaceID: ws, Name: "并发",
				ImageMediaIDs: []int64{ids[i]}}, max)
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, repo.ErrQuota):
				quota.Add(1)
			default:
				t.Errorf("意外错误：%v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	list, _, _ := r.ListProducts(ctx, repo.ProductFilter{WorkspaceID: ws, Limit: 100})
	if ok.Load() != max || quota.Load() != n-max || len(list) != max {
		t.Fatalf("并发新增不应越过上限 %d：ok=%d quota=%d 实际 %d 行", max, ok.Load(), quota.Load(), len(list))
	}
	// 软删一个就腾出一个名额；别的工作区不受影响。
	if _, err := r.SoftDeleteProduct(ctx, ws, list[0].ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.InsertProduct(ctx, repo.Product{ID: next(), WorkspaceID: ws, Name: "补", ImageMediaIDs: []int64{1}}, max); err != nil {
		t.Fatalf("软删后应腾出名额：%v", err)
	}
	if _, err := r.InsertProduct(ctx, repo.Product{ID: next(), WorkspaceID: ws + 1, Name: "别家", ImageMediaIDs: []int64{1}}, max); err != nil {
		t.Fatalf("配额按工作区计：%v", err)
	}
}

func TestProduct_搜索转义(t *testing.T) {
	r, next := setup(t)
	ctx := context.Background()
	const ws = 4001
	newProduct(t, r, next, ws, "100%纯果汁")
	newProduct(t, r, next, ws, "1000ml 装")
	newProduct(t, r, next, ws, "a_b 款")
	newProduct(t, r, next, ws, "axb 款")
	for q, want := range map[string]int{"100%": 1, "a_b": 1, "纯果": 1, "款": 2, "ML": 1, "%": 1, "_": 1} {
		list, _, err := r.ListProducts(ctx, repo.ProductFilter{WorkspaceID: ws, Q: q})
		if err != nil || len(list) != want {
			t.Errorf("搜 %q 应命中 %d 个，得到 %d（%v）", q, want, len(list), err)
		}
	}
}

func TestProduct_游标分页三种排序(t *testing.T) {
	r, next := setup(t)
	ctx := context.Background()
	const ws = 5001
	names := []string{"c", "A", "b", "E", "d"}
	for _, n := range names {
		newProduct(t, r, next, ws, n)
	}
	for _, s := range []repo.ProductSort{repo.ProductSortUpdated, repo.ProductSortCreated, repo.ProductSortName} {
		seen := map[int64]bool{}
		var order []string
		cursor := ""
		for page := 0; page < 10; page++ {
			list, more, err := r.ListProducts(ctx, repo.ProductFilter{WorkspaceID: ws, Sort: s, Cursor: cursor, Limit: 2})
			if err != nil {
				t.Fatalf("%s：%v", s, err)
			}
			for _, p := range list {
				if seen[p.ID] {
					t.Fatalf("%s 翻页出现重复：%s", s, p.Name)
				}
				seen[p.ID] = true
				order = append(order, p.Name)
			}
			if !more {
				break
			}
			cursor = repo.EncodeProductCursor(s, list[len(list)-1])
		}
		if len(seen) != len(names) {
			t.Fatalf("%s 翻页漏行：%v", s, order)
		}
		if s == repo.ProductSortName && (order[0] != "A" || order[4] != "E") {
			t.Fatalf("按名称应不区分大小写升序：%v", order)
		}
		if s == repo.ProductSortCreated && order[0] != "d" {
			t.Fatalf("按创建应最新在前：%v", order)
		}
	}
	// 拿 updated 的游标翻 name 排序：拒绝，而不是翻出错乱的一页。
	list, _, _ := r.ListProducts(ctx, repo.ProductFilter{WorkspaceID: ws, Limit: 1})
	cur := repo.EncodeProductCursor(repo.ProductSortUpdated, list[0])
	if _, _, err := r.ListProducts(ctx, repo.ProductFilter{WorkspaceID: ws, Sort: repo.ProductSortName, Cursor: cur}); !errors.Is(err, repo.ErrBadCursor) {
		t.Fatalf("排序不匹配的游标应 ErrBadCursor：%v", err)
	}
}

func TestProductMediaReferences_未删除才命中(t *testing.T) {
	r, next := setup(t)
	ctx := context.Background()
	const img = int64(987654321012)
	p1 := newProduct(t, r, next, 6001, "一", img, 1)
	p2 := newProduct(t, r, next, 6002, "二（别的工作区）", 2, img)
	refs, err := r.ProductMediaReferences(ctx, img)
	if err != nil || len(refs) != 2 {
		t.Fatalf("跨工作区应命中两个商品：%v %v", err, refs)
	}
	if refs, _ := r.ProductMediaReferences(ctx, 3); len(refs) != 0 {
		t.Fatalf("没用到的图不应命中：%v", refs)
	}
	if _, err := r.SoftDeleteProduct(ctx, 6001, p1.ID, next()); err != nil {
		t.Fatal(err)
	}
	refs, _ = r.ProductMediaReferences(ctx, img)
	if len(refs) != 1 || refs[0] != p2.ID {
		t.Fatalf("软删后不应再命中：%v", refs)
	}
}
