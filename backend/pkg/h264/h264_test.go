package h264

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// libx264 的映射必须与来源系统逐字一致：来源的 argv 断言测试与成片验收都建立在这几串参数上。
func TestEncodeArgs_libx264与来源一致(t *testing.T) {
	cases := []struct {
		in   Intent
		want string
	}{
		{Intent{Tier: TierFinal}, "-c:v libx264 -preset veryfast -pix_fmt yuv420p"},
		{Intent{Tier: TierIntermediate}, "-c:v libx264 -preset veryfast -crf 16 -pix_fmt yuv420p"},
		{Intent{Tier: TierCut}, "-c:v libx264 -preset veryfast -crf 18 -pix_fmt yuv420p"},
		{Intent{Tier: TierProxy, MaxRateK: 1200, BufSizeK: 2400, GOP: 25},
			"-c:v libx264 -preset veryfast -crf 28 -maxrate 1200k -bufsize 2400k -pix_fmt yuv420p -g 25 -keyint_min 25 -sc_threshold 0"},
	}
	for _, c := range cases {
		for _, enc := range []Encoder{{}, {Name: "libx264"}} {
			if got := strings.Join(EncodeArgs(enc, c.in), " "); got != c.want {
				t.Errorf("%+v %+v:\n got %s\nwant %s", enc, c.in, got, c.want)
			}
		}
	}
}

func TestEncodeArgs_每个编码器每档都给像素格式且不带x264专有参数(t *testing.T) {
	encoders := []string{"h264_videotoolbox", "h264_nvenc", "h264_qsv", "h264_amf", "h264_mf", "libopenh264"}
	tiers := []Tier{TierFinal, TierIntermediate, TierCut, TierProxy}
	for _, e := range encoders {
		for _, tier := range tiers {
			a := EncodeArgs(Encoder{Name: e}, Intent{Tier: tier, Width: 1920, Height: 1080, FPS: 30, GOP: 30})
			s := strings.Join(a, " ")
			if !strings.HasPrefix(s, "-c:v "+e+" ") {
				t.Errorf("%s: 编码器段不对：%s", e, s)
			}
			if strings.Contains(s, "-crf") || strings.Contains(s, "-sc_threshold") {
				t.Errorf("%s: 带了 x264 专有参数：%s", e, s)
			}
			if !strings.Contains(s, "-pix_fmt yuv420p") && !strings.Contains(s, "-pix_fmt nv12") {
				t.Errorf("%s: 没有统一像素格式：%s", e, s)
			}
			if !strings.Contains(s, "-g 30") {
				t.Errorf("%s: 固定关键帧间隔丢了：%s", e, s)
			}
		}
	}
}

func TestEncodeArgs_码率按画面估且受上限约束(t *testing.T) {
	hi := EncodeArgs(Encoder{Name: "libopenh264"}, Intent{Tier: TierIntermediate, Width: 1920, Height: 1080, FPS: 30})
	lo := EncodeArgs(Encoder{Name: "libopenh264"}, Intent{Tier: TierFinal, Width: 1920, Height: 1080, FPS: 30})
	if rate(hi) <= rate(lo) {
		t.Fatalf("中间文件码率应高于成片：%v %v", hi, lo)
	}
	px := EncodeArgs(Encoder{Name: "h264_videotoolbox"}, Intent{Tier: TierProxy, Width: 3840, Height: 2160, FPS: 60, MaxRateK: 1200, BufSizeK: 2400})
	if rate(px) != "1200k" || !strings.Contains(strings.Join(px, " "), "-maxrate 1200k -bufsize 2400k") {
		t.Fatalf("代理码率应夹到上限：%v", px)
	}
	// 未知尺寸按 1080p30 估，不给 0 码率。
	if r := rate(EncodeArgs(Encoder{Name: "h264_mf"}, Intent{Tier: TierFinal})); r == "" || r == "0k" {
		t.Fatalf("未知尺寸也要有码率：%q", r)
	}
}

func rate(a []string) string {
	for i := range a {
		if a[i] == "-b:v" && i+1 < len(a) {
			return a[i+1]
		}
	}
	return ""
}

const encodersFixture = `Encoders:
 V..... = Video
 ------
 V....D libopenh264          OpenH264 H.264 / AVC / MPEG-4 AVC / MPEG-4 part 10 (codec h264)
 V....D h264_nvenc           NVIDIA NVENC H.264 encoder (codec h264)
 V....D h264_videotoolbox    VideoToolbox H.264 Encoder (codec h264)
 A....D aac                  AAC (Advanced Audio Coding)
`

func TestParseEncoders(t *testing.T) {
	got := ParseEncoders(encodersFixture)
	for _, n := range []string{"libopenh264", "h264_nvenc", "h264_videotoolbox"} {
		if !got[n] {
			t.Errorf("缺 %s", n)
		}
	}
	if got["aac"] || got["Video"] {
		t.Errorf("音频编码器与表头不该算进来：%v", got)
	}
}

// 列出来不等于能用：nvenc 在没有 NVIDIA 显卡的机器上照样出现在 -encoders 里。
func TestDetect_试编码失败的跳过并回落openh264(t *testing.T) {
	run := func(_ context.Context, args ...string) (string, error) {
		if args[len(args)-1] != "-" {
			return encodersFixture, nil
		}
		if strings.Contains(strings.Join(args, " "), "h264_nvenc") {
			return "No capable devices found", errors.New("exit status 1")
		}
		return "", nil
	}
	sel := Detect(context.Background(), "linux", run)
	if sel.Encoder.Name != "libopenh264" {
		t.Fatalf("应回落 openh264：%+v", sel)
	}
	want := []Skipped{{"libx264", "当前 ffmpeg 没有编入"}, {"h264_nvenc", "试编码失败：No capable devices found"}}
	if !reflect.DeepEqual(sel.Skipped, want) {
		t.Fatalf("跳过原因：%+v", sel.Skipped)
	}
}

func TestDetect_一个都不可用(t *testing.T) {
	run := func(_ context.Context, args ...string) (string, error) {
		if args[len(args)-1] != "-" {
			return "Encoders:\n ------\n A....D aac   AAC\n", nil
		}
		return "", nil
	}
	if sel := Detect(context.Background(), "darwin", run); sel.Encoder.Name != "" || len(sel.Skipped) != 3 {
		t.Fatalf("不该选中任何编码器：%+v", sel)
	}
	failing := func(context.Context, ...string) (string, error) { return "", errors.New("not found") }
	if sel := Detect(context.Background(), "darwin", failing); sel.Encoder.Name != "" || sel.Skipped[0].Name != "ffmpeg" {
		t.Fatalf("ffmpeg 本身跑不起来：%+v", sel)
	}
}
