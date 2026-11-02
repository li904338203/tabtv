// Package h264 是 H.264 编码器的选择与参数映射（dev/04 TTD-51，TT-T-27）。
//
// 只依赖标准库、不执行任何命令（探测用的 Runner 由调用方注入）：timeline / media 的 domain 层
// 要用它拼 argv，而那两层的纯度测试不许依赖 internal 下的任何包。执行与缓存在 internal/platform/ffbin。
package h264

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// H.264 编码器的探测与参数映射（dev/04 TTD-51，TT-T-27）。
//
// 随包的 ffmpeg 是 LGPL 构建，**没有 libx264**。来源系统 4 处写死了 `-c:v libx264 -preset -crf`，
// 原样搬过来的话每一次渲染、裁切、剪辑代理都会报「Unknown encoder 'libx264'」—— 而那是在用户已经
// 排了很久的队之后。所以启动时探测一次本机能用哪个编码器，各处按「质量档」要参数，不再自己写编码器名。
//
// ⚠️ 只换编码器段，**滤镜图一律不动**：来源系统那些「渲染成功但片子不对」的规则
// （setsar、atempo 串联、xfade offset、amix duration=first…）全在滤镜段，与编码器无关。

// Encoder 是选中的 H.264 编码器。零值 = libx264（测试与「用户自己指定了带 x264 的 ffmpeg」时）。
type Encoder struct {
	Name string
}

// Tier 是质量档：调用方只说「要多好」，不说「用什么参数」。
type Tier int

const (
	// TierFinal 成片收尾那一遍（来源：libx264 默认 crf）。
	TierFinal Tier = iota
	// TierIntermediate 分块渲染的中间文件：还要再编码一次，给得高一些免得两代压缩糊掉（来源 crf 16）。
	TierIntermediate
	// TierCut 上游产物裁切（来源 crf 18）。
	TierCut
	// TierProxy 智能剪辑的预览代理（来源 crf 28 + 码率上限）。
	TierProxy
)

// Intent 是一次编码的要求。
type Intent struct {
	Tier Tier
	// Width / Height / FPS 用来给按码率控制的编码器估一个码率；未知时按 1080p30 估。
	Width, Height int
	FPS           float64
	// MaxRateK / BufSizeK 码率上限（kbit/s）；0 = 不限。
	MaxRateK, BufSizeK int
	// GOP 固定关键帧间隔（帧）；0 = 编码器默认。设了就不让场景切换额外插关键帧。
	GOP int
}

// 各档的 x264 crf（与来源系统逐字一致；0 = 不写 crf，用 x264 默认值）。
var x264CRF = map[Tier]int{TierFinal: 0, TierIntermediate: 16, TierCut: 18, TierProxy: 28}

// 按码率控制的编码器用的「每像素每帧比特数」，按 x264 在 veryfast 下对应 crf 的大致码率取。
// ⚠️ 硬件编码器的码率控制与 crf 不等价（TTR-6），这里只求结构性指标对、画质不明显变差。
var bitsPerPixel = map[Tier]float64{TierFinal: 0.10, TierIntermediate: 0.16, TierCut: 0.13, TierProxy: 0.05}

// 各档给 nvenc/qsv/amf 的恒定质量值（数值越小越好，量纲与 crf 接近）。
var hwQuality = map[Tier]int{TierFinal: 23, TierIntermediate: 17, TierCut: 19, TierProxy: 30}

func (in Intent) bitrateK() int {
	w, h, fps := in.Width, in.Height, in.FPS
	if w <= 0 || h <= 0 {
		w, h = 1920, 1080
	}
	if fps <= 0 {
		fps = 30
	}
	k := int(float64(w*h)*fps*bitsPerPixel[in.Tier]/1000 + 0.5)
	if k < 300 {
		k = 300
	}
	if in.MaxRateK > 0 && k > in.MaxRateK {
		k = in.MaxRateK
	}
	return k
}

