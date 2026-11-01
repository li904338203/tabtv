package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
)

// VideoModelInfo 是系统提示词「当前视频模型」一节的事实来源（模型目录，AGT-05）。
type VideoModelInfo struct {
	// IsVideo 为 false：模型存在但不是视频模型。
	IsVideo bool
	// MaxDurationSec 为 nil = 后台没配置单次最长时长。
	MaxDurationSec *int
	MultiShot      bool
	NativeAudio    bool
}

// VideoModels 查视频模型的能力。模型不存在时返回错误。
type VideoModels interface {
	VideoModel(ctx context.Context, code string) (VideoModelInfo, error)
}

// ModelBrief 是一个此刻可用的模型（「智能匹配」时列给推理模型挑）。
type ModelBrief struct {
	Code string
	Name string
	// Operations 该模型在这类产出下能用的生成方式（操作码）。
	Operations []string
}

// ModelDirectory 列出某产出类型（video / image）下此刻可用的模型：已启用、Provider 就绪、
// 付费主体的会员等级够、至少有一个能用的生成方式（04 文档 P-59）。
type ModelDirectory interface {
	UsableModels(ctx context.Context, output string, memberLevelID int64) ([]ModelBrief, error)
}

// ReasoningDefaults 给出平台默认的推理模型：按这个会员等级**报得出价**的第一个（04 文档 P-13）。
// 一个都报不出价时返回第一个可用的（报价会把原因带给用户：等级不够 / 缺价）；一个都没有时返回空串与 nil。
//
// ⚠️ 原先只取排序第一个，不看会员等级与价格：第一个恰好需要更高会员时，没选模型的用户每一轮都 quote_failed，
// 而列表后面明明有他能用的。
type ReasoningDefaults interface {
	DefaultReasoningModel(ctx context.Context, memberLevelID int64) (string, error)
}

// resolveVideoModel 会话偏好里的视频模型（prefs.video.model）；空 = 智能匹配 / 未指定。
func resolveVideoModel(s repo.Session) string {
	var p struct {
		Video struct {
			Model string `json:"model"`
		} `json:"video"`
	}
	if len(s.Prefs) > 0 && json.Unmarshal(s.Prefs, &p) == nil {
		return strings.TrimSpace(p.Video.Model)
	}
	return ""
}

// VideoModelSection 拼出系统提示词里「当前视频模型」一节。
//
// 起因（AGT-30 发现）：单段 / 多段的分界要从模型目录读、不能写死秒数，但模型此前既看不到
// 会话选的视频模型，也看不到它的单次最长时长 —— 技能正文只能写「以模型目录为准」，而模型
// 无从查起，只能猜。这一节由**代码**按目录事实拼（格式契约，不进技能正文），模型照着判断。
//
// ⚠️ 未配置或查不到时**明说不知道**，不编一个数：猜错的代价是规划出一条上游拒绝的长视频，
// 而那要到受理时才暴露（钱已经按规划报过价了）。
func VideoModelSection(code string, info *VideoModelInfo, lookupErr error) string {
	if code == "" {
		return "用户没有指定视频模型（智能匹配）。规划视频前用 quote 按计划时长报价确认可用；" +
			"拿不准用哪个模型时用 ask_user 询问，不要自行假设模型能力。"
	}
	if lookupErr != nil || info == nil {
		return fmt.Sprintf("用户指定的视频模型是 %s，但目前查不到它的能力信息。"+
			"不要假设它的单次最长时长；规划前用 quote 报价确认，或用 ask_user 询问。", code)
	}
	if !info.IsVideo {
		return fmt.Sprintf("用户偏好里的 %s 不是视频模型。生成视频前用 ask_user 让用户重新选择视频模型。", code)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "- 模型：%s\n", code)
	if info.MaxDurationSec != nil {
		fmt.Fprintf(&b, "- 单次最长时长：%d 秒\n", *info.MaxDurationSec)
	} else {
		b.WriteString("- 单次最长时长：后台未配置（不要假设，规划前用 quote 报价确认，或用 ask_user 询问）\n")
	}
	fmt.Fprintf(&b, "- 支持一次生成多个镜头：%s\n", yesNo(info.MultiShot))
	fmt.Fprintf(&b, "- 自带音轨：%s\n", yesNo(info.NativeAudio))
	// ⚠️ 一支视频一次生成，不自动拆段（design/01 修订块 R-1，会话 107419662995685376）。
	// 原先「不支持多镜头」或「上限没配」就写「按多段规划」—— 目录里 is_multi_shot 全是默认的否，
	// 于是每个会话都被要求拆段，15 秒的视频被拆成 5 秒 + 10 秒两次生成。多镜头现在只决定提示词怎么写。
	b.WriteString("- 一支视频一次生成：一个视频节点、一次生成，镜头切换写进同一段提示词")
	if !info.MultiShot {
		b.WriteString("（目录标注不支持多镜头：把几个画面写成一个连续镜头里的动作与运镜变化）")
	}
	b.WriteString("。不要把一支视频拆成几段分别生成再拼接。\n")
	// 上限由 domain.SingleSegmentLimit 从目录事实算出（AGT-50）：改目录里的最长时长，这里随之变化。
	caps := domain.ModelCaps{MaxDurationSec: info.MaxDurationSec, MultiShot: info.MultiShot}
	if limit, ok := domain.SingleSegmentLimit(caps); ok {
		fmt.Fprintf(&b, "- 计划时长超过 %d 秒时，先用 ask_user 让用户选：缩短到 %d 秒以内，或换一个单次时长够长的模型；"+
			"用户明确要做更长的片子时，才走 脚本 → 分镜 → 资产 → 提示词 → 分镜图组 → 视频组 → 时间线装配 → 渲染 的多段流程。",
			limit, limit)
	} else {
		b.WriteString("- 单次最长时长没有配置：规划时用 quote 按计划时长报价确认；报不出价且原因指向时长时，" +
			"用 ask_user 让用户选缩短或换模型，不要自行拆段。")
	}
	return b.String()
}

