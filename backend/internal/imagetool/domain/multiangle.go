package domain

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// 多角度（docs/agent/imagetools dev/01 §3.1，R-5）：用图片大模型图生图，机位（水平环绕 / 垂直俯仰 / 景别）
// 由这里翻译成**固定格式**的机位描述句，界面上的三个数决定发出去的那句话。

// 范围：horizontal 0~360、vertical -30~90、zoom 0~10。
const (
	VerticalMin      = -30
	VerticalMax      = 90
	ZoomMin          = 0
	ZoomMax          = 10
	ZoomDefault      = 5
	MultiAnglePrompt = 200 // 补充提示词上限（码点）
)

// MultiAngleConstraint 固定约束句：换机位最常见的失败是模型顺手换了人、换了衣服、换了场景。
const MultiAngleConstraint = "保持主体的外观特征、服装与姿态，以及场景内容、光线与画面风格不变，只改变相机机位与取景。"

// MultiAnglePreset 是一个预设（IMG-PARAM-20）。
type MultiAnglePreset struct {
	Code       string `json:"code"`
	Name       string `json:"name"`
	Horizontal int    `json:"horizontal"`
	Vertical   int    `json:"vertical"`
	Zoom       int    `json:"zoom"`
	// Extra 是代码固定的附加句（鱼眼、倾斜这类三个数表达不了的镜头语言）；不下发前端。
	Extra string `json:"-"`
}

// MultiAnglePresets 预设表（顺序即页签顺序）。
var MultiAnglePresets = []MultiAnglePreset{
	{Code: "custom", Name: "自定义", Horizontal: 0, Vertical: 0, Zoom: ZoomDefault},
	{Code: "fisheye", Name: "鱼眼视角", Horizontal: 0, Vertical: 0, Zoom: 6,
		Extra: "使用鱼眼镜头，画面呈明显的桶形畸变"},
	{Code: "dutch", Name: "倾斜视角", Horizontal: 30, Vertical: 10, Zoom: ZoomDefault,
		Extra: "荷兰角构图，相机向一侧倾斜约 20 度"},
	{Code: "high_front", Name: "正面俯拍", Horizontal: 0, Vertical: 45, Zoom: ZoomDefault},
	{Code: "low_front", Name: "正面仰拍", Horizontal: 0, Vertical: -30, Zoom: ZoomDefault},
	{Code: "birds_eye", Name: "全景俯拍", Horizontal: 0, Vertical: 75, Zoom: 1},
	{Code: "back", Name: "背面视角", Horizontal: 180, Vertical: 0, Zoom: ZoomDefault},
}

func multiAnglePreset(code string) (MultiAnglePreset, bool) {
	for _, p := range MultiAnglePresets {
		if p.Code == code {
			return p, true
		}
	}
	return MultiAnglePreset{}, false
}

// ZoomBand 是景别名分档（IMG-PARAM-21）。
type ZoomBand struct {
	Max  int    `json:"max"`
	Name string `json:"name"`
}

// ZoomBands 按上界升序：zoom ≤ Max 即这一档。
var ZoomBands = []ZoomBand{{1, "大远景"}, {3, "远景"}, {6, "中景"}, {8, "近景"}, {10, "特写"}}

// ZoomLabel 景别名。
func ZoomLabel(zoom int) string {
	for _, b := range ZoomBands {
		if zoom <= b.Max {
			return b.Name
		}
	}
	return ZoomBands[len(ZoomBands)-1].Name
}

// MultiAngleOptions 是多角度的选项。
type MultiAngleOptions struct {
	Preset     string `json:"preset"`
	Horizontal int    `json:"horizontal"`
	Vertical   int    `json:"vertical"`
	Zoom       *int   `json:"zoom"`
	Prompt     string `json:"prompt,omitempty"`
}

