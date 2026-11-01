package domain

import "testing"

func TestSwitchDecision(t *testing.T) {
	cases := []struct {
		name string
		s    SwitchState
		want SwitchVerdict
	}{
		{"全开", SwitchState{true, true, true}, SwitchAllow},
		{"环境变量关", SwitchState{false, true, true}, SwitchNotFound},
		{"环境变量关且工作区关仍是404", SwitchState{false, true, false}, SwitchNotFound},
		{"平台关", SwitchState{true, false, true}, SwitchNotFound},
		// 平台关着时不泄露工作区开没开：一律 404。
		{"平台关且工作区关", SwitchState{true, false, false}, SwitchNotFound},
		{"工作区关", SwitchState{true, true, false}, SwitchWorkspaceDisabled},
		{"全关", SwitchState{false, false, false}, SwitchNotFound},
	}
	for _, c := range cases {
		if got := SwitchDecision(c.s); got != c.want {
			t.Errorf("%s：got %v want %v", c.name, got, c.want)
		}
	}
}
