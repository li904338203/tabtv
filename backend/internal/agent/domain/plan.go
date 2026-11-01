package domain

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// 营销会话的规划条目（MKT-T-20、MKD-9）：条目键、选图与换图的纯规则。
//
// ⚠️ 付费生成必须对应已批准的计划项（dev/01 §12 #2）：确认卡上的图就是发给上游的图。
// 条目的商品图、输入计数在这里**算一次**，submit_plan 发卡、批准时换图、执行时组装输入（MKT-T-22）
// 用同一个函数 —— 各算一份的话，卡片上的价与真实扣费迟早对不上（MKT-D-45）。

// 工具层错误码（回给模型，不是 HTTP 错误码）。`agent_step.error_code` 记同一组值（MKT-T-22 写入）。
// ⚠️ 判定按码不按文案。too_many_variants / plan_stopped_insufficient_credits 见 MKT-T-27（plan_batch.go）。
const (
	// ToolErrTooManyVariants 规划的变体数超过配置上限（SEEAITV_MARKETING_MAX_VARIANTS）：不出卡。
	ToolErrTooManyVariants = "too_many_variants"
	// ToolErrPlanStopped 同一计划里已有条目因余额不足失败：后续变体不受理、不扣费（MKT-F-51）。
	ToolErrPlanStopped = "plan_stopped_insufficient_credits"
	// ToolErrTooManyImages 计划项选用的商品图超过模型 × 操作的上限：不出卡，回上限让 Agent 重选。
	ToolErrTooManyImages = "too_many_images"
	// ToolErrPlanItemNotApproved generate 引用的条目不在已批准的计划里（MKT-T-22）。
	ToolErrPlanItemNotApproved = "plan_item_not_approved"
	// ToolErrPlanItemMismatch generate 给的模型 / 操作 / 输入与已批准条目不一致（MKT-T-22）。
	ToolErrPlanItemMismatch = "plan_item_mismatch"
	// ToolErrWrongTool 这件事不归 generate 做（成片渲染、分镜、提示词合成…），要换工具（04 文档）。
	// ToolErrSpecUnconfirmed 营销会话规划视频时，画幅 / 清晰度 / 时长里有用户选了「智能」又没确认的项。
	ToolErrSpecUnconfirmed = "video_spec_unconfirmed"
	ToolErrWrongTool       = "wrong_tool"
)

// renderOperations 是「成片渲染」的各种写法（模型会自己造：render / timeline_render / render_timeline）。
var renderOperations = map[string]bool{"render": true, "timeline_render": true, "render_timeline": true}

// planKindTools 是不走 generate 的计划项类型 → 该用的工具（说明给模型）。
var planKindTools = map[string]string{
	"render":     "先用 timeline_assemble 把片段装进时间线（不走脚本时用 nodeIds 按播放顺序列出视频节点），再用 render（给 timelineId，不需要 modelCode）",
	"storyboard": "用 script_storyboard",
	"compose":    "用 script_compose",
	"asset":      "用 script_assets",
}

// WrongToolFor 判断一次 generate 是不是用错了工具：操作码是渲染，或者对应的计划项类型不归 generate。
// 返回给模型的改法；空串 = 没用错。
//
// 起因（实测会话 106895426245885952）：三段视频出好后，模型拿 generate 去「渲染」，先后写了
// operationCode=render / timeline_render / render_timeline、modelCode=render 或空，
// 每次都先建了一个节点、再在报价时失败（「该模型不支持操作 render」）。
func WrongToolFor(operationCode, planKind string) string {
	if renderOperations[strings.TrimSpace(operationCode)] {
		return planKindTools["render"]
	}
	return planKindTools[planKind]
}

// ToolRejection 是工具「按规则拒绝」的结构化结果：没有副作用（不发卡、不扣费），
// 给模型一个可读的错误码与改法所需的数字（如 max）。
//
// Runner 把它原样写成工具结果 `{error: Code, message, ...Details}`；普通 error 只有一句话，
// 模型拿不到「最多几张」这类数字就只能瞎猜着重试。
type ToolRejection struct {
	Code    string
	Message string
	Details map[string]any
}

func (e *ToolRejection) Error() string { return e.Code + "：" + e.Message }

// Content 是回给模型的工具结果（JSON 对象）。Details 里的 error / message 键不会覆盖前两者。
func (e *ToolRejection) Content() json.RawMessage {
	m := make(map[string]any, len(e.Details)+2)
	for k, v := range e.Details {
		m[k] = v
	}
	m["error"], m["message"] = e.Code, e.Message
	b, err := json.Marshal(m)
	if err != nil {
		b, _ = json.Marshal(map[string]string{"error": e.Code, "message": e.Message})
	}
	return b
}

// planKeyPattern 是条目键的格式：模型自己起的短标识（v1 / hero_shot），字母数字下划线连字符。
var planKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// ValidPlanKey 判断条目键格式是否合法。
func ValidPlanKey(k string) bool { return planKeyPattern.MatchString(k) }

// DefaultPlanKey 是模型没给条目键时按位置补的键（第 n 项 → "item{n}"）。
func DefaultPlanKey(n int) string { return fmt.Sprintf("item%d", n) }

