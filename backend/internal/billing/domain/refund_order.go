package domain

import (
	"fmt"
	"time"
)

// 订单退款的纯规则层（GO-04、P11-03）。
//
// ⚠️ **退钱与回收权益是两件事**，这一节要把它们分开说清楚：
// "退多少钱"是商业政策（PARAM-01，未定稿），
// "能不能退"取决于权益有没有被用掉（这是可以判定的事实）。
// 混在一起的话，一个政策改动会把"已经用掉的积分也退了"这种 bug 带进来。
//
// ⚠️ 与 `ledger.go` 里的 `PlanRefund` **不是一回事**：那个是"任务失败，
// 把扣掉的积分还回去"；这里是"用户买错了，把订单的钱退回去、把发出的权益收回来"。
// 两者都叫 refund，但账务方向与前置条件完全不同 —— 合并的话，
// 一次任务失败会被当成订单退款去调支付渠道。

// RefundWindow 退款窗口：支付后多久内可以申请。
//
// ⚠️ **PARAM-03，策略未批准**：7 天是旧系统的做法，这里当设计建议值。
// 由配置覆盖，**不得在别处写死**。
const RefundWindow = 7 * 24 * time.Hour

// 不可退的原因码。
//
// ⚠️ 分成一个个码而不是一句话：前端要按"用户自己能不能解决"分语气
// （与图片工具栏的 `userFixable` 同一条），而按文案匹配的话
// 改一个字分类就失效，失效的表现是所有提示都退回默认那句。
const (
	RefundOK = ""
	// RefundNotPaid 订单不是已支付状态。
	RefundNotPaid = "not_paid"
	// RefundWindowClosed 超出退款窗口。
	RefundWindowClosed = "window_closed"
	// RefundCreditsUsed 发出去的积分已经用掉了（哪怕只用掉一分）。
	RefundCreditsUsed = "credits_used"
	// RefundGrantMissing 找不到这一单发出的批次（数据异常）。
	RefundGrantMissing = "grant_missing"
	// RefundAlreadyRequested 已经有一笔在处理或已成功的退款。
	RefundAlreadyRequested = "already_requested"
	// RefundMembershipExpired 会员已经到期（用完了整个周期）。
	RefundMembershipExpired = "membership_expired"
)

// RefundFacts 是判定可退性要用到的全部事实。
//
// ⚠️ 全部由调用方查好传进来，这一层**不碰数据库** —— 这些规则每条都决定
// "用户拿不拿得回钱"，必须能用表驱动测试逐条钉住。
type RefundFacts struct {
	PayStatus string
	PaidAt    *time.Time
	// GrantAmount / GrantRemaining 这一单发出的积分批次。
	// ⚠️ `GrantAmount == 0` 表示这一单**本来就不发积分**（比如 0 赠分的会员），
	// 与"找不到批次"是两回事 —— 混为一谈的话前者会被判成数据异常而退不了。
	GrantAmount    int
	GrantRemaining int
	// HasGrant 这一单有没有对应的批次记录。
	HasGrant bool
	// ExpectCredits 订单快照里写的"应发多少积分"。
	ExpectCredits int
	// HasPendingRefund 已经有一笔 pending/success 的退款。
	HasPendingRefund bool
	// IsMembership 会员订单。
	IsMembership bool
	// MembershipExpireAt 这一单买到的会员到期时间。
	MembershipExpireAt *time.Time
}

// CheckRefundable 判定能不能退。返回 `RefundOK` 表示可以。
//
// ⚠️ **顺序有意义**：先判"状态不对"再判"超窗口"再判"用掉了" ——
// 用户最该先看到的是最容易理解的那条。反过来的话，一张早就取消的订单
// 会被告知"积分已使用"，而那句话对他毫无意义。
func CheckRefundable(f RefundFacts, now time.Time, window time.Duration) string {
	if f.PayStatus != "paid" {
		return RefundNotPaid
	}
	if f.HasPendingRefund {
		return RefundAlreadyRequested
	}
	if f.PaidAt == nil || now.Sub(*f.PaidAt) > window {
		return RefundWindowClosed
	}
	// ⚠️ 这一单本来就不发积分时**跳过批次判定**：要求它有批次的话，
	// 0 赠分的会员单永远退不了，而那是完全正常的一种商品配置。
	if f.ExpectCredits > 0 {
		if !f.HasGrant {
			return RefundGrantMissing
		}
		// ⚠️ **用掉一分就不给退**（GO-04「关联发放批次未使用」）。
		// 按比例退的话要回答"用掉的那部分按什么价折算"——
		// 而积分的单价随套餐/活动浮动，没有唯一答案。
		// 这条是保守的，但它保守在**可解释**的一侧。
		if f.GrantRemaining < f.GrantAmount {
			return RefundCreditsUsed
		}
	}
	// 会员单：已经过期说明整个周期都用完了。
	if f.IsMembership && f.MembershipExpireAt != nil &&
		!now.Before(*f.MembershipExpireAt) {
		return RefundMembershipExpired
	}
	return RefundOK
}