func yesNo(v bool) string {
	if v {
		return "是"
	}
	return "否"
}

// maxListedModels 是「智能匹配」时最多列出几个模型（再多推理模型也挑不过来，还撑长提示词）。
const maxListedModels = 12

// VideoModelExtras 是「当前视频模型」一节的补充（04 文档 P-59）：
//
//   - 选了视频模型：列出它能用的生成方式与各自收什么输入、参数范围。原先只写时长和多镜头，
//     模型不知道这个模型有没有图生视频，带了参考图也照样写成文生视频；
//   - 没选（智能匹配）：列出此刻可用的视频模型。原先只说「用 quote 确认」，而 quote / generate
//     都要填 modelCode，模型手上一个 code 都没有，只能猜或者反问用户。
//
// ⚠️ 不写以「- 模型：」开头的行：假上游（scripts/fake-upstream）按它认「当前视频模型」。
func VideoModelExtras(code string, fits []domain.OperationFit, usable []ModelBrief) string {
	var b strings.Builder
	if code != "" {
		if list := domain.DescribeFits("video", fits); list != "" {
			b.WriteString("- 能用的生成方式（operationCode 要与输入一致：带参考图用收图片的方式，纯文字才用 text2video）：\n")
			b.WriteString(indentLines(list, "  "))
		}
		return b.String()
	}
	if len(usable) == 0 {
		return ""
	}
	b.WriteString("可以选的视频模型（modelCode（名称）：能用的生成方式）：\n")
	b.WriteString(modelLines(usable))
	return b.String()
}

