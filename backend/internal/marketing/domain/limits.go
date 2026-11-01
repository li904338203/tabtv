package domain

import "unicode/utf8"

// 长度上限。⚠️ 一律按 **Unicode 码点**计（`utf8.RuneCountInString`），
// 不是 Go 的 `len()`（字节）—— 一个汉字 3 字节，按字节算的话 30 字的商品名
// 只能写 10 个汉字，而界面上的计数器说还能写 20 个（与 `per_kchar` 计价同一条）。
const (
	// MaxProductName / MaxSellingPoints 是商品字段（MKT-D-41），随消息存、在代码里校验。
	MaxProductName   = 30
	MaxSellingPoints = 300

	// 选项字段（MKT-D-22、25、27）。
	MaxOptionName        = 24
	MaxOptionDescription = 60
	MaxOptionPrompt      = 1000

	// 封面张数（MKT-D-26）：创意 1~3 张（模板墙三联），Hook / 风格 1 张。
	MaxCreativeCovers = 3
	MaxOtherCovers    = 1

	// MaxCharacterRefs 是角色槽在营销场景下的上限。
	//
	// ⚠️ 需求只定了商品槽的上限（20，配置值），角色槽没有定 —— 这里取 dev/01 §4.3
	// 示例里的 2，是**设计建议值**。技能声明的 maxCount 不得超过它。
	MaxCharacterRefs = 2
	// MaxSourceVideos 是源视频槽上限：裂变一次只针对一条源视频。
	MaxSourceVideos = 1
)

// 商品库与角色库（MKT-T-33；dev/05 MKD-34）。以下全部是**设计建议值**（MKT-PARAM-16~19），
// 经 `GET /marketing/config` 下发（MKT-T-37 加齐），前端不写死。
const (
	// MaxProductDescription 商品描述（MKT-D-50）。名称 / 卖点沿用 MaxProductName / MaxSellingPoints。
	MaxProductDescription = 300
	// MaxProductsPerSession 一次创作可选商品数（MK-22；多选待定 MKT-PARAM-16）。
	MaxProductsPerSession = 1
	// MaxCharacterImages 每个角色的图片上限（MKT-PARAM-19）。
	MaxCharacterImages = 9
	// MaxCharacterGenCount 一次「生成角色」的张数上限。
	MaxCharacterGenCount = 4
	// MaxAppearance / MaxVoiceText 角色形象描述 / 音色描述。
	MaxAppearance = 500
	MaxVoiceText  = 500
	// MaxCharacterName 角色名（MKD-36）。
	MaxCharacterName = 50

	// 下面三个是配置项（SEEAITV_MARKETING_MAX_PRODUCT_IMAGES / _MAX_PRODUCTS / _MAX_CHARACTERS）的默认值，
	// 只在调用方没传（≤ 0）时兜底；真正生效的数以配置为准。
	DefaultMaxProductImages = 10
	DefaultMaxProducts      = 500
	DefaultMaxCharacters    = 200
)

// RuneLen 是本域统一的长度口径（码点）。
func RuneLen(s string) int { return utf8.RuneCountInString(s) }

// ValidateProductFields 校验商品名称与卖点的长度（可为空：未填写时摘要写「未填写」）。
func ValidateProductFields(name, sellingPoints string) []Issue {
	var out []Issue
	if n := RuneLen(name); n > MaxProductName {
		out = append(out, tooLong("product.name", "商品名称", n, MaxProductName))
	}
	if n := RuneLen(sellingPoints); n > MaxSellingPoints {
		out = append(out, tooLong("product.sellingPoints", "卖点", n, MaxSellingPoints))
	}
	return out
}
