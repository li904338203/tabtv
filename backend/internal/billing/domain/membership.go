package domain

import (
	"errors"
	"time"
)

// 会员的纯规则层：有效期怎么算、续费从哪天开始、升级补多少钱（GO-04）。
//
// ⚠️ 这一节的每条规则都直接决定用户**付多少钱**，所以一律不碰数据库、
// 不取当前时间（`now` 由调用方传进来）—— 只有这样才能用表驱动测试
// 把每个边界逐条钉住。钱算错了用户当场就会发现，而争议是不可逆的。

var (
	// ErrDowngrade 不支持直接降级购买（GO-04 原文）。
	//
	// ⚠️ 要**明确拒绝**而不是按续费或升级静默处理：静默按升级算会得出
	// 一个负的差价（等于给用户退钱），静默按续费算会把他的等级悄悄降下去
	// —— 两种都不是他要的，而他要到下个月才发现。
	ErrDowngrade = errors.New("不支持直接降级购买，请等当前会员到期")
	// ErrSameLevelUpgrade 同等级不叫升级，走续费。
	ErrSameLevelUpgrade = errors.New("同等级请走续费")
	// ErrPeriodInvalid 周期取值非法。
	ErrPeriodInvalid = errors.New("套餐周期非法")
)

// 套餐周期。与 `member_plan.period` 的 CHECK 是同一份取值。
const (
	PeriodMonth   = "month"
	PeriodQuarter = "quarter"
	PeriodYear    = "year"
)

// PeriodMonths 周期折合几个自然月。
func PeriodMonths(period string) (int, error) {
	switch period {
	case PeriodMonth:
		return 1, nil
	case PeriodQuarter:
		return 3, nil
	case PeriodYear:
		return 12, nil
	}
	return 0, ErrPeriodInvalid
}

// AddPeriod 从 `from` 起顺延一个周期。
//
// ⚠️ 按**自然月**加而不是按固定天数（30/90/365）：用户在 1 月买了月卡，
// 他理解的到期日是 2 月同一天，而不是 1 月 31 日。按 30 天算的话
// 一年下来他会少拿 5 天，而他自己算得出来这笔账。
//
// ⚠️ Go 的 `AddDate` 会**规范化**溢出的日期：1 月 31 日加一个月得到 3 月 3 日
// （2 月没有 31 日）。这对用户不利也不符合直觉，所以溢出时**夹到当月最后一天**
// —— 1 月 31 日 + 1 个月 = 2 月 28/29 日。
func AddPeriod(from time.Time, period string) (time.Time, error) {
	months, err := PeriodMonths(period)
	if err != nil {
		return time.Time{}, err
	}
	return addMonths(from, months), nil
}

func addMonths(from time.Time, months int) time.Time {
	y, m, d := from.Date()
	// 先按"当月 1 号"加月份，再把日子补回去 —— 这样不会经过 AddDate 的规范化。
	first := time.Date(y, m, 1, from.Hour(), from.Minute(), from.Second(),
		from.Nanosecond(), from.Location()).AddDate(0, months, 0)
	if last := daysIn(first.Year(), first.Month()); d > last {
		d = last
	}
	return time.Date(first.Year(), first.Month(), d, from.Hour(), from.Minute(),
		from.Second(), from.Nanosecond(), from.Location())
}

