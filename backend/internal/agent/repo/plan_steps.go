package repo

import (
	"context"
	"time"
)

// PlanSteps 取一张已批准计划的 generate 步骤（MKT-T-27）：本会话里带计划项键、在批准之后开始的步骤，
// 按开始时间升序。「停后续」「部分成功」按它们现算（agent/domain.ShouldStop / PlanProgress）。
//
// ⚠️ 以批准时刻划界：条目键在不同的计划里会重复（v1 / v2 …），上一张计划里 v2 的余额不足
// 不能把新计划停掉，上一张里 v1 的成功也不能算进新计划。走 idx_agent_step_plan。
func (r *Repo) PlanSteps(ctx context.Context, sessionID int64, since time.Time) ([]Step, error) {
	return r.querySteps(ctx, `select `+stepCols+` from agent_step
		where session_id = $1 and plan_item_key is not null and tool = 'generate' and started_at >= $2
		order by started_at, id`, sessionID, since)
}