func kbps(k int) string { return strconv.Itoa(k) + "k" }

// EncodeArgs 返回视频编码段的参数（从 `-c:v` 起，到 `-pix_fmt` 与关键帧参数为止），纯函数。
//
// ⚠️ 一律 `-pix_fmt yuv420p`（qsv 例外，它只收 nv12 —— 两者都是 4:2:0，播放端看不出区别）：
// 不统一的话硬件编码器会挑它自己喜欢的像素格式，有的播放器打不开。
func EncodeArgs(enc Encoder, in Intent) []string {
	name := enc.Name
	if name == "" {
		name = "libx264"
	}
	var a []string
	pixFmt := "yuv420p"
	rate := func() {
		a = append(a, "-b:v", kbps(in.bitrateK()))
		if in.MaxRateK > 0 {
			a = append(a, "-maxrate", kbps(in.MaxRateK))
		}
		if in.BufSizeK > 0 {
			a = append(a, "-bufsize", kbps(in.BufSizeK))
		}
	}
	switch name {
	case "libx264":
		a = []string{"-c:v", "libx264", "-preset", "veryfast"}
		if crf := x264CRF[in.Tier]; crf > 0 {
			a = append(a, "-crf", strconv.Itoa(crf))
		}
		if in.MaxRateK > 0 {
			a = append(a, "-maxrate", kbps(in.MaxRateK))
		}
		if in.BufSizeK > 0 {
			a = append(a, "-bufsize", kbps(in.BufSizeK))
		}
	case "h264_videotoolbox":
		// allow_sw：没有硬件编码单元（虚拟机、部分老机型）时用苹果的软件实现，而不是直接失败。
		a = []string{"-c:v", name, "-allow_sw", "1", "-realtime", "0"}
		rate()
	case "h264_nvenc":
		a = []string{"-c:v", name, "-preset", "p4", "-rc", "vbr", "-cq", strconv.Itoa(hwQuality[in.Tier]), "-b:v", "0"}
		if in.MaxRateK > 0 {
			a = append(a, "-maxrate", kbps(in.MaxRateK))
		}
		if in.BufSizeK > 0 {
			a = append(a, "-bufsize", kbps(in.BufSizeK))
		}
	case "h264_qsv":
		a = []string{"-c:v", name, "-preset", "veryfast", "-global_quality", strconv.Itoa(hwQuality[in.Tier])}
		if in.MaxRateK > 0 {
			a = append(a, "-maxrate", kbps(in.MaxRateK))
		}
		pixFmt = "nv12"
	case "h264_amf":
		q := strconv.Itoa(hwQuality[in.Tier])
		a = []string{"-c:v", name, "-quality", "balanced", "-rc", "cqp", "-qp_i", q, "-qp_p", q}
	case "h264_mf":
		a = []string{"-c:v", name, "-rate_control", "u_vbr"}
		rate()
	case "libopenh264":
		// openh264 只有码率控制可用（quality 模式在 ffmpeg 的封装里不稳定）。
		a = []string{"-c:v", name, "-rc_mode", "bitrate", "-allow_skip_frames", "0"}
		rate()
	default:
		a = []string{"-c:v", name}
		rate()
	}
	a = append(a, "-pix_fmt", pixFmt)
	if in.GOP > 0 {
		g := strconv.Itoa(in.GOP)
		a = append(a, "-g", g, "-keyint_min", g)
		switch name {
		case "libx264":
			// 不让场景切换额外插关键帧，否则「每秒一个」就不成立了。
			a = append(a, "-sc_threshold", "0")
		case "h264_nvenc":
			a = append(a, "-forced-idr", "1", "-no-scenecut", "1")
		}
	}
	return a
}

// ---------------------------------------------------------------- 探测