// RefundReasonText 把原因码翻成能给用户看的话。
//
// ⚠️ 要说清**为什么**而不只是"不能退"：用户看到"不能退"只会去提工单，
// 而看到"这一单发的 1000 积分已经用掉了 300"他自己就明白了。
func RefundReasonText(code string, f RefundFacts) string {
	switch code {
	case RefundOK:
		return ""
	case RefundNotPaid:
		return "这张订单不是已支付状态，无从退款"
	case RefundAlreadyRequested:
		return "已经有一笔退款在处理或已完成"
	case RefundWindowClosed:
		return fmt.Sprintf("超出退款窗口（支付后 %d 天内可申请）",
			int(RefundWindow.Hours()/24))
	case RefundGrantMissing:
		return "找不到这一单发放的积分批次，请联系客服核对"
	case RefundCreditsUsed:
		return fmt.Sprintf("这一单发放的 %d 积分已使用 %d，不能退款",
			f.GrantAmount, f.GrantAmount-f.GrantRemaining)
	case RefundMembershipExpired:
		return "会员有效期已经结束，不能退款"
	}
	return code
}

// UserFixableRefund 这个原因用户自己能不能解决。
//
// ⚠️ 全都不能 —— 退款的拒绝理由没有一条是用户"再试一次"能绕过的。
// 保留这个函数是为了让前端的语气判断有一个**显式的**依据，
// 而不是靠"这里大概都不能吧"的默认假设（那种假设会在加新原因码时悄悄失效）。
func UserFixableRefund(string) bool { return false }

// OrderRefundPlan 是一次**订单退款**要做的事。
//
// ⚠️ 名字里的 `Order` 不能省：`ledger.go` 里已经有一个 `RefundPlan`，
// 那是"任务失败把积分还回去"。两者都叫 refund 但账务方向完全不同 ——
// 重名的话总有一天会有人把一次任务失败拿去调支付渠道退钱。
type OrderRefundPlan struct {
	// AmountCents 退多少钱。
	AmountCents int64
	// RevokeCredits 要收回多少积分。
	RevokeCredits int
	// RevokeMembership 要不要回收会员资格。
	RevokeMembership bool
}

// PlanOrderRefund 算一次退款的内容。
//
// ⚠️ **当前是全额退**（`AmountCents = 订单金额`）。按剩余天数折算听起来更"公平"，
// 但那需要回答"已用天数按什么价算"——而会员的日价随套餐浮动，
// 且升级单的金额本身就已经是按剩余天数折算过的，再折算一次没有意义。
// **PARAM-01 未定稿**：这里是设计建议值，不是已批准政策。
//
// ⚠️ 回收的积分是**批次里还剩的**而不是"应发的"：两者在可退的前提下相等
// （没用过），但写成"应发的"会在将来放开"部分使用也可退"时悄悄变成
// 把用户没拿到的积分也扣掉。
func PlanOrderRefund(orderCents int64, grantRemaining int, isMembership bool) OrderRefundPlan {
	if grantRemaining < 0 {
		grantRemaining = 0
	}
	return OrderRefundPlan{
		AmountCents:      orderCents,
		RevokeCredits:    grantRemaining,
		RevokeMembership: isMembership,
	}
}

// 退款状态。与 `refund` 表的 CHECK 是同一份取值。
const (
	RefundPending = "pending"
	RefundSuccess = "success"
	RefundFailed  = "failed"
)

// 对账结论。与 `purchase_order.reconcile_status` 的 CHECK 同一份取值。
const (
	ReconcileNone    = "none"
	ReconcilePending = "pending"
	ReconcileMatched = "matched"
	ReconcileDiff    = "diff"
)

// ReconcileVerdict 把"我们以为的"与"渠道说的"比对出一个结论。
//
// ⚠️ 对账的价值全在 `diff` 那一格：`matched` 只是确认没事，
// 而 `diff` 是"有一笔钱两边说法不一样"——那必须有人去看。
// 把两者合成一个布尔的话，`diff` 的**原因**就丢了，而原因才是可行动的信息。
func ReconcileVerdict(localPaid bool, localCents int64,
	channelPaid bool, channelCents int64) (status, detail string) {
	switch {
	case localPaid && channelPaid && localCents == channelCents:
		return ReconcileMatched, ""
	case localPaid && channelPaid:
		// ⚠️ 金额对不上是最要紧的一种：钱数不同意味着某一侧记错了账。
		return ReconcileDiff, fmt.Sprintf(
			"金额不一致：本地 %d 分，渠道 %d 分", localCents, channelCents)
	case localPaid && !channelPaid:
		// ⚠️ **我们以为收到了、渠道说没有** —— 最坏的一种：
		// 权益已经发出去了而钱可能根本没到账。
		return ReconcileDiff, "本地已履约但渠道无此笔收款"
	case !localPaid && channelPaid:
		// 渠道收了钱我们没履约 —— 回调丢了，要补。
		return ReconcileDiff, fmt.Sprintf(
			"渠道已收款 %d 分但本地未履约（回调可能丢了，可用查单补）", channelCents)
	}
	// 两边都说没付：正常的未支付订单，不算差异。
	return ReconcileMatched, ""
}
