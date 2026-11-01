package domain

import (
	"encoding/json"
	"sort"
)

// 成片时间（docs/agent/smartcut SCK-15、dev/01 SCD-14、SCD-15）。
//
// 所有轨道统一用成片时间：
//   - 画面轨：start[0] = 0；start[i] = start[i-1] + dur[i-1] − 转入[i]（只扣生效的、i>0 的转入：
//     第一段的转入是从黑场淡入，不与谁重叠）；dur 是成片长度（算上变速）。
//   - 音频 / 字幕 / 文字轨：起点就是存储值（按起点放置，SCD-15）。
//   - 跟随片段的字幕：按片段当前的成片起点与取用区间换算（SC-F-56）。
//
// ⚠️ 这一套只有一个实现：Sequence 的落点、EffectiveDuration、渲染的 xfade offset 与 SnapDuration、
// 整份内容的 Layout 都走 placeVideo。各算各的话，有转场时卡点、字幕、配乐会整体错位，
// 而每一处单独看都"对"。前端那一份由 api/fixtures/smartcut/layout/ 绊住（SC-N-15）。

// seam 是画面轨上的一段：成片长度，以及它与上一段重叠多久（生效的转入；第一段为 0）。
type seam struct{ dur, overlap int }

// placeVideo 画面轨每段的成片起点，以及最后一段的结束时刻（不截断：调用方按需要夹到 0）。
func placeVideo(seams []seam) (starts []int, end int) {
	starts = make([]int, len(seams))
	for i, s := range seams {
		start := end
		if i > 0 {
			start -= s.overlap
		}
		starts[i] = start
		end = start + s.dur
	}
	return starts, end
}

// clipSeams 按切片顺序取画面轨各段的成片长度与重叠。
func clipSeams(clips []Clip) []seam {
	out := make([]seam, len(clips))
	for i, c := range clips {
		out[i] = seam{dur: c.PlayLen()}
		if i > 0 && c.TransitionIn.IsActive() {
			out[i].overlap = c.TransitionIn.DurationMs
		}
	}
	return out
}

// VideoPlacement 画面轨（按切片顺序）每段的成片起点与成片总长。
func VideoPlacement(clips []Clip) (starts []int, effectiveMs int) {
	starts, end := placeVideo(clipSeams(clips))
	if end < 0 {
		end = 0
	}
	return starts, end
}

// ---------------------------------------------------------------- 按起点放置的轨道

// PlaceByStart 按起点放置的轨道（音频轨）：按（起点，原 idx）排序、重编 idx 为 1..N、
// 时长按成片长度重算；**起点不动**。
//
// ⚠️ 不能走 Reflow：Reflow 把每段首尾相接，Agent 移动一次配乐，就被挤到上一段末尾（SCB-2）。
func PlaceByStart(clips []Clip) []Clip {
	out := make([]Clip, len(clips))
	copy(out, clips)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].StartMs != out[j].StartMs {
			return out[i].StartMs < out[j].StartMs
		}
		return out[i].Idx < out[j].Idx
	})
	for i := range out {
		out[i].Idx = i + 1
		out[i].DurationMs = out[i].PlayLen()
	}
	return out
}

// span 是按起点放置的一个条目：成片起止，以及它在调用方列表里的下标（报错时指回去）。
type span struct{ start, end, index int }

// firstOverlap 同一轨内第一处重叠里**后一个**条目的下标（按起点排序后看相邻两个；首尾相接不算重叠）。
// 没有重叠回 -1。排序按（起点，下标），所以同一份数据每次报同一个。
func firstOverlap(items []span) int {
	sorted := make([]span, len(items))
	copy(sorted, items)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].start != sorted[j].start {
			return sorted[i].start < sorted[j].start
		}
		return sorted[i].index < sorted[j].index
	})
	for i := 1; i < len(sorted); i++ {
		if sorted[i].start < sorted[i-1].end {
			return sorted[i].index
		}
	}
	return -1
}

// OverlapIndex 音频轨上第一处重叠里后一段在 clips 里的下标（SCD-15：音频轨与文字轨同轨不重叠）。
// 没有重叠回 -1。回下标不回 ID：刚加进来的那一段还没有 ID。
func OverlapIndex(clips []Clip) int {
	items := make([]span, len(clips))
	for i, c := range clips {
		items[i] = span{start: c.StartMs, end: c.StartMs + c.PlayLen(), index: i}
	}
	return firstOverlap(items)
}

// TrackEnd 按起点放置的轨道上最后一段的结束时刻（空轨为 0）：往这条轨上追加新段时默认放在这里，不与谁重叠。
func TrackEnd(clips []Clip) int {
	end := 0
	for _, c := range clips {
		if e := c.StartMs + c.PlayLen(); e > end {
			end = e
		}
	}
	return end
}

// ---------------------------------------------------------------- 跟随片段

