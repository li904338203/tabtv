package domain

import (
	"errors"
	"testing"
	"time"
)

func tp(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return &t
}

// ⚠️ 投放状态是**算出来的**，不是作业标出来的 —— 靠定时任务翻 is_enabled 的话，
// 作业停一天首页上就有一整天的过期 Banner。
func TestBannerActiveAt(t *testing.T) {
	now := *tp("2026-09-23T12:00:00Z")
	cases := []struct {
		name    string
		enabled bool
		s       BannerSchedule
		want    bool
	}{
		{"停用的一律不投", false, BannerSchedule{}, false},
		{"两端都不限就一直投", true, BannerSchedule{}, true},
		{"还没开始", true, BannerSchedule{StartAt: tp("2026-09-23T13:00:00Z")}, false},
		// ⚠️ 边界取 [start, end)：开始那一刻就投。
		{"开始那一刻就投", true, BannerSchedule{StartAt: tp("2026-09-23T12:00:00Z")}, true},
		{"已经结束", true, BannerSchedule{EndAt: tp("2026-09-23T11:00:00Z")}, false},
		// ⚠️ 结束那一刻就停 —— 两端都闭的话，"到 12:00 结束"的那条会与
		// 下一条在 12:00:00 那一秒同时出现在首页上。
		{"结束那一刻就停", true, BannerSchedule{EndAt: tp("2026-09-23T12:00:00Z")}, false},
		{"窗口内", true, BannerSchedule{
			StartAt: tp("2026-09-23T11:00:00Z"), EndAt: tp("2026-09-23T13:00:00Z"),
		}, true},
		{"停用优先于窗口", false, BannerSchedule{
			StartAt: tp("2026-09-23T11:00:00Z"), EndAt: tp("2026-09-23T13:00:00Z"),
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.s.ActiveAt(c.enabled, now); got != c.want {
				t.Fatalf("ActiveAt = %v, want %v", got, c.want)
			}
		})
	}
}

func TestValidateSchedule(t *testing.T) {
	if err := ValidateSchedule(BannerSchedule{}); err != nil {
		t.Fatalf("两端都不限应合法：%v", err)
	}
	ok := BannerSchedule{StartAt: tp("2026-09-23T10:00:00Z"), EndAt: tp("2026-09-23T11:00:00Z")}
	if err := ValidateSchedule(ok); err != nil {
		t.Fatalf("正常窗口应合法：%v", err)
	}
	bad := BannerSchedule{StartAt: tp("2026-09-23T11:00:00Z"), EndAt: tp("2026-09-23T10:00:00Z")}
	if err := ValidateSchedule(bad); !errors.Is(err, ErrBadSchedule) {
		t.Fatalf("倒置的窗口应被拒，得到 %v", err)
	}
	// 相等也不行：那是一个零长度的窗口，永远投不出去而运营看不出为什么。
	same := BannerSchedule{StartAt: tp("2026-09-23T10:00:00Z"), EndAt: tp("2026-09-23T10:00:00Z")}
	if err := ValidateSchedule(same); !errors.Is(err, ErrBadSchedule) {
		t.Fatalf("零长度窗口应被拒，得到 %v", err)
	}
}

// ⚠️ 写作/数字人在本项目明确排除 —— 保留一个选不生效的位置比没有更糟。
func TestBannerPosition(t *testing.T) {
	if !PositionHome.Valid() || !PositionVideo.Valid() {
		t.Fatal("home 与 video 应合法")
	}
	for _, p := range []BannerPosition{"writing", "avatar", "", "HOME"} {
		if BannerPosition(p).Valid() {
			t.Fatalf("%q 不该合法", p)
		}
	}
}

// 管理端的「展示中」必须与用户端实际投放是同一个判定：逐个边界比对 ActiveAt。
func TestLifecycle_与投放判定一致(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
	cases := []struct {
		name    string
		enabled bool
		s       BannerSchedule
		want    BannerLifecycle
	}{
		{"停用即使在窗口内", false, BannerSchedule{}, LifecycleDisabled},
		{"不限", true, BannerSchedule{}, LifecycleActive},
		{"开始于此刻（左闭）", true, BannerSchedule{StartAt: at(0)}, LifecycleActive},
		{"还没开始", true, BannerSchedule{StartAt: at(time.Second)}, LifecycleScheduled},
		{"结束于此刻（右开）", true, BannerSchedule{EndAt: at(0)}, LifecycleExpired},
		{"还没结束", true, BannerSchedule{EndAt: at(time.Second)}, LifecycleActive},
		{"窗口内", true, BannerSchedule{StartAt: at(-time.Hour), EndAt: at(time.Hour)}, LifecycleActive},
	}
	for _, c := range cases {
		got := c.s.Lifecycle(c.enabled, now)
		if got != c.want {
			t.Errorf("%s：得到 %s，应为 %s", c.name, got, c.want)
		}
		if (got == LifecycleActive) != c.s.ActiveAt(c.enabled, now) {
			t.Errorf("%s：Lifecycle 与 ActiveAt 不一致 —— 管理端的「展示中」与首页实际投放会对不上", c.name)
		}
	}
}

// 一串空格不是正文：放过去的话全站会收到一条点开一片空白的公告，而发布不可撤销。
func TestValidateAnnouncementContent_空白等于空(t *testing.T) {
	for _, c := range []string{"", "   ", "\n\t "} {
		if err := ValidateAnnouncementContent(c); !errors.Is(err, ErrEmptyContent) {
			t.Fatalf("正文 %q 应被拒，得到 %v", c, err)
		}
	}
	if err := ValidateAnnouncementContent(" 维护通知 "); err != nil {
		t.Fatalf("有内容的正文应通过，得到 %v", err)
	}
}

