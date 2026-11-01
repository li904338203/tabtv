package typeset

import (
	"encoding/json"
	"fmt"
	"image/color"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// 文字种类：字幕按「距底部」定位，文字层按位置框定位（dev/02 §2）。
const (
	KindSubtitle = "subtitle"
	KindText     = "text"
)

// 排版常量（SC-PARAM-31、dev/01 §4.5）。它们不是样式字段：SCT-03 定的文字样式里没有这几项。
const (
	maxLines       = 3    // 超过这么多行时先缩字号
	minFontSizePct = 3.0  // 缩字号的下限（画面高度的百分比）
	subtitleMaxEm  = 16.0 // 字幕没给 maxWidthPct 时，一行最多约 16 个汉字宽
	lineHeightEm   = 1.25 // 行高是字号的倍数
)

// Style 是一段文字的排版样式（已补缺省值、已夹回范围）。
//
// ⚠️ 所有长度都按**画面高度的百分比**存：字号、描边宽度、底框内边距都是。换画幅、换预览尺寸时
// 比例不变，同一份样式在 360p 预览与 1080p 成片里长得一样（dev/02 §2「字号是画面高度的百分比」；
// dev/01 的示例 `stroke.widthPct: 0.6` 也只有按画面高度算才有意义 —— 按字号算的话细到看不见）。
type Style struct {
	FontSizePct float64 // 字号，2~20
	Color       color.NRGBA
	Stroke      *Stroke     // nil = 不描边
	Background  *Background // nil = 没有底框
	Align       string      // left / center / right：多行时各行在文字块里怎么对齐
	MaxWidthPct float64     // 一行最宽占画面宽度的百分比，10~100
	// MaxEm 是一行最多几个字宽（按定下来的字号算，缩字号时不跟着缩），0 = 不限。
	// 只有字幕在没给 maxWidthPct 时才有（SC-PARAM-31「每行最多约 16 个汉字（按宽度算）」）：
	// 横屏下 90% 的画宽能放三十多个字，一眼读不完。
	MaxEm float64

	BottomPct float64 // 字幕：外框底边距画面底边，0~50
	XPct      float64 // 文字层：外框水平中心，0~100（来自位置框）
	YPct      float64 // 文字层：锚点纵坐标，0~100（来自位置框）
	Anchor    string  // 文字层：top / center / bottom —— 外框的哪条边落在 YPct 上
}

// Stroke 是描边：宽度是画面高度的百分比（0~5）。
type Stroke struct {
	Color    color.NRGBA
	WidthPct float64
}

// Background 是底框：内边距是画面高度的百分比（0~10）。
type Background struct {
	Color      color.NRGBA
	Opacity    float64 // 0~1
	PaddingPct float64
}

// FieldError 指出是哪一项不合法（40001 的 details.path）。
type FieldError struct {
	Path string
	Msg  string
}

func (e *FieldError) Error() string { return e.Path + "：" + e.Msg }

// 取值范围与 SCT-03 的保存校验一致（timeline/domain/content.go 的 textStyle / box，
// api/fixtures/smartcut/README.md「各子对象」表）。改范围要两边一起改。
type numRange struct {
	key    string
	lo, hi float64
}

var (
	rangeFontSize   = numRange{"fontSizePct", 2, 20}
	rangeMaxWidth   = numRange{"maxWidthPct", 10, 100}
	rangeBottom     = numRange{"bottomPct", 0, 50}
	rangeStrokeW    = numRange{"widthPct", 0, 5}
	rangeBgOpacity  = numRange{"opacity", 0, 1}
	rangeBgPadding  = numRange{"paddingPct", 0, 10}
	rangeX          = numRange{"xPct", 0, 100}
	rangeY          = numRange{"yPct", 0, 100}
	colorPattern    = regexp.MustCompile(`^#([0-9a-fA-F]{3}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})$`)
	textAligns      = map[string]bool{"left": true, "center": true, "right": true}
	boxAnchors      = map[string]bool{"top": true, "center": true, "bottom": true}
	defaultStroke   = Stroke{Color: color.NRGBA{A: 255}, WidthPct: 0.3}
	defaultBgColor  = color.NRGBA{A: 255}
	defaultBgAlpha  = 0.6
	defaultBgPad    = 1.0
	defaultColor    = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
	defaultMaxWidth = 90.0
)

// defaultStyle 是各种类的缺省样式（SC-PARAM-31、dev/01 §4.5）。
func defaultStyle(kind string) Style {
	s := Style{Color: defaultColor, Align: "center", MaxWidthPct: defaultMaxWidth}
	if kind == KindSubtitle {
		// 字号 4.5%、底部安全区 8%、一行约 16 个字宽；白字黑描边 —— 字幕叠在任意画面上都得看得清。
		s.FontSizePct = 4.5
		s.BottomPct = 8
		s.MaxEm = subtitleMaxEm
		st := defaultStroke
		s.Stroke = &st
		s.XPct = 50
	} else {
		// 文字层多是标题：字号 8%（dev/01 的示例值），默认在画面正中。
		s.FontSizePct = 8
		s.XPct, s.YPct = 50, 50
		s.Anchor = "center"
	}
	return s
}

// ParseStyle 按种类取缺省样式，再用 style（文字样式）与 box（文字层的位置框）覆盖。
//
// ⚠️ **不报错**：范围外的数夹回范围，类型不对或认不出来的值按缺省处理，未知字段忽略。
// 存进库的样式已经过保存校验（SCT-03）；这里再拦一次的话，一条历史数据就能让整次渲染失败，
// 而预览与成片又必须是同一个结果 —— 所以两处都按同一套夹回规则走。
// 需要「指出哪一项不合法」的地方（排版接口的入参）先调 [ValidateStyle]。
func ParseStyle(kind string, style, box map[string]any) Style {
	if kind != KindSubtitle {
		kind = KindText
	}
	s := defaultStyle(kind)
	clampNum(style, rangeFontSize, &s.FontSizePct)
	if c, ok := lenientColor(style["color"]); ok {
		s.Color = c
	}
	if v, present := style["stroke"]; present {
		s.Stroke = lenientStroke(v, s.Stroke)
	}
	if v, present := style["background"]; present {
		s.Background = lenientBackground(v)
	}
	if a, ok := style["align"].(string); ok && textAligns[a] {
		s.Align = a
	}
	if clampNum(style, rangeMaxWidth, &s.MaxWidthPct) {
		s.MaxEm = 0 // 显式给了最大宽度就按它，不再套 16 个字宽
	}
	if kind == KindSubtitle {
		clampNum(style, rangeBottom, &s.BottomPct)
		return s
	}
	clampNum(box, rangeX, &s.XPct)
	clampNum(box, rangeY, &s.YPct)
	if a, ok := box["anchor"].(string); ok && boxAnchors[a] {
		s.Anchor = a
	}
	return s
}

// ValidateStyle 严格校验排版接口收到的样式与位置框，指出第一处不合法的字段。
// 范围与 [ParseStyle] 夹回用的是同一张表。
func ValidateStyle(kind string, style, box map[string]any) error {
	if err := checkNum(style, "style", rangeFontSize); err != nil {
		return err
	}
	if err := checkColor(style, "style"); err != nil {
		return err
	}
	if v, present := style["stroke"]; present && v != nil {
		m, ok := v.(map[string]any)
		if !ok {
			return &FieldError{Path: "style.stroke", Msg: "必须是对象"}
		}
		if err := checkColor(m, "style.stroke"); err != nil {
			return err
		}
		if err := checkNum(m, "style.stroke", rangeStrokeW); err != nil {
			return err
		}
	}
	if v, present := style["background"]; present && v != nil {
		m, ok := v.(map[string]any)
		if !ok {
			return &FieldError{Path: "style.background", Msg: "必须是对象"}
		}
		if err := checkColor(m, "style.background"); err != nil {
			return err
		}
		for _, r := range []numRange{rangeBgOpacity, rangeBgPadding} {
			if err := checkNum(m, "style.background", r); err != nil {
				return err
			}
		}
	}
	if v, present := style["align"]; present && v != nil {
		if a, ok := v.(string); !ok || !textAligns[a] {
			return &FieldError{Path: "style.align", Msg: "只支持 left / center / right"}
		}
	}
	if err := checkNum(style, "style", rangeMaxWidth); err != nil {
		return err
	}
	if kind == KindSubtitle {
		return checkNum(style, "style", rangeBottom)
	}
	for _, r := range []numRange{rangeX, rangeY} {
		if err := checkNum(box, "box", r); err != nil {
			return err
		}
	}
	if v, present := box["anchor"]; present && v != nil {
		if a, ok := v.(string); !ok || !boxAnchors[a] {
			return &FieldError{Path: "box.anchor", Msg: "只支持 top / center / bottom"}
		}
	}
	return nil
}

// clampNum 读一个数并夹回范围；读到了（不论夹没夹）返回 true。
func clampNum(m map[string]any, r numRange, dst *float64) bool {
	f, ok := toFloat(m[r.key])
	if !ok {
		return false
	}
	*dst = math.Min(r.hi, math.Max(r.lo, f))
	return true
}

func checkNum(m map[string]any, prefix string, r numRange) error {
	v, present := m[r.key]
	if !present || v == nil {
		return nil
	}
	if f, ok := toFloat(v); !ok || f < r.lo || f > r.hi {
		return &FieldError{Path: prefix + "." + r.key, Msg: fmt.Sprintf("应在 %g~%g 之间", r.lo, r.hi)}
	}
	return nil
}

func checkColor(m map[string]any, prefix string) error {
	v, present := m["color"]
	if !present || v == nil {
		return nil
	}
	if _, ok := lenientColor(v); !ok {
		return &FieldError{Path: prefix + ".color", Msg: "颜色只支持 #RGB / #RRGGBB / #RRGGBBAA"}
	}
	return nil
}

// lenientStroke：显式 null 或宽度为 0 = 不描边；对象里缺的项沿用缺省；类型不对时保持原样（缺省）。
func lenientStroke(v any, fallback *Stroke) *Stroke {
	if v == nil {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return fallback
	}
	st := defaultStroke
	if c, ok := lenientColor(m["color"]); ok {
		st.Color = c
	}
	clampNum(m, rangeStrokeW, &st.WidthPct)
	if st.WidthPct == 0 {
		return nil
	}
	return &st
}

// lenientBackground：显式 null、类型不对或不透明度为 0 = 没有底框。
func lenientBackground(v any) *Background {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	bg := Background{Color: defaultBgColor, Opacity: defaultBgAlpha, PaddingPct: defaultBgPad}
	if c, ok := lenientColor(m["color"]); ok {
		bg.Color = c
	}
	clampNum(m, rangeBgOpacity, &bg.Opacity)
	clampNum(m, rangeBgPadding, &bg.PaddingPct)
	if bg.Opacity == 0 {
		return nil
	}
	return &bg
}

func toFloat(v any) (float64, bool) {
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case float32:
		f = float64(x)
	case int:
		f = float64(x)
	case int64:
		f = float64(x)
	case json.Number:
		// 调用方用 UseNumber 解码时也能接住数字。
		var err error
		if f, err = x.Float64(); err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// lenientColor 解析 #RGB / #RRGGBB / #RRGGBBAA（与保存校验同一个正则）。
func lenientColor(v any) (color.NRGBA, bool) {
	s, ok := v.(string)
	if !ok || !colorPattern.MatchString(s) {
		return color.NRGBA{}, false
	}
	h := s[1:]
	if len(h) == 3 {
		h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
	}
	if len(h) == 6 {
		h += "ff"
	}
	n, err := strconv.ParseUint(h, 16, 32)
	if err != nil {
		return color.NRGBA{}, false
	}
	return color.NRGBA{R: uint8(n >> 24), G: uint8(n >> 16), B: uint8(n >> 8), A: uint8(n)}, true
}

// canonical 是样式的规范化串，计入缓存键：同一组取值永远得到同一串（键按字典序）。
// ⚠️ 不能对原始 map 直接 json.Marshal 当键用：取值相同但多带一个未知字段，会得到不同的键；
// 反过来按解析后的值算，`#FFF` 与 `#FFFFFF`、夹回前后的同一个数也会落到同一个键上。
func (s Style) canonical() string {
	kv := map[string]string{
		"fs": fmtF(s.FontSizePct),
		"c":  fmtC(s.Color),
		"al": s.Align,
		"mw": fmtF(s.MaxWidthPct),
		"me": fmtF(s.MaxEm),
		"bp": fmtF(s.BottomPct),
		"x":  fmtF(s.XPct),
		"y":  fmtF(s.YPct),
		"an": s.Anchor,
	}
	if s.Stroke != nil {
		kv["sc"] = fmtC(s.Stroke.Color)
		kv["sw"] = fmtF(s.Stroke.WidthPct)
	}
	if s.Background != nil {
		kv["bgc"] = fmtC(s.Background.Color)
		kv["bgo"] = fmtF(s.Background.Opacity)
		kv["bgp"] = fmtF(s.Background.PaddingPct)
	}
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(kv[k])
		b.WriteByte(';')
	}
	return b.String()
}

func fmtF(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func fmtC(c color.NRGBA) string {
	return fmt.Sprintf("%02x%02x%02x%02x", c.R, c.G, c.B, c.A)
}
