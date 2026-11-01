package typeset

import (
	"bytes"
	"encoding/json"
	"errors"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

var (
	testFontsOnce sync.Once
	testFontSet   *FontSet
	testFontsErr  error
)

// testFonts 取真字体。缺字体时**明确失败**而不是跳过：跳过的话 CI 上这个包一条断言都不跑，
// 而界面上是一片绿。
func testFonts(t *testing.T) *FontSet {
	t.Helper()
	testFontsOnce.Do(func() { testFontSet, testFontsErr = LoadFonts(ResolveFontDir("")) })
	if testFontsErr != nil {
		t.Fatalf("排版测试需要真字体：%v；字体放在别处时用 %s 指过去", testFontsErr, EnvFontDir)
	}
	return testFontSet
}

type layoutCase struct {
	Name        string         `json:"name"`
	Kind        string         `json:"kind"`
	Width       int            `json:"width"`
	Height      int            `json:"height"`
	Style       map[string]any `json:"style"`
	Box         map[string]any `json:"box"`
	Text        string         `json:"text"`
	Lines       []string       `json:"lines"`
	FontSizePct float64        `json:"fontSizePct"`
	Missing     []string       `json:"missing"`
}

// TestLayoutFixtures 跑 testdata/layout.json：避头尾、缩字号、英文按词断行、中英混排、缺字。
// 只比行切分、实际字号与缺字列表，不比像素（像素随字体版本与光栅器变化，比它只会制造噪声）。
func TestLayoutFixtures(t *testing.T) {
	fs := testFonts(t)
	raw, err := os.ReadFile(filepath.Join("testdata", "layout.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Cases []layoutCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Cases) == 0 {
		t.Fatal("fixture 里一条用例都没有")
	}
	for _, c := range file.Cases {
		t.Run(c.Name, func(t *testing.T) {
			res, err := Layout(fs, Request{Text: c.Text, Kind: c.Kind, Style: c.Style, Box: c.Box, Width: c.Width, Height: c.Height})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(res.Lines, c.Lines) {
				t.Errorf("断行\n得到 %q\n期望 %q", res.Lines, c.Lines)
			}
			if res.FontSizePct != c.FontSizePct {
				t.Errorf("字号 = %v，期望 %v", res.FontSizePct, c.FontSizePct)
			}
			if !slices.Equal(nonNil(res.Missing), nonNil(c.Missing)) {
				t.Errorf("缺字 = %q，期望 %q", res.Missing, c.Missing)
			}
		})
	}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// TestKinsokuHoldsAtEveryWidth 把一段标点密集的中文按 15%~100% 的各种宽度排一遍：
// 除段首外没有一行以「行首不放」开头，除段尾外没有一行以「行尾不放」结尾，且一个字都不丢。
func TestKinsokuHoldsAtEveryWidth(t *testing.T) {
	fs := testFonts(t)
	text := "「你好，」他说：「今天（星期三）天气不错。」——我们去公园吧！好啊……那就走吧？" +
		"『三体』与《流浪地球》都是刘慈欣写的，一共卖了 1000 万册、翻译成 30 多种语言。" +
		"ヴァイオリンとチェロ、それっきりだった。"
	for w := 15; w <= 100; w++ {
		res, err := Layout(fs, Request{Text: text, Style: map[string]any{"fontSizePct": 4, "maxWidthPct": w}, Width: 1000, Height: 1000})
		if err != nil {
			t.Fatalf("宽度 %d%%：%v", w, err)
		}
		// 行尾空格不算进行里（trimEnd），所以去掉空格再比。
		if got, want := strings.Join(res.Lines, ""), strings.ReplaceAll(text, " ", ""); strings.ReplaceAll(got, " ", "") != want {
			t.Fatalf("宽度 %d%%：拼回去不是原文\n得到 %q", w, got)
		}
		// 判据用卡片上的原文，不用实现里的表：拿实现自己的表去验实现，表漏了一个字也验不出来。
		const mustNotStart, mustNotEnd = "，。、；：？！）」』》…", "「『（《"
		for i, l := range res.Lines {
			r := []rune(l)
			if i > 0 && strings.ContainsRune(mustNotStart, r[0]) {
				t.Errorf("宽度 %d%%：第 %d 行以「%c」开头：%q", w, i+1, r[0], res.Lines)
			}
			if i < len(res.Lines)-1 && strings.ContainsRune(mustNotEnd, r[len(r)-1]) {
				t.Errorf("宽度 %d%%：第 %d 行以「%c」结尾：%q", w, i+1, r[len(r)-1], res.Lines)
			}
		}
	}
}

// TestSameLinesAtEveryResolution：同样的画幅比例下断行与分辨率无关 —— 预览（360p）与成片
// （1080p）断在同一个字上（SC-N-06）。按实际字号整形的话，整形器把字号取整到整像素，
// 不同分辨率下的字距比例不同，这条就会红。
func TestSameLinesAtEveryResolution(t *testing.T) {
	fs := testFonts(t)
	texts := []string{
		"超长中文字幕自动换行，行首没有逗号、句号；日文こんにちは、韩文안녕하세요、emoji😀👍都能显示。",
		"We shape once at a reference size and scale, so the preview breaks where the film breaks.",
		"标题 Title 测试，AVAWA 的字偶距也算在里面。",
	}
	for _, kind := range []string{KindSubtitle, KindText} {
		for _, text := range texts {
			var want *Result
			for _, size := range [][2]int{{640, 360}, {1280, 720}, {1920, 1080}, {3840, 2160}} {
				res, err := Layout(fs, Request{Text: text, Kind: kind, Width: size[0], Height: size[1]})
				if err != nil {
					t.Fatal(err)
				}
				if want == nil {
					want = res
					continue
				}
				if !reflect.DeepEqual(res.Lines, want.Lines) || res.FontSizePct != want.FontSizePct {
					t.Errorf("%s %dx%d：%q（%v%%），360p 是 %q（%v%%）", kind, size[0], size[1],
						res.Lines, res.FontSizePct, want.Lines, want.FontSizePct)
				}
			}
		}
	}
}

// TestPNGStableAndKeyCoversEveryStyleField：同一输入两次排版字节完全一致；改动任何一项样式
// （以及种类、尺寸、文字），缓存键都会变。
func TestPNGStableAndKeyCoversEveryStyleField(t *testing.T) {
	fs := testFonts(t)
	base := func() Request {
		return Request{
			Text: "同一段字，两次排版。Same bytes 😀", Kind: KindText, Width: 1280, Height: 720,
			Style: map[string]any{
				"fontSizePct": 6, "color": "#FFD400", "align": "left", "maxWidthPct": 60,
				"stroke":     map[string]any{"color": "#000000", "widthPct": 0.5},
				"background": map[string]any{"color": "#202020", "opacity": 0.5, "paddingPct": 1},
			},
			Box: map[string]any{"xPct": 40, "yPct": 30, "anchor": "top"},
		}
	}
	a, err := Layout(fs, base())
	if err != nil {
		t.Fatal(err)
	}
	b, err := Layout(fs, base())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.PNG, b.PNG) || a.Key != b.Key || a.Box != b.Box {
		t.Fatal("同一输入两次排版的结果不一致")
	}
	img, err := png.Decode(bytes.NewReader(a.PNG))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != a.Box.W || img.Bounds().Dy() != a.Box.H {
		t.Fatalf("PNG 尺寸 %v 与包围框 %+v 不一致", img.Bounds(), a.Box)
	}

	mutations := map[string]func(r *Request){
		"fontSizePct":     func(r *Request) { r.Style["fontSizePct"] = 7 },
		"color":           func(r *Request) { r.Style["color"] = "#FFFFFF" },
		"align":           func(r *Request) { r.Style["align"] = "right" },
		"maxWidthPct":     func(r *Request) { r.Style["maxWidthPct"] = 70 },
		"stroke.color":    func(r *Request) { r.Style["stroke"] = map[string]any{"color": "#FF0000", "widthPct": 0.5} },
		"stroke.widthPct": func(r *Request) { r.Style["stroke"] = map[string]any{"color": "#000000", "widthPct": 1} },
		"stroke=null":     func(r *Request) { r.Style["stroke"] = nil },
		"background.color": func(r *Request) {
			r.Style["background"] = map[string]any{"color": "#0000FF", "opacity": 0.5, "paddingPct": 1}
		},
		"background.opacity": func(r *Request) {
			r.Style["background"] = map[string]any{"color": "#202020", "opacity": 0.8, "paddingPct": 1}
		},
		"background.padding": func(r *Request) {
			r.Style["background"] = map[string]any{"color": "#202020", "opacity": 0.5, "paddingPct": 2}
		},
		"background=null":     func(r *Request) { r.Style["background"] = nil },
		"box.xPct":            func(r *Request) { r.Box["xPct"] = 60 },
		"box.yPct":            func(r *Request) { r.Box["yPct"] = 50 },
		"box.anchor":          func(r *Request) { r.Box["anchor"] = "bottom" },
		"kind":                func(r *Request) { r.Kind = KindSubtitle },
		"width":               func(r *Request) { r.Width = 1080 },
		"height":              func(r *Request) { r.Height = 1080 },
		"text":                func(r *Request) { r.Text += "！" },
		"subtitle.bottomPct":  func(r *Request) { r.Kind = KindSubtitle; r.Style["bottomPct"] = 20 },
		"subtitle.maxWidth=∅": func(r *Request) { r.Kind = KindSubtitle; delete(r.Style, "maxWidthPct") },
	}
	seen := map[string]string{a.Key: "base"}
	for name, mutate := range mutations {
		req := base()
		mutate(&req)
		res, err := Layout(fs, req)
		if err != nil {
			t.Fatalf("%s：%v", name, err)
		}
		if prev, dup := seen[res.Key]; dup {
			t.Errorf("改动 %s 之后缓存键与 %s 相同", name, prev)
		}
		seen[res.Key] = name
	}

	// 反过来：未知字段、等价写法、夹回前后的同一个值，键不变（缓存才命中得了）。
	same := map[string]func(r *Request){
		"未知字段": func(r *Request) { r.Style["shadow"] = map[string]any{"blur": 3} },
		"#RGB": func(r *Request) {
			r.Style["stroke"] = map[string]any{"color": "#000", "widthPct": 0.5}
		},
		"字幕专属字段在文字层上": func(r *Request) { r.Style["bottomPct"] = 30 },
	}
	for name, mutate := range same {
		req := base()
		mutate(&req)
		res, err := Layout(fs, req)
		if err != nil {
			t.Fatalf("%s：%v", name, err)
		}
		if res.Key != a.Key || !bytes.Equal(res.PNG, a.PNG) {
			t.Errorf("%s：结果不该变", name)
		}
	}
}

func TestMissingGlyphsListedAndDrawn(t *testing.T) {
	fs := testFonts(t)
	plain, err := Layout(fs, Request{Text: "中文", Width: 1000, Height: 1000})
	if err != nil {
		t.Fatal(err)
	}
	withArabic, err := Layout(fs, Request{Text: "中文مرحبا", Width: 1000, Height: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(withArabic.Missing, []string{"م", "ر", "ح", "ب", "ا"}) {
		t.Errorf("缺字 = %q", withArabic.Missing)
	}
	// 缺的字画替代字形（方框），不是什么都不画：画出来的图要比只有「中文」的宽。
	if withArabic.Box.W <= plain.Box.W {
		t.Errorf("替代字形没有画出来：%d <= %d", withArabic.Box.W, plain.Box.W)
	}
	emoji, err := Layout(fs, Request{Text: "😀👍🎉👨‍👩‍👧 日本語ひらがなカタカナ 한국어", Width: 1000, Height: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(emoji.Missing) != 0 {
		t.Errorf("emoji / 日文 / 韩文不该缺字：%q", emoji.Missing)
	}
}

func TestPlacement(t *testing.T) {
	fs := testFonts(t)
	// 字幕：外框底边落在距底 8% 处（含描边的包围框会再往外几个像素）。
	sub, err := Layout(fs, Request{Text: "底部安全区", Kind: KindSubtitle, Width: 1920, Height: 1080})
	if err != nil {
		t.Fatal(err)
	}
	bottom := sub.Box.Y + sub.Box.H
	if want := 994; bottom < want-12 || bottom > want+12 { // 1080 × (1 - 8%) ≈ 994
		t.Errorf("字幕底边 = %d，期望在 %d 附近", bottom, want)
	}
	if mid := sub.Box.X + sub.Box.W/2; mid < 950 || mid > 970 {
		t.Errorf("字幕应水平居中，中线在 %d", mid)
	}
	// 文字层：anchor=top 时外框顶边落在 yPct 上。
	top, err := Layout(fs, Request{Text: "标题", Width: 1000, Height: 1000, Box: map[string]any{"yPct": 20, "anchor": "top"}})
	if err != nil {
		t.Fatal(err)
	}
	// 包围框是墨迹的范围：汉字的顶在行框顶下面一点（行高 1.25 倍 + 字身留白），但不会高过锚点。
	if top.Box.Y < 200 || top.Box.Y > 230 {
		t.Errorf("anchor=top 时顶边 = %d，期望在 200~230", top.Box.Y)
	}
	// 位置框把字推出画面时整个挪回画面里：被裁掉一截的字是一种「安静的错误」。
	edge, err := Layout(fs, Request{Text: "贴边的标题", Width: 1000, Height: 1000, Box: map[string]any{"xPct": 100, "yPct": 100, "anchor": "top"}})
	if err != nil {
		t.Fatal(err)
	}
	if edge.Box.X+edge.Box.W > 1000 || edge.Box.Y+edge.Box.H > 1000 || edge.Box.X < 0 || edge.Box.Y < 0 {
		t.Errorf("外框伸出了画面：%+v", edge.Box)
	}
	if edge.Box.X+edge.Box.W < 975 || edge.Box.Y+edge.Box.H < 975 {
		t.Errorf("应贴着右下角：%+v", edge.Box)
	}
	if edge.BoxPct.X != float64(edge.Box.X)/10 {
		t.Errorf("百分比 = %v，像素 = %d", edge.BoxPct.X, edge.Box.X)
	}
}

func TestRequestLimits(t *testing.T) {
	fs := testFonts(t)
	long := strings.Repeat("字", MaxTextRunes+1)
	for _, c := range []struct {
		name string
		req  Request
		path string
	}{
		{"空文字", Request{Text: " \n ", Width: 1000, Height: 1000}, "text"},
		{"超过 500 字", Request{Text: long, Width: 1000, Height: 1000}, "text"},
		{"宽太小", Request{Text: "字", Width: 63, Height: 1000}, "width"},
		{"高太大", Request{Text: "字", Width: 1000, Height: MaxFrameSize + 1}, "height"},
		{"种类不认识", Request{Text: "字", Kind: "title", Width: 1000, Height: 1000}, "kind"},
		{"放不进画面", Request{Text: strings.Repeat("字", 400), Style: map[string]any{"fontSizePct": 20, "maxWidthPct": 100}, Width: 64, Height: 3840}, "text"},
	} {
		_, err := Layout(fs, c.req)
		var fe *FieldError
		if !errors.As(err, &fe) || fe.Path != c.path {
			t.Errorf("%s：err = %v，期望指出 %s", c.name, err, c.path)
		}
	}
	// 正好 500 字是合法的。
	if _, err := Layout(fs, Request{Text: strings.Repeat("字", MaxTextRunes), Width: 1920, Height: 1080}); err != nil {
		t.Errorf("500 字应能排：%v", err)
	}
}

func TestLoadFontsSaysHowToFix(t *testing.T) {
	_, err := LoadFonts(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "make fonts") {
		t.Fatalf("缺字体的报错要说清怎么补：%v", err)
	}
}
