package domain

import (
	"strings"
	"testing"
	"time"
)

func paidFacts(over func(*RefundFacts)) RefundFacts {
	t0 := ts("2026-06-01T00:00:00Z")
	f := RefundFacts{
		PayStatus: "paid", PaidAt: &t0,
		GrantAmount: 1000, GrantRemaining: 1000, HasGrant: true,
		ExpectCredits: 1000,
	}
	if over != nil {
		over(&f)
	}
	return f
}

func TestCheckRefundable_可退的情况(t *testing.T) {
	now := ts("2026-06-03T00:00:00Z")
	if got := CheckRefundable(paidFacts(nil), now, RefundWindow); got != RefundOK {
		t.Fatalf("窗口内、积分没动过应可退，得到 %q", got)
	}
}

// ⚠️ **顺序有意义**：先判"状态不对"再判"超窗口"再判"用掉了" ——
// 一张早就取消的订单被告知"积分已使用"对用户毫无意义。
func TestCheckRefundable_判定顺序(t *testing.T) {
	now := ts("2026-09-01T00:00:00Z") // 早就超窗口了
	// 状态不对 + 超窗口 + 用掉了 —— 应该报"状态不对"。
	f := paidFacts(func(f *RefundFacts) {
		f.PayStatus = "cancelled"
		f.GrantRemaining = 0
	})
	if got := CheckRefundable(f, now, RefundWindow); got != RefundNotPaid {
		t.Fatalf("应优先报状态不对，得到 %q", got)
	}
	// 超窗口 + 用掉了 —— 应该报超窗口。
	f = paidFacts(func(f *RefundFacts) { f.GrantRemaining = 0 })
	if got := CheckRefundable(f, now, RefundWindow); got != RefundWindowClosed {
		t.Fatalf("应优先报超窗口，得到 %q", got)
	}
}

// ⚠️ **用掉一分就不给退**（GO-04「关联发放批次未使用」）。
// 按比例退的话要回答"用掉的那部分按什么价折算"—— 积分单价随套餐浮动，
// 没有唯一答案。这条保守在**可解释**的一侧。
func TestCheckRefundable_用掉一分就不给退(t *testing.T) {
	now := ts("2026-06-02T00:00:00Z")
	f := paidFacts(func(f *RefundFacts) { f.GrantRemaining = 999 })
	if got := CheckRefundable(f, now, RefundWindow); got != RefundCreditsUsed {
		t.Fatalf("用掉 1 分就该拒，得到 %q", got)
	}
	// 文案要说清**用掉了多少** —— 只说"不能退"用户只会去提工单。
	msg := RefundReasonText(RefundCreditsUsed, f)
	if !strings.Contains(msg, "1000") || !strings.Contains(msg, "1") {
		t.Fatalf("文案应说清发了多少、用了多少：%s", msg)
	}
}

// ⚠️ 这一单本来就不发积分时要**跳过批次判定**：要求它有批次的话，
// 0 赠分的会员单永远退不了，而那是完全正常的一种商品配置。
func TestCheckRefundable_不发积分的单不查批次(t *testing.T) {
	now := ts("2026-06-02T00:00:00Z")
	f := paidFacts(func(f *RefundFacts) {
		f.ExpectCredits = 0
		f.HasGrant = false
		f.GrantAmount, f.GrantRemaining = 0, 0
	})
	if got := CheckRefundable(f, now, RefundWindow); got != RefundOK {
		t.Fatalf("0 赠分的单应可退，得到 %q", got)
	}
	// 而**应该发却没发**的是数据异常，要报出来。
	f.ExpectCredits = 500
	if got := CheckRefundable(f, now, RefundWindow); got != RefundGrantMissing {
		t.Fatalf("应发却没发应报数据异常，得到 %q", got)
	}
}

func TestCheckRefundable_窗口边界(t *testing.T) {
	t0 := ts("2026-06-01T00:00:00Z")
	f := paidFacts(nil)
	// 正好第 7 天的那一刻仍然可退（用 `>` 不是 `>=`）——
	// 边界判反的话用户在最后一天点不动"申请退款"，而界面上写着 7 天。
	if got := CheckRefundable(f, t0.Add(RefundWindow), RefundWindow); got != RefundOK {
		t.Fatalf("正好到窗口末尾应仍可退，得到 %q", got)
	}
	if got := CheckRefundable(f, t0.Add(RefundWindow+time.Second),
		RefundWindow); got != RefundWindowClosed {
		t.Fatalf("超过一秒就该拒，得到 %q", got)
	}
	// 没有支付时间（数据异常）按超窗口处理 —— 放行的话等于无限期可退。
	f.PaidAt = nil
	if got := CheckRefundable(f, t0, RefundWindow); got != RefundWindowClosed {
		t.Fatalf("缺支付时间应拒，得到 %q", got)
	}
}

