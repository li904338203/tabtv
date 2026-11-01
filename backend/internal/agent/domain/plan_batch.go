package domain

import (
	"fmt"
	"sort"
)

// 批量变体（MKT-T-27、MKD-10、MKT-F-50~52）：一张确认卡上的多个变体条目。
//
// 规则照抄组节点（AV-22~24），实现落在 agent（营销批量没有镜头，不复用脚本域的组节点）：
//   - 变体数 ≤ 配置上限（SEEAITV_MARKETING_MAX_VARIANTS，默认 4）：规划时不出卡、批准时兜底拒。
//   - 每个变体独立受理、独立计费；余额不足**停掉后续**而不是逐个失败。
//   - 部分成功不摊平；失败的变体单独重试 = 新步骤、新幂等键（PaidIdempotencyKey 带步骤 ID）。
//
// ⚠️ 「停后续」「部分成功」都是**按步骤现算**的（agent_step 的 plan_item_key + error_code），
// 不打标记：标记式的正确性依赖每个改动点都记得改，漏一处不会报错 —— 症状是余额不足之后
// Agent 又提交了两个变体、两次都失败（CD-36 同一条）。

// DefaultMaxVariants 是变体上限的缺省值（配置没给或给了非正数时用）。
const DefaultMaxVariants = 4

// StepErrInsufficientCredits 是受理侧「余额不足」的错误码名（errcode.InsufficientCredits.Name()，
// generate 写进 agent_step.error_code）。domain 不依赖 transport，这里有一份字面量，
// 由 tools 包的测试钉在一起（改一边另一边红）。
const StepErrInsufficientCredits = "INSUFFICIENT_CREDITS"

// MaxVariantsOr 把配置值规整成可用的上限（≤ 0 → DefaultMaxVariants）。
func MaxVariantsOr(n int) int {
	if n <= 0 {
		return DefaultMaxVariants
	}
	return n
}

// CountVariants 是计划里不同 variant 的个数（variant=0 = 不是批量变体，不计）。
//
// ⚠️ 按不同的 variant 计，不按条目数：一个变体可以有好几个条目（如同一组合的图 + 视频）。
func CountVariants(items []PlanItem) int {
	seen := map[int]bool{}
	for _, it := range items {
		if it.Variant > 0 {
			seen[it.Variant] = true
		}
	}
	return len(seen)
}

// ValidateVariants 校验变体数不超过上限（max ≤ 0 按缺省）。超过时返回 too_many_variants 的结构化拒绝：
// 消息里的数字取配置，不写死 4 —— 改了环境变量而提示还是旧数，用户会照着错的数去改。
func ValidateVariants(items []PlanItem, max int) error {
	max = MaxVariantsOr(max)
	n := CountVariants(items)
	if n <= max {
		return nil
	}
	return &ToolRejection{Code: ToolErrTooManyVariants,
		Message: fmt.Sprintf("单次最多 %d 个变体（本次规划了 %d 个）", max, n),
		Details: map[string]any{"max": max, "variants": n}}
}

// 计划项执行状态（PlanProgress 的逐条结果）。
const (
	PlanItemPending   = "pending"   // 还没有提交过
	PlanItemSubmitted = "submitted" // 已受理、任务还在跑
	PlanItemSuccess   = "success"
	PlanItemFailed    = "failed"
	PlanItemStopped   = "stopped" // 因余额不足停掉（没有提交、没有扣费）
)

// 计划执行汇总。⚠️ partial_success 是独立状态，不摊平成 success 或 failed（AV-24）：
// 显示成功的话用户不会去翻那条失败的，显示失败的话他会把已经成功的也重跑一遍 —— 两种都在花他的钱。
const (
	PlanProgressPending        = "pending" // 一条都还没开始
	PlanProgressRunning        = "running"
	PlanProgressPartialSuccess = "partial_success"
	PlanProgressSuccess        = "success"
	PlanProgressFailed         = "failed"
)

// 步骤状态（与 agent_step 的 ck_agent_step_status 逐字一致；domain 不依赖 repo，这里只认字面量）。
// failed / cancelled 都按「这一次没成」处理，不单列。
const (
	stepRunning = "running"
	stepWaiting = "waiting"
	stepSuccess = "success"
)

// PlanStep 是本计划里一次 generate 步骤的事实（agent_step：按开始时间升序）。
type PlanStep struct {
	Key       string
	Status    string // running / waiting / success / failed / cancelled
	ErrorCode string
}

// latestByKey 是每个条目键最近一次步骤（steps 已按开始时间升序）。
func latestByKey(steps []PlanStep) map[string]PlanStep {
	out := map[string]PlanStep{}
	for _, s := range steps {
		if s.Key != "" {
			out[s.Key] = s
		}
	}
	return out
}

