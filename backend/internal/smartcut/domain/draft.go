package domain

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	tl "github.com/li904338203/tabtv/backend/internal/timeline/domain"

	"github.com/li904338203/tabtv/backend/internal/platform/lenientjson"
)

// 方案的解析与校验（dev/01 §4.2、SC-AC-31）。
//
// AI 只出「引用编号」的方案：用哪几段、顺序、每段取哪部分、起什么作用、转场、字幕与文字、配乐、理由（SC-F-81）。
// 这里逐项过白名单：编号必须在素材条里、枚举必须合法、取段比例在 0.1~1。不合规的项丢掉并记下原因 ——
// 原因既给用户看（被跳过的段），也原样回灌给模型修一次（RepairHint）。
//
// ⚠️ 模型写出的任何时间字段（startMs 之类）一律忽略，不报错（SC-AC-31）：时间由编译算，模型给的时间
// 与素材的真实时长对不上是常态。忽略了什么记进 Ignored，不进 Dropped（那不是需要修的错）。
// ⚠️ 文字（字幕、标题、理由）原样保留，只校验长度上限（SCT-26 风险条）：截断字幕等于替用户改了台词，
// 超长的去掉并说明。理由与说明是解释性的文字，超长按上限截断。

// 解析的上限。
const (
	MaxDraftSegments  = 100
	MaxDraftOps       = 50
	MaxSubtitleRunes  = 100
	MaxTitleRunes     = 40
	MaxReasonRunes    = 200
	MaxNoteRunes      = 200
	MaxDraftNotes     = 10
	MaxClipNoteRunes  = 300
	DraftSnippetRunes = 300
	maxRepairLines    = 20
)

