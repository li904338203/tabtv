package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 一个区间：起止 + 可见条目的文字（按区间内的固定顺序）。
type segWant struct {
	start, end int
	texts      []string
}

func segTexts(segs []OverlaySegment) []segWant {
	out := make([]segWant, 0, len(segs))
	for _, s := range segs {
		w := segWant{start: s.StartMs, end: s.EndMs}
		for _, it := range s.Items {
			w.texts = append(w.texts, it.Text)
		}
		out = append(out, w)
	}
	return out
}

func sameSegs(got, want []segWant) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i].start != want[i].start || got[i].end != want[i].end || len(got[i].texts) != len(want[i].texts) {
			return false
		}
		for j := range got[i].texts {
			if got[i].texts[j] != want[i].texts[j] {
				return false
			}
		}
	}
	return true
}

// 相邻、重叠、包含三种关系，以及没有字的空区间：区间连续铺满 [0, 总长)，空区间照样出现（叠一张全透明图）。
func TestOverlaySegments_相邻重叠包含与空区间(t *testing.T) {
	snap := RenderSnapshot{
		SubtitleTracks: []SnapSubtitleTrack{{Order: 1, Items: []SnapSubItem{
			{StartMs: 1000, EndMs: 3000, Text: "A"},
			{StartMs: 3000, EndMs: 5000, Text: "B"}, // 与 A 首尾相接
		}}},
		// 标题 T 与 A 重叠，并且整段包含 B。
		Texts: []SnapText{{TrackOrder: 1, StartMs: 2000, EndMs: 8000, Text: "T"}},
	}
	got := segTexts(OverlaySegments(snap, 10000))
	want := []segWant{
		{0, 1000, nil},
		{1000, 2000, []string{"A"}},
		{2000, 3000, []string{"A", "T"}},
		{3000, 5000, []string{"B", "T"}},
		{5000, 8000, []string{"T"}},
		{8000, 10000, nil},
	}
	if !sameSegs(got, want) {
		t.Fatalf("区间：\n  得到 %v\n  期望 %v", got, want)
	}
	segs := OverlaySegments(snap, 10000)
	if segs[2].Items[0].Kind != OverlaySubtitle || segs[2].Items[1].Kind != OverlayText {
		t.Fatalf("字幕在前、文字层在后：%+v", segs[2].Items)
	}
}

// 相邻两条文字、样式完全相同的字幕（识别常把一句话切成两条）合并成一个区间：画面上什么都没变，
// 多切一刀只是多一张图、多一行 concat。
func TestOverlaySegments_内容相同的相邻区间合并(t *testing.T) {
	snap := RenderSnapshot{SubtitleTracks: []SnapSubtitleTrack{{Order: 1, Items: []SnapSubItem{
		{StartMs: 0, EndMs: 1000, Text: "你好"},
		{StartMs: 1000, EndMs: 2000, Text: "你好"},
		{StartMs: 2000, EndMs: 3000, Text: "你好", Style: json.RawMessage(`{"color":"#ff0000"}`)},
	}}}}
	got := segTexts(OverlaySegments(snap, 3000))
	want := []segWant{{0, 2000, []string{"你好"}}, {2000, 3000, []string{"你好"}}}
	if !sameSegs(got, want) {
		t.Fatalf("同文同样式的相邻区间合并，样式不同的不合并：\n  得到 %v\n  期望 %v", got, want)
	}
}

// 两条字幕轨同时有字：按轨道 Order 升序（第一条在最下方），与它们在快照里的先后无关；
// 文字层排在全部字幕之后，即使它的轨道序号更小。
func TestOverlaySegments_两条字幕轨同时出现的顺序(t *testing.T) {
	snap := RenderSnapshot{
		SubtitleTracks: []SnapSubtitleTrack{
			{Order: 2, Items: []SnapSubItem{{StartMs: 0, EndMs: 2000, Text: "英文"}}},
			{Order: 1, Items: []SnapSubItem{
				{StartMs: 500, EndMs: 2000, Text: "中文2"},
				{StartMs: 0, EndMs: 500, Text: "中文1"},
			}},
		},
		Texts: []SnapText{{TrackOrder: 1, StartMs: 0, EndMs: 2000, Text: "标题"}},
	}
	segs := OverlaySegments(snap, 2000)
	got := segTexts(segs)
	want := []segWant{
		{0, 500, []string{"中文1", "英文", "标题"}},
		{500, 2000, []string{"中文2", "英文", "标题"}},
	}
	if !sameSegs(got, want) {
		t.Fatalf("顺序：\n  得到 %v\n  期望 %v", got, want)
	}
	if segs[0].Items[0].TrackOrder != 1 || segs[0].Items[1].TrackOrder != 2 {
		t.Fatalf("条目带上轨道序号：%+v", segs[0].Items)
	}
}

