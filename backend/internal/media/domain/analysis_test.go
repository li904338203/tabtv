package domain

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

func TestAnalysisEngine_扫描种类各有版本(t *testing.T) {
	seen := map[string]bool{}
	for _, k := range ScannerKinds {
		e := AnalysisEngine(k)
		if e == "" || seen[e] {
			t.Fatalf("%s 的引擎版本为空或重复：%q", k, e)
		}
		seen[e] = true
	}
	if AnalysisEngine(AnalysisBeats) != BeatsEngine {
		t.Fatal("节拍的引擎版本应与算法包一致")
	}
	// 识别与看图描述不归扫描器：没有扫描引擎版本，也不在领取范围里。
	for _, k := range []string{AnalysisASR, AnalysisCaption, "nope"} {
		if AnalysisEngine(k) != "" || slices.Contains(ScannerKinds, k) {
			t.Fatalf("%s 不该由扫描器处理", k)
		}
	}
}

func TestAnalysisApplies(t *testing.T) {
	cases := []struct {
		kind  string
		video bool
		audio bool
	}{
		{AnalysisPreviewProxy, true, false},
		{AnalysisFilmstrip, true, false},
		{AnalysisPeaks, true, true},
		{AnalysisSpeechAudio, true, true},
		{AnalysisBeats, false, true},
		{AnalysisASR, false, false},
	}
	for _, c := range cases {
		if AnalysisApplies(c.kind, "video") != c.video || AnalysisApplies(c.kind, "audio") != c.audio ||
			AnalysisApplies(c.kind, "image") {
			t.Errorf("%s：视频 %v 音频 %v、图片一律不做", c.kind, c.video, c.audio)
		}
	}
}

func TestAnalysisTimeout与重试间隔(t *testing.T) {
	cases := []struct {
		ms   int
		want time.Duration
	}{
		{0, 30 * time.Minute},
		{-5, 30 * time.Minute},
		{10_000, 10 * time.Minute},
		{10 * 60_000, 30 * time.Minute},
		{3 * 3_600_000, 3 * time.Hour},
	}
	for _, c := range cases {
		if got := AnalysisTimeout(c.ms); got != c.want {
			t.Errorf("时长 %dms：%v，应为 %v", c.ms, got, c.want)
		}
	}
	if AnalysisRetryAfter(0) != time.Minute || AnalysisRetryAfter(2) != 2*time.Minute {
		t.Fatal("第 n 次失败后 n 分钟再试")
	}
}

func TestParseProbe(t *testing.T) {
	cases := []struct {
		name string
		json string
		want StreamInfo
	}{
		{"横屏带音轨",
			`{"streams":[{"codec_type":"video","width":1920,"height":1080,"avg_frame_rate":"30000/1001"},
			 {"codec_type":"audio","channels":1},{"codec_type":"audio","channels":6}],"format":{"duration":"12.0215"}}`,
			StreamInfo{HasVideo: true, HasAudio: true, Width: 1920, Height: 1080, FPS: 30000.0 / 1001, DurationMs: 12022,
				AudioChannels: 1}},
		{"旋转 90 度按显示尺寸",
			`{"streams":[{"codec_type":"video","width":1280,"height":720,"avg_frame_rate":"30/1",
			 "side_data_list":[{"rotation":-90}]}],"format":{"duration":"5"}}`,
			StreamInfo{HasVideo: true, Width: 720, Height: 1280, FPS: 30, DurationMs: 5000}},
		{"旧式 rotate 标签",
			`{"streams":[{"codec_type":"video","width":1280,"height":720,"avg_frame_rate":"25/1",
			 "tags":{"rotate":"270"}}],"format":{"duration":"1"}}`,
			StreamInfo{HasVideo: true, Width: 720, Height: 1280, FPS: 25, DurationMs: 1000}},
		{"旋转 180 不换宽高",
			`{"streams":[{"codec_type":"video","width":1280,"height":720,"avg_frame_rate":"25/1",
			 "side_data_list":[{"rotation":180}]}],"format":{}}`,
			StreamInfo{HasVideo: true, Width: 1280, Height: 720, FPS: 25}},
		{"MP3 的专辑封面不算视频",
			`{"streams":[{"codec_type":"audio"},{"codec_type":"video","width":600,"height":600,
			 "disposition":{"attached_pic":1}}],"format":{"duration":"9.0"}}`,
			StreamInfo{HasAudio: true, DurationMs: 9000}},
		{"平均帧率未知时用 r_frame_rate、时长 N/A",
			`{"streams":[{"codec_type":"video","width":640,"height":360,"avg_frame_rate":"0/0",
			 "r_frame_rate":"24/1"}],"format":{"duration":"N/A"}}`,
			StreamInfo{HasVideo: true, Width: 640, Height: 360, FPS: 24}},
		{"只取第一条视频流",
			`{"streams":[{"codec_type":"video","width":640,"height":360},{"codec_type":"video","width":1920,"height":1080}],
			 "format":{}}`,
			StreamInfo{HasVideo: true, Width: 640, Height: 360}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseProbe([]byte(c.json))
			if err != nil || got != c.want {
				t.Fatalf("%+v %v，应为 %+v", got, err, c.want)
			}
		})
	}
	if _, err := ParseProbe([]byte("not json")); err == nil {
		t.Fatal("坏 JSON 要报错")
	}
}