// Draft 是解析、校验之后的方案。按模式只填对应的字段。
type Draft struct {
	Mode string `json:"mode"`
	// 素材混剪 / 分镜成片。
	Segments []Segment `json:"segments,omitempty"`
	Title    *Title    `json:"title,omitempty"`
	Music    string    `json:"music,omitempty"`
	// 口播精剪：保留哪些句子（from~to，闭区间）、删掉哪些（只作说明）、要不要字幕（nil = 要）。
	Keep     []SentenceRange `json:"keep,omitempty"`
	Drop     []SentenceDrop  `json:"drop,omitempty"`
	Subtitle *bool           `json:"subtitle,omitempty"`
	// 解说成片：每句配哪些画面。
	Lines []NarrLine `json:"lines,omitempty"`
	// 追加指令。
	Ops     []EditOp `json:"ops,omitempty"`
	Summary string   `json:"summary,omitempty"`
	// 翻译。
	Items []TranslateItem `json:"items,omitempty"`

	Notes    []string `json:"notes,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	// ClipNotes 模型对看图素材的描述（写进缓存），按编号排序。
	ClipNotes []ClipNote `json:"clipNotes,omitempty"`
}

// Segment 是方案里的一段（素材混剪 / 分镜成片）。Transition 是从上一段切到这一段的方式。
type Segment struct {
	Clip       string `json:"clip"`
	Take       Take   `json:"take"`
	Role       string `json:"role"`
	Weight     int    `json:"weight"`
	Transition string `json:"transition"`
	Subtitle   string `json:"subtitle,omitempty"`
	// SubtitleOff 模型明确写了空字幕（分镜成片里表示这个镜头不要台词字幕）。
	SubtitleOff bool   `json:"subtitleOff,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// Title 开头标题。
type Title struct {
	Text       string `json:"text"`
	DurationMs int    `json:"durationMs,omitempty"`
}

// SentenceRange 口播保留的一段句子（句号，闭区间）。
type SentenceRange struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// SentenceDrop 口播删掉的一句与原因（只作说明：没写进 keep 的句子都不保留）。
type SentenceDrop struct {
	Sentence string `json:"sentence"`
	Reason   string `json:"reason,omitempty"`
}

// NarrLine 解说的一句配哪些画面（按顺序均分句长）。
type NarrLine struct {
	Sentence string   `json:"sentence"`
	Clips    []string `json:"clips"`
	Take     Take     `json:"take"`
}

// 追加指令的原子操作（dev/01 §4.2）。
const (
	OpTrim       = "trim"
	OpSwap       = "swap"
	OpReplace    = "replace"
	OpSpeed      = "speed"
	OpTransition = "transition"
	OpText       = "text"
	OpInsert     = "insert"
	OpDelete     = "delete"
)

// EditOp 是一个原子操作。字段按操作取用：
//   - trim：Clip（cN）、Take、Take.RatioPermille；
//   - swap：A、B；
//   - replace：Clip、With（素材编号）、Take（可省）；
//   - speed：Clip（cN 或 *）、SpeedPct；
//   - transition：Clip、Kind（cut / fade / …）、DurationMs（可省）；
//   - text：Text，Target（tN：改已有的文字）或 Clip（cN：跟着这一段出现，可省）；
//   - insert：With（素材编号）、After（cN / start / end，默认 end）、Take（可省）；
//   - delete：Clip。
type EditOp struct {
	Op         string `json:"op"`
	Clip       string `json:"clip,omitempty"`
	A          string `json:"a,omitempty"`
	B          string `json:"b,omitempty"`
	With       string `json:"with,omitempty"`
	After      string `json:"after,omitempty"`
	Take       *Take  `json:"take,omitempty"`
	SpeedPct   int    `json:"speedPct,omitempty"`
	Kind       string `json:"kind,omitempty"`
	DurationMs int    `json:"durationMs,omitempty"`
	Text       string `json:"text,omitempty"`
	Target     string `json:"target,omitempty"`
}

// TranslateItem 翻译的一条（N 与输入的序号一一对应）。
type TranslateItem struct {
	N    int    `json:"n"`
	Text string `json:"text"`
}

// ClipNote 模型对一段素材的描述。
type ClipNote struct {
	Clip string `json:"clip"`
	Note string `json:"note"`
}

// Refs 是这一次方案能引用的编号（解析时据此校验）。
type Refs struct {
	// Cards 素材卡片（混剪的画面与配乐、分镜的配乐、解说的画面、追加指令的替换与插入）。
	Cards []Card
	// Shots 分镜成片的镜头（b1~bN）。
	Shots []string
	// Sentences 口播 / 解说的句子（s1~sN）。
	Sentences []string
	// Partial 口播长稿分段规划时的一段（SCT-34）：这一段一句都不留是合法的（整段都是重录或废话），
	// 只要模型明确作了决定（给了 keep —— 空数组也算 —— 或者用 drop 删光了这一段）。
	// 整份稿子一句都不留仍按不合规处理（由调用方在拼接之后判）。
	Partial bool
	// Clips / Texts 追加指令的当前片段（c1~cN）与文字层（t1~tN）。
	Clips []string
	Texts []string
	// Items 翻译的条数（序号 1~Items）。
	Items int
}

// ParseResult 是解析的结果。
type ParseResult struct {
	Draft Draft
	// Dropped 不合规被丢掉的（每条一句）：给用户看，也回灌给模型修。
	Dropped []string
	// Ignored 被忽略的字段（模型写的时间之类），不是错误。
	Ignored []string
}

// ErrDraftInvalid 表示模型输出救不回来，或过完白名单之后什么都不剩。
// Snippet 是原文（去掉首尾空白后）的前 300 个字，给运营判断是模型跑题了还是提示词写坏了。
type ErrDraftInvalid struct {
	Reason  string
	Snippet string
}

func (e *ErrDraftInvalid) Error() string {
	return e.Reason + "（原文开头：" + e.Snippet + "）"
}

// ParseDraft 解析模型输出：先严格解析，失败再走容错层；然后逐项过白名单。
func ParseDraft(mode, raw string, refs Refs) (ParseResult, error) {
	snippet := headRunes(strings.TrimSpace(raw), DraftSnippetRunes)
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		if err := lenientjson.Decode(raw, &v); err != nil {
			reason := "模型输出无法解析为 JSON"
			if !strings.ContainsAny(raw, "{[") {
				reason = "模型没有按要求输出 JSON，回的是一段文字"
			}
			return ParseResult{}, &ErrDraftInvalid{Reason: reason, Snippet: snippet}
		}
	}
	root, ok := v.(map[string]any)
	if !ok {
		return ParseResult{}, &ErrDraftInvalid{Reason: "模型输出不是 JSON 对象", Snippet: snippet}
	}
	p := &draftParser{refs: refs, cards: cardIndex(refs.Cards), ignored: map[string]int{}}
	d := Draft{Mode: mode}
	d.Notes = p.texts(root["notes"])
	d.Warnings = p.texts(root["warnings"])
	p.ignoreTimes(root, nil)

	empty := ""
	switch mode {
	case ModeMix, ModeBoard:
		d.Segments = p.segments(root["segments"], mode)
		d.Title = p.title(root["title"])
		d.Music = p.music(root["music"])
		d.ClipNotes = p.clipNotes(root["clipNotes"])
		if mode == ModeMix && len(d.Segments) == 0 {
			empty = "模型没有给出可用的片段"
		}
		if mode == ModeBoard && root["segments"] == nil && d.Title == nil && d.Music == "" {
			empty = "模型没有给出分镜成片的包装方案"
		}
	case ModeTalk:
		d.Keep, d.Drop = p.talk(root)
		if b, ok := root["subtitle"].(bool); ok {
			d.Subtitle = &b
		}
		d.Music = p.music(root["music"])
		// 分段里的一段：模型明确作了决定（给了 keep，哪怕是空数组；或者只给了 drop、删光了这一段）就算合规。
		_, keepGiven := root["keep"].([]any)
		decided := keepGiven || len(d.Drop) > 0
		if len(d.Keep) == 0 && !(refs.Partial && decided && len(p.dropped) == 0) {
			empty = "模型没有给出要保留的句子"
		}
	case ModeNarr:
		d.Lines = p.lines(root["lines"])
		d.Music = p.music(root["music"])
		d.ClipNotes = p.clipNotes(root["clipNotes"])
		if len(d.Lines) == 0 {
			empty = "模型没有给每句配画面"
		}
	case ModeEdit:
		d.Ops = p.ops(root["ops"])
		if s, ok := root["summary"].(string); ok {
			d.Summary = headRunes(strings.TrimSpace(s), MaxReasonRunes)
		}
		if len(d.Ops) == 0 {
			empty = "模型没有给出可执行的修改"
		}
	case ModeTranslate:
		d.Items = p.items(root["items"])
		if len(d.Items) == 0 {
			empty = "模型没有给出译文"
		} else {
			// 漏掉的序号也算不合规（回灌修一次）：契约要求每条都有，漏了的那几条用户就得自己补。
			// 写了这个序号但内容不对的（缺 text 之类）已经单独说过，不重复。
			var missing []string
			for n := 1; n <= p.refs.Items; n++ {
				if !p.itemSeen[n] {
					missing = append(missing, strconv.Itoa(n))
				}
			}
			if len(missing) > 0 {
				p.drop("items 缺少第 %s 条（每条都要有，不要合并、拆分或跳过）", strings.Join(missing, "、"))
			}
		}
	default:
		return ParseResult{}, fmt.Errorf("不支持的剪辑方式 %q", mode)
	}
	if empty != "" {
		if len(p.dropped) > 0 {
			empty += "：" + p.dropped[0]
		}
		return ParseResult{Dropped: p.dropped}, &ErrDraftInvalid{Reason: empty, Snippet: snippet}
	}
	return ParseResult{Draft: d, Dropped: p.dropped, Ignored: p.ignoredNotes()}, nil
}