// 越界的夹回 [0, 总长)；完全落在外面的、空文字、起止倒置的不出现。
func TestOverlaySegments_越界夹回与跳过(t *testing.T) {
	snap := RenderSnapshot{
		SubtitleTracks: []SnapSubtitleTrack{{Order: 1, Items: []SnapSubItem{
			{StartMs: -500, EndMs: 1000, Text: "开头"},
			{StartMs: 1500, EndMs: 1500, Text: "零长"},
			{StartMs: 2000, EndMs: 1800, Text: "倒置"},
			{StartMs: 1200, EndMs: 1400, Text: "  \n "},
		}}},
		Texts: []SnapText{
			{TrackOrder: 1, StartMs: 4000, EndMs: 9000, Text: "结尾"},
			{TrackOrder: 1, StartMs: 6000, EndMs: 7000, Text: "片尾之后"},
		},
	}
	got := segTexts(OverlaySegments(snap, 5000))
	want := []segWant{{0, 1000, []string{"开头"}}, {1000, 4000, nil}, {4000, 5000, []string{"结尾"}}}
	if !sameSegs(got, want) {
		t.Fatalf("夹回与跳过：\n  得到 %v\n  期望 %v", got, want)
	}
	if segs := OverlaySegments(snap, 0); len(segs) != 0 {
		t.Fatalf("总长为 0 时没有区间：%+v", segs)
	}
	if segs := OverlaySegments(RenderSnapshot{}, 3000); len(segs) != 1 || segs[0].StartMs != 0 ||
		segs[0].EndMs != 3000 || len(segs[0].Items) != 0 {
		t.Fatalf("没有任何字时是一整段空区间：%+v", segs)
	}
}

// 字幕条目的样式 = 轨道样式 ⊕ 条目样式（条目覆盖轨道，嵌套对象逐键合并）；文字层的样式与位置框原样带出。
func TestOverlaySegments_样式合并与文字层原样(t *testing.T) {
	snap := RenderSnapshot{
		SubtitleTracks: []SnapSubtitleTrack{{Order: 1,
			Style: json.RawMessage(`{"color":"#ffffff","stroke":{"color":"#000000","widthPct":0.6},"bottomPct":8}`),
			Items: []SnapSubItem{{StartMs: 0, EndMs: 1000, Text: "字幕", Style: json.RawMessage(`{"stroke":{"widthPct":1}}`)}},
		}},
		Texts: []SnapText{{TrackOrder: 1, StartMs: 0, EndMs: 1000, Text: "标题",
			Style: json.RawMessage(`{"fontSizePct":6}`), Box: json.RawMessage(`{"xPct":50,"yPct":20}`)}},
	}
	items := OverlaySegments(snap, 1000)[0].Items
	assertJSON(t, items[0].Style, `{"bottomPct":8,"color":"#ffffff","stroke":{"color":"#000000","widthPct":1}}`)
	if items[0].Box != nil {
		t.Fatal("字幕没有位置框")
	}
	assertJSON(t, items[1].Style, `{"fontSizePct":6}`)
	assertJSON(t, items[1].Box, `{"xPct":50,"yPct":20}`)
}

func assertJSON(t *testing.T, got json.RawMessage, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("不是合法 JSON：%s", got)
	}
	_ = json.Unmarshal([]byte(want), &w)
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	if !bytes.Equal(gb, wb) {
		t.Fatalf("得到 %s，期望 %s", got, want)
	}
}

