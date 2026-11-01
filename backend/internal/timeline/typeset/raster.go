package typeset

import (
	"image"
	"image/color"
	"image/draw"
	"math"

	"github.com/go-text/typesetting/font"
	ot "github.com/go-text/typesetting/font/opentype"
	"golang.org/x/image/vector"
)

// placed 是摆好位置的一个字形：(x, y) 是基线原点在画布里的坐标。
type placed struct {
	face *font.Face
	gid  font.GID
	x, y float64
	px   float64 // 字号（像素）
}

// glyphMask 把全部字形画进一张覆盖度图（0~255）。
//
// ⚠️ 逐个字形光栅化、按 Over 叠进同一张图，不把所有轮廓塞进一个光栅器：思源黑体（CFF）与
// Noto Emoji（TrueType）的轮廓绕向相反，两个字形挨着、边缘重叠时，放进同一个光栅器的话
// 相反的绕向会互相抵消，在交界处挖出一道细缝。逐个画也让光栅器的缓冲只有一个字那么大。
func glyphMask(glyphs []placed, w, h int) *image.Alpha {
	mask := image.NewAlpha(image.Rect(0, 0, w, h))
	var z vector.Rasterizer
	for _, g := range glyphs {
		out, ok := g.face.GlyphDataOutline(g.gid)
		if !ok || len(out.Segments) == 0 {
			continue // 空格这类没有轮廓的字形
		}
		s := g.px / float64(g.face.Upem())
		// 控制点的范围包住整条曲线，拿它当这个字的包围盒。
		minX, minY := math.Inf(1), math.Inf(1)
		maxX, maxY := math.Inf(-1), math.Inf(-1)
		for i := range out.Segments {
			for _, pt := range out.Segments[i].ArgsSlice() {
				x, y := g.x+float64(pt.X)*s, g.y-float64(pt.Y)*s
				minX, maxX = math.Min(minX, x), math.Max(maxX, x)
				minY, maxY = math.Min(minY, y), math.Max(maxY, y)
			}
		}
		r := image.Rect(int(math.Floor(minX))-1, int(math.Floor(minY))-1,
			int(math.Ceil(maxX))+1, int(math.Ceil(maxY))+1).Intersect(mask.Rect)
		if r.Empty() {
			continue // 整个字都在画面外（被画面边缘裁掉的部分）
		}
		z.Reset(r.Dx(), r.Dy())
		z.DrawOp = draw.Over // Reset 会把它改回 Over，这里写明是有意的
		addOutline(&z, out, float32(g.x-float64(r.Min.X)), float32(g.y-float64(r.Min.Y)), float32(s))
		z.Draw(mask, r, image.Opaque, image.Point{})
	}
	return mask
}

// addOutline 把一个字形的轮廓加进光栅器。字体坐标 y 朝上，画布 y 朝下。
func addOutline(z *vector.Rasterizer, o font.GlyphOutline, ox, oy, s float32) {
	open := false
	for _, seg := range o.Segments {
		a := seg.Args
		switch seg.Op {
		case ot.SegmentOpMoveTo:
			if open {
				z.ClosePath()
			}
			z.MoveTo(ox+a[0].X*s, oy-a[0].Y*s)
			open = true
		case ot.SegmentOpLineTo:
			z.LineTo(ox+a[0].X*s, oy-a[0].Y*s)
		case ot.SegmentOpQuadTo:
			z.QuadTo(ox+a[0].X*s, oy-a[0].Y*s, ox+a[1].X*s, oy-a[1].Y*s)
		case ot.SegmentOpCubeTo:
			z.CubeTo(ox+a[0].X*s, oy-a[0].Y*s, ox+a[1].X*s, oy-a[1].Y*s, ox+a[2].X*s, oy-a[2].Y*s)
		}
	}
	if open {
		z.ClosePath() // 光栅器不会自己闭合子路径；不闭合的话那一圈的覆盖度算不对
	}
}

// roundRect 是底框：圆角矩形，坐标是画布坐标。覆盖度按像素中心解析地算，不走光栅器 ——
// 底框常常铺满大半个画面，为它开一个整画布大小的光栅缓冲（每像素 4 字节）不值。
type roundRect struct {
	x0, y0, x1, y1, r float64
}

// coverage 是像素 (x, y) 被底框盖住的比例（边缘与圆角各有一个像素的抗锯齿过渡）。
func (b roundRect) coverage(x, y int) float64 {
	cx, cy := float64(x)+0.5, float64(y)+0.5
	// 圆角区：到圆心的距离决定覆盖度。
	ccx, ccy := cx, cy
	switch {
	case cx < b.x0+b.r:
		ccx = b.x0 + b.r
	case cx > b.x1-b.r:
		ccx = b.x1 - b.r
	}
	switch {
	case cy < b.y0+b.r:
		ccy = b.y0 + b.r
	case cy > b.y1-b.r:
		ccy = b.y1 - b.r
	}
	if b.r > 0 && ccx != cx && ccy != cy {
		return clamp01(b.r + 0.5 - math.Hypot(cx-ccx, cy-ccy))
	}
	return clamp01(min(cx-b.x0, b.x1-cx)+0.5) * clamp01(min(cy-b.y0, b.y1-cy)+0.5)
}

