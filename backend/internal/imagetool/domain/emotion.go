package domain

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"sort"
	"strings"

	xdraw "golang.org/x/image/draw"
)

// 情绪调节（dev/01 §3.3）。
//
// 用户手动框选要改的人（R-5：不做自动识别）。服务端把这几个人所在的区域（外接框外扩）裁出来，
// 连同提示词交给图片大模型图生图；产物缩回裁切区域的尺寸，再只把选中人物的框（外扩、羽化）贴回原图 ——
// 框外像素逐像素等于原图（IMGK-9）。只靠提示词说「别动其他人」做不到这一点；裁出局部还让人脸在模型眼里更大、表情改得更准。

// 情绪调节的上限与参数（IMG-PARAM-04 / 05）。
const (
	MaxRoles = 6
	// MinBoxSide 框的最短边（像素）：再小的话脸都看不清，编辑也没有意义。
	MinBoxSide = 24
	// BoxExpand 贴回时框外扩的比例（含头发、下巴的过渡区）。
	BoxExpand = 0.15
	// FeatherRatio 羽化宽度 = 框短边 × 它。
	FeatherRatio = 0.06
	// CropExpand 裁切区域 = 全部人物框的外接框四周各外扩它（按外接框的边长），给模型留一点上下文。
	CropExpand = 0.25
	// MaxCount 一次最多出几张。
	MaxCount = 4
)

// ExpressionConstraint 固定约束句。
// ⚠️ 不写「身份不变」「人物」：「改表情 + 保持身份」的组合容易被上游当成篡改真人面部拦下；主体也可能是动物或卡通角色。
const ExpressionConstraint = "只调整面部表情与神态，主体的外观特征、毛发或发型、服装、姿态与背景保持不变；" +
	"保持构图、取景与画面边缘的内容不变，不要缩放、平移或裁切画面。"

// Emotion 是坐标盘上的一格。Arousal：+2 激动 … -2 平静；Warmth：+2 亲近 … -2 疏离。
type Emotion struct {
	Arousal int    `json:"arousal"`
	Warmth  int    `json:"warmth"`
	Name    string `json:"name"`
	// English 进提示词（代码固定；模型对英文表情词更稳）。不下发前端。
	English string `json:"-"`
}

// Emotions 25 格（IMG-PARAM-06），行 = arousal 从 +2 到 -2，列 = warmth 从 +2（亲近）到 -2（疏离）。
var Emotions = func() []Emotion {
	names := [5][5]string{
		{"欣喜若狂", "兴高采烈", "激动万分", "愤愤不平", "怒不可遏"},
		{"喜笑颜开", "热情洋溢", "惊讶", "焦躁不安", "怒目而视"},
		{"温柔亲切", "和颜悦色", "淡然自若", "冷淡", "冷若冰霜"},
		{"含情脉脉", "恬静安然", "若有所思", "落寞", "漠然"},
		{"安详满足", "宁静祥和", "平静如水", "黯然神伤", "心灰意冷"},
	}
	// ⚠️ 英文描述只写神态、用温和的词：furious / shouting / glare / hopeless 这类激烈词会被上游内容审核拒掉
	// （2026-10-31 实测「怒不可遏」整单被拒）。
	en := [5][5]string{
		{"overjoyed, big open-mouthed smile", "delighted, wide joyful smile", "very excited, eyes wide with excitement",
			"annoyed, slight frown", "very angry expression, deeply furrowed brows"},
		{"beaming, warm broad smile", "enthusiastic, lively friendly smile", "surprised, raised eyebrows, mouth slightly open",
			"uneasy, tense brows", "stern, frowning look"},
		{"gentle and kind, soft smile", "pleasant, friendly expression", "calm, relaxed neutral expression",
			"cool, unsmiling", "cold, expressionless look"},
		{"affectionate, tender gaze", "serene, faint smile", "thoughtful, pensive look",
			"a little lonely, downcast eyes", "indifferent, detached look"},
		{"peaceful and content", "tranquil and relaxed", "perfectly calm",
			"sad, eyes lowered", "dejected, tired look"},
	}
	out := make([]Emotion, 0, 25)
	for r := 0; r < 5; r++ {
		for c := 0; c < 5; c++ {
			out = append(out, Emotion{Arousal: 2 - r, Warmth: 2 - c, Name: names[r][c], English: en[r][c]})
		}
	}
	return out
}()

// EmotionAt 取一格；越界返回 false。
func EmotionAt(arousal, warmth int) (Emotion, bool) {
	if arousal < -2 || arousal > 2 || warmth < -2 || warmth > 2 {
		return Emotion{}, false
	}
	return Emotions[(2-arousal)*5+(2-warmth)], true
}

