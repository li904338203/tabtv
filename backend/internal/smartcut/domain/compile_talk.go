package domain

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	tl "github.com/li904338203/tabtv/backend/internal/timeline/domain"
)

// 口播精剪（SC-F-98~100，dev/01 §4.3 第 2 条）：
// 保留句 → 字级时间区间 → 剪点外扩 30~200ms（取相邻停顿的一半，不越过相邻的字）→ 在剪点 ±60ms 内取波形
// 能量最低处校正（不进字里）→ 每个剪点 30ms 声音淡入淡出 → 合并间隔小于 120ms 的相邻区间。
// 保留的句子里超过 600ms 的停顿与口癖（嗯、啊）也剪掉（SC-F-98；兜底方案就是只做这一步）。

// 口播参数（SC-PARAM-26，设计建议值）。
const (
	PauseCutMs     = 600
	CutPadMinMs    = 30
	CutPadMaxMs    = 200
	EnergySearchMs = 60
	CutFadeMs      = 30
	MergeGapMs     = 120
	// weakFillerGapMs 「啊」「哦」这类字只有前后都停顿过这么久（或在句首）才算口癖 ——「好啊」的「啊」不是。
	weakFillerGapMs = 120
)

// 口癖（按识别结果里保留下来的字判断，识别时不开「顺滑」）。强口癖在哪儿都算；弱口癖要看位置。
var (
	strongFillers = map[string]bool{"嗯": true, "嗯嗯": true, "呃": true, "额": true, "um": true, "umm": true,
		"uh": true, "er": true, "erm": true, "hmm": true, "mm": true}
	weakFillers = map[string]bool{"啊": true, "哦": true, "噢": true, "唔": true, "诶": true, "欸": true, "哎": true,
		"ah": true}
)

func fillerKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// talkWord 一个字与它的去留。
type talkWord struct {
	Word
	sentence int
	keep     bool
	filler   bool
}

// talkInterval 剪出来的一段（素材时间）以及这段里保留的字。
type talkInterval struct {
	card       Card
	start, end int
	words      []talkWord
}

// compileTalk 口播精剪。fallback：保留全部句子，只剪长停顿与口癖。
func compileTalk(in PlanInput, d Draft, fallback bool) (Result, error) {
	sents := TalkSentences(in)
	if len(sents) == 0 {
		return Result{}, &ErrUnsuitable{Reason: UnsuitableASRMissing, Detail: "口播还没有识别结果"}
	}
	keep := make([]bool, len(sents))
	if fallback {
		for i := range keep {
			keep[i] = true
		}
	} else {
		index := map[string]int{}
		for i, s := range sents {
			index[s.Ref] = i
		}
		for _, r := range d.Keep {
			fi, okF := index[r.From]
			ti, okT := index[r.To]
			if !okF || !okT {
				continue
			}
			for k := fi; k <= ti; k++ {
				keep[k] = true
			}
		}
	}
	cards := cardIndex(in.Cards)

	var res Result
	var intervals []talkInterval
	fillers, pauses := 0, 0
	spans := talkSpans(in)
	for _, k := range talkSpanOrder(sents) {
		card := cards[spans[k].card.No]
		words := talkWords(sents, k, keep, in.Spec.KeepFillers)
		for _, w := range words {
			// 只数真的剪掉的：选了保留口癖时它们留在片子里，摘要不能说「去掉了」。
			if w.filler && keep[w.sentence] && !w.keep {
				fillers++
			}
		}
		ivs, p := talkCuts(card, words, in.Peaks[card.No], spans[k].InMs, spans[k].OutMs)
		pauses += p
		intervals = append(intervals, ivs...)
	}
	if len(intervals) == 0 {
		return Result{}, &ErrUnsuitable{Reason: UnsuitableASRMissing, Detail: "方案没有保留任何一句"}
	}

	cuts := make([]cut, len(intervals))
	for k, iv := range intervals {
		cuts[k] = cut{card: iv.card, inMs: iv.start, outMs: iv.end, speed: tl.DefaultSpeed}
	}
	starts, total := placeCuts(cuts)
	a := arrangement{videoRole: RoleVoice}
	for k, c := range cuts {
		clip := c.clipOf(videoClipID(k))
		fade := min(CutFadeMs, c.playLen()/2)
		clip.FadeInMs, clip.FadeOutMs = fade, fade
		a.video = append(a.video, clip)
	}
	if in.Spec.WithSubtitles && (d.Subtitle == nil || *d.Subtitle) {
		a.subtitles = talkSubtitles(intervals, cuts, starts)
		a.replaceSubtitles = len(a.subtitles) > 0
		a.subtitleLang = firstNonEmpty(in.Spec.SubtitleLang, transcriptLang(in, sents))
	}
	if music, ok, _ := pickMusic(in.Cards, d.Music); ok && d.Music != "" {
		clip, warn := musicClip(music, total)
		a.music = clip
		if warn != "" {
			res.Warnings = append(res.Warnings, warn)
		}
	}

	kept := 0
	for _, k := range keep {
		if k {
			kept++
		}
	}
	for _, dr := range d.Drop {
		if dr.Reason != "" {
			res.Notes = append(res.Notes, "删掉了 "+dr.Sentence+"："+dr.Reason)
		}
	}
	if fallback {
		res.UsedFallback = true
		res.Notes = append([]string{FallbackNote}, res.Notes...)
	}
	parts := []string{fmt.Sprintf("保留了 %d 句（共 %d 句）", kept, len(sents))}
	if fillers > 0 {
		parts = append(parts, fmt.Sprintf("去掉口癖 %d 处", fillers))
	}
	switch {
	case pauses > 0:
		parts = append(parts, fmt.Sprintf("剪掉长停顿 %d 处", pauses))
	case fallback:
		// 兜底只做去口癖与去停顿：一处都没剪到也要说出来，否则用户看不出规则到底剪了什么。
		parts = append(parts, "没有超过 "+secText(PauseCutMs)+" 秒的长停顿")
	}
	res.Summary = strings.Join(parts, "，") + fmt.Sprintf("，成片 %s 秒", secText(total))
	res.UsedClips = usedOrder(cuts)
	return finish(in, a, cuts, res), nil
}

