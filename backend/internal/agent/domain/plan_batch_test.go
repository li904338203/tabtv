package domain

import (
	"errors"
	"strings"
	"testing"
)

func variantItems(variants ...int) []PlanItem {
	out := make([]PlanItem, 0, len(variants))
	for i, v := range variants {
		out = append(out, PlanItem{Kind: "video", Key: "v" + string(rune('1'+i)), Variant: v,
			Label: "变体", Quantity: 5, Unit: "second"})
	}
	return out
}

func TestValidateVariants_4过5拒(t *testing.T) {
	if err := ValidateVariants(variantItems(1, 2, 3, 4), 4); err != nil {
		t.Fatalf("4 个变体应通过：%v", err)
	}
	err := ValidateVariants(variantItems(1, 2, 3, 4, 5), 4)
	var rej *ToolRejection
	if !errors.As(err, &rej) || rej.Code != ToolErrTooManyVariants {
		t.Fatalf("5 个变体应 too_many_variants：%v", err)
	}
	if !strings.Contains(rej.Message, "单次最多 4 个变体") || rej.Details["max"] != 4 || rej.Details["variants"] != 5 {
		t.Fatalf("消息与数字：%+v", rej)
	}
	// 数字取配置，不写死 4。
	if err := ValidateVariants(variantItems(1, 2, 3, 4, 5), 6); err != nil {
		t.Fatalf("上限 6 时 5 个应通过：%v", err)
	}
	if err := ValidateVariants(variantItems(1, 2, 3), 2); err == nil || !strings.Contains(err.Error(), "单次最多 2 个变体") {
		t.Fatalf("上限 2：%v", err)
	}
	// 配置非正数按缺省 4。
	if err := ValidateVariants(variantItems(1, 2, 3, 4, 5), 0); err == nil {
		t.Fatal("上限 0 应按缺省 4 拒绝 5 个")
	}
	// 按不同 variant 计：同一变体的多个条目只算一个；variant=0 不计。
	if err := ValidateVariants(variantItems(1, 1, 2, 2, 3, 3, 4, 4, 0, 0), 4); err != nil {
		t.Fatalf("4 个不同变体（各两项）应通过：%v", err)
	}
}

func TestShouldStop_按error_code不按文案(t *testing.T) {
	steps := []PlanStep{
		{Key: "v1", Status: "waiting"},
		{Key: "v2", Status: "failed", ErrorCode: StepErrInsufficientCredits},
	}
	if by, stop := ShouldStop(steps, "v3"); !stop || by != "v2" {
		t.Fatalf("v2 余额不足，v3 应停：%q %v", by, stop)
	}
	// 被余额挡住的那一项自己可以重试。
	if _, stop := ShouldStop(steps, "v2"); stop {
		t.Fatal("v2 自己的重试不该被停")
	}
	// 文案里写着「余额不足」但码不是：不停。
	other := []PlanStep{{Key: "v2", Status: "failed", ErrorCode: "PROVIDER_FAILED"}}
	if _, stop := ShouldStop(other, "v3"); stop {
		t.Fatal("非余额不足的失败不停后续")
	}
	// 只看最近一次：v2 充值后重试成功，停止自然解除。
	retried := append(append([]PlanStep(nil), steps...), PlanStep{Key: "v2", Status: "waiting"})
	if _, stop := ShouldStop(retried, "v3"); stop {
		t.Fatal("v2 重试已受理，停止应解除")
	}
	// 被停掉的步骤不传染。
	chain := []PlanStep{{Key: "v3", Status: "failed", ErrorCode: ToolErrPlanStopped}}
	if _, stop := ShouldStop(chain, "v4"); stop {
		t.Fatal("plan_stopped 的步骤不是余额不足的那一项")
	}
	if _, stop := ShouldStop(nil, "v1"); stop {
		t.Fatal("没有步骤不停")
	}
}