func TestCheckRefundable_重复申请与过期会员(t *testing.T) {
	now := ts("2026-06-02T00:00:00Z")
	f := paidFacts(func(f *RefundFacts) { f.HasPendingRefund = true })
	if got := CheckRefundable(f, now, RefundWindow); got != RefundAlreadyRequested {
		t.Fatalf("已有退款应拒，得到 %q", got)
	}
	// 会员已经到期 = 整个周期都用完了。
	exp := ts("2026-06-01T12:00:00Z")
	f = paidFacts(func(f *RefundFacts) {
		f.IsMembership = true
		f.MembershipExpireAt = &exp
	})
	if got := CheckRefundable(f, now, RefundWindow); got != RefundMembershipExpired {
		t.Fatalf("会员已过期应拒，得到 %q", got)
	}
	// 还没到期就正常。
	future := ts("2026-07-01T00:00:00Z")
	f.MembershipExpireAt = &future
	if got := CheckRefundable(f, now, RefundWindow); got != RefundOK {
		t.Fatalf("会员未过期应可退，得到 %q", got)
	}
}

// 每个原因码都要有一句能给用户看的话 —— 漏一个的话界面上会出现一个裸码。
func TestRefundReasonText_每个码都有文案(t *testing.T) {
	f := paidFacts(nil)
	for _, code := range []string{
		RefundNotPaid, RefundAlreadyRequested, RefundWindowClosed,
		RefundGrantMissing, RefundCreditsUsed, RefundMembershipExpired,
	} {
		msg := RefundReasonText(code, f)
		if msg == "" || msg == code {
			t.Fatalf("原因码 %q 缺文案（界面上会出现一个裸码）", code)
		}
	}
	if RefundReasonText(RefundOK, f) != "" {
		t.Fatal("可退时不该有文案")
	}
}

// ⚠️ 回收的积分是**批次里还剩的**而不是"应发的"：两者在可退的前提下相等，
// 但写成"应发的"会在将来放开"部分使用也可退"时悄悄把用户没拿到的也扣掉。
func TestPlanOrderRefund(t *testing.T) {
	p := PlanOrderRefund(2900, 1000, true)
	if p.AmountCents != 2900 {
		t.Fatalf("当前政策是全额退，得到 %d", p.AmountCents)
	}
	if p.RevokeCredits != 1000 || !p.RevokeMembership {
		t.Fatalf("回收内容不对：%+v", p)
	}
	// 负数（数据异常）夹到 0 —— 不夹的话会"回收"出一笔负积分。
	if PlanOrderRefund(100, -5, false).RevokeCredits != 0 {
		t.Fatal("负的剩余应夹到 0")
	}
}

// ⚠️ 对账的价值全在 `diff` 那一格，而且**原因必须说出来** ——
// 把结论压成一个布尔的话，可行动的信息就丢了。
func TestReconcileVerdict(t *testing.T) {
	// 两边都说付了、金额一致 → 匹配。
	if st, _ := ReconcileVerdict(true, 2900, true, 2900); st != ReconcileMatched {
		t.Fatalf("一致应为 matched，得到 %s", st)
	}
	// 两边都说没付 → 也是匹配（正常的未支付订单不算差异）。
	if st, _ := ReconcileVerdict(false, 2900, false, 0); st != ReconcileMatched {
		t.Fatalf("都未付应为 matched，得到 %s", st)
	}
	// 金额不一致。
	st, detail := ReconcileVerdict(true, 2900, true, 100)
	if st != ReconcileDiff || !strings.Contains(detail, "2900") ||
		!strings.Contains(detail, "100") {
		t.Fatalf("金额不一致要写清两边的数：%s / %s", st, detail)
	}
	// ⚠️ 最坏的一种：我们以为收到了、渠道说没有 ——
	// 权益已经发出去了而钱可能根本没到账。
	st, detail = ReconcileVerdict(true, 2900, false, 0)
	if st != ReconcileDiff || !strings.Contains(detail, "已履约") {
		t.Fatalf("本地已履约而渠道无此笔要明说：%s / %s", st, detail)
	}
	// 渠道收了钱我们没履约 —— 要提示可以用查单补。
	st, detail = ReconcileVerdict(false, 2900, true, 2900)
	if st != ReconcileDiff || !strings.Contains(detail, "查单") {
		t.Fatalf("渠道已收款而本地未履约要给出路：%s / %s", st, detail)
	}
}
