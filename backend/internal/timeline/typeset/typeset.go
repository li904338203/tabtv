// Package typeset 把字幕与文字排版并光栅化成透明 PNG（SCT-13、SCD-28）。
//
// 预览（`POST /smartcut/typeset`）与成片叠加（SCT-14）调的是**同一个**函数 [Layout]：
// 本机 ffmpeg 既没有 libass 也没有 drawtext，与其让预览用浏览器排、成片用另一套排，
// 不如两处都用这里出的同一张图 —— 那样「预览里一行、成片里两行」就不可能发生（SC-N-06）。
//
// 流程：按字体回退整形（思源黑体 → Noto Emoji → 替代字形）→ UAX #14 断行 + 避头尾 →
// 超过 3 行先缩字号 → 光栅化（字、描边、底框）→ 裁到非透明区域 → PNG。
package typeset

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/png"
	"math"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// 入参上限（dev/01 §8：光栅化在 API 进程里同步执行，长度与尺寸不设上限的话，
// 这个接口能被拿来生成大图、打满 CPU）。宽高与契约 TypesetRequest 一致（64~3840）。
const (
	MaxTextRunes = 500 // 与 timeline/domain.MaxTextChars 一致（测试钉住）
	MinFrameSize = 64
	MaxFrameSize = 3840
)

// layoutVersion 计入缓存键：排版算法改了，旧的缓存（以及 Worker 写在临时目录里的图）就不能再用。
const layoutVersion = "1"

// ErrFontsUnavailable 表示字体没装好（`make fonts`）。排版接口把它报成 503，而不是 500。
var ErrFontsUnavailable = errors.New("排版字体未就绪")

// Request 是一次排版的输入。
type Request struct {
	Text   string
	Kind   string         // subtitle / text，空 = text
	Style  map[string]any // 文字样式（SCT-03 定的字段，见 [ParseStyle]）
	Box    map[string]any // 文字层的位置框：xPct / yPct / anchor；字幕不看它
	Width  int            // 画面宽（像素）
	Height int            // 画面高（像素）
}

// Box 是一块区域在画面里的位置（像素）。
type Box struct{ X, Y, W, H int }

// BoxPct 是同一块区域按画面宽高折成的百分比。
type BoxPct struct{ X, Y, W, H float64 }

// Result 是排版结果。从缓存取到的结果与缓存共用，调用方不得修改。
type Result struct {
	PNG         []byte   // 透明 PNG，尺寸就是 Box 的宽高
	Box         Box      // PNG 在画面里的位置：非透明区域的包围框（含描边与底框）
	BoxPct      BoxPct   // 同上，百分比
	Lines       []string // 断好的各行（去掉行尾空白）
	FontSizePct float64  // 实际用的字号：超过 3 行时缩过
	Missing     []string // 字体里都没有、画成替代字形的字符（按出现顺序去重）
	Key         string   // 缓存键：sha256(算法版本 + 字体版本 + 种类 + 尺寸 + 样式 + 文字)
}

// input 是校验、规范化之后的请求。
type input struct {
	text  []rune
	kind  string
	style Style
	w, h  int
}

// normalize 校验文字、种类与尺寸，并解析样式（样式按 [ParseStyle] 夹回，不报错）。
func normalize(req Request) (input, error) {
	kind := req.Kind
	if kind == "" {
		kind = KindText
	}
	if kind != KindSubtitle && kind != KindText {
		return input{}, &FieldError{Path: "kind", Msg: "只支持 subtitle / text"}
	}
	if n := utf8.RuneCountInString(req.Text); n > MaxTextRunes {
		return input{}, &FieldError{Path: "text", Msg: fmt.Sprintf("最多 %d 个字（现在 %d 个）", MaxTextRunes, n)}
	}
	text := strings.TrimSpace(strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(req.Text))
	if text == "" {
		return input{}, &FieldError{Path: "text", Msg: "不能为空"}
	}
	for _, d := range []struct {
		path string
		v    int
	}{{"width", req.Width}, {"height", req.Height}} {
		if d.v < MinFrameSize || d.v > MaxFrameSize {
			return input{}, &FieldError{Path: d.path, Msg: fmt.Sprintf("应在 %d~%d 之间", MinFrameSize, MaxFrameSize)}
		}
	}
	return input{
		text:  []rune(text),
		kind:  kind,
		style: ParseStyle(kind, req.Style, req.Box),
		w:     req.Width,
		h:     req.Height,
	}, nil
}