func TestValidateBannerTitle(t *testing.T) {
	if err := ValidateBannerTitle("  "); !errors.Is(err, ErrEmptyTitle) {
		t.Fatalf("空白标题应被拒，得到 %v", err)
	}
	long := make([]rune, 101)
	for i := range long {
		long[i] = '字'
	}
	if err := ValidateBannerTitle(string(long)); !errors.Is(err, ErrTitleTooLong) {
		t.Fatalf("超长标题应被拒，得到 %v", err)
	}
	// 按**字符**不按字节：100 个汉字是 300 字节，按字节算会把合法标题挡掉。
	okTitle := make([]rune, 100)
	for i := range okTitle {
		okTitle[i] = '字'
	}
	if err := ValidateBannerTitle(string(okTitle)); err != nil {
		t.Fatalf("100 个汉字应合法：%v", err)
	}
}

// ⚠️ 发布是单独一次显式操作，不混进 update —— 混进去的话，运营改个错字
// 保存一下就等于给全站重新发了一次公告，而这个动作不可撤销。
func TestAnnouncementStateMachine(t *testing.T) {
	if err := CanPublish(StatusDraft); err != nil {
		t.Fatalf("草稿应可发布：%v", err)
	}
	if err := CanPublish(StatusPublished); !errors.Is(err, ErrNotDraft) {
		t.Fatalf("已发布的不该能再发布，得到 %v", err)
	}
	if err := CanPublish(StatusOffline); !errors.Is(err, ErrNotDraft) {
		t.Fatalf("已下线的不该能直接再发布，得到 %v", err)
	}
	if err := CanOffline(StatusPublished); err != nil {
		t.Fatalf("已发布的应可下线：%v", err)
	}
	if err := CanOffline(StatusDraft); !errors.Is(err, ErrNotPublished) {
		t.Fatalf("草稿不该能下线，得到 %v", err)
	}
}

func TestParseSource(t *testing.T) {
	for _, s := range []string{"notification", "announcement"} {
		if _, err := ParseSource(s); err != nil {
			t.Fatalf("%q 应合法：%v", s, err)
		}
	}
	// ⚠️ 两张表的 id 在同一个空间里无从区分，所以 source 必填且必须校验。
	for _, s := range []string{"", "msg", "Notification"} {
		if _, err := ParseSource(s); !errors.Is(err, ErrUnknownSource) {
			t.Fatalf("%q 不该合法，得到 %v", s, err)
		}
	}
}

// ⚠️ 公开三条件缺一不可。这张表就是"取消加精之后直链还能打开"这类
// 安静错误的回归。
func TestPubliclyVisible(t *testing.T) {
	cases := []struct {
		name string
		w    Work
		want bool
	}{
		{"全满足", Work{ReviewStatus: "approved", IsFeatured: true}, true},
		{"未审核", Work{ReviewStatus: "pending", IsFeatured: true}, false},
		{"被驳回", Work{ReviewStatus: "rejected", IsFeatured: true}, false},
		// 审核通过只是"没有违规"，不是"值得展示" —— 加精才是发布决定。
		{"通过但未加精", Work{ReviewStatus: "approved"}, false},
		// 下架要能立刻收回，所以它是独立的一位。
		{"已加精但下架", Work{ReviewStatus: "approved", IsFeatured: true, IsOffline: true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := PubliclyVisible(c.w); got != c.want {
				t.Fatalf("PubliclyVisible = %v, want %v", got, c.want)
			}
		})
	}
}

// ⚠️ 非法值必须报错而不是回落成「全部」：回落的话筛音频却出来一堆视频。
func TestParseShowcaseKind_白名单与非法值(t *testing.T) {
	for _, s := range []string{"", "video", "image", "audio"} {
		k, err := ParseShowcaseKind(s)
		if err != nil || string(k) != s {
			t.Fatalf("%q 应合法，得到 %q / %v", s, k, err)
		}
	}
	// other 只出现在作品上，不是可筛的分类；大小写不同也不认（白名单是精确匹配）。
	for _, s := range []string{"other", "foo", "Video", " video", "video/"} {
		if _, err := ParseShowcaseKind(s); !errors.Is(err, ErrBadShowcaseKind) {
			t.Fatalf("%q 不该合法，得到 %v", s, err)
		}
	}
}

func TestKindOfContentType_按前缀归类且未知归other(t *testing.T) {
	cases := map[string]ShowcaseKind{
		"video/mp4":                ShowcaseKindVideo,
		"Video/QuickTime":          ShowcaseKindVideo,
		"image/png":                ShowcaseKindImage,
		" image/webp ":             ShowcaseKindImage,
		"audio/mpeg":               ShowcaseKindAudio,
		"":                         ShowcaseKindOther, // 没有媒体的公开作品
		"application/octet-stream": ShowcaseKindOther,
		"text/plain":               ShowcaseKindOther,
		"videos/mp4":               ShowcaseKindOther, // 前缀要带斜杠，不是字符串包含
	}
	for ct, want := range cases {
		if got := KindOfContentType(ct); got != want {
			t.Errorf("KindOfContentType(%q) = %q, want %q", ct, got, want)
		}
	}
	// 每个可筛的类型，用它自己的前缀造一个 MIME 必须归回它自己 —— 筛选与归类同一张表。
	for _, k := range []ShowcaseKind{ShowcaseKindImage, ShowcaseKindVideo, ShowcaseKindAudio} {
		if got := KindOfContentType(k.ContentTypePrefix() + "x"); got != k {
			t.Errorf("%q 的前缀归类成了 %q", k, got)
		}
	}
	if ShowcaseKindAll.ContentTypePrefix() != "" || ShowcaseKindOther.ContentTypePrefix() != "" {
		t.Error("不筛与 other 不该有前缀")
	}
}
