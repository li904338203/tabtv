package app

import (
	"bytes"
	"context"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/media/domain"
)

// makeVideo 用 ffmpeg 自带的测试图源造一段 2 秒的视频；fadeOut 为真时最后 0.6 秒淡出到黑场。
func makeVideo(t *testing.T, dir string, fadeOut bool) string {
	t.Helper()
	if fadeOut {
		return makeVideoVF(t, dir, "fade=t=out:st=1.2:d=0.6")
	}
	return makeVideoVF(t, dir, "")
}

// makeVideoVF 同上，滤镜自己给（空串＝不加）。
func makeVideoVF(t *testing.T, dir, vf string) string {
	t.Helper()
	out := filepath.Join(dir, "in.mp4")
	args := []string{"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=320x180:rate=10:duration=2"}
	if vf != "" {
		args = append(args, "-vf", vf)
	}
	args = append(args, "-pix_fmt", "yuv420p", out)
	if msg, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("造测试视频失败：%v %s", err, msg)
	}
	return out
}

// 末帧（补镜的参考）取最后一帧；成片淡出到黑场时往前找，不拿一张黑图当参考。
//
// ⚠️ 真的跑 ffmpeg：参数拼对了没有（`-sseof` + `-update` 能不能留下最后一帧）只有跑了才知道。
func TestExtractLastFrame_取最后一帧且避开黑场(t *testing.T) {
	if !domain.FFmpegAvailable() {
		t.Skip("本机没有 ffmpeg")
	}
	ctx := context.Background()

	plainDir := t.TempDir()
	frame, err := extractLastFrame(ctx, plainDir, makeVideo(t, plainDir, false), 2000)
	if err != nil {
		t.Fatal(err)
	}
	img, err := jpeg.Decode(bytes.NewReader(frame))
	if err != nil {
		t.Fatalf("封面帧应是 JPEG：%v", err)
	}
	if b := img.Bounds(); b.Dx() != 320 || b.Dy() != 180 {
		t.Fatalf("封面帧应保持原画幅，得到 %v", b)
	}
	if domain.MostlyBlack(img) {
		t.Fatal("正常视频的最后一帧不该是黑场")
	}

	fadeDir := t.TempDir()
	faded := makeVideo(t, fadeDir, true)
	// 前提：这段视频的最后一帧确实是黑场 —— 不是的话下面那条断言什么都没验到
	last := filepath.Join(fadeDir, "probe.jpg")
	if err := runFFmpeg(ctx, domain.LastFrameArgs(faded, last)); err != nil {
		t.Fatal(err)
	}
	if _, lastImg, err := readFrame(last); err != nil || !domain.MostlyBlack(lastImg) {
		t.Fatalf("前提不成立：淡出视频的最后一帧应是黑场（%v）", err)
	}
	frame, err = extractLastFrame(ctx, fadeDir, faded, 2000)
	if err != nil {
		t.Fatal(err)
	}
	img, err = jpeg.Decode(bytes.NewReader(frame))
	if err != nil {
		t.Fatal(err)
	}
	if domain.MostlyBlack(img) {
		t.Fatal("淡出到黑场的成片应往前找一帧不黑的，而不是拿黑图当封面")
	}
}

// 一帧都没截出来（不是视频）要报错，不能把空文件当成封面帧存进去。
func TestExtractFrames_截不出画面要报错(t *testing.T) {
	if !domain.FFmpegAvailable() {
		t.Skip("本机没有 ffmpeg")
	}
	dir := t.TempDir()
	bogus := filepath.Join(dir, "in.mp4")
	if err := os.WriteFile(bogus, []byte("这不是视频"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := extractLastFrame(context.Background(), dir, bogus, 0); err == nil {
		t.Fatal("坏文件应截帧失败")
	}
	if _, err := extractFirstFrame(context.Background(), dir, bogus, 0); err == nil {
		t.Fatal("坏文件截第一帧也应失败")
	}
}

// 封面取第一帧（2026-10-30）：testsrc 的画面随时间变，第一帧与最后一帧不同；
// 淡入（开头是黑场）时往后找一帧不黑的。
func TestExtractFirstFrame_取第一帧且避开淡入黑场(t *testing.T) {
	if !domain.FFmpegAvailable() {
		t.Skip("本机没有 ffmpeg")
	}
	ctx := context.Background()

	dir := t.TempDir()
	src := makeVideo(t, dir, false)
	first, err := extractFirstFrame(ctx, dir, src, 2000)
	if err != nil {
		t.Fatal(err)
	}
	// 对照：直接截第 0 秒那一帧，应与封面完全一样（同一条 ffmpeg 解码路径）
	ref := filepath.Join(dir, "ref.jpg")
	if err := runFFmpeg(ctx, domain.FrameAtArgs(src, ref, 0)); err != nil {
		t.Fatal(err)
	}
	refData, _, err := readFrame(ref)
	if err != nil {
		t.Fatal(err)
	}
	last, err := extractLastFrame(ctx, dir, src, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, refData) {
		t.Fatal("封面应是第 0 秒那一帧")
	}
	if bytes.Equal(first, last) {
		t.Fatal("封面（第一帧）不该与末帧相同")
	}

	fadeDir := t.TempDir()
	faded := makeVideoVF(t, fadeDir, "fade=t=in:st=0:d=0.6")
	frame, err := extractFirstFrame(ctx, fadeDir, faded, 2000)
	if err != nil {
		t.Fatal(err)
	}
	img, err := jpeg.Decode(bytes.NewReader(frame))
	if err != nil {
		t.Fatal(err)
	}
	if domain.MostlyBlack(img) {
		t.Fatal("淡入的视频应往后找一帧不黑的当封面")
	}
}
