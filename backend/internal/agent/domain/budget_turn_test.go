package domain

import (
	"errors"
	"math"
	"testing"
)

func TestCheckBudget_恰好等于额度放行(t *testing.T) {
	d, err := CheckBudget(60, 40, 20)
	if err != nil || !d.Allow || d.Shortfall != 0 {
		t.Errorf("used+quote == approved 应放行，得到 %+v %v", d, err)
	}
	d, err = CheckBudget(0, 0, 0)
	if err != nil || !d.Allow {
		t.Errorf("0 积分调用应放行，得到 %+v %v", d, err)
	}
}

func TestCheckBudget_超出给出差额(t *testing.T) {
	cases := []struct{ approved, used, quote, short int }{
		{60, 40, 21, 1},
		{60, 60, 20, 20},
		{0, 0, 5, 5},
	}
	for _, c := range cases {
		d, err := CheckBudget(c.approved, c.used, c.quote)
		if err != nil {
			t.Fatal(err)
		}
		if d.Allow || d.Shortfall != c.short {
			t.Errorf("%+v：得到 %+v，期望差额 %d", c, d, c.short)
		}
	}
}

func TestCheckBudget_溢出报错(t *testing.T) {
	cases := [][3]int{
		{math.MaxInt, math.MaxInt, 1},
		{10, 1, math.MaxInt},
		{10, 0, -1},
		{-1, 0, 0},
		{10, -5, 0},
	}
	for _, c := range cases {
		if _, err := CheckBudget(c[0], c[1], c[2]); !errors.Is(err, ErrBudgetInput) {
			t.Errorf("%v 应报 ErrBudgetInput，得到 %v", c, err)
		}
	}
}

func TestDecideTurnSettle_无输出且出错全额退款(t *testing.T) {
	s := DecideTurnSettle(0, errors.New("上游 502"))
	if !s.Refund || s.Status != TurnFailed || s.Reason == "" {
		t.Errorf("得到 %+v", s)
	}
	if !s.CallRefund(3) {
		t.Error("扣过费应调用退款")
	}
}

func TestDecideTurnSettle_空回复也退款(t *testing.T) {
	s := DecideTurnSettle(0, nil)
	if !s.Refund || s.Status != TurnFailed {
		t.Errorf("空回复应退款并 failed，得到 %+v", s)
	}
	// 负数视为没有输出。
	if s := DecideTurnSettle(-1, nil); !s.Refund {
		t.Error("负数有效输出应视为 0")
	}
}

func TestDecideTurnSettle_有输出后出错不退(t *testing.T) {
	for _, err := range []error{nil, errors.New("第 3 次调用超时")} {
		s := DecideTurnSettle(2, err)
		if s.Refund || s.Status != TurnSuccess {
			t.Errorf("err=%v：得到 %+v", err, s)
		}
		if s.CallRefund(3) {
			t.Error("有产出不应调用退款")
		}
	}
}

func TestDecideTurnSettle_零积分轮不调用退款(t *testing.T) {
	s := DecideTurnSettle(0, errors.New("x"))
	if s.CallRefund(0) {
		t.Error("0 积分的轮不能调 Refund（账本会返回 ErrNotFound）")
	}
	if !TriggerUser.Billable() || TriggerSystem.Billable() {
		t.Error("只有用户触发的轮计费")
	}
	for _, st := range []TurnStatus{TurnSuccess, TurnFailed, TurnCancelled} {
		if !st.IsTerminal() {
			t.Errorf("%s 应为终态", st)
		}
	}
	if TurnPending.IsTerminal() || TurnRunning.IsTerminal() {
		t.Error("pending / running 不是终态")
	}
}