// SpanPlace 字幕或文字层在成片里的起止。Hidden = 跟随的片段把它所在的区间裁掉了（隐藏，不删除）。
type SpanPlace struct {
	StartMs int  `json:"startMs"`
	EndMs   int  `json:"endMs"`
	Hidden  bool `json:"hidden,omitempty"`
}

// PlaceAnchored 跟随片段的条目在成片里的起止（SC-F-56）。
//
// 取素材区间与片段当前取用区间的交集，换算到成片：
// `start = clipStart + (max(srcStart, in) − in) × 100 / speed`，`end` 同理（整数除法向下取整）。
//
// 交集不到原区间的一半就隐藏：那一句话大半已经被裁掉了，把整句文字留在剩下的一小截上只会一闪而过。
// 隐藏的条目回 fallback（它上一次的成片时间，按原样留在原处），恢复区间后重新出现。
func PlaceAnchored(srcStart, srcEnd, clipStart int, c Clip, fallback SpanPlace) SpanPlace {
	speed := c.Speed
	if speed <= 0 {
		speed = DefaultSpeed
	}
	lo, hi := max(srcStart, c.SourceInMs), min(srcEnd, c.SourceOutMs)
	visible := hi - lo
	if visible <= 0 || visible*2 < srcEnd-srcStart {
		fallback.Hidden = true
		return fallback
	}
	return SpanPlace{
		StartMs: clipStart + (lo-c.SourceInMs)*100/speed,
		EndMs:   clipStart + (hi-c.SourceInMs)*100/speed,
	}
}

// FollowClip 跟随片段的音频（按画面生成的音效，SC-F-56）在成片里的落点与取用区间。
//
// 锚点记下的是生成时那一段素材 [srcStart, srcEnd]，音频从 0 起与它一一对应：
//   - 取它与片段当前取用区间的交集 [lo, hi]，换算到成片：`start = 片段成片起点 + (lo − in) × 100 / speed`；
//   - 音频取 [lo − srcStart, hi − srcStart]：裁掉片段开头时音效跟着跳过那一截，否则整段错位；
//   - 速度跟着片段：片段 2 倍速时音效也 2 倍速，否则越往后错得越多；
//   - 交集不足一个最短片段（MinClipMs）时隐藏（不删除），按存储值留在原处，恢复区间后重新出现。
//
// ⚠️ 与字幕的 PlaceAnchored 不同：字幕交集不到一半就隐藏（一整句文字留在一小截上只会一闪而过），
// 音效没有这个问题 —— 剩下多少放多少。
func FollowClip(srcStart, srcEnd, targetStart int, target, stored Clip) ClipPlace {
	speed := target.Speed
	if speed <= 0 {
		speed = DefaultSpeed
	}
	lo, hi := max(srcStart, target.SourceInMs), min(srcEnd, target.SourceOutMs)
	if hi-lo < MinClipMs {
		own := stored.Speed
		if own <= 0 {
			own = DefaultSpeed
		}
		return ClipPlace{StartMs: stored.StartMs, DurationMs: stored.PlayLen(),
			Follow: &FollowPlace{SourceInMs: stored.SourceInMs, SourceOutMs: stored.SourceOutMs, Speed: own, Hidden: true}}
	}
	f := Clip{SourceInMs: lo - srcStart, SourceOutMs: hi - srcStart, Speed: speed}
	return ClipPlace{StartMs: targetStart + (lo-target.SourceInMs)*100/speed, DurationMs: f.PlayLen(),
		Follow: &FollowPlace{SourceInMs: f.SourceInMs, SourceOutMs: f.SourceOutMs, Speed: speed}}
}

// ---------------------------------------------------------------- 整份内容

// ClipPlace 片段在成片里的起点与时长。
type ClipPlace struct {
	StartMs    int `json:"startMs"`
	DurationMs int `json:"durationMs"`
	// Follow 只在跟随片段的音频上有（SC-F-56）：按锚点算出的取用区间与速度，以及是否被裁到看不见了。
	Follow *FollowPlace `json:"follow,omitempty"`
}

// FollowPlace 跟随片段的音频算出来的取用区间与速度。Hidden = 跟随的那一段被裁掉了（不渲染、不播放）。
type FollowPlace struct {
	SourceInMs  int  `json:"sourceInMs"`
	SourceOutMs int  `json:"sourceOutMs"`
	Speed       int  `json:"speed"`
	Hidden      bool `json:"hidden,omitempty"`
}

// LayoutResult 整份内容里每个对象的成片时间（按 ID 索引）。
type LayoutResult struct {
	Clips     map[string]ClipPlace `json:"clips"`
	Subtitles map[string]SpanPlace `json:"subtitles"`
	Texts     map[string]SpanPlace `json:"texts"`
	// TotalMs 画面轨各段成片长度之和（不扣转场重叠）；EffectiveMs 成片总长（扣掉重叠）。
	TotalMs     int `json:"totalMs"`
	EffectiveMs int `json:"effectiveMs"`
}

