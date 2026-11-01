package domain

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	tl "github.com/li904338203/tabtv/backend/internal/timeline/domain"
)

// 分镜成片（SC-F-95、96）：镜头顺序与取用区间**以装配为准**（SCT-28 按镜号装配、只补缺口），
// 模型只做包装 —— 转场、开头标题、台词字幕的取舍、配乐。让模型重排镜头会和脚本的镜号对不上。

// BoardShot 是装配好的一镜。
type BoardShot struct {
	// Ref 交给模型的镜头编号（b1~bN，NumberShots 按装配顺序编）。
	Ref          string `json:"ref"`
	ShotNo       int    `json:"shotNo"`
	Source       string `json:"source,omitempty"`
	SourceNodeID string `json:"sourceNodeId,omitempty"`
	MediaID      int64  `json:"mediaId,string"`
	// DurationMs 素材本身的时长；SourceInMs / SourceOutMs 装配给出的取用区间。
	DurationMs  int    `json:"durationMs,omitempty"`
	SourceInMs  int    `json:"sourceInMs"`
	SourceOutMs int    `json:"sourceOutMs"`
	HasAudio    bool   `json:"hasAudio,omitempty"`
	Dialogue    string `json:"dialogue,omitempty"`
	Description string `json:"description,omitempty"`
}

// NumberShots 按装配顺序给镜头编号 b1~bN。
func NumberShots(shots []BoardShot) []BoardShot {
	out := append([]BoardShot(nil), shots...)
	for i := range out {
		out[i].Ref = "b" + strconv.Itoa(i+1)
	}
	return out
}

// ShotRefs 镜头编号（解析方案时校验用）。
func ShotRefs(shots []BoardShot) []string {
	out := make([]string, len(shots))
	for i, s := range shots {
		out[i] = s.Ref
	}
	return out
}

// subtitleChunkRunes 一条字幕最多多少字：超过时在逗号处断开（SC-PARAM-31：每行约 16 个汉字，两行以内）。
const subtitleChunkRunes = 24

// compileBoard 分镜成片的包装。fallback 时方案为空：硬切、台词字幕（打开字幕时）、没有标题。
func compileBoard(in PlanInput, d Draft, fallback bool) (Result, error) {
	if len(in.Shots) == 0 {
		return Result{}, &ErrUnsuitable{Reason: UnsuitableNoShots, Detail: "没有装配出可用的镜头"}
	}
	segs := map[string]Segment{}
	for _, s := range d.Segments {
		segs[s.Clip] = s
	}
	n := len(in.Shots)
	cuts := make([]cut, n)
	overlaps := make([]int, n)
	for k, sh := range in.Shots {
		dur := sh.DurationMs
		if dur <= 0 {
			dur = sh.SourceOutMs
		}
		card := Card{No: sh.Ref, Kind: InputVideo, MediaID: sh.MediaID, DurationMs: dur, HasAudio: sh.HasAudio,
			SourceNodeID: sh.SourceNodeID}
		cuts[k] = cut{card: card, inMs: sh.SourceInMs, outMs: sh.SourceOutMs, speed: tl.DefaultSpeed}
		seg := segs[sh.Ref]
		text := seg.Subtitle
		if text == "" && !seg.SubtitleOff {
			text = sh.Dialogue
		}
		cuts[k].subtitle = strings.TrimSpace(text)
		if k > 0 && seg.Transition != "" && seg.Transition != TransCut {
			overlaps[k] = DefaultTransitionMs
			cuts[k].trans.Kind = seg.Transition
		}
	}
	// 时长以装配为准：转场按相邻两段较短者的一半收短，不改取用区间。
	for k := 1; k < n; k++ {
		o := min(overlaps[k], cuts[k-1].playLen()/2, cuts[k].playLen()/2)
		if o < tl.MinTransitionMs {
			o = 0
		}
		overlaps[k] = o
		cuts[k].trans.DurationMs = o
		if o == 0 {
			cuts[k].trans = tl.Transition{}
		}
	}
	starts, total := placeCuts(cuts)

	var res Result
	a := arrangement{}
	for k, c := range cuts {
		a.video = append(a.video, c.clipOf(videoClipID(k)))
	}
	if in.Spec.WithSubtitles {
		a.subtitles = dialogueSubtitles(cuts, starts, overlaps)
		a.replaceSubtitles = len(a.subtitles) > 0
		a.subtitleLang = in.Spec.SubtitleLang
	}
	a.title = titleLayer(d.Title, total)
	if music, ok, note := pickMusic(in.Cards, d.Music); ok {
		if note != "" {
			res.Notes = append(res.Notes, note)
		}
		clip, warn := musicClip(music, total)
		a.music = clip
		if warn != "" {
			res.Warnings = append(res.Warnings, warn)
		}
	}
	if t := in.Spec.TargetMs; t > 0 && absInt(total-t) >= 1000 {
		res.Notes = append(res.Notes, fmt.Sprintf("分镜成片按镜头时长装配（共 %s 秒），没有按目标时长 %s 秒截取", secText(total), secText(t)))
	}
	if fallback {
		res.UsedFallback = true
		res.Notes = append([]string{FallbackNote}, res.Notes...)
	}
	refs := make([]string, n)
	for k, sh := range in.Shots {
		refs[k] = sh.Ref
	}
	res.UsedClips = refs
	res.Summary = fmt.Sprintf("按分镜装配了 %d 个镜头，成片 %s 秒", n, secText(total))
	return finish(in, a, cuts, res), nil
}

