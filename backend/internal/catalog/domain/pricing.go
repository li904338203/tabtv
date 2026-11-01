package domain

import (
	"fmt"
	"math"
	"sort"
	"unicode/utf8"
)

// Unit 是计价单位（03 §3.2）。
type Unit string

const (
	UnitPerTime   Unit = "per_time"   // 按次
	UnitPerImage  Unit = "per_image"  // 按张
	UnitPerSecond Unit = "per_second" // 按秒
	UnitPerKChar  Unit = "per_kchar"  // 按千字
)

var allUnits = map[Unit]bool{
	UnitPerTime: true, UnitPerImage: true, UnitPerSecond: true, UnitPerKChar: true,
}

func (u Unit) Valid() bool { return allUnits[u] }

// 报价阻塞的细分原因（放在 BizError.Details["reason"]）。
//
// 起因：`CapabilityUnsupported` 一个码同时覆盖「调用方给错了参数」（模型没绑这个操作、
// 输入角色/数量对不上）与「运营没配好」（操作未启用、没声明能力矩阵）。Agent 的 submit_plan
// 要把前者退回给模型自己改、后者照常发卡说明原因（CD 见 agent tools/submit_plan.go）——
// 只按业务码分不开，按文案分又是「改一个字就失效」（§5.9）。所以在出错的地方把细分原因
// 作为机器可读的字段带出去。
const (
	// QuoteBlockReasonKey 是 Details 里放细分原因的键。
	QuoteBlockReasonKey = "reason"
	// QuoteBlockNotBound 模型 × 操作查不到：模型码 / 操作码不存在，或该模型没绑这个操作。
	QuoteBlockNotBound = "model_operation_not_bound"
	// QuoteBlockOperationDisabled 操作或该模型上的绑定被停用（运营处理）。
	QuoteBlockOperationDisabled = "operation_disabled"
	// QuoteBlockCapabilityUndeclared 没有声明输入能力矩阵（运营处理）。
	QuoteBlockCapabilityUndeclared = "capability_undeclared"
	// QuoteBlockInputMismatch 输入角色 / 数量不符合能力矩阵（调用方的输入问题）。
	QuoteBlockInputMismatch = "input_mismatch"
)

// DefaultDurationSec 是 per_second 缺省时长（03 §3.2）。
const DefaultDurationSec = 5

// QuoteInput 是一次报价所需的事实。
type QuoteInput struct {
	Unit Unit
	// Credits 是单价。
	Credits int
	// Count 请求产出数量（多图）。
	Count int
	// DurationSec 视频/音频时长。
	DurationSec int
	// Text 是计费文本。按码点计长度。
	Text string
}

// CalcCredits 按单位算出本次消耗的积分。
//
// ⚠️ 三条容易搞错的规则（03 §3.2）：
//  1. `per_second` 与 `per_kchar` **不再乘 count**。旧实现曾乘过，
//     导致一次多产出请求按 N 倍时长计费。
//  2. `per_kchar` 按 **Unicode 码点**不按字节。Go 的 len(s) 是字节数，
//     中文一个字 3 字节 —— 用它算会让中文用户被多收 3 倍。
//  3. 向上取整且至少 1 个计费单位，避免 0 积分调用。
func CalcCredits(in QuoteInput) (int, error) {
	if !in.Unit.Valid() {
		return 0, fmt.Errorf("未知计价单位: %q", in.Unit)
	}
	if in.Credits < 0 {
		return 0, fmt.Errorf("单价不能为负")
	}
	count := in.Count
	if count <= 0 {
		count = 1
	}

	var units int
	switch in.Unit {
	case UnitPerTime, UnitPerImage:
		units = count
	case UnitPerSecond:
		d := in.DurationSec
		if d <= 0 {
			d = DefaultDurationSec
		}
		units = d
	case UnitPerKChar:
		// utf8.RuneCountInString 才是码点数；len() 是字节数。
		n := utf8.RuneCountInString(in.Text)
		units = int(math.Ceil(float64(n) / 1000.0))
		if units < 1 {
			units = 1
		}
	}

	// 溢出检查：积分是整数，乘法前必须查（GO-19）。
	if in.Credits != 0 && units > math.MaxInt32/in.Credits {
		return 0, fmt.Errorf("计费结果溢出：单价 %d × %d 个单位", in.Credits, units)
	}
	return in.Credits * units, nil
}

// ResolveEstSeconds 取预计耗时。
//
// ⚠️ **操作级优先，回落模型级**（03 §2.3 / 数据模型 §6）。
// 同一个文本模型做纯文本润色十几秒、做图片反推要几分钟，
// 只有模型级 est 会让视觉理解请求一律撞上按短耗时算出的超时墙。
func ResolveEstSeconds(operationEst *int, modelEst int) int {
	if operationEst != nil && *operationEst > 0 {
		return *operationEst
	}
	if modelEst > 0 {
		return modelEst
	}
	return 30
}

