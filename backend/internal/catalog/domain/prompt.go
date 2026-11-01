package domain

import (
	"fmt"
	"sort"
	"strings"
)

// PromptStatus 提示词模板状态。
type PromptStatus string

const (
	PromptDraft     PromptStatus = "draft"
	PromptPublished PromptStatus = "published"
	PromptArchived  PromptStatus = "archived"
)

// PromptSpec 描述一个提示词用途的占位符契约。
//
// ⚠️ 占位符必须**在保存时校验**：模板里写错一个变量名，
// 运行时拼出来的就是字面量 `{{shotCount}}` 发给模型，
// 而这不会报错 —— 模型会照着那段乱码生成，用户看到莫名其妙的结果。
type PromptSpec struct {
	Code string
	Name string
	// Required 必须出现的占位符。缺一个就等于丢掉一段上下文。
	Required []string
	// Optional 允许但不强制。
	Optional []string
	// MaxChars 内容长度上限。
	MaxChars int
	// Static 纯规则文本、**不接受任何占位符**（调用方渲染时不给变量）。
	// 与「一个可用占位符都没有」的误配区分开：后者是漏登记变量，前者是有意的。
	Static bool
}

// promptSpecs 是内置的用途目录。
// 新增用途在这里加一行，校验规则随之生效。
// ⚠️ 这三个用途的内容占位符都是**可选**的，不是必需的 ——
// 剧情、镜头描述这些上下文由**调用方拼的 user 消息**保证送达，
// 系统提示词只写"怎么做"。
//
// 一开始它们被定成必需（"缺一个就等于丢掉一段上下文"），但那前提是
// 模板是上下文的唯一载体。实现落下来不是这样：
//
//	① 强制模板内联剧情 = 同一段剧情在 system 和 user 里各发一遍，
//	   一个几千字的剧本每次分镜都双倍计费；
//	② 更糟的是**回落路径**：渲染失败、库里一条都没有、或调用方没接
//	   目录域时会退回内置兜底，而兜底里没有剧情 —— 于是模型
//	   一个字的剧情都没收到，却照样生成，链路全绿、结果全是编的。
//
// 运营想把上下文内联进模板仍然可以（占位符照常渲染），只是不强制。
var promptSpecs = map[string]PromptSpec{
	"storyboard_gen.system": {
		Code: "storyboard_gen.system", Name: "分镜生成系统提示词",
		Optional: []string{"story", "shotCount", "globalStyle", "characterRefs", "duration"},
		MaxChars: 8000,
	},
	"prompt_compose.system": {
		Code: "prompt_compose.system", Name: "提示词合成系统提示词",
		Optional: []string{"shotDescription", "assetProfiles", "globalStyle",
			"shotSize", "cameraMove", "lighting"},
		MaxChars: 8000,
	},
	"asset_profile.system": {
		Code: "asset_profile.system", Name: "资产档案生成提示词",
		Optional: []string{"assetName", "assetType", "story", "globalStyle"},
		MaxChars: 6000,
	},
	// 营销会话 Agent 的场景段（MKT-T-18）：拼在「## 场景：营销」之后，渲染方 agent/app.MarketingSceneVars
	// 给全这四个变量（`TestMarketingSceneVars_覆盖用途契约` 钉住两边一致）。
	// 商品、卖点、所选项不在这里：它们在代码拼的「## 营销输入」段里（格式契约）。
	"agent.scene.marketing": {
		Code: "agent.scene.marketing", Name: "营销会话场景段（Agent 系统提示词）",
		Optional: []string{"state", "reasoningModel", "maxVariants", "productCount"},
		MaxChars: 4000,
	},
	// 营销生成提示词末尾的场景约束段（MKT-T-21 使用）：平台规则，渲染时不给变量，**不进** Agent 上下文。
	"agent.scene.marketing.constraint": {
		Code: "agent.scene.marketing.constraint", Name: "营销生成场景约束段",
		Static: true, MaxChars: 1000,
	},
	// 3D 导演台 AI 搭景的创作要求（docs/agent/director dev/01 §5，DRT-19）：输出格式契约由代码固定追加在它之后
	// （改格式等于改解析器，DRD-25），场景摘要与描述在 user 消息里，所以不收变量。
	// ⚠️ 正文里不要出现「分镜」二字：假上游见到它就回一份分镜 JSON，E2E 每次都会是 42216。
	"director.scene_build.system": {
		Code: "director.scene_build.system", Name: "导演台 AI 搭景系统提示词",
		Static: true, MaxChars: 4000,
	},
	// 3D 导演台的下游约束段（DRT-07，DRD-18）：输入里有导演台截图 / 预演视频时追加在生成提示词里，
	// 位置在预设之后、营销之前（generation/domain.OrderAppendix）。平台规则，渲染时不给变量。
	"director.reference_guard.capture": {
		Code: "director.reference_guard.capture", Name: "导演台截图参考约束",
		Static: true, MaxChars: 1000,
	},
	"director.reference_guard.preview": {
		Code: "director.reference_guard.preview", Name: "导演台预演视频参考约束",
		Static: true, MaxChars: 1000,
	},
	// 智能剪辑（docs/agent/smartcut dev/01 §5，SCT-08 登记、SCT-27 写内置兜底）：创作要求在这里，
	// 输出格式契约由代码追加（改格式等于改解析器），素材描述与识别稿放 user 消息 —— 所以都不收变量。
	"smartcut.mix.system":       {Code: "smartcut.mix.system", Name: "AI 剪辑：素材混剪", Static: true, MaxChars: 4000},
	"smartcut.board.system":     {Code: "smartcut.board.system", Name: "AI 剪辑：分镜成片", Static: true, MaxChars: 4000},
	"smartcut.talk.system":      {Code: "smartcut.talk.system", Name: "AI 剪辑：口播精剪", Static: true, MaxChars: 4000},
	"smartcut.narr.system":      {Code: "smartcut.narr.system", Name: "AI 剪辑：解说成片", Static: true, MaxChars: 4000},
	"smartcut.edit.system":      {Code: "smartcut.edit.system", Name: "AI 剪辑：追加指令", Static: true, MaxChars: 4000},
	"smartcut.translate.system": {Code: "smartcut.translate.system", Name: "AI 剪辑：字幕翻译", Static: true, MaxChars: 4000},
}

