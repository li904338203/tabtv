//go:build integration

// 媒体生命周期的真库测试。
//
// ⚠️ 必须用真库：归属过滤、活动引用保护、回收判据都写在 SQL 里。
// P03-04 的 `Open(ctx, id)` 把归属校验写成注释交给调用方，而 handler 并没有做 ——
// 这类"注释里的约定"只有真库测试能钉住。
package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	"github.com/li904338203/tabtv/backend/internal/media/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
)

const (
	wsA = int64(7001)
	wsB = int64(7002)
)

func setup(t *testing.T) (*repo.Repo, *pgxpool.Pool, func() int64) {
	t.Helper()
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	return repo.New(pool), pool, gen.Next
}

func insert(t *testing.T, r *repo.Repo, next func() int64, ws int64, purpose string) int64 {
	t.Helper()
	m := repo.MediaObject{
		ID: next(), WorkspaceID: ws, Bucket: "seeaitv",
		ObjectKey:   purpose + "/" + time.Now().Format("150405.000000000"),
		ContentType: "image/png", SizeBytes: 1024, Checksum: "abc",
		Purpose: purpose, CreatedByUserID: 1,
	}
	if err := r.Insert(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	return m.ID
}

// 归属条件落在 SQL 里：查不到就是没有，不存在"忘了比对"这种可能。
func TestGetScoped_跨工作区取不到(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	mine := insert(t, r, next, wsA, "upload_reference")

	if _, err := r.GetScoped(ctx, wsA, mine); err != nil {
		t.Fatalf("本工作区应能取到: %v", err)
	}
	if _, err := r.GetScoped(ctx, wsB, mine); !errors.Is(err, repo.ErrNotFound) {
		t.Fatal("别的工作区居然取到了 —— 这正是 P03-04 留下的越权口子")
	}

	// 平台级素材（workspace_id = 0）登录即可读：封面、试听本来就是公共的。
	platform := insert(t, r, next, 0, "material_cover")
	if _, err := r.GetScoped(ctx, wsB, platform); err != nil {
		t.Errorf("平台级素材应可读: %v", err)
	}
}

// 管理端读取（`GET /admin/media/{id}`）固定传工作区 0。它的全部安全性就压在这一条上：
// 归属条件是「本工作区 或 工作区 0」，所以传 0 只取得到平台级素材 ——
// 哪天有人把条件改成「工作区 0 表示不限」，后台就能按 ID 翻遍所有用户的私有产物。
func TestGetScoped_工作区0只取得到平台级素材(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	private := insert(t, r, next, wsA, "generation_output")
	platform := insert(t, r, next, 0, "banner")

	if _, err := r.GetScoped(ctx, 0, private); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("工作区 0 居然取到了用户的私有产物（err=%v）—— 管理端读取会变成越权口子", err)
	}
	if _, err := r.GetScoped(ctx, 0, platform); err != nil {
		t.Fatalf("工作区 0 应能取到平台级素材: %v", err)
	}
}

// 删掉一个正被进行中任务引用的文件，任务会在 Worker 那侧取不到输入而失败，
// 而用户以为自己只是清理了一张图。
func TestSoftDelete_活动引用保护(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	mid := insert(t, r, next, wsA, "upload_reference")

	if err := r.AddUsage(ctx, next(), mid, "generation_input", 555); err != nil {
		t.Fatal(err)
	}
	if err := r.SoftDelete(ctx, wsA, mid); !errors.Is(err, repo.ErrInUse) {
		t.Fatalf("有活动引用时应拒绝删除，实际 %v", err)
	}

	// 引用解除后可以删。
	if n, err := r.ReleaseUsage(ctx, "generation_input", 555); err != nil || n != 1 {
		t.Fatalf("解除引用应影响 1 行: n=%d err=%v", n, err)
	}
	if err := r.SoftDelete(ctx, wsA, mid); err != nil {
		t.Fatalf("无引用后应可删除: %v", err)
	}
	// 软删后读不到（GetScoped 只看 active）。
	if _, err := r.GetScoped(ctx, wsA, mid); !errors.Is(err, repo.ErrNotFound) {
		t.Error("软删后不该再读到")
	}
}

