package domain

import (
	"errors"
	"math"
	"testing"
	"time"
)

var base = time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)

func at(days int) *time.Time {
	t := base.AddDate(0, 0, days)
	return &t
}

func grant(id int64, remaining int, expire *time.Time) Grant {
	return Grant{ID: id, Amount: remaining, Remaining: remaining, ExpireAt: expire}
}

// 先扣快到期的赠送积分，永久购买积分最后扣 —— 反过来排会让用户的赠送积分白白过期。
func TestSortConsumable_先扣将过期的(t *testing.T) {
	grants := []Grant{
		grant(1, 100, nil),    // 永久
		grant(2, 100, at(30)), // 30 天后过期
		grant(3, 100, at(7)),  // 7 天后过期
		grant(4, 100, nil),    // 永久
	}
	got := SortConsumable(grants)
	var ids []int64
	for _, g := range got {
		ids = append(ids, g.ID)
	}
	want := []int64{3, 2, 1, 4} // 近的先、远的次之、永久最后（同为永久按 id）
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("消费顺序应为 %v，实际 %v", want, ids)
		}
	}
}

// 同到期时间用 id 兜底：不确定的顺序会让同一笔扣款重放两次得到不同明细，对账时无法解释。
func TestSortConsumable_同到期时间按id(t *testing.T) {
	same := at(10)
	got := SortConsumable([]Grant{grant(9, 10, same), grant(3, 10, same), grant(5, 10, same)})
	if got[0].ID != 3 || got[1].ID != 5 || got[2].ID != 9 {
		t.Fatalf("同到期应按 id 升序，实际 %v %v %v", got[0].ID, got[1].ID, got[2].ID)
	}
}

func TestPlanConsume_跨批扣款(t *testing.T) {
	grants := []Grant{
		grant(1, 30, at(7)),
		grant(2, 50, at(30)),
		grant(3, 100, nil),
	}
	allocs, err := PlanConsume(grants, 60)
	if err != nil {
		t.Fatal(err)
	}
	want := []Allocation{{1, 30}, {2, 30}}
	if len(allocs) != len(want) {
		t.Fatalf("应跨 2 批，实际 %+v", allocs)
	}
	for i := range want {
		if allocs[i] != want[i] {
			t.Fatalf("期望 %+v，实际 %+v", want, allocs)
		}
	}
	// 永久批次没被动到。
	for _, a := range allocs {
		if a.GrantID == 3 {
			t.Error("还有将过期的额度时不该动永久批次")
		}
	}
}

// 部分扣款会留下一笔对不上任何业务的流水，用户看到扣了钱却没结果。
func TestPlanConsume_余额不足时不产生任何分配(t *testing.T) {
	grants := []Grant{grant(1, 30, at(7)), grant(2, 20, nil)}
	allocs, err := PlanConsume(grants, 60)
	if !errors.Is(err, ErrInsufficient) {
		t.Fatalf("应报余额不足，实际 %v", err)
	}
	if allocs != nil {
		t.Fatalf("不足时不该产生分配，实际 %+v", allocs)
	}
	// 错误信息要能看出差多少。
	if err.Error() == "" || !contains(err.Error(), "60") || !contains(err.Error(), "50") {
		t.Errorf("错误应说明需要与可用，实际 %q", err.Error())
	}
}

func TestPlanConsume_金额非法(t *testing.T) {
	for _, amt := range []int{0, -1, -100} {
		if _, err := PlanConsume([]Grant{grant(1, 100, nil)}, amt); !errors.Is(err, ErrAmountInvalid) {
			t.Errorf("金额 %d 应被拒，实际 %v", amt, err)
		}
	}
}

