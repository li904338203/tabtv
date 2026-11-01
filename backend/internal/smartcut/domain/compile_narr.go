package domain

import (
	"fmt"
	"strconv"

	tl "github.com/li904338203/tabtv/backend/internal/timeline/domain"
)

// 解说成片（SC-F-102~104，dev/01 §4.3 第 3 条）：以配音时长为准，给每句配画面。
//
//   - 画面铺满整段配音（配音之外的时间不铺画面）：第 k 句的画面从这一句的起点放到下一句的起点
//     （第一句从 0 起，最后一句到配音结束）；方案里没配画面的句子沿用上一句的画面。
//   - 一句配几段画面就按顺序均分句长；某段素材不够长时，差的时间结转给后面的画面（不放慢、不重复，
//     开关打开时按开关处理），最后仍不够就如实说明。
//   - 字幕用原稿文字（没有原稿时用识别文字；对不上的句子退回识别文字并点名），跟随配音片段（锚点）。
//   - 配音可以有好几条：按编号顺序首尾相接（成片长度 = 各条之和）。

// narrPiece 画面轨上的一段（编译中间态）。
type narrPiece struct {
	card   Card
	take   Take
	target int
	repeat bool
}

// compileNarr 解说成片。fallback：按句均分画面。
func compileNarr(in PlanInput, d Draft, fallback bool) (Result, error) {
	voices, voiceDur := narrVoices(in.Cards)
	if len(voices) == 0 {
		return Result{}, &ErrUnsuitable{Reason: UnsuitableNoVoice, Detail: "解说成片需要一条配音（用途为人声的音频）"}
	}
	for _, v := range voices {
		if _, ok := in.Transcripts[v.card.No]; !ok {
			return Result{}, &ErrUnsuitable{Reason: UnsuitableASRMissing, Detail: "配音（编号 " + v.card.No + "）还没有识别结果"}
		}
	}
	sents, alignWarns := narrSentences(in)
	if len(sents) == 0 {
		return Result{}, &ErrUnsuitable{Reason: UnsuitableASRMissing, Detail: "配音里没有识别出句子"}
	}
	var pictures []Card
	for _, c := range in.Cards {
		if c.IsPicture() {
			pictures = append(pictures, c)
		}
	}
	if len(pictures) == 0 {
		return Result{}, &ErrUnsuitable{Reason: UnsuitableNoPicture, Detail: "没有可用的画面素材"}
	}
	lines := d.Lines
	if fallback || len(lines) == 0 {
		lines = evenLines(sents, pictures)
	}

	// 每句的画面区间：这一句的起点 → 下一句（有画面的）的起点。
	index := map[string]int{}
	for i, s := range sents {
		index[s.Ref] = i
	}
	cards := cardIndex(in.Cards)
	var pieces []narrPiece
	for li, ln := range lines {
		si, ok := index[ln.Sentence]
		if !ok {
			continue
		}
		from := sents[si].StartMs
		if li == 0 {
			from = 0
		}
		to := voiceDur
		if li+1 < len(lines) {
			if nj, ok := index[lines[li+1].Sentence]; ok {
				to = sents[nj].StartMs
			}
		}
		if to <= from {
			continue
		}
		var cs []Card
		for _, no := range ln.Clips {
			if c, ok := cards[no]; ok && c.IsPicture() {
				cs = append(cs, c)
			}
		}
		if len(cs) == 0 {
			continue
		}
		ws := make([]int, len(cs))
		for i := range ws {
			ws[i] = 1
		}
		for i, part := range distribute(to-from, ws) {
			pieces = append(pieces, narrPiece{card: cs[i], take: ln.Take, target: part})
		}
	}
	if len(pieces) == 0 {
		return Result{}, &ErrUnsuitable{Reason: UnsuitableNoPicture, Detail: "方案没有给任何一句配上画面"}
	}
	// 太短的一份并进前一份（第一份就并进后一份）。
	merged := pieces[:0]
	for _, p := range pieces {
		if n := len(merged); n > 0 && p.target < tl.MinClipMs {
			merged[n-1].target += p.target
			continue
		}
		merged = append(merged, p)
	}
	pieces = merged
	if len(pieces) > 1 && pieces[0].target < tl.MinClipMs {
		pieces[1].target += pieces[0].target
		pieces = pieces[1:]
	}

	// 分配：素材不够长时差的结转给后面；放慢与重复按开关。
	var cuts []cut
	carry := 0
	place := func(p narrPiece) int {
		want := p.target + carry
		speed := tl.DefaultSpeed
		got := want
		if p.card.Kind == InputVideo {
			avail := p.card.DurationMs
			if want > avail && in.Spec.AllowSlow {
				speed = max(MinSlowSpeed, avail*100/max(want, 1))
				avail = avail * 100 / speed
			}
			got = min(want, avail)
		}
		if got < tl.MinClipMs {
			carry = want
			return 0
		}
		c := cut{card: p.card, speed: speed, take: p.take}
		c.inMs, c.outMs = narrWindow(p.card, p.take, srcLenFor(got, speed))
		cuts = append(cuts, c)
		carry = want - c.playLen()
		return c.playLen()
	}
	for _, p := range pieces {
		place(p)
	}
	// 最后仍差：从后往前把有余量的画面拉长（只让后面几句的画面晚一点出现，不让结尾那段配音没有画面）。
	for k := len(cuts) - 1; k >= 0 && carry >= tl.MinClipMs; k-- {
		c := &cuts[k]
		if c.card.Kind != InputVideo {
			continue
		}
		slack := c.card.DurationMs*100/c.speed - c.playLen()
		if slack <= 0 {
			continue
		}
		grow := min(slack, carry)
		before := c.playLen()
		c.inMs, c.outMs = narrWindow(c.card, c.take, min(srcLenFor(before+grow, c.speed), c.card.DurationMs))
		carry -= c.playLen() - before
	}
	var repeated []string
	if carry >= tl.MinClipMs && in.Spec.AllowRepeat {
		used := usedOrder(cuts)
		for k := 0; carry >= tl.MinClipMs && k < 3*len(used); k++ {
			c := cards[used[k%len(used)]]
			if place(narrPiece{card: c, take: Take{Kind: TakeFull}, repeat: true}) > 0 {
				repeated = append(repeated, c.No)
			}
		}
	}
	_, total := placeCuts(cuts)

	var res Result
	res.Warnings = append(res.Warnings, alignWarns...)
	a := arrangement{}
	for k, c := range cuts {
		a.video = append(a.video, c.clipOf(videoClipID(k)))
	}
	// 配音按编号顺序首尾相接放上人声轨；字幕跟随它所在的那一条（锚点用那一条自己的时间）。
	for k, v := range voices {
		a.voices = append(a.voices, tl.ContentClip{ID: voiceClipID(k), MediaID: v.card.MediaID, StartMs: intPtr(v.offset),
			SourceInMs: 0, SourceOutMs: v.card.DurationMs, Origin: tl.OriginAI, SourceNodeID: v.card.SourceNodeID})
	}
	if in.Spec.WithSubtitles {
		for _, s := range sents {
			for _, p := range s.phrases {
				k := voiceAt(voices, p.start)
				v := voices[k]
				vc := tl.Clip{SourceInMs: 0, SourceOutMs: v.card.DurationMs, Speed: tl.DefaultSpeed}
				if sub, ok := anchoredSubtitle("tmp-s"+strconv.Itoa(len(a.subtitles)+1), p.text, voiceClipID(k), v.offset, vc,
					p.start-v.offset, min(p.end, v.offset+v.card.DurationMs)-v.offset); ok {
					a.subtitles = append(a.subtitles, sub)
				}
			}
		}
		a.replaceSubtitles = len(a.subtitles) > 0
		a.subtitleLang = firstNonEmpty(in.Spec.SubtitleLang, normalizeLang(in.Transcripts[voices[0].card.No].Language))
	}
	if music, ok, _ := pickMusic(in.Cards, d.Music); ok {
		clip, warn := musicClip(music, total)
		a.music = clip
		if warn != "" {
			res.Warnings = append(res.Warnings, warn)
		}
	}
	if short := voiceDur - total; short >= tl.MinClipMs {
		res.Warnings = append(res.Warnings, fmt.Sprintf("画面共 %s 秒，不足配音 %s 秒：结尾 %s 秒的配音没有画面，导出时会被截掉",
			secText(total), secText(voiceDur), secText(short)))
		res.Shortage = &Shortage{TotalMs: total, TargetMs: voiceDur}
	}
	slowed := 0
	for _, c := range cuts {
		if c.speed < tl.DefaultSpeed {
			slowed++
		}
	}
	if slowed > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("画面不够长，放慢了 %d 段", slowed))
	}
	if len(repeated) > 0 {
		res.Warnings = append(res.Warnings, "画面不够长，重复用了编号 "+joinNos(uniqStrings(repeated)))
	}
	used := usedOrder(cuts)
	if w := unusedPictureWarning(in.Cards, used); w != "" {
		res.Warnings = append(res.Warnings, w)
	}
	if fallback {
		res.UsedFallback = true
		res.Notes = append([]string{FallbackNote}, res.Notes...)
	}
	res.UsedClips = used
	res.Summary = fmt.Sprintf("解说 %d 句，配了 %d 段画面，成片 %s 秒", len(sents), len(cuts), secText(total))
	return finish(in, a, cuts, res), nil
}

