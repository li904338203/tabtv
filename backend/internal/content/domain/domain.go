// Package domain 是内容域的纯规则：Banner 排期、公告状态机、
// 消息来源的合并与已读、作品的公开可见性。
//
// ⚠️ 这一域的错误多半**看得见但不出声**：一条过期的 Banner 还挂在首页、
// 一个已下架的作品还能被外人打开、一条公告标了已读第二天又冒出来。
// 所以规则全部摊成纯函数（不碰库、不取当前时间，`now` 由调用方传进来），
// 每条单独可测。
package domain

import (
	"errors"
	"strings"
	"time"
)

// ---------------------------------------------------------------- Banner

// BannerPosition 是投放位置。
//
// ⚠️ **只有 home 与 video 两个值**。旧系统还有 writing / avatar，
// 而那两个产品在本项目明确排除（FULL-01 会验「不建对应路由、队列、权限、
// 表和后台入口」）—— 保留一个选不生效的位置比没有更糟
// （同旧系统 `presetCode` 选了等于没选的老毛病）。
type BannerPosition string

const (
	PositionHome  BannerPosition = "home"
	PositionVideo BannerPosition = "video"
)

func (p BannerPosition) Valid() bool {
	return p == PositionHome || p == PositionVideo
}

var (
	ErrBadPosition   = errors.New("投放位置只能是 home 或 video")
	ErrEmptyTitle    = errors.New("标题不能为空")
	ErrTitleTooLong  = errors.New("标题最多 100 字")
	ErrBadSchedule   = errors.New("结束时间必须晚于开始时间")
	ErrLinkTooLong   = errors.New("链接过长")
	ErrBadStatus     = errors.New("状态取值非法")
	ErrNotDraft      = errors.New("只有草稿可以发布")
	ErrNotPublished  = errors.New("只有已发布的可以下线")
	ErrEmptyContent  = errors.New("内容不能为空")
	ErrUnknownSource = errors.New("消息来源非法")
)

// BannerSchedule 是一条 Banner 的投放窗口。两端都可空 = 不限。
type BannerSchedule struct {
	StartAt *time.Time
	EndAt   *time.Time
}

// ActiveAt 判定某一刻这条 Banner 是否在投放。
//
// ⚠️ 投放状态是**算出来的**，不是作业标出来的。靠定时任务去翻 `is_enabled`
// 的话，作业停一天首页上就有一整天的过期 Banner —— 与提示词 ready/stale、
// 导出包过期、会员权益按 expire_at 判定是同一条规则（这已经是第五处）。
//
// ⚠️ 边界取 `[start, end)`：开始那一刻就投，结束那一刻就停。
// 两端都用闭区间的话，一条"到 12:00 结束"的 Banner 会在 12:00:00 那一秒
// 与下一条同时出现在首页上。
func (s BannerSchedule) ActiveAt(enabled bool, now time.Time) bool {
	if !enabled {
		return false
	}
	if s.StartAt != nil && now.Before(*s.StartAt) {
		return false
	}
	if s.EndAt != nil && !now.Before(*s.EndAt) {
		return false
	}
	return true
}

// BannerLifecycle 是管理端列表上的投放状态（展示中 / 待开始 / 已结束 / 已停用）。
type BannerLifecycle string

const (
	LifecycleActive    BannerLifecycle = "active"
	LifecycleScheduled BannerLifecycle = "scheduled"
	LifecycleExpired   BannerLifecycle = "expired"
	LifecycleDisabled  BannerLifecycle = "disabled"
)

// Lifecycle 给出某一刻的投放状态，`active` 当且仅当 ActiveAt 为真。
//
// ⚠️ 由服务端算、随列表下发，不让管理端拿本地时钟再算一遍：
// 两处各写一份的话边界迟早不一致（`[start, end)` 写成两端闭区间就是一例），
// 而运营看到的「展示中」与用户首页上实际投放的不是同一批 —— 两边都"没错"。
func (s BannerSchedule) Lifecycle(enabled bool, now time.Time) BannerLifecycle {
	switch {
	case !enabled:
		return LifecycleDisabled
	case s.StartAt != nil && now.Before(*s.StartAt):
		return LifecycleScheduled
	case s.EndAt != nil && !now.Before(*s.EndAt):
		return LifecycleExpired
	}
	return LifecycleActive
}

