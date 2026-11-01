package domain

import (
	"fmt"
	"strconv"
	"strings"
)

// 打光（dev/01 §3.2，R-5）：用图片大模型图生图，方向 / 亮度 / 颜色 / 轮廓光都由这里翻译成**固定格式**的句子 ——
// 不让前端拼（CLAUDE.md §5.9），也不让模型自由发挥约束句。

// RelightConstraint 固定约束句（代码固定）：打光最常见的失败是模型顺手把人换了、把构图改了。
const RelightConstraint = "只改变光照方向、强度、颜色与阴影；构图、主体的外观特征、姿态、服装与背景内容保持不变。"

// RelightSmartSentence 智能模式只发这一句。
const RelightSmartSentence = "按画面内容与场景自然地重新布光，让主体更立体、光影更有层次。"

// LightPreset 是主光源六个方向按钮的固定值。
type LightPreset struct {
	Code      string `json:"code"`
	Name      string `json:"name"`
	Azimuth   int    `json:"azimuth"`
	Elevation int    `json:"elevation"`
}

// LightPresets 顺序即界面上的两行三列：左侧 / 顶部 / 右侧 / 前方 / 底部 / 后方。
var LightPresets = []LightPreset{
	{"left", "左侧", 270, 0}, {"top", "顶部", 0, 80}, {"right", "右侧", 90, 0},
	{"front", "前方", 0, 0}, {"bottom", "底部", 0, -80}, {"back", "后方", 180, 0},
}

// NamedColor 是颜色命名表的一项（前端色板与它同序）。
type NamedColor struct {
	Name string `json:"name"`
	Hex  string `json:"hex"`
}

// NamedColors 16 色。用户自定义取色时取最接近的一个名字进提示词（模型听得懂「琥珀色」，听不懂 #E8A23B）。
var NamedColors = []NamedColor{
	{"暖白", "#FFF1DC"}, {"冷白", "#E6F0FF"}, {"琥珀", "#FFB347"}, {"橙", "#FF8C1A"},
	{"红", "#FF3B30"}, {"品红", "#FF2D95"}, {"粉", "#FF9EC4"}, {"紫", "#8E5CFF"},
	{"蓝", "#2F6BFF"}, {"青", "#24C8DB"}, {"绿", "#34C759"}, {"黄绿", "#B4E04B"},
	{"金黄", "#FFD23F"}, {"月光蓝", "#9DB8FF"}, {"霓虹青", "#00FFE1"}, {"夕阳红", "#FF6A3D"},
}

// RelightOptions 是打光的选项。
type RelightOptions struct {
	Smart      bool    `json:"smart"`
	Preset     string  `json:"preset,omitempty"`
	Azimuth    int     `json:"azimuth"`
	Elevation  int     `json:"elevation"`
	Brightness *int    `json:"brightness"`
	Color      *string `json:"color"`
	Rim        bool    `json:"rim"`
}

func (o RelightOptions) prepare() (Prepared, error) {
	var prompt string
	var opts any
	if o.Smart {
		// ⚠️ 智能模式下其余字段**忽略且不进快照**（IMG-F-43）：留着的话界面回显的是灰掉的值，
		// 而排障时会以为它们参与了。
		prompt = RelightSmartSentence + RelightConstraint
		opts = map[string]any{"smart": true}
	} else {
		if o.Preset != "" {
			found := false
			for _, p := range LightPresets {
				if p.Code == o.Preset {
					found = true
				}
			}
			if !found {
				return Prepared{}, fieldErr("preset", "未知方向 %s", o.Preset)
			}
		}
		if o.Azimuth < 0 || o.Azimuth > 360 {
			return Prepared{}, fieldErr("azimuth", "方位角应在 0~360°")
		}
		if o.Elevation < -90 || o.Elevation > 90 {
			return Prepared{}, fieldErr("elevation", "仰角应在 -90~90°")
		}
		bright := 50
		if o.Brightness != nil {
			bright = *o.Brightness
		}
		if bright < 0 || bright > 100 {
			return Prepared{}, fieldErr("brightness", "亮度应在 0~100")
		}
		colorName := ""
		if o.Color != nil && *o.Color != "" {
			c, ok := parseHex(*o.Color)
			if !ok {
				return Prepared{}, fieldErr("color", "颜色应为 #rrggbb")
			}
			colorName = NearestColor(c)
		}
		parts := []string{
			"重新布光：主光源来自" + LightDirection(o.Azimuth%360, o.Elevation) + "，光线" + BrightnessBand(bright) + "。",
		}
		if colorName != "" {
			parts = append(parts, "光色为"+colorName+"色调。")
		} else {
			parts = append(parts, "保持原有色温。")
		}
		if o.Rim {
			parts = append(parts, "加入明显的轮廓光，勾勒主体边缘。")
		}
		parts = append(parts, RelightConstraint)
		prompt = strings.Join(parts, "")
		opts = RelightOptions{Preset: o.Preset, Azimuth: o.Azimuth % 360, Elevation: o.Elevation,
			Brightness: &bright, Color: o.Color, Rim: o.Rim}
	}
	return Prepared{Tool: ToolRelight, Options: toMap(opts), Prompt: prompt, Count: 1}, nil
}

// LightDirection 把方位角 / 仰角换成方位描述（12 个：顶光、底光，以及平视的 8 个方位，斜上斜下加「略高 / 略低」）。
// azimuth：0 = 前方（相机方向）、90 = 右侧、180 = 后方、270 = 左侧。
func LightDirection(azimuth, elevation int) string {
	switch {
	case elevation >= 60:
		return "正上方（顶光）"
	case elevation <= -60:
		return "正下方（底光）"
	}
	names := []string{"正前方", "右前方", "右侧", "右后方（逆光）", "正后方（逆光）", "左后方（逆光）", "左侧", "左前方"}
	idx := ((azimuth%360 + 360 + 22) % 360) / 45
	name := names[idx]
	switch {
	case elevation >= 20:
		name += "略高处"
	case elevation <= -20:
		name += "略低处"
	}
	return name
}

// BrightnessBand 亮度 0~100 → 5 档。
func BrightnessBand(b int) string {
	switch {
	case b < 20:
		return "很暗、低调"
	case b < 40:
		return "偏暗"
	case b < 60:
		return "适中"
	case b < 80:
		return "明亮"
	}
	return "很亮、高调"
}

type rgb struct{ r, g, b int }

func parseHex(s string) (rgb, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(s) != 6 {
		return rgb{}, false
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return rgb{}, false
	}
	return rgb{int(v >> 16 & 0xff), int(v >> 8 & 0xff), int(v & 0xff)}, true
}

// NearestColor 取最接近的命名色（RGB 欧氏距离；够用：只为给模型一个它听得懂的词）。
func NearestColor(c rgb) string {
	best, bestD := "", -1
	for _, n := range NamedColors {
		x, _ := parseHex(n.Hex)
		d := sq(c.r-x.r) + sq(c.g-x.g) + sq(c.b-x.b)
		if bestD < 0 || d < bestD {
			best, bestD = n.Name, d
		}
	}
	return best
}

// NearestColorHex 便于测试：按十六进制串取命名色。
func NearestColorHex(hex string) (string, error) {
	c, ok := parseHex(hex)
	if !ok {
		return "", fmt.Errorf("颜色应为 #rrggbb")
	}
	return NearestColor(c), nil
}

func sq(n int) int { return n * n }
