package typeset

import (
	"math"
	"strings"
	"unicode"

	"github.com/go-text/typesetting/segmenter"
)

// 避头尾（SCT-13、SC-F-53）。
//
// 行首不放：句读、右括号、右引号、省略号、长音、小写假名……
// 行尾不放：左括号、左引号。
// UAX #14 已经挡住了大部分情形（它不在句读、右括号前给断点，也不在左括号后给断点），
// 这里补三件事：
//  1. 句号、逗号这类**挂在行尾**：前面的字放得下、只差末尾的标点时，标点挂在本行，不把字挤下去；
//  2. 一个片段宽过整行、只能逐字切的时候，切出来的小段也要守避头尾；
//  3. UAX #14 允许、中文排版不允许的个别组合（如汉字后的 `%`）。
const (
	noStart = "，。、；：？！）」』》〉】〕｝’”…‥・ー々〜ゝゞヽヾぁぃぅぇぉっゃゅょゎァィゥェォッャュョヮヵヶ" +
		",.:;?!)]}%"
	noEnd = "（「『《〈【〔｛‘“([{"
)

func isNoStart(r rune) bool { return strings.ContainsRune(noStart, r) }
func isNoEnd(r rune) bool   { return strings.ContainsRune(noEnd, r) }

// span 是一行在整段文字里的下标区间 [start, end)。
type span struct{ start, end int }

// atom 是排行时不再拆开的一小段：UAX #14 两个断点之间的文字，或者超宽片段逐字切开后的一个字素。
type atom struct {
	start, end int
	mandatory  bool // 末尾是必须换行的断点（换行符）
}

// wrapLines 按最大宽度断行：先切成 atom、按避头尾并起来，再贪心地一行一行装。
//
// maxW 是一行的宽度上限；hardW 是挂在行尾的标点也不能越过的线（画面宽度减去底框内边距）——
// 标点只能挂进行宽之外的留白里，挂出画面就会被裁掉。
func wrapLines(p paragraph, maxW, hardW float64) []span {
	if len(p.text) == 0 {
		return nil
	}
	atoms := joinKinsoku(p, splitAtoms(p, maxW), maxW)

	var lines []span
	cur := span{atoms[0].start, atoms[0].start}
	for _, a := range atoms {
		switch {
		case cur.end == cur.start:
			cur.end = a.end // 行首：再宽也只能放（逐字切过之后，只剩一个字就宽过整行这一种情况）
		case fits(visibleWidth(p, cur.start, a.end), maxW) || hangs(p, cur.start, a, maxW, hardW):
			cur.end = a.end
		default:
			lines = append(lines, cur)
			cur = span{a.start, a.end}
		}
		if a.mandatory {
			lines = append(lines, cur)
			cur = span{a.end, a.end}
		}
	}
	if cur.end > cur.start {
		lines = append(lines, cur)
	}
	// 去掉只有空白的行：两个连着的换行符、或者结尾的换行符，都会留下一行空的。
	out := lines[:0]
	for _, l := range lines {
		if trimEnd(p.text, l.start, l.end) > l.start {
			out = append(out, l)
		}
	}
	return out
}

// splitAtoms 按 UAX #14 的断点切开；宽过一整行的片段（很长的英文单词、没有断点的长串）
// 再按字素切开 —— 否则它会原样伸出行外。
func splitAtoms(p paragraph, maxW float64) []atom {
	var atoms []atom
	var seg segmenter.Segmenter
	seg.Init(p.text)
	it := seg.LineIterator()
	for it.Next() {
		l := it.Line()
		s, e := l.Offset, l.Offset+len(l.Text)
		if fits(visibleWidth(p, s, e), maxW) {
			atoms = append(atoms, atom{s, e, l.IsMandatoryBreak})
			continue
		}
		var gs segmenter.Segmenter
		gs.Init(p.text[s:e])
		gi := gs.GraphemeIterator()
		n := len(atoms)
		for gi.Next() {
			g := gi.Grapheme()
			atoms = append(atoms, atom{start: s + g.Offset, end: s + g.Offset + len(g.Text)})
		}
		if len(atoms) == n {
			atoms = append(atoms, atom{start: s, end: e})
		}
		atoms[len(atoms)-1].mandatory = l.IsMandatoryBreak
	}
	return atoms
}

// joinKinsoku 把会落到行首的「行首不放」与会留在行尾的「行尾不放」并进相邻的 atom。
// 并起来比一行还宽的不并：一长串标点只能照常折行，否则那一行会宽出画面。
func joinKinsoku(p paragraph, atoms []atom, maxW float64) []atom {
	out := make([]atom, 0, len(atoms))
	for _, a := range atoms {
		if n := len(out); n > 0 && !out[n-1].mandatory {
			prev := out[n-1]
			pe := trimEnd(p.text, prev.start, prev.end)
			bad := isNoStart(p.text[a.start]) || (pe > prev.start && isNoEnd(p.text[pe-1]))
			if bad && fits(visibleWidth(p, prev.start, a.end), maxW) {
				out[n-1] = atom{start: prev.start, end: a.end, mandatory: a.mandatory}
				continue
			}
		}
		out = append(out, a)
	}
	return out
}

// hangs：从 lineStart 排到 a.end 放不下，但去掉 a 末尾那几个「行首不放」的标点就放得下 ——
// 标点挂在本行行尾（SCT-13：句号落在行首时挪到上一行）。整个 atom 都是这类标点时也挂上来。
func hangs(p paragraph, lineStart int, a atom, maxW, hardW float64) bool {
	k := trimEnd(p.text, a.start, a.end)
	j := k
	for j > a.start && isNoStart(p.text[j-1]) {
		j--
	}
	if j == k || !fits(p.width(lineStart, k), hardW) {
		return false // 末尾不是这类标点，或者挂上去就出画面了
	}
	return j == a.start || fits(visibleWidth(p, lineStart, j), maxW)
}

// fits 比宽度时留一点浮点余量：宽度按「参考字号下的步进 × 比例」算，行宽按「画面 × 百分比」算，
// 正好相等的情形（16 个字正好排满 16 个字宽）会因为末位舍入一会儿放得下、一会儿放不下 ——
// 同一段字幕在 360p 预览与 1080p 成片里断在不同的字上。
func fits(w, limit float64) bool { return w <= limit+math.Abs(limit)*1e-9 }

// visibleWidth 是 [a, b) 去掉末尾空白之后的宽度：行尾的空格不占行宽。
func visibleWidth(p paragraph, a, b int) float64 { return p.width(a, trimEnd(p.text, a, b)) }

// trimEnd 返回去掉 [a, b) 末尾空白、换行符之后的结束位置。
func trimEnd(text []rune, a, b int) int {
	for b > a && (unicode.IsSpace(text[b-1]) || unicode.IsControl(text[b-1])) {
		b--
	}
	return b
}
