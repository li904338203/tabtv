package domain

import (
	"fmt"
	"strings"
	"unicode"
)

// TagTalk 是「口播」标签：由管理端显式打上，校验据此触发，**不按名称猜**（MKT-F-76）。
const TagTalk = "口播"

// TalkDisclaimer 是口播类选项 / 技能面向用户的文案必须包含的提示句（MK-13）。
// 规划文档与确认卡的 notices 也用这一句（MKD-12）—— 别处不要再抄一份字面量。
const TalkDisclaimer = "画面为人物对镜讲解，不保证口型与语音逐字同步"

// BannedTalkWords 是禁用词：出现在任何面向用户的字段里都**拦截**并点名字段，
// 不论有没有「口播」标签（MK-5 / MK-13：不做数字人、口型驱动、声音克隆）。
//
// ⚠️ 这是**代码常量**，与 04 §3.3「数字人类」一致 —— 它是合规底线，不是运营调优项，
// 调整要改代码。⚠️ 全仓只有这一份（MKD-12）：技能保存经 agent 的 `CopyChecker` 端口
// 调到 CheckTalkCopy（MKT-T-05），管理端前端只用共享 fixture 做体验性预检。
var BannedTalkWords = []string{"数字人", "对口型", "口型同步", "真人克隆"}

// CautionWords 是 04 §3.3 的合规用语词表（绝对化 / 功效、冒充背书、伪造体验）：
// 只**告警**不拦截（dev/01 §8：判错两个方向代价不对称，同 PARAM-09 的思路）。
//
// ⚠️ 有意没收单字「最」与「第一」：它们在正常文案里随处可见（「最后一镜」「第一视角」），
// 收进来的话几乎每条保存都带一条告警 —— 告警多到没人看，真正该看的那条就被淹没了。
var CautionWords = []string{
	// 绝对化 / 功效
	"唯一", "顶级", "100%", "根治", "速效", "美白", "减肥",
	// 冒充背书
	"检测认证", "专家推荐", "医生同款", "实验室证明",
	// 伪造体验
	"真实用户", "亲测", "路人真实反馈",
}

// CheckTalkCopy 检查面向用户的文案（MKT-F-76、MKT-AC-29）。
//
//   - fields：字段名 → 文案（调用方只传**用户看得到**的字段，见 UserVisibleFields；
//     技能传名称、说明、预填描述、详情）。
//   - 禁用词 → error，**点名字段与词**；同一字段含多个禁用词各报一条。
//   - 带「口播」标签而**没有任何一个字段**含 TalkDisclaimer → warn（Field 为空：
//     这句写在哪个字段都行，名称放不下它，通常写在说明里）。
//   - 合规用语词表 → warn，点名字段与词。
//
// 匹配前去掉空白与零宽字符：`数 字 人` 与 `数\u200b字人` 同样被拦。
// 字段按名字排序遍历，输出顺序稳定（审计 diff 与 fixture 比对都依赖它）。
func CheckTalkCopy(fields map[string]string, tags []string) (errs, warns []Issue) {
	keys := sortedKeys(fields)
	hasDisclaimer := false
	disclaimer := normalizeCopy(TalkDisclaimer)
	for _, k := range keys {
		text := normalizeCopy(fields[k])
		for _, w := range BannedTalkWords {
			if strings.Contains(text, w) {
				errs = append(errs, errIssue(k, IssueBannedWord, fmt.Sprintf("「%s」含禁用词「%s」，营销文案不得出现数字人 / 口型驱动 / 声音克隆类表述", k, w)))
			}
		}
		for _, w := range CautionWords {
			if strings.Contains(text, normalizeCopy(w)) {
				warns = append(warns, warnIssue(k, IssueCautionWord, fmt.Sprintf("「%s」含合规风险用语「%s」，请确认是否必要", k, w)))
			}
		}
		if strings.Contains(text, disclaimer) {
			hasDisclaimer = true
		}
	}
	if HasTag(tags, TagTalk) && !hasDisclaimer {
		warns = append(warns, warnIssue("", IssueMissingNote, "带「口播」标签的文案须写明「"+TalkDisclaimer+"」"))
	}
	return errs, warns
}

// normalizeCopy 去掉空白与零宽字符，并把半角逗号统一成全角（提示句里那个逗号常被输入法换掉）。
func normalizeCopy(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case unicode.IsSpace(r), r == '\u200b', r == '\u200c', r == '\u200d', r == '\ufeff':
			continue
		case r == ',':
			b.WriteRune('，')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
