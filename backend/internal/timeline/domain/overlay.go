package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	"sort"
	"strings"
)

// 字幕与文字叠加（docs/agent/smartcut SCT-14，SCD-27 / SCD-28）。
//
// 成片里的字幕与文字层合成**一路透明叠加流**，只 overlay 一次：
//  1. 把成片时间切成「可见内容不变」的区间（OverlaySegments）；
//  2. runner 逐个区间调排版（timeline/typeset），把排好的条目贴到一张画面大小的透明图上（ComposeOverlay）；
//  3. 用 concat 分离器按时长把这些图拼成一路流（OverlayConcatScript），在水印之前 `overlay=0:0`。
//
// ⚠️ 不用 `subtitles` / `drawtext`：本机 ffmpeg 两个都没有（没有 libass），而且那样预览（排版接口出的 PNG）
// 与成片是两套排版，字号、换行、避头尾迟早对不上。这里只做纯计算：不碰文件、不跑进程、不 import typeset。

// 叠加条目的种类。
const (
	OverlaySubtitle = "subtitle"
	OverlayText     = "text"
)

// OverlayItem 是一个区间里可见的一条字幕或一个文字层。
//
// Style：字幕是「轨道样式 ⊕ 条目样式」合并后的结果（MergeStyle）；文字层原样。Box 只有文字层有。
type OverlayItem struct {
	Kind       string
	TrackOrder int
	Text       string
	Style      json.RawMessage
	Box        json.RawMessage
}

// OverlaySegment 是成片里的一段 [StartMs, EndMs)：这一段里可见的条目不变。Items 为空 = 这一段没有字
// （照样要有一张全透明图占住这段时间，concat 分离器才能按时长把后面的图对齐）。
type OverlaySegment struct {
	StartMs int
	EndMs   int
	Items   []OverlayItem
}

// overlayEntry 是排好序的一个可见条目：成片起止（已夹回）与排序键。
type overlayEntry struct {
	item       OverlayItem
	start, end int
	// 排序键：字幕在前、文字层在后；字幕按轨道 Order、同 Order 按轨道在快照里的位置、同轨按起点；
	// 文字层按轨道序号、起点；最后按出现顺序（保证同一份快照每次切出同一个结果）。
	rank, order, trackIdx, seq int
}