// talkSpanOrder 口播各段的顺序（按句子出现的顺序；没有句子的段不出现）。
func talkSpanOrder(sents []Sentence) []int {
	var out []int
	seen := map[int]bool{}
	for _, s := range sents {
		if !seen[s.span] {
			seen[s.span] = true
			out = append(out, s.span)
		}
	}
	return out
}

// talkWords 一段里的全部字（按时间），标出去留与口癖。
func talkWords(sents []Sentence, span int, keep []bool, keepFillers bool) []talkWord {
	var out []talkWord
	for si, s := range sents {
		if s.span != span {
			continue
		}
		for wi, w := range s.words {
			tw := talkWord{Word: w, sentence: si}
			key := fillerKey(w.Text)
			switch {
			case strongFillers[key]:
				tw.filler = true
			case weakFillers[key]:
				first := wi == 0
				gapBefore, gapAfter := weakFillerGapMs, weakFillerGapMs
				if wi > 0 {
					gapBefore = w.StartMs - s.words[wi-1].EndMs
				}
				if wi+1 < len(s.words) {
					gapAfter = s.words[wi+1].StartMs - w.EndMs
				}
				only := len(s.words) == 1
				tw.filler = first || only || (gapBefore >= weakFillerGapMs && gapAfter >= weakFillerGapMs)
			}
			tw.keep = keep[si] && !(tw.filler && !keepFillers)
			out = append(out, tw)
		}
	}
	return out
}

// talkCuts 一段素材的剪法：连续保留、停顿不超过 600ms 的字归成一段，外扩、能量校正、合并。
// lo / hi 是这一段在素材里的边界：外扩与能量校正都不越过它（用户剪掉的开头不会被外扩带回来）。
// 返回剪出来的段与剪掉的长停顿处数。
func talkCuts(card Card, words []talkWord, peaks Peaks, lo, hi int) ([]talkInterval, int) {
	type run struct{ first, last int }
	var runs []run
	pauses := 0
	for i := 0; i < len(words); i++ {
		if !words[i].keep {
			continue
		}
		if len(runs) > 0 {
			r := &runs[len(runs)-1]
			if r.last == i-1 {
				if words[i].StartMs-words[r.last].EndMs <= PauseCutMs {
					r.last = i
					continue
				}
				pauses++
			}
		}
		runs = append(runs, run{i, i})
	}
	limit := card.DurationMs
	if limit <= 0 || hi < limit {
		limit = hi
	}
	var ivs []talkInterval
	for _, r := range runs {
		a, b := words[r.first].StartMs, words[r.last].EndMs
		prevEnd, nextStart := max(0, lo), limit
		if r.first > 0 {
			prevEnd = min(words[r.first-1].EndMs, a)
		}
		if r.last+1 < len(words) {
			nextStart = max(words[r.last+1].StartMs, b)
		}
		start := a - cutPad(a-prevEnd)
		end := b + cutPad(nextStart-b)
		if peaks.IntervalMs > 0 && len(peaks.Values) > 0 {
			start = quietest(peaks, start, max(prevEnd, start-EnergySearchMs), min(a, start+EnergySearchMs))
			end = quietest(peaks, end, max(b, end-EnergySearchMs), min(nextStart, end+EnergySearchMs))
		}
		iv := talkInterval{card: card, start: start, end: end}
		for k := r.first; k <= r.last; k++ {
			if words[k].keep {
				iv.words = append(iv.words, words[k])
			}
		}
		ivs = append(ivs, iv)
	}
	// 合并间隔小于 120ms 的相邻段（剪口太碎反而听得出来）。
	var merged []talkInterval
	for _, iv := range ivs {
		if n := len(merged); n > 0 && iv.start-merged[n-1].end < MergeGapMs {
			merged[n-1].end = max(merged[n-1].end, iv.end)
			merged[n-1].words = append(merged[n-1].words, iv.words...)
			continue
		}
		merged = append(merged, iv)
	}
	// 太短的段撑到 100ms（在素材范围内），撑不开就不要。
	out := merged[:0]
	for _, iv := range merged {
		if iv.end-iv.start < tl.MinClipMs {
			grow := tl.MinClipMs - (iv.end - iv.start)
			iv.start = max(max(0, lo), iv.start-grow/2)
			iv.end = iv.start + tl.MinClipMs
			if limit > 0 && iv.end > limit {
				iv.end = limit
				iv.start = max(max(0, lo), limit-tl.MinClipMs)
			}
			if iv.end-iv.start < tl.MinClipMs {
				continue
			}
		}
		out = append(out, iv)
	}
	return out, pauses
}

