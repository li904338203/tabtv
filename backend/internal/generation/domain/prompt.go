package domain

import "strings"

// 提示词的拼接顺序。
//
// ⚠️ **顺序不可换**：`上游文本 → 本节点 prompt → 预设版式与微调 → 导演台约束段 → 营销片段 → 风格/特效素材词`。
// 识别信息（画什么）必须排在审美词前 —— 反过来的话模型先照着风格词画，
// 把用户真正要的内容挤掉。
//
// 前两段由 Provider 侧在发请求时拼（它手上才有解析好的上游输入），
// 后几段在受理时拼进快照。这个函数负责的就是**后几段的先后**，
// 放在生成域是因为"这次生成的提示词长什么样"是生成域的事 ——
// 目录域、营销域只负责把配方取出来。
//
// 营销段（MKT-T-21、MKD-6）内部已由营销域排好「创意 → Hook → 风格 → 场景约束段」，
// 这里把它整体当一段。它排在预设之后、素材词之前：预设是版式（画成什么结构），
// 营销是这条广告片的创作配方，素材词是纯审美修饰。
//
// 3D 导演台约束段（DRD-18）排在预设之后、营销之前：输入里有导演台截图 / 预演视频时，
// 它讲「只参考人物站位与机位，不采用白模外观」—— 也是对画面结构的要求，要压过创作配方与审美词。
//
// 任一段为空（含只有空白）就跳过，不留空行。
func OrderAppendix(presetPart, directorPart, marketingPart, materialPart string) string {
	parts := make([]string, 0, 4)
	for _, p := range []string{presetPart, directorPart, marketingPart, materialPart} {
		if s := strings.TrimSpace(p); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n")
}
