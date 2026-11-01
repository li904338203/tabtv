package domain

// 不可用原因（MKT-F-19）。全部 userFixable：用户换个模型、换个技能、补一张图就能解决。
// ⚠️ 前端按 code / userFixable 决定语气，**不匹配文案**（CD-48 同一条）。
const (
	BlockOutputMismatch = "output_mismatch"
	BlockNeedsAudio     = "needs_audio_model"
	BlockNeedsProduct   = "needs_product"
	// 其余必填槽位按 "needs_" + 槽位 key 给出（needs_character / needs_source_video）。
)

// UsageContext 是判定「当前能不能用这个选项」的上下文。
type UsageContext struct {
	// OutputType 是当前产出（video / image）；空 = 还不确定，不判。
	OutputType string
	// ModelHasAudio：所选模型是否出声。⚠️ nil = 模型未确定（「智能匹配」），**不判** ——
	// 判成不出声的话所有抓耳音效类 Hook 在默认状态下全是灰的，而智能匹配完全可能选中出声模型。
	ModelHasAudio *bool
	// Slots 是各槽位已填的数量。
	Slots map[string]int
}

// BlockReason 给出选项在当前上下文里不可用的原因；可用时返回 ("", false)。
//
// 不可用的选项**照样列出并说明原因**（MKT-F-19），所以这里只回答「为什么」，不负责过滤。
// 多个原因同时成立时给**最根本**的那一条：产出不匹配（换技能）→ 不出声（换模型）→ 缺输入（补图）。
// 先说「需要商品图」的话，用户补完图发现还是灰的。
func BlockReason(o Option, ctx UsageContext) (code string, userFixable bool) {
	if ctx.OutputType != "" && len(o.OutputTypes) > 0 && !contains(o.OutputTypes, ctx.OutputType) {
		return BlockOutputMismatch, true
	}
	if o.IsAudioRequired && ctx.ModelHasAudio != nil && !*ctx.ModelHasAudio {
		return BlockNeedsAudio, true
	}
	for _, k := range SlotKeys {
		if o.InputRequirements[k] == RequirementRequired && ctx.Slots[k] <= 0 {
			return "needs_" + k, true
		}
	}
	return "", false
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
