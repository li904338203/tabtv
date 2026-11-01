package domain

import (
	"image"
	"os/exec"
	"strconv"
	"time"
)

// 视频的封面帧与末帧（CD-83）。
//
// 项目卡片的封面只能是图片：列表里每张卡片都把整段视频取下来放进 <video> 的话，
// 一屏几十个项目就是几百 MB 的内存。所以给视频派生一张静态的封面帧。
//
// ⚠️ 封面取**第一帧**（2026-10-30 起，原先取最后一帧）：用户在资产库、节点卡上认一段视频
// 靠的是「它从哪儿开始」，结尾那一帧常是转场中途或淡出，认不出来。第一帧是黑场（淡入）就往后找，
// 全是黑场才如实用第一帧（编一张不存在的画面更糟）。
//
// 末帧另存一张（`video_lastframe`）：智能剪辑「补镜」要的是上一段**结束时**的画面当参考图，
// 拿第一帧去接会让补出来的镜头从上一段的开头重新开始。两者用途不同，不能共用一张。

// PosterTimeout 单次截帧的上限。
//
// ⚠️ 必须有：畸形文件会让 ffmpeg 一直读下去，而截帧跑在 Worker 的扫描里 ——
// 卡住一个外部进程，后面所有视频的封面都排着等它。
const PosterTimeout = 30 * time.Second

// LastFrameArgs 截最后一帧：从结尾前 1 秒开始解码，每解出一帧就覆盖输出，留下的就是最后一帧。
//
// ⚠️ 不用 `-sseof -0.1 -frames:v 1`：最后一个关键帧常离结尾好几秒，seek 到 0.1 秒之内
// 可能一帧都解不出来 —— ffmpeg 什么都不写而退出码是 0，看起来像「截成功了」。
// 输入输出都是我们自己建的临时文件的绝对路径，不会被当成选项解析。
func LastFrameArgs(input, output string) []string {
	return []string{"-hide_banner", "-loglevel", "error", "-y",
		"-sseof", "-1", "-i", input,
		"-an", "-update", "1", "-q:v", "3", output}
}

// FirstFrameArgs 截第一帧（`-frames:v 1`：解出一帧就停，不读整段）。
func FirstFrameArgs(input, output string) []string {
	return []string{"-hide_banner", "-loglevel", "error", "-y",
		"-i", input, "-an", "-frames:v", "1", "-q:v", "3", output}
}

// FrameAtArgs 截第 atSec 秒那一帧（`-ss` 放在 `-i` 前面：按关键帧快速定位，不从头解码）。
func FrameAtArgs(input, output string, atSec float64) []string {
	return []string{"-hide_banner", "-loglevel", "error", "-y",
		"-ss", strconv.FormatFloat(atSec, 'f', 2, 64), "-i", input,
		"-an", "-frames:v", "1", "-q:v", "3", output}
}

// FirstFrameFallbacks 是第一帧为黑场（淡入）时依次往后去试的位置（秒）。
//
// 时长未知时不试 —— 不知道多长就不知道往后试到哪里不会越界，留着第一帧。
func FirstFrameFallbacks(durationMs int) []float64 {
	if durationMs <= 0 {
		return nil
	}
	sec := float64(durationMs) / 1000
	return []float64{sec * 0.1, sec * 0.25, sec * 0.5}
}

// FallbackPositions 是最后一帧为黑场时依次去试的位置（秒）。
//
// 时长未知时不试 —— 没有时长就不知道「往前」是哪里，留着最后一帧。
func FallbackPositions(durationMs int) []float64 {
	if durationMs <= 0 {
		return nil
	}
	sec := float64(durationMs) / 1000
	return []float64{sec * 0.8, sec * 0.6, sec * 0.4}
}

// MostlyBlack 判断一帧是不是黑场：均匀取 32×32 个点，平均亮度很低**且**几乎没有亮点。
//
// ⚠️ 两个条件缺一不可：只看平均亮度的话，夜景（大片暗部里几盏灯）会被当成黑场跳过，
// 用户的封面莫名其妙变成了视频中段的另一个画面。
func MostlyBlack(img image.Image) bool {
	b := img.Bounds()
	if b.Empty() {
		return true
	}
	const grid = 32
	var sum, bright, n int
	for i := 0; i < grid; i++ {
		for j := 0; j < grid; j++ {
			x := b.Min.X + (b.Dx()-1)*i/(grid-1)
			y := b.Min.Y + (b.Dy()-1)*j/(grid-1)
			r, g, bl, _ := img.At(x, y).RGBA()
			luma := (299*int(r>>8) + 587*int(g>>8) + 114*int(bl>>8)) / 1000
			sum += luma
			if luma > 40 {
				bright++
			}
			n++
		}
	}
	return sum/n < 16 && bright*100 < n*2
}

// FFmpegAvailable 说明本机有没有 ffmpeg（截帧要它）。
//
// 启动时查一次写进日志：没有它时所有视频都没有封面帧，而「为什么视频项目一直是占位图」
// 是个很难猜的问题 —— 日志里有一句就不用猜。
func FFmpegAvailable() bool {
	_, err := exec.LookPath("ffmpeg")
	return err == nil
}
