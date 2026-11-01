package app

import (
	"errors"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
)

// StepError 是带步骤标记的工具错误（MKT-T-22）：Runner 把 PlanItemKey / ErrorCode 写进
// agent_step.plan_item_key / error_code，回给模型的内容仍按被包的错误给（*domain.ToolRejection 照常结构化）。
//
// ⚠️ 错误码是**判定依据**（停后续、部分成功按它现算，MKT-T-27），文案只给人看 —— 文案改一个字
// 不能让判定失效。
type StepError struct {
	Err         error
	PlanItemKey string
	ErrorCode   string
}

func (e *StepError) Error() string {
	if e.Err == nil {
		return e.ErrorCode
	}
	return e.Err.Error()
}

func (e *StepError) Unwrap() error { return e.Err }

// stepTagOf 取工具错误上的步骤标记：*StepError 的键与码优先；没有码时取 *domain.ToolRejection 的码。
func stepTagOf(err error) (planItemKey, errorCode string) {
	var se *StepError
	if errors.As(err, &se) {
		planItemKey, errorCode = se.PlanItemKey, se.ErrorCode
	}
	if errorCode == "" {
		var rej *domain.ToolRejection
		if errors.As(err, &rej) {
			errorCode = rej.Code
		}
	}
	return planItemKey, errorCode
}