func TestKeyframeInterval(t *testing.T) {
	cases := map[float64]int{0: 30, -1: 30, 29.97: 30, 23.976: 24, 60: 60, 0.2: 1, 1000: 120}
	for fps, want := range cases {
		if got := KeyframeInterval(fps); got != want {
			t.Errorf("fps %v：%d，应为 %d", fps, got, want)
		}
	}
}

// 参数形状：-g 与关键帧最小间隔都等于帧率、音轨可选、moov 前置、输入输出在固定位置。
func TestProxyArgs(t *testing.T) {
	args := ProxyArgs("/tmp/in.mov", "/tmp/out.mp4", 25)
	joined := strings.Join(args, " ")
	for _, want := range []string{"-i /tmp/in.mov", "-map 0:a:0?", "-vf " + ProxyScaleFilter,
		"-g 25 -keyint_min 25 -sc_threshold 0", "-b:a 64k", "-movflags +faststart", "-pix_fmt yuv420p"} {
		if !strings.Contains(joined, want) {
			t.Errorf("缺少 %q：%s", want, joined)
		}
	}
	if args[len(args)-1] != "/tmp/out.mp4" {
		t.Fatal("输出放最后")
	}
	if !strings.Contains(ProxyScaleFilter, "min(480,trunc(iw/2)*2)") || !strings.Contains(ProxyScaleFilter, "min(480,trunc(ih/2)*2)") {
		t.Fatal("短边不超过 480、取偶数")
	}
}

func TestFilmstrip版式(t *testing.T) {
	cases := []struct {
		count  int
		cols   int
		sheets []int
	}{
		{0, 0, nil},
		{1, 1, []int{1}},
		{9, 9, []int{9}},
		{10, 10, []int{10}},
		{100, 10, []int{100}},
		{101, 10, []int{100, 1}},
		{250, 10, []int{100, 100, 50}},
	}
	for _, c := range cases {
		if got := FilmstripColsFor(c.count); got != c.cols {
			t.Errorf("%d 帧的列数 %d，应为 %d", c.count, got, c.cols)
		}
		if got := FilmstripSheets(c.count); !slices.Equal(got, c.sheets) {
			t.Errorf("%d 帧的分张 %v，应为 %v", c.count, got, c.sheets)
		}
	}
	args := strings.Join(FilmstripFramesArgs("in.mp4", "f-%06d.jpg"), " ")
	if !strings.Contains(args, "-vf fps=1,scale=160:-2") || !strings.HasSuffix(args, "f-%06d.jpg") {
		t.Fatalf("每秒一帧、宽 160：%s", args)
	}
}

func TestComposeSheet(t *testing.T) {
	solid := func(c color.Color, w, h int) image.Image {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				img.Set(x, y, c)
			}
		}
		return img
	}
	red, green, blue := color.RGBA{255, 0, 0, 255}, color.RGBA{0, 255, 0, 255}, color.RGBA{0, 0, 255, 255}
	// 第三帧比格子大：多出来的裁掉，不能画进别的格子。
	sheet := ComposeSheet([]image.Image{solid(red, 4, 3), solid(green, 4, 3), solid(blue, 6, 5)}, 2, 4, 3)
	if b := sheet.Bounds(); b.Dx() != 8 || b.Dy() != 6 {
		t.Fatalf("2 列 × 2 行（最后一行不补满）：%v", b)
	}
	at := func(x, y int) color.RGBA { return sheet.RGBAAt(x, y) }
	if at(1, 1) != red || at(5, 1) != green || at(1, 4) != blue || at(3, 5) != blue {
		t.Fatal("帧按行优先落在各自的格子里")
	}
	if at(5, 4) != (color.RGBA{}) {
		t.Fatal("第四格没有帧，应保持透明")
	}
	if s := ComposeSheet(nil, 10, 160, 90); s.Bounds().Dx() != 0 {
		t.Fatal("没有帧时为空图")
	}
}

