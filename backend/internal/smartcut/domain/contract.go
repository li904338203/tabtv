package domain

import (
	"fmt"
	"strings"
)

// 输出格式契约（SCD-25）：代码固定追加在系统提示词（`smartcut.{mode}.system`）之后。
//
// ⚠️ **格式写在代码里，创作要求放进 prompt_template**：改格式等于改解析器（draft.go），不能交给运营后台；
// 选段原则、删句原则、翻译口吻这些创作要求在模板里调，不发版（CLAUDE.md §5.7）。
// ⚠️ 契约里的字段名、取值与上限都取自解析器用的常量，不手抄（`TestOutputContract_与解析器一致` 钉住）。
// ⚠️ 契约里不写「分镜」二字：假上游看到它就回一份分镜 JSON（同导演台 DRT-30 的教训）；
// 假上游按标题与第二行的模式码分辨请求（`## 输出格式（智能剪辑）` + `模式：mix`）。

// ContractHeading 契约的固定标题（假上游据此识别智能剪辑的请求）。
const ContractHeading = "## 输出格式（智能剪辑）"

// contractModeNames 第二行的模式说明。
var contractModeNames = map[string]string{
	ModeMix:       "素材混剪",
	ModeBoard:     "按镜头清单装配成片",
	ModeTalk:      "口播精剪",
	ModeNarr:      "解说成片",
	ModeEdit:      "追加修改",
	ModeTranslate: "字幕翻译",
}

