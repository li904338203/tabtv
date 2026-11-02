package domain

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 源素材一律用 ffmpeg 自带的 mpeg4 编码器造（TT-T-27）：随包的 LGPL 构建没有 libx264，
// 造素材这一步不该决定测试能不能跑。被测的编码器由 TABTV_TEST_H264 选（见 planOf）。
//
// 用真的 ffmpeg 量产物（SCT-11）：argv 断言只能证明命令长这样，证明不了片子对 ——
// 这一域的错几乎都是「渲染成功、片子不对」。本机没有 ffmpeg 时跳过。

// realPlanOf 是真实 ffmpeg 测试用的 planOf：TABTV_TEST_H264=h264_videotoolbox / libopenh264 等时用那个编码器
// 再跑一遍（TT-T-27）。滤镜图与编码器无关，换编码器后量产物的断言必须照样成立。
// ⚠️ 只给真实 ffmpeg 的测试用：argv 断言测试钉的是来源系统的 libx264 参数，不能跟着环境变量变。
func realPlanOf(t *testing.T, snap RenderSnapshot, io RenderIO) RenderPlan {
	t.Helper()
	if io.Encoder.Name == "" {
		io.Encoder.Name = os.Getenv("TABTV_TEST_H264")
	}
	return planOf(t, snap, io)
}

func needFFmpeg(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("本机没有 ffmpeg")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("本机没有 ffprobe")
	}
	return t.TempDir()
}

func ff(t *testing.T, dir string, args ...string) {
	t.Helper()
	// ⚠️ 带超时：滤镜图写错时 ffmpeg 可能空转而不报错（变换段的帧率那次就是），不设上限的话整个测试卡到 go test 的 10 分钟。
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %v：%v\n%s", args, err, out)
	}
}