// RepairHint 把不合规的原因列成一段话，回灌给模型修一次（SCT-27）。没有可修的返回空。
func RepairHint(dropped []string, invalid error) string {
	lines := []string{}
	var di *ErrDraftInvalid
	if invalid != nil {
		if e, ok := invalid.(*ErrDraftInvalid); ok {
			di = e
			lines = append(lines, e.Reason)
		} else {
			lines = append(lines, invalid.Error())
		}
	}
	for _, d := range dropped {
		if di != nil && strings.HasSuffix(di.Reason, "："+d) {
			continue
		}
		lines = append(lines, d)
	}
	if len(lines) == 0 {
		return ""
	}
	if len(lines) > maxRepairLines {
		lines = append(lines[:maxRepairLines], fmt.Sprintf("……还有 %d 处", len(lines)-maxRepairLines))
	}
	return "上一次的输出有以下问题，请改正后按同样的格式重新输出完整的 JSON（只输出 JSON，不要解释）：\n- " +
		strings.Join(lines, "\n- ")
}

// ---------------------------------------------------------------- 解析器

type draftParser struct {
	refs    Refs
	cards   map[string]Card
	dropped []string
	ignored map[string]int
	// itemSeen 翻译：写到了的序号（不管内容合不合规）。
	itemSeen map[int]bool
}

func (p *draftParser) drop(format string, args ...any) {
	p.dropped = append(p.dropped, fmt.Sprintf(format, args...))
}

// timeKeys 模型常写的时间字段：一律忽略（SC-AC-31）。允许的少数几个（标题的 durationSec、取段的 aroundSec、
// 转场的 durationSec）在各自的解析里显式读，不在这里记。
var timeKeys = map[string]bool{
	"startMs": true, "endMs": true, "start": true, "end": true, "startSec": true, "endSec": true,
	"durationMs": true, "duration": true, "durationSec": true, "lengthMs": true, "length": true,
	"inMs": true, "outMs": true, "sourceInMs": true, "sourceOutMs": true, "offsetMs": true, "offset": true,
	"atMs": true, "atSec": true, "timeMs": true, "time": true, "startTime": true, "endTime": true,
	"totalMs": true, "totalSec": true, "targetMs": true, "targetSec": true,
}

// ignoreTimes 记下一个对象里被忽略的时间字段（allowed 里的不算）。
func (p *draftParser) ignoreTimes(m map[string]any, allowed map[string]bool) {
	for k := range m {
		if timeKeys[k] && !allowed[k] {
			p.ignored[k]++
		}
	}
}

func (p *draftParser) ignoredNotes() []string {
	if len(p.ignored) == 0 {
		return nil
	}
	keys := make([]string, 0, len(p.ignored))
	total := 0
	for k, n := range p.ignored {
		keys = append(keys, k)
		total += n
	}
	sort.Strings(keys)
	return []string{fmt.Sprintf("方案里写了 %d 处时间（%s），已忽略：时间由系统按素材编排", total, strings.Join(keys, "、"))}
}