// ValidateRequest 严格校验排版接口的入参：文字、种类、尺寸，再加上样式与位置框的取值范围。
// 第一处不合法的字段以 [*FieldError] 返回（40001 的 details.path）。
func ValidateRequest(req Request) error {
	if _, err := normalize(req); err != nil {
		return err
	}
	kind := req.Kind
	if kind == "" {
		kind = KindText
	}
	return ValidateStyle(kind, req.Style, req.Box)
}

func (in input) key(fontVersion string) string {
	h := sha256.New()
	fmt.Fprintf(h, "typeset/%s\n%s\n%s\n%dx%d\n%s\n", layoutVersion, fontVersion, in.kind, in.w, in.h, in.style.canonical())
	h.Write([]byte(string(in.text)))
	return hex.EncodeToString(h.Sum(nil))
}

// Layout 排版并光栅化，不走缓存。渲染与排版接口共用它：同一个输入永远得到同一串 PNG 字节。
func Layout(fs *FontSet, req Request) (*Result, error) {
	in, err := normalize(req)
	if err != nil {
		return nil, err
	}
	fc := fs.acquire()
	defer fs.release(fc)
	res, err := build(fc, in)
	if err != nil {
		return nil, err
	}
	res.Key = in.key(fs.version)
	return res, nil
}