// ShouldStop 判断 generate 条目 key 时是否该停：除 key 自己以外，有条目**最近一次**步骤
// 因余额不足失败（error_code = INSUFFICIENT_CREDITS，**按码不按文案**）。返回停掉后续的那个条目键。
//
//   - 只看最近一次：那个条目充值后重试成功，停止就自然解除 —— 现算的好处，不需要谁去「解除标记」。
//   - 不拦 key 自己：被余额挡住的那一项本身必须能重试，否则充值之后整张计划再也走不动。
//   - 被停掉的步骤（plan_stopped_insufficient_credits）不传染：它们不是余额不足的那一项。
func ShouldStop(steps []PlanStep, key string) (stoppedBy string, stop bool) {
	latest := latestByKey(steps)
	keys := make([]string, 0, len(latest))
	for k := range latest {
		keys = append(keys, k)
	}
	sort.Strings(keys) // 多个时给出确定的一个
	for _, k := range keys {
		if k != key && latest[k].ErrorCode == StepErrInsufficientCredits {
			return k, true
		}
	}
	return "", false
}

// PlanItemProgress 是一个计划项的执行状态。
type PlanItemProgress struct {
	Key          string
	Label        string
	Variant      int
	VariantLabel string
	State        string
	// ErrorCode 最近一次失败的错误码（只在 failed / stopped 时给）。
	ErrorCode string
	// Attempts 这一项提交过几次（含被拒的；重试即多一次）。
	Attempts int
}

// PlanProgressView 是一张已批准计划的执行汇总（会话详情 planProgress，现算）。
type PlanProgressView struct {
	Status    string
	Total     int
	Succeeded int
	Failed    int
	Stopped   int
	Running   int
	Pending   int
	Items     []PlanItemProgress
}

// PlanProgress 按步骤现算每个条目的状态与汇总。只算带条目键的条目（营销条目）；一条都没有时返回 false。
//
// 单条：有在跑的 → submitted；否则有成功过的 → success；否则看最近一次：余额停止 → stopped、
// 失败 / 取消 / 被拒 → failed；没有步骤 → pending（若计划已因余额不足停止 → stopped）。
//
// 汇总优先级同 AV-24：还在跑 > 有成有败 > 全成 > 全败 > 还没开始。
// open=false（会话已不在执行：空闲 / 失败 / 取消 / 回到规划）时，没提交的条目不再算「还在跑」——
// 否则 Agent 结束了而漏了一项的计划会永远显示「执行中」。
func PlanProgress(items []PlanItem, steps []PlanStep, open bool) (PlanProgressView, bool) {
	byKey := map[string][]PlanStep{}
	for _, s := range steps {
		if s.Key != "" {
			byKey[s.Key] = append(byKey[s.Key], s)
		}
	}
	_, stopped := ShouldStop(steps, "")
	var v PlanProgressView
	for _, it := range items {
		if it.Key == "" {
			continue
		}
		p := PlanItemProgress{Key: it.Key, Label: it.Label, Variant: it.Variant, VariantLabel: it.VariantLabel}
		ss := byKey[it.Key]
		p.Attempts = len(ss)
		p.State = itemState(ss, stopped)
		if p.State == PlanItemFailed || p.State == PlanItemStopped {
			if len(ss) > 0 {
				p.ErrorCode = ss[len(ss)-1].ErrorCode
			}
		}
		switch p.State {
		case PlanItemSuccess:
			v.Succeeded++
		case PlanItemFailed:
			v.Failed++
		case PlanItemStopped:
			v.Stopped++
		case PlanItemSubmitted:
			v.Running++
		default:
			v.Pending++
		}
		v.Items = append(v.Items, p)
	}
	if len(v.Items) == 0 {
		return PlanProgressView{}, false
	}
	v.Total = len(v.Items)
	v.Status = progressStatus(v, open)
	return v, true
}

func itemState(ss []PlanStep, planStopped bool) string {
	if len(ss) == 0 {
		if planStopped {
			return PlanItemStopped
		}
		return PlanItemPending
	}
	succeeded := false
	for _, s := range ss {
		switch s.Status {
		case stepRunning, stepWaiting:
			return PlanItemSubmitted
		case stepSuccess:
			succeeded = true
		}
	}
	if succeeded {
		return PlanItemSuccess
	}
	if ss[len(ss)-1].ErrorCode == ToolErrPlanStopped {
		return PlanItemStopped
	}
	return PlanItemFailed
}

func progressStatus(v PlanProgressView, open bool) string {
	bad := v.Failed + v.Stopped
	switch {
	case v.Running > 0:
		return PlanProgressRunning
	case v.Pending > 0 && open:
		if v.Succeeded == 0 && bad == 0 {
			return PlanProgressPending
		}
		return PlanProgressRunning
	case v.Succeeded > 0 && (bad > 0 || v.Pending > 0):
		return PlanProgressPartialSuccess
	case v.Succeeded == v.Total:
		return PlanProgressSuccess
	case bad > 0:
		return PlanProgressFailed
	}
	return PlanProgressPending
}

// SessionOpen 判断会话是否还在执行这张计划（PlanProgress 的 open）：执行中 / 等任务 / 等追加额度或输入。
func SessionOpen(st State) bool {
	switch st {
	case StateExecuting, StateWaitingTask, StateAwaitingApproval, StateAwaitingInput:
		return true
	}
	return false
}