// PrefsSection 是「## 创作偏好」一节（04 文档 P-60）：画幅 / 清晰度 / 时长（spec：偏好或问答卡里确认过的）与图片模型。
//
// 原先服务端只读偏好里的推理模型与视频模型，其余全部丢掉 —— 用户选了 9:16、1080p、某个图片模型，
// 对 Agent 没有任何影响。偏好是「智能」又没问过的项（spec.Missing）写成「规划视频之前必须先确认」
// （会话 107419662995685376）。什么都没有、也没有可列的图片模型时为空串（不写这一节）。
func PrefsSection(prefs json.RawMessage, spec VideoSpecState, imageUsable []ModelBrief) string {
	p := ParseGenPrefs(prefs)
	var lines []string
	if v := specText(spec.Ratio, spec.Answered[domain.SpecRatio]); v != "" {
		lines = append(lines, "- 画幅："+v+"（视频与图片的 aspect_ratio 按它填）")
	}
	if v := specText(spec.Resolution, spec.Answered[domain.SpecResolution]); v != "" {
		lines = append(lines, "- 视频清晰度："+v+"（resolution 按它填）")
	}
	if spec.Duration > 0 {
		lines = append(lines, fmt.Sprintf("- 视频时长：%d 秒（一支视频就是这么长、一次生成；超过所选模型的单次最长时长时，"+
			"用 ask_user 让用户选缩短或换模型，不要拆成几段）", spec.Duration))
	} else if a := spec.Answered[domain.SpecDuration]; a != "" {
		lines = append(lines, "- 视频时长：用户回答「"+a+"」（按它定一支视频的时长、一次生成，不要拆成几段）")
	}
	if m := p.ImageModel; m != "" {
		lines = append(lines, "- 图片模型："+m+"（出图、分镜图、资产图都用它）")
	} else if len(imageUsable) > 0 {
		lines = append(lines, "- 图片模型：未指定（智能匹配），从下面挑：\n"+indentLines(modelLines(imageUsable), "  "))
	}
	if len(spec.Missing) > 0 {
		lines = append(lines, missingSpecLine(spec.Missing))
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n与模型能力冲突时以模型能力为准，并在规划里说明。"
}

// specText 是一项规格写给模型的值：取得出具体值写值，只有回答原文时照原文写（让模型自己理解）。
func specText(value, raw string) string {
	switch {
	case value != "":
		return value
	case raw != "":
		return "用户回答「" + raw + "」"
	}
	return ""
}

// missingSpecLine 是「还没确认的规格」那一行：题目 id 写死给模型，服务端按 id 认「问过没有」。
func missingSpecLine(missing []string) string {
	labels := make([]string, 0, len(missing))
	for _, k := range missing {
		labels = append(labels, domain.SpecLabel(k))
	}
	return "- " + strings.Join(labels, "、") + "：用户没有指定（输入框偏好里是「智能」，或会话没有设置）。规划视频之前必须先用 ask_user 跟用户确认这几项：" +
		"kind=questions，与其他要问的问题合在同一张卡里；题目 id 分别用 " + strings.Join(missing, " / ") + "，单选；" +
		"选项只列所选视频模型支持的值，推荐值放第一个并在 label 里注明「推荐」。按用户选的值规划、报价和生成，不要自己定；" +
		"没确认就提交带视频的规划会被退回。"
}

// VideoSpecState 是本会话视频规格的生效值：画幅 / 清晰度 / 时长。
//
// 用户在输入框偏好里选定的值优先；选的是「智能」（偏好里没有这个字段）时，取问答卡里按固定题目 id
// （domain.SpecKeys）问到的回答。两边都没有的项进 Missing：规划视频之前 Agent 必须用 ask_user 确认，
// 营销会话里没确认就 submit_plan 会被退回（会话 107419662995685376：用户一项都没选，Agent 自己定了 9:16 · 15 秒 · 720p）。
type VideoSpecState struct {
	Ratio      string // 归一后的画幅；空 = 不知道（没选，或回答里取不出具体值）
	Resolution string // 归一后的清晰度
	Duration   int    // 秒；0 = 不知道
	// Answered 问答卡里的回答原文（题目 id → 原文）。取不出具体值时也写进提示词，让模型照原文理解。
	Answered map[string]string
	// Missing 偏好是「智能」又还没在问答卡里回答过的题目 id（domain.SpecKeys 的子集，按其顺序）。
	Missing []string
}

// ResolveVideoSpec 合成规格生效值：偏好 → 问答卡里的回答 → 缺。
func ResolveVideoSpec(p GenPrefs, answers map[string]string) VideoSpecState {
	st := VideoSpecState{Answered: map[string]string{}}
	pick := func(key, pref string, norm func(string) string) string {
		if pref != "" {
			return norm(pref)
		}
		if a := answers[key]; a != "" {
			st.Answered[key] = a
			return norm(a)
		}
		st.Missing = append(st.Missing, key)
		return ""
	}
	st.Ratio = pick(domain.SpecRatio, p.Ratio, func(v string) string {
		if r := domain.NormalizeRatio(v); r != "" {
			return r
		}
		return ""
	})
	st.Resolution = pick(domain.SpecResolution, p.Resolution, domain.NormalizeResolution)
	if d := pick(domain.SpecDuration, p.Duration, func(v string) string {
		if n := domain.ParseDurationSec(v); n > 0 {
			return strconv.Itoa(n)
		}
		return ""
	}); d != "" {
		st.Duration, _ = strconv.Atoi(d)
	}
	return st
}

// GenPrefs 是会话偏好里与生成参数有关的几项（空 = 没设）。
//
// 写给模型看的「创作偏好」一节（PrefsSection）与 generate 给新建节点补默认值（agent/tools）读的是
// 同一份解析：各解析一份的话，两边会对「偏好里写了什么」各说各的（比如一边认数字时长、一边不认）。
type GenPrefs struct {
	Ratio      string // video.ratio：视频与图片的画幅
	Resolution string // video.resolution
	Duration   string // video.duration（秒）
	ImageModel string // image.model
}

// ParseGenPrefs 解析会话偏好；解不开当作什么都没设。
func ParseGenPrefs(prefs json.RawMessage) GenPrefs {
	var p struct {
		Video struct {
			Ratio      any `json:"ratio"`
			Resolution any `json:"resolution"`
			Duration   any `json:"duration"`
		} `json:"video"`
		Image struct {
			Model string `json:"model"`
		} `json:"image"`
	}
	if len(prefs) > 0 {
		_ = json.Unmarshal(prefs, &p)
	}
	return GenPrefs{Ratio: prefValue(p.Video.Ratio), Resolution: prefValue(p.Video.Resolution),
		Duration: prefValue(p.Video.Duration), ImageModel: strings.TrimSpace(p.Image.Model)}
}

func prefValue(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case float64:
		if x > 0 {
			return fmt.Sprintf("%g", x)
		}
	}
	return ""
}

func modelLines(list []ModelBrief) string {
	var lines []string
	for i, m := range list {
		if i >= maxListedModels {
			lines = append(lines, fmt.Sprintf("- ……另有 %d 个（不常用，需要时问用户）", len(list)-maxListedModels))
			break
		}
		label := m.Code
		if n := strings.TrimSpace(m.Name); n != "" && n != m.Code {
			label += "（" + n + "）"
		}
		lines = append(lines, "- "+label+"："+strings.Join(m.Operations, "、"))
	}
	return strings.Join(lines, "\n")
}

func indentLines(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