func TestMergeStyle(t *testing.T) {
	cases := []struct {
		name   string
		layers []json.RawMessage
		want   string // 空 = nil
	}{
		{"后者覆盖前者", []json.RawMessage{json.RawMessage(`{"color":"#fff","align":"center"}`),
			json.RawMessage(`{"color":"#ff0"}`)}, `{"align":"center","color":"#ff0"}`},
		{"嵌套对象逐键合并", []json.RawMessage{json.RawMessage(`{"background":{"color":"#000","opacity":0.5}}`),
			json.RawMessage(`{"background":{"opacity":0.8,"paddingPct":1}}`)},
			`{"background":{"color":"#000","opacity":0.8,"paddingPct":1}}`},
		{"对象被标量覆盖、标量被对象覆盖", []json.RawMessage{json.RawMessage(`{"stroke":{"widthPct":1},"x":1}`),
			json.RawMessage(`{"stroke":false,"x":{"y":2}}`)}, `{"stroke":false,"x":{"y":2}}`},
		{"null 与缺省同义：不抹掉前面的值", []json.RawMessage{json.RawMessage(`{"color":"#fff"}`),
			json.RawMessage(`{"color":null}`)}, `{"color":"#fff"}`},
		{"坏层、空层、非对象层跳过", []json.RawMessage{nil, json.RawMessage(`[1]`), json.RawMessage(`{坏`),
			json.RawMessage(`null`), json.RawMessage(`{"a":1}`)}, `{"a":1}`},
		{"一层都没有", []json.RawMessage{nil, json.RawMessage(`"x"`)}, ""},
		{"数字原样（不经 float64 舍入）", []json.RawMessage{json.RawMessage(`{"widthPct":0.6,"big":12345678901234567890}`)},
			`{"big":12345678901234567890,"widthPct":0.6}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := MergeStyle(c.layers...)
			if c.want == "" {
				if got != nil {
					t.Fatalf("应为 nil：%s", got)
				}
				return
			}
			if string(got) != c.want {
				t.Fatalf("得到 %s，期望 %s", got, c.want)
			}
		})
	}
	// 不改动传进来的层（runner 会把轨道样式拿去合并好几次）。
	track := json.RawMessage(`{"stroke":{"widthPct":0.6}}`)
	MergeStyle(track, json.RawMessage(`{"stroke":{"widthPct":2}}`))
	if string(track) != `{"stroke":{"widthPct":0.6}}` {
		t.Fatalf("轨道样式被改了：%s", track)
	}
}

func TestStackBottomPct(t *testing.T) {
	cases := []struct {
		name    string
		heights []float64
		want    []float64
	}{
		{"逐条往上让出前面各条的高度加间距", []float64{5, 4, 3}, []float64{8, 14, 19}},
		{"中间那条这一段没字：不占位置也不占间距", []float64{5, 0, 3}, []float64{8, 14, 14}},
		{"第一条没字：第二条落在最下面", []float64{0, 5}, []float64{8, 8}},
		{"负高度按 0", []float64{-2, 5}, []float64{8, 8}},
		{"没有轨道", nil, []float64{}},
	}
	for _, c := range cases {
		got := StackBottomPct(8, c.heights, 1)
		if len(got) != len(c.want) {
			t.Fatalf("%s：%v", c.name, got)
		}
		for i := range got {
			if diff := got[i] - c.want[i]; diff > 1e-9 || diff < -1e-9 {
				t.Fatalf("%s：得到 %v，期望 %v", c.name, got, c.want)
			}
		}
	}
}

func solid(w, h int, c color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

// 全透明画布、按位置贴、后贴的压在上面（Over 合成）、超出画布的部分裁掉。
func TestComposeOverlay(t *testing.T) {
	red := color.NRGBA{255, 0, 0, 255}
	halfGreen := color.NRGBA{0, 255, 0, 128}
	got := ComposeOverlay(10, 10, []PlacedImage{
		{Img: solid(4, 4, red), X: 2, Y: 3},
		{Img: solid(4, 4, halfGreen), X: 4, Y: 5}, // 与红块右下角重叠 2×2
		{Img: solid(4, 4, red), X: 8, Y: 8},       // 只有 2×2 落在画布里
		{Img: solid(4, 4, red), X: -2, Y: -2},     // 只有 2×2 落在画布里
		{Img: nil, X: 1, Y: 1},
		{Img: solid(3, 3, red), X: 20, Y: 20}, // 完全在画布外
	})
	if b := got.Bounds(); b.Dx() != 10 || b.Dy() != 10 {
		t.Fatalf("画布大小：%v", b)
	}
	if c := got.NRGBAAt(3, 4); c != red {
		t.Fatalf("红块位置：%v", c)
	}
	if c := got.NRGBAAt(0, 9); c.A != 0 {
		t.Fatalf("没贴的地方全透明：%v", c)
	}
	// 半透明绿压在红上：仍然不透明，红绿各约一半。
	if c := got.NRGBAAt(5, 6); c.A != 255 || c.G < 120 || c.G > 135 || c.R < 120 || c.R > 135 || c.B != 0 {
		t.Fatalf("Over 合成：%v", c)
	}
	// 半透明绿贴在透明处：保留它自己的透明度，不被底下的透明画布「染黑」。
	if c := got.NRGBAAt(7, 8); c.A < 126 || c.A > 130 || c.G < 250 || c.R != 0 {
		t.Fatalf("贴在透明处：%v", c)
	}
	if got.NRGBAAt(9, 9) != red || got.NRGBAAt(0, 0) != red || got.NRGBAAt(1, 1) != red || got.NRGBAAt(2, 2).A != 0 {
		t.Fatal("超出画布的部分裁掉、留下的贴在边角")
	}
}

func TestOverlayConcatScript(t *testing.T) {
	got := OverlayConcatScript([]string{"a.png", "b c.png", "it's.png"}, []int{1000, 2500, 33})
	want := "ffconcat version 1.0\n" +
		"file 'a.png'\nduration 1.000\n" +
		"file 'b c.png'\nduration 2.500\n" +
		"file 'it'\\''s.png'\nduration 0.033\n" +
		"file 'it'\\''s.png'\n"
	if got != want {
		t.Fatalf("脚本形状：\n%s\n期望\n%s", got, want)
	}
	if got := OverlayConcatScript(nil, nil); got != "ffconcat version 1.0\n" {
		t.Fatalf("没有文件时只有头：%q", got)
	}
	// 数量对不上时只配到较短的那一边；负时长按 0。
	if got := OverlayConcatScript([]string{"a.png", "b.png"}, []int{-5}); got !=
		"ffconcat version 1.0\nfile 'a.png'\nduration 0.000\nfile 'a.png'\n" {
		t.Fatalf("数量对不上：%q", got)
	}
}

// 用真的 ffmpeg 走一遍「切区间 → 合成整幅透明图 → concat 分离器 → overlay」（本机没有 ffmpeg 时跳过）：
// 透明处露出底下的画面、有字的区间出字、片尾最后一个区间（2.5~3 秒）也在。
func TestOverlay_真实ffmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("本机没有 ffmpeg")
	}
	const w, h, total = 64, 36, 3000
	snap := RenderSnapshot{SubtitleTracks: []SnapSubtitleTrack{{Order: 1, Items: []SnapSubItem{
		{StartMs: 1000, EndMs: 2000, Text: "中"},
		{StartMs: 2500, EndMs: 3000, Text: "尾"},
	}}}}
	segs := OverlaySegments(snap, total)
	dir := t.TempDir()
	var files []string
	var durs []int
	for i, s := range segs {
		var placed []PlacedImage
		if len(s.Items) > 0 { // 有字的区间：在 (20,20) 贴一块黄色当作排好的字
			placed = append(placed, PlacedImage{Img: solid(10, 6, color.NRGBA{255, 255, 0, 255}), X: 20, Y: 20})
		}
		name := fmt.Sprintf("seg-%d.png", i)
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := png.Encode(f, ComposeOverlay(w, h, placed)); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		files = append(files, name)
		durs = append(durs, s.EndMs-s.StartMs)
	}
	script := filepath.Join(dir, "overlay.txt")
	if err := os.WriteFile(script, []byte(OverlayConcatScript(files, durs)), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.mp4")
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("color=c=gray:s=%dx%d:r=25:d=3", w, h),
		"-f", "concat", "-safe", "0", "-i", script,
		"-filter_complex", "[1:v]format=rgba,fps=25[ov];[0:v][ov]overlay=0:0:eof_action=pass:format=auto,format=yuv420p[v]",
		"-map", "[v]", "-c:v", "libx264", "-preset", "veryfast", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg：%v %s", err, b)
	}
	pixel := func(at string) [3]byte {
		t.Helper()
		b, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-ss", at, "-i", out,
			"-frames:v", "1", "-vf", "crop=2:2:24:22", "-f", "rawvideo", "-pix_fmt", "rgb24", "-").Output()
		if err != nil || len(b) < 3 {
			t.Fatalf("取 %s 秒的像素：%v", at, err)
		}
		return [3]byte{b[0], b[1], b[2]}
	}
	isYellow := func(p [3]byte) bool { return p[0] > 200 && p[1] > 200 && p[2] < 60 }
	isGray := func(p [3]byte) bool { return p[0] > 100 && p[0] < 160 && p[2] > 100 && p[2] < 160 }
	for _, c := range []struct {
		at     string
		yellow bool
	}{{"0.5", false}, {"1.5", true}, {"2.2", false}, {"2.7", true}, {"2.96", true}} {
		p := pixel(c.at)
		if (c.yellow && !isYellow(p)) || (!c.yellow && !isGray(p)) {
			t.Fatalf("%s 秒：%v（期望%s）", c.at, p, map[bool]string{true: "出字", false: "透出底图"}[c.yellow])
		}
	}
	if !strings.HasSuffix(OverlayConcatScript(files, durs), "file '"+files[len(files)-1]+"'\n") {
		t.Fatal("末尾重复最后一个文件")
	}
}
