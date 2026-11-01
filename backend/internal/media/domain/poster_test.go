package domain

import (
	"image"
	"image/color"
	"reflect"
	"strings"
	"testing"
)

func solid(w, h int, c color.Color) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

// CD-83：成片常以淡出到黑场结尾，最后一帧是黑的就不该拿它当封面；但夜景不是黑场。
func TestMostlyBlack_黑场与夜景分得开(t *testing.T) {
	if !MostlyBlack(solid(64, 36, color.Black)) {
		t.Fatal("纯黑应判为黑场")
	}
	if !MostlyBlack(solid(64, 36, color.RGBA{R: 8, G: 8, B: 10, A: 255})) {
		t.Fatal("接近全黑（编码噪点）也应判为黑场")
	}
	if MostlyBlack(solid(64, 36, color.RGBA{R: 120, G: 90, B: 60, A: 255})) {
		t.Fatal("正常画面不是黑场")
	}
	// 夜景：大片暗部 + 一片亮的窗户（占画面一成多）
	night := solid(64, 36, color.RGBA{R: 6, G: 6, B: 12, A: 255})
	for y := 10; y < 22; y++ {
		for x := 20; x < 40; x++ {
			night.Set(x, y, color.RGBA{R: 250, G: 220, B: 140, A: 255})
		}
	}
	if MostlyBlack(night) {
		t.Fatal("夜景里有亮处，不该被当成黑场跳过")
	}
	if !MostlyBlack(image.NewRGBA(image.Rect(0, 0, 0, 0))) {
		t.Fatal("空图按黑场处理（截出来是空的就别用）")
	}
}

func TestFallbackPositions_时长未知不往前找(t *testing.T) {
	if got := FallbackPositions(0); got != nil {
		t.Fatalf("时长未知应不试，得到 %v", got)
	}
	if got := FallbackPositions(10_000); !reflect.DeepEqual(got, []float64{8, 6, 4}) {
		t.Fatalf("10 秒应依次试 8/6/4 秒，得到 %v", got)
	}
}

// 截最后一帧要从结尾前一段开始解码并覆盖输出 —— 只 seek 到结尾附近取一帧的话，
// 最后一个关键帧离结尾太远时一帧都解不出来，而 ffmpeg 的退出码照样是 0。
func TestLastFrameArgs_从结尾前一秒解码并覆盖输出(t *testing.T) {
	args := strings.Join(LastFrameArgs("/tmp/in.mp4", "/tmp/out.jpg"), " ")
	for _, want := range []string{"-sseof -1 -i /tmp/in.mp4", "-update 1", "-an"} {
		if !strings.Contains(args, want) {
			t.Fatalf("参数里缺「%s」：%s", want, args)
		}
	}
	if !strings.HasSuffix(args, "/tmp/out.jpg") {
		t.Fatalf("输出要在最后：%s", args)
	}
	at := strings.Join(FrameAtArgs("/tmp/in.mp4", "/tmp/out.jpg", 7.5), " ")
	if !strings.Contains(at, "-ss 7.50 -i /tmp/in.mp4") || !strings.Contains(at, "-frames:v 1") {
		t.Fatalf("定点截帧的 -ss 要在 -i 之前且只取一帧：%s", at)
	}
}

// 封面取第一帧：解出一帧就停；第一帧黑场时往后找，时长未知不找。
func TestFirstFrame_第一帧与淡入时往后找(t *testing.T) {
	args := strings.Join(FirstFrameArgs("/tmp/in.mp4", "/tmp/out.jpg"), " ")
	for _, want := range []string{"-i /tmp/in.mp4", "-frames:v 1", "-an"} {
		if !strings.Contains(args, want) {
			t.Fatalf("参数缺 %q：%s", want, args)
		}
	}
	if strings.Contains(args, "-sseof") {
		t.Fatalf("第一帧不该从结尾 seek：%s", args)
	}
	if got := FirstFrameFallbacks(0); got != nil {
		t.Fatalf("时长未知应不试，得到 %v", got)
	}
	if got := FirstFrameFallbacks(10_000); !reflect.DeepEqual(got, []float64{1, 2.5, 5}) {
		t.Fatalf("10 秒应依次试 1/2.5/5 秒，得到 %v", got)
	}
}
