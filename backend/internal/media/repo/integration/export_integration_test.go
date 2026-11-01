//go:build integration

// 派生媒体与异步导出的真库测试（P09-03）。
//
// ⚠️ 必须用真库：派生的唯一约束、CAS 领取、过期扫描的 join 都写在 SQL 里，
// 内存假实现证明不了它们 —— 而这三件事各自都有"看着成功其实错了"的失败模式。
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/li904338203/tabtv/backend/internal/media/app"
	"github.com/li904338203/tabtv/backend/internal/media/domain"
	"github.com/li904338203/tabtv/backend/internal/media/repo"
)

func insertTile(t *testing.T, r *repo.Repo, next func() int64, ws, source int64,
	key string, idx int) int64 {
	t.Helper()
	meta, _ := json.Marshal(map[string]int{"row": (idx - 1) / 3, "col": (idx - 1) % 3})
	m := repo.MediaObject{
		ID: next(), WorkspaceID: ws, Bucket: "seeaitv",
		ObjectKey:   "tile/" + key + "/" + time.Now().Format("150405.000000000"),
		ContentType: "image/png", SizeBytes: 512, Purpose: "upload_reference",
		CreatedByUserID: 1, Width: 100, Height: 100,
		SourceMediaID: source, DeriveKind: app.DeriveGridTile,
		DeriveKey: key, DeriveIndex: idx, DeriveMeta: meta,
	}
	if err := r.Insert(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	return m.ID
}

// ⚠️ 重复点"切分"不该越切越多：同一张源图、同一种格数的同一格只该有一份。
func TestIntegration_同格重复切分被唯一索引挡住(t *testing.T) {
	r, _, next := setup(t)
	src := insert(t, r, next, wsA, "upload_reference")

	insertTile(t, r, next, wsA, src, "3x3", 1)
	if err := func() (err error) {
		defer func() { _ = recover() }()
		meta, _ := json.Marshal(map[string]int{})
		return r.Insert(context.Background(), repo.MediaObject{
			ID: next(), WorkspaceID: wsA, Bucket: "seeaitv", ObjectKey: "dup",
			ContentType: "image/png", SizeBytes: 1, Purpose: "upload_reference",
			CreatedByUserID: 1, SourceMediaID: src, DeriveKind: app.DeriveGridTile,
			DeriveKey: "3x3", DeriveIndex: 1, DeriveMeta: meta,
		})
	}(); err == nil {
		t.Fatal("同一格重复插入应被唯一索引拒绝")
	}
}

// ⚠️ 没有 derive_key 的话一张图一辈子只能切一种格数 ——
// 用户先切 2×2 再想切 3×3 会撞唯一索引，而那是个完全正当的操作。
func TestIntegration_不同格数可以共存(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	src := insert(t, r, next, wsA, "upload_reference")

	for i := 1; i <= 4; i++ {
		insertTile(t, r, next, wsA, src, "2x2", i)
	}
	for i := 1; i <= 9; i++ {
		insertTile(t, r, next, wsA, src, "3x3", i)
	}

	two, err := r.ListDerived(ctx, wsA, src, app.DeriveGridTile, "2x2")
	if err != nil || len(two) != 4 {
		t.Fatalf("2×2 应有 4 块，实际 %d（%v）", len(two), err)
	}
	three, err := r.ListDerived(ctx, wsA, src, app.DeriveGridTile, "3x3")
	if err != nil || len(three) != 9 {
		t.Fatalf("3×3 应有 9 块，实际 %d（%v）", len(three), err)
	}
	// 按编号升序：顺序错了用户对不上"第几格"。
	for i, m := range three {
		if m.DeriveIndex != i+1 {
			t.Fatalf("第 %d 项编号应为 %d，实际 %d", i, i+1, m.DeriveIndex)
		}
	}
}

// ⚠️ 派生列表也是一个"按 id 探测别人有什么"的入口。
func TestIntegration_派生列表按工作区隔离(t *testing.T) {
	r, _, next := setup(t)
	src := insert(t, r, next, wsA, "upload_reference")
	insertTile(t, r, next, wsA, src, "2x2", 1)

	got, err := r.ListDerived(context.Background(), wsB, src, app.DeriveGridTile, "2x2")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatal("别的工作区居然列到了切块")
	}
}

// ⚠️ 源图删了，切块仍然有效（NEW-16）：切块是一份独立的字节，
// 它的存在不依赖源图还在不在。
func TestIntegration_源图软删后切块仍可用(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	src := insert(t, r, next, wsA, "upload_reference")
	tile := insertTile(t, r, next, wsA, src, "2x2", 1)

	if err := r.SoftDelete(ctx, wsA, src); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetScoped(ctx, wsA, src); !errors.Is(err, repo.ErrNotFound) {
		t.Fatal("源图应已软删")
	}
	if _, err := r.GetScoped(ctx, wsA, tile); err != nil {
		t.Fatalf("切块应仍然可用: %v", err)
	}
}

// ---------------------------------------------------------------- 导出

func newExport(t *testing.T, r *repo.Repo, next func() int64, ws int64,
	expires time.Time) repo.ExportTask {
	t.Helper()
	items, _ := json.Marshal([]domain.ExportItem{{MediaID: 1, Name: "a"}})
	task := repo.ExportTask{
		ID: next(), WorkspaceID: ws, UserID: 1, Kind: "zip",
		Items: items, ItemCount: 1, ExpiresAt: expires,
	}
	payload, _ := json.Marshal(app.ExportPayload{ExportID: "x"})
	if err := r.InsertExport(context.Background(), task, next(), app.ExportTopic, payload); err != nil {
		t.Fatal(err)
	}
	return task
}