// 重试、重复保存都会走到登记引用这一步，报错只会把调用方逼着去 catch。
func TestAddUsage_幂等(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	mid := insert(t, r, next, wsA, "upload_reference")

	for i := 0; i < 3; i++ {
		if err := r.AddUsage(ctx, next(), mid, "project_cover", 99); err != nil {
			t.Fatalf("第 %d 次登记应幂等: %v", i+1, err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx,
		`select count(*) from media_usage where media_object_id=$1`, mid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应只有 1 条引用，实际 %d", n)
	}
}

// 回收判据是**两个条件都要**：无引用 且 超保留期。
func TestFindCollectable_判据(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	before := time.Now().Add(-7 * 24 * time.Hour)

	// ① 刚软删、未过期 → 不收（用户可能反悔，保留期就是给他的窗口）
	fresh := insert(t, r, next, wsA, "upload_reference")
	_ = r.SoftDelete(ctx, wsA, fresh)

	// ② 软删且已过期、无引用 → 收
	stale := insert(t, r, next, wsA, "upload_reference")
	_ = r.SoftDelete(ctx, wsA, stale)
	if _, err := pool.Exec(ctx,
		`update media_object set deleted_at = now() - interval '30 days' where id=$1`, stale); err != nil {
		t.Fatal(err)
	}

	// ③ 软删已过期但**仍有引用** → 不收（引用还在就还有人要用）
	used := insert(t, r, next, wsA, "upload_reference")
	if _, err := pool.Exec(ctx, `
		update media_object set status='deleted', deleted_at = now() - interval '30 days'
		where id=$1`, used); err != nil {
		t.Fatal(err)
	}
	if err := r.AddUsage(ctx, next(), used, "timeline_clip", 7); err != nil {
		t.Fatal(err)
	}

	// ④ 用户没删、但很旧且没有 usage 行的上传 → **不收**。
	//
	// ⚠️ 这一条此前是反的（"传了没用也该按同样期限回收"），而它删的正是
	// 用户素材库里还看得见的文件 —— `/assets` 列的就是
	// `status='active' and purpose='upload_reference'`。
	// 叠加上"八个 UsageType 只有两个真的有人登记"，「没有 usage 行」对
	// 几乎每一个素材都成立，于是素材库会在七天后自己清空，**而没有任何报错**。
	kept := insert(t, r, next, wsA, "upload_reference")
	if _, err := pool.Exec(ctx,
		`update media_object set created_at = now() - interval '30 days' where id=$1`, kept); err != nil {
		t.Fatal(err)
	}

	// ⑤ 生成产物即使旧、也不收：它属于作品，删除要走别的路径
	output := insert(t, r, next, wsA, "generation_output")
	if _, err := pool.Exec(ctx,
		`update media_object set created_at = now() - interval '30 days' where id=$1`, output); err != nil {
		t.Fatal(err)
	}

	list, err := r.FindCollectable(ctx, before, 100)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]bool{}
	for _, m := range list {
		got[m.ID] = true
	}
	if !got[stale] {
		t.Error("软删已过期且无引用的应被回收")
	}
	if got[fresh] {
		t.Error("刚软删未过期的不该被回收 —— 用户可能反悔")
	}
	if got[used] {
		t.Error("仍有引用的不该被回收")
	}
	if got[kept] {
		t.Error("用户没删的素材绝不能被自动回收 —— " +
			"这一条删的是他素材库里还看得见的文件，而且不会有任何报错")
	}
	if got[output] {
		t.Error("生成产物不该被自动回收")
	}
}

// 回收只认「用户删过」这一个前提。
//
// ⚠️ 单独钉一条是因为上面那个用例里 `kept` 只是众多断言之一，
// 而这一条是整个回收策略的地基：任何时候有人想"顺带也收一下 active 的"，
// 这条会立刻红。留存期是 PARAM-05 未定稿，默认只能是「不删用户看得见的东西」。
func TestFindCollectable_从不回收用户没删过的媒体(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()

	for _, purpose := range []string{
		"upload_reference", "generation_output", "render_output",
		"script_asset", "material_cover", "avatar", "export_archive",
	} {
		id := insert(t, r, next, wsA, purpose)
		if _, err := pool.Exec(ctx, `
			update media_object set created_at = now() - interval '3650 days'
			where id=$1`, id); err != nil {
			t.Fatal(err)
		}
	}

	// 用一个极宽的窗口（十年前的都算"过期"）去扫：一条都不该被收。
	list, err := r.FindCollectable(ctx, time.Now(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("没有一条是用户删过的，却收上来 %d 条", len(list))
	}
}

func TestDeleteRow(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	mid := insert(t, r, next, wsA, "upload_reference")
	if err := r.DeleteRow(ctx, mid); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = pool.QueryRow(ctx, `select count(*) from media_object where id=$1`, mid).Scan(&n)
	if n != 0 {
		t.Fatal("行应已删除")
	}
}

// 视频的封面帧（design/03 R-5）：只认本工作区、未删除的 video_poster 派生件；还没截出来不算错。
// 前端靠它不下载整段视频就能显示画面、按真实比例摆卡片。
func TestPosterOf_本工作区未删除的封面帧(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	svc := mediaapp.New(r, nil, next)
	video := insert(t, r, next, wsA, "generation_output")

	if _, ok, err := svc.PosterOf(ctx, wsA, video); err != nil || ok {
		t.Fatalf("还没截封面帧时应是 ok=false 且不报错：ok=%v err=%v", ok, err)
	}
	poster := repo.MediaObject{
		ID: next(), WorkspaceID: wsA, Bucket: "seeaitv", ObjectKey: "generation_output/poster-" + time.Now().Format("150405.000000000"),
		ContentType: "image/jpeg", SizeBytes: 100, Checksum: "p", Purpose: "generation_output", CreatedByUserID: 1,
		Width: 480, Height: 854, SourceMediaID: video, DeriveKind: mediaapp.DerivePoster, DeriveIndex: 1,
	}
	if err := r.Insert(ctx, poster); err != nil {
		t.Fatal(err)
	}
	p, ok, err := svc.PosterOf(ctx, wsA, video)
	if err != nil || !ok || p.ID != poster.ID || p.Width != 480 || p.Height != 854 {
		t.Fatalf("应取到封面帧及其尺寸：%+v ok=%v err=%v", p, ok, err)
	}
	if _, ok, _ := svc.PosterOf(ctx, wsB, video); ok {
		t.Fatal("别的工作区取到了封面帧 —— 归属条件必须在查询里")
	}
	if _, err := pool.Exec(ctx, `update media_object set status = 'deleted' where id = $1`, poster.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := svc.PosterOf(ctx, wsA, video); ok {
		t.Fatal("删掉的封面帧不该再给")
	}
}
