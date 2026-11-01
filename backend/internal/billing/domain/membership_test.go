package domain

import (
	"errors"
	"testing"
	"time"
)

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// ⚠️ 按自然月而不是固定 30 天：用户在 1 月买了月卡，他理解的到期日是
// 2 月同一天。按 30 天算的话一年下来他会少拿 5 天，而他自己算得出来这笔账。
func TestAddPeriod_按自然月(t *testing.T) {
	cases := []struct {
		from, period, want string
	}{
		{"2026-01-15T10:00:00Z", PeriodMonth, "2026-02-15T10:00:00Z"},
		{"2026-01-15T10:00:00Z", PeriodQuarter, "2026-04-15T10:00:00Z"},
		{"2026-01-15T10:00:00Z", PeriodYear, "2027-01-15T10:00:00Z"},
	}
	for _, c := range cases {
		got, err := AddPeriod(ts(c.from), c.period)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Equal(ts(c.want)) {
			t.Fatalf("%s + %s 应为 %s，得到 %s", c.from, c.period, c.want, got)
		}
	}
	if _, err := AddPeriod(ts("2026-01-15T10:00:00Z"), "week"); !errors.Is(err, ErrPeriodInvalid) {
		t.Fatal("未知周期应报错")
	}
}

// ⚠️ Go 的 AddDate 会**规范化**溢出日期：1 月 31 日 + 1 个月 = 3 月 3 日
// （2 月没有 31 日）。对用户不利也不符合直觉 —— 要夹到当月最后一天。
func TestAddPeriod_月末不跳到下个月(t *testing.T) {
	// 2026 不是闰年 → 2 月 28 日
	got, _ := AddPeriod(ts("2026-01-31T10:00:00Z"), PeriodMonth)
	if !got.Equal(ts("2026-02-28T10:00:00Z")) {
		t.Fatalf("1/31 + 1 月应为 2/28，得到 %s", got)
	}
	// 2028 是闰年 → 2 月 29 日
	got, _ = AddPeriod(ts("2028-01-31T10:00:00Z"), PeriodMonth)
	if !got.Equal(ts("2028-02-29T10:00:00Z")) {
		t.Fatalf("闰年 1/31 + 1 月应为 2/29，得到 %s", got)
	}
	// 5 月 31 日 + 1 月 = 6 月 30 日（6 月只有 30 天）
	got, _ = AddPeriod(ts("2026-05-31T10:00:00Z"), PeriodMonth)
	if !got.Equal(ts("2026-06-30T10:00:00Z")) {
		t.Fatalf("5/31 + 1 月应为 6/30，得到 %s", got)
	}
	// 不溢出的月份保持原日期
	got, _ = AddPeriod(ts("2026-01-31T10:00:00Z"), PeriodQuarter)
	if !got.Equal(ts("2026-04-30T10:00:00Z")) {
		t.Fatalf("1/31 + 3 月应为 4/30，得到 %s", got)
	}
}

// ⚠️ 有效性是**算出来的**，不是作业标出来的：作业停一天就有一整天的人
// 在用已经过期的等级权益，而权益差异直接体现在"能不能调那个贵模型"上。
func TestMembership_有效性是算出来的(t *testing.T) {
	m := Membership{ExpireAt: ts("2026-03-01T00:00:00Z")}
	if !m.IsActive(ts("2026-02-28T23:59:59Z")) {
		t.Fatal("到期前应有效")
	}
	if m.IsActive(ts("2026-03-01T00:00:00Z")) {
		t.Fatal("到期那一刻应失效")
	}
	if (Membership{}).IsActive(ts("2026-01-01T00:00:00Z")) {
		t.Fatal("零值不该有效")
	}
}

// ⚠️ 剩余天数**向上取整**：剩 0.2 天也是"还剩 1 天"。向下取整的话，
// 在最后一天升级的用户会被算成"剩 0 天"、白白损失这一天的差价折抵。
func TestRemainingDays_向上取整(t *testing.T) {
	m := Membership{ExpireAt: ts("2026-03-10T00:00:00Z")}
	if got := m.RemainingDays(ts("2026-03-01T00:00:00Z")); got != 9 {
		t.Fatalf("整 9 天应为 9，得到 %d", got)
	}
	if got := m.RemainingDays(ts("2026-03-01T12:00:00Z")); got != 9 {
		t.Fatalf("8.5 天应向上取整为 9，得到 %d", got)
	}
	if got := m.RemainingDays(ts("2026-03-09T23:00:00Z")); got != 1 {
		t.Fatalf("最后一小时也算 1 天，得到 %d", got)
	}
	if got := m.RemainingDays(ts("2026-03-11T00:00:00Z")); got != 0 {
		t.Fatalf("已过期应为 0，得到 %d", got)
	}
}

