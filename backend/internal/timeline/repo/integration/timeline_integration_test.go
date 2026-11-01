//go:build integration

// 时间线的真库测试（P10-01）。
//
// ⚠️ 必须用真库：两阶段重编号与乐观锁的正确性全在 SQL 里。
// PG 的唯一索引**不延迟检查** —— `update ... set idx = idx + 1` 当场就报重复，
// 而内存假实现完全看不到这一点（脚本域的镜号重排踩过同一个坑）。
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
	"github.com/li904338203/tabtv/backend/internal/timeline/domain"
	"github.com/li904338203/tabtv/backend/internal/timeline/repo"
)

const ws = int64(9001)

func setup(t *testing.T) (*repo.Repo, *pgxpool.Pool, func() int64) {
	t.Helper()
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	return repo.New(pool, gen.Next), pool, gen.Next
}

func newTimeline(t *testing.T, r *repo.Repo) repo.Timeline {
	t.Helper()
	tl, err := r.Create(context.Background(), repo.Timeline{
		WorkspaceID: ws, ProjectID: 777, Name: "测试", AspectRatio: "16:9",
		FPS: 30, Width: 1920, Height: 1080,
	})
	if err != nil {
		t.Fatal(err)
	}
	return tl
}

func videoTrack(t *testing.T, r *repo.Repo, timelineID int64) repo.Track {
	t.Helper()
	tracks, err := r.Tracks(context.Background(), timelineID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range tracks {
		if tr.Kind == domain.TrackVideo {
			return tr
		}
	}
	t.Fatal("没有画面轨")
	return repo.Track{}
}

// ⚠️ 分两次写的话，写完时间线就崩溃会留下一条没有任何轨道的时间线 ——
// 打开是空白，而用户不知道该点哪儿才能修好它。
func TestIntegration_建时间线同时铺好三条轨(t *testing.T) {
	r, _, _ := setup(t)
	tl := newTimeline(t, r)

	tracks, err := r.Tracks(context.Background(), tl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 3 {
		t.Fatalf("应有画面/音频/字幕三条轨，实际 %d", len(tracks))
	}
	kinds := map[string]bool{}
	for _, tr := range tracks {
		kinds[tr.Kind] = true
	}
	for _, k := range []string{domain.TrackVideo, domain.TrackAudio, domain.TrackSubtitle} {
		if !kinds[k] {
			t.Fatalf("缺少 %s 轨", k)
		}
	}
	if tl.Version != 1 {
		t.Fatalf("新建版本应为 1，实际 %d", tl.Version)
	}
}

func TestIntegration_按工作区隔离(t *testing.T) {
	r, _, _ := setup(t)
	tl := newTimeline(t, r)
	if _, err := r.Get(context.Background(), ws+1, tl.ID); !errors.Is(err, repo.ErrNotFound) {
		t.Fatal("别的工作区居然取到了时间线")
	}
}

// ⚠️ 这是这一层最关键的一条：`uk_clip_idx(track_id, idx)` 是普通唯一索引，
// PG **不延迟检查**。不做两阶段的话，把第 3 段移到最前就会当场报重复。
func TestIntegration_整体重排不撞唯一索引(t *testing.T) {
	r, _, _ := setup(t)
	ctx := context.Background()
	tl := newTimeline(t, r)
	track := videoTrack(t, r, tl.ID)

	clips := []domain.Clip{
		{Idx: 1, MediaID: 101, SourceOutMs: 1000, StartMs: 0, DurationMs: 1000, Speed: 100, Volume: 100},
		{Idx: 2, MediaID: 102, SourceOutMs: 2000, StartMs: 1000, DurationMs: 2000, Speed: 100, Volume: 100},
		{Idx: 3, MediaID: 103, SourceOutMs: 3000, StartMs: 3000, DurationMs: 3000, Speed: 100, Volume: 100},
	}
	v, err := r.SaveClips(ctx, tl.ID, track.ID, 1, repo.Editor{}, clips)
	if err != nil {
		t.Fatal(err)
	}

	saved, err := r.Clips(ctx, track.ID)
	if err != nil || len(saved) != 3 {
		t.Fatalf("应写入 3 段，实际 %d（%v）", len(saved), err)
	}

	// 把第 3 段挪到最前 —— 这一步正是会撞唯一索引的地方。
	moved, err := domain.Reorder(saved, saved[2].ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.SaveClips(ctx, tl.ID, track.ID, v, repo.Editor{}, moved); err != nil {
		t.Fatalf("整体重排应当成功（两阶段）：%v", err)
	}
	after, _ := r.Clips(ctx, track.ID)
	if after[0].MediaID != 103 {
		t.Fatalf("第 3 段应到最前，实际 media=%d", after[0].MediaID)
	}
	// 号还是 1..N 连续。
	for i, c := range after {
		if c.Idx != i+1 {
			t.Fatalf("第 %d 段的号应为 %d，实际 %d", i, i+1, c.Idx)
		}
	}
}

// ⚠️ 上一次失败可能已经在负数区留了行。临时区要**低于所有现存值**，
// 简单取负会与它们撞上，而撞不撞取决于执行计划 —— 表现为时有时无的失败。
func TestIntegration_临时区低于所有现存值(t *testing.T) {
	r, pool, _ := setup(t)
	ctx := context.Background()
	tl := newTimeline(t, r)
	track := videoTrack(t, r, tl.ID)

	clips := []domain.Clip{
		{Idx: 1, MediaID: 101, SourceOutMs: 1000, DurationMs: 1000, Speed: 100, Volume: 100},
		{Idx: 2, MediaID: 102, SourceOutMs: 2000, DurationMs: 2000, Speed: 100, Volume: 100},
	}
	v, err := r.SaveClips(ctx, tl.ID, track.ID, 1, repo.Editor{}, clips)
	if err != nil {
		t.Fatal(err)
	}
	// 人为在负数区留一行（模拟上一次失败的残留）。
	if _, err := pool.Exec(ctx,
		`update timeline_clip set idx = -1 where track_id = $1 and idx = 2`,
		track.ID); err != nil {
		t.Fatal(err)
	}

	saved, _ := r.Clips(ctx, track.ID)
	reordered, err := domain.Reorder(saved, saved[len(saved)-1].ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.SaveClips(ctx, tl.ID, track.ID, v, repo.Editor{}, reordered); err != nil {
		t.Fatalf("临时区里已有 -1 时重排仍应成功：%v", err)
	}
}

// ⚠️ 0 行更新返回冲突，**不能静默当成功** —— 静默的话两个人同时编辑，
// 后保存的那份把前一份悄悄覆盖掉（与画布保存同一条）。
func TestIntegration_乐观锁挡住过期的写入(t *testing.T) {
	r, _, _ := setup(t)
	ctx := context.Background()
	tl := newTimeline(t, r)
	track := videoTrack(t, r, tl.ID)

	clips := []domain.Clip{
		{Idx: 1, MediaID: 101, SourceOutMs: 1000, DurationMs: 1000, Speed: 100, Volume: 100},
	}
	if _, err := r.SaveClips(ctx, tl.ID, track.ID, 1, repo.Editor{}, clips); err != nil {
		t.Fatal(err)
	}
	// 拿着旧版本再写一次：应当冲突。
	if _, err := r.SaveClips(ctx, tl.ID, track.ID, 1, repo.Editor{}, clips); !errors.Is(err, repo.ErrVersionConflict) {
		t.Fatalf("过期版本应冲突，实际 %v", err)
	}
}

// ⚠️ 顺序调用的测试会先撞上别的判断，把乐观锁整条删掉照样绿。
// 这条用真并发：只有一个协程能写进去。
func TestIntegration_并发保存只有一个成功(t *testing.T) {
	r, _, _ := setup(t)
	ctx := context.Background()
	tl := newTimeline(t, r)
	track := videoTrack(t, r, tl.ID)

	clips := []domain.Clip{
		{Idx: 1, MediaID: 101, SourceOutMs: 1000, DurationMs: 1000, Speed: 100, Volume: 100},
	}
	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, err := r.SaveClips(ctx, tl.ID, track.ID, 1, repo.Editor{}, clips); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("应只有一个协程写进去，实际 %d —— 去掉乐观锁这里就会红", ok)
	}
	got, _ := r.Get(ctx, ws, tl.ID)
	if got.Version != 2 {
		t.Fatalf("版本只该进一位，实际 %d", got.Version)
	}
}

// 不在清单里的段要删掉；这是"用户删了那一段"的落地方式。
func TestIntegration_清单之外的段被删掉(t *testing.T) {
	r, _, _ := setup(t)
	ctx := context.Background()
	tl := newTimeline(t, r)
	track := videoTrack(t, r, tl.ID)

	v, err := r.SaveClips(ctx, tl.ID, track.ID, 1, repo.Editor{}, []domain.Clip{
		{Idx: 1, MediaID: 101, SourceOutMs: 1000, DurationMs: 1000, Speed: 100, Volume: 100},
		{Idx: 2, MediaID: 102, SourceOutMs: 2000, DurationMs: 2000, Speed: 100, Volume: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	saved, _ := r.Clips(ctx, track.ID)
	if _, err := r.SaveClips(ctx, tl.ID, track.ID, v, repo.Editor{}, saved[:1]); err != nil {
		t.Fatal(err)
	}
	after, _ := r.Clips(ctx, track.ID)
	if len(after) != 1 || after[0].MediaID != 101 {
		t.Fatalf("应只剩第一段，实际 %+v", after)
	}
}

// ⚠️ 同一条轨道里，一镜最多只该有一段自动装配出来的片段 ——
// 否则重复点"装配"会越装越多。
func TestIntegration_自动装配的溯源唯一(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	tl := newTimeline(t, r)
	track := videoTrack(t, r, tl.ID)

	if _, err := r.SaveClips(ctx, tl.ID, track.ID, 1, repo.Editor{}, []domain.Clip{
		{Idx: 1, MediaID: 101, SourceOutMs: 1000, DurationMs: 1000,
			Speed: 100, Volume: 100, SourceShotID: 555},
	}); err != nil {
		t.Fatal(err)
	}
	// 绕过服务层直接插一条同镜的：应被唯一索引挡住。
	_, err := pool.Exec(ctx, `
		insert into timeline_clip (id, track_id, idx, media_object_id,
			source_in_ms, source_out_ms, start_ms, duration_ms, source_shot_id)
		values ($1,$2,99,102,0,1000,0,1000,555)`, next(), track.ID)
	if err == nil {
		t.Fatal("同一镜的自动装配片段应唯一")
	}

	// ⚠️ 手工加的段（source_shot_id 为 null）不受这条约束 ——
	// PG 的部分索引不覆盖 null 行。这是有意的：约束管的是自动装配，
	// 不是"用户不能把同一镜用两次"。
	if _, err := pool.Exec(ctx, `
		insert into timeline_clip (id, track_id, idx, media_object_id,
			source_in_ms, source_out_ms, start_ms, duration_ms)
		values ($1,$2,98,102,0,1000,0,1000)`, next(), track.ID); err != nil {
		t.Fatalf("手工段不该被挡：%v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into timeline_clip (id, track_id, idx, media_object_id,
			source_in_ms, source_out_ms, start_ms, duration_ms)
		values ($1,$2,97,103,0,1000,0,1000)`, next(), track.ID); err != nil {
		t.Fatalf("第二条手工段也不该被挡：%v", err)
	}
}

func TestIntegration_字幕整批替换(t *testing.T) {
	r, pool, _ := setup(t)
	ctx := context.Background()
	tl := newTimeline(t, r)

	v, err := r.SaveSubtitles(ctx, tl.ID, 1, repo.Editor{}, []repo.Subtitle{
		{StartMs: 0, EndMs: 2000, Text: "第一句"},
		{StartMs: 2000, EndMs: 4000, Text: "第二句"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := r.Subtitles(ctx, tl.ID)
	if len(got) != 2 || got[0].Text != "第一句" {
		t.Fatalf("应有两条字幕：%+v", got)
	}
	// 再存一批：整批替换，旧的不留。
	if _, err := r.SaveSubtitles(ctx, tl.ID, v, repo.Editor{}, []repo.Subtitle{
		{StartMs: 0, EndMs: 1000, Text: "改过了"},
	}); err != nil {
		t.Fatal(err)
	}
	got2, _ := r.Subtitles(ctx, tl.ID)
	if len(got2) != 1 || got2[0].Text != "改过了" {
		t.Fatalf("整批替换后应只剩一条：%+v", got2)
	}
	// 00067 起字幕必须归属字幕轨（SC-F-52）：这条旧接口写进默认字幕轨（idx 最小的那条）。
	var onDefault int
	if err := pool.QueryRow(ctx, `select count(*) from timeline_subtitle s
		where s.timeline_id = $1 and s.track_id = (select k.id from timeline_track k
			where k.timeline_id = $1 and k.kind = 'subtitle' order by k.idx limit 1)`, tl.ID).Scan(&onDefault); err != nil {
		t.Fatal(err)
	}
	if onDefault != 1 {
		t.Fatalf("保存的字幕应挂在默认字幕轨上，实际 %d 条", onDefault)
	}
}

func TestIntegration_字幕也走乐观锁(t *testing.T) {
	r, _, _ := setup(t)
	ctx := context.Background()
	tl := newTimeline(t, r)
	if _, err := r.SaveSubtitles(ctx, tl.ID, 1, repo.Editor{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SaveSubtitles(ctx, tl.ID, 1, repo.Editor{}, nil); !errors.Is(err, repo.ErrVersionConflict) {
		t.Fatalf("过期版本应冲突，实际 %v", err)
	}
}

// ⚠️ 转场存在 jsonb 列里，存进去与读出来必须是同一件事 ——
// 存了读不回来的话，用户设完转场刷新一下就没了，而保存是"成功"的。
func TestIntegration_转场存得进读得出(t *testing.T) {
	r, _, _ := setup(t)
	ctx := context.Background()
	tl := newTimeline(t, r)
	track := videoTrack(t, r, tl.ID)

	v, err := r.SaveClips(ctx, tl.ID, track.ID, 1, repo.Editor{}, []domain.Clip{
		{Idx: 1, MediaID: 101, SourceOutMs: 4000, DurationMs: 4000, Speed: 100, Volume: 100},
		{Idx: 2, MediaID: 102, SourceOutMs: 4000, DurationMs: 4000, Speed: 100, Volume: 100,
			TransitionIn: domain.Transition{Kind: domain.TransitionFade, DurationMs: 500}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Clips(ctx, track.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 没设转场的那一段读出来是"没有转场"，不是一个空壳对象。
	if got[0].TransitionIn.IsActive() {
		t.Fatalf("第一段不该有转场：%+v", got[0].TransitionIn)
	}
	if got[1].TransitionIn.Kind != domain.TransitionFade ||
		got[1].TransitionIn.DurationMs != 500 {
		t.Fatalf("转场没读回来：%+v", got[1].TransitionIn)
	}

	// 改成"无"之后要真的清掉，不能留着旧值。
	got[1].TransitionIn = domain.Transition{}
	if _, err := r.SaveClips(ctx, tl.ID, track.ID, v, repo.Editor{}, got); err != nil {
		t.Fatal(err)
	}
	after, _ := r.Clips(ctx, track.ID)
	if after[1].TransitionIn.IsActive() {
		t.Fatalf("清掉转场后不该还在：%+v", after[1].TransitionIn)
	}
}

// ⚠️ jsonb 列里可能留着历史数据或手工改库的脏值。
// "有一段的转场字段坏了"不该让整条时间线打不开。
func TestIntegration_坏的转场值不影响读取(t *testing.T) {
	r, pool, _ := setup(t)
	ctx := context.Background()
	tl := newTimeline(t, r)
	track := videoTrack(t, r, tl.ID)

	if _, err := r.SaveClips(ctx, tl.ID, track.ID, 1, repo.Editor{}, []domain.Clip{
		{Idx: 1, MediaID: 101, SourceOutMs: 4000, DurationMs: 4000, Speed: 100, Volume: 100},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`update timeline_clip set transition_in = '{"kind":"explode"}'::jsonb
		 where track_id = $1`, track.ID); err != nil {
		t.Fatal(err)
	}
	got, err := r.Clips(ctx, track.ID)
	if err != nil {
		t.Fatalf("坏值不该让读取失败：%v", err)
	}
	if got[0].TransitionIn.IsActive() {
		t.Fatalf("坏值应当被当成「没有转场」：%+v", got[0].TransitionIn)
	}
}

// ---------------------------------------------------------------- 渲染

func newRender(t *testing.T, r *repo.Repo, pool *pgxpool.Pool, next func() int64,
	timelineID int64, cost int) repo.RenderJob {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	deadline := time.Now().Add(time.Hour)
	job := repo.RenderJob{
		ID: next(), WorkspaceID: ws, TimelineID: timelineID, TimelineVersion: 1,
		Snapshot: json.RawMessage(`{"clips":[]}`), PayerAccountID: 4242,
		CreatedByUserID: 1, CreditsCost: cost, DeadlineAt: &deadline,
	}
	if err := r.InsertRenderInTx(ctx, tx, job, next(), "render.requested",
		json.RawMessage(`{"renderId":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return job
}

// ⚠️ at-least-once 投递意味着同一条消息会重复到达。没有 CAS 的话
// 同一次渲染会跑两遍 —— 两个 ffmpeg 同时啃同一台机器的 CPU，
// 而只有一个的结果会被采信。
func TestIntegration_并发领取渲染只有一个成功(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	tl := newTimeline(t, r)
	job := newRender(t, r, pool, next, tl.ID, 4)

	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, err := r.ClaimRender(ctx, job.ID, "ffmpeg-test",
				time.Now().Add(time.Hour), 0); err == nil {
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

// ⚠️ 受理、扣费、写 outbox 必须同一个事务：拆开的话中间崩溃会留下
// 「扣了钱没有任务」或「有任务没扣钱」，而这两种都只能靠对账人工修。
func TestIntegration_渲染受理同时写任务与outbox(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	tl := newTimeline(t, r)
	job := newRender(t, r, pool, next, tl.ID, 4)

	var n int
	if err := pool.QueryRow(ctx,
		`select count(*) from outbox_message where aggregate_type='render' and aggregate_id=$1`,
		job.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应写入 1 条 outbox 消息，实际 %d", n)
	}
}

// ⚠️ 退款只能成功一次：`FailRender` / `CancelRender` 返回
// "是不是本次真的改了状态"，调用方据此决定退不退。
// 不看这个返回值的话，重复消息会退两次钱。
func TestIntegration_失败与取消只认第一次(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	tl := newTimeline(t, r)
	job := newRender(t, r, pool, next, tl.ID, 4)

	if _, err := r.ClaimRender(ctx, job.ID, "v", time.Now().Add(time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	changed, err := r.FailRender(ctx, job.ID, "render_failed", "炸了")
	if err != nil || !changed {
		t.Fatalf("第一次应当改成失败：%v %v", changed, err)
	}
	changed2, err := r.FailRender(ctx, job.ID, "render_failed", "又炸了")
	if err != nil {
		t.Fatal(err)
	}
	if changed2 {
		t.Fatal("第二次不该再改 —— 不看这个返回值就会退两次钱")
	}
	// 已是终态的任务取消不了：要明确告诉用户"已经跑完了"。
	cancelled, err := r.CancelRender(ctx, ws, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled {
		t.Fatal("终态任务不该能取消")
	}
}

// ⚠️ 晚到的进度不能把已取消的任务拉回 running —— 否则用户按了取消
// 却看着进度条继续走。
func TestIntegration_晚到的进度不复活已取消的任务(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	tl := newTimeline(t, r)
	job := newRender(t, r, pool, next, tl.ID, 4)

	if _, err := r.ClaimRender(ctx, job.ID, "v", time.Now().Add(time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	if ok, err := r.CancelRender(ctx, ws, job.ID); err != nil || !ok {
		t.Fatalf("应当取消成功：%v %v", ok, err)
	}
	if err := r.RenderProgress(ctx, job.ID, "encoding", 80); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Render(ctx, ws, job.ID)
	if got.Status != "cancelled" || got.Progress == 80 {
		t.Fatalf("取消之后不该被进度拉回：%+v", got)
	}
	// 晚到的成功同样不能覆盖。
	if err := r.FinishRender(ctx, job.ID, 999, nil); !errors.Is(err, repo.ErrRenderBusy) {
		t.Fatalf("已取消的任务不该被标成功，实际 %v", err)
	}
}

// ⚠️ ffmpeg 可能卡死（畸形输入、磁盘满），而卡着的任务一直占着并发额度 ——
// 后面排队的所有渲染都跟着不动，而它们各自看都是"排队中"。
func TestIntegration_超时渲染能被扫出来(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	tl := newTimeline(t, r)
	job := newRender(t, r, pool, next, tl.ID, 4)

	if _, err := pool.Exec(ctx,
		`update render_job set deadline_at = now() - interval '1 hour' where id=$1`,
		job.ID); err != nil {
		t.Fatal(err)
	}
	list, err := r.StaleRenders(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != job.ID {
		t.Fatalf("应扫出这一条，实际 %+v", list)
	}
	// 已经结束的任务不该被扫出来。
	if _, err := r.FailRender(ctx, job.ID, "x", "y"); err != nil {
		t.Fatal(err)
	}
	after, _ := r.StaleRenders(ctx, 10)
	if len(after) != 0 {
		t.Fatalf("终态任务不该被扫出来：%+v", after)
	}
}

// ⚠️ 并发上限**读库不读进程内计数**：Worker 多实例时进程内各算各的，
// 合起来照样超 —— 而渲染是 CPU 密集的，超了会把整台机器拖垮。
func TestIntegration_运行中的渲染数从库里读(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	tl := newTimeline(t, r)
	a := newRender(t, r, pool, next, tl.ID, 1)
	newRender(t, r, pool, next, tl.ID, 1)

	if n, _ := r.RunningRenders(ctx); n != 0 {
		t.Fatalf("还没领取时应为 0，实际 %d", n)
	}
	if _, err := r.ClaimRender(ctx, a.ID, "v", time.Now().Add(time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	if n, _ := r.RunningRenders(ctx); n != 1 {
		t.Fatalf("领取一条后应为 1，实际 %d", n)
	}
}

// ------------------------------------------------- 水印与成片入库（P10-04）

// ⚠️ 单行配置表：保存两次要**更新同一行**而不是再插一行。
// 插两行的话 `order by id limit 1` 拿到的是旧那条 —— 运营改了配置、
// 渲染出来的片子还是老水印，而后台显示的是新值。
func TestIntegration_水印配置是单行且版本进位(t *testing.T) {
	r, pool, _ := setup(t)
	ctx := context.Background()

	// 没配过时回零值（＝不加水印），**不是错误**。
	got, err := r.RenderSetting(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.WatermarkMediaID != 0 || got.Version != 0 {
		t.Fatalf("没配过应回零值：%+v", got)
	}

	if err := r.SaveRenderSetting(ctx, repo.RenderSetting{
		WatermarkMediaID: 111, Position: domain.WatermarkBottomRight,
		Opacity: 60, Scale: 12,
	}, 7); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveRenderSetting(ctx, repo.RenderSetting{
		WatermarkMediaID: 222, Position: domain.WatermarkTopLeft,
		Opacity: 30, Scale: 20,
	}, 7); err != nil {
		t.Fatal(err)
	}
	// ⚠️ 明确断言**只有一行**。这条以前漏了：`SaveRenderSetting` 每次现取
	// 雪花 ID，`on conflict (id)` 永远不命中，保存一次就多一行；
	// 而读取用 `order by id limit 1` 拿到的是**最早**那行 ——
	// 症状是"后台改了配置、保存成功了、渲染出来还是老水印"，
	// 而配置页回显的是新值。当时的测试两次传同一个 id，恰好把它掩盖了。
	var rows int
	if err := pool.QueryRow(ctx, `select count(*) from render_setting`).
		Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("单行配置表应只有一行，实际 %d 行", rows)
	}
	got, err = r.RenderSetting(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.WatermarkMediaID != 222 || got.Position != domain.WatermarkTopLeft {
		t.Fatalf("应更新同一行：%+v", got)
	}
	if got.Version != 2 {
		t.Fatalf("版本应进位到 2，得到 %d", got.Version)
	}
}

// ⚠️ 成片进作品库要**幂等**：重复消息导致的重渲不能产生第二件作品。
// 靠 `uk_work_source(source_type, source_id)`，不靠"先查再插"。
func TestIntegration_成片入库幂等且落待审(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	tl := newTimeline(t, r)
	job := newRender(t, r, pool, next, tl.ID, 10)

	if err := r.InsertWork(ctx, next(), ws, job.ID, tl.ID, 555); err != nil {
		t.Fatal(err)
	}
	// 再来一次（重投）——不能多出一件作品。
	if err := r.InsertWork(ctx, next(), ws, job.ID, tl.ID, 666); err != nil {
		t.Fatal(err)
	}
	var n int
	var status, title string
	var featured, offline bool
	var mediaID int64
	if err := pool.QueryRow(ctx, `select count(*) from work
		where source_type='render' and source_id=$1`, job.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("重投不该多出作品，实际 %d 件", n)
	}
	if err := pool.QueryRow(ctx, `select review_status, coalesce(title,''),
		is_featured, is_offline, coalesce(media_object_id,0) from work
		where source_type='render' and source_id=$1`, job.ID).
		Scan(&status, &title, &featured, &offline, &mediaID); err != nil {
		t.Fatal(err)
	}
	// ⚠️ **进作品库不等于公开发布**：渲完就上首页的话，
	// 任何人渲一个片子都上了公开墙。
	if status != "pending" || featured || offline {
		t.Fatalf("应落待审且未加精：status=%s featured=%v offline=%v",
			status, featured, offline)
	}
	if title != tl.Name {
		t.Fatalf("标题应取时间线名，得到 %q", title)
	}
	// 重投时产出以最后一次为准（重渲产生了新的成片）。
	if mediaID != 666 {
		t.Fatalf("产出应更新到最后一次，得到 %d", mediaID)
	}
}

// ⚠️ `work_id` 用标量子查询而不是 join：内连接会让还没登记作品的任务
// （排队中、失败的）整行消失 —— 症状是"列表里刚提交的那条不见了"，
// 而任务其实在正常跑。
func TestIntegration_没登记作品的渲染照常查得到(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	tl := newTimeline(t, r)
	job := newRender(t, r, pool, next, tl.ID, 10)

	got, err := r.Render(ctx, ws, job.ID)
	if err != nil {
		t.Fatalf("没登记作品的任务应查得到：%v", err)
	}
	if got.WorkID != 0 {
		t.Fatalf("还没登记时 workId 应为 0，得到 %d", got.WorkID)
	}
	list, err := r.ListRenders(ctx, ws, tl.ID, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("列表里应有 1 条：%v %d", err, len(list))
	}

	if err := r.InsertWork(ctx, next(), ws, job.ID, tl.ID, 555); err != nil {
		t.Fatal(err)
	}
	got, err = r.Render(ctx, ws, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkID == 0 {
		t.Fatal("登记后应带出 workId")
	}
}

// ⚠️ "这次到底加没加水印"要记在任务上：用户可能自己关了、平台可能还没配图、
// 图也可能取不回来。只看配置的话事后三种分不出来，而处理完全不同。
func TestIntegration_水印是否加上记在任务上(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	tl := newTimeline(t, r)
	job := newRender(t, r, pool, next, tl.ID, 10)

	got, err := r.Render(ctx, ws, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.WatermarkApplied {
		t.Fatal("默认应为未加")
	}
	if err := r.MarkWatermarked(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	got, err = r.Render(ctx, ws, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.WatermarkApplied {
		t.Fatal("标记后应为已加")
	}
}

// ⚠️ 后台追溯要同时给**快照里用到的素材**与**当前时间线** ——
// 只看当前时间线会得出完全错误的结论（用户很可能渲完又改过）。
func TestIntegration_后台追溯给出快照素材与作品(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	tl := newTimeline(t, r)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Hour)
	job := repo.RenderJob{
		ID: next(), WorkspaceID: ws, TimelineID: tl.ID, TimelineVersion: 1,
		Snapshot: json.RawMessage(`{"clips":[{"mediaId":11},{"mediaId":12},
			{"mediaId":11}],"audio":[{"mediaId":21}],
			"watermark":{"mediaId":31}}`),
		PayerAccountID: 4242, CreatedByUserID: 1, CreditsCost: 10,
		DeadlineAt: &deadline,
	}
	if err := r.InsertRenderInTx(ctx, tx, job, next(), "render.requested",
		json.RawMessage(`{"renderId":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.InsertWork(ctx, next(), ws, job.ID, tl.ID, 999); err != nil {
		t.Fatal(err)
	}

	tr, err := r.TraceRender(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 三类素材都要出现，且**去重**（同一段素材被用两次不该列两遍）。
	want := map[int64]bool{11: true, 12: true, 21: true, 31: true}
	if len(tr.MediaIDs) != len(want) {
		t.Fatalf("应去重后列出 4 个素材，得到 %v", tr.MediaIDs)
	}
	for _, id := range tr.MediaIDs {
		if !want[id] {
			t.Fatalf("不该出现的素材 %d：%v", id, tr.MediaIDs)
		}
	}
	if tr.Timeline.ID != tl.ID || tr.Timeline.Name != tl.Name {
		t.Fatalf("应带出当前时间线：%+v", tr.Timeline)
	}
	if tr.WorkID == 0 {
		t.Fatal("应带出成片作品")
	}
}
