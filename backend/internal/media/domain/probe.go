package domain

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/li904338203/tabtv/backend/internal/platform/ffbin"
)

// 媒体时长探测（NEW-07 的前置：裁切区间不能超出素材本身）。
//
// ⚠️ 没有时长就没法校验裁切区间，而超出的后果是渲染出一段**黑屏**，
// 且任务是"成功"的 —— 与蒙版尺寸对不上那条是同一类问题：
// 错得很安静，只有看成片的人才发现。

// ProbeTimeout 单次探测的上限。
//
// ⚠️ 必须有：ffprobe 遇到畸形文件会一直读下去。上传链路上卡住一个
// 外部进程，表现是"上传转圈转到超时"，而谁也想不到是 ffprobe。
const ProbeTimeout = 10 * time.Second

// ProbeDuration 读出音视频时长（毫秒）。
//
// ⚠️ 探测失败一律返回 `(0,false)` 而**不是错误**：ffprobe 可能没装、
// 可能不认识这个封装格式。这不该让上传失败 —— 那个文件浏览器能播、
// 上游也能用，只是我们记不下时长。记不下的后果是裁切校验放行
// （见 CheckClipRange），比拒掉一个好文件轻得多。
//
// ⚠️ 只对音视频探测：图片没有时长，跑一次 ffprobe 纯属浪费
// （一次上传高峰就是几百次进程创建）。
func ProbeDuration(ctx context.Context, path, mime string) (ms int, ok bool) {
	switch kindOfMIME(mime) {
	case "audio", "video":
	default:
		return 0, false
	}
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()

	// ⚠️ 路径放在**最后**并用 `--` 隔开：以 `-` 开头的文件名会被 ffprobe
	// 当成选项解析。我们的临时文件名是自己生成的，但这条防线成本为零，
	// 而少了它就是一个「文件名即参数注入」的口子。
	out, err := exec.CommandContext(ctx, ffbin.FFprobe(),
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		"--", path).Output()
	if err != nil {
		return 0, false
	}
	sec, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || sec <= 0 {
		// 有些流式封装的 format.duration 是 "N/A"。解不出来就是不知道。
		return 0, false
	}
	return int(sec * 1000), true
}

func kindOfMIME(mime string) string {
	switch {
	case strings.HasPrefix(mime, "audio/"):
		return "audio"
	case strings.HasPrefix(mime, "video/"):
		return "video"
	case strings.HasPrefix(mime, "image/"):
		return "image"
	}
	return ""
}

// ProbeAvailable 说明本机有没有 ffprobe。
//
// 启动时查一次写进日志：没有它时时长一律未知，而"为什么所有素材都没有时长"
// 是个很难猜的问题 —— 日志里有一句就不用猜。
func ProbeAvailable() bool {
	_, err := exec.LookPath(ffbin.FFprobe())
	return err == nil
}
