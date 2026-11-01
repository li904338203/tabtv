package domain

import (
	"errors"
	"math"
)

// ErrBudgetInput 额度判定的入参非法（负数或溢出）。
var ErrBudgetInput = errors.New("额度判定入参非法")

// BudgetDecision 是一次付费工具调用前的额度判定结果。
type BudgetDecision struct {
	Allow bool
	// Shortfall：不放行时还差多少（= used + quote − approved）；放行时为 0。
	Shortfall int
}

// CheckBudget 判断「会话已用 + 本次报价 ≤ 批准额度」（design/01 §6.2，D-3）。
//
//   - approved：会话批准额度（批准即写入，= 规划合计，不加缓冲）；
//   - used：已预留与已扣的合计（agent_budget_entry 中 reserved + charged）；
//   - quote：本次报价，由服务端算，不信模型给的金额。
//
// ⚠️ 积分一律 int，加法前查溢出：溢出后变成负数会让判定悄悄放行。
// ⚠️ 0 积分的调用也走这里（必然放行），不在调用方特判 —— 判断只有一处。
func CheckBudget(approved, used, quote int) (BudgetDecision, error) {
	if approved < 0 || used < 0 || quote < 0 {
		return BudgetDecision{}, ErrBudgetInput
	}
	if used > math.MaxInt-quote {
		return BudgetDecision{}, ErrBudgetInput
	}
	total := used + quote
	if total <= approved {
		return BudgetDecision{Allow: true}, nil
	}
	return BudgetDecision{Shortfall: total - approved}, nil
}