func (o MultiAngleOptions) prepare() (Prepared, error) {
	preset := o.Preset
	if preset == "" {
		preset = "custom"
	}
	p, ok := multiAnglePreset(preset)
	if !ok {
		return Prepared{}, fieldErr("preset", "未知预设 %s", o.Preset)
	}
	h := o.Horizontal
	if h < 0 || h > 360 {
		return Prepared{}, fieldErr("horizontal", "水平环绕应在 0~360°")
	}
	if h == 360 {
		h = 0
	}
	if o.Vertical < VerticalMin || o.Vertical > VerticalMax {
		return Prepared{}, fieldErr("vertical", "垂直俯仰应在 %d~%d°", VerticalMin, VerticalMax)
	}
	zoom := ZoomDefault
	if o.Zoom != nil {
		zoom = *o.Zoom
	}
	if zoom < ZoomMin || zoom > ZoomMax {
		return Prepared{}, fieldErr("zoom", "景别应在 %d~%d", ZoomMin, ZoomMax)
	}
	extra := strings.TrimSpace(o.Prompt)
	if utf8.RuneCountInString(extra) > MultiAnglePrompt {
		return Prepared{}, fieldErr("prompt", "提示词最多 %d 字", MultiAnglePrompt)
	}
	prompt := MultiAnglePromptOf(h, o.Vertical, zoom, p.Extra, extra)
	opts := MultiAngleOptions{Preset: preset, Horizontal: h, Vertical: o.Vertical, Zoom: &zoom, Prompt: extra}
	return Prepared{Tool: ToolMultiAngle, Options: toMap(opts), Prompt: prompt, Count: 1}, nil
}

// MultiAnglePromptOf 机位 → 提示词：「换一个机位重新拍：水平…，垂直…，景别…。预设附加句。用户补充。约束句」。
func MultiAnglePromptOf(horizontal, vertical, zoom int, presetExtra, userExtra string) string {
	parts := []string{"以同一主体、同一场景，换一个相机机位重新拍摄这张画面：" +
		HorizontalPhrase(horizontal) + "，" + VerticalPhrase(vertical) + "，景别为" + ZoomLabel(zoom) + "。"}
	for _, x := range nonEmpty(presetExtra, userExtra) {
		parts = append(parts, strings.TrimRight(x, "。.")+"。")
	}
	parts = append(parts, MultiAngleConstraint)
	return strings.Join(parts, "")
}

// HorizontalPhrase 水平环绕角 → 机位方位（0 正面、90 右侧、180 背面、270 左侧），非整方位带度数。
func HorizontalPhrase(h int) string {
	h = ((h % 360) + 360) % 360
	switch {
	case h <= 10 || h >= 350:
		return "相机在主体正前方（正面视角）"
	case h < 80:
		return fmt.Sprintf("相机向主体右侧环绕约 %d 度（右前侧视角）", h)
	case h <= 100:
		return "相机在主体右侧（右侧面视角）"
	case h < 170:
		return fmt.Sprintf("相机绕到主体右后方约 %d 度（右后方视角）", h)
	case h <= 190:
		return "相机在主体正后方（背面视角，看到主体的背部）"
	case h < 260:
		return fmt.Sprintf("相机绕到主体左后方约 %d 度（左后方视角）", 360-h)
	case h <= 280:
		return "相机在主体左侧（左侧面视角）"
	}
	return fmt.Sprintf("相机向主体左侧环绕约 %d 度（左前侧视角）", 360-h)
}

// VerticalPhrase 垂直俯仰角 → 机位高度（-30 仰拍 … 0 平视 … 90 正上方）。
func VerticalPhrase(v int) string {
	switch {
	case v <= -20:
		return fmt.Sprintf("低机位仰拍约 %d 度", -v)
	case v < -5:
		return "略低于主体的机位、轻微仰视"
	case v <= 5:
		return "与主体平齐的平视机位"
	case v < 40:
		return fmt.Sprintf("高机位俯拍约 %d 度", v)
	case v < 75:
		return fmt.Sprintf("大角度俯拍约 %d 度", v)
	}
	return "相机几乎在正上方垂直向下拍摄（鸟瞰）"
}

func nonEmpty(ss ...string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
