package protocol

import (
	"fmt"
	"strings"
)

// 视频协议共用的输入整理与规格解析。
//
// ⚠️ 四家的请求体形状**完全不同**（见各自的注释），没有公共子集可抽；
// 这里只共用「怎么从 Job 里取到有序的图/视频」和「规格词表怎么解析」
// —— 那两件事才是真正重复的。

// mediaByKind 按连线顺序取出某类媒体的地址。
//
// ⚠️ 顺序就是语义：`first_last_frame` 的第一张是首帧、第二张是尾帧。
// 解析或查询中丢掉顺序，用户会拿到一段首尾颠倒的过渡视频，而且看不出哪里不对。
func mediaByKind(job Job, kind string) []string {
	out := make([]string, 0, len(job.Media))
	for _, m := range job.Media {
		if m.Kind == kind {
			out = append(out, m.Value)
		}
	}
	return out
}

// requireCount 检查输入数量。
func requireCount(values []string, want int, message string) error {
	if len(values) < want {
		return fmt.Errorf("%s（当前 %d 个）", message, len(values))
	}
	return nil
}

// videoPrompt 取提示词，空时按输入形态给一句兜底。
//
// ⚠️ 兜底不是可有可无：图生视频的提示词为空时上游多半直接 400，
// 而用户的心智是「我给了图，你动起来就行」—— 没有兜底的话，
// 一个完全合理的操作会以一句上游错误告终。
func videoPrompt(job Job) (string, error) {
	if p := strings.TrimSpace(job.Prompt); p != "" {
		return p, nil
	}
	if len(mediaByKind(job, "image")) > 0 {
		return "Animate the provided image naturally while preserving its subject, composition, and visual style.", nil
	}
	if len(mediaByKind(job, "video")) > 0 {
		return "Preserve the source video while applying the requested motion and visual changes.", nil
	}
	return "", fmt.Errorf("视频生成提示词不能为空")
}

// resolutionOf 取清晰度。
//
// ⚠️ 这里只做**宽松的兜底白名单**，不按型号分支：哪个型号支持到哪一档
// 由 `model_param_support` 按型号下发。把型号能力写进代码的话，
// 上游放开一档就得改代码重新发版 —— 这正是配置驱动要避免的。
func resolutionOf(params map[string]any, allowed []string, fallback string) (string, error) {
	v := paramString(params, "resolution", "")
	if v == "" {
		v = nestedString(params, "size", "clarity")
	}
	if v == "" {
		return fallback, nil
	}
	v = strings.ToLower(v)
	for _, a := range allowed {
		if v == a {
			return v, nil
		}
	}
	return "", fmt.Errorf("视频分辨率 %q 不受支持（可选 %s）", v, strings.Join(allowed, "、"))
}

// ratioOf 取画幅比。返回空串表示"不带这个字段，由上游用默认值"。
func ratioOf(params map[string]any, allowed []string) (string, error) {
	v := paramString(params, "aspect_ratio", "")
	if v == "" {
		v = paramString(params, "aspectRatio", "")
	}
	if v == "" {
		v = nestedString(params, "size", "ratio")
	}
	if v == "" {
		return "", nil
	}
	for _, a := range allowed {
		if strings.EqualFold(v, a) {
			return v, nil
		}
	}
	return "", fmt.Errorf("视频比例 %q 不受支持（可选 %s）", v, strings.Join(allowed, "、"))
}

// durationOf 取时长。
//
// ⚠️ 默认值必须与计价的默认时长一致（`per_second` = 单价 × 时长）：
// 两边默认值不一致就会出现「扣的积分和实际生成时长对不上」，而这种错
// 在对账之前谁都不会发现。
func durationOf(params map[string]any, minSec, maxSec int) (int, error) {
	d := paramInt(params, "duration", 0)
	if d == 0 {
		d = nestedInt(params, "size", "duration")
	}
	if d == 0 {
		d = DefaultVideoSeconds
	}
	if d < minSec || d > maxSec {
		return 0, fmt.Errorf("视频时长必须在 %d 到 %d 秒之间（当前 %d）", minSec, maxSec, d)
	}
	return d, nil
}

// DefaultVideoSeconds 是视频时长的默认值，**与计价侧共用同一个常量语义**。
const DefaultVideoSeconds = 5

func paramBool(params map[string]any, fallback bool, names ...string) bool {
	for _, n := range names {
		if v, ok := params[n].(bool); ok {
			return v
		}
	}
	if v, ok := nested(params, "size", "audio").(bool); ok && len(names) == 1 && names[0] == "audio" {
		return v
	}
	return fallback
}

func nested(params map[string]any, outer, inner string) any {
	m, ok := params[outer].(map[string]any)
	if !ok {
		return nil
	}
	return m[inner]
}

func nestedString(params map[string]any, outer, inner string) string {
	if s, ok := nested(params, outer, inner).(string); ok {
		return s
	}
	return ""
}

func nestedInt(params map[string]any, outer, inner string) int {
	switch v := nested(params, outer, inner).(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

// rejectAudioInputs 明确拒绝音频参考。
//
// ⚠️ **宁可指名拒绝，也不猜一个字段名发上去**：猜错换回来的是一个看不懂的
// 上游 400，而用户完全不知道是音频那一路出的问题。画布的「图+音→全能参考」
// 会真的带音频输入进来，所以这条路一定会被走到。
func rejectAudioInputs(job Job, who string) error {
	if len(mediaByKind(job, "audio")) > 0 {
		return fmt.Errorf("%s暂不支持音频参考输入，请去掉音频连线后重试", who)
	}
	return nil
}