// OutputContract 按模式返回输出格式契约全文。同一版本的代码每次返回同一段文字。
func OutputContract(mode string) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	w("%s\n模式：%s（%s）\n\n", ContractHeading, mode, contractModeNames[mode])
	w("只输出一个 JSON 对象：不要解释，不要反问，不要用代码块包起来。")
	w("**不要写任何时间**（startMs、durationMs 之类）：时间由系统按素材编排，写了也会被忽略。")
	w("信息不够时按常理安排，并把你的取舍写进 notes，每条一句（最多 %d 条）。\n\n", MaxDraftNotes)
	switch mode {
	case ModeMix:
		w("### segments：按播放顺序列出要用的片段（至少一段，最多 %d 段）\n\n", MaxDraftSegments)
		w("- clip：素材编号（素材清单里的「编号」，例如 \"3\"、\"3-1\"），只能用视频或图片。\n")
		writeTake(w)
		w("- role：作用。hook 开头抓人的钩子；body 主体；climax 高潮；end 结尾。钩子会放在最前、结尾放在最后。\n")
		w("- weight：1~3，越大分到的时长越多；要突出的重点写 3。\n")
		writeTransition(w, "这一段")
		w("- subtitle：这一段的字幕（可省，不超过 %d 个字）。\n", MaxSubtitleRunes)
		w("- reason：为什么这样用，一句话（不超过 %d 个字）。\n", MaxReasonRunes)
		w("- 同一段素材可以取不同的部分用两次；同一部分不要重复。\n\n")
		writeExtras(w, true)
		w("示例：\n%s\n", mixExample)
	case ModeBoard:
		w("镜头的顺序与每个镜头取用的部分由系统按镜头清单装配，不能改；你只决定包装。\n\n")
		w("### segments：给镜头配转场与字幕（可省；没写到的镜头硬切、字幕用台词）\n\n")
		w("- clip：镜头编号（镜头清单里的 b1、b2……）。\n")
		writeTransition(w, "这个镜头")
		w("- subtitle：这个镜头的字幕（不超过 %d 个字）；不写时用台词；写空字符串表示这个镜头不要字幕。\n", MaxSubtitleRunes)
		w("- reason：为什么这样处理，一句话。\n\n")
		writeExtras(w, true)
		w("示例：\n%s\n", boardExample)
	case ModeTalk:
		w("输入是按句编号的口播识别稿（s1、s2……）。\n\n")
		w("- keep：要保留的句子，按顺序写成区间 [{\"from\": \"s1\", \"to\": \"s4\"}]，单独一句写 {\"from\": \"s6\", \"to\": \"s6\"}。没写进 keep 的句子都会被剪掉。\n")
		w("- drop：删掉的句子与原因 [{\"sentence\": \"s5\", \"reason\": \"重复\"}]，只作说明。\n")
		w("- subtitle：要不要自动加字幕，true / false，缺省 true。\n")
		w("- 停顿与口癖（嗯、啊）由系统按字级时间剪，不用写。\n\n")
		w("示例：\n%s\n", talkExample)
	case ModeNarr:
		w("输入是按句编号的配音稿（s1、s2……，带每句的时长）与素材清单。画面以配音为准铺满整段配音。\n\n")
		w("- lines：每句配什么画面 [{\"sentence\": \"s1\", \"clips\": [\"2\", \"5\"], \"take\": \"head\"}]。\n")
		w("  - clips：这一句按顺序要用的素材编号（视频或图片），这句的时长会按顺序平分给它们。\n")
		w("  - take：从素材的哪里取：full 从头开始；head 开头；middle 中间；tail 结尾；{\"aroundSec\": 2.5} 围绕第 2.5 秒。\n")
		w("  - 没写到的句子沿用上一句的画面。\n")
		w("- music：配乐 {\"clip\": \"音频的编号\"}（可省）。\n")
		w("- clipNotes：看图理解的素材描述 {\"编号\": \"画面里有什么\"}（可省，每条不超过 %d 个字）。\n\n", MaxClipNoteRunes)
		w("示例：\n%s\n", narrExample)
	case ModeEdit:
		w("输入是当前时间线的片段清单（c1、c2……，带来源素材编号、取用部分与「谁改的」）与文字清单（t1、t2……）。")
		w("只输出要做的修改，没改的片段不用列。\n\n")
		w("### ops：按顺序执行的修改（最多 %d 处）\n\n", MaxDraftOps)
		w("- {\"op\": \"trim\", \"clip\": \"c3\", \"take\": \"head\", \"ratio\": 0.6}：只保留 c3 当前取用部分的一部分。take：head 前面、middle 中间、tail 后面、{\"aroundSec\": 秒} 围绕素材的第几秒；ratio 是保留的比例（0.1~1）。take 写 full 表示改用这段素材的整段。\n")
		w("- {\"op\": \"swap\", \"a\": \"c1\", \"b\": \"c4\"}：交换两段的位置。\n")
		w("- {\"op\": \"replace\", \"clip\": \"c2\", \"with\": \"5\"}：把 c2 换成素材 5（视频或图片，尽量保持原来的长度）。\n")
		w("- {\"op\": \"speed\", \"clip\": \"c2\", \"value\": 1.25}：改速度（0.25~4）；clip 写 \"*\" 表示全部片段。\n")
		w("- {\"op\": \"transition\", \"clip\": \"c2\", \"kind\": \"dissolve\", \"durationSec\": 0.5}：改 c2 与上一段之间的转场；kind 取 cut / fade / dissolve / slide_left / wipe，durationSec 可省（0.1~3）。\n")
		w("- {\"op\": \"text\", \"text\": \"新品上市\", \"clip\": \"c1\"}：加一段文字，跟着 c1 出现（clip 可省，省略时放在开头）；改已有的文字写 {\"op\": \"text\", \"target\": \"t1\", \"text\": \"……\"}。\n")
		w("- {\"op\": \"insert\", \"with\": \"5\", \"after\": \"c2\", \"take\": \"head\"}：在 c2 后面插入素材 5（after 也可以写 start / end）。\n")
		w("- {\"op\": \"delete\", \"clip\": \"c3\"}：删掉 c3。\n")
		w("- 清单里标为「你改过」的片段不要改，系统也不会改它们。\n")
		w("- summary：一句话说明这次改了什么。\n\n")
		w("示例：\n%s\n", editExample)
	case ModeTranslate:
		w("输入是按序号排列的字幕（1、2……）与目标语言。\n\n")
		w("- items：[{\"n\": 1, \"text\": \"译文\"}]，n 与输入的序号一一对应；每条都要有，不要合并、拆分或跳过。\n")
		w("- 每条不超过 500 个字。\n\n")
		w("示例：\n%s\n", translateExample)
	}
	return strings.TrimRight(b.String(), "\n")
}