// Layout 算出整份内容里每个片段、字幕、文字的成片时间。内容应当先过 ValidateContent。
//
// 跟随片段的字幕：所属片段不在这份内容里（被删了）时按存储的起止，不隐藏 ——
// 锚点失效，字幕按最后一次的成片时间留在原处（dev/02 §2）。
func Layout(c Content) LayoutResult {
	out := LayoutResult{
		Clips:     map[string]ClipPlace{},
		Subtitles: map[string]SpanPlace{},
		Texts:     map[string]SpanPlace{},
	}
	type placed struct {
		start int
		clip  Clip
	}
	byID := map[string]placed{}
	// videoIDs 画面轨的片段（音频只能跟随它们）；anchored 跟随片段的音频，画面轨都排好之后再算。
	videoIDs := map[string]bool{}
	type follower struct {
		id     string
		clip   Clip
		anchor json.RawMessage
	}
	var anchored []follower
	videoDone := false
	for _, t := range c.Tracks {
		if t.Kind == TrackVideo {
			clips := make([]Clip, len(t.Clips))
			for i, cc := range t.Clips {
				clips[i] = contentClip(cc)
			}
			starts, end := placeVideo(clipSeams(clips))
			for i, cc := range t.Clips {
				out.Clips[cc.ID] = ClipPlace{StartMs: starts[i], DurationMs: clips[i].PlayLen()}
				byID[cc.ID] = placed{starts[i], clips[i]}
				videoIDs[cc.ID] = true
			}
			if !videoDone {
				for _, cl := range clips {
					out.TotalMs += cl.PlayLen()
				}
				out.EffectiveMs = max(end, 0)
				videoDone = true
			}
			continue
		}
		for _, cc := range t.Clips {
			cl := contentClip(cc)
			out.Clips[cc.ID] = ClipPlace{StartMs: cl.StartMs, DurationMs: cl.PlayLen()}
			byID[cc.ID] = placed{cl.StartMs, cl}
			if t.Kind == TrackAudio && len(cc.Anchor) > 0 {
				anchored = append(anchored, follower{cc.ID, cl, cc.Anchor})
			}
		}
	}
	// 跟随片段的音频：锚点失效（坏值、所属片段不在这份内容里或不在画面轨上）时按存储值，不隐藏 ——
	// 同字幕：锚点失效，留在最后一次的位置。
	for _, f := range anchored {
		var a struct {
			ClipID     string `json:"clipId"`
			SrcStartMs int    `json:"srcStartMs"`
			SrcEndMs   int    `json:"srcEndMs"`
		}
		if json.Unmarshal(f.anchor, &a) != nil || a.SrcEndMs <= a.SrcStartMs || !videoIDs[a.ClipID] {
			continue
		}
		p := byID[a.ClipID]
		pl := FollowClip(a.SrcStartMs, a.SrcEndMs, p.start, p.clip, f.clip)
		out.Clips[f.id] = pl
		if !pl.Follow.Hidden {
			// 跟随这段音频的字幕（识别出来的）按它算出来的位置换算。
			byID[f.id] = placed{pl.StartMs, Clip{SourceInMs: pl.Follow.SourceInMs, SourceOutMs: pl.Follow.SourceOutMs,
				Speed: pl.Follow.Speed}}
		}
	}
	for _, s := range c.Subtitles {
		stored := SpanPlace{StartMs: s.StartMs, EndMs: s.EndMs}
		var a struct {
			ClipID     string `json:"clipId"`
			SrcStartMs int    `json:"srcStartMs"`
			SrcEndMs   int    `json:"srcEndMs"`
		}
		if len(s.Anchor) == 0 || json.Unmarshal(s.Anchor, &a) != nil || a.SrcEndMs <= a.SrcStartMs {
			out.Subtitles[s.ID] = stored
			continue
		}
		p, ok := byID[a.ClipID]
		if !ok {
			out.Subtitles[s.ID] = stored
			continue
		}
		out.Subtitles[s.ID] = PlaceAnchored(a.SrcStartMs, a.SrcEndMs, p.start, p.clip, stored)
	}
	for _, t := range c.Texts {
		out.Texts[t.ID] = SpanPlace{StartMs: t.StartMs, EndMs: t.EndMs}
	}
	return out
}

// contentClip 整份内容里的片段换成领域片段（只取算落点要用的字段）。
func contentClip(cc ContentClip) Clip {
	c := Clip{
		SourceInMs: cc.SourceInMs, SourceOutMs: cc.SourceOutMs,
		StartMs: intOr(cc.StartMs, 0), Speed: intOr(cc.Speed, DefaultSpeed),
	}
	if cc.TransitionIn != nil {
		c.TransitionIn = *cc.TransitionIn
	}
	return c
}