func daysIn(year int, m time.Month) int {
	// 下个月 1 号往前一天 —— 闰年不用自己判。
	return time.Date(year, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// Membership 是一次会员资格。
type Membership struct {
	LevelID  int64
	StartAt  time.Time
	ExpireAt time.Time
}

// IsActive 当前是否有效。
//
// ⚠️ 有效性是**算出来的**（`now < expireAt`），不是某个作业标出来的。
// 靠作业标记的话，作业停一天就有一整天的人在用已经过期的等级权益，
// 而权益差异直接体现在"能不能调那个贵模型"上。过期作业只负责
// 把行标成 `expired`（为了统计与列表），**判定不依赖它**。
// 这与图片导出的过期、提示词的 ready/stale 是同一条规则。
func (m Membership) IsActive(now time.Time) bool {
	return !m.ExpireAt.IsZero() && now.Before(m.ExpireAt)
}

// RemainingDays 还剩几天（向上取整，至少 0）。
//
// ⚠️ **向上取整**：剩 0.2 天也是"还剩 1 天"。向下取整的话，
// 在最后一天升级的用户会被算成"剩 0 天"、白白损失这一天的差价折抵。
func (m Membership) RemainingDays(now time.Time) int {
	if !m.IsActive(now) {
		return 0
	}
	d := m.ExpireAt.Sub(now)
	days := int(d / (24 * time.Hour))
	if d%(24*time.Hour) > 0 {
		days++
	}
	return days
}

// RenewResult 续费的结果。
type RenewResult struct {
	StartAt  time.Time
	ExpireAt time.Time
	// Extended 是否是在原有效期上顺延（false 表示原来已过期、从现在重新起算）。
	Extended bool
}

// PlanRenew 同等级续费。
//
// ⚠️ **从当前有效期继续延长**（GO-04 原文），不是从今天重新起算：
// 从今天起算的话，用户提前一个月续费就白白损失一个月 ——
// 而"提前续费"恰恰是我们希望他做的事。
//
// 原来已经过期（或从来没买过）时才从 `now` 起算。
func PlanRenew(current *Membership, period string, now time.Time) (RenewResult, error) {
	from := now
	extended := false
	if current != nil && current.IsActive(now) {
		from = current.ExpireAt
		extended = true
	}
	expire, err := AddPeriod(from, period)
	if err != nil {
		return RenewResult{}, err
	}
	start := now
	if extended {
		// 顺延时起始时间保持原样 —— 改成 now 的话会员列表里
		// 会显示"这份会员是今天开始的"，而它其实已经用了半年。
		start = current.StartAt
	}
	return RenewResult{StartAt: start, ExpireAt: expire, Extended: extended}, nil
}

// UpgradeResult 升级的报价。
type UpgradeResult struct {
	// AmountCents 要补的差价（分）。
	AmountCents int64
	// RemainingDays 折算用的剩余天数。
	RemainingDays int
	// ExpireAt 升级后的到期时间 —— **与原来相同**。
	ExpireAt time.Time
	// GiftCredits 赠分按差额处理（GO-04 原文）。
	GiftCredits int
}

// UpgradePlan 是升级报价要用到的两个套餐。
//
// 用同周期的两个套餐算日价差：跨周期比价（月卡 vs 年卡）算出来的
// "日价"完全不是一回事，年卡的日价天然便宜，拿它去折抵会让升级几乎免费。
type UpgradePlan struct {
	LevelID     int64
	LevelSort   int
	Period      string
	PriceCents  int64
	GiftCredits int
}

// PlanUpgrade 升级报价：**维持原到期时间**，按剩余天数与日价差计价并向上取整。
//
// ⚠️ **维持原到期时间**（GO-04 原文）而不是重新起算一个完整周期：
// 重新起算的话用户花一点差价就白得一整个周期，而他付的只是差额。
//
// ⚠️ **向上取整**且用整数分算：`float64` 的差价在分位上会出现
// 0.999999，截断后每次少收一分 —— 一年下来对不上账，而没人查得出来。
//
// ⚠️ **降级明确报错**（见 ErrDowngrade）。等级高低按 `member_level.sort` 比，
// 不按 id：id 是雪花号，先建的等级不一定更低。
func PlanUpgrade(current Membership, from, to UpgradePlan, now time.Time) (
	UpgradeResult, error) {
	if to.LevelSort < from.LevelSort {
		return UpgradeResult{}, ErrDowngrade
	}
	if to.LevelID == from.LevelID {
		return UpgradeResult{}, ErrSameLevelUpgrade
	}
	months, err := PeriodMonths(from.Period)
	if err != nil {
		return UpgradeResult{}, err
	}
	if from.Period != to.Period {
		return UpgradeResult{}, ErrPeriodInvalid
	}
	days := current.RemainingDays(now)
	// 周期的名义天数：按自然月算的话同一个"月卡"在 2 月和 3 月的日价不同，
	// 而用户不会接受"二月升级更贵"。名义天数取 30/月。
	periodDays := months * 30
	if periodDays <= 0 {
		return UpgradeResult{}, ErrPeriodInvalid
	}
	if days > periodDays {
		// 剩余天数不可能超过一个周期（超了说明数据有问题）——
		// 夹住而不是报错：报错会让一个数据异常把用户的升级入口整个堵死。
		days = periodDays
	}

	amount := ceilDiv((to.PriceCents-from.PriceCents)*int64(days), int64(periodDays))
	if amount < 0 {
		// 同 sort 不同价的两个等级（运营配错了）会走到这里。
		// 补 0 而不是退钱 —— 退钱要走退款链路，不能从下单这条路出去。
		amount = 0
	}
	gift := to.GiftCredits - from.GiftCredits
	if gift < 0 {
		gift = 0
	}
	return UpgradeResult{
		AmountCents: amount, RemainingDays: days,
		ExpireAt: current.ExpireAt, GiftCredits: gift,
	}, nil
}

// ceilDiv 向上取整的整数除法（只处理非负除数）。
func ceilDiv(a, b int64) int64 {
	if b == 0 {
		return 0
	}
	if a <= 0 {
		return a / b
	}
	return (a + b - 1) / b
}