// MinDeadlineSec 是超时墙下限。
const MinDeadlineSec = 30

// CalcDeadlineSec 算出任务死线：max(30, est×3)。
//
// ⚠️ deadline 在**受理时计算并存库**（06 §3.3）。
// 恢复扫描不得按当前配置重算旧任务 —— 后台改了估时，
// 历史任务的死线就会漂移，已经在跑的任务可能突然被判超时。
func CalcDeadlineSec(estSeconds int) int {
	d := estSeconds * 3
	if d < MinDeadlineSec {
		return MinDeadlineSec
	}
	return d
}

// Price 是一条价格记录。ModelCode 为空表示 operation 级兜底价。
type Price struct {
	OperationCode string
	ModelCode     string
	Credits       int
	Unit          Unit
	Enabled       bool
}

// IsFallback 兜底价：不绑定具体模型。
func (p Price) IsFallback() bool { return p.ModelCode == "" }

// ResolvePrice 选价：**精确价优先，回落兜底价**。
//
// 两者都没有即无法报价 —— 这正是发布检查里 pricing_missing 要拦的情况，
// 否则用户点生成时才看到错误。
func ResolvePrice(prices []Price, operationCode, modelCode string) (Price, bool) {
	var fallback Price
	var hasFallback bool
	for _, p := range prices {
		if !p.Enabled || p.OperationCode != operationCode {
			continue
		}
		if p.ModelCode == modelCode {
			return p, true
		}
		if p.IsFallback() {
			fallback, hasFallback = p, true
		}
	}
	return fallback, hasFallback
}

// OpTimelineRender 是时间线渲染的操作码（按秒计费，不绑模型）。
const OpTimelineRender = "timeline_render"

// ModellessOperations 是**不绑模型**、只靠兜底价报价的操作（QuoteOperation 只收这些）。
//
// ⚠️ 这张表同时是删价保护的依据：它们没有任何模型绑定，
// 不登记在这里的话，删掉它们的兜底价时"没有模型受影响"，于是放行 ——
// 而那一刻起这项能力就报不出价了。新增一个不绑模型的计费操作时加进来；
// 忘了加的话 QuoteOperation 第一次调用就会报错，而不是悄悄按错的规则报价。
var ModellessOperations = []string{OpTimelineRender}

// IsModelless 判定一个操作是否不绑模型。
func IsModelless(op string) bool {
	for _, o := range ModellessOperations {
		if o == op {
			return true
		}
	}
	return false
}

// LiveBinding 是一个此刻会被报价的（模型, 操作）：模型已启用、绑定已启用。
type LiveBinding struct {
	ModelCode     string
	OperationCode string
}

// MaxPriceCredits 是单价上限：列是 integer，再大就是写库时的 500；
// 而一次生成动辄几十上百积分，单价到这个量级只可能是手滑多打了几个零。
const MaxPriceCredits = 1_000_000

// QuoteLosers 去掉（或停用）target 这条价格之后，哪些在用的模型会**报不出价**。
//
// ⚠️ 判定直接复用 ResolvePrice —— 报价真正用的那条规则，而不是另写一份
// 「有没有精确价 / 有没有兜底价」的推断：两份早晚会不一致，而不一致的方向是
// 「后台说可以删、删完用户那边的模型全部报不出价」（02 §3.3：下架价格不能让
// 已启用的能力失去可用报价）。
// target 本来就是停用的话，去掉它什么都不会变，返回空。
func QuoteLosers(prices []Price, live []LiveBinding, target Price) []string {
	after := make([]Price, 0, len(prices))
	for _, p := range prices {
		if p.OperationCode == target.OperationCode && p.ModelCode == target.ModelCode {
			continue
		}
		after = append(after, p)
	}
	var out []string
	for _, b := range live {
		if b.OperationCode != target.OperationCode {
			continue
		}
		_, before := ResolvePrice(prices, b.OperationCode, b.ModelCode)
		_, still := ResolvePrice(after, b.OperationCode, b.ModelCode)
		if before && !still {
			out = append(out, b.ModelCode)
		}
	}
	sort.Strings(out)
	return out
}

// ValidatePrice 校验一条价格。
func ValidatePrice(p Price) error {
	if p.OperationCode == "" {
		return fmt.Errorf("必须指定 operation")
	}
	if !p.Unit.Valid() {
		return fmt.Errorf("未知计价单位: %q", p.Unit)
	}
	if p.Credits < 0 {
		return fmt.Errorf("单价不能为负")
	}
	if p.Credits > MaxPriceCredits {
		return fmt.Errorf("单价不能超过 %d 积分", MaxPriceCredits)
	}
	return nil
}
