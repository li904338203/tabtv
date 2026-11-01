package domain

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// 识别结果、句子编号与原稿对齐（口播精剪 SC-F-98~100、解说成片 SC-F-102~104）。

// Transcript 是一段素材的识别结果（media_analysis kind=asr 的 result，dev/02 §2）。时间一律是素材里的毫秒。
type Transcript struct {
	Language string `json:"language"`
	// Granularity char / word / segment / text：segment 与 text 没有字级时间，只能按整句剪。
	Granularity string      `json:"granularity"`
	Utterances  []Utterance `json:"utterances"`
}

// Utterance 识别出的一句。
type Utterance struct {
	Text    string `json:"text"`
	StartMs int    `json:"startMs"`
	EndMs   int    `json:"endMs"`
	Words   []Word `json:"words,omitempty"`
}

// Word 一个字（中文）或一个词（英文）。
type Word struct {
	Text    string `json:"text"`
	StartMs int    `json:"startMs"`
	EndMs   int    `json:"endMs"`
}

// Peaks 波形：每 IntervalMs 一个 0~255 的峰值（media_analysis kind=peaks 解出 base64 之后）。
type Peaks struct {
	IntervalMs int
	Values     []byte
}

// at t 毫秒处的峰值；越界按 255（不在那里剪）。
func (p Peaks) at(t int) int {
	if p.IntervalMs <= 0 || t < 0 {
		return 255
	}
	i := t / p.IntervalMs
	if i >= len(p.Values) {
		return 255
	}
	return int(p.Values[i])
}

// Sentence 是交给模型的一句（s1~sN）。
type Sentence struct {
	Ref    string `json:"ref"`
	CardNo string `json:"cardNo"`
	Text   string `json:"text"`
	// StartMs / EndMs 这一句在素材里的起止（口播：视频；解说：配音）。
	StartMs int `json:"startMs"`
	EndMs   int `json:"endMs"`

	words   []Word
	phrases []phrase
	// span 口播：这一句属于第几段（TalkSpans 的下标）。同一份素材分成几段用时，各段分别剪、分别合并。
	span int
}

// phrase 解说的一条字幕：原稿文字与对齐出来的起止。
type phrase struct {
	text       string
	start, end int
}

// sentenceRefs 句号列表（解析方案时校验用）。
func sentenceRefs(list []Sentence) []string {
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = s.Ref
	}
	return out
}

// SentenceRefs 句号列表。
func SentenceRefs(list []Sentence) []string { return sentenceRefs(list) }