func TestPlanProgress_部分成功不摊平(t *testing.T) {
	items := variantItems(1, 2, 3)
	steps := []PlanStep{
		{Key: "v1", Status: "success"},
		{Key: "v2", Status: "failed", ErrorCode: "PROVIDER_FAILED"},
		{Key: "v3", Status: "success"},
	}
	v, ok := PlanProgress(items, steps, true)
	if !ok || v.Status != PlanProgressPartialSuccess || v.Succeeded != 2 || v.Failed != 1 || v.Total != 3 {
		t.Fatalf("部分成功（2/3）：%+v", v)
	}
	if v.Items[1].State != PlanItemFailed || v.Items[1].ErrorCode != "PROVIDER_FAILED" || v.Items[1].Attempts != 1 {
		t.Fatalf("失败那条带错误码：%+v", v.Items[1])
	}
	// 失败的重试成功 → 全成；重试中 → 还在跑。
	retry := append(append([]PlanStep(nil), steps...), PlanStep{Key: "v2", Status: "waiting"})
	if v, _ := PlanProgress(items, retry, true); v.Status != PlanProgressRunning || v.Items[1].Attempts != 2 {
		t.Fatalf("重试中应 running：%+v", v)
	}
	retry[3].Status = "success"
	if v, _ := PlanProgress(items, retry, true); v.Status != PlanProgressSuccess {
		t.Fatalf("重试成功应 success：%+v", v)
	}
	// 余额不足停后续：v2 failed（余额）、v3 没提交 → stopped；汇总部分成功。
	stop := []PlanStep{{Key: "v1", Status: "success"},
		{Key: "v2", Status: "failed", ErrorCode: StepErrInsufficientCredits}}
	v, _ = PlanProgress(items, stop, true)
	if v.Status != PlanProgressPartialSuccess || v.Items[2].State != PlanItemStopped || v.Stopped != 1 || v.Failed != 1 {
		t.Fatalf("余额停后续：%+v", v)
	}
	stop = append(stop, PlanStep{Key: "v3", Status: "failed", ErrorCode: ToolErrPlanStopped})
	if v, _ := PlanProgress(items, stop, true); v.Items[2].State != PlanItemStopped ||
		v.Items[2].ErrorCode != ToolErrPlanStopped {
		t.Fatalf("被停掉的提交也是 stopped：%+v", v.Items[2])
	}
	// 没有条目键（通用卡片）→ 没有进度。
	if _, ok := PlanProgress([]PlanItem{{Kind: "video", Label: "x"}}, nil, true); ok {
		t.Fatal("通用条目不给进度")
	}
}

func TestPlanProgress_聚合优先级(t *testing.T) {
	items := variantItems(1, 2, 3)
	cases := []struct {
		name  string
		steps []PlanStep
		open  bool
		want  string
	}{
		{"还没开始", nil, true, PlanProgressPending},
		{"还在跑优先于有成有败", []PlanStep{{Key: "v1", Status: "success"}, {Key: "v2", Status: "failed"},
			{Key: "v3", Status: "waiting"}}, true, PlanProgressRunning},
		{"有成有败", []PlanStep{{Key: "v1", Status: "success"}, {Key: "v2", Status: "failed"},
			{Key: "v3", Status: "cancelled"}}, true, PlanProgressPartialSuccess},
		{"全成", []PlanStep{{Key: "v1", Status: "success"}, {Key: "v2", Status: "success"},
			{Key: "v3", Status: "success"}}, true, PlanProgressSuccess},
		{"全败", []PlanStep{{Key: "v1", Status: "failed"}, {Key: "v2", Status: "failed"},
			{Key: "v3", Status: "failed"}}, true, PlanProgressFailed},
		{"成功一条其余未提交且会话还在执行 = 还在跑", []PlanStep{{Key: "v1", Status: "success"}}, true,
			PlanProgressRunning},
		{"成功一条其余未提交而会话已结束 = 部分成功", []PlanStep{{Key: "v1", Status: "success"}}, false,
			PlanProgressPartialSuccess},
		{"失败一条其余未提交而会话已结束 = 失败", []PlanStep{{Key: "v1", Status: "failed"}}, false,
			PlanProgressFailed},
		{"成功过的条目后来被拒仍算成功", []PlanStep{{Key: "v1", Status: "success"},
			{Key: "v1", Status: "failed", ErrorCode: ToolErrPlanItemMismatch}, {Key: "v2", Status: "success"},
			{Key: "v3", Status: "success"}}, true, PlanProgressSuccess},
	}
	for _, c := range cases {
		v, _ := PlanProgress(items, c.steps, c.open)
		if v.Status != c.want {
			t.Errorf("%s：%s，期望 %s（%+v）", c.name, v.Status, c.want, v)
		}
	}
}

func TestSessionOpen(t *testing.T) {
	for st, want := range map[State]bool{StateExecuting: true, StateWaitingTask: true, StateAwaitingApproval: true,
		StateIdle: false, StatePlanning: false, StateFailed: false, StateCancelled: false} {
		if SessionOpen(st) != want {
			t.Errorf("%s：%v", st, !want)
		}
	}
}