// Box 是原图像素坐标里的矩形。
type Box struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

func (b Box) rect() image.Rectangle { return image.Rect(b.X, b.Y, b.X+b.W, b.Y+b.H) }

// Role 是一个要改表情的人。
type Role struct {
	ID      string `json:"id"`
	Box     Box    `json:"box"`
	Arousal int    `json:"arousal"`
	Warmth  int    `json:"warmth"`
}

// ExpressionOptions 是情绪调节的选项。
type ExpressionOptions struct {
	SourceMediaID string `json:"sourceMediaId"`
	Count         int    `json:"count"`
	Roles         []Role `json:"roles"`
}

// ErrSourceChanged 人物框所属的源图与当前源图不一致（IMGD-11）。
type ErrSourceChanged struct{ Want, Got int64 }

func (e *ErrSourceChanged) Error() string {
	return fmt.Sprintf("人物框是在另一张图上框选的（%d），当前源图是 %d，请重新框选人物", e.Want, e.Got)
}

func (o ExpressionOptions) prepare(src SourceFacts) (Prepared, error) {
	if o.SourceMediaID == "" {
		return Prepared{}, fieldErr("sourceMediaId", "缺少人物框所属的源图")
	}
	if src.MediaID != 0 && o.SourceMediaID != fmt.Sprint(src.MediaID) {
		var want int64
		_, _ = fmt.Sscan(o.SourceMediaID, &want)
		return Prepared{}, &ErrSourceChanged{Want: want, Got: src.MediaID}
	}
	if src.Width <= 0 || src.Height <= 0 {
		return Prepared{}, fieldErr("sourceMediaId", "源图尺寸未知，无法裁切与贴回")
	}
	count := o.Count
	if count == 0 {
		count = 1
	}
	if count < 1 || count > MaxCount {
		return Prepared{}, fieldErr("count", "张数应在 1~%d", MaxCount)
	}
	if len(o.Roles) == 0 {
		return Prepared{}, fieldErr("roles", "先框选一个人物并选择情绪")
	}
	if len(o.Roles) > MaxRoles {
		return Prepared{}, fieldErr("roles", "最多 %d 个角色", MaxRoles)
	}
	img := image.Rect(0, 0, src.Width, src.Height)
	seen := map[Box]bool{}
	for i, r := range o.Roles {
		p := fmt.Sprintf("roles[%d]", i)
		if _, ok := EmotionAt(r.Arousal, r.Warmth); !ok {
			return Prepared{}, fieldErr(p, "情绪坐标应在 -2~2")
		}
		if r.Box.W < MinBoxSide || r.Box.H < MinBoxSide {
			return Prepared{}, fieldErr(p+".box", "框太小（至少 %d 像素）", MinBoxSide)
		}
		if !r.Box.rect().In(img) {
			return Prepared{}, fieldErr(p+".box", "框超出了图片范围")
		}
		if seen[r.Box] {
			return Prepared{}, fieldErr(p+".box", "与另一个角色的框完全重合")
		}
		seen[r.Box] = true
	}

	roles := append([]Role(nil), o.Roles...)
	sort.SliceStable(roles, func(i, j int) bool { return roles[i].Box.X < roles[j].Box.X })
	crop := CropOf(roles, src.Width, src.Height)
	prompt := ExpressionPrompt(roles, crop)
	opts := ExpressionOptions{SourceMediaID: o.SourceMediaID, Count: count, Roles: roles}
	return Prepared{Tool: ToolExpression, Options: toMap(opts), Prompt: prompt, Count: count, Crop: &crop,
		Params: imageParams(crop.W, crop.H)}, nil
}

// CropOf 裁切区域：全部人物框的外接框，四周各外扩 CropExpand，夹到图内。
func CropOf(roles []Role, imgW, imgH int) Box {
	r := roles[0].Box.rect()
	for _, x := range roles[1:] {
		r = r.Union(x.Box.rect())
	}
	px := int(float64(r.Dx()) * CropExpand)
	py := int(float64(r.Dy()) * CropExpand)
	r = image.Rect(r.Min.X-px, r.Min.Y-py, r.Max.X+px, r.Max.Y+py).Intersect(image.Rect(0, 0, imgW, imgH))
	return Box{X: r.Min.X, Y: r.Min.Y, W: r.Dx(), H: r.Dy()}
}