// OverlaySegments 把 [0, totalMs) 切成可见内容不变的区间，连续铺满整段（没有字的区间也在）。
//
// 规则：
//   - 条目只在自己的 [起点, 终点) 里可见；越界的夹到 [0, totalMs)，夹完为空的（含整段落在外面的）、
//     起止倒置的、文字全是空白的都不出现；
//   - 区间内条目的顺序固定：字幕按轨道 Order 升序（第一条在最下方）、同轨按起点，再到文字层（按轨道序号、起点）；
//   - 相邻两个区间的内容完全相同（同样的条目、同样的文字与样式）时合并 —— 识别常把一句话切成两条同文的字幕，
//     画面上什么都没变，多切一刀只是多一张图、多一行 concat。
//
// totalMs <= 0 时没有区间。
func OverlaySegments(snap RenderSnapshot, totalMs int) []OverlaySegment {
	if totalMs <= 0 {
		return nil
	}
	var entries []overlayEntry
	add := func(e overlayEntry) {
		e.start, e.end = max(e.start, 0), min(e.end, totalMs)
		if e.end <= e.start || strings.TrimSpace(e.item.Text) == "" {
			return
		}
		e.seq = len(entries)
		entries = append(entries, e)
	}
	for ti, tr := range snap.SubtitleTracks {
		for _, it := range tr.Items {
			add(overlayEntry{
				item: OverlayItem{Kind: OverlaySubtitle, TrackOrder: tr.Order, Text: it.Text,
					Style: MergeStyle(tr.Style, it.Style)},
				start: it.StartMs, end: it.EndMs, rank: 0, order: tr.Order, trackIdx: ti,
			})
		}
	}
	for _, tx := range snap.Texts {
		add(overlayEntry{
			item: OverlayItem{Kind: OverlayText, TrackOrder: tx.TrackOrder, Text: tx.Text,
				Style: cloneRaw(tx.Style), Box: cloneRaw(tx.Box)},
			start: tx.StartMs, end: tx.EndMs, rank: 1, order: tx.TrackOrder,
		})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		switch {
		case a.rank != b.rank:
			return a.rank < b.rank
		case a.order != b.order:
			return a.order < b.order
		case a.trackIdx != b.trackIdx:
			return a.trackIdx < b.trackIdx
		case a.start != b.start:
			return a.start < b.start
		}
		return a.seq < b.seq
	})

	// 切点：0、totalMs 与每个条目的起止。每个相邻切点之间可见的条目不变。
	cuts := []int{0, totalMs}
	for _, e := range entries {
		cuts = append(cuts, e.start, e.end)
	}
	sort.Ints(cuts)
	bounds := make([]int, 0, len(cuts))
	for _, c := range cuts {
		if len(bounds) == 0 || c != bounds[len(bounds)-1] {
			bounds = append(bounds, c)
		}
	}
	// 按排好的顺序把每个条目挂到它覆盖的每个小区间上：挂上去的顺序就是区间内的顺序。
	members := make([][]int, len(bounds)-1)
	for k, e := range entries {
		from := sort.SearchInts(bounds, e.start)
		to := sort.SearchInts(bounds, e.end)
		for i := from; i < to; i++ {
			members[i] = append(members[i], k)
		}
	}

	var out []OverlaySegment
	for i := 0; i+1 < len(bounds); i++ {
		items := make([]OverlayItem, 0, len(members[i]))
		for _, k := range members[i] {
			items = append(items, entries[k].item)
		}
		if n := len(out); n > 0 && sameOverlayItems(out[n-1].Items, items) {
			out[n-1].EndMs = bounds[i+1]
			continue
		}
		if len(items) == 0 {
			items = nil
		}
		out = append(out, OverlaySegment{StartMs: bounds[i], EndMs: bounds[i+1], Items: items})
	}
	return out
}

// sameOverlayItems 两个区间画出来是不是同一张图（逐条比种类、轨道、文字、样式与位置框的字节）。
func sameOverlayItems(a, b []OverlayItem) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.Kind != y.Kind || x.TrackOrder != y.TrackOrder || x.Text != y.Text ||
			!bytes.Equal(x.Style, y.Style) || !bytes.Equal(x.Box, y.Box) {
			return false
		}
	}
	return true
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

// MergeStyle 把几层样式按顺序合并：后面的覆盖前面的；两边都是对象的键递归合并（描边、底框这类嵌套样式
// 只改一项时，其余几项照旧）。字幕条目的样式 = MergeStyle(轨道样式, 条目样式)。
//
// 约定（与整份内容的校验一致）：
//   - `null` 与缺省同义：后一层写 null 不会抹掉前一层的值；
//   - 空的、坏的、不是对象的层整层跳过（一条字幕的样式坏了，不该让整条时间线导不出来）；
//   - 一层对象都没有时回 nil。
//
// 输出键按字典序、数字保持原文（不经 float64 舍入）：同样的输入永远得到同样的字节，排版缓存键才稳定。
// 不改动传进来的层。
func MergeStyle(layers ...json.RawMessage) json.RawMessage {
	var merged map[string]any
	for _, raw := range layers {
		m := decodeObject(raw)
		if m == nil {
			continue
		}
		if merged == nil {
			merged = map[string]any{}
		}
		mergeInto(merged, m)
	}
	if merged == nil {
		return nil
	}
	out, err := json.Marshal(merged)
	if err != nil {
		return nil
	}
	return out
}

// decodeObject 解析成对象（数字保留原文）；不是对象、解析不了的回 nil。
func decodeObject(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil || dec.More() {
		return nil
	}
	return m
}

