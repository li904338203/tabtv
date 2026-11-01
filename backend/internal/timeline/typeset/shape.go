package typeset

import (
	"unicode"

	"github.com/go-text/typesetting/di"
	"github.com/go-text/typesetting/font"
	"github.com/go-text/typesetting/language"
	"github.com/go-text/typesetting/shaping"
	"golang.org/x/image/math/fixed"
)

// refPx 是整形用的参考字号：整段只在这个字号下整形一次，量宽、画字时再按比例换算到实际字号。
//
// ⚠️ 不能直接按实际字号整形：go-text 的整形器把字号**向上取整到整像素**（`Size.Ceil()`），
// 而轮廓是按小数字号缩放的 —— 48.6px 的字幕会按 49px 的步进排、按 48.6px 的大小画，
// 字距凭空大一截，量出来的行宽也对不上。按比例换算还有一个好处：同样的画幅比例下，
// 断行与分辨率无关 —— 360p 的预览与 1080p 的成片断在同一个字上（SC-N-06）。
// 取 1000：思源黑体的 em 是 1000 单位，汉字的步进在参考字号下正好是整数。
const refPx = 1000

// shapedGlyph 是整形后的一个字形，位置是参考字号下的像素。
type shapedGlyph struct {
	gid     font.GID
	face    *font.Face
	cluster int     // 这个字形属于第几个字符（整段文字里的下标）
	adv     float64 // 前进量
	xoff    float64
	yoff    float64
}

// paragraph 是一段文字整形之后的结果。
//
// ⚠️ 量宽与画字用的是**同一份**整形结果：换行时按 runeAdv 累加量出来的宽度，
// 就是画出来的宽度。各量各画的话，量的时候以为放得下、画出来却超出底框。
type paragraph struct {
	text    []rune
	glyphs  []shapedGlyph
	runeAdv []float64 // 第 i 个字符分到的前进量（参考字号下；一个字形簇的量记在它的第一个字符上）
	missing []string  // 字体里都没有、画成替代字形的字符（去重，按出现顺序）
	k       float64   // 实际字号 / 参考字号
}

// at 换算到实际字号（只改比例，整形结果共用）。
func (p paragraph) at(px float64) paragraph {
	p.k = px / refPx
	return p
}

// shapeParagraph 按字体回退把文字切成若干段（同一段用同一个字体），逐段整形。
func shapeParagraph(fs *faces, text []rune) paragraph {
	p := paragraph{text: text, runeAdv: make([]float64, len(text)), k: 1}
	if len(text) == 0 {
		return p
	}
	seen := map[rune]bool{}
	faces := make([]*font.Face, len(text))
	for i, r := range text {
		switch {
		case isJoiner(r) && i > 0:
			faces[i] = faces[i-1]
		case unicode.IsControl(r) || unicode.IsSpace(r):
			faces[i] = fs.regular
		default:
			face, miss := fs.faceFor(r)
			faces[i] = face
			if miss && !seen[r] {
				seen[r] = true
				p.missing = append(p.missing, string(r))
			}
		}
	}

	scripts := resolveScripts(text)
	for start := 0; start < len(text); {
		end := start + 1
		for end < len(text) && faces[end] == faces[start] && scripts[end] == scripts[start] {
			end++
		}
		out := fs.shaper.Shape(shaping.Input{
			Text:      text,
			RunStart:  start,
			RunEnd:    end,
			Direction: di.DirectionLTR,
			Face:      faces[start],
			Size:      fixed.I(refPx),
			Script:    scripts[start],
			Language:  langZh,
		})
		for _, g := range out.Glyphs {
			idx := g.ClusterIndex
			if idx < start || idx >= end {
				idx = start
			}
			adv := f26(g.Advance)
			// 换行符、制表符这类控制字符不占宽度（字体里可能给它们一个方框的宽）。
			if unicode.IsControl(text[idx]) {
				adv = 0
			}
			p.glyphs = append(p.glyphs, shapedGlyph{
				gid: g.GlyphID, face: faces[start], cluster: idx,
				adv: adv, xoff: f26(g.XOffset), yoff: f26(g.YOffset),
			})
			p.runeAdv[idx] += adv
		}
		start = end
	}
	return p
}

// width 量 [a, b) 这些字符在实际字号下的宽度。
func (p paragraph) width(a, b int) float64 {
	var w float64
	for i := a; i < b; i++ {
		w += p.runeAdv[i]
	}
	return w * p.k
}

var langZh = language.NewLanguage("zh")

// resolveScripts 给每个字符定书写系统：标点、空格、数字这类通用字符跟着前一个字符走
// （开头的跟着后面第一个有归属的字符）。整形按「同一字体 + 同一书写系统」分段：
// 思源黑体同时覆盖汉字与拉丁字母，不按书写系统分开的话，英文按汉字的规则整形，
// 字体里登记在拉丁文下的字偶距调整就用不上。
func resolveScripts(text []rune) []language.Script {
	out := make([]language.Script, len(text))
	known := make([]bool, len(text))
	first := -1
	for i, r := range text {
		s := language.LookupScript(r)
		if s == language.Common || s == language.Inherited || s == language.Unknown {
			continue
		}
		out[i], known[i] = s, true
		if first < 0 {
			first = i
		}
	}
	if first < 0 {
		for i := range out {
			out[i] = language.Latin
		}
		return out
	}
	last := out[first]
	for i := range out {
		if known[i] {
			last = out[i]
		} else {
			out[i] = last
		}
	}
	return out
}

func f26(v fixed.Int26_6) float64 { return float64(v) / 64 }