// cutPad 剪点外扩：相邻停顿的一半，30~200ms，且不越过相邻的字（停顿本身比 30ms 还短时就只扩到那个字）。
func cutPad(gap int) int {
	if gap <= 0 {
		return 0
	}
	return min(max(gap/2, CutPadMinMs), CutPadMaxMs, gap)
}

// quietest 在 [lo, hi] 里按波形的采样间隔找能量最低的时刻；一样低时取离 at 最近的，再一样取早的。
// 窗口为空（lo > hi）时原样返回 at。
func quietest(p Peaks, at, lo, hi int) int {
	if lo > hi {
		return at
	}
	best, bestE := at, p.at(at)
	if at < lo || at > hi {
		best, bestE = lo, p.at(lo)
	}
	step := p.IntervalMs
	first := (lo + step - 1) / step * step
	for t := first; t <= hi; t += step {
		e := p.at(t)
		if e < bestE || (e == bestE && absInt(t-at) < absInt(best-at)) {
			best, bestE = t, e
		}
	}
	return best
}

// talkSubtitles 口播字幕：每句保留下来的字按所在的段分组，每组按字数切成字幕条，跟随片段（SC-F-100）。
func talkSubtitles(ivs []talkInterval, cuts []cut, starts []int) []tl.ContentSubtitle {
	var out []tl.ContentSubtitle
	for k, iv := range ivs {
		var group []Word
		flush := func() {
			if len(group) == 0 {
				return
			}
			text := joinWords(group)
			if s, ok := anchoredSubtitle("tmp-s"+strconv.Itoa(len(out)+1), text, videoClipID(k), starts[k], clipOfCut(cuts[k]),
				group[0].StartMs, group[len(group)-1].EndMs); ok {
				out = append(out, s)
			}
			group = nil
		}
		sentence := -1
		for _, w := range iv.words {
			if w.sentence != sentence || runeLen(joinWords(append(group, w.Word))) > subtitleChunkRunes {
				flush()
				sentence = w.sentence
			}
			group = append(group, w.Word)
		}
		flush()
	}
	return out
}

// transcriptLang 识别出的语言（新建字幕轨用）。
func transcriptLang(in PlanInput, sents []Sentence) string {
	if len(sents) == 0 {
		return ""
	}
	return normalizeLang(in.Transcripts[sents[0].CardNo].Language)
}

// NormalizeLang 识别结果里的语言 → 字幕轨的语言代码（zh-CN / en / ja / ko；认不出的为空）。
func NormalizeLang(l string) string { return normalizeLang(l) }

// normalizeLang 识别结果里的语言 → 字幕轨的语言代码（认不出的留空，用默认值）。
func normalizeLang(l string) string {
	switch strings.ToLower(strings.TrimSpace(l)) {
	case "zh", "zh-cn", "zh_cn", "cmn", "chinese", "mandarin":
		return "zh-CN"
	case "en", "en-us", "english":
		return "en"
	case "ja", "ja-jp", "japanese":
		return "ja"
	case "ko", "ko-kr", "korean":
		return "ko"
	}
	return ""
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
