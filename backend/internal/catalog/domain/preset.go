package domain

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// 图片预设：一次成图的完整配方（NEW-12、CV-14）。
//
// ⚠️ 预设**不是"一组参数默认值"**。旧实现里 `presetCode` 写进 params 之后
// 没有任何代码读它 —— 选了等于没选，只改画幅不改画面。
// 一个预设 = 归组 + 文案 + **提示词** + 参数默认，而提示词那部分
// 只有在服务端拼进去才真正生效。
//
// ⚠️ 拼接**发生在服务端**。前端拼出来的词会落进 `canvasData`：
// 用户改一个字玩法就失效，历史节点永远停在旧词，调词也没法只靠迁移完成。

// PresetInputMode 说明这个预设需要什么输入。
type PresetInputMode string

const (
	// InputImageRequired 必须有图（打光、高清、抠图这类加工）。
	InputImageRequired PresetInputMode = "image_required"
	// InputTextRequired 必须有文字（纯生成类）。
	InputTextRequired PresetInputMode = "text_required"
	// InputEither 两者有其一即可。
	InputEither PresetInputMode = "either"
)

// Preset 是一个图片预设。
type Preset struct {
	Code      string
	Name      string
	Group     string
	Spec      string
	InputMode PresetInputMode
	// Prompt 是追加的正文，**前端不可见**。
	Prompt string
	// Dimensions 是多维调节：每维有若干选项，每维有一个 default。
	Dimensions []PresetDimension
	Operation  string
}

// PresetDimension 是一个可调维度（例如"光线方向"）。
type PresetDimension struct {
	Code    string
	Name    string
	Default string
	Options []PresetOption
}

// PresetOption 是维度里的一个取值。
type PresetOption struct {
	Value string
	Label string
	// Prompt 是选中这一项时追加的词。
	Prompt string
}

// ParseDimensions 解析 `image_preset.dimensions` 的 JSON。
//
// ⚠️ 解不开时**返回空而不是报错**：一个维度配坏了不该让整个预设不可用 ——
// 预设正文还在，用户至少能用默认效果出图。
func ParseDimensions(raw json.RawMessage) []PresetDimension {
	if len(raw) == 0 {
		return nil
	}
	var out []PresetDimension
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// ---------------------------------------------------------------- 输入校验

// ValidatePresetInput 校验这次调用满不满足预设的输入要求。
//
// ⚠️ 在**扣费之前**校验（NEW-12 的"能力"那一半）：不校验的话，
// 用户对一个空节点点"打光"，钱扣了、上游收到一句没有图的提示词，
// 出来一张跟他的照片毫无关系的图。
func ValidatePresetInput(p Preset, hasImage bool, text string) error {
	switch p.InputMode {
	case InputImageRequired:
		if !hasImage {
			return fmt.Errorf("「%s」需要先连一张图片", p.Name)
		}
	case InputTextRequired:
		if strings.TrimSpace(text) == "" {
			return fmt.Errorf("「%s」需要填写描述", p.Name)
		}
	case InputEither:
		if !hasImage && strings.TrimSpace(text) == "" {
			return fmt.Errorf("「%s」需要一张图片或一段描述", p.Name)
		}
	}
	return nil
}

// ---------------------------------------------------------------- 提示词拼接

// ComposePreset 把预设拼成要追加的提示词。
//
// 顺序是 **预设正文 → 维度微调**，而整体又排在**用户描述之后、素材词之前**
// （那一层的先后由 generation/domain.OrderAppendix 定 —— "这次生成的提示词
// 长什么样"是生成域的事，目录域只负责把配方取出来）。
//
// ⚠️ 用户没打开过调节面板时，`选中值` 是空的 —— 这时要按每维的 `default`
// 出词，而**不是跳过这一维**。跳过的话同一个预设会"时有时无地缺几句"：
// 打开过面板的用户和没打开过的用户拿到的是两种效果，而他们选的是同一个预设。
func ComposePreset(p Preset, selected map[string]string) string {
	var parts []string
	if s := strings.TrimSpace(p.Prompt); s != "" {
		parts = append(parts, s)
	}
	for _, d := range p.Dimensions {
		value := selected[d.Code]
		if value == "" {
			value = d.Default
		}
		if value == "" {
			continue
		}
		for _, o := range d.Options {
			if o.Value != value {
				continue
			}
			if s := strings.TrimSpace(o.Prompt); s != "" {
				parts = append(parts, s)
			}
			break
		}
	}
	return strings.Join(parts, "\n")
}

// ---------------------------------------------------------------- 参数默认

// ApplyPresetParams 把预设的参数默认值并进节点参数。
//
// ⚠️ **用户已经设过的值优先**：预设只填他没碰过的那些。
// 反过来的话，用户调好画幅再选一个预设，画幅被悄悄改回去了 ——
// 而他完全不知道是谁改的。
func ApplyPresetParams(nodeParams, presetParams json.RawMessage) json.RawMessage {
	if len(presetParams) == 0 {
		return nodeParams
	}
	var preset map[string]any
	if json.Unmarshal(presetParams, &preset) != nil {
		return nodeParams
	}
	node := map[string]any{}
	if len(nodeParams) > 0 {
		if json.Unmarshal(nodeParams, &node) != nil {
			return nodeParams
		}
	}
	for k, v := range preset {
		if _, exists := node[k]; !exists {
			node[k] = v
		}
	}
	out, err := json.Marshal(node)
	if err != nil {
		return nodeParams
	}
	return out
}

// SortPresets 让预设按 group 再按 sort 稳定排序，供选择器展示。
func SortPresets[T any](items []T, group func(T) string, sortKey func(T) int) {
	sort.SliceStable(items, func(i, j int) bool {
		gi, gj := group(items[i]), group(items[j])
		if gi != gj {
			return groupRank(gi) < groupRank(gj)
		}
		return sortKey(items[i]) < sortKey(items[j])
	})
}

// groupRank 固定分组的展示顺序。
//
// ⚠️ 按字典序排的话，"空间与机位"会排在"设定图"前面 ——
// 而用户的工作顺序是先出设定图再调空间。
func groupRank(g string) int {
	switch g {
	case "sheet":
		return 0
	case "tuning":
		return 1
	case "storyboard":
		return 2
	case "space":
		return 3
	}
	return 9
}
