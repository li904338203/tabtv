package domain

import (
	"fmt"
	"regexp"
	"strings"
)

// 技能配置的纯规则（AGT-60；AG-13，dev/02 §2.3、design/01 §10）。
//
// 管理端保存技能时在这里一次报完全部问题（一条一条报的话运营要「改一个、保存、再改一个」
// 来回好几轮）。这里只判**形状**：工具名是否在规格表里、步骤序号是否连续、长度与格式；
// 「绑定的模型存在且对得上」「正文能渲染」要查目录 / 调渲染器，在 app 层判。
//
// ⚠️ 工具白名单**只能收窄**：落点在 AllowedTools（与状态允许的集合取交集），
// 这里不需要、也不应该再写一份「规划态不给付费工具」—— 两份判断早晚不一致。
// 技能配置里**没有**任何字段能关掉「付费步骤前必须经用户确认」；steps.checkpoint 只是展示。

// SkillStep 是技能的一个步骤（agent_skill.steps 的一项）。
type SkillStep struct {
	No         int    `json:"no"`
	Name       string `json:"name"`
	Checkpoint bool   `json:"checkpoint"`
}

// SkillBindings 是技能绑定的模型（agent_skill.model_bindings）；空串 = 不绑定（按用户偏好 / 平台默认）。
type SkillBindings struct {
	Video     string `json:"video,omitempty"`
	Image     string `json:"image,omitempty"`
	Reasoning string `json:"reasoning,omitempty"`
}

// SkillDraft 是一次保存要校验的技能内容（已 trim）。
type SkillDraft struct {
	Code        string
	Name        string
	Category    string
	Description string
	Tags        []string
	Body        string
	Steps       []SkillStep
	Tools       []string
	Sort        int
	// Scenes 是技能出现在哪些场景（MKT-D-10）；空 = 只在通用场景。
	Scenes []string
	// PrefillText 是选中技能时预填进输入框的描述（MKT-D-12，用户可见）。
	PrefillText string
}

// 技能场景（MKT-D-10）。⚠️ 空 scenes 等价于只属通用场景；同时属于两边写
// ["general","marketing"]。不认识的值一律拒：存进去一个拼错的场景名不会报错，
// 只会让这个技能在两个页面上都消失。
const (
	SkillSceneGeneral   = "general"
	SkillSceneMarketing = "marketing"
)

// SkillScenes 是全部合法场景（按展示顺序）。
var SkillScenes = []string{SkillSceneGeneral, SkillSceneMarketing}

// ValidSkillScene 判断场景取值是否合法。
func ValidSkillScene(s string) bool {
	for _, x := range SkillScenes {
		if x == s {
			return true
		}
	}
	return false
}

// SkillInScene 判断技能是否出现在 scene 场景的列表里（MKT-AC-15）：
//   - general：scenes 为空或含 general —— 即**不列仅属营销的技能**；
//   - marketing：只列含 marketing 的。
//
// 这是唯一的场景判定，用户端列表、将来的会话内技能加载都按它判。
// SceneKey 把会话场景换成技能场景的取值（空 = 通用）。
func SceneKey(s Scene) string {
	if s == "" {
		return SkillSceneGeneral
	}
	return string(s)
}

func SkillInScene(scenes []string, scene string) bool {
	if scene == SkillSceneGeneral && len(scenes) == 0 {
		return true
	}
	for _, s := range scenes {
		if s == scene {
			return true
		}
	}
	return false
}

// SkillPrefillMax 是预填描述上限（码点，MKT-D-12）；与 agent_skill.prefill_text 的 varchar 一致。
const SkillPrefillMax = 200

// 长度上限（按 Unicode 码点）。与表结构的 varchar 一致；正文上限是为了别把系统提示词撑爆。
const (
	SkillCodeMax        = 64
	SkillNameMax        = 64
	SkillCategoryMax    = 32
	SkillDescriptionMax = 500
	SkillTagMax         = 16
	SkillTagsMax        = 10
	SkillBodyMax        = 20000
	SkillStepsMax       = 20
	SkillStepNameMax    = 64
	SkillSortMax        = 100000
	SkillRefNameMax     = 64
	SkillRefBodyMax     = 50000
)

// skillCodeRe：小写字母开头，小写字母 / 数字 / 下划线。code 会写进会话的 skill_codes，
// 也是模型调用 load_skill 时传的值 —— 不允许空格与大写，免得模型抄错一个字符就加载不上。
var skillCodeRe = regexp.MustCompile(`^[a-z][a-z0-9_]{1,63}$`)

// ValidSkillCode 判断技能 code 格式。
func ValidSkillCode(code string) bool { return skillCodeRe.MatchString(code) }

