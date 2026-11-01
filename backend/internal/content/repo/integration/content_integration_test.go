//go:build integration

// 内容域的真库测试（P13-01；AD-02/03、CV-01）。
//
// ⚠️ 必须用真库：这一组要守的全部由 **SQL** 决定 ——
// Banner 的排期边界、消息中心的**跨表 union 分页**、公开可见性的三条件、
// 以及"下线一条公告之后它立刻从所有人的列表里消失"。
package integration

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/content/domain"
	"github.com/li904338203/tabtv/backend/internal/content/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
)

func setup(t *testing.T) (*repo.Repo, *pgxpool.Pool, func() int64) {
	t.Helper()
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	return repo.New(pool, gen.Next), pool, gen.Next
}

func at(s string) *time.Time {
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return &v
}

// ⚠️ 排期是**算出来的**（SQL 条件），不是作业标出来的。
// 这一条同时验 SQL 与 `domain.ActiveAt` —— 两处是同一个式子，
// 用同一批数据同时验，改一处漏改另一处就会红。
func TestActiveBanners_排期边界(t *testing.T) {
	r, _, _ := setup(t)
	ctx := context.Background()
	now := *at("2026-09-23T12:00:00Z")

	mk := func(title string, enabled bool, start, end *time.Time) repo.Banner {
		b, err := r.InsertBanner(ctx, repo.Banner{
			Position: domain.PositionHome, Title: title, IsEnabled: enabled,
			StartAt: start, EndAt: end,
		})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	mk("一直投", true, nil, nil)
	mk("停用的", false, nil, nil)
	mk("还没开始", true, at("2026-09-23T13:00:00Z"), nil)
	mk("已经结束", true, nil, at("2026-09-23T11:00:00Z"))
	// ⚠️ 边界 [start, end)：结束那一刻就停 —— 两端都闭的话，
	// "到 12:00 结束"的那条会与下一条在 12:00:00 那一秒同时出现在首页上。
	mk("正好结束", true, nil, at("2026-09-23T12:00:00Z"))
	mk("正好开始", true, at("2026-09-23T12:00:00Z"), nil)
	mk("窗口内", true, at("2026-09-23T11:00:00Z"), at("2026-09-23T13:00:00Z"))

	got, err := r.ActiveBanners(ctx, domain.PositionHome, now, 50)
	if err != nil {
		t.Fatal(err)
	}
	titles := map[string]bool{}
	for _, b := range got {
		titles[b.Title] = true
		// SQL 与 domain 必须给出同一个答案。
		if !(domain.BannerSchedule{StartAt: b.StartAt, EndAt: b.EndAt}).
			ActiveAt(b.IsEnabled, now) {
			t.Errorf("SQL 认为 %q 在投，而 domain.ActiveAt 不认", b.Title)
		}
	}
	for _, want := range []string{"一直投", "正好开始", "窗口内"} {
		if !titles[want] {
			t.Errorf("%q 应在投，实际没有：%v", want, titles)
		}
	}
	for _, no := range []string{"停用的", "还没开始", "已经结束", "正好结束"} {
		if titles[no] {
			t.Errorf("%q 不该在投", no)
		}
	}

	// ⚠️ 管理端**不过滤排期**：运营要看得到没生效与已过期的 ——
	// 过滤掉的话他排好的明天那条会从列表里消失，于是他会再配一条。
	all, err := r.ListBanners(ctx, string(domain.PositionHome))
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 7 {
		t.Fatalf("管理端应看到全部 7 条，实际 %d", len(all))
	}
}

// ⚠️ 消息中心的合并**必须发生在 SQL 里**：两边各取一页再在内存里拼，
// **第二页会漏消息** —— 各取 N 条合出来的 N 条，与真正按时间序的第 1~N 条
// 不是同一批，被挤掉的那些**再也不会出现在任何一页上**，
// 而用户看到的列表"看起来很正常"。
func TestListMessages_跨表分页不漏不重(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	user := mkUser(t, pool, next, "读者", "13400000001")

	// 交错造 6 条通知 + 6 条公告，时间严格递增。
	base := time.Now().Add(-time.Hour)
	want := make([]string, 0, 12)
	for i := 0; i < 6; i++ {
		nt := base.Add(time.Duration(i*2) * time.Minute)
		ct := base.Add(time.Duration(i*2+1) * time.Minute)
		nid, aid := next(), next()
		if _, err := pool.Exec(ctx, `
			insert into notification (id, user_id, type, title, created_at)
			values ($1,$2,'generation',$3,$4)`, nid, user, "通知"+itoa(i), nt); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			insert into announcement (id, title, content, status, published_at,
				created_by_admin_id, created_at)
			values ($1,$2,'正文','published',$3,1,$3)`, aid, "公告"+itoa(i), ct); err != nil {
			t.Fatal(err)
		}
		want = append(want, "通知"+itoa(i), "公告"+itoa(i))
	}
	// 期望的顺序是**时间倒序**。
	for i, j := 0, len(want)-1; i < j; i, j = i+1, j-1 {
		want[i], want[j] = want[j], want[i]
	}

	// 每页 5 条翻完，收集标题。
	var got []string
	var cursorAt *time.Time
	var cursorID int64
	for page := 0; page < 10; page++ {
		list, err := r.ListMessages(ctx, user, 5, cursorAt, cursorID)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) == 0 {
			break
		}
		for _, m := range list {
			got = append(got, m.Title)
		}
		last := list[len(list)-1]
		cursorAt, cursorID = &last.CreatedAt, last.ID
		if len(list) < 5 {
			break
		}
	}
	if len(got) != len(want) {
		t.Fatalf("翻完应得到 %d 条，实际 %d 条：%v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 条应是 %q，实际 %q（完整：%v）", i, want[i], got[i], got)
		}
	}
	// 不重复。
	seen := map[string]bool{}
	for _, tt := range got {
		if seen[tt] {
			t.Fatalf("%q 出现了两次", tt)
		}
		seen[tt] = true
	}
}

// ⚠️ 下线一条公告之后它必须**立刻**从所有人的列表里消失 ——
// 状态写在查询条件里，而不是靠发布时给每个人塞一行然后再去删。
func TestListMessages_只看已发布的公告(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	user := mkUser(t, pool, next, "读者", "13400000011")

	a, err := r.InsertAnnouncement(ctx, repo.Announcement{
		Title: "维护通知", Content: "正文", CreatedBy: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 草稿不该出现。
	if list, _ := r.ListMessages(ctx, user, 10, nil, 0); len(list) != 0 {
		t.Fatalf("草稿不该出现在用户面前，得到 %+v", list)
	}
	if _, err := r.SetAnnouncementStatus(ctx, a.ID,
		domain.StatusDraft, domain.StatusPublished, true); err != nil {
		t.Fatal(err)
	}
	list, _ := r.ListMessages(ctx, user, 10, nil, 0)
	if len(list) != 1 || list[0].Source != domain.SourceAnnouncement {
		t.Fatalf("发布后应出现，得到 %+v", list)
	}
	if n, _ := r.UnreadCount(ctx, user); n != 1 {
		t.Fatalf("未读数应是 1，得到 %d", n)
	}

	// 下线后立刻消失，未读数同步归零 ——
	// ⚠️ 列表与未读数用**同一套条件**：分开写的话红点显示有未读
	// 而点进去什么都没有，且这种不一致没有任何报错。
	if _, err := r.SetAnnouncementStatus(ctx, a.ID,
		domain.StatusPublished, domain.StatusOffline, false); err != nil {
		t.Fatal(err)
	}
	if list, _ := r.ListMessages(ctx, user, 10, nil, 0); len(list) != 0 {
		t.Fatalf("下线后应立刻消失，得到 %+v", list)
	}
	if n, _ := r.UnreadCount(ctx, user); n != 0 {
		t.Fatalf("下线后未读数应归零，得到 %d", n)
	}
}

// ⚠️ CAS：两个运营同时点发布只有一个成功。
// 不带 `status = 期望值` 的话，第二次点会把 published_at 刷新一遍 ——
// 公告按发布时间排序，于是它会莫名其妙地跳到最前面。
func TestSetAnnouncementStatus_CAS(t *testing.T) {
	r, _, _ := setup(t)
	ctx := context.Background()
	a, err := r.InsertAnnouncement(ctx, repo.Announcement{
		Title: "只发一次", Content: "正文", CreatedBy: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := r.SetAnnouncementStatus(ctx, a.ID,
		domain.StatusDraft, domain.StatusPublished, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetAnnouncementStatus(ctx, a.ID,
		domain.StatusDraft, domain.StatusPublished, true); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("第二次发布应 CAS 落空，得到 %v", err)
	}
	after, _ := r.GetAnnouncement(ctx, a.ID)
	if !after.PublishedAt.Equal(*first.PublishedAt) {
		t.Fatal("发布时间不该被第二次点击刷新")
	}
}

// ⚠️ 标已读必须带 source：两张表的 id 在同一个空间里无从区分。
// 并且通知的更新条件带 user_id —— 不带的话任何人都能把别人的通知标已读。
func TestMarkRead(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	me := mkUser(t, pool, next, "我", "13400000021")
	other := mkUser(t, pool, next, "别人", "13400000022")

	nid := next()
	if _, err := pool.Exec(ctx, `
		insert into notification (id, user_id, type, title)
		values ($1,$2,'generation','出图完成')`, nid, me); err != nil {
		t.Fatal(err)
	}
	// 别人标不动我的通知。
	if err := r.MarkRead(ctx, other, nid, domain.SourceNotification); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("别人不该能标我的通知，得到 %v", err)
	}
	if err := r.MarkRead(ctx, me, nid, domain.SourceNotification); err != nil {
		t.Fatal(err)
	}
	// ⚠️ **重复标已读不是错误**（多个标签页、重试都会发生）——
	// 报错的话前端得为一个无害的重复操作弹提示。
	if err := r.MarkRead(ctx, me, nid, domain.SourceNotification); err != nil {
		t.Fatalf("重复标已读不该报错：%v", err)
	}
	if n, _ := r.UnreadCount(ctx, me); n != 0 {
		t.Fatalf("已读后未读数应是 0，得到 %d", n)
	}

	// 公告的已读是**插入**而不是更新（全站一行，不可能去改那一行）。
	a, _ := r.InsertAnnouncement(ctx, repo.Announcement{Title: "公告", Content: "x", CreatedBy: 1})
	// 草稿标不动 —— 用户根本看不到它，标它已读只能是构造出来的请求。
	if err := r.MarkRead(ctx, me, a.ID, domain.SourceAnnouncement); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("草稿公告不该能标已读，得到 %v", err)
	}
	if _, err := r.SetAnnouncementStatus(ctx, a.ID,
		domain.StatusDraft, domain.StatusPublished, true); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkRead(ctx, me, a.ID, domain.SourceAnnouncement); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkRead(ctx, me, a.ID, domain.SourceAnnouncement); err != nil {
		t.Fatalf("重复标已读不该报错：%v", err)
	}
	// 我读了不影响别人。
	if n, _ := r.UnreadCount(ctx, other); n != 1 {
		t.Fatalf("别人应仍有 1 条未读，得到 %d", n)
	}
}

func TestMarkAllRead(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	me := mkUser(t, pool, next, "我", "13400000031")
	for i := 0; i < 3; i++ {
		if _, err := pool.Exec(ctx, `
			insert into notification (id, user_id, type, title)
			values ($1,$2,'generation',$3)`, next(), me, "通知"+itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		a, _ := r.InsertAnnouncement(ctx, repo.Announcement{
			Title: "公告" + itoa(i), Content: "x", CreatedBy: 1})
		if _, err := r.SetAnnouncementStatus(ctx, a.ID,
			domain.StatusDraft, domain.StatusPublished, true); err != nil {
			t.Fatal(err)
		}
	}
	n, err := r.MarkAllRead(ctx, me)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Fatalf("应标记 5 条，得到 %d", n)
	}
	if u, _ := r.UnreadCount(ctx, me); u != 0 {
		t.Fatalf("全部已读后应为 0，得到 %d", u)
	}
	// 幂等：再点一次不报错也不重复插入。
	if n, err := r.MarkAllRead(ctx, me); err != nil || n != 0 {
		t.Fatalf("重复全部已读应是 0 条无错，得到 %d / %v", n, err)
	}
}

// ⚠️ 通知幂等：Worker 重试同一个任务不会发第二条（FLOW-10）。
func TestInsertNotification_幂等(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	me := mkUser(t, pool, next, "我", "13400000041")
	msg := repo.Message{Type: "generation", Title: "出图完成",
		RefType: "generation", RefID: "999"}

	ok, err := r.InsertNotification(ctx, msg, me)
	if err != nil || !ok {
		t.Fatalf("第一次应写入：ok=%v err=%v", ok, err)
	}
	ok, err = r.InsertNotification(ctx, msg, me)
	if err != nil {
		t.Fatalf("重复写入不该报错：%v", err)
	}
	if ok {
		t.Fatal("重复写入应返回 false（已经发过了）")
	}
	if n, _ := r.UnreadCount(ctx, me); n != 1 {
		t.Fatalf("应只有 1 条，得到 %d", n)
	}
	// ⚠️ 审核类的 refId 要带动作（"217:approved"），否则先驳回后通过只能发一条。
	msg2 := msg
	msg2.RefID = "999:approved"
	if ok, _ := r.InsertNotification(ctx, msg2, me); !ok {
		t.Fatal("不同 refId 应能各发一条")
	}
}

// ⚠️ 公开三条件缺一不可，且**列表与取图用同一个判定** ——
// 分开写的话会出现"列表里没有但直链还能打开"。
func TestShowcase_公开可见性(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()

	mkWork := func(title string, status string, featured, offline bool) int64 {
		id := next()
		if _, err := pool.Exec(ctx, `
			insert into work (id, workspace_id, source_type, source_id, media_object_id,
				title, review_status, is_featured, is_offline)
			values ($1,1,'generation',$2,$3,$4,$5,$6,$7)`,
			id, next(), next(), title, status, featured, offline); err != nil {
			t.Fatal(err)
		}
		return id
	}
	public := mkWork("公开的", "approved", true, false)
	pending := mkWork("待审的", "pending", true, false)
	notFeatured := mkWork("没加精", "approved", false, false)
	offline := mkWork("已下架", "approved", true, true)

	items, err := r.ListShowcase(ctx, 50, 0, domain.ShowcaseKindAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != public {
		t.Fatalf("只有三条件全满足的才公开，得到 %+v", items)
	}

	// 取图与列表**同一个判定**。
	if _, err := r.PublicMediaOf(ctx, public); err != nil {
		t.Fatalf("公开作品应取得到图：%v", err)
	}
	for _, id := range []int64{pending, notFeatured, offline} {
		if _, err := r.PublicMediaOf(ctx, id); !errors.Is(err, repo.ErrNotFound) {
			t.Fatalf("不公开的作品不该取得到图（work=%d），得到 %v", id, err)
		}
	}
	// 取消加精之后**立刻**取不到 —— 不需要任何缓存失效。
	if _, err := pool.Exec(ctx,
		`update work set is_featured = false where id = $1`, public); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PublicMediaOf(ctx, public); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("取消加精后应立刻取不到，得到 %v", err)
	}
}

// mkShowcaseWork 造一件作品；contentType 为空 = 没有媒体（media_object_id 为空）。
func mkShowcaseWork(t *testing.T, pool *pgxpool.Pool, next func() int64,
	title, contentType string, featured bool) int64 {
	t.Helper()
	ctx := context.Background()
	var mediaID any
	if contentType != "" {
		mid := next()
		if _, err := pool.Exec(ctx, `
			insert into media_object (id, workspace_id, bucket, object_key, content_type,
				size_bytes, purpose, created_by_user_id)
			values ($1,1,'b',$2,$3,1,'generation_output',1)`,
			mid, "k/"+strconv.FormatInt(mid, 10), contentType); err != nil {
			t.Fatal(err)
		}
		mediaID = mid
	}
	id := next()
	if _, err := pool.Exec(ctx, `
		insert into work (id, workspace_id, source_type, source_id, media_object_id,
			title, review_status, is_featured, is_offline)
		values ($1,1,'generation',$2,$3,$4,'approved',$5,false)`,
		id, next(), mediaID, title, featured); err != nil {
		t.Fatal(err)
	}
	return id
}

// AGT-41：按类型服务端筛选，且每件作品带上由 content_type 推导的类型。
func TestShowcase_按类型筛选(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	video := mkShowcaseWork(t, pool, next, "视频", "video/mp4", true)
	image := mkShowcaseWork(t, pool, next, "图片", "image/png", true)
	audio := mkShowcaseWork(t, pool, next, "音频", "audio/mpeg", true)
	bare := mkShowcaseWork(t, pool, next, "无媒体", "", true)

	// 「全部」下没有媒体的那件也要在（left join，不是 join），且各自带对的类型。
	all, err := r.ListShowcase(ctx, 50, 0, domain.ShowcaseKindAll)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]domain.ShowcaseKind{
		video: domain.ShowcaseKindVideo, image: domain.ShowcaseKindImage,
		audio: domain.ShowcaseKindAudio, bare: domain.ShowcaseKindOther,
	}
	if len(all) != len(want) {
		t.Fatalf("全部应有 %d 件，得到 %+v", len(want), all)
	}
	for _, it := range all {
		if it.Type != want[it.ID] {
			t.Errorf("作品 %q 的类型应为 %q，得到 %q", it.Title, want[it.ID], it.Type)
		}
	}

	for kind, id := range map[domain.ShowcaseKind]int64{
		domain.ShowcaseKindVideo: video, domain.ShowcaseKindImage: image,
		domain.ShowcaseKindAudio: audio,
	} {
		got, err := r.ListShowcase(ctx, 50, 0, kind)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ID != id || got[0].Type != kind {
			t.Fatalf("type=%s 只应返回那一件，得到 %+v", kind, got)
		}
	}
}

// ⚠️ 类型筛选只能收窄、不能放宽公开边界：未加精 / 已下架 / 未过审的视频
// 在 type=video 下也不能出现。
func TestShowcase_类型筛选不放宽公开边界(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	public := mkShowcaseWork(t, pool, next, "公开视频", "video/mp4", true)
	mkShowcaseWork(t, pool, next, "没加精的视频", "video/mp4", false)
	offline := mkShowcaseWork(t, pool, next, "下架的视频", "video/mp4", true)
	pending := mkShowcaseWork(t, pool, next, "待审的视频", "video/mp4", true)
	if _, err := pool.Exec(ctx, `update work set is_offline = true where id = $1`,
		offline); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `update work set review_status = 'pending' where id = $1`,
		pending); err != nil {
		t.Fatal(err)
	}

	got, err := r.ListShowcase(ctx, 50, 0, domain.ShowcaseKindVideo)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != public {
		t.Fatalf("type=video 下只应有公开的那件，得到 %+v", got)
	}
}

// 带 type 时游标翻页不重不漏：其他类型的作品穿插在中间，也不能挤占页内名额。
func TestShowcase_类型筛选游标翻页不重不漏(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	videos := map[int64]bool{}
	for i := 0; i < 5; i++ {
		videos[mkShowcaseWork(t, pool, next, "视频"+strconv.Itoa(i), "video/mp4", true)] = true
		mkShowcaseWork(t, pool, next, "图片"+strconv.Itoa(i), "image/png", true)
	}

	seen := map[int64]bool{}
	var cursor int64
	for page := 0; page < 10; page++ {
		got, err := r.ListShowcase(ctx, 2, cursor, domain.ShowcaseKindVideo)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range got {
			if !videos[it.ID] {
				t.Fatalf("type=video 翻页出现了非视频作品 %+v", it)
			}
			if seen[it.ID] {
				t.Fatalf("翻页重复：%+v", it)
			}
			seen[it.ID] = true
		}
		if len(got) < 2 {
			break
		}
		cursor = got[len(got)-1].ID
	}
	if len(seen) != len(videos) {
		t.Fatalf("翻页漏了：应有 %d 件，取到 %d 件", len(videos), len(seen))
	}
}

// 最近项目：回收站里的不该出现 —— 用户刚删掉它，首页却还推荐他打开。
func TestRecentProjects(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	const ws = int64(7777)
	mkProject := func(name, status string) {
		if _, err := pool.Exec(ctx, `
			insert into project (id, workspace_id, created_by_user_id, name, canvas_data, status)
			values ($1,$2,1,$3,'{}'::jsonb,$4)`, next(), ws, name, status); err != nil {
			t.Fatal(err)
		}
	}
	mkProject("活着的", "active")
	mkProject("回收站里的", "deleted")

	got, err := r.RecentProjects(ctx, ws, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "活着的" {
		t.Fatalf("回收站里的不该出现，得到 %+v", got)
	}
	// 跨工作区拿不到。
	if other, _ := r.RecentProjects(ctx, ws+1, 10); len(other) != 0 {
		t.Fatalf("跨工作区不该看到，得到 %+v", other)
	}
}

// 最近项目带 origin（MKT-T-32）：首页按它显示「营销」角标，与全部项目页同一列同一取值；
// 未记录的输出空串（transport 层转成 null）。
func TestRecentProjects_带项目来源(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	const ws = int64(7779)
	if _, err := pool.Exec(ctx, `
		insert into project (id, workspace_id, created_by_user_id, name, canvas_data, status, origin, updated_at)
		values ($1,$3,1,'营销的','{}'::jsonb,'active','marketing', now()),
		       ($2,$3,1,'普通的','{}'::jsonb,'active',null, now() - interval '1 minute')`,
		next(), next(), ws); err != nil {
		t.Fatal(err)
	}
	got, err := r.RecentProjects(ctx, ws, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Origin != "marketing" || got[1].Origin != "" {
		t.Fatalf("origin 不对：%+v", got)
	}
}

func mkUser(t *testing.T, pool *pgxpool.Pool, next func() int64, nickname, phone string) int64 {
	t.Helper()
	id := next()
	if _, err := pool.Exec(context.Background(), `
		insert into app_user (id, phone, nickname, password_hash)
		values ($1,$2,$3,'x')`, id, phone, nickname); err != nil {
		t.Fatal(err)
	}
	return id
}

func itoa(i int) string { return string(rune('0' + i)) }

// 首页「最近项目」的封面与「全部项目」页**必须是同一个**。
//
// ⚠️ 两个域各自读 `project` 表（CD-61 的决定），而「默认封面」这条规则只写在
// SQL 函数 `project_display_cover()` 里一处（迁移 00030）。这条用例钉住 content 这边
// 真的调了它 —— 哪天有人在这里另写一段子查询，两个页面的封面就会对不上，而不报错。
func TestRecentProjects_展示封面取最近一张产出图(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	const ws = int64(7778)

	pid := next()
	if _, err := pool.Exec(ctx, `
		insert into project (id, workspace_id, created_by_user_id, name, canvas_data, status)
		values ($1,$2,1,'出过图的项目','{}'::jsonb,'active')`, pid, ws); err != nil {
		t.Fatal(err)
	}
	mid := next()
	if _, err := pool.Exec(ctx, `
		insert into media_object (id, workspace_id, bucket, object_key, content_type, size_bytes,
		                          purpose, created_by_user_id)
		values ($1,$2,'seeaitv',$3,'image/png',1,'generation_output',1)`,
		mid, ws, "t/recent.png"); err != nil {
		t.Fatal(err)
	}
	gid := next()
	if _, err := pool.Exec(ctx, `
		insert into generation (id, workspace_id, payer_account_id, created_by_user_id, project_id,
		                        operation_id, model_id, result_type, idempotency_key, status)
		values ($1,$2,1,1,$3,1,1,'image','k-recent','success')`, gid, ws, pid); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into generation_output (id, generation_id, idx, media_object_id, output_type)
		values ($1,$2,0,$3,'image')`, next(), gid, mid); err != nil {
		t.Fatal(err)
	}

	got, err := r.RecentProjects(ctx, ws, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("应有 1 个最近项目，得到 %+v", got)
	}
	if got[0].CoverMediaID != 0 {
		t.Fatalf("没手动设过封面，CoverMediaID 应为 0，得到 %d", got[0].CoverMediaID)
	}
	if got[0].DisplayCoverMediaID != mid {
		t.Fatalf("展示封面应为那张产出图 %d，得到 %d —— 首页会一直显示占位图", mid, got[0].DisplayCoverMediaID)
	}
}

// ── 我的作品（成功产出画廊，GET /creations）───────────────────────────
//
// ⚠️ 起因：移植过来的「我的作品」页照老语义调 `/works`，而新系统的 `/works` 是
// 「提交去审核的作品」—— 用户一件都没提交过，这一页永远是空的。
// 这组用例钉住新查询的每一条规则；全都是 SQL 决定的，只能在真库上验。

type creationSeed struct {
	ws, project int64
	status, typ string
}

func seedCreation(t *testing.T, pool *pgxpool.Pool, next func() int64, s creationSeed,
	outputs ...func(genID int64)) int64 {
	t.Helper()
	gid := next()
	if _, err := pool.Exec(context.Background(), `
		insert into generation (id, workspace_id, payer_account_id, created_by_user_id, project_id,
		                        operation_id, model_id, result_type, idempotency_key, status)
		values ($1,$2,1,1,$3,1,1,$4,$5,$6)`,
		gid, s.ws, s.project, s.typ, "k-"+strconv.FormatInt(gid, 10), s.status); err != nil {
		t.Fatal(err)
	}
	for _, add := range outputs {
		add(gid)
	}
	return gid
}

func mediaOutput(t *testing.T, pool *pgxpool.Pool, next func() int64, ws int64, idx int) (func(int64), int64) {
	t.Helper()
	mid := next()
	if _, err := pool.Exec(context.Background(), `
		insert into media_object (id, workspace_id, bucket, object_key, content_type, size_bytes,
		                          purpose, created_by_user_id)
		values ($1,$2,'seeaitv',$3,'image/png',1,'generation_output',1)`,
		mid, ws, "t/c-"+strconv.FormatInt(mid, 10)); err != nil {
		t.Fatal(err)
	}
	return func(gid int64) {
		if _, err := pool.Exec(context.Background(), `
			insert into generation_output (id, generation_id, idx, media_object_id, output_type)
			values ($1,$2,$3,$4,'image')`, next(), gid, idx, mid); err != nil {
			t.Fatal(err)
		}
	}, mid
}

func textOutput(t *testing.T, pool *pgxpool.Pool, next func() int64, body string) func(int64) {
	t.Helper()
	return func(gid int64) {
		if _, err := pool.Exec(context.Background(), `
			insert into generation_output (id, generation_id, idx, output_type, text_content)
			values ($1,$2,0,'text',$3)`, next(), gid, body); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCreations_只列成功的_且按工作区隔离(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	const ws, other = int64(8801), int64(8802)
	proj := next()
	if _, err := pool.Exec(ctx, `insert into project (id, workspace_id, created_by_user_id, name, canvas_data, status)
		values ($1,$2,1,'画廊项目','{}'::jsonb,'active')`, proj, ws); err != nil {
		t.Fatal(err)
	}
	img, _ := mediaOutput(t, pool, next, ws, 0)
	ok := seedCreation(t, pool, next, creationSeed{ws, proj, "success", "image"}, img)
	for _, st := range []string{"queued", "running", "failed", "cancelled"} {
		seedCreation(t, pool, next, creationSeed{ws, proj, st, "image"})
	}
	otherImg, _ := mediaOutput(t, pool, next, other, 0)
	seedCreation(t, pool, next, creationSeed{other, proj, "success", "image"}, otherImg)

	got, err := r.ListCreations(ctx, repo.CreationFilter{WorkspaceID: ws, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != ok {
		t.Fatalf("只应列出本工作区那 1 条成功的生成，得到 %+v", got)
	}
	if got[0].ProjectName != "画廊项目" {
		t.Fatalf("应带上项目名，得到 %q", got[0].ProjectName)
	}
	if n, _ := r.CountCreations(ctx, repo.CreationFilter{WorkspaceID: ws}); n != 1 {
		t.Fatalf("计数应与列表同一套条件 = 1，得到 %d", n)
	}
}

func TestCreations_首项预览_产物计数_类型筛选_不收文本(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	const ws = int64(8803)
	proj := next() // 故意不建项目行：项目被彻底删除后，产出仍要列出

	// 一次出两张图，且产物行**倒着插**：首项必须按 idx 取，而不是按插入顺序。
	second, _ := mediaOutput(t, pool, next, ws, 1)
	first, firstMedia := mediaOutput(t, pool, next, ws, 0)
	multi := seedCreation(t, pool, next, creationSeed{ws, proj, "success", "image"}, second, first)

	// ⚠️ 创作资产**不收文本**：成功的文本生成既不出现在「全部」里，也不算进总数。
	//    只在前端藏的话，页头的数字会把文本算进去，而列表里又看不到它们 —— 对不上。
	text := seedCreation(t, pool, next, creationSeed{ws, proj, "success", "text"},
		textOutput(t, pool, next, "一段文本"))

	all, err := r.ListCreations(ctx, repo.CreationFilter{WorkspaceID: ws, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range all {
		if c.ID == text {
			t.Fatal("「全部」里出现了文本生成 —— 创作资产只收图片、视频、音频")
		}
	}
	if len(all) != 1 || all[0].ID != multi {
		t.Fatalf("「全部」应只有那 1 条图片生成，得到 %+v", all)
	}
	if n, _ := r.CountCreations(ctx, repo.CreationFilter{WorkspaceID: ws}); n != 1 {
		t.Fatalf("「全部」的总数不该算文本，应为 1，得到 %d", n)
	}

	m := all[0]
	if m.CoverMedia != firstMedia || m.OutputCount != 2 {
		t.Fatalf("封面应是 idx=0 的那张 %d、共 2 项，得到 cover=%d count=%d", firstMedia, m.CoverMedia, m.OutputCount)
	}
	if m.ProjectName != "" {
		t.Fatalf("项目已不存在时项目名应为空串（产出照样列出），得到 %q", m.ProjectName)
	}

	images, _ := r.ListCreations(ctx, repo.CreationFilter{WorkspaceID: ws, ResultType: "image", Limit: 10})
	if len(images) != 1 || images[0].ID != multi {
		t.Fatalf("筛图片应只剩 1 条，得到 %+v", images)
	}
	if n, _ := r.CountCreations(ctx, repo.CreationFilter{WorkspaceID: ws, ResultType: "video"}); n != 0 {
		t.Fatalf("筛视频应为 0，得到 %d", n)
	}
}

// 游标翻页：不重、不漏。
func TestCreations_游标翻页不重不漏(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	const ws = int64(8804)
	want := map[int64]bool{}
	for i := 0; i < 5; i++ {
		add, _ := mediaOutput(t, pool, next, ws, 0)
		want[seedCreation(t, pool, next, creationSeed{ws, next(), "success", "image"}, add)] = true
	}
	seen := map[int64]int{}
	var cursor int64
	for pages := 0; pages < 10; pages++ {
		rows, err := r.ListCreations(ctx, repo.CreationFilter{WorkspaceID: ws, Limit: 2, CursorID: cursor})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			break
		}
		for _, c := range rows {
			seen[c.ID]++
		}
		cursor = rows[len(rows)-1].ID
	}
	if len(seen) != len(want) {
		t.Fatalf("应翻出全部 %d 条，得到 %d", len(want), len(seen))
	}
	for id, n := range seen {
		if n != 1 || !want[id] {
			t.Fatalf("id %d 出现 %d 次（或不该出现）", id, n)
		}
	}
}