// voiceClipID 第 k 条配音片段的临时 ID（从 1 起）。
func voiceClipID(k int) string { return "tmp-voice-" + strconv.Itoa(k+1) }

// narrWindow 解说的取段：长度由句子定，取段只决定从哪里取 —— 前（从头）、后（到尾）、中（居中）、围绕某一秒。
func narrWindow(c Card, t Take, srcLen int) (in, out int) {
	if c.Kind == InputImage {
		return 0, srcLen
	}
	d := c.DurationMs
	srcLen = min(srcLen, d)
	switch t.Kind {
	case TakeTail:
		in = d - srcLen
	case TakeMiddle:
		in = (d - srcLen) / 2
	case TakeAround:
		in = t.AroundMs - srcLen/2
	}
	in = min(max(in, 0), d-srcLen)
	return in, in + srcLen
}

// evenLines 兜底：按句均分画面。画面比句子多时每句分几段；少时一段画面连着配几句。
func evenLines(sents []Sentence, pictures []Card) []NarrLine {
	n, m := len(sents), len(pictures)
	var out []NarrLine
	if m >= n {
		at := 0
		for i, s := range sents {
			cnt := m*(i+1)/n - m*i/n
			var clips []string
			for k := 0; k < cnt; k++ {
				clips = append(clips, pictures[at].No)
				at++
			}
			out = append(out, NarrLine{Sentence: s.Ref, Clips: clips, Take: Take{Kind: TakeFull}})
		}
		return out
	}
	for j, p := range pictures {
		first := n * j / m
		out = append(out, NarrLine{Sentence: sents[first].Ref, Clips: []string{p.No}, Take: Take{Kind: TakeFull}})
	}
	return out
}