// runPlan 照 runner 的做法跑一份计划：先写分块的 concat 脚本、逐块渲染，再跑收尾那一遍。
func runPlan(t *testing.T, dir string, plan RenderPlan) {
	t.Helper()
	for _, c := range plan.Chunks {
		ff(t, dir, c.Args[3:]...) // 去掉 -hide_banner -nostdin -y（ff 自己加）
	}
	if plan.ConcatList != "" {
		if err := os.WriteFile(plan.ConcatList, []byte(plan.ConcatListBody), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ff(t, dir, plan.Args[3:]...)
}

func durationMs(t *testing.T, path string) int {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	f, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	return int(f * 1000)
}

// pixel 取第 atSec 秒那一帧 (x, y) 处的 RGB。
func pixel(t *testing.T, path string, atSec float64, x, y int) [3]int {
	t.Helper()
	out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-ss", fmt.Sprintf("%.3f", atSec), "-i", path,
		"-frames:v", "1", "-vf", fmt.Sprintf("crop=2:2:%d:%d", x, y), "-f", "rawvideo", "-pix_fmt", "rgb24", "-").Output()
	if err != nil || len(out) < 3 {
		t.Fatalf("取帧失败：%v %d", err, len(out))
	}
	return [3]int{int(out[0]), int(out[1]), int(out[2])}
}

func near(p [3]int, r, g, b int) bool {
	d := func(a, b int) int {
		if a > b {
			return a - b
		}
		return b - a
	}
	return d(p[0], r) < 40 && d(p[1], g) < 40 && d(p[2], b) < 40
}

func smallSnap(clips ...SnapClip) RenderSnapshot {
	return RenderSnapshot{Schema: SnapshotSchema, Width: 320, Height: 180, FPS: 25, Clips: clips, VideoVolume: 100}
}

// SC-AC-14（SCB-1）：图片 3 秒 + 视频 2 秒 → 成片 5 秒（误差不超过一帧），前 3 秒是那张图。
func TestRender_ffmpeg_图片段时长(t *testing.T) {
	dir := needFFmpeg(t)
	ff(t, dir, "-f", "lavfi", "-i", "color=c=red:s=640x360", "-frames:v", "1", "red.png")
	ff(t, dir, "-f", "lavfi", "-i", "color=c=blue:s=640x360:r=25:d=4", "-c:v", "mpeg4", "-q:v", "2", "-pix_fmt", "yuv420p", "blue.mp4")
	img := SnapClip{MediaID: 1, Kind: "image", SourceOutMs: 3000, DurationMs: 3000, Speed: 100, Volume: 100}
	vid := SnapClip{MediaID: 2, Kind: "video", SourceInMs: 1000, SourceOutMs: 3000, DurationMs: 2000, Speed: 100, Volume: 100}
	out := filepath.Join(dir, "out.mp4")
	runPlan(t, dir, realPlanOf(t, smallSnap(img, vid), RenderIO{OutPath: out, WorkDir: dir, Files: map[int64]MediaFile{
		1: {Path: filepath.Join(dir, "red.png"), HasVideo: true}, 2: {Path: filepath.Join(dir, "blue.mp4"), HasVideo: true}}}))
	if d := durationMs(t, out); d < 5000-40 || d > 5000+40 {
		t.Fatalf("成片应为 5 秒：%dms", d)
	}
	if p := pixel(t, out, 1.5, 160, 90); !near(p, 255, 0, 0) {
		t.Fatalf("前 3 秒是图片（红）：%v", p)
	}
	if p := pixel(t, out, 4.0, 160, 90); !near(p, 0, 0, 255) {
		t.Fatalf("后 2 秒是视频（蓝）：%v", p)
	}
}

// SC-AC-15：缩放 50%、移到左上角（x=-25, y=-25）的一段：画面占左上四分之一，其余是黑边。
// 最后一段淡出到黑场：最后一帧是黑的（SCB-7 转出）。
func TestRender_ffmpeg_画面变换与淡出(t *testing.T) {
	dir := needFFmpeg(t)
	ff(t, dir, "-f", "lavfi", "-i", "color=c=green:s=640x360:r=25:d=3", "-c:v", "mpeg4", "-q:v", "2", "-pix_fmt", "yuv420p", "g.mp4")
	c := SnapClip{MediaID: 1, Kind: "video", SourceOutMs: 2000, DurationMs: 2000, Speed: 100, Volume: 100,
		Transform:     &Transform{X: -25, Y: -25, Scale: 0.5},
		TransitionOut: &Transition{Kind: TransitionFade, DurationMs: 600}}
	out := filepath.Join(dir, "out.mp4")
	runPlan(t, dir, realPlanOf(t, smallSnap(c), RenderIO{OutPath: out, WorkDir: dir,
		Files: map[int64]MediaFile{1: {Path: filepath.Join(dir, "g.mp4"), HasVideo: true}}}))
	if p := pixel(t, out, 0.5, 40, 22); !near(p, 0, 128, 0) {
		t.Fatalf("左上四分之一是画面（绿）：%v", p)
	}
	for _, xy := range [][2]int{{240, 135}, {240, 22}, {40, 135}} {
		if p := pixel(t, out, 0.5, xy[0], xy[1]); !near(p, 0, 0, 0) {
			t.Fatalf("其余三块是黑边 %v：%v", xy, p)
		}
	}
	if p := pixel(t, out, 1.96, 40, 22); !near(p, 0, 0, 0) {
		t.Fatalf("最后一帧淡出到黑：%v", p)
	}
}

// 变换段的素材帧率与画布不同（30fps 的素材、25fps 的画布）：渲染要能结束、帧率是画布的（SCT-44 量产物时撞上的空转）。
func TestRender_ffmpeg_变换段帧率与画布不同(t *testing.T) {
	dir := needFFmpeg(t)
	ff(t, dir, "-f", "lavfi", "-i", "color=c=green:s=640x360:r=30:d=2", "-c:v", "mpeg4", "-q:v", "2", "-pix_fmt", "yuv420p", "g30.mp4")
	c := SnapClip{MediaID: 1, Kind: "video", SourceOutMs: 2000, DurationMs: 2000, Speed: 100, Volume: 100,
		Transform: &Transform{X: -25, Y: -25, Scale: 0.5}}
	out := filepath.Join(dir, "out.mp4")
	start := time.Now()
	runPlan(t, dir, realPlanOf(t, smallSnap(c), RenderIO{OutPath: out, WorkDir: dir,
		Files: map[int64]MediaFile{1: {Path: filepath.Join(dir, "g30.mp4"), HasVideo: true}}}))
	if time.Since(start) > time.Minute {
		t.Fatalf("2 秒的片子渲了 %v", time.Since(start))
	}
	if d := durationMs(t, out); d < 2000-40 || d > 2000+40 {
		t.Fatalf("成片应为 2 秒：%dms", d)
	}
	if p := pixel(t, out, 0.5, 40, 22); !near(p, 0, 128, 0) {
		t.Fatalf("左上四分之一是画面（绿）：%v", p)
	}
}

// 第一段带转入：从黑场淡入 —— 第一帧是黑的，淡入结束之后是画面本身。
func TestRender_ffmpeg_第一段从黑场淡入(t *testing.T) {
	dir := needFFmpeg(t)
	ff(t, dir, "-f", "lavfi", "-i", "color=c=green:s=640x360:r=25:d=3", "-c:v", "mpeg4", "-q:v", "2", "-pix_fmt", "yuv420p", "g.mp4")
	c := SnapClip{MediaID: 1, Kind: "video", SourceOutMs: 2000, DurationMs: 2000, Speed: 100, Volume: 100,
		Transition: Transition{Kind: TransitionFade, DurationMs: 800}}
	out := filepath.Join(dir, "out.mp4")
	runPlan(t, dir, realPlanOf(t, smallSnap(c), RenderIO{OutPath: out, WorkDir: dir,
		Files: map[int64]MediaFile{1: {Path: filepath.Join(dir, "g.mp4"), HasVideo: true}}}))
	if p := pixel(t, out, 0, 160, 90); !near(p, 0, 0, 0) {
		t.Fatalf("第一帧是黑的：%v", p)
	}
	if p := pixel(t, out, 1.5, 160, 90); !near(p, 0, 128, 0) {
		t.Fatalf("淡入之后是画面（绿）：%v", p)
	}
}

// 70 段的时间线分块渲染：成片时长等于各段之和（误差不超过两帧：两块之间拼接处各取整一次）。
func TestRender_ffmpeg_分块渲染(t *testing.T) {
	dir := needFFmpeg(t)
	ff(t, dir, "-f", "lavfi", "-i", "color=c=red:s=160x90:r=25:d=2", "-c:v", "mpeg4", "-q:v", "2", "-pix_fmt", "yuv420p", "a.mp4")
	ff(t, dir, "-f", "lavfi", "-i", "color=c=blue:s=160x90:r=25:d=2", "-c:v", "mpeg4", "-q:v", "2", "-pix_fmt", "yuv420p", "b.mp4")
	clips := make([]SnapClip, 70)
	for i := range clips {
		clips[i] = SnapClip{MediaID: int64(i%2 + 1), Kind: "video", SourceOutMs: 400, DurationMs: 400, Speed: 100, Volume: 100}
	}
	clips[65].Transition = Transition{Kind: TransitionFade, DurationMs: 200} // 分块边界之外的转场照常
	out := filepath.Join(dir, "out.mp4")
	plan := realPlanOf(t, smallSnap(clips...), RenderIO{OutPath: out, WorkDir: dir, Files: map[int64]MediaFile{
		1: {Path: filepath.Join(dir, "a.mp4"), HasVideo: true}, 2: {Path: filepath.Join(dir, "b.mp4"), HasVideo: true}}})
	if len(plan.Chunks) != 2 {
		t.Fatalf("应分两块：%d", len(plan.Chunks))
	}
	runPlan(t, dir, plan)
	want := SnapDuration(smallSnap(clips...))
	if d := durationMs(t, out); d < want-80 || d > want+80 {
		t.Fatalf("成片 %dms，应为 %dms", d, want)
	}
}