// ExpressionPrompt 逐个角色写「（在局部图里的）位置 + 情绪」，最后是固定约束句。
func ExpressionPrompt(roles []Role, crop Box) string {
	words := make([]string, len(roles))
	count := map[string]int{}
	for i, r := range roles {
		local := Box{X: r.Box.X - crop.X, Y: r.Box.Y - crop.Y, W: r.Box.W, H: r.Box.H}
		words[i] = PositionWord(local, crop.W, crop.H)
		count[words[i]]++
	}
	nth := map[string]int{}
	var b strings.Builder
	b.WriteString("这是一张图片的局部画面。")
	if len(roles) == 1 {
		e, _ := EmotionAt(roles[0].Arousal, roles[0].Warmth)
		fmt.Fprintf(&b, "把画面主体的面部表情调整为%s（%s）；", e.Name, e.English)
	} else {
		b.WriteString("调整画面中角色的面部表情：")
		for i, r := range roles {
			e, _ := EmotionAt(r.Arousal, r.Warmth)
			pos := words[i]
			if count[pos] > 1 {
				nth[pos]++
				pos += fmt.Sprintf("（从左数第 %d 个）", nth[pos])
			}
			fmt.Fprintf(&b, "位于画面%s的角色，表情调整为%s（%s）；", pos, e.Name, e.English)
		}
	}
	b.WriteString(ExpressionConstraint)
	return b.String()
}

// PositionWord 框中心在图中的相对位置：左侧 / 中间偏左 / 中间 / 中间偏右 / 右侧 × 上部 / 中部 / 下部。
func PositionWord(b Box, imgW, imgH int) string {
	cx := float64(b.X) + float64(b.W)/2
	cy := float64(b.Y) + float64(b.H)/2
	fx, fy := cx/float64(imgW), cy/float64(imgH)
	h := []string{"左侧", "中间偏左", "中间", "中间偏右", "右侧"}[clampIdx(int(fx*5), 5)]
	v := []string{"上部", "中部", "下部"}[clampIdx(int(fy*3), 3)]
	return h + v
}

func clampIdx(i, n int) int {
	if i < 0 {
		return 0
	}
	if i >= n {
		return n - 1
	}
	return i
}

// ScaleBox 把原图坐标的框换算到 w×h 的输出尺寸。
func ScaleBox(b Box, srcW, srcH, w, h int) Box {
	return Box{X: b.X * w / srcW, Y: b.Y * h / srcH, W: b.W * w / srcW, H: b.H * h / srcH}
}

// Resize 双线性缩放到 w×h。
func Resize(src image.Image, w, h int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	xdraw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, src.Bounds(), xdraw.Src, nil)
	return dst
}

// Composite 把 edited 中每个框（外扩 BoxExpand、羽化）的区域贴到 base 上；框外像素**逐像素等于 base**。
// edited 尺寸与 base 不同时先缩放到 base 的尺寸。boxes 已是 base 坐标。
func Composite(base, edited image.Image, boxes []Box) *image.RGBA {
	bb := base.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, bb.Dx(), bb.Dy()))
	draw.Draw(out, out.Bounds(), base, bb.Min, draw.Src)
	ed := edited
	if eb := edited.Bounds(); eb.Dx() != bb.Dx() || eb.Dy() != bb.Dy() {
		ed = Resize(edited, bb.Dx(), bb.Dy())
	}
	edb := ed.Bounds()
	for _, b := range boxes {
		short := min(b.W, b.H)
		pad := int(float64(short) * BoxExpand)
		feather := max(1, int(float64(short)*FeatherRatio))
		r := image.Rect(b.X-pad, b.Y-pad, b.X+b.W+pad, b.Y+b.H+pad).Intersect(out.Bounds())
		for y := r.Min.Y; y < r.Max.Y; y++ {
			for x := r.Min.X; x < r.Max.X; x++ {
				// 到矩形边的最近距离决定 alpha：边上 0，往里 feather 像素处到 1。
				d := min(x-r.Min.X, r.Max.X-1-x, y-r.Min.Y, r.Max.Y-1-y)
				a := float64(d+1) / float64(feather+1)
				if a >= 1 {
					out.Set(x, y, ed.At(x+edb.Min.X, y+edb.Min.Y))
					continue
				}
				if a <= 0 {
					continue
				}
				out.Set(x, y, blend(out.RGBAAt(x, y), ed.At(x+edb.Min.X, y+edb.Min.Y), a))
			}
		}
	}
	return out
}

func blend(base color.RGBA, top color.Color, a float64) color.RGBA {
	tr, tg, tb, ta := top.RGBA()
	mix := func(b uint8, t uint32) uint8 { return uint8(float64(b)*(1-a) + float64(t>>8)*a + 0.5) }
	return color.RGBA{mix(base.R, tr), mix(base.G, tg), mix(base.B, tb), mix(base.A, ta)}
}