// cleanWords 一句里的字：按起点排序、夹到 [0, limit]、去掉起止颠倒的；没有字级时间时整句当一个字。
func cleanWords(u Utterance, limit int) []Word {
	src := u.Words
	if len(src) == 0 {
		src = []Word{{Text: u.Text, StartMs: u.StartMs, EndMs: u.EndMs}}
	}
	out := make([]Word, 0, len(src))
	for _, w := range src {
		if limit > 0 {
			w.StartMs, w.EndMs = min(max(w.StartMs, 0), limit), min(max(w.EndMs, 0), limit)
		}
		if w.EndMs <= w.StartMs || strings.TrimSpace(w.Text) == "" {
			continue
		}
		out = append(out, w)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].StartMs < out[j-1].StartMs; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// TalkSentences 口播精剪的句子（s1~sN）：按段（TalkSpans，成片顺序）取有识别结果的视频，每个识别句一句。
// 编译与给模型的稿子用同一份编号。
//
//   - 句子按段裁剪：只留整个落在段里的字（不切在字中间）；一句跨了段界就按段切开，裁剪后没有字的句子丢掉。
//   - 段外的字不进稿子（用户剪掉开头的那一秒，模型就看不到那一秒说了什么）。
func TalkSentences(in PlanInput) []Sentence {
	var out []Sentence
	for k, sp := range talkSpans(in) {
		c := sp.card
		for _, u := range in.Transcripts[c.No].Utterances {
			all := cleanWords(u, c.DurationMs)
			words := make([]Word, 0, len(all))
			for _, w := range all {
				if w.StartMs >= sp.InMs && w.EndMs <= sp.OutMs {
					words = append(words, w)
				}
			}
			if len(words) == 0 {
				continue
			}
			text := strings.TrimSpace(u.Text)
			if text == "" || len(words) != len(all) {
				text = joinWords(words)
			}
			out = append(out, Sentence{Ref: "s" + strconv.Itoa(len(out)+1), CardNo: c.No, Text: text,
				StartMs: words[0].StartMs, EndMs: words[len(words)-1].EndMs, words: words, span: k})
		}
	}
	return out
}

// talkSpan 一段口播素材（连着它的卡片）。
type talkSpan struct {
	TalkSpan
	card Card
}

// talkSpans 口播要用的段：Spec.TalkSpans 给了就照它（找不到卡片或识别结果的段跳过），否则每份有识别结果的视频整段。
func talkSpans(in PlanInput) []talkSpan {
	byMedia := map[int64]Card{}
	var whole []talkSpan
	for _, c := range in.Cards {
		if _, ok := in.Transcripts[c.No]; !ok || c.Kind != InputVideo {
			continue
		}
		if _, dup := byMedia[c.MediaID]; !dup {
			byMedia[c.MediaID] = c
		}
		end := c.DurationMs
		if end <= 0 {
			end = math.MaxInt32
		}
		whole = append(whole, talkSpan{TalkSpan{MediaID: c.MediaID, InMs: 0, OutMs: end}, c})
	}
	if len(in.Spec.TalkSpans) == 0 {
		return whole
	}
	var out []talkSpan
	for _, sp := range in.Spec.TalkSpans {
		c, ok := byMedia[sp.MediaID]
		if !ok || sp.OutMs <= sp.InMs {
			continue
		}
		out = append(out, talkSpan{sp, c})
	}
	return out
}

// joinWords 字连成文字：中文直接连，两边都是拉丁字母或数字时加空格。
func joinWords(words []Word) string {
	var b strings.Builder
	prevLatin := false
	for i, w := range words {
		t := strings.TrimSpace(w.Text)
		latin := isLatinStart(t)
		if i > 0 && latin && prevLatin {
			b.WriteByte(' ')
		}
		b.WriteString(t)
		prevLatin = isLatinEnd(t)
	}
	return b.String()
}

func isLatinStart(s string) bool {
	for _, r := range s {
		return r < 0x3000 && (unicode.IsLetter(r) || unicode.IsDigit(r))
	}
	return false
}

func isLatinEnd(s string) bool {
	rs := []rune(s)
	if len(rs) == 0 {
		return false
	}
	r := rs[len(rs)-1]
	return r < 0x3000 && (unicode.IsLetter(r) || unicode.IsDigit(r))
}

// ---------------------------------------------------------------- 对齐（解说成片）

// unit 是对齐的最小单位：一个汉字 / 假名 / 谚文，或一串拉丁字母与数字（小写）。标点与空白不参与。
type unit struct {
	text       string
	start, end int
	// word 来自识别结果里的第几个字（narrUnits 的下标）：对不上的句子退回识别文字时按它取回原字。
	word int
}

// tokenize 文字 → 对齐单位。
func tokenize(s string) []string {
	var out []string
	var latin strings.Builder
	flush := func() {
		if latin.Len() > 0 {
			out = append(out, strings.ToLower(latin.String()))
			latin.Reset()
		}
	}
	for _, r := range s {
		switch {
		case isWide(r):
			flush()
			out = append(out, string(r))
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			latin.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return out
}

// isWide 按单字对齐的文字（中日韩）。
func isWide(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r)
}

// narrWord 识别出的一个字（词），时间换算成成片时间；utter 是第几条配音的第几句（退回识别文字时按它断句）。
type narrWord struct {
	Word
	utter [2]int
}

// narrUnits 各条配音的识别结果 → 成片时间上的字与对齐单位：一个词拆成几个单位时按单位序号在词的起止里线性插值。
func narrUnits(in PlanInput, voices []narrVoice) ([]narrWord, []unit) {
	var words []narrWord
	var units []unit
	for vi, v := range voices {
		for ui, u := range in.Transcripts[v.card.No].Utterances {
			for _, w := range cleanWords(u, v.card.DurationMs) {
				w.StartMs += v.offset
				w.EndMs += v.offset
				wi := len(words)
				words = append(words, narrWord{Word: w, utter: [2]int{vi, ui}})
				toks := tokenize(w.Text)
				span := w.EndMs - w.StartMs
				for i, t := range toks {
					units = append(units, unit{text: t, start: w.StartMs + span*i/len(toks),
						end: w.StartMs + span*(i+1)/len(toks), word: wi})
				}
			}
		}
	}
	return words, units
}

// alignUnits 编辑距离对齐（带状 DP，沿按长度缩放的对角线、半宽 64 + 两边长度差）：
// 返回原稿每个单位对上的识别单位下标（对不上的为 -1）。相同算 0、替换 / 多 / 少各算 2；
// 替换也算对上（数字念成汉字、同音字这类差异，时间仍然可用）。
//
// cheap[i]（长度 n+1，可为 nil）为真时，在原稿第 i 个单位之前多出来的识别单位只算 1：句子与字幕条的边界上
// 多出来的话（语气词、念了原稿里没有的一句）比句子中间多出来的常见得多。不这样分的话，「目」念成「木」之后又多念了
// 一句时，「目↔木 + 多念一句」与「多念一句 + 目↔那一句的最后一个字」代价相同，回溯常常挑中后一种 —— 上一句的
// 结尾被拉到多念那句的末尾，那一句的识别文字就没了着落（SCT-35）。
func alignUnits(script []string, asr []unit, cheap []bool) []int {
	n, m := len(script), len(asr)
	match := make([]int, n)
	for i := range match {
		match[i] = -1
	}
	if n == 0 || m == 0 {
		return match
	}
	w := 64 + absInt(n-m)
	lo := func(i int) int { return max(0, i*m/n-w) }
	hi := func(i int) int { return min(m, i*m/n+w) }
	const inf = 1 << 30
	ins := func(i int) int32 {
		if i < len(cheap) && cheap[i] {
			return 1
		}
		return 2
	}
	type row struct {
		lo   int
		cost []int32
		from []byte // 0 对角 1 上（原稿多） 2 左（识别多）
	}
	rows := make([]row, n+1)
	for i := 0; i <= n; i++ {
		l, h := lo(i), hi(i)
		r := row{lo: l, cost: make([]int32, h-l+1), from: make([]byte, h-l+1)}
		for j := l; j <= h; j++ {
			best, from := int32(inf), byte(0)
			if i == 0 {
				best, from = int32(j)*ins(0), 2
			} else {
				prev := rows[i-1]
				get := func(jj int) int32 {
					if jj < prev.lo || jj-prev.lo >= len(prev.cost) {
						return inf
					}
					return prev.cost[jj-prev.lo]
				}
				if j > 0 {
					c := int32(2)
					if script[i-1] == asr[j-1].text {
						c = 0
					}
					if v := get(j-1) + c; v < best {
						best, from = v, 0
					}
				}
				if v := get(j) + 2; v < best {
					best, from = v, 1
				}
				if j > l {
					if v := r.cost[j-1-l] + ins(i); v < best {
						best, from = v, 2
					}
				}
			}
			r.cost[j-l], r.from[j-l] = best, from
		}
		rows[i] = r
	}
	i, j := n, m
	if j > hi(n) {
		j = hi(n)
	}
	for i > 0 && j >= 0 {
		r := rows[i]
		if j < r.lo || j-r.lo >= len(r.from) {
			break
		}
		switch r.from[j-r.lo] {
		case 0:
			match[i-1] = j - 1
			i, j = i-1, j-1
		case 1:
			i--
		default:
			j--
		}
	}
	return match
}

// NarrSentences 解说成片的句子（编号 s1~sN）。规则见 narrSentences。
func NarrSentences(in PlanInput) []Sentence {
	sents, _ := narrSentences(in)
	return sents
}

// narrDraft 原稿里的一句（对齐前）。
type narrDraft struct {
	text    string
	phrases []string
	units   [][]string // 每条字幕的对齐单位
}

func (d narrDraft) size() int {
	n := 0
	for _, u := range d.units {
		n += len(u)
	}
	return n
}

// narrGroup 连续几句对不上的原稿，以及两侧对得上的句子之间没被认领的识别单位 [lo, hi]（lo > hi 为空）。
type narrGroup struct {
	first, last int
	lo, hi      int
}

// narrSentences 解说成片的句子与对齐警告（SC-F-102、SCT-35）。
//
//   - 原稿：连进来的文本素材优先，其次是每条配音那次文字转语音的原稿，再没有就用识别出来的文字（narrScript）。
//     没有原稿时把识别出来的每句当作原稿，走同一条对齐路径（时间逐字一致）。
//   - 配音可以有好几条：按编号顺序首尾相接，时间一律换算成成片时间。
//   - 对齐两遍。第一遍整份原稿对齐，**按逐字相同的比例**判每句对不对得上（不到一半算对不上）：替换也算对齐上，
//     两句毫不相干、长度差不多的话会被逐字「替换」对上 —— 字幕照原稿出而声音说的是另一回事，没有任何报错。
//     第二遍只拿对得上的句子再对齐：第一遍里对不上的句子可能把一两个字对到了邻句上（编辑距离在这种地方有等价的
//     几种对法），去掉它们邻句才拿得回自己的字。
//   - 对不上的句子（连续的算一组）：两侧之间没被认领的识别文字就是配音实际说的，字幕改用它；一个字都没有的是
//     漏念了，这几句不出字幕。两种都写进警告、点名是原稿第几句 —— 对齐失败不能静默（字幕与声音错位而没人知道）。
//   - 对得上的句子里对上的字不到一半的那条字幕按前后插值；夹在两条之间、没对上的识别单位（数字念成汉字、多念的字）
//     归给时间上近的一条，开头与结尾的归第一条与最后一条。配音没有任何识别单位时按字数在整段配音里均分。
func narrSentences(in PlanInput) ([]Sentence, []string) {
	voices, total := narrVoices(in.Cards)
	if len(voices) == 0 {
		return nil, nil
	}
	script := narrScript(in, voices)
	if script == "" {
		return nil, nil
	}
	words, asr := narrUnits(in, voices)

	var draft []narrDraft
	for _, sText := range splitKeep(script, "。！？!?；;…\n") {
		d := narrDraft{text: sText}
		for _, pText := range subtitlePhrases(sText, subtitleChunkRunes) {
			d.phrases = append(d.phrases, pText)
			d.units = append(d.units, tokenize(pText))
		}
		if d.size() > 0 {
			draft = append(draft, d)
		}
	}
	if len(draft) == 0 {
		return nil, nil
	}

	// 第一遍：每句对不对得上。
	aligned := make([]bool, len(draft))
	if len(asr) == 0 {
		for i := range aligned {
			aligned[i] = true
		}
	} else {
		var all []string
		var owner []int
		cheap := []bool{true}
		for si, d := range draft {
			for _, u := range d.units {
				all = append(all, u...)
				for k := range u {
					owner = append(owner, si)
					cheap = append(cheap, k == len(u)-1)
				}
			}
		}
		same := make([]int, len(draft))
		for i, j := range alignUnits(all, asr, cheap) {
			if j >= 0 && all[i] == asr[j].text {
				same[owner[i]]++
			}
		}
		for si, d := range draft {
			aligned[si] = same[si]*2 >= d.size()
		}
	}

	// 第二遍：只拿对得上的句子对齐，得到每条字幕对上的识别单位范围。
	type key struct{ sentence, phrase int }
	type span struct{ lo, hi int }
	var units []string
	var refs []key
	cheap := []bool{true}
	for si, d := range draft {
		if !aligned[si] {
			continue
		}
		for pi, u := range d.units {
			units = append(units, u...)
			for k := range u {
				refs = append(refs, key{si, pi})
				cheap = append(cheap, k == len(u)-1)
			}
		}
	}
	match := alignUnits(units, asr, cheap)
	// 一条字幕对上的字不到一半时当作没对上，按前后插值（编辑距离在这种地方有等价的两种对法，另一种会把它的
	// 一两个字对到邻句的字上，把邻句拆开）。
	counts := map[key][2]int{}
	for i, j := range match {
		c := counts[refs[i]]
		c[1]++
		if j >= 0 {
			c[0]++
		}
		counts[refs[i]] = c
	}
	spans := map[key]*span{}
	for i, j := range match {
		if j < 0 {
			continue
		}
		if c := counts[refs[i]]; c[0]*2 < c[1] {
			continue
		}
		sp := spans[refs[i]]
		if sp == nil {
			sp = &span{lo: j, hi: j}
			spans[refs[i]] = sp
		}
		sp.lo, sp.hi = min(sp.lo, j), max(sp.hi, j)
	}

	// 夹在两条之间的识别单位：中间有对不上的句子时归那一组，否则归时间上近的一条。
	var groups []*narrGroup
	groupAt := map[int]*narrGroup{}
	var prev *span
	var open *narrGroup
	closeGroup := func(hi int) {
		open.lo = 0
		if prev != nil {
			open.lo = prev.hi + 1
		}
		open.hi = hi
		open = nil
	}
	for si, d := range draft {
		if !aligned[si] {
			if open == nil {
				open = &narrGroup{first: si}
				groups = append(groups, open)
				groupAt[si] = open
			}
			open.last = si
			continue
		}
		for pi := range d.units {
			sp := spans[key{si, pi}]
			if sp == nil {
				continue
			}
			switch {
			case open != nil:
				closeGroup(sp.lo - 1)
			case prev == nil:
				sp.lo = 0
			default:
				for k := prev.hi + 1; k < sp.lo; k++ {
					if asr[k].start-asr[prev.hi].end < asr[sp.lo].start-asr[k].end {
						prev.hi = k
						continue
					}
					sp.lo = k
					break
				}
			}
			prev = sp
		}
	}
	if open != nil {
		closeGroup(len(asr) - 1)
	} else if prev != nil {
		prev.hi = len(asr) - 1
	}

	var out []Sentence
	var warns []string
	matchedAny := false
	for si := 0; si < len(draft); si++ {
		if g := groupAt[si]; g != nil {
			got := recognizedSentences(words, asr, g.lo, g.hi)
			warns = append(warns, groupWarning(draft, g, got))
			out = append(out, got...)
			si = g.last
			continue
		}
		matchedAny = true
		s := Sentence{Text: draft[si].text}
		for pi, text := range draft[si].phrases {
			ph := phrase{text: text, start: -1, end: -1}
			if sp := spans[key{si, pi}]; sp != nil {
				ph.start, ph.end = asr[sp.lo].start, asr[sp.hi].end
			}
			s.phrases = append(s.phrases, ph)
		}
		out = append(out, s)
	}
	if !matchedAny {
		warns = []string{"解说稿和配音对不上（可能连错了文本），字幕用了识别出来的文字"}
	}
	fillPhraseTimes(out, total, len(asr) == 0)
	for i := range out {
		ps := out[i].phrases
		out[i].Ref = "s" + strconv.Itoa(i+1)
		out[i].StartMs, out[i].EndMs = ps[0].start, ps[len(ps)-1].end
		out[i].CardNo = voices[voiceAt(voices, out[i].StartMs)].card.No
	}
	return out, warns
}

// recognizedSentences 识别单位 [lo, hi] 里的字按识别出来的句子分组，每组一句（字幕按字数切成条，起止取字的时间）。
// 只有口癖的一组不要（漏念的地方常常只剩一个「嗯」，给它出一条字幕没有意义）。
func recognizedSentences(words []narrWord, asr []unit, lo, hi int) []Sentence {
	var picked []narrWord
	last := -1
	for k := max(lo, 0); k <= hi && k < len(asr); k++ {
		if wi := asr[k].word; wi != last {
			picked = append(picked, words[wi])
			last = wi
		}
	}
	var out []Sentence
	for len(picked) > 0 {
		n := 1
		for n < len(picked) && picked[n].utter == picked[0].utter {
			n++
		}
		group := picked[:n]
		picked = picked[n:]
		onlyFillers := true
		for _, w := range group {
			if k := fillerKey(w.Text); !strongFillers[k] && !weakFillers[k] {
				onlyFillers = false
			}
		}
		if onlyFillers {
			continue
		}
		var plain []Word
		for _, w := range group {
			plain = append(plain, w.Word)
		}
		s := Sentence{Text: joinWords(plain)}
		var chunk []Word
		flush := func() {
			if len(chunk) > 0 {
				s.phrases = append(s.phrases, phrase{text: joinWords(chunk), start: chunk[0].StartMs, end: chunk[len(chunk)-1].EndMs})
				chunk = nil
			}
		}
		for _, w := range plain {
			if len(chunk) > 0 && runeLen(joinWords(append(chunk, w))) > subtitleChunkRunes {
				flush()
			}
			chunk = append(chunk, w)
		}
		flush()
		out = append(out, s)
	}
	return out
}

// groupWarning 一组对不上的原稿怎么处理的（点名原稿第几句）。
func groupWarning(draft []narrDraft, g *narrGroup, got []Sentence) string {
	which := fmt.Sprintf("解说稿第 %d 句「%s」", g.first+1, quoteHead(draft[g.first].text))
	if g.last > g.first {
		which = fmt.Sprintf("解说稿第 %d~%d 句（「%s」起）", g.first+1, g.last+1, quoteHead(draft[g.first].text))
	}
	if len(got) == 0 {
		return which + "在配音里没有找到（可能漏念了），没有出字幕"
	}
	var heard []string
	for _, s := range got {
		heard = append(heard, s.Text)
	}
	return which + "和配音对不上，这段字幕用了识别出来的文字「" + quoteHead(strings.Join(heard, "")) + "」"
}

// quoteHead 警告里引用的一段文字：去掉首尾的标点与空白，太长时截断。
func quoteHead(s string) string {
	s = strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsPunct(r) })
	if runeLen(s) > 16 {
		return headRunes(s, 16) + "…"
	}
	return s
}

