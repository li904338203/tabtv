package domain

// 单段 / 多段分流（design/01 §9、AGT-50；2026-10-24 修订，见 design/01 修订块 R-1）。
//
// ⚠️ **一支视频一次生成，不自动拆段**（会话 107419662995685376，用户明确要求「视频不能分开」）：
// 原先目录里「是否支持多镜头」为否、或单次最长时长没配，就一律走多段 —— 而目录里所有视频模型的
// is_multi_shot 都是默认的 false（从没配过），于是每个会话都被要求拆段：一支 15 秒的带货视频被拆成
// 5 秒 + 10 秒两次生成，模型还替这条规则编了个理由「seedance_2_5 不支持一次生成多镜头」。
// 现在：
//   - 单段只看单次最长时长；**多镜头只决定提示词怎么写**（支持就写镜头切换，不支持就写成一个连续镜头里的
//     动作与运镜变化），不再决定拆不拆；
//   - 总时长超过单次上限：不自动拆，由 Agent 用 ask_user 让用户选（缩短 / 换模型 / 明确同意做多段长片）；
//   - 上限没配：先按单段，规划时用 quote 按计划时长报价确认（协议范围在报价时就会拦下越界的时长）。
//
// 分界仍**从模型目录读**（AGT-05 的 max_duration_sec），不写死秒数。

// Segmentation 是分流结论。
type Segmentation string

const (
	// SegmentSingle 一个视频节点、一次生成，多个镜头写进同一段提示词。
	SegmentSingle Segmentation = "single"
	// SegmentMulti 超出单次上限：要用户决定（缩短 / 换模型 / 走脚本线做多段长片），不自动走。
	SegmentMulti Segmentation = "multi"
)

// ModelCaps 是视频模型目录里与分流有关的事实。
type ModelCaps struct {
	// MaxDurationSec 单次最长时长；nil = 后台没配置。
	MaxDurationSec *int
	// MultiShot 一次生成里能不能写多个镜头。只影响提示词的写法，不影响拆不拆。
	MultiShot bool
}

// SingleSegmentLimit 单段能容纳的最长总时长。ok=false：目录没配单次最长时长（不知道上限，不代表要拆）。
func SingleSegmentLimit(caps ModelCaps) (sec int, ok bool) {
	if caps.MaxDurationSec == nil || *caps.MaxDurationSec <= 0 {
		return 0, false
	}
	return *caps.MaxDurationSec, true
}

// DecideSegments 只有「上限已知且总时长超过它」才是多段（而且要用户决定）；其余一律单段 ——
// 上限未知或总时长未知时先按单段规划，越界由报价按协议范围拦下（04 文档 P-61）。
func DecideSegments(totalSec int, caps ModelCaps) Segmentation {
	if limit, ok := SingleSegmentLimit(caps); ok && totalSec > limit {
		return SegmentMulti
	}
	return SegmentSingle
}

// SplitShots 把总时长切成每段不超过 maxSec 的若干段，返回每段秒数（总和 == totalSec）。
//
// 均匀切而不是「先切满再剩一截」：40 秒按 15 秒上限切成 14/13/13 而不是 15/15/10 ——
// 剩下的那一截往往短到撑不起一个镜头，而均匀切出来的每段节奏一致。
// totalSec<=0 返回 nil；maxSec<=0 视为不限（整段一段）。
func SplitShots(totalSec, maxSec int) []int {
	if totalSec <= 0 {
		return nil
	}
	if maxSec <= 0 || totalSec <= maxSec {
		return []int{totalSec}
	}
	n := (totalSec + maxSec - 1) / maxSec
	base, extra := totalSec/n, totalSec%n
	out := make([]int, n)
	for i := range out {
		out[i] = base
		if i < extra {
			out[i]++
		}
	}
	return out
}