// ValidateSchedule 校验排期本身是否讲得通。
func ValidateSchedule(s BannerSchedule) error {
	if s.StartAt != nil && s.EndAt != nil && !s.EndAt.After(*s.StartAt) {
		return ErrBadSchedule
	}
	return nil
}

// ValidateBannerTitle 校验标题。
func ValidateBannerTitle(title string) error {
	t := strings.TrimSpace(title)
	if t == "" {
		return ErrEmptyTitle
	}
	if len([]rune(t)) > 100 {
		return ErrTitleTooLong
	}
	return nil
}

// ValidateAnnouncementContent 校验公告正文。
//
// ⚠️ 按**去掉空白之后**判空：只判 `== ""` 的话一串空格就能通过，
// 全站用户会在消息中心收到一条只有标题、点开一片空白的公告 —— 而它发出去就撤不回来。
func ValidateAnnouncementContent(content string) error {
	if strings.TrimSpace(content) == "" {
		return ErrEmptyContent
	}
	return nil
}

// ValidateLink 校验跳转链接长度（内容由运营负责，这里只挡住存不下的）。
func ValidateLink(u string) error {
	if len(u) > 512 {
		return ErrLinkTooLong
	}
	return nil
}

// ---------------------------------------------------------------- 公告

// AnnouncementStatus 是公告状态机：draft → published → offline。
type AnnouncementStatus string

const (
	StatusDraft     AnnouncementStatus = "draft"
	StatusPublished AnnouncementStatus = "published"
	StatusOffline   AnnouncementStatus = "offline"
)

func (s AnnouncementStatus) Valid() bool {
	switch s {
	case StatusDraft, StatusPublished, StatusOffline:
		return true
	}
	return false
}

// CanPublish 判定能否发布。
//
// ⚠️ **发布是单独一次显式操作，不混进 update**。混进去的话，运营改个错字
// 保存一下就等于给全站重新发了一次公告 —— 而这个动作**不可撤销**
// （已经推给所有人了）。同理下线也单独一条路径。
func CanPublish(cur AnnouncementStatus) error {
	if cur != StatusDraft {
		return ErrNotDraft
	}
	return nil
}

// CanOffline 判定能否下线。
func CanOffline(cur AnnouncementStatus) error {
	if cur != StatusPublished {
		return ErrNotPublished
	}
	return nil
}

// ---------------------------------------------------------------- 消息中心

// MessageSource 区分一条消息来自哪张表。
//
// ⚠️ **标已读必须回传 source**：两张表的 id 在同一个空间里**无从区分**
// （都是雪花号）。只传 id 的话，服务端只能两张表都试一遍 ——
// 而万一撞上（理论上不会，但雪花号的唯一性不是跨表保证的），
// 会把另一个人的公告标成已读。
type MessageSource string

const (
	SourceNotification MessageSource = "notification" // 个人通知，一人一行
	SourceAnnouncement MessageSource = "announcement" // 运营广播，全站一行
)

func (s MessageSource) Valid() bool {
	return s == SourceNotification || s == SourceAnnouncement
}

// ParseSource 解析并校验来源。
func ParseSource(s string) (MessageSource, error) {
	m := MessageSource(strings.TrimSpace(s))
	if !m.Valid() {
		return "", ErrUnknownSource
	}
	return m, nil
}

// ---------------------------------------------------------------- 作品可见性

// Work 是判定公开可见性所需的最小状态。
type Work struct {
	ReviewStatus string
	IsFeatured   bool
	IsOffline    bool
}