// narrScript 解说稿：连进来的文本素材（in.Script）优先；没有时每条配音用它那次文字转语音的原稿（in.VoiceScripts，
// 从生成快照还原 —— 不读画布上节点现在的文字，生成之后改过的话节点上的字与声音对不上），再没有用识别出来的文字。
func narrScript(in PlanInput, voices []narrVoice) string {
	if s := strings.TrimSpace(in.Script); s != "" {
		return s
	}
	var parts []string
	for _, v := range voices {
		if s := strings.TrimSpace(in.VoiceScripts[v.card.No]); s != "" {
			parts = append(parts, s)
			continue
		}
		for _, u := range in.Transcripts[v.card.No].Utterances {
			if t := strings.TrimSpace(u.Text); t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// fillPhraseTimes 对不上的字幕条按前后插值、保证单调；even = 完全没有时间，按字数均分整段配音。
func fillPhraseTimes(sents []Sentence, total int, even bool) {
	var all []*phrase
	for i := range sents {
		for j := range sents[i].phrases {
			all = append(all, &sents[i].phrases[j])
		}
	}
	if len(all) == 0 {
		return
	}
	if even {
		weights := make([]int, len(all))
		for i, p := range all {
			weights[i] = max(spokenRunes(p.text), 1)
		}
		parts := distribute(total, weights)
		at := 0
		for i, p := range all {
			p.start, p.end = at, at+parts[i]
			at += parts[i]
		}
		return
	}
	// 单调：起点不早于上一条的终点。
	last := 0
	for _, p := range all {
		if p.start >= 0 {
			p.start = max(p.start, last)
			p.end = max(p.end, p.start+1)
			last = p.end
		}
	}
	// 插值：连续对不上的几条，按字数分「上一条终点 ~ 下一条起点」之间的时间。
	for i := 0; i < len(all); {
		if all[i].start >= 0 {
			i++
			continue
		}
		j := i
		for j < len(all) && all[j].start < 0 {
			j++
		}
		lo := 0
		if i > 0 {
			lo = all[i-1].end
		}
		hi := total
		if j < len(all) {
			hi = all[j].start
		}
		hi = max(hi, lo+(j-i))
		weights := make([]int, j-i)
		for k := i; k < j; k++ {
			weights[k-i] = max(spokenRunes(all[k].text), 1)
		}
		parts := distribute(hi-lo, weights)
		at := lo
		for k := i; k < j; k++ {
			all[k].start, all[k].end = at, at+parts[k-i]
			at += parts[k-i]
		}
		i = j
	}
}

// ScriptPart 生成输入快照（`generation.inputs_snapshot`）里的一条，只取文字转语音念的那部分。
type ScriptPart struct {
	Kind  string `json:"kind"`
	Order int    `json:"order"`
	Text  string `json:"text"`
}

// SpokenScript 那次文字转语音念的是什么：按连线顺序的文本输入，再加上节点自己的提示词，换行连接 ——
// 与 provider/app.composeInputs 拼给上游的顺序一致（素材词那段不算原稿：它不是用户写的解说）。
func SpokenScript(inputs []ScriptPart, prompt string) string {
	parts := append([]ScriptPart(nil), inputs...)
	sort.SliceStable(parts, func(i, j int) bool { return parts[i].Order < parts[j].Order })
	var out []string
	for _, p := range parts {
		if p.Kind == "text" && strings.TrimSpace(p.Text) != "" {
			out = append(out, p.Text)
		}
	}
	if t := strings.TrimSpace(prompt); t != "" {
		out = append(out, t)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// narrVoice 一条配音与它在成片里的起点。
type narrVoice struct {
	card   Card
	offset int
}

// narrVoices 解说的配音：用途为人声的音频，多条按编号顺序首尾相接（SCT-35）。返回各条与总长。
func narrVoices(cards []Card) ([]narrVoice, int) {
	var out []narrVoice
	at := 0
	for _, c := range cards {
		if c.Kind == InputAudio && c.AudioRole == RoleVoice {
			out = append(out, narrVoice{card: c, offset: at})
			at += c.DurationMs
		}
	}
	return out, at
}

// voiceAt 成片里 t 毫秒处是第几条配音（越界的归最后一条或第一条）。
func voiceAt(voices []narrVoice, t int) int {
	for i := len(voices) - 1; i > 0; i-- {
		if t >= voices[i].offset {
			return i
		}
	}
	return 0
}

// TalkChunkRunes 口播稿一次交给模型的字数上限（设计建议值）。30 分钟的口播大约七八千字：整份塞进一次调用的话，
// 输出（每句的去留）跟着变长，越长越容易写坏 JSON（脚本域「输出过长写出裸引号」的同一类问题）。超过的分段规划、
// 每段独立调一次，结果按句号拼起来。
const TalkChunkRunes = 3000

// TalkChunks 把句子分成若干段，每段的文字不超过 maxRunes —— **不切句**：单独一句就超过上限的自成一段。
func TalkChunks(sents []Sentence, maxRunes int) [][]Sentence {
	if maxRunes <= 0 {
		maxRunes = TalkChunkRunes
	}
	var out [][]Sentence
	var cur []Sentence
	size := 0
	for _, s := range sents {
		n := len([]rune(s.Text))
		if len(cur) > 0 && size+n > maxRunes {
			out = append(out, cur)
			cur, size = nil, 0
		}
		cur = append(cur, s)
		size += n
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}
