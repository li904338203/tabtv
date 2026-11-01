package domain

import "time"

// LifecycleState 是选项在某一刻的投放状态（管理端列表的「未开始 / 在投 / 已结束 / 已停用」）。
type LifecycleState string

const (
	LifecycleNotStarted LifecycleState = "not_started"
	LifecycleLive       LifecycleState = "live"
	LifecycleEnded      LifecycleState = "ended"
	LifecycleDisabled   LifecycleState = "disabled"
)

// Visible 判断选项此刻是否在投：`is_enabled ∧ (start 空 ∨ start ≤ now) ∧ (end 空 ∨ now < end)`。
//
// ⚠️ 投放状态是**算出来的**，不是作业翻开关翻出来的（MKD-13、CD-61）：作业停一天，
// 模板墙上就有一整天的过期活动。与 Banner（`content/domain.BannerSchedule.ActiveAt`）同式，
// repo 里的 SQL 谓词是它的同义写法 —— 列表、详情、封面公开取图、建会话校验、
// 钉版本**全部用这一个判定**，各写一份就会出现「列表里没有但封面直链还能打开」。
//
// ⚠️ 边界取 `[start, end)`：开始那一刻就投、结束那一刻就停。两端都闭的话，
// "到 12:00 结束"的那条会在 12:00:00 与下一条同时出现。
func Visible(o Option, now time.Time) bool {
	return Lifecycle(o, now) == LifecycleLive
}

// Lifecycle 给出此刻的投放状态，`live` 当且仅当 Visible 为真。停用恒为 `disabled`
// （不论时段）。⚠️ 由服务端算、随列表下发，不让管理端拿本地时钟再算一遍。
func Lifecycle(o Option, now time.Time) LifecycleState {
	switch {
	case !o.IsEnabled:
		return LifecycleDisabled
	case o.StartAt != nil && now.Before(*o.StartAt):
		return LifecycleNotStarted
	case o.EndAt != nil && !now.Before(*o.EndAt):
		return LifecycleEnded
	}
	return LifecycleLive
}
