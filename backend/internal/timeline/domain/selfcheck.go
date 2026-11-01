package domain

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
)

// 导出后自检（docs/agent/smartcut SCT-15，SC-F-75）：时长、整片响度、黑场。
//
// ⚠️ **只警告，不判失败、不退款**：成片已经渲出来了，黑场可能正是用户要的（片头留黑），
// 响度偏一点也照样能看。判失败的话用户拿不到片子，还得再等一遍渲染。

// SelfCheck 是 render_job.self_check 的形状（契约 RenderJob.selfCheck）。
type SelfCheck struct {
	DurationMs   int        `json:"durationMs"`
	LoudnessLufs *float64   `json:"loudnessLufs,omitempty"`
	BlackSpans   []TimeSpan `json:"blackSpans"`
	Warnings     []string   `json:"warnings"`
}

// TimeSpan 一段时间（毫秒）。
type TimeSpan struct {
	StartMs int `json:"startMs"`
	EndMs   int `json:"endMs"`
}

// 自检的门槛（设计建议值）：黑场至少 1 秒才算；响度偏离目标超过 2 LU 才提醒（目标 -14 LUFS，SC-PARAM-27）。
const (
	SelfCheckBlackMinSec  = 1
	SelfCheckLoudnessDiff = 2.0
	SelfCheckTargetLufs   = -14.0
)

// SelfCheckArgs 一遍 ffmpeg 同时测响度（ebur128）与黑场（blackdetect）。没有声音时只测黑场
// （对着没有音轨的文件写 -af 会让 ffmpeg 报错）。
func SelfCheckArgs(path string, hasAudio bool) []string {
	args := []string{"-hide_banner", "-nostdin", "-nostats", "-i", path,
		"-vf", fmt.Sprintf("blackdetect=d=%d:pix_th=0.10", SelfCheckBlackMinSec)}
	if hasAudio {
		args = append(args, "-af", "ebur128=framelog=quiet")
	} else {
		args = append(args, "-an")
	}
	return append(args, "-f", "null", "-")
}

var (
	blackLine = regexp.MustCompile(`black_start:\s*([0-9.]+)\s+black_end:\s*([0-9.]+)`)
	// ebur128 的总结里那一行 `I:         -14.0 LUFS`（逐帧日志已用 framelog=quiet 关掉）。
	integratedLine = regexp.MustCompile(`(?m)^\s*I:\s+(-?[0-9.]+|-inf)\s+LUFS`)
)

// ParseSelfCheck 从 ffmpeg 的 stderr 里取黑场区间与整片响度（取最后一个 I: —— 总结在最后）。
func ParseSelfCheck(stderr []byte) (blacks []TimeSpan, lufs *float64) {
	for _, m := range blackLine.FindAllSubmatch(stderr, -1) {
		s, _ := strconv.ParseFloat(string(m[1]), 64)
		e, _ := strconv.ParseFloat(string(m[2]), 64)
		blacks = append(blacks, TimeSpan{StartMs: int(math.Round(s * 1000)), EndMs: int(math.Round(e * 1000))})
	}
	all := integratedLine.FindAllSubmatch(stderr, -1)
	if len(all) > 0 {
		if v, err := strconv.ParseFloat(string(all[len(all)-1][1]), 64); err == nil {
			lufs = &v
		}
	}
	return blacks, lufs
}

// BuildSelfCheck 汇总结论与提醒。wantMs 是快照的成片时长，fps 用来算「一帧」的容差。
func BuildSelfCheck(gotMs, wantMs, fps int, lufs *float64, blacks []TimeSpan) SelfCheck {
	c := SelfCheck{DurationMs: gotMs, LoudnessLufs: lufs, BlackSpans: blacks, Warnings: []string{}}
	if c.BlackSpans == nil {
		c.BlackSpans = []TimeSpan{}
	}
	frame := 1000 / max(fps, 1)
	if d := gotMs - wantMs; d > frame || d < -frame {
		c.Warnings = append(c.Warnings, fmt.Sprintf("成片时长 %.2f 秒，与剪辑的 %.2f 秒相差 %d 毫秒",
			float64(gotMs)/1000, float64(wantMs)/1000, d))
	}
	if lufs != nil && !math.IsInf(*lufs, 0) && math.Abs(*lufs-SelfCheckTargetLufs) > SelfCheckLoudnessDiff {
		c.Warnings = append(c.Warnings, fmt.Sprintf("整片响度 %.1f LUFS，偏离目标 %.0f LUFS", *lufs, SelfCheckTargetLufs))
	}
	for _, b := range blacks {
		c.Warnings = append(c.Warnings, fmt.Sprintf("检测到黑场：%.1f~%.1f 秒", float64(b.StartMs)/1000, float64(b.EndMs)/1000))
	}
	return c
}