// MarketingRoleCounts 是一个营销条目发给上游的输入计数：商品图 + 角色（都是图）+ 源视频。
//
// ⚠️ 与执行时组装的输入同源（MKT-D-45）：报价按它算、能力矩阵按它判，执行时发出的输入也是这几样。
// 零值角色不写进去（纯文生不带空的 image:0，避免通用卡片形状被改变）。
func MarketingRoleCounts(products, characters int, sourceVideo bool) map[string]int {
	out := map[string]int{}
	if n := products + characters; n > 0 {
		out["image"] = n
	}
	if sourceVideo {
		out["video"] = 1
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ProductImageLimit 是一个条目最多能用几张商品图：模型 × 操作的 image 上限减去角色图占用，不小于 0。
func ProductImageLimit(imageMax, characters int) int {
	if n := imageMax - characters; n > 0 {
		return n
	}
	return 0
}

// ProductIndexError 是商品图序号不合法（越界 / 重复 / 超上限）。字段原样带出：
// 模型或用户要据此重选，只说「选多了」的话不知道该减到几张。
type ProductIndexError struct {
	Code  string // out_of_range / duplicate / too_many（与 marketing/domain.SelectImages 的分类逐字一致）
	Index int
	Total int
	Max   int
}

// 商品图序号错误分类。
const (
	ProductIndexOutOfRange = "out_of_range"
	ProductIndexDuplicate  = "duplicate"
	ProductIndexTooMany    = "too_many"
)

func (e *ProductIndexError) Error() string {
	switch e.Code {
	case ProductIndexOutOfRange:
		return fmt.Sprintf("商品图 #%d 不存在：共 %d 张，序号范围 #1~#%d", e.Index, e.Total, e.Total)
	case ProductIndexDuplicate:
		return fmt.Sprintf("商品图 #%d 重复选取", e.Index)
	}
	return fmt.Sprintf("选取的商品图超过本项上限：最多 %d 张", e.Max)
}

// SelectProductIndexes 校验一个条目选用的商品图序号（从 1 起）；为空时按商品槽顺序取前 max 张。
//
// ⚠️ 语义与 marketing/domain.SelectImages **逐条一致**（越界 → 重复 → 超上限，超上限拒绝不截断，
// max ≤ 0 = 不收图）。agent 不能 import marketing，所以这里有一份；两份由 wiring 的
// TestSelectProductIndexes_与营销域SelectImages一致 按同一张表钉在一起，改一边另一边红。
// submit_plan 发卡与确认卡换图（validateOverrides）共用这一份。
func SelectProductIndexes(total int, indexes []int, max int) ([]int, error) {
	if len(indexes) == 0 {
		n := total
		if max < n {
			n = max
		}
		if n < 0 {
			n = 0
		}
		out := make([]int, 0, n)
		for i := 1; i <= n; i++ {
			out = append(out, i)
		}
		return out, nil
	}
	seen := make(map[int]bool, len(indexes))
	for _, i := range indexes {
		if i < 1 || i > total {
			return nil, &ProductIndexError{Code: ProductIndexOutOfRange, Index: i, Total: total, Max: max}
		}
		if seen[i] {
			return nil, &ProductIndexError{Code: ProductIndexDuplicate, Index: i, Total: total, Max: max}
		}
		seen[i] = true
	}
	if len(indexes) > max {
		return nil, &ProductIndexError{Code: ProductIndexTooMany, Total: total, Max: max}
	}
	return append([]int(nil), indexes...), nil
}

// MarketingInputs 是营销条目的输入事实（会话 scene_input 里与报价相关的部分）。
type MarketingInputs struct {
	// Products 会话的商品图 mediaId（有序，[0] 为主图）。
	Products    []string
	Characters  int
	SourceVideo bool
}

// ApplyProductSelection 把一组商品图序号（已校验：从 1 起、在范围内、不重复）写进条目，
// 并按同一份事实重算输入计数（报价、执行同源）。返回新条目，不改入参。
func ApplyProductSelection(it PlanItem, indexes []int, in MarketingInputs) (PlanItem, error) {
	ids := make([]string, 0, len(indexes))
	for _, i := range indexes {
		if i < 1 || i > len(in.Products) {
			return PlanItem{}, fmt.Errorf("商品图 #%d 不存在（共 %d 张）", i, len(in.Products))
		}
		ids = append(ids, in.Products[i-1])
	}
	it.ProductIndexes = append([]int(nil), indexes...)
	it.ProductMediaIDs = ids
	it.ProductTotal = len(in.Products)
	// 源视频只在 video2video 路线上发（MKT-T-28）；那条路线不选商品图、不走这里 ——
	// 会话有源视频不等于每一项都带它（重新生成的条目与普通条目都不发源视频）。
	it.Inputs = MarketingRoleCounts(len(ids), in.Characters, it.FissionRoute == FissionVideo2Video)
	return it, nil
}

// ApplyOverrides 按确认卡上的换图重算条目（ValidateSubmission 已校验过键、范围与上限）。
// changed 表示是否真的有条目被改动（序号与原来一致的覆盖不算改动）。
func ApplyOverrides(items []PlanItem, ovs []ItemOverride, in MarketingInputs) (out []PlanItem, changed bool, err error) {
	byKey := make(map[string][]int, len(ovs))
	for _, o := range ovs {
		byKey[o.Key] = o.ProductIndexes
	}
	out = make([]PlanItem, len(items))
	for i, it := range items {
		idx, ok := byKey[it.Key]
		if !ok || it.Key == "" {
			out[i] = it
			continue
		}
		if it.ProductTotal == 0 {
			return nil, false, fmt.Errorf("条目 %q 不使用商品图", it.Key)
		}
		if !sameInts(idx, it.ProductIndexes) {
			changed = true
		}
		n, err := ApplyProductSelection(it, idx, in)
		if err != nil {
			return nil, false, fmt.Errorf("条目 %q：%w", it.Key, err)
		}
		out[i] = n
	}
	return out, changed, nil
}

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
