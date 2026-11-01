package domain

import "fmt"

// 图片工具的可用性（NEW-12、FULL-04、GAP-01）。
//
// ⚠️ 「这个工具现在能不能点」是**可调用能力合取式**的一次求值，
// 不是一个独立的开关。真正的判定复用报价那条路径（`QuoteWithFacts`）——
// 这里只处理它管不到的那一半：**用户这边缺不缺输入**。
//
// 为什么要分成两半：缺图是**用户自己能解决**的（连一张图上去就行），
// 而"模型没绑这个操作""这个能力没配价"是**运营要解决**的。
// 混成一句"该工具不可用"的话，用户会一直盯着那个灰掉的按钮等运营修，
// 而其实他自己点两下就能用。

// ToolBlockCode 是阻塞原因的分类。
//
// ⚠️ 前端要按这个**分类**决定提示语气，不是按文案做字符串匹配 ——
// 文案改一个字那种匹配就失效了，而失效的表现是提示全变成默认那句。
const (
	// ToolNeedsImage 需要先连一张图。用户可自行解决。
	ToolNeedsImage = "needs_image"
	// ToolNeedsText 需要先写一段描述。用户可自行解决。
	ToolNeedsText = "needs_text"
)

// 预设的输入要求。与 `image_preset.input_mode` 一一对应。
const (
	InputModeImageRequired = "image_required"
	InputModeTextRequired  = "text_required"
	InputModeEither        = "either"
)

// ToolProbe 是一次工具可用性探测的上下文。
//
// ⚠️ `ImageCount` / `HasText` 是**前端报的当前节点状态**，只用于给提示。
// 这与「角色计数由解析方给出、不信客户端」并不矛盾：那条规则约束的是
// **报价与受理**（信客户端的话少报一张图就能绕过能力矩阵），
// 而这里是一个只读的提示接口 —— 客户端谎报只会误导它自己，
// 真正的校验仍然发生在受理链路上。把这句话写在这里，
// 是为了以后有人看到这个结构体时不会以为那条规则被破了。
type ToolProbe struct {
	InputMode  string
	ImageCount int
	HasText    bool
}

// ToolBlock 是一条阻塞原因。
type ToolBlock struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// CheckToolInput 判断「用户这边缺不缺输入」。够了返回 nil。
func CheckToolInput(p ToolProbe, toolName string) *ToolBlock {
	switch p.InputMode {
	case InputModeImageRequired:
		if p.ImageCount <= 0 {
			return &ToolBlock{ToolNeedsImage,
				fmt.Sprintf("「%s」需要先连一张图片", toolName)}
		}
	case InputModeTextRequired:
		if !p.HasText {
			return &ToolBlock{ToolNeedsText,
				fmt.Sprintf("「%s」需要先写一段描述", toolName)}
		}
	case InputModeEither:
		// ⚠️ `either` 要求**至少有一样**，不是"两样都不要也行"：
		// 两样都没有时模型收到的是一句空提示词，出来的图和用户想要的
		// 毫无关系 —— 而任务是"成功"的，钱照扣。
		if p.ImageCount <= 0 && !p.HasText {
			return &ToolBlock{ToolNeedsText,
				fmt.Sprintf("「%s」需要一张图或一段描述", toolName)}
		}
	}
	return nil
}

// UserFixable 告诉前端这条阻塞是不是用户自己能解决的。
//
// 分类而不是文案匹配：文案改一个字，匹配就失效，
// 而失效的表现是所有提示都退回那句默认的"暂不可用"。
func UserFixable(code string) bool {
	return code == ToolNeedsImage || code == ToolNeedsText || code == ToolMediaRejected
}

// ToolMediaRejected 这个操作本身不收媒体输入（`text2img` 之类）。
//
// 归到"用户自己能解决"：断开那条图片连线就能用 —— 而这确实是他的选择。
const ToolMediaRejected = "media_not_accepted"

// CheckOperationAcceptsMedia 判断「连着图，但这个操作根本不收图」。
//
// ⚠️ 与能力矩阵**不是一回事**：能力矩阵说的是"这个模型这次能收几张图"，
// 这里说的是"这个操作本身收不收图"。两条都要判 ——
// 少判这一条的症状是**工具栏说能用、点下去 422**，
// 而那正是工具栏存在的全部理由。
//
// ⚠️ `accepts` 为空表示这个操作没有声明输入类型（配置里没填）。
// 这时**放行**：配置可以比代码新，不认识的东西不该在这里被拦下来，
// 受理链路上还有一道真正的校验。
func CheckOperationAcceptsMedia(accepts []string, imageCount int, toolName string) *ToolBlock {
	if imageCount <= 0 || len(accepts) == 0 {
		return nil
	}
	for _, a := range accepts {
		if a == "image" {
			return nil
		}
	}
	return &ToolBlock{ToolMediaRejected,
		fmt.Sprintf("「%s」只按描述出图，请先断开图片输入", toolName)}
}