// ⚠️ 两步分开写的话，写完任务行就崩溃会留下一个永远排队的导出。
func TestIntegration_受理同时写任务与outbox(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	task := newExport(t, r, next, wsA, time.Now().Add(time.Hour))

	var n int
	if err := pool.QueryRow(ctx,
		`select count(*) from outbox_message where aggregate_type='export' and aggregate_id=$1`,
		task.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应写入 1 条 outbox 消息，实际 %d", n)
	}
	// ⚠️ 已有的投递器认领时**不按 aggregate_type 过滤** —— 这条断言就是
	// 「不需要第二个投递器」的依据。哪天有人给认领加了过滤，这里会红。
	var pending int
	if err := pool.QueryRow(ctx,
		`select count(*) from outbox_message where published_at is null`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("待投递消息应为 1 条，实际 %d", pending)
	}
}

// ⚠️ at-least-once 投递意味着同一条消息会重复到达。没有 CAS 的话
// 同一个导出会被打两遍包，两份都往对象存储里传。
func TestIntegration_并发领取只有一个成功(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	task := newExport(t, r, next, wsA, time.Now().Add(time.Hour))

	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, err := r.ClaimExport(ctx, task.ID); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("应只有一个协程领到，实际 %d —— 去掉 CAS 这里就会红", ok)
	}
}

// ⚠️ 带 status='running' 条件：晚到的结果不能覆盖已经被置失败的任务。
func TestIntegration_终态不被晚到结果覆盖(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	task := newExport(t, r, next, wsA, time.Now().Add(time.Hour))

	if _, err := r.ClaimExport(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.FailExport(ctx, task.ID, "INTERNAL", "上游炸了"); err != nil {
		t.Fatal(err)
	}
	if err := r.FinishExport(ctx, task.ID, 999, 100); !errors.Is(err, repo.ErrExportBusy) {
		t.Fatal("已判失败的任务不该被晚到的成功覆盖")
	}
	got, _ := r.GetExport(ctx, wsA, task.ID)
	if got.Status != domain.ExportFailed || got.ErrorMsg != "上游炸了" {
		t.Fatalf("应保持失败与原因，实际 %s / %s", got.Status, got.ErrorMsg)
	}
}

func TestIntegration_导出按工作区隔离(t *testing.T) {
	r, _, next := setup(t)
	task := newExport(t, r, next, wsA, time.Now().Add(time.Hour))
	if _, err := r.GetExport(context.Background(), wsB, task.ID); !errors.Is(err, repo.ErrNotFound) {
		t.Fatal("别的工作区居然取到了导出任务")
	}
}

// ⚠️ 清理的是**包**不是任务行：任务行留着，用户点开历史看到的是"已过期"
// 而不是"这次导出从没发生过"。
func TestIntegration_过期扫描只捞已出包且已过期的(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	now := time.Now()

	// ① 已过期且已出包 → 该捞
	expired := newExport(t, r, next, wsA, now.Add(-time.Hour))
	media := insert(t, r, next, wsA, "export_archive")
	if _, err := r.ClaimExport(ctx, expired.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.FinishExport(ctx, expired.ID, media, 4096); err != nil {
		t.Fatal(err)
	}
	// ② 已过期但还在排队 → 不该捞（它根本还没有包）
	newExport(t, r, next, wsA, now.Add(-time.Hour))
	// ③ 已出包但没过期 → 不该捞
	fresh := newExport(t, r, next, wsA, now.Add(time.Hour))
	freshMedia := insert(t, r, next, wsA, "export_archive")
	if _, err := r.ClaimExport(ctx, fresh.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.FinishExport(ctx, fresh.ID, freshMedia, 4096); err != nil {
		t.Fatal(err)
	}

	list, err := r.FindExpiredExports(ctx, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].TaskID != expired.ID {
		t.Fatalf("应只捞到 1 条已过期且已出包的，实际 %d 条 %+v", len(list), list)
	}

	// 清掉引用之后任务行还在，状态仍是 success —— 对外由 expires_at 算成 expired。
	if err := r.ClearExportMedia(ctx, expired.ID); err != nil {
		t.Fatal(err)
	}
	got, err := r.GetExport(ctx, wsA, expired.ID)
	if err != nil {
		t.Fatalf("任务行应保留: %v", err)
	}
	if got.MediaID != 0 {
		t.Fatal("引用应已清空")
	}
	if s := domain.EffectiveExportStatus(got.Status, got.ExpiresAt, now); s != domain.ExportExpired {
		t.Fatalf("对外应显示 expired，实际 %s", s)
	}
}

func TestIntegration_导出列表按工作区倒序翻页(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	var ids []int64
	for i := 0; i < 3; i++ {
		ids = append(ids, newExport(t, r, next, wsA, time.Now().Add(time.Hour)).ID)
	}
	newExport(t, r, next, wsB, time.Now().Add(time.Hour))

	page, err := r.ListExports(ctx, wsA, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || page[0].ID != ids[2] || page[1].ID != ids[1] {
		t.Fatalf("应按 id 倒序取两条，实际 %+v", page)
	}
	next2, err := r.ListExports(ctx, wsA, 2, page[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(next2) != 1 || next2[0].ID != ids[0] {
		t.Fatalf("游标翻页应取到最后一条，实际 %+v", next2)
	}
}