func TestQuantizePeak(t *testing.T) {
	cases := map[int]byte{-3: 0, 0: 0, 64: 0, 65: 1, 128: 1, 16384: 128, 32767: 255, 32768: 255, 40000: 255}
	for p, want := range cases {
		if got := QuantizePeak(p); got != want {
			t.Errorf("峰值 %d → %d，应为 %d", p, got, want)
		}
	}
}

func pcm16(samples ...int16) []byte {
	buf := make([]byte, 2*len(samples))
	for i, s := range samples {
		binary.LittleEndian.PutUint16(buf[2*i:], uint16(s))
	}
	return buf
}

func TestPeaksFromPCM(t *testing.T) {
	// 两个完整窗口 + 半个窗口：第一窗峰值来自负样本，第二窗为静音，第三窗不足 80 个样本也算一个。
	s := make([]int16, 200)
	s[10], s[40] = 1000, -32768
	s[170] = 16384
	data := pcm16(s...)
	want := []byte{255, 0, 128}

	got, err := PeaksFromPCM(bytes.NewReader(data))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%v %v，应为 %v", got, err, want)
	}
	// 一次只给一个字节：样本跨读取边界时照样拼得对。
	got, err = PeaksFromPCM(iotest.OneByteReader(bytes.NewReader(data)))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("逐字节读：%v %v", got, err)
	}
	// 末尾不足 2 字节的残片丢弃。
	got, _ = PeaksFromPCM(bytes.NewReader(append(pcm16(make([]int16, 80)...), 0x7f)))
	if !bytes.Equal(got, []byte{0}) {
		t.Fatalf("残片应丢弃：%v", got)
	}
	// 空输入是空数组而不是 nil（结果要序列化成 ""，不是 null）。
	got, err = PeaksFromPCM(bytes.NewReader(nil))
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("空输入：%v %v", got, err)
	}
	boom := errors.New("读失败")
	if _, err := PeaksFromPCM(iotest.ErrReader(boom)); !errors.Is(err, boom) {
		t.Fatalf("读错误原样返回：%v", err)
	}
}

func TestStderrTail(t *testing.T) {
	if StderrTail("  short\n", 300) != "short" {
		t.Fatal("短的原样（去首尾空白）")
	}
	s := strings.Repeat("a", 10) + "错误在这里"
	got := StderrTail(s, 10)
	if !strings.HasSuffix(got, "这里") || strings.ContainsRune(got, '�') || len(got) > 10 {
		t.Fatalf("不能从汉字中间切开：%q", got)
	}
}

// 用真的 ffmpeg 核对缩放表达式（本机没有 ffmpeg 时跳过）：横屏、竖屏、旋转、奇数尺寸、本来就小的。
// 表达式写错不报编译错误，只会在第一个用户上传视频时让整条预览链失败 —— 所以要真跑一次。
func TestProxyArgs_真实ffmpeg(t *testing.T) {
	if !FFmpegAvailable() || !ProbeAvailable() {
		t.Skip("本机没有 ffmpeg / ffprobe")
	}
	dir := t.TempDir()
	cases := []struct {
		name         string
		size         string
		rotate       string
		wantW, wantH int
	}{
		{"横屏", "1280x720", "", 854, 480},
		{"竖屏", "720x1280", "", 480, 854},
		{"带旋转的横向编码", "1280x720", "90", 480, 854},
		{"奇数尺寸不放大", "459x301", "", 458, 300},
		{"本来就小", "320x180", "", 320, 180},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := filepath.Join(dir, c.name+".mp4")
			gen := []string{"-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
				"-i", "testsrc2=size=" + c.size + ":rate=25", "-t", "2", "-c:v", "libx264", "-pix_fmt", "yuv420p", src}
			if out, err := exec.Command("ffmpeg", gen...).CombinedOutput(); err != nil {
				t.Fatalf("造素材失败：%v %s", err, out)
			}
			if c.rotate != "" {
				rot := filepath.Join(dir, c.name+"-rot.mp4")
				if out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
					"-display_rotation", c.rotate, "-i", src, "-c", "copy", rot).CombinedOutput(); err != nil {
					t.Fatalf("加旋转失败：%v %s", err, out)
				}
				src = rot
			}
			in, err := ProbeStreams(context.Background(), src)
			if err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(dir, c.name+"-proxy.mp4")
			if out, err := exec.Command("ffmpeg", ProxyArgs(src, dst, in.FPS)...).CombinedOutput(); err != nil {
				t.Fatalf("转预览版失败：%v %s", err, out)
			}
			got, err := ProbeStreams(context.Background(), dst)
			if err != nil || got.Width != c.wantW || got.Height != c.wantH || got.HasAudio {
				t.Fatalf("预览版 %+v %v，应为 %dx%d、没有音轨", got, err, c.wantW, c.wantH)
			}
		})
	}
}
