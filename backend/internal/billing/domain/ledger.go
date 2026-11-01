// Package domain 是积分账本的纯规则层：批次选择、退款计划、溢出检查。
//
// 账本是整个系统里最不能出错的一块，因此把"扣哪几批、各扣多少、退回哪里"
// 抽成不碰数据库的纯函数 —— 这些规则必须能用表驱动测试逐条钉住。
//
// ⚠️ 积分**一律整数**，禁止浮点（GO-19）。求和与乘法前查溢出：
// 一个负数余额或一个绕回的总额，比报错严重得多。
package domain

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

var (
	ErrInsufficient  = errors.New("积分不足")
	ErrAmountInvalid = errors.New("金额必须为正整数")
	ErrOverflow      = errors.New("积分数值溢出")
)

// Grant 是一个积分批次。
type Grant struct {
	ID        int64
	Amount    int
	Remaining int
	// ExpireAt 为 nil 表示永久批次。
	ExpireAt *time.Time
	Source   string
}

// IsPermanent 永久批次最后扣。
func (g Grant) IsPermanent() bool { return g.ExpireAt == nil }

// Allocation 是一次消费在某个批次上的扣减。
type Allocation struct {
	GrantID int64
	Amount  int
}

// SortConsumable 按消费顺序排序：**先扣将过期的，永久批次最后扣**。
//
// ⚠️ 这个顺序对用户最有利：赠送积分通常有有效期，先花掉它们，
// 永久购买的积分留到最后。反过来排会让用户的赠送积分白白过期，
// 而他其实一直在消费。
//
// 同到期时间用 id 兜底，保证顺序确定 —— 不确定的顺序会让
// "同一批扣款重放两次得到不同明细"，对账时无法解释。
func SortConsumable(grants []Grant) []Grant {
	out := make([]Grant, len(grants))
	copy(out, grants)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.IsPermanent() != b.IsPermanent() {
			return !a.IsPermanent() // 非永久的排前面
		}
		if !a.IsPermanent() && !a.ExpireAt.Equal(*b.ExpireAt) {
			return a.ExpireAt.Before(*b.ExpireAt)
		}
		return a.ID < b.ID
	})
	return out
}

// PlanConsume 计算一次消费的逐批扣减。
//
// ⚠️ **要么全扣要么不扣**：余额不足时返回 ErrInsufficient 且不产生任何分配。
// 部分扣款会留下一笔对不上任何业务的流水，用户看到扣了钱却没结果。
func PlanConsume(grants []Grant, amount int) ([]Allocation, error) {
	if amount <= 0 {
		return nil, fmt.Errorf("%w: %d", ErrAmountInvalid, amount)
	}
	total, err := SumRemaining(grants)
	if err != nil {
		return nil, err
	}
	if total < amount {
		return nil, fmt.Errorf("%w: 需要 %d，可用 %d", ErrInsufficient, amount, total)
	}

	var out []Allocation
	left := amount
	for _, g := range SortConsumable(grants) {
		if left == 0 {
			break
		}
		if g.Remaining <= 0 {
			continue
		}
		take := g.Remaining
		if take > left {
			take = left
		}
		out = append(out, Allocation{GrantID: g.ID, Amount: take})
		left -= take
	}
	if left != 0 {
		// 到这里说明 SumRemaining 与逐批相加不一致，属于数据损坏，宁可报错。
		return nil, fmt.Errorf("%w: 仍差 %d", ErrInsufficient, left)
	}
	return out, nil
}

// SumRemaining 求可用余额，带溢出检查。
func SumRemaining(grants []Grant) (int, error) {
	total := 0
	for _, g := range grants {
		if g.Remaining < 0 {
			return 0, fmt.Errorf("%w: 批次 %d 余额为负", ErrOverflow, g.ID)
		}
		if total > math.MaxInt32-g.Remaining {
			return 0, ErrOverflow
		}
		total += g.Remaining
	}
	return total, nil
}

// RefundTarget 是退款要回补的一个批次。
type RefundTarget struct {
	GrantID int64
	Amount  int
	// Expired 为真时**不回补原批次**，由调用方改发补偿批次。
	Expired bool
}

// RefundPlan 是一次退款的完整计划。
type RefundPlan struct {
	// Restore 回补到原批次。
	Restore []RefundTarget
	// Compensate 是要新发的补偿总额（原批次已过期的那部分）。
	Compensate int
	Total      int
}

// PlanRefund 按**原消费批次**计算退款。
//
// ⚠️ 两条铁律：
//
//  1. **按原批次回补**，不凭空发永久积分。旧系统的历史 bug 正是把退款一律发成
//     永久积分，于是失败一次任务就把赠送积分洗成了永久积分。
//  2. **已过期的批次不复活**。复活等于把一个用户本该失去的额度又还给他，
//     而且那个批次的到期时间已经过去，复活后立刻又会被过期扫描收走 ——
//     改为新发一条补偿批次，流水里标明来源（GO-25）。
func PlanRefund(details []Allocation, grants map[int64]Grant, now time.Time) (RefundPlan, error) {
	var plan RefundPlan
	for _, d := range details {
		if d.Amount <= 0 {
			return RefundPlan{}, fmt.Errorf("%w: 明细金额 %d", ErrAmountInvalid, d.Amount)
		}
		if plan.Total > math.MaxInt32-d.Amount {
			return RefundPlan{}, ErrOverflow
		}
		plan.Total += d.Amount

		g, ok := grants[d.GrantID]
		expired := !ok || (g.ExpireAt != nil && !g.ExpireAt.After(now))
		if expired {
			plan.Compensate += d.Amount
			plan.Restore = append(plan.Restore, RefundTarget{
				GrantID: d.GrantID, Amount: d.Amount, Expired: true,
			})
			continue
		}
		// ⚠️ 回补不能让 remaining 超过 amount —— 数据库有 CHECK，
		// 但在这里先拦住能给出更清楚的错误。
		if g.Remaining+d.Amount > g.Amount {
			return RefundPlan{}, fmt.Errorf(
				"批次 %d 回补 %d 会超过发放总额（当前 %d / 总额 %d）",
				d.GrantID, d.Amount, g.Remaining, g.Amount)
		}
		plan.Restore = append(plan.Restore, RefundTarget{GrantID: d.GrantID, Amount: d.Amount})
	}
	return plan, nil
}

// CompensationExpiry 计算补偿批次的到期时间。
//
// ⚠️ 有效期是 **PARAM-02，策略未批准**：这里只提供计算，
// 具体天数从配置读，**不得写死**。推荐 30 天的依据是任务失败常发生在
// 用户不在场时，7 天可能在他下次登录前就过期，补偿等于没发。
func CompensationExpiry(now time.Time, validFor time.Duration) *time.Time {
	if validFor <= 0 {
		return nil // 0 表示永久，留给"不设期限"的政策选项
	}
	t := now.Add(validFor)
	return &t
}
