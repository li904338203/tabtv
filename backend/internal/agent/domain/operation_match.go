package domain

import (
	"fmt"
	"sort"
	"strings"
)

// 生成方式与输入的匹配（04 文档「技能和模型参数」：带了参考图 / 商品图，却走了文生视频）。
//
// 实测的失败链：模型给计划项写了 inputs={image:1} 却没写操作码 → 按产出类型取默认 text2video →
// 报价「该模型此操作不接受 image 输入」→ 模型把参考图去掉、照样用 text2video；营销会话里更糟：
// 选了商品图的条目配 text2video，提示「最多用 0 张」，模型于是把商品图、再把所有营销选项一并去掉，
// 产出的视频里没有商品。而所选模型（seedance_2_5）明明支持 img2video / ref2video。
//
// 这里只做一件事：**当前的生成方式收不下这批输入时，换成该模型收得下的那一种**。
// 判断用目录的能力矩阵（与报价同一份），不写型号名单；收得下就不换（模型选的就是它）。

// RoleRange 是一个输入角色的数量范围。
type RoleRange struct {
	Min int
	Max int
}

// OperationFit 是一个模型的一个可用生成方式（来自目录）。
type OperationFit struct {
	Code string
	// Output 产出类型：video / image / audio / text。
	Output string
	// Roles 输入角色 → 数量范围（能力矩阵）。为空 = 没配能力矩阵（报价会拦，这里不选它）。
	Roles map[string]RoleRange
	// Limits 视频参数范围的说明（时长 / 清晰度 / 画幅）；不是视频为空。
	Limits string
}

// Accepts 报告这个生成方式收不收这批输入：声明的角色数量在范围内，没声明的角色不能有。
// 与目录 CapabilitySet.Check 同一条规则。
func (f OperationFit) Accepts(counts map[string]int) bool {
	if len(f.Roles) == 0 {
		return false
	}
	for role, r := range f.Roles {
		n := counts[role]
		if n < r.Min || n > r.Max {
			return false
		}
	}
	for role, n := range counts {
		if n <= 0 {
			continue
		}
		if _, ok := f.Roles[role]; !ok {
			return false
		}
	}
	return true
}

// textOnlyOps 是只收文字的生成方式。
var textOnlyOps = map[string]bool{"text2video": true, "text2img": true}

// IsTextOnlyOperation 报告这个生成方式是不是只收文字（文生视频 / 文生图）。
func IsTextOnlyOperation(op string) bool { return textOnlyOps[op] }

// candidateOps 按输入给出候选生成方式（按偏好排序）。只覆盖视频与图片：其余产出没有可换的方式。
func candidateOps(kind string, counts map[string]int) []string {
	images, videos, audios := counts["image"], counts["video"], counts["audio"]
	switch kind {
	case "video":
		switch {
		case videos > 0:
			return []string{"video2video", "ref2video"}
		case audios > 0:
			return []string{"ref2video"}
		case images == 1:
			return []string{"img2video", "ref2video"}
		case images == 2:
			// 两张图默认按参考图用；首尾帧要模型明确写（顺序就是语义）。
			return []string{"ref2video", "first_last_frame"}
		case images > 2:
			return []string{"ref2video"}
		}
		return []string{"text2video"}
	case "image":
		if images > 0 {
			return []string{"img2img"}
		}
		return []string{"text2img"}
	}
	return nil
}

// MatchOperation 给一批输入挑生成方式。
//
//   - current 非空且收得下这批输入 → 原样返回（switched=false）：模型选的就是它，不替它改；
//   - 否则按输入在该模型的生成方式里挑第一个收得下的（switched = current 非空且换了）；
//   - 一个都收不下 → ok=false，调用方照原样报价，让报价说明原因（并用 DescribeFits 告诉模型能用哪些）。
//
// current 为空表示调用方没写（原先按产出类型取默认，带图也是文生）。
func MatchOperation(kind, current string, counts map[string]int, fits []OperationFit) (op string, switched, ok bool) {
	byCode := make(map[string]OperationFit, len(fits))
	for _, f := range fits {
		if f.Output == "" || f.Output == kind {
			byCode[f.Code] = f
		}
	}
	if current != "" {
		if f, has := byCode[current]; has && f.Accepts(counts) {
			return current, false, true
		}
	}
	for _, c := range candidateOps(kind, counts) {
		if f, has := byCode[c]; has && f.Accepts(counts) {
			return c, current != "" && c != current, true
		}
	}
	return current, false, false
}

// DescribeFits 列出模型在某产出类型下的生成方式与各自收什么输入（写给模型看）。
func DescribeFits(kind string, fits []OperationFit) string {
	var lines []string
	for _, f := range fits {
		if f.Output != "" && f.Output != kind {
			continue
		}
		line := "- " + f.Code + "：" + describeRoles(f.Roles)
		if f.Limits != "" {
			line += "；" + f.Limits
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

var roleLabels = map[string]string{"text": "文字", "image": "图片", "video": "视频", "audio": "音频"}

func describeRoles(roles map[string]RoleRange) string {
	if len(roles) == 0 {
		return "未配置输入能力（不可用）"
	}
	keys := make([]string, 0, len(roles))
	for k := range roles {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var media, parts []string
	for _, k := range keys {
		r := roles[k]
		if k == "text" {
			continue
		}
		label := roleLabels[k]
		if label == "" {
			label = k
		}
		switch {
		case r.Min == r.Max:
			media = append(media, fmt.Sprintf("%s恰好 %d 个", label, r.Min))
		case r.Min == 0:
			media = append(media, fmt.Sprintf("%s最多 %d 个", label, r.Max))
		default:
			media = append(media, fmt.Sprintf("%s %d~%d 个", label, r.Min, r.Max))
		}
	}
	if len(media) == 0 {
		parts = append(parts, "只收文字")
	} else {
		parts = append(parts, strings.Join(media, "、"))
	}
	return strings.Join(parts, "")
}