// dialogueSubtitles 台词字幕：按句（过长的在逗号处断开）在镜头的可见范围里按字数分配时长，跟随片段。
func dialogueSubtitles(cuts []cut, starts, overlaps []int) []tl.ContentSubtitle {
	var out []tl.ContentSubtitle
	for k, c := range cuts {
		phrases := subtitlePhrases(c.subtitle, subtitleChunkRunes)
		if len(phrases) == 0 {
			continue
		}
		next := 0
		if k+1 < len(cuts) {
			next = overlaps[k+1]
		}
		lo := c.inMs + overlaps[k]/2
		hi := c.outMs - (next - next/2)
		if hi-lo < tl.MinClipMs {
			lo, hi = c.inMs, c.outMs
		}
		weights := make([]int, len(phrases))
		for i, p := range phrases {
			weights[i] = max(spokenRunes(p), 1)
		}
		bounds := distribute(hi-lo, weights)
		at := lo
		for i, p := range phrases {
			end := at + bounds[i]
			if s, ok := anchoredSubtitle("tmp-s"+strconv.Itoa(len(out)+1), p, videoClipID(k), starts[k], clipOfCut(c), at, end); ok {
				out = append(out, s)
			}
			at = end
		}
	}
	return out
}

// subtitlePhrases 把一段台词切成字幕：先按句末标点与换行断句（标点跟着前一句），
// 超过 maxRunes 的句子在逗号、顿号处断开并尽量凑满一条；仍然超长的原样留着（排版会换行，不在词中间硬断）。
func subtitlePhrases(text string, maxRunes int) []string {
	var out []string
	for _, sentence := range splitKeep(text, "。！？!?；;…\n") {
		if runeLen(sentence) <= maxRunes {
			out = append(out, sentence)
			continue
		}
		cur := ""
		for _, part := range splitKeep(sentence, "，、,：:") {
			if cur != "" && runeLen(cur)+runeLen(part) > maxRunes {
				out = append(out, cur)
				cur = ""
			}
			cur += part
		}
		if cur != "" {
			out = append(out, cur)
		}
	}
	return out
}

// splitKeep 在 seps 里的字符处断开，分隔符留在前一段末尾；去掉首尾空白与空段。
func splitKeep(text, seps string) []string {
	var out []string
	var b strings.Builder
	flush := func() {
		if s := strings.TrimSpace(b.String()); s != "" && hasSpoken(s) {
			out = append(out, s)
		} else if s != "" && len(out) > 0 {
			out[len(out)-1] += s
		}
		b.Reset()
	}
	for _, r := range text {
		if r == '\n' && strings.ContainsRune(seps, '\n') {
			flush()
			continue
		}
		b.WriteRune(r)
		if strings.ContainsRune(seps, r) {
			flush()
		}
	}
	flush()
	return out
}

// hasSpoken 有没有要念出来的字（只有标点的不算一句）。
func hasSpoken(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// spokenRunes 要念出来的字数（不算空白与标点）。
func spokenRunes(s string) int {
	n := 0
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			n++
		}
	}
	return n
}

// distribute 把 total 按权重分成整数段，和恰为 total（按累计比例取整，平滑不偏向哪一头）。
func distribute(total int, weights []int) []int {
	out := make([]int, len(weights))
	sum := sumInts(weights)
	if sum <= 0 {
		return out
	}
	acc, prev := 0, 0
	for i, w := range weights {
		acc += w
		edge := total * acc / sum
		out[i] = edge - prev
		prev = edge
	}
	return out
}
