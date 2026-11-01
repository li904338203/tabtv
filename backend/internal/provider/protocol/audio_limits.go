package protocol

import "fmt"

// 非视频协议在扣费之前要查的参数（docs/agent/smartcut SCT-39：配乐、按画面配音效）。
//
// 视频协议有 VideoLimitsDeclaring（时长之外还有清晰度、画幅，报价只在产出是视频时查）。配乐与配音效的产出是音频，
// 它们的上下限原先只写在各家 BuildSubmit 里 —— 那时任务已经受理、积分已经扣了：豆包音乐收到一段 90 秒的请求、
// 可灵收到一段 25 秒的片段，都要扣费、排队、失败、退款走一圈。现在报价与受理在扣费前就问协议（CheckParamsOf），
// 剪辑台的模型清单按同一份声明给出时长范围（DurationRangeOf），确认框据此把时长截到上限并说明。

// DurationDeclaring 由「单次时长有协议层上下限」的非视频协议实现。
type DurationDeclaring interface {
	// DurationRange 某个操作的单次时长范围（秒）；ok=false 表示不声明。
	DurationRange(operation string) (minSec, maxSec int, ok bool)
}

// DurationRangeOf 协议在某操作下的单次时长范围：视频协议取 VideoLimits，其余取 DurationDeclaring。
func DurationRangeOf(protocolCode, operation string) (minSec, maxSec int, ok bool) {
	if lim, ok := VideoLimitsOf(protocolCode, operation); ok {
		return lim.MinSeconds, lim.MaxSeconds, true
	}
	p, found := registry[protocolCode]
	if !found {
		return 0, 0, false
	}
	if d, isD := p.(DurationDeclaring); isD {
		return d.DurationRange(operation)
	}
	return 0, 0, false
}

// ParamChecking 由要在扣费之前校验参数的协议实现。durationSec 是计费时长（受理按区间或参数算好的），
// params 是节点参数（提示词在 `prompt` 里）。
//
// ⚠️ 与 BuildSubmit 用同一份规则（各协议自己的常量）：两边各写一份，迟早出现「报价放行、Worker 拒收」。
type ParamChecking interface {
	CheckParams(operation string, durationSec int, params map[string]any) error
}

// CheckParamsOf 按协议的声明校验参数；协议没实现 ParamChecking 时放行（不知道就别拦）。
func CheckParamsOf(protocolCode, operation string, durationSec int, params map[string]any) error {
	p, ok := registry[protocolCode]
	if !ok {
		return nil
	}
	if c, isC := p.(ParamChecking); isC {
		return c.CheckParams(operation, durationSec, params)
	}
	return nil
}

// checkSeconds 单次时长在不在范围内（0 = 调用方没给，放行：BuildSubmit 会按参数再查一次）。
func checkSeconds(what string, sec, minSec, maxSec int) error {
	if sec == 0 {
		return nil
	}
	if sec < minSec || sec > maxSec {
		return fmt.Errorf("%s单次只能 %d~%d 秒（当前 %d 秒）", what, minSec, maxSec, sec)
	}
	return nil
}