// ValidateSkill 返回全部问题（没有问题返回空切片）。
//
// registry 是可写进白名单的工具名（传 Tools() 的名字）：不在其中的一律拒 ——
// 写进去一个不存在的名字不会报错，只会让白名单悄悄多收窄一层（那个名字永远匹配不上）。
func ValidateSkill(d SkillDraft, registry []ToolName) []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }

	if !ValidSkillCode(d.Code) {
		add("code「%s」格式不对：小写字母开头，只含小写字母、数字、下划线，2~%d 位", d.Code, SkillCodeMax)
	}
	checkLen := func(label, v string, min, max int) {
		n := runeLen(v)
		switch {
		case n < min:
			add("%s不能为空", label)
		case n > max:
			add("%s %d 字，超过上限 %d", label, n, max)
		}
	}
	checkLen("名称", d.Name, 1, SkillNameMax)
	checkLen("分类", d.Category, 1, SkillCategoryMax)
	checkLen("说明", d.Description, 0, SkillDescriptionMax)
	// 主说明按去掉首尾空白后的长度判「不能为空」（04 文档 P-54①）：纯空白能存进去的话，
	// 技能显示「已加载」、白名单也生效，而系统提示词里跳过了空白正文 —— 模型看不到任何说明。
	if strings.TrimSpace(d.Body) == "" {
		add("主说明不能为空")
	} else {
		checkLen("主说明", d.Body, 1, SkillBodyMax)
	}
	if d.Sort < 0 || d.Sort > SkillSortMax {
		add("排序 %d 超出范围 0~%d", d.Sort, SkillSortMax)
	}

	if len(d.Tags) > SkillTagsMax {
		add("标签 %d 个，最多 %d 个", len(d.Tags), SkillTagsMax)
	}
	seenTag := map[string]bool{}
	for _, t := range d.Tags {
		switch {
		case strings.TrimSpace(t) == "":
			add("标签不能为空串")
		case runeLen(t) > SkillTagMax:
			add("标签「%s」超过 %d 字", t, SkillTagMax)
		case seenTag[t]:
			add("标签「%s」重复", t)
		}
		seenTag[t] = true
	}

	checkLen("预填描述", d.PrefillText, 0, SkillPrefillMax)
	seenScene := map[string]bool{}
	for _, sc := range d.Scenes {
		switch {
		case !ValidSkillScene(sc):
			add("场景「%s」不认识：只能是 %s", sc, strings.Join(SkillScenes, " / "))
		case seenScene[sc]:
			add("场景「%s」重复", sc)
		}
		seenScene[sc] = true
	}

	out = append(out, ValidateSkillSteps(d.Steps)...)
	out = append(out, ValidateSkillTools(d.Tools, registry)...)
	return out
}

// ValidateSkillSteps 步骤序号必须从 1 起连续（按数组顺序）。
//
// ⚠️ 不悄悄重编号：前端已经按顺序重编过，到这里还不连续说明请求被拼错了，
// 悄悄改掉的话「第 3 步是检查点」可能就落到了别的步骤上。
func ValidateSkillSteps(steps []SkillStep) []string {
	var out []string
	if len(steps) > SkillStepsMax {
		out = append(out, fmt.Sprintf("步骤 %d 个，最多 %d 个", len(steps), SkillStepsMax))
	}
	for i, s := range steps {
		if s.No != i+1 {
			out = append(out, fmt.Sprintf("步骤序号必须从 1 起连续：第 %d 项的序号是 %d", i+1, s.No))
		}
		n := runeLen(strings.TrimSpace(s.Name))
		switch {
		case n == 0:
			out = append(out, fmt.Sprintf("第 %d 步缺少名称", i+1))
		case n > SkillStepNameMax:
			out = append(out, fmt.Sprintf("第 %d 步名称超过 %d 字", i+1, SkillStepNameMax))
		}
	}
	return out
}

// ValidateSkillTools 白名单每项必须是已注册的工具名、且不重复。空白名单 = 不收窄，合法。
func ValidateSkillTools(tools []string, registry []ToolName) []string {
	known := make(map[string]bool, len(registry))
	for _, n := range registry {
		known[string(n)] = true
	}
	var out []string
	seen := map[string]bool{}
	for _, t := range tools {
		switch {
		case !known[t]:
			out = append(out, fmt.Sprintf("工具白名单里的「%s」不是已注册的工具", t))
		case seen[t]:
			out = append(out, fmt.Sprintf("工具白名单里的「%s」重复", t))
		}
		seen[t] = true
	}
	return out
}

// ToolNames 是 Tools() 的名字（白名单可选项，按声明顺序）。
func ToolNames() []ToolName {
	specs := Tools()
	out := make([]ToolName, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.Name)
	}
	return out
}

// ValidateSkillRef 校验一份附属文档的名称与正文长度（渲染在 app 层）。
func ValidateSkillRef(name, body string) []string {
	var out []string
	switch n := runeLen(name); {
	case n == 0:
		out = append(out, "附属文档名称不能为空")
	case n > SkillRefNameMax:
		out = append(out, fmt.Sprintf("附属文档名称超过 %d 字", SkillRefNameMax))
	}
	switch n := runeLen(body); {
	case strings.TrimSpace(body) == "":
		out = append(out, "附属文档正文不能为空")
	case n > SkillRefBodyMax:
		out = append(out, fmt.Sprintf("附属文档正文 %d 字，超过上限 %d", n, SkillRefBodyMax))
	}
	return out
}

func runeLen(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}
