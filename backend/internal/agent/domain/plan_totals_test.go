package domain

import "testing"

func TestPlanTotals(t *testing.T) {
	ok := PlanItem{Credits: 60}
	blocked := PlanItem{BlockedCode: "MODEL_UNAVAILABLE"}
	cases := []struct {
		name       string
		items      []PlanItem
		balance    int
		total      int
		short      int
		canApprove bool
	}{
		{"够", []PlanItem{ok, {Credits: 6}}, 100, 66, 0, true},
		{"恰好够", []PlanItem{ok}, 60, 60, 0, true},
		{"差额", []PlanItem{ok}, 50, 60, 10, false},
		{"有不可用项", []PlanItem{ok, blocked}, 1000, 60, 0, false},
		{"空", nil, 1000, 0, 0, false},
	}
	for _, c := range cases {
		total, short, can, valid := PlanTotals(c.items, c.balance)
		if !valid || total != c.total || short != c.short || can != c.canApprove {
			t.Errorf("%s: total=%d short=%d can=%v valid=%v", c.name, total, short, can, valid)
		}
	}
	if _, _, _, valid := PlanTotals([]PlanItem{{Credits: maxPlanCredits}, {Credits: 1}}, 0); valid {
		t.Error("溢出应返回 ok=false")
	}
}
