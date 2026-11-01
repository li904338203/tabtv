package domain

import "fmt"

// UIConfig 是下发给前端的工具条配置（`/canvas/config.imageTools`，IMG-I-04）。
//
// ⚠️ **不含**提示词（多角度预设的附加句、情绪的英文描述、高清各档的提示词）：
// 那些是服务端的翻译规则，下发了前端也用不上，反而让人以为改前端就能改发给上游的东西。
type UIConfig struct {
	Tools             []UITool           `json:"tools"`
	PortraitPreset    string             `json:"portraitPreset"`
	PanoramaPreset    string             `json:"panoramaPreset"`
	MultiAnglePresets []MultiAnglePreset `json:"multiAnglePresets"`
	ZoomBands         []ZoomBand         `json:"zoomBands"`
	LightPresets      []LightPreset      `json:"lightPresets"`
	NamedColors       []NamedColor       `json:"namedColors"`
	Emotions          []UIEmotion        `json:"emotions"`
	UpscaleLevels     []UpscaleLevel     `json:"upscaleLevels"`
	MaxRoles          int                `json:"maxRoles"`
}

// UITool 是工具条上的一个工具。
type UITool struct {
	ToolInfo
	IsNew bool `json:"isNew"`
}

// UIEmotion 是坐标盘上的一格（带示意头像地址）。
type UIEmotion struct {
	Arousal int    `json:"arousal"`
	Warmth  int    `json:"warmth"`
	Name    string `json:"name"`
	Image   string `json:"image"`
}

// EmotionImagePath 示意头像（前端静态资源，IMG-D-06）。
func EmotionImagePath(arousal, warmth int) string {
	return fmt.Sprintf("/imagetools/emotion/%d_%d.svg", arousal, warmth)
}

// BuildUIConfig 组装工具条配置。
func BuildUIConfig() UIConfig {
	tools := make([]UITool, 0, 4)
	for _, t := range Tools() {
		// 「人像质感调节 ▾」下拉里的「情绪调节」是本轮新功能，带 NEW。
		tools = append(tools, UITool{ToolInfo: t, IsNew: t.Code == ToolExpression})
	}
	emotions := make([]UIEmotion, 0, len(Emotions))
	for _, e := range Emotions {
		emotions = append(emotions, UIEmotion{Arousal: e.Arousal, Warmth: e.Warmth, Name: e.Name,
			Image: EmotionImagePath(e.Arousal, e.Warmth)})
	}
	return UIConfig{
		Tools: tools, PortraitPreset: "portrait_tuning", PanoramaPreset: "panorama_720",
		MultiAnglePresets: MultiAnglePresets, ZoomBands: ZoomBands, LightPresets: LightPresets,
		NamedColors: NamedColors, Emotions: emotions, UpscaleLevels: UpscaleLevels, MaxRoles: MaxRoles,
	}
}
