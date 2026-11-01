package app

import (
	"fmt"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
)

// 挂起（T-4）：等用户（卡片）与等任务（生成）都是「把会话状态写库后结束本次运行」。
// 唤醒靠 API 写 outbox（用户提交卡片 / 发消息）或 Worker 扫描循环（任务终态，AGT-25）。
//
// 本卡只做「落库状态 + 跳出」：卡片由工具自己写（ask_user / submit_plan / 追加额度），
// 工具在 ToolResult.Suspend 里给出迁移事件；Runner 在结算时按迁移表 CAS 推进会话状态。

// suspendEvents 是工具可以要求的挂起事件。其余事件（取消、批准…）不该由工具发起。
var suspendEvents = map[domain.Event]bool{
	domain.EvAskUser:    true,
	domain.EvSubmitPlan: true,
	domain.EvOverBudget: true,
	domain.EvWaitTask:   true,
}

// suspendEvent 校验工具要求的挂起，返回要用的迁移事件。
//
// ⚠️ 不合法（事件不是挂起类、或当前状态不接受）时报错，调用方退回「本轮正常结束」——
// 不能因为一个工具实现的 bug 把会话推进到迁移表外的状态（例如规划态直接进 waiting_task）。
func suspendEvent(from domain.State, s *Suspension) (domain.Event, error) {
	if s == nil {
		return "", fmt.Errorf("没有挂起请求")
	}
	if !suspendEvents[s.Event] {
		return "", fmt.Errorf("工具要求的挂起事件 %q 不是挂起类事件", s.Event)
	}
	if _, ok := domain.CanTransition(from, s.Event); !ok {
		return "", fmt.Errorf("状态 %s 不接受挂起事件 %s", from, s.Event)
	}
	return s.Event, nil
}
