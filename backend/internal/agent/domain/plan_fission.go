package domain

import (
	"strings"
)

// 素材裂变（MKT-T-28、MKT-F-53、MKD-10）：以会话的源视频为源，产出换 Hook / 风格 / 画幅 / 开头的变体。
//
// 每个裂变条目在规划时定一条路线，执行时按它组装输入（与报价同源，MKT-D-45）：
//   - video2video：目标模型绑定了 video2video 且**报得出价**（报价即能力判定：绑定 ∧ 启用 ∧
//     能力矩阵 ∧ 有价 ∧ 会员等级，同一份 QuoteWithFacts，不在 agent 里另写模型名单，GAP-22）
//     → 操作改成 video2video，输入恰好 1 个视频（源视频）；该操作只收视频，商品图与角色不发。
//   - regenerate：否则按条目自己的操作重新生成（商品图 + 角色 + 选项），**不发源视频**。
//     不用源视频截出来的帧（video_poster / video_lastframe）当参考：当初封面取的是**最后一帧**，
//     拿它当首帧会让变体从原片的结尾开始（结论见 dev/01 R-8）。2026-10-30 起封面改取第一帧、
//     末帧另存 video_lastframe，上传的视频也截 —— 规则不变：重新生成不发源视频的任何画面。
//
// 卡片上的说明（FissionNote）由服务端按报价写：走哪条、为什么、本项花多少 —— 不靠 Agent 写。

// 裂变路线。
const (
	FissionVideo2Video = "video2video"
	FissionRegenerate  = "regenerate"
)

// OpVideo2Video 是视频生视频的操作码（与 seed 的 operation.code 逐字一致）。
const OpVideo2Video = "video2video"

// Video2VideoItem 是一个裂变条目走 video2video 时的样子：操作 = video2video、输入 = 1 个视频，
// 商品图选择清空（该操作只收视频）。报价、发卡、执行都用这一份。
func Video2VideoItem(it PlanItem, sourceVideoID string) PlanItem {
	it.OperationCode = OpVideo2Video
	it.Inputs = MarketingRoleCounts(0, 0, true)
	it.ProductIndexes, it.ProductMediaIDs, it.ProductTotal, it.ImageLimit = nil, nil, 0, nil
	it.SourceVideoID = sourceVideoID
	it.FissionRoute, it.FissionReason = FissionVideo2Video, ""
	return it
}

// RegenerateItem 是一个裂变条目走「重新生成」时的样子：不发源视频；条目写的是 video2video
// （或没写）时操作退回按产出类型取的默认操作（报价映射里补），由商品图选择照常进行。
func RegenerateItem(it PlanItem, sourceVideoID, reason string) PlanItem {
	if it.OperationCode == OpVideo2Video {
		it.OperationCode = ""
	}
	it.SourceVideoID = sourceVideoID
	it.FissionRoute, it.FissionReason = FissionRegenerate, strings.TrimSpace(reason)
	return it
}

// FissionNote 是裂变条目在卡片上的说明。非裂变条目返回 ""。
//
// ⚠️ 金额取条目上的报价：批准时重新报价后要重写（WithFissionNotes），否则卡片说明里的数
// 与条目金额对不上。
func FissionNote(it PlanItem) string {
	var b strings.Builder
	switch it.FissionRoute {
	case FissionVideo2Video:
		b.WriteString("走「视频生视频」：以源视频为输入改写，不使用商品图与角色")
	case FissionRegenerate:
		b.WriteString("走「重新生成」：")
		if it.FissionReason != "" {
			b.WriteString(strings.TrimRight(it.FissionReason, "。；，"))
			b.WriteString("，")
		}
		b.WriteString("本项按商品图与选项重新生成，不参考源视频画面")
	default:
		return ""
	}
	// tabtv 全部 0 价（TT-T-09）：不再写「本项 ✦N」，只在不可用时说明。
	if !it.Available() {
		b.WriteString("，本项暂不可用")
	}
	return b.String()
}

// WithFissionNotes 按当前报价重写每个裂变条目的说明（返回新切片，不改入参）。
func WithFissionNotes(items []PlanItem) []PlanItem {
	out := make([]PlanItem, len(items))
	for i, it := range items {
		if it.FissionRoute != "" {
			it.FissionNote = FissionNote(it)
		}
		out[i] = it
	}
	return out
}