func writeTake(w func(string, ...any)) {
	w("- take：取哪一部分。full 整段；head 开头；middle 中间；tail 结尾；{\"aroundSec\": 2.5} 围绕第 2.5 秒。")
	w("可以另写 ratio（0.1~1）表示取多少，缺省取 %d%%（不少于 %s 秒）。\n", TakePartPermille/10, secText(TakePartMinMs))
}

func writeTransition(w func(string, ...any), what string) {
	w("- transition：从上一段切到%s的方式。cut 硬切；fade 淡入淡出；dissolve 叠化；slide_left 左滑；wipe 划像。第一段与钩子写 cut。\n", what)
}

func writeExtras(w func(string, ...any), title bool) {
	w("### 其它字段（都可省）\n\n")
	if title {
		w("- title：开头标题 {\"text\": \"不超过 %d 个字\", \"durationSec\": %s~%s}。\n", MaxTitleRunes, secText(TitleMinMs), secText(TitleMaxMs))
	}
	w("- music：配乐 {\"clip\": \"音频的编号\"}，只能是音频素材。\n")
	w("- warnings：素材本身的问题（例如画面模糊、不够用），每条一句。\n")
	w("- clipNotes：看图理解的素材描述 {\"编号\": \"画面里有什么\"}，每条不超过 %d 个字。\n\n", MaxClipNoteRunes)
}

const mixExample = `{"segments": [
  {"clip": "3", "take": "head", "role": "hook", "weight": 3, "transition": "cut", "subtitle": "一口入魂", "reason": "产品特写最抓人"},
  {"clip": "1", "take": "full", "role": "body", "weight": 2, "transition": "dissolve", "reason": "交代使用场景"},
  {"clip": "5", "take": {"aroundSec": 4}, "role": "end", "weight": 2, "transition": "fade", "reason": "收在产品与标志上"}
],
 "title": {"text": "夏日新品", "durationSec": 2},
 "music": {"clip": "8"},
 "notes": ["素材 2 与素材 1 镜头重复，没有用"],
 "clipNotes": {"5": "白色桌面上的产品与标志"}}`

const boardExample = `{"segments": [
  {"clip": "b1", "transition": "cut"},
  {"clip": "b2", "transition": "dissolve", "subtitle": "那天的雨很大"},
  {"clip": "b3", "transition": "fade", "subtitle": ""}
],
 "title": {"text": "雨夜", "durationSec": 2},
 "music": {"clip": "4"}}`

const talkExample = `{"keep": [{"from": "s1", "to": "s3"}, {"from": "s5", "to": "s9"}],
 "drop": [{"sentence": "s4", "reason": "和 s5 重复，s5 说得更顺"}],
 "subtitle": true}`

const narrExample = `{"lines": [
  {"sentence": "s1", "clips": ["2"], "take": "head"},
  {"sentence": "s2", "clips": ["5", "6"], "take": "middle"},
  {"sentence": "s4", "clips": ["3"], "take": {"aroundSec": 6}}
],
 "notes": ["s3 沿用 s2 的画面"]}`

const editExample = `{"ops": [
  {"op": "trim", "clip": "c3", "take": "head", "ratio": 0.6},
  {"op": "swap", "a": "c1", "b": "c4"},
  {"op": "transition", "clip": "c2", "kind": "dissolve"}
],
 "summary": "第 3 段改短，开头换成产品镜头"}`

const translateExample = `{"items": [{"n": 1, "text": "Summer is here"}, {"n": 2, "text": "Try it today"}]}`