// mergeInto 把 src 合并进 dst：两边都是对象时递归，src 的 null 跳过，其余以 src 为准。
func mergeInto(dst, src map[string]any) {
	for k, v := range src {
		if v == nil {
			continue
		}
		if sv, ok := v.(map[string]any); ok {
			if dv, ok := dst[k].(map[string]any); ok {
				cp := make(map[string]any, len(dv))
				for dk, dvv := range dv {
					cp[dk] = dvv
				}
				mergeInto(cp, sv)
				dst[k] = cp
				continue
			}
			cp := map[string]any{}
			mergeInto(cp, sv)
			dst[k] = cp
			continue
		}
		dst[k] = v
	}
}

// StackBottomPct 多条字幕轨同时有字时，每条轨的下边距（画面高度的百分比）：第一条在 basePct，
// 后面每条往上让出前面各条实际排出来的高度再加一个间距。
//
// heightsPct[i] 是第 i 条（按轨道 Order）在这个区间排出来的高度（排版结果的 BoxPct.H）；
// 这一段没字的轨给 0 —— 它不占位置也不占间距，上面的轨照常落下来，不会凭空空出一行。
// 负数按 0。
func StackBottomPct(basePct float64, heightsPct []float64, gapPct float64) []float64 {
	out := make([]float64, len(heightsPct))
	at := basePct
	for i, h := range heightsPct {
		out[i] = at
		if h > 0 {
			at += h + gapPct
		}
	}
	return out
}

// PlacedImage 是一张排好的图与它左上角在画面里的位置（像素，可以是负数或超出画面）。
type PlacedImage struct {
	Img  image.Image
	X, Y int
}

// ComposeOverlay 画一张 w×h 的全透明图，按顺序把每张贴到 (X, Y)：后贴的压在上面（Over 合成），
// 超出画面的部分裁掉。没有图时就是一张全透明图（没有字的区间也要它占住时间）。
//
// ⚠️ 透明处必须保持 alpha=0：overlay 按 alpha 透出底下的画面，画成不透明的黑会把整片盖住。
// 输出非预乘的 NRGBA，png.Encode 直接写、不再转换。
func ComposeOverlay(w, h int, items []PlacedImage) *image.NRGBA {
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	for _, it := range items {
		if it.Img == nil {
			continue
		}
		b := it.Img.Bounds()
		r := image.Rect(it.X, it.Y, it.X+b.Dx(), it.Y+b.Dy()).Intersect(dst.Bounds())
		if r.Empty() {
			continue
		}
		draw.Draw(dst, r, it.Img, b.Min.Add(r.Min.Sub(image.Pt(it.X, it.Y))), draw.Over)
	}
	return dst
}

// OverlayConcatScript 写 concat 分离器的脚本：每个文件一行 file、一行 duration（秒，三位小数），
// 末尾把最后一个文件再写一次。files 与 durationsMs 一一对应，数量不一致时只配到较短的一边；负时长按 0。
//
// ⚠️ 末尾那行重复的 file 是给老版本 ffmpeg 的保险：早年的 concat 分离器只在最后一个文件后面还有文件时
// 才认它的 duration，不写的话最后一段（片尾那句字幕）会被吞掉。本机 8.1.2 去掉这一行也不丢（测试里试过），
// 但部署镜像里的 ffmpeg 版本未必相同，多写一行没有代价。
// ⚠️ 文件名一律加单引号；名字里的单引号要先关掉引号、写一个反斜杠转义的单引号、再重新开引号
// （ffconcat 的转义规则，见 concatQuote）。
// 调用方传 `-f concat -safe 0`：临时目录下的文件名不一定满足 safe 模式的字符要求。
func OverlayConcatScript(files []string, durationsMs []int) string {
	var b strings.Builder
	b.WriteString("ffconcat version 1.0\n")
	n := min(len(files), len(durationsMs))
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "file %s\nduration %s\n", concatQuote(files[i]), overlayDur(durationsMs[i]))
	}
	if n > 0 {
		fmt.Fprintf(&b, "file %s\n", concatQuote(files[n-1]))
	}
	return b.String()
}

func concatQuote(name string) string {
	return "'" + strings.ReplaceAll(name, "'", `'\''`) + "'"
}

func overlayDur(ms int) string {
	if ms < 0 {
		ms = 0
	}
	return fmt.Sprintf("%.3f", float64(ms)/1000)
}