// PubliclyVisible 判定一件作品能否被**任何人**（含未登录访客）看到。
//
// ⚠️ 三个条件**缺一不可**：审核通过 ∧ 已加精 ∧ 未下架。
//   - 只看审核的话，所有通过审核的作品都上了公开墙 —— 而审核只是"没有违规"，
//     不是"值得展示"；
//   - 加精是运营的一次**显式发布决定**，公开边界就由它划定；
//   - 下架要能**立刻**收回，所以它是独立的一位而不是把 review_status 改回去
//     （改回去的话下架与驳回混成一件事，而"驳回"意味着作者要改，
//     "下架"可能只是运营临时撤下）。
//
// 取消加精或下架之后，公开取图必须**立刻**取不到 —— 与首页列表的可见性
// 始终一致。判定只有这一处，所以不会出现"列表里没有但直链还能打开"。
func PubliclyVisible(w Work) bool {
	return w.ReviewStatus == "approved" && w.IsFeatured && !w.IsOffline
}

// ---------------------------------------------------------------- 精选作品的媒体类型

// ShowcaseKind 是精选作品按媒体划分的类型（AGT-41）。
//
// ⚠️ `work` 表**没有媒体类型列**：类型只能从 `media_object.content_type` 推导，
// 推导规则只在 KindOfContentType 这一处。仓储按它把每一行归类、按
// ContentTypePrefix 拼筛选条件 —— 两处各写一份前缀表的话，迟早会出现
// "筛视频时出现了一件、而它在卡片上被标成了图片"。
type ShowcaseKind string

const (
	// ShowcaseKindAll 是"不筛"，只作为筛选条件出现，不会出现在作品上。
	ShowcaseKindAll   ShowcaseKind = ""
	ShowcaseKindImage ShowcaseKind = "image"
	ShowcaseKindVideo ShowcaseKind = "video"
	ShowcaseKindAudio ShowcaseKind = "audio"
	// ShowcaseKindOther 是无媒体或认不出的类型。只出现在作品上、**不可作为筛选条件**：
	// 「其他」不是一个用户会去找的分类。
	ShowcaseKindOther ShowcaseKind = "other"
)

// ErrBadShowcaseKind 是筛选类型不在白名单里。
//
// ⚠️ 非法值**报错而不是回落成「全部」**：回落的话筛音频却出来一堆视频，
// 而调用方看不出是参数写错了还是真的没有音频作品。
var ErrBadShowcaseKind = errors.New("作品类型只能是 video、image 或 audio")

// ParseShowcaseKind 解析筛选条件：空串 = 不筛；只认 video / image / audio。
func ParseShowcaseKind(s string) (ShowcaseKind, error) {
	switch k := ShowcaseKind(s); k {
	case ShowcaseKindAll, ShowcaseKindImage, ShowcaseKindVideo, ShowcaseKindAudio:
		return k, nil
	default:
		return "", ErrBadShowcaseKind
	}
}

// ContentTypePrefix 是这个类型对应的 MIME 前缀（`video/` 等）；不筛与 other 为空串。
func (k ShowcaseKind) ContentTypePrefix() string {
	switch k {
	case ShowcaseKindImage, ShowcaseKindVideo, ShowcaseKindAudio:
		return string(k) + "/"
	default:
		return ""
	}
}

// KindOfContentType 按 MIME 前缀归类；无媒体（空串）与认不出的一律 other。
//
// 大小写与首尾空白不敏感（`Video/MP4` 也是视频）—— 仓储的筛选条件同样按
// `lower(content_type)` 比，两边口径一致。
func KindOfContentType(ct string) ShowcaseKind {
	ct = strings.ToLower(strings.TrimSpace(ct))
	for _, k := range []ShowcaseKind{ShowcaseKindVideo, ShowcaseKindImage, ShowcaseKindAudio} {
		if strings.HasPrefix(ct, k.ContentTypePrefix()) {
			return k
		}
	}
	return ShowcaseKindOther
}