// strokeCoverage 算描边的覆盖度：字形向外扩 radius 像素，边缘抗锯齿。
//
// 用精确的欧氏距离变换（Felzenszwalb–Huttenlocher，O(像素数)），不用「拿圆盘逐像素膨胀」：
// 后者是 O(像素数 × 半径²)，标题字号下描边半径几十像素时，一张图要在 API 进程里算好几秒。
func strokeCoverage(fill *image.Alpha, radius float64) []float32 {
	w, h := fill.Rect.Dx(), fill.Rect.Dy()
	g := make([]float32, w*h) // 先存到最近「字内」像素的距离的平方，最后原地换成覆盖度
	for y := 0; y < h; y++ {
		row := fill.Pix[y*fill.Stride : y*fill.Stride+w]
		for x, a := range row {
			if a < 128 {
				g[y*w+x] = edtInf
			}
		}
	}
	n := max(w, h)
	f := make([]float64, n)
	d := make([]float64, n)
	v := make([]int, n)
	z := make([]float64, n+1)
	for x := 0; x < w; x++ { // 先按列
		for y := 0; y < h; y++ {
			f[y] = float64(g[y*w+x])
		}
		distance1D(f[:h], d[:h], v, z)
		for y := 0; y < h; y++ {
			g[y*w+x] = float32(d[y])
		}
	}
	for y := 0; y < h; y++ { // 再按行
		row := g[y*w : (y+1)*w]
		for x := range row {
			f[x] = float64(row[x])
		}
		distance1D(f[:w], d[:w], v, z)
		for x := range row {
			// 距离算的是像素中心到最近的字内像素中心；字的边缘大约在字内像素外半个像素处，
			// 所以描边外沿落在 radius + 0.5，再给半个像素的抗锯齿过渡。
			row[x] = float32(clamp01(radius + 1 - math.Sqrt(d[x])))
		}
	}
	return g
}

const edtInf = 1e20

// distance1D 是一维的平方距离变换：d[q] = min_p (q-p)² + f[p]（抛物线下包络）。
func distance1D(f, d []float64, v []int, z []float64) {
	k := 0
	v[0] = 0
	z[0], z[1] = -edtInf, edtInf
	for q := 1; q < len(f); q++ {
		s := parabolaCross(f, q, v[k])
		for s <= z[k] {
			k--
			s = parabolaCross(f, q, v[k])
		}
		k++
		v[k] = q
		z[k] = s
		z[k+1] = edtInf
	}
	k = 0
	for q := range f {
		for z[k+1] < float64(q) {
			k++
		}
		dq := float64(q - v[k])
		d[q] = dq*dq + f[v[k]]
	}
}

func parabolaCross(f []float64, q, p int) float64 {
	return ((f[q] + float64(q*q)) - (f[p] + float64(p*p))) / float64(2*(q-p))
}

// compose 把三层叠起来（底框 → 描边 → 字），出一张非预乘的 RGBA（PNG 存的就是这种）。
func compose(w, h int, fill *image.Alpha, stroke []float32, bg *roundRect, st Style) *image.NRGBA {
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	fillC := unit(st.Color, 1)
	var strokeC, bgC [4]float64
	if st.Stroke != nil {
		strokeC = unit(st.Stroke.Color, 1)
	}
	if st.Background != nil {
		bgC = unit(st.Background.Color, st.Background.Opacity)
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var acc [4]float64 // 预乘的 r, g, b, a
			if bg != nil {
				acc = over(acc, bgC, bg.coverage(x, y))
			}
			if stroke != nil {
				acc = over(acc, strokeC, float64(stroke[y*w+x]))
			}
			acc = over(acc, fillC, float64(fill.Pix[y*fill.Stride+x])/255)
			if acc[3] <= 0 {
				continue
			}
			o := y*out.Stride + x*4
			out.Pix[o+0] = to8(acc[0] / acc[3])
			out.Pix[o+1] = to8(acc[1] / acc[3])
			out.Pix[o+2] = to8(acc[2] / acc[3])
			out.Pix[o+3] = to8(acc[3])
		}
	}
	return out
}

// unit 把颜色换成 0~1 的 r, g, b, a（a 乘上额外的不透明度）。
func unit(c color.NRGBA, opacity float64) [4]float64 {
	return [4]float64{float64(c.R) / 255, float64(c.G) / 255, float64(c.B) / 255, float64(c.A) / 255 * opacity}
}

// over 把颜色 c（非预乘）以覆盖度 cov 叠在 dst（预乘）上面。
func over(dst, c [4]float64, cov float64) [4]float64 {
	a := c[3] * cov
	if a <= 0 {
		return dst
	}
	k := 1 - a
	return [4]float64{c[0]*a + dst[0]*k, c[1]*a + dst[1]*k, c[2]*a + dst[2]*k, a + dst[3]*k}
}

func to8(v float64) uint8 { return uint8(math.Round(clamp01(v) * 255)) }

func clamp01(v float64) float64 {
	// 不用 math.Min / Max：它们要处理 NaN 与正负零，逐像素调用时占了合成耗时的一半。
	if v <= 0 {
		return 0
	}
	if v >= 1 {
		return 1
	}
	return v
}

// opaqueBounds 是图里不透明像素的包围框；全透明时为空。
func opaqueBounds(img *image.NRGBA) image.Rectangle {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	minX, minY, maxX, maxY := w, h, -1, -1
	for y := 0; y < h; y++ {
		row := img.Pix[y*img.Stride : y*img.Stride+w*4]
		for x := 0; x < w; x++ {
			if row[x*4+3] == 0 {
				continue
			}
			minX, maxX = min(minX, x), max(maxX, x)
			minY, maxY = min(minY, y), max(maxY, y)
		}
	}
	if maxX < 0 {
		return image.Rectangle{}
	}
	return image.Rect(minX, minY, maxX+1, maxY+1)
}
