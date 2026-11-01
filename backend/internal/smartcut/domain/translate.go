package domain

import (
	"sort"
	"strconv"

	tl "github.com/li904338203/tabtv/backend/internal/timeline/domain"
)

// 字幕翻译（SC-F-125 / 126，docs/agent/smartcut SCT-36）：按序号逐条翻译，结果靠序号对回原条 ——
// 不按文字匹配（模型会合并或拆分句子，按文字找会张冠李戴）。

// TranslateBatch 一次交给模型的条数（设计建议值）：一条字幕轨可能有上千条，整份塞进一次调用的话输出过长、
// 越长越容易写坏 JSON（脚本域「输出过长写出裸引号」的同一类问题）。
const TranslateBatch = 50

// translatedTrack 新建译文字幕轨的临时 ID。
const translatedTrack = "tmp-track-translate"

// TranslatedTrackTmpID 新建译文字幕轨的临时 ID（保存后按它从 ID 对照里取正式 ID）。
func TranslatedTrackTmpID() string { return translatedTrack }

// SubtitleOrder 一条字幕轨上的字幕在 c.Subtitles 里的下标，按起点排序（起点相同按原来的先后）。翻译的序号
// （从 1 起）就是这个顺序。
func SubtitleOrder(c tl.Content, trackID string) []int {
	var out []int
	for i, s := range c.Subtitles {
		if s.TrackID == trackID {
			out = append(out, i)
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return c.Subtitles[out[a]].StartMs < c.Subtitles[out[b]].StartMs })
	return out
}

// 翻译的写法。
const (
	TranslateNewTrack = "newTrack"
	TranslateReplace  = "replace"
)

// ApplyTranslation 把译文写进内容。texts 按 SubtitleOrder 的序号（从 1 起），没有的那几条是没译出来的：
//
//   - newTrack：新建一条目标语言的字幕轨（样式照抄原轨），只放译出来的那几条，时间、锚点、样式照抄原条；
//   - replace：只改译出来的那几条的文字，没译出来的原样不动（SC-F-126）。
//
// 返回新内容与译文所在的轨道 ID（newTrack 时是临时 ID）。
func ApplyTranslation(c tl.Content, trackID string, texts map[int]string, mode, lang string) (tl.Content, string) {
	order := SubtitleOrder(c, trackID)
	out := c
	out.Subtitles = append([]tl.ContentSubtitle(nil), c.Subtitles...)
	if mode == TranslateReplace {
		for n, i := range order {
			if t, ok := texts[n+1]; ok {
				out.Subtitles[i].Text = t
			}
		}
		return out, trackID
	}
	var src tl.ContentTrack
	for _, t := range c.Tracks {
		if t.ID == trackID {
			src = t
		}
	}
	out.Tracks = append(append([]tl.ContentTrack(nil), c.Tracks...), tl.ContentTrack{ID: translatedTrack,
		Kind: tl.TrackSubtitle, Lang: lang, Style: src.Style})
	for n, i := range order {
		t, ok := texts[n+1]
		if !ok {
			continue
		}
		s := c.Subtitles[i]
		s.ID, s.TrackID, s.Text = "tmp-tr-"+strconv.Itoa(n+1), translatedTrack, t
		out.Subtitles = append(out.Subtitles, s)
	}
	return out, translatedTrack
}