func PromptSpecs() []PromptSpec {
	out := make([]PromptSpec, 0, len(promptSpecs))
	for _, s := range promptSpecs {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

func LookupPromptSpec(code string) (PromptSpec, bool) {
	s, ok := promptSpecs[code]
	return s, ok
}

// ExtractPlaceholders 提取 {{name}} 形态的占位符。
func ExtractPlaceholders(content string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	rest := content
	for {
		i := strings.Index(rest, "{{")
		if i < 0 {
			break
		}
		j := strings.Index(rest[i:], "}}")
		if j < 0 {
			// 有开无闭：运行时会把后面整段都当成占位符名，或原样发给模型。
			return nil, fmt.Errorf("占位符未闭合：%q 附近缺少 }}", preview(rest[i:]))
		}
		name := strings.TrimSpace(rest[i+2 : i+j])
		if name == "" {
			return nil, fmt.Errorf("存在空占位符 {{}}")
		}
		if strings.ContainsAny(name, "{}") {
			return nil, fmt.Errorf("占位符 %q 含非法字符", name)
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
		rest = rest[i+j+2:]
	}
	// 闭合符多于开启符。
	if strings.Contains(rest, "}}") {
		return nil, fmt.Errorf("存在多余的 }}")
	}
	sort.Strings(out)
	return out, nil
}

// ValidatePromptContent 按用途契约校验模板内容。
func ValidatePromptContent(code, content string) error {
	spec, ok := LookupPromptSpec(code)
	if !ok {
		return fmt.Errorf("未知提示词用途: %q", code)
	}
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("内容不能为空")
	}
	if n := len([]rune(content)); spec.MaxChars > 0 && n > spec.MaxChars {
		// ⚠️ 提示词过长会把模型输出撑短甚至撑坏：
		// 旧系统曾因样板段落过长，让模型在 JSON 字符串里写出裸引号，整批响应作废。
		return fmt.Errorf("内容长度 %d 超过上限 %d 字符", n, spec.MaxChars)
	}

	found, err := ExtractPlaceholders(content)
	if err != nil {
		return err
	}
	allowed := map[string]bool{}
	for _, p := range spec.Required {
		allowed[p] = true
	}
	for _, p := range spec.Optional {
		allowed[p] = true
	}

	var unknown []string
	for _, p := range found {
		if !allowed[p] {
			unknown = append(unknown, p)
		}
	}
	if len(unknown) > 0 {
		// 未知占位符不会被替换，会原样发给模型 —— 且**不报错**。
		return fmt.Errorf("存在未知占位符：%s（可用：%s）",
			strings.Join(unknown, "、"), strings.Join(append(append([]string{}, spec.Required...), spec.Optional...), "、"))
	}

	foundSet := map[string]bool{}
	for _, p := range found {
		foundSet[p] = true
	}
	var missing []string
	for _, p := range spec.Required {
		if !foundSet[p] {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("缺少必需占位符：%s（缺失即丢掉对应上下文）", strings.Join(missing, "、"))
	}
	return nil
}

// RenderPrompt 用变量替换占位符。
//
// ⚠️ 缺变量时返回错误而不是留下空串：
// 静默留空会让模型收到一段缺了关键上下文的提示词并照样生成，
// 结果看起来"能跑"但质量莫名其妙地差。
func RenderPrompt(content string, vars map[string]string) (string, error) {
	names, err := ExtractPlaceholders(content)
	if err != nil {
		return "", err
	}
	var missing []string
	for _, n := range names {
		if _, ok := vars[n]; !ok {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("渲染缺少变量：%s", strings.Join(missing, "、"))
	}
	// ⚠️ 按 ExtractPlaceholders 同一个规则逐个替换（04 文档 P-45）：它先 TrimSpace 再认名字，
	// 所以 `{{ state }}` 能通过保存校验；原先这里只替换 `{{state}}`，带空格的那一份原样发给模型，
	// 渲染也不报错 —— 模型收到一段字面量，表现为「质量莫名其妙地差」。
	var b strings.Builder
	rest := content
	for {
		i := strings.Index(rest, "{{")
		if i < 0 {
			b.WriteString(rest)
			break
		}
		j := strings.Index(rest[i:], "}}")
		if j < 0 { // ExtractPlaceholders 已拒未闭合，走不到这里
			b.WriteString(rest)
			break
		}
		b.WriteString(rest[:i])
		b.WriteString(vars[strings.TrimSpace(rest[i+2:i+j])])
		rest = rest[i+j+2:]
	}
	return b.String(), nil
}

func preview(s string) string {
	r := []rune(s)
	if len(r) > 20 {
		return string(r[:20]) + "…"
	}
	return string(r)
}
