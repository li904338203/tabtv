package domain

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	tl "github.com/li904338203/tabtv/backend/internal/timeline/domain"
)

// 识别结果 → 字幕（智能识别字幕 SC-F-120，docs/agent/smartcut SCT-36）。

// ASRSubtitles 把识别结果做成跟随片段的字幕：
//
//   - 按素材识别、按片段的取用区间映射到成片：每个片段只取落在取用区间里的字（一个字至少一半在区间里才算），
//     一句跨了区间就切开；
//   - 逐句出字幕（识别出的一句一条；过长的先在标点处、再按字数切），文字不带标点；
//   - 锚点挂在片段上，成片里的位置由 tl.Layout 算（不自己累加）；之后裁切、移动片段时字幕跟着走，
//     被裁掉区间里的那几条在读取时隐藏（SC-F-56）。
//
// 选哪些片段与识别同一个规则（TranscribeSources）。transcripts 按媒体 ID：没有识别结果的片段不出字幕，
// 也不算覆盖到（它原来的字幕不动）。返回字幕（TrackID 待定，ID 是临时的）与覆盖到的片段 ID。
func ASRSubtitles(c tl.Content, clipIDs []string, transcripts map[int64]Transcript) ([]tl.ContentSubtitle, map[string]bool) {
	clips, _ := transcribeClips(c, clipIDs)
	lay := tl.Layout(c)
	covered := map[string]bool{}
	var out []tl.ContentSubtitle
	for _, cc := range clips {
		tr, ok := transcripts[cc.MediaID]
		if !ok {
			continue
		}
		covered[cc.ID] = true
		place, ok := lay.Clips[cc.ID]
		if !ok {
			continue
		}
		clip := tl.Clip{SourceInMs: cc.SourceInMs, SourceOutMs: cc.SourceOutMs, Speed: tl.DefaultSpeed}
		if cc.Speed != nil && *cc.Speed > 0 {
			clip.Speed = *cc.Speed
		}
		for _, u := range tr.Utterances {
			for _, chunk := range utteranceChunks(u, cc.SourceInMs, cc.SourceOutMs) {
				from := max(chunk[0].StartMs, cc.SourceInMs)
				to := min(chunk[len(chunk)-1].EndMs, cc.SourceOutMs)
				if s, ok := anchoredSubtitle("tmp-asr-"+strconv.Itoa(len(out)+1), joinWords(chunk), cc.ID, place.StartMs,
					clip, from, to); ok {
					out = append(out, s)
				}
			}
		}
	}
	return out, covered
}

// SubtitlePauseMs 识别字幕在多长的停顿处断开（设计建议值）：不断的话停顿期间那条字幕一直挂在屏幕上，
// 而有的识别结果根本不带标点，停顿是唯一的线索。
const SubtitlePauseMs = 300

// utteranceChunks 识别出的一句里落在 [in, out] 的字，按字幕条切开：识别原文里有断句标点的地方、停顿超过
// SubtitlePauseMs 的地方断开，再按 subtitleChunkRunes 个字断开。没有字级时间的整句当一个字（时间是整句的起止）。
func utteranceChunks(u Utterance, in, out int) [][]Word {
	words := cleanWords(u, 0)
	breaks := punctuationBreaks(u.Text, words)
	var chunks [][]Word
	var cur []Word
	flush := func() {
		if len(cur) > 0 {
			chunks = append(chunks, cur)
			cur = nil
		}
	}
	for i, w := range words {
		// 一个字至少一半落在取用区间里才算（整句没有字级时间时就是整句的一半）。
		if overlap := min(w.EndMs, out) - max(w.StartMs, in); overlap*2 < w.EndMs-w.StartMs || overlap <= 0 {
			flush()
			continue
		}
		if len(cur) > 0 && (breaks[i] || w.StartMs-cur[len(cur)-1].EndMs >= SubtitlePauseMs ||
			runeLen(joinWords(append(cur, w))) > subtitleChunkRunes) {
			flush()
		}
		cur = append(cur, w)
	}
	flush()
	return chunks
}

// punctuationBreaks 第 i 个字前面是不是断句的地方：在识别原文里按顺序找每个字，两个字之间夹着标点就算。
// 找不到的字（识别原文与逐字结果对不上，比如数字写法不同）不给判断，交给按字数断。
func punctuationBreaks(text string, words []Word) []bool {
	out := make([]bool, len(words))
	pos := 0
	for i, w := range words {
		t := strings.TrimSpace(w.Text)
		if t == "" {
			continue
		}
		at := strings.Index(text[pos:], t)
		if at < 0 {
			continue
		}
		if i > 0 && hasBreakPunct(text[pos:pos+at]) {
			out[i] = true
		}
		pos += at + len(t)
	}
	return out
}

func hasBreakPunct(s string) bool {
	for len(s) > 0 {
		r, n := utf8.DecodeRuneInString(s)
		if unicode.IsPunct(r) && !strings.ContainsRune("'’-·", r) {
			return true
		}
		s = s[n:]
	}
	return false
}

// MergeASRSubtitles 把识别字幕放进字幕轨：放进哪一条见 subtitleTrackFor（按识别语言）；只替换这条轨上跟随
// 「覆盖到的片段」的字幕 —— 别的字幕（手写的、跟随别的片段的）原样保留，重新识别一次就是把这几段的字幕换一遍。
// 没有可用的字幕轨（满了）时返回 ok=false；changed=false 表示什么都没变（不用写）。
func MergeASRSubtitles(c tl.Content, subs []tl.ContentSubtitle, covered map[string]bool, lang string) (
	out tl.Content, changed, ok bool) {
	out = c
	out.Tracks = append([]tl.ContentTrack(nil), c.Tracks...)
	si := subtitleTrackFor(out.Tracks, lang)
	if si < 0 {
		return c, false, false
	}
	if si == len(out.Tracks) {
		if len(subs) == 0 {
			return c, false, true
		}
		out.Tracks = append(out.Tracks, newSubtitleTrack(lang))
	}
	track := out.Tracks[si].ID
	out.Subtitles = nil
	for _, s := range c.Subtitles {
		if id, anchored := anchorClipID(s.Anchor); s.TrackID == track && anchored && covered[id] {
			changed = true
			continue
		}
		out.Subtitles = append(out.Subtitles, s)
	}
	for _, s := range subs {
		s.TrackID = track
		out.Subtitles = append(out.Subtitles, s)
		changed = true
	}
	if !changed {
		// 什么都没写也没换掉：不为了给轨道填一个语言就推进一次版本。
		return c, false, true
	}
	return out, true, true
}