func TestPlanConsume_恰好用尽(t *testing.T) {
	grants := []Grant{grant(1, 30, at(1)), grant(2, 20, at(2))}
	allocs, err := PlanConsume(grants, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(allocs) != 2 || allocs[0].Amount != 30 || allocs[1].Amount != 20 {
		t.Fatalf("应恰好扣完两批，实际 %+v", allocs)
	}
}

func TestPlanConsume_跳过空批次(t *testing.T) {
	grants := []Grant{
		{ID: 1, Amount: 50, Remaining: 0, ExpireAt: at(1)}, // 已扣完
		grant(2, 40, at(2)),
	}
	allocs, err := PlanConsume(grants, 40)
	if err != nil {
		t.Fatal(err)
	}
	if len(allocs) != 1 || allocs[0].GrantID != 2 {
		t.Fatalf("空批次应被跳过，实际 %+v", allocs)
	}
}

func TestSumRemaining_溢出与负数(t *testing.T) {
	if _, err := SumRemaining([]Grant{{ID: 1, Remaining: -1}}); !errors.Is(err, ErrOverflow) {
		t.Error("负余额应报错")
	}
	big := []Grant{{ID: 1, Remaining: math.MaxInt32 - 1}, {ID: 2, Remaining: 100}}
	if _, err := SumRemaining(big); !errors.Is(err, ErrOverflow) {
		t.Error("求和溢出应报错，不能绕回成负数")
	}
}

// 旧系统的历史 bug：退款一律发永久积分，失败一次任务就把赠送积分洗成了永久积分。
func TestPlanRefund_按原批次回补(t *testing.T) {
	grants := map[int64]Grant{
		1: {ID: 1, Amount: 100, Remaining: 70, ExpireAt: at(7)},
		2: {ID: 2, Amount: 100, Remaining: 80, ExpireAt: nil},
	}
	plan, err := PlanRefund([]Allocation{{1, 30}, {2, 20}}, grants, base)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Total != 50 || plan.Compensate != 0 {
		t.Fatalf("应全额回补原批次，实际 %+v", plan)
	}
	if len(plan.Restore) != 2 || plan.Restore[0].GrantID != 1 || plan.Restore[0].Amount != 30 {
		t.Fatalf("回补目标不对: %+v", plan.Restore)
	}
	for _, r := range plan.Restore {
		if r.Expired {
			t.Error("未过期的批次不该被标记为过期")
		}
	}
}

// 复活等于把用户本该失去的额度又还给他，而且那个批次立刻又会被过期扫描收走。
func TestPlanRefund_过期批次不复活而是补偿(t *testing.T) {
	grants := map[int64]Grant{
		1: {ID: 1, Amount: 100, Remaining: 0, ExpireAt: at(-1)}, // 昨天就过期了
		2: {ID: 2, Amount: 100, Remaining: 80, ExpireAt: at(30)},
	}
	plan, err := PlanRefund([]Allocation{{1, 30}, {2, 20}}, grants, base)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Total != 50 {
		t.Fatalf("总额应为 50，实际 %d", plan.Total)
	}
	if plan.Compensate != 30 {
		t.Fatalf("过期那 30 应走补偿，实际 %d", plan.Compensate)
	}
	if !plan.Restore[0].Expired {
		t.Error("过期批次应被标记")
	}
	if plan.Restore[1].Expired {
		t.Error("未过期批次不该被标记")
	}
}

// 批次被清理掉（查不到）与过期同等处理：都不能凭空回补到一个不存在的批次上。
func TestPlanRefund_批次不存在也走补偿(t *testing.T) {
	plan, err := PlanRefund([]Allocation{{99, 40}}, map[int64]Grant{}, base)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Compensate != 40 {
		t.Fatalf("查不到的批次应走补偿，实际 %d", plan.Compensate)
	}
}

// 恰好在到期时刻：已经过期（到期时间不在未来）。
func TestPlanRefund_到期边界(t *testing.T) {
	exact := base
	grants := map[int64]Grant{1: {ID: 1, Amount: 100, Remaining: 50, ExpireAt: &exact}}
	plan, _ := PlanRefund([]Allocation{{1, 10}}, grants, base)
	if plan.Compensate != 10 {
		t.Error("到期时刻应判为已过期")
	}
}

func TestPlanRefund_回补不得超过发放总额(t *testing.T) {
	grants := map[int64]Grant{1: {ID: 1, Amount: 100, Remaining: 90, ExpireAt: at(7)}}
	if _, err := PlanRefund([]Allocation{{1, 20}}, grants, base); err == nil {
		t.Fatal("回补后会超过发放总额，应报错")
	}
}

func TestCompensationExpiry(t *testing.T) {
	// 有效期从配置读，不写死 —— 这里只验计算。
	got := CompensationExpiry(base, 30*24*time.Hour)
	if got == nil || !got.Equal(base.AddDate(0, 0, 30)) {
		t.Fatalf("30 天后应为 %v，实际 %v", base.AddDate(0, 0, 30), got)
	}
	if CompensationExpiry(base, 0) != nil {
		t.Error("0 表示永久，应返回 nil")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
