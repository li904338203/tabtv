package domain

// 高清（dev/01 R-5）：用图片大模型图生图做「高清重绘」，按最高画质出图。
//
// ⚠️ 图片大模型做不到「按倍数放大像素」（出图尺寸由模型定），所以这里不提倍数，只有「增强程度」三档，
// 对应三档强度的提示词。约束句固定：高清最常见的失败是模型顺手改了构图、人脸或文字。

// UpscaleConstraint 固定约束句。
const UpscaleConstraint = "保持原图的构图、主体外观、颜色、文字与所有内容不变，不增减任何元素，只提升清晰度与细节。"

// UpscaleLevel 是一档增强程度。
type UpscaleLevel struct {
	Code string `json:"code"`
	Name string `json:"name"`
	// Prompt 这一档的提示词正文（代码固定；不下发前端）。
	Prompt string `json:"-"`
}

// UpscaleLevels 三档（顺序即界面上的顺序）。
var UpscaleLevels = []UpscaleLevel{
	{Code: "standard", Name: "标准", Prompt: "对这张图片做高清重绘：提高清晰度，去除噪点、模糊与压缩痕迹，边缘干净锐利。"},
	{Code: "fine", Name: "精细", Prompt: "对这张图片做精细的高清重绘：显著提高清晰度，补全细小纹理（皮肤、毛发、织物、材质表面），去除噪点、模糊与压缩痕迹。"},
	{Code: "ultra", Name: "极致", Prompt: "对这张图片做极致的高清重绘：以最高画质重建全部细节，纹理清晰可辨、边缘锐利、层次丰富，彻底去除噪点、模糊与压缩痕迹。"},
}

// UpscaleOptions 是高清的选项。
type UpscaleOptions struct {
	Level string `json:"level"`
}

func (o UpscaleOptions) prepare() (Prepared, error) {
	code := o.Level
	if code == "" {
		code = UpscaleLevels[0].Code
	}
	for _, l := range UpscaleLevels {
		if l.Code == code {
			return Prepared{Tool: ToolUpscale, Options: toMap(UpscaleOptions{Level: code}),
				Prompt: l.Prompt + UpscaleConstraint, Count: 1}, nil
		}
	}
	return Prepared{}, fieldErr("level", "增强程度只能是 standard / fine / ultra")
}