// texts notes / warnings：字符串数组（也宽容写成一句话的），每条截到上限。
func (p *draftParser) texts(v any) []string {
	list, _ := v.([]any)
	if s, ok := v.(string); ok {
		list = []any{s}
	}
	var out []string
	for _, item := range list {
		s, ok := item.(string)
		s = strings.TrimSpace(s)
		if !ok || s == "" {
			continue
		}
		if len(out) == MaxDraftNotes {
			break
		}
		out = append(out, headRunes(s, MaxNoteRunes))
	}
	return out
}

// cardRef 素材编号：字符串原样（去掉首尾空白与「#」「编号」前缀），整数按十进制。
func cardRef(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		s := strings.TrimSpace(x)
		s = strings.TrimPrefix(s, "编号")
		s = strings.TrimPrefix(s, "#")
		s = strings.TrimSpace(s)
		return s, s != ""
	case float64:
		if x >= 0 && x < 1e9 && x == math.Trunc(x) {
			return strconv.FormatInt(int64(x), 10), true
		}
	}
	return "", false
}

// prefixedRef 带字母前缀的编号（s3、c2、t1、b4）：大小写不敏感；只写了数字的按这个前缀补上。
func prefixedRef(v any, prefix string) (string, bool) {
	s, ok := cardRef(v)
	if !ok {
		return "", false
	}
	if len(s) > 0 && s[0] >= '0' && s[0] <= '9' {
		s = prefix + s
	}
	return strings.ToLower(s), true
}

func refIn(ref string, list []string) bool {
	for _, r := range list {
		if r == ref {
			return true
		}
	}
	return false
}