// Candidates 是本平台的候选顺序。
//
// libx264 排第一：随包的构建里没有它，只有用户在设置里指定了自己的 ffmpeg 时才会命中 ——
// 那是他明确的选择，而且 x264 的画质与可预期性都更好。之后硬件优先（快、不吃 CPU），
// openh264 兜底（三平台都有，慢一些）。
// ⚠️ 没有 h264_vaapi：它要求 `-vaapi_device` 写在输入之前、滤镜末尾 `hwupload`，
// 与渲染的 filter_complex 拼法冲突；Linux 上没有 nvenc 时走 openh264（dev/04 R-4）。
func Candidates(goos string) []string {
	switch goos {
	case "darwin":
		return []string{"libx264", "h264_videotoolbox", "libopenh264"}
	case "windows":
		return []string{"libx264", "h264_nvenc", "h264_qsv", "h264_amf", "h264_mf", "libopenh264"}
	default:
		return []string{"libx264", "h264_nvenc", "libopenh264"}
	}
}

// ParseEncoders 解析 `ffmpeg -hide_banner -encoders` 的输出，返回编码器名集合。
func ParseEncoders(out string) map[string]bool {
	names := map[string]bool{}
	started := false
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if !started {
			// 表头以一行 `------` 结束。
			if strings.HasPrefix(t, "---") {
				started = true
			}
			continue
		}
		f := strings.Fields(t)
		if len(f) >= 2 && len(f[0]) == 6 && f[0][0] == 'V' {
			names[f[1]] = true
		}
	}
	return names
}

// Skipped 是一个没被选中的候选与原因（自检页展示）。
type Skipped struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Selection 是一次探测的结果。Encoder.Name 为空 = 没有任何可用的 H.264 编码器。
type Selection struct {
	Encoder Encoder   `json:"encoder"`
	Skipped []Skipped `json:"skipped"`
}

// Runner 执行 ffmpeg 并返回合并输出（测试替身用）。
type Runner func(ctx context.Context, args ...string) (string, error)

// TrialArgs 是 1 帧试编码的参数。⚠️ 用 256×256 而不是更小：nvenc 有最小尺寸要求，
// 64×64 会让一块好好的显卡被判成「不可用」；尺寸取偶数（部分 Intel Mac 的 videotoolbox 对奇数宽高报错）。
func TrialArgs(enc string) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin",
		"-f", "lavfi", "-i", "color=c=black:s=256x256:r=30", "-frames:v", "1"}
	args = append(args, EncodeArgs(Encoder{Name: enc}, Intent{Tier: TierFinal, Width: 256, Height: 256, FPS: 30})...)
	return append(args, "-f", "null", "-")
}

// Detect 按候选顺序找第一个能真正编出一帧的编码器。
//
// ⚠️ 只看 `-encoders` 不够：nvenc 在没有 NVIDIA 显卡的机器上照样列出来，qsv / amf 同理。
// 不试编码的话会选中一个必然失败的编码器，所有渲染都在最后一步报错。
func Detect(ctx context.Context, goos string, run Runner) Selection {
	var sel Selection
	out, err := run(ctx, "-hide_banner", "-encoders")
	if err != nil {
		sel.Skipped = append(sel.Skipped, Skipped{Name: "ffmpeg", Reason: "无法列出编码器：" + tail(out, err)})
		return sel
	}
	have := ParseEncoders(out)
	for _, c := range Candidates(goos) {
		if !have[c] {
			sel.Skipped = append(sel.Skipped, Skipped{Name: c, Reason: "当前 ffmpeg 没有编入"})
			continue
		}
		tctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		o, err := run(tctx, TrialArgs(c)...)
		cancel()
		if err != nil {
			sel.Skipped = append(sel.Skipped, Skipped{Name: c, Reason: "试编码失败：" + tail(o, err)})
			continue
		}
		sel.Encoder = Encoder{Name: c}
		return sel
	}
	return sel
}

func tail(out string, err error) string {
	s := strings.TrimSpace(out)
	if s == "" && err != nil {
		s = err.Error()
	}
	if r := []rune(s); len(r) > 200 {
		s = string(r[len(r)-200:])
	}
	return s
}