// ⚠️ **从当前有效期继续延长**（GO-04）：从今天重新起算的话，
// 用户提前一个月续费就白白损失一个月 —— 而提前续费恰恰是我们希望他做的。
func TestPlanRenew_顺延不重置(t *testing.T) {
	cur := &Membership{
		StartAt: ts("2026-01-01T00:00:00Z"), ExpireAt: ts("2026-04-01T00:00:00Z"),
	}
	got, err := PlanRenew(cur, PeriodMonth, ts("2026-03-01T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if !got.ExpireAt.Equal(ts("2026-05-01T00:00:00Z")) {
		t.Fatalf("应从原到期日顺延到 5/1，得到 %s", got.ExpireAt)
	}
	if !got.Extended {
		t.Fatal("应标记为顺延")
	}
	// ⚠️ 顺延时起始时间保持原样 —— 改成 now 的话会员列表里会显示
	// "这份会员是今天开始的"，而它其实已经用了两个月。
	if !got.StartAt.Equal(cur.StartAt) {
		t.Fatalf("顺延不该改起始时间，得到 %s", got.StartAt)
	}
}

func TestPlanRenew_过期或从没买过时从现在起算(t *testing.T) {
	now := ts("2026-05-01T00:00:00Z")
	expired := &Membership{
		StartAt: ts("2026-01-01T00:00:00Z"), ExpireAt: ts("2026-04-01T00:00:00Z"),
	}
	got, _ := PlanRenew(expired, PeriodMonth, now)
	if !got.ExpireAt.Equal(ts("2026-06-01T00:00:00Z")) || got.Extended {
		t.Fatalf("已过期应从现在起算：%+v", got)
	}
	got, _ = PlanRenew(nil, PeriodYear, now)
	if !got.ExpireAt.Equal(ts("2027-05-01T00:00:00Z")) || got.Extended {
		t.Fatalf("从没买过应从现在起算：%+v", got)
	}
	if !got.StartAt.Equal(now) {
		t.Fatalf("新买的起始时间应为现在，得到 %s", got.StartAt)
	}
}

func basic(price int64, gift int) UpgradePlan {
	return UpgradePlan{LevelID: 1, LevelSort: 10, Period: PeriodMonth,
		PriceCents: price, GiftCredits: gift}
}

func pro(price int64, gift int) UpgradePlan {
	return UpgradePlan{LevelID: 2, LevelSort: 20, Period: PeriodMonth,
		PriceCents: price, GiftCredits: gift}
}

// ⚠️ **维持原到期时间**（GO-04）而不是重新起算一个完整周期：
// 重新起算的话用户花一点差价就白得一整个周期，而他付的只是差额。
func TestPlanUpgrade_保持原到期时间且按剩余天数折算(t *testing.T) {
	now := ts("2026-03-01T00:00:00Z")
	cur := Membership{LevelID: 1, ExpireAt: ts("2026-03-16T00:00:00Z")} // 剩 15 天
	got, err := PlanUpgrade(cur, basic(3000, 100), pro(9000, 400), now)
	if err != nil {
		t.Fatal(err)
	}
	if !got.ExpireAt.Equal(cur.ExpireAt) {
		t.Fatalf("升级不该改到期时间，得到 %s", got.ExpireAt)
	}
	if got.RemainingDays != 15 {
		t.Fatalf("剩余天数应为 15，得到 %d", got.RemainingDays)
	}
	// (9000-3000) × 15 / 30 = 3000
	if got.AmountCents != 3000 {
		t.Fatalf("差价应为 3000 分，得到 %d", got.AmountCents)
	}
	// 赠分按差额处理（GO-04）。
	if got.GiftCredits != 300 {
		t.Fatalf("赠分应为差额 300，得到 %d", got.GiftCredits)
	}
}

// ⚠️ **向上取整**且用整数分算：float64 的差价在分位上会出现 0.999999，
// 截断后每次少收一分 —— 一年下来对不上账，而没人查得出来。
func TestPlanUpgrade_差价向上取整(t *testing.T) {
	now := ts("2026-03-01T00:00:00Z")
	cur := Membership{LevelID: 1, ExpireAt: ts("2026-03-02T00:00:00Z")} // 剩 1 天
	// (9999-1) × 1 / 30 = 333.27 → 334
	got, err := PlanUpgrade(cur, basic(1, 0), pro(9999, 0), now)
	if err != nil {
		t.Fatal(err)
	}
	if got.AmountCents != 334 {
		t.Fatalf("应向上取整为 334，得到 %d", got.AmountCents)
	}
}

// ⚠️ 降级要**明确拒绝**：静默按升级算会得出负差价（等于给用户退钱），
// 静默按续费算会把他的等级悄悄降下去 —— 两种都不是他要的。
func TestPlanUpgrade_降级与同级要报错(t *testing.T) {
	now := ts("2026-03-01T00:00:00Z")
	cur := Membership{LevelID: 2, ExpireAt: ts("2026-03-16T00:00:00Z")}
	if _, err := PlanUpgrade(cur, pro(9000, 400), basic(3000, 100), now); !errors.Is(err, ErrDowngrade) {
		t.Fatalf("降级应报 ErrDowngrade，得到 %v", err)
	}
	same := UpgradePlan{LevelID: 1, LevelSort: 10, Period: PeriodMonth, PriceCents: 3000}
	if _, err := PlanUpgrade(cur, same, same, now); !errors.Is(err, ErrSameLevelUpgrade) {
		t.Fatalf("同等级应报 ErrSameLevelUpgrade，得到 %v", err)
	}
}

// ⚠️ 跨周期比价（月卡 vs 年卡）算出来的"日价"完全不是一回事 ——
// 年卡的日价天然便宜，拿它去折抵会让升级几乎免费。
func TestPlanUpgrade_跨周期要拒绝(t *testing.T) {
	now := ts("2026-03-01T00:00:00Z")
	cur := Membership{LevelID: 1, ExpireAt: ts("2026-03-16T00:00:00Z")}
	yearly := UpgradePlan{LevelID: 2, LevelSort: 20, Period: PeriodYear, PriceCents: 90000}
	if _, err := PlanUpgrade(cur, basic(3000, 0), yearly, now); !errors.Is(err, ErrPeriodInvalid) {
		t.Fatalf("跨周期升级应被拒，得到 %v", err)
	}
}

// 运营把两个同 sort 的等级配成了不同价：补 0 而不是算出一个负数。
// ⚠️ 退钱要走退款链路，不能从下单这条路出去。
func TestPlanUpgrade_负差价补零(t *testing.T) {
	now := ts("2026-03-01T00:00:00Z")
	cur := Membership{LevelID: 1, ExpireAt: ts("2026-03-16T00:00:00Z")}
	cheaper := UpgradePlan{LevelID: 2, LevelSort: 10, Period: PeriodMonth, PriceCents: 1000}
	got, err := PlanUpgrade(cur, basic(3000, 200), cheaper, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.AmountCents != 0 {
		t.Fatalf("负差价应补 0，得到 %d", got.AmountCents)
	}
	if got.GiftCredits != 0 {
		t.Fatalf("负赠分应补 0，得到 %d", got.GiftCredits)
	}
}

// 剩余天数超过一个周期（数据异常）时夹住而不是报错 ——
// 报错会让一个数据异常把用户的升级入口整个堵死。
func TestPlanUpgrade_剩余天数超周期时夹住(t *testing.T) {
	now := ts("2026-03-01T00:00:00Z")
	cur := Membership{LevelID: 1, ExpireAt: ts("2027-03-01T00:00:00Z")} // 剩 365 天
	got, err := PlanUpgrade(cur, basic(3000, 0), pro(9000, 0), now)
	if err != nil {
		t.Fatal(err)
	}
	if got.RemainingDays != 30 {
		t.Fatalf("应夹到 30 天，得到 %d", got.RemainingDays)
	}
	if got.AmountCents != 6000 {
		t.Fatalf("应按整周期差价 6000，得到 %d", got.AmountCents)
	}
}