func display(v any) string {
	switch x := v.(type) {
	case nil:
		return "（空）"
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// take 取段：字符串 full / head / middle / tail，或对象 {"aroundSec": 秒}；缺省 full。
// 越界（aroundSec 超出素材）返回错误说明。
func (p *draftParser) take(v any, c Card, ratio any) (Take, string) {
	t := Take{Kind: TakeFull}
	switch x := v.(type) {
	case nil:
	case string:
		k := strings.ToLower(strings.TrimSpace(x))
		switch k {
		case TakeFull, TakeHead, TakeMiddle, TakeTail:
			t.Kind = k
		default:
			return t, fmt.Sprintf("take「%s」不认识（只能是 full / head / middle / tail 或 {\"aroundSec\": 秒}）", x)
		}
	case map[string]any:
		sec, ok := x["aroundSec"].(float64)
		if !ok || sec < 0 {
			return t, fmt.Sprintf("take %s 不认识（围绕某一秒要写成 {\"aroundSec\": 秒}）", display(v))
		}
		ms := int(math.Round(sec * 1000))
		if c.Kind == InputVideo && c.DurationMs > 0 && ms >= c.DurationMs {
			return t, fmt.Sprintf("aroundSec %s 超出了素材的 %s 秒", display(sec), secText(c.DurationMs))
		}
		t.Kind, t.AroundMs = TakeAround, ms
		if r, present := x["ratio"]; present && ratio == nil {
			ratio = r
		}
	default:
		return t, fmt.Sprintf("take %s 不认识", display(v))
	}
	if ratio != nil {
		r, ok := ratio.(float64)
		if !ok || r < 0.1 || r > 1 {
			return t, fmt.Sprintf("ratio %s 不在 0.1~1 之间", display(ratio))
		}
		if t.Kind != TakeFull {
			t.RatioPermille = int(math.Round(r * 1000))
		}
	}
	return t, ""
}

var (
	segRoles   = map[string]bool{SegHook: true, SegBody: true, SegClimax: true, SegEnd: true}
	draftTrans = map[string]bool{TransCut: true, tl.TransitionFade: true, tl.TransitionDissolve: true,
		tl.TransitionSlideLeft: true, tl.TransitionWipe: true}
)

func transitionName(v any) (string, bool) {
	if v == nil {
		return TransCut, true
	}
	s, ok := v.(string)
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "none" || s == "" {
		s = TransCut
	}
	return s, ok && draftTrans[s]
}

// segments 素材混剪 / 分镜成片的片段。分镜成片的 clip 是镜头编号（b1~bN），顺序与取段以装配为准：
// take / role / weight 读了也不用（不当错误）。
func (p *draftParser) segments(v any, mode string) []Segment {
	list, ok := v.([]any)
	if !ok {
		if v != nil {
			p.drop("segments 必须是数组")
		}
		return nil
	}
	var out []Segment
	seen := map[string]int{}
	for i, item := range list {
		n := i + 1
		if len(out) == MaxDraftSegments {
			p.drop("片段超过 %d 段，后面的没有用", MaxDraftSegments)
			break
		}
		m, ok := item.(map[string]any)
		if !ok {
			p.drop("第 %d 段不是对象", n)
			continue
		}
		p.ignoreTimes(m, nil)
		var s Segment
		var c Card
		if mode == ModeBoard {
			ref, ok := prefixedRef(m["clip"], "b")
			if !ok || !refIn(ref, p.refs.Shots) {
				p.drop("第 %d 段：镜头「%s」不在镜头清单里", n, display(m["clip"]))
				continue
			}
			s.Clip, s.Take, s.Role, s.Weight = ref, Take{Kind: TakeFull}, SegBody, 1
		} else {
			ref, ok := cardRef(m["clip"])
			card, found := p.cards[ref]
			switch {
			case !ok || !found:
				p.drop("第 %d 段：编号「%s」不在素材条里", n, display(m["clip"]))
				continue
			case !card.IsPicture():
				p.drop("第 %d 段：编号「%s」是%s，不能放进画面", n, ref, kindLabel(card.Kind))
				continue
			}
			c, s.Clip = card, ref
			t, problem := p.take(m["take"], c, m["ratio"])
			if problem != "" {
				p.drop("第 %d 段：%s", n, problem)
				continue
			}
			s.Take = t
			s.Role = SegBody
			if r, present := m["role"]; present && r != nil {
				rs, _ := r.(string)
				rs = strings.ToLower(strings.TrimSpace(rs))
				if !segRoles[rs] {
					p.drop("第 %d 段：role「%s」不认识（只能是 hook / body / climax / end）", n, display(r))
					continue
				}
				s.Role = rs
			}
			s.Weight = 1
			if w, present := m["weight"]; present && w != nil {
				wf, ok := w.(float64)
				if !ok || wf < 1 || wf > 3 {
					p.drop("第 %d 段：weight %s 不在 1~3 之间", n, display(w))
					continue
				}
				s.Weight = int(math.Round(wf))
			}
		}
		tr, ok := transitionName(m["transition"])
		if !ok {
			p.drop("第 %d 段：transition「%s」不认识（只能是 cut / fade / dissolve / slide_left / wipe）", n, display(m["transition"]))
			continue
		}
		s.Transition = tr
		if sub, present := m["subtitle"]; present && sub != nil {
			text, isStr := sub.(string)
			text = strings.TrimSpace(text)
			switch {
			case (isStr && text == "") || sub == false:
				s.SubtitleOff = true
			case runeLen(text) > MaxSubtitleRunes:
				p.drop("第 %d 段的字幕超过 %d 个字，已去掉", n, MaxSubtitleRunes)
			default:
				s.Subtitle = text
			}
		}
		if r, ok := m["reason"].(string); ok {
			s.Reason = headRunes(strings.TrimSpace(r), MaxReasonRunes)
		}
		// 重复只在整段都合规之后判：先登记再丢的话，后面一段合规的会被当成重复误删。
		key := s.Clip
		if mode != ModeBoard {
			key += "|" + s.Take.Kind + "|" + strconv.Itoa(s.Take.AroundMs) + "|" + strconv.Itoa(s.Take.RatioPermille)
		}
		if prev, dup := seen[key]; dup {
			if mode == ModeBoard {
				p.drop("第 %d 段与第 %d 段是同一个镜头「%s」，只用第一处", n, prev, s.Clip)
			} else {
				p.drop("第 %d 段与第 %d 段重复（同一个编号、同样的取段），已去掉", n, prev)
			}
			continue
		}
		seen[key] = n
		out = append(out, s)
	}
	return out
}

func kindLabel(kind string) string {
	switch kind {
	case InputAudio:
		return "音频"
	case InputText:
		return "文本"
	case InputVideo:
		return "视频"
	case InputImage:
		return "图片"
	case InputShotGroup:
		return "镜头组"
	}
	return kind
}

// title 开头标题：{"text": "…", "durationSec": 2}。
func (p *draftParser) title(v any) *Title {
	if v == nil {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		if s, isStr := v.(string); isStr && strings.TrimSpace(s) != "" {
			m = map[string]any{"text": s}
		} else {
			p.drop("title 必须是对象 {\"text\": …, \"durationSec\": …}")
			return nil
		}
	}
	p.ignoreTimes(m, map[string]bool{"durationSec": true})
	text, _ := m["text"].(string)
	text = strings.TrimSpace(text)
	switch {
	case text == "":
		p.drop("title：缺少 text")
		return nil
	case runeLen(text) > MaxTitleRunes:
		p.drop("title：标题超过 %d 个字，已去掉", MaxTitleRunes)
		return nil
	}
	t := &Title{Text: text}
	if sec, ok := m["durationSec"].(float64); ok && sec > 0 {
		t.DurationMs = min(max(int(math.Round(sec*1000)), TitleMinMs), TitleMaxMs)
	}
	return t
}

// music 配乐：{"clip": "8"}（也宽容直接写编号），必须是音频素材。
func (p *draftParser) music(v any) string {
	if v == nil {
		return ""
	}
	raw := v
	if m, ok := v.(map[string]any); ok {
		p.ignoreTimes(m, nil)
		raw = m["clip"]
	}
	ref, ok := cardRef(raw)
	card, found := p.cards[ref]
	switch {
	case !ok || !found:
		p.drop("music：编号「%s」不在素材条里", display(raw))
		return ""
	case card.Kind != InputAudio:
		p.drop("music：编号「%s」是%s，不是音频", ref, kindLabel(card.Kind))
		return ""
	}
	return ref
}

// clipNotes 看图素材的描述（写进缓存）：键必须是素材编号，按编号排序；不认识的键直接忽略（不需要修）。
func (p *draftParser) clipNotes(v any) []ClipNote {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	var out []ClipNote
	for k, val := range m {
		ref, _ := cardRef(k)
		s, isStr := val.(string)
		s = strings.TrimSpace(s)
		if _, found := p.cards[ref]; !found || !isStr || s == "" {
			continue
		}
		out = append(out, ClipNote{Clip: ref, Note: headRunes(s, MaxClipNoteRunes)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Clip < out[j].Clip })
	return out
}

// talk 口播精剪：keep [{from,to}]（也宽容 "s3" 或 {"sentence":"s3"}），drop 只作说明。
// 只给了 drop 没给 keep 时，按「保留其余句子」理解（并说明）；同一句既在 keep 又在 drop 时以 drop 为准。
func (p *draftParser) talk(root map[string]any) ([]SentenceRange, []SentenceDrop) {
	index := map[string]int{}
	for i, s := range p.refs.Sentences {
		index[s] = i
	}
	var drops []SentenceDrop
	dropped := map[string]bool{}
	if list, ok := root["drop"].([]any); ok {
		for i, item := range list {
			var ref string
			var reason string
			switch x := item.(type) {
			case map[string]any:
				ref, _ = prefixedRef(x["sentence"], "s")
				reason, _ = x["reason"].(string)
			default:
				ref, _ = prefixedRef(x, "s")
			}
			if _, ok := index[ref]; !ok {
				p.drop("drop 第 %d 项：句子「%s」不存在", i+1, ref)
				continue
			}
			if !dropped[ref] {
				dropped[ref] = true
				drops = append(drops, SentenceDrop{Sentence: ref, Reason: headRunes(strings.TrimSpace(reason), MaxReasonRunes)})
			}
		}
	}
	keepSet := make([]bool, len(p.refs.Sentences))
	kept := false
	if list, ok := root["keep"].([]any); ok {
		for i, item := range list {
			n := i + 1
			var from, to string
			switch x := item.(type) {
			case map[string]any:
				p.ignoreTimes(x, nil)
				if s, present := x["sentence"]; present {
					from, _ = prefixedRef(s, "s")
					to = from
				} else {
					from, _ = prefixedRef(x["from"], "s")
					to, _ = prefixedRef(x["to"], "s")
					if x["to"] == nil {
						to = from
					}
				}
			default:
				from, _ = prefixedRef(x, "s")
				to = from
			}
			fi, okF := index[from]
			ti, okT := index[to]
			switch {
			case !okF:
				p.drop("keep 第 %d 项：句子「%s」不存在", n, from)
				continue
			case !okT:
				p.drop("keep 第 %d 项：句子「%s」不存在", n, to)
				continue
			case fi > ti:
				p.drop("keep 第 %d 项：from「%s」在 to「%s」之后", n, from, to)
				continue
			}
			for k := fi; k <= ti; k++ {
				keepSet[k] = true
				kept = true
			}
		}
	} else if len(drops) > 0 {
		for k := range keepSet {
			keepSet[k] = true
			kept = true
		}
	}
	if !kept {
		return nil, drops
	}
	// 合并成连续区间（以 drop 为准）。
	var out []SentenceRange
	start := -1
	for k := 0; k <= len(keepSet); k++ {
		on := k < len(keepSet) && keepSet[k] && !dropped[p.refs.Sentences[k]]
		if on && start < 0 {
			start = k
		}
		if !on && start >= 0 {
			out = append(out, SentenceRange{From: p.refs.Sentences[start], To: p.refs.Sentences[k-1]})
			start = -1
		}
	}
	return out, drops
}

// lines 解说成片：每句配哪些画面。clips 必须是画面素材；同一句只用第一处。
func (p *draftParser) lines(v any) []NarrLine {
	list, ok := v.([]any)
	if !ok {
		if v != nil {
			p.drop("lines 必须是数组")
		}
		return nil
	}
	index := map[string]int{}
	for i, s := range p.refs.Sentences {
		index[s] = i
	}
	var out []NarrLine
	seen := map[string]int{}
	for i, item := range list {
		n := i + 1
		m, ok := item.(map[string]any)
		if !ok {
			p.drop("lines 第 %d 项不是对象", n)
			continue
		}
		p.ignoreTimes(m, nil)
		ref, _ := prefixedRef(m["sentence"], "s")
		if _, ok := index[ref]; !ok {
			p.drop("lines 第 %d 项：句子「%s」不存在", n, display(m["sentence"]))
			continue
		}
		if prev, dup := seen[ref]; dup {
			p.drop("lines 第 %d 项与第 %d 项是同一句「%s」，只用第一处", n, prev, ref)
			continue
		}
		clipsRaw, _ := m["clips"].([]any)
		if c, present := m["clip"]; present && clipsRaw == nil {
			clipsRaw = []any{c}
		}
		var clips []string
		bad := ""
		var first Card
		for _, cv := range clipsRaw {
			c, ok := cardRef(cv)
			card, found := p.cards[c]
			switch {
			case !ok || !found:
				bad = fmt.Sprintf("lines 第 %d 项：编号「%s」不在素材条里", n, display(cv))
			case !card.IsPicture():
				bad = fmt.Sprintf("lines 第 %d 项：编号「%s」是%s，不能放进画面", n, c, kindLabel(card.Kind))
			default:
				if len(clips) == 0 {
					first = card
				}
				clips = append(clips, c)
			}
		}
		if bad != "" {
			p.dropped = append(p.dropped, bad)
		}
		if len(clips) == 0 {
			if bad == "" {
				p.drop("lines 第 %d 项：没有配画面", n)
			}
			continue
		}
		t, problem := p.take(m["take"], first, m["ratio"])
		if problem != "" {
			p.drop("lines 第 %d 项：%s", n, problem)
			continue
		}
		seen[ref] = n
		out = append(out, NarrLine{Sentence: ref, Clips: clips, Take: t})
	}
	sort.SliceStable(out, func(i, j int) bool { return index[out[i].Sentence] < index[out[j].Sentence] })
	return out
}

var editOps = map[string]bool{OpTrim: true, OpSwap: true, OpReplace: true, OpSpeed: true, OpTransition: true,
	OpText: true, OpInsert: true, OpDelete: true}

// ops 追加指令的原子操作（只校验形状与引用；用户改过的片段在应用时跳过）。
func (p *draftParser) ops(v any) []EditOp {
	list, ok := v.([]any)
	if !ok {
		if v != nil {
			p.drop("ops 必须是数组")
		}
		return nil
	}
	var out []EditOp
	for i, item := range list {
		n := i + 1
		if len(out) == MaxDraftOps {
			p.drop("修改超过 %d 处，后面的没有用", MaxDraftOps)
			break
		}
		m, ok := item.(map[string]any)
		if !ok {
			p.drop("ops 第 %d 项不是对象", n)
			continue
		}
		op, _ := m["op"].(string)
		op = strings.ToLower(strings.TrimSpace(op))
		if !editOps[op] {
			p.drop("ops 第 %d 项：op「%s」不认识（只能是 trim / swap / replace / speed / transition / text / insert / delete）", n, display(m["op"]))
			continue
		}
		allowed := map[string]bool{}
		if op == OpTransition {
			allowed["durationSec"] = true
		}
		p.ignoreTimes(m, allowed)
		e := EditOp{Op: op}
		clipRef := func(key string) (string, bool) {
			ref, ok := prefixedRef(m[key], "c")
			if !ok || !refIn(ref, p.refs.Clips) {
				p.drop("ops 第 %d 项（%s）：片段「%s」不存在", n, op, display(m[key]))
				return "", false
			}
			return ref, true
		}
		pictureRef := func(key string) (string, Card, bool) {
			ref, ok := cardRef(m[key])
			card, found := p.cards[ref]
			switch {
			case !ok || !found:
				p.drop("ops 第 %d 项（%s）：编号「%s」不在素材条里", n, op, display(m[key]))
			case !card.IsPicture():
				p.drop("ops 第 %d 项（%s）：编号「%s」是%s，不能放进画面", n, op, ref, kindLabel(card.Kind))
			default:
				return ref, card, true
			}
			return "", Card{}, false
		}
		valid := true
		switch op {
		case OpTrim:
			e.Clip, valid = clipRef("clip")
			if valid {
				t, problem := p.take(m["take"], Card{}, m["ratio"])
				switch {
				case problem != "":
					p.drop("ops 第 %d 项（trim）：%s", n, problem)
					valid = false
				case t.Kind != TakeFull && m["ratio"] == nil && !hasRatio(m["take"]):
					p.drop("ops 第 %d 项（trim）：缺少 ratio（保留的比例，0.1~1）", n)
					valid = false
				default:
					e.Take = &t
				}
			}
		case OpSwap:
			var okA, okB bool
			e.A, okA = clipRef("a")
			e.B, okB = clipRef("b")
			valid = okA && okB
			if valid && e.A == e.B {
				p.drop("ops 第 %d 项（swap）：a 与 b 是同一段", n)
				valid = false
			}
		case OpReplace:
			var okC, okW bool
			e.Clip, okC = clipRef("clip")
			var card Card
			e.With, card, okW = pictureRef("with")
			valid = okC && okW
			if valid && m["take"] != nil {
				t, problem := p.take(m["take"], card, m["ratio"])
				if problem != "" {
					p.drop("ops 第 %d 项（replace）：%s", n, problem)
					valid = false
				} else {
					e.Take = &t
				}
			}
		case OpSpeed:
			if s, _ := m["clip"].(string); strings.TrimSpace(s) == "*" {
				e.Clip = "*"
			} else {
				e.Clip, valid = clipRef("clip")
			}
			if valid {
				val, ok := m["value"].(float64)
				if !ok || val < 0.25 || val > 4 {
					p.drop("ops 第 %d 项（speed）：value %s 不在 0.25~4 之间", n, display(m["value"]))
					valid = false
				} else {
					e.SpeedPct = int(math.Round(val * 100))
				}
			}
		case OpTransition:
			e.Clip, valid = clipRef("clip")
			if valid {
				tr, ok := transitionName(m["kind"])
				if !ok {
					p.drop("ops 第 %d 项（transition）：kind「%s」不认识（只能是 cut / fade / dissolve / slide_left / wipe）", n, display(m["kind"]))
					valid = false
				}
				e.Kind = tr
				if sec, present := m["durationSec"]; valid && present && sec != nil {
					f, ok := sec.(float64)
					if !ok || f < 0.1 || f > 3 {
						p.drop("ops 第 %d 项（transition）：durationSec %s 不在 0.1~3 之间", n, display(sec))
						valid = false
					} else {
						e.DurationMs = int(math.Round(f * 1000))
					}
				}
			}
		case OpText:
			text, _ := m["text"].(string)
			e.Text = strings.TrimSpace(text)
			switch {
			case e.Text == "":
				p.drop("ops 第 %d 项（text）：缺少 text", n)
				valid = false
			case runeLen(e.Text) > MaxTitleRunes*2:
				p.drop("ops 第 %d 项（text）：文字超过 %d 个字", n, MaxTitleRunes*2)
				valid = false
			}
			if valid && m["target"] != nil {
				ref, ok := prefixedRef(m["target"], "t")
				if !ok || !refIn(ref, p.refs.Texts) {
					p.drop("ops 第 %d 项（text）：文字「%s」不存在", n, display(m["target"]))
					valid = false
				}
				e.Target = ref
			} else if valid && m["clip"] != nil {
				e.Clip, valid = clipRef("clip")
			}
		case OpInsert:
			var card Card
			key := "with"
			if m["with"] == nil {
				key = "clip"
			}
			e.With, card, valid = pictureRef(key)
			if valid {
				switch after := m["after"].(type) {
				case nil:
					e.After = "end"
				case string:
					a := strings.ToLower(strings.TrimSpace(after))
					if a == "start" || a == "end" {
						e.After = a
					} else {
						e.After, valid = clipRef("after")
					}
				default:
					e.After, valid = clipRef("after")
				}
			}
			if valid && m["take"] != nil {
				t, problem := p.take(m["take"], card, m["ratio"])
				if problem != "" {
					p.drop("ops 第 %d 项（insert）：%s", n, problem)
					valid = false
				} else {
					e.Take = &t
				}
			}
		case OpDelete:
			e.Clip, valid = clipRef("clip")
		}
		if valid {
			out = append(out, e)
		}
	}
	return out
}

func hasRatio(take any) bool {
	m, ok := take.(map[string]any)
	return ok && m["ratio"] != nil
}

// items 翻译：n 在 1~Items 之间、text 非空不超长；同一个 n 只用第一处。
func (p *draftParser) items(v any) []TranslateItem {
	list, ok := v.([]any)
	if !ok {
		if v != nil {
			p.drop("items 必须是数组")
		}
		return nil
	}
	var out []TranslateItem
	seen := map[int]bool{}
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			p.drop("items 第 %d 项不是对象", i+1)
			continue
		}
		nf, ok := m["n"].(float64)
		n := int(nf)
		if !ok || nf != math.Trunc(nf) || n < 1 || n > p.refs.Items {
			p.drop("items 第 %d 项：n=%s 不在 1~%d 之间", i+1, display(m["n"]), p.refs.Items)
			continue
		}
		if p.itemSeen == nil {
			p.itemSeen = map[int]bool{}
		}
		p.itemSeen[n] = true
		text, _ := m["text"].(string)
		text = strings.TrimSpace(text)
		switch {
		case text == "":
			p.drop("items 第 %d 项（n=%d）：缺少 text", i+1, n)
			continue
		case runeLen(text) > tl.MaxTextChars:
			p.drop("items 第 %d 项（n=%d）：译文超过 %d 个字", i+1, n, tl.MaxTextChars)
			continue
		case seen[n]:
			continue
		}
		seen[n] = true
		out = append(out, TranslateItem{N: n, Text: text})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].N < out[j].N })
	return out
}