// build 是排版的主体：断行、缩字号、定位、光栅化。
func build(fc *faces, in input) (*Result, error) {
	st := in.style
	W, H := float64(in.w), float64(in.h)
	basePx := st.FontSizePct / 100 * H
	pad := 0.0
	if st.Background != nil {
		pad = st.Background.PaddingPct / 100 * H
	}
	hardW := W - 2*pad // 外框（文字块 + 内边距）不能比画面宽
	maxW := math.Min(st.MaxWidthPct/100*W, hardW)
	if st.MaxEm > 0 {
		maxW = math.Min(maxW, st.MaxEm*basePx)
	}

	// 超过 3 行先把字号缩到下限，仍超长再继续换行（SC-PARAM-31）。缩了行数也没变少的
	// （多半是手动换的行）就不缩：那样只会把字变小，换不来任何东西。
	sizePct := st.FontSizePct
	shaped := shapeParagraph(fc, in.text) // 只整形一次：换字号只是换比例
	p := shaped.at(basePx)
	lines := wrapLines(p, maxW, hardW)
	if lo := math.Min(minFontSizePct, st.FontSizePct); len(lines) > maxLines && lo < sizePct {
		p2 := shaped.at(lo / 100 * H)
		if l2 := wrapLines(p2, maxW, hardW); len(l2) < len(lines) {
			p, lines, sizePct = p2, l2, lo
		}
	}
	if len(lines) == 0 {
		return nil, &FieldError{Path: "text", Msg: "没有可显示的字符"}
	}
	px := sizePct / 100 * H

	// 文字块：各行宽度取到行尾空白之前；行高固定为字号的 1.25 倍，基线让字身在行里居中。
	lineH := px * lineHeightEm
	asc, desc := emAscent*px, emDescent*px
	lineW := make([]float64, len(lines))
	var blockW float64
	for i, l := range lines {
		lineW[i] = p.width(l.start, trimEnd(p.text, l.start, l.end))
		blockW = math.Max(blockW, lineW[i])
	}
	boxW, boxH := blockW+2*pad, float64(len(lines))*lineH+2*pad
	if boxW > W+0.5 || boxH > H+0.5 {
		return nil, &FieldError{Path: "text", Msg: fmt.Sprintf(
			"排版后的文字（%.0f×%.0f）超出画面（%d×%d），请减少文字或调小字号", boxW, boxH, in.w, in.h)}
	}

	// 外框定位：字幕底边落在底部安全区上，文字层按位置框；水平都以 xPct 为中心。
	// 然后把外框整个挪进画面 —— 被画面边缘裁掉一截的字幕是一种「安静的错误」：
	// 任务成功了，而观众看到的是半行字。
	bx := st.XPct/100*W - boxW/2
	var by float64
	if in.kind == KindSubtitle {
		by = H*(1-st.BottomPct/100) - boxH
	} else {
		ay := st.YPct / 100 * H
		switch st.Anchor {
		case "top":
			by = ay
		case "bottom":
			by = ay - boxH
		default:
			by = ay - boxH/2
		}
	}
	bx = math.Max(0, math.Min(bx, W-boxW))
	by = math.Max(0, math.Min(by, H-boxH))

	// 画布：外框加上描边与字形越出行框的余量，再裁到画面里。
	radius := 0.0
	if st.Stroke != nil {
		radius = st.Stroke.WidthPct / 100 * H
	}
	m := radius + 0.3*px + 2
	canvas := image.Rect(int(math.Floor(bx-m)), int(math.Floor(by-m)),
		int(math.Ceil(bx+boxW+m)), int(math.Ceil(by+boxH+m))).Intersect(image.Rect(0, 0, in.w, in.h))
	ox, oy := float64(canvas.Min.X), float64(canvas.Min.Y)
	cw, ch := canvas.Dx(), canvas.Dy()

	var glyphs []placed
	base0 := by + pad + (lineH-(asc+desc))/2 + asc
	for i, l := range lines {
		x := bx + pad
		switch st.Align {
		case "left":
		case "right":
			x += blockW - lineW[i]
		default:
			x += (blockW - lineW[i]) / 2
		}
		y := base0 + float64(i)*lineH
		end := trimEnd(p.text, l.start, l.end)
		for _, g := range p.glyphs {
			if g.cluster < l.start || g.cluster >= end {
				continue
			}
			// 控制字符（行内的制表符等）不画：字体里没有它们，画出来是个方框。
			if !unicode.IsControl(p.text[g.cluster]) {
				glyphs = append(glyphs, placed{face: g.face, gid: g.gid, x: x + g.xoff*p.k - ox, y: y - g.yoff*p.k - oy, px: px})
			}
			x += g.adv * p.k
		}
	}

	fill := glyphMask(glyphs, cw, ch)
	var stroke []float32
	if radius > 0 {
		stroke = strokeCoverage(fill, radius)
	}
	var bg *roundRect
	if st.Background != nil {
		r := math.Min(0.3*px, math.Min(pad, math.Min(boxW, boxH)/2))
		bg = &roundRect{x0: bx - ox, y0: by - oy, x1: bx + boxW - ox, y1: by + boxH - oy, r: r}
	}
	img := compose(cw, ch, fill, stroke, bg, st)
	vis := opaqueBounds(img)
	if vis.Empty() {
		return nil, &FieldError{Path: "text", Msg: "没有可显示的字符"}
	}
	var buf bytes.Buffer
	if err := pngEncoder.Encode(&buf, img.SubImage(vis)); err != nil {
		return nil, fmt.Errorf("编码 PNG：%w", err)
	}

	box := Box{X: canvas.Min.X + vis.Min.X, Y: canvas.Min.Y + vis.Min.Y, W: vis.Dx(), H: vis.Dy()}
	res := &Result{
		PNG: buf.Bytes(),
		Box: box,
		BoxPct: BoxPct{
			X: pct(box.X, in.w), Y: pct(box.Y, in.h), W: pct(box.W, in.w), H: pct(box.H, in.h),
		},
		Lines:       make([]string, len(lines)),
		FontSizePct: sizePct,
		Missing:     p.missing,
	}
	for i, l := range lines {
		res.Lines[i] = string(p.text[l.start:trimEnd(p.text, l.start, l.end)])
	}
	return res, nil
}

// pngEncoder 复用压缩器：每次新建一个 zlib 写入器要分配一兆多的表，比排版本身还贵。
// BestSpeed：大块透明区域在哪一档都压得很小，压缩档位只影响这个同步接口的耗时。
var pngEncoder = &png.Encoder{CompressionLevel: png.BestSpeed, BufferPool: &encoderPool{}}

type encoderPool struct{ p sync.Pool }

func (e *encoderPool) Get() *png.EncoderBuffer {
	b, _ := e.p.Get().(*png.EncoderBuffer)
	return b
}

func (e *encoderPool) Put(b *png.EncoderBuffer) { e.p.Put(b) }

// 表意字框（ideographic em-box）：思源黑体是 [-120, 880]/1000。基线按它在行里上下居中，
// 汉字才正好落在行的正中间。不用 hhea 的 1160 / -288：那是给拉丁重音与降部留的空间，
// 按它算的话汉字整体偏下，带底框时上下留白明显不等。
const (
	emAscent  = 0.88
	emDescent = 0.12
)

func pct(v, total int) float64 { return math.Round(float64(v)/float64(total)*1e6) / 1e4 }

// Options 是 [New] 的参数，零值即可用。
type Options struct {
	FontDir       string // 字体目录；空 = [ResolveFontDir]（环境变量 SEEAITV_TYPESET_FONT_DIR，再往上找源码树）
	CacheBytes    int64  // 缓存上限；0 = 64 MB
	MaxConcurrent int    // 同时光栅化的个数；0 = 2
}

// Typesetter 给排版接口用：字体懒加载、按内容哈希缓存、限制同时光栅化的个数。
type Typesetter struct {
	dir   string
	cache *lru
	sem   chan struct{}

	mu sync.Mutex
	fs *FontSet
}

// New 建一个排版器。不在这里加载字体：API 进程不必为一个预览接口在启动时多读几十 MB，
// 字体缺了也只有这个接口报错，而不是整个进程起不来。
func New(opt Options) *Typesetter {
	n := opt.MaxConcurrent
	if n <= 0 {
		n = 2
	}
	return &Typesetter{dir: ResolveFontDir(opt.FontDir), cache: newLRU(opt.CacheBytes), sem: make(chan struct{}, n)}
}

// Fonts 取字体，第一次用到时加载。失败不记住：补上字体（`make fonts`）之后不用重启。
func (t *Typesetter) Fonts() (*FontSet, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.fs != nil {
		return t.fs, nil
	}
	fs, err := LoadFonts(t.dir)
	if err != nil {
		return nil, fmt.Errorf("%w：%w", ErrFontsUnavailable, err)
	}
	t.fs = fs
	return fs, nil
}

// Typeset 排版，先查缓存。光栅化在调用方的 goroutine 里同步执行，同时最多 MaxConcurrent 个，
// 其余排队到 ctx 结束 —— 一张满屏的大字要几十 MB 内存，不设并发上限的话一批请求就能把 API 撑爆。
func (t *Typesetter) Typeset(ctx context.Context, req Request) (*Result, error) {
	in, err := normalize(req)
	if err != nil {
		return nil, err
	}
	fs, err := t.Fonts()
	if err != nil {
		return nil, err
	}
	key := in.key(fs.version)
	if r, ok := t.cache.get(key); ok {
		return r, nil
	}
	select {
	case t.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-t.sem }()
	if r, ok := t.cache.get(key); ok { // 排队期间别的请求可能刚排好同一段
		return r, nil
	}
	fc := fs.acquire()
	res, err := build(fc, in)
	fs.release(fc)
	if err != nil {
		return nil, err
	}
	res.Key = key
	t.cache.put(key, res)
	return res, nil
}
