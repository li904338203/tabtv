package domain

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"io"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// 剪辑台的预览素材与分析（docs/agent/smartcut SCT-16，SCD-30）。
//
// 剪辑台与节点卡只加载低清预览版（SCK-10）：媒体下载要整份读进内存，几百 MB 的上传视频会把标签页撑爆。
// Worker 的分析扫描器按素材生成并缓存在 `media_analysis`，同一份只做一次。
// 这里只放纯计算：ffmpeg 参数、探测结果的解析、峰值量化、拼图版式 —— 执行在 media/app。

// 分析种类（`media_analysis.kind`，与 00067 的 ck_analysis_kind 一致）。
const (
	AnalysisPreviewProxy = "preview_proxy"
	AnalysisFilmstrip    = "filmstrip"
	AnalysisPeaks        = "peaks"
	AnalysisSpeechAudio  = "speech_audio"
	AnalysisBeats        = "beats"
	// AnalysisASR 识别结果：由识别任务完成时写入（SCT-33），扫描器不做。
	AnalysisASR = "asr"
	// AnalysisCaption AI 剪辑看图后对该段的描述：由规划调用写入（SCT-27），扫描器不做。
	AnalysisCaption = "caption"
)

// ScannerKinds 是分析扫描器会领取的种类。asr / caption 由别的流程写，扫描器领走的话会被当成「不认识的种类」判失败。
var ScannerKinds = []string{AnalysisPreviewProxy, AnalysisFilmstrip, AnalysisPeaks, AnalysisSpeechAudio, AnalysisBeats}

// AnalysisEngine 是写进 `media_analysis.engine` 的算法版本。
//
// ⚠️ 参数或算法变了就换版本号：缓存键里有它，旧结果按版本自然失效、重新排队；
// 不换的话改了缩放规则，已经生成过的素材永远停在旧的预览版上，而那不报错。
func AnalysisEngine(kind string) string {
	switch kind {
	case AnalysisPreviewProxy:
		return "proxy-v1"
	case AnalysisFilmstrip:
		return "filmstrip-v1"
	case AnalysisPeaks:
		return "peaks-v1"
	case AnalysisSpeechAudio:
		return "speech-v1"
	case AnalysisBeats:
		return BeatsEngine
	}
	return ""
}

// AnalysisApplies 判断一种分析对一类媒体（image / audio / video）有没有意义。
//
// 纯音频只做波形、节拍与识别用音轨；节拍只给音频（配乐，SCT-17）—— 视频原声卡不了点；
// 图片什么都不做。排队时就过滤掉，不让扫描器去领一行注定做不了的。
func AnalysisApplies(kind, mediaKind string) bool {
	switch kind {
	case AnalysisPreviewProxy, AnalysisFilmstrip:
		return mediaKind == "video"
	case AnalysisPeaks, AnalysisSpeechAudio:
		return mediaKind == "video" || mediaKind == "audio"
	case AnalysisBeats:
		return mediaKind == "audio"
	}
	return false
}

// AnalysisMaxAttempts 失败（含进程退出、超时）满这么多次就判 failed，不再自动重试。
//
// ⚠️ 进程中途退出也算一次：一份能把 Worker 撑爆的畸形文件，不封顶的话会在「领取 → 崩溃 → 租约过期 → 再领取」
// 之间无限循环，每一圈都把一个 Worker 拖死。
const AnalysisMaxAttempts = 3

// AnalysisRetryAfter 第 n 次失败后多久再试（1 分钟、2 分钟……）：立即重试多半撞上同一个临时故障。
func AnalysisRetryAfter(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	return time.Duration(attempt) * time.Minute
}

// AnalysisTimeout 单个分析任务的上限（下载 + ffmpeg + 上传），按素材时长取：
// 不少于 10 分钟、约 3 倍时长、最多 3 小时；时长未知按 30 分钟。
//
// ⚠️ 必须有：畸形文件会让 ffmpeg 一直读下去，而执行者在给租约续期 —— 没有上限的话它会永远占着这一行。
func AnalysisTimeout(durationMs int) time.Duration {
	if durationMs <= 0 {
		return 30 * time.Minute
	}
	d := 3 * time.Duration(durationMs) * time.Millisecond
	switch {
	case d < 10*time.Minute:
		return 10 * time.Minute
	case d > 3*time.Hour:
		return 3 * time.Hour
	}
	return d
}

// ---------------------------------------------------------------- 探测

// StreamInfo 是 ffprobe 看到的流概况。
type StreamInfo struct {
	// HasVideo 有没有真正的视频流（MP3 里内嵌的封面图不算）。
	HasVideo bool
	HasAudio bool
	// Width / Height 是**显示尺寸**：手机竖拍的视频常是横着编码、带 90° 旋转，按编码尺寸算比例会把竖屏当横屏。
	Width, Height int
	// FPS 平均帧率；未知为 0。
	FPS        float64
	DurationMs int
	// AudioChannels 第一条音轨的声道数；未知为 0（渲染时单声道要复制成双声道，不按默认矩阵上混 —— 默认会低 3dB）。
	AudioChannels int
}

type probeJSON struct {
	Streams []struct {
		CodecType    string `json:"codec_type"`
		Channels     int    `json:"channels"`
		Width        int    `json:"width"`
		Height       int    `json:"height"`
		AvgFrameRate string `json:"avg_frame_rate"`
		RFrameRate   string `json:"r_frame_rate"`
		Disposition  struct {
			AttachedPic int `json:"attached_pic"`
		} `json:"disposition"`
		Tags struct {
			Rotate string `json:"rotate"`
		} `json:"tags"`
		SideData []struct {
			Rotation float64 `json:"rotation"`
		} `json:"side_data_list"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

// ProbeArgs 是 ffprobe 的参数（路径放最后并用 `--` 隔开，同 ProbeDuration）。
func ProbeArgs(path string) []string {
	return []string{"-v", "error",
		"-show_entries",
		"stream=codec_type,channels,width,height,avg_frame_rate,r_frame_rate:stream_disposition=attached_pic:" +
			"stream_tags=rotate:stream_side_data=rotation:format=duration",
		"-of", "json", "--", path}
}

// ParseProbe 解析 ffprobe 的 JSON 输出。
//
// ⚠️ 封面图（`attached_pic`）在 ffprobe 里也是一条 video 流：MP3 带专辑封面时不排除的话，
// 一首歌会被当成视频去转预览版。取第一条真正的视频流与第一条音频流。
func ParseProbe(data []byte) (StreamInfo, error) {
	var p probeJSON
	if err := json.Unmarshal(data, &p); err != nil {
		return StreamInfo{}, fmt.Errorf("探测结果解不开：%w", err)
	}
	var info StreamInfo
	for _, s := range p.Streams {
		switch s.CodecType {
		case "audio":
			if !info.HasAudio {
				info.AudioChannels = s.Channels
			}
			info.HasAudio = true
		case "video":
			if s.Disposition.AttachedPic == 1 || info.HasVideo {
				continue
			}
			info.HasVideo = true
			info.Width, info.Height = s.Width, s.Height
			rot := 0.0
			if r, err := strconv.ParseFloat(strings.TrimSpace(s.Tags.Rotate), 64); err == nil {
				rot = r
			}
			for _, sd := range s.SideData {
				if sd.Rotation != 0 {
					rot = sd.Rotation
				}
			}
			if q := int(math.Round(math.Abs(rot))) % 180; q == 90 {
				info.Width, info.Height = info.Height, info.Width
			}
			info.FPS = parseRate(s.AvgFrameRate)
			if info.FPS == 0 {
				info.FPS = parseRate(s.RFrameRate)
			}
		}
	}
	if sec, err := strconv.ParseFloat(strings.TrimSpace(p.Format.Duration), 64); err == nil && sec > 0 {
		info.DurationMs = int(math.Round(sec * 1000))
	}
	return info, nil
}

// parseRate 解析 "30000/1001" 这类帧率；"0/0" 与非法值为 0。
func parseRate(s string) float64 {
	num, den, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok {
		f, err := strconv.ParseFloat(num, 64)
		if err != nil || f <= 0 || math.IsInf(f, 0) {
			return 0
		}
		return f
	}
	n, err1 := strconv.ParseFloat(num, 64)
	d, err2 := strconv.ParseFloat(den, 64)
	if err1 != nil || err2 != nil || n <= 0 || d <= 0 {
		return 0
	}
	return n / d
}

// ProbeStreams 跑 ffprobe 读流概况（带 ProbeTimeout）。
func ProbeStreams(ctx context.Context, path string) (StreamInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "ffprobe", ProbeArgs(path)...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return StreamInfo{}, fmt.Errorf("ffprobe 失败：%w %s", err, StderrTail(stderr.String(), 300))
	}
	return ParseProbe(out)
}

// StderrTail 只留 stderr 的尾巴：真正的原因在最后几行，完整输出会撑爆错误列。
func StderrTail(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	tail := s[len(s)-max:]
	// 别从一个 UTF-8 字符中间切开（ffmpeg 会原样打出含中文的文件名）。
	for i := 0; i < len(tail) && i < 4; i++ {
		if tail[i]&0xC0 != 0x80 {
			return tail[i:]
		}
	}
	return tail
}

// ---------------------------------------------------------------- 预览版

// ProxyShortEdge 预览版的短边（SCK-10）：预览版是浏览器预览内存的主要来源（SCR-4）。
const ProxyShortEdge = 480

// ProxyScaleFilter 把短边缩到 480（本来就更小的不放大），另一边按比例取偶数。
//
// 用表达式而不是先探尺寸再算：ffmpeg 默认先按旋转元数据把画面转正再进滤镜，表达式里的 iw / ih
// 已经是显示尺寸 —— 自己探的话要另外解析旋转，手机竖拍的视频最容易在这里错成横屏。
// `trunc(…/2)*2`：yuv420p 要求宽高都是偶数，459 这样的奇数源尺寸不取偶的话编码器直接报错。
const ProxyScaleFilter = "scale=w='if(gt(iw,ih),-2,min(480,trunc(iw/2)*2))'" +
	":h='if(gt(iw,ih),min(480,trunc(ih/2)*2),-2)'"

// KeyframeInterval 预览版的关键帧间隔（帧数）= 帧率：每秒一个关键帧，剪辑台拖动播放头时定位快。
// 帧率未知按 30。
func KeyframeInterval(fps float64) int {
	if fps <= 0 || math.IsNaN(fps) || math.IsInf(fps, 0) {
		return 30
	}
	n := int(math.Round(fps))
	switch {
	case n < 1:
		return 1
	case n > 120:
		return 120
	}
	return n
}

// ProxyArgs 生成预览版：H.264 + AAC 64k，`+faststart` 把 moov 挪到文件头（浏览器不用下完整个文件才能播）。
//
// ⚠️ 音轨用 `0:a:0?`（可选）：生成出来的视频十有八九没有音轨，写成必选的话 ffmpeg 直接报错。
// ⚠️ `-sc_threshold 0`：不让场景切换额外插关键帧，否则「每秒一个」就不成立了（多了不出错，但间隔不再整齐）。
func ProxyArgs(input, output string, fps float64) []string {
	g := strconv.Itoa(KeyframeInterval(fps))
	return []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-i", input,
		"-map", "0:v:0", "-map", "0:a:0?",
		"-vf", ProxyScaleFilter,
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "28", "-maxrate", "1200k", "-bufsize", "2400k",
		"-pix_fmt", "yuv420p", "-g", g, "-keyint_min", g, "-sc_threshold", "0",
		"-c:a", "aac", "-b:a", "64k", "-ac", "2",
		"-sn", "-dn", "-map_metadata", "-1",
		"-movflags", "+faststart", output}
}

// ---------------------------------------------------------------- 缩略图拼图

const (
	// FilmstripFrameWidth 每一帧的宽（高按比例取偶数）。
	FilmstripFrameWidth = 160
	// FilmstripIntervalMs 每秒一帧。
	FilmstripIntervalMs = 1000
	// FilmstripCols / FilmstripPerSheet 每张 10 列、最多 100 帧，超出分多张。
	FilmstripCols     = 10
	FilmstripPerSheet = 100
)

// FilmstripFramesArgs 把视频抽成每秒一帧、宽 160 的小图（pattern 形如 `f-%06d.jpg`）。
//
// ⚠️ 抽帧之后在 Go 里拼版，而不是直接用 `tile=10x10`：tile 不告诉你一共抽出了几帧（`count` 要靠时长去猜，
// 差一帧前端就整条错位），最后一张还会补满黑格。帧号从 1 起、按时间顺序。
func FilmstripFramesArgs(input, pattern string) []string {
	return []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-i", input,
		"-map", "0:v:0", "-an", "-sn", "-dn",
		"-vf", "fps=1,scale=" + strconv.Itoa(FilmstripFrameWidth) + ":-2",
		"-q:v", "5", pattern}
}

// FilmstripColsFor 是拼图的列数：不足 10 帧时就是帧数（一行），否则 10。
func FilmstripColsFor(count int) int {
	switch {
	case count <= 0:
		return 0
	case count < FilmstripCols:
		return count
	}
	return FilmstripCols
}

// FilmstripSheets 把 count 帧分到若干张，返回每张的帧数（每张最多 100 帧）。
// 前端按 `第 i 帧 → 第 i/100 张、第 (i%100)/cols 行、第 (i%100)%cols 列` 取图。
func FilmstripSheets(count int) []int {
	var out []int
	for count > 0 {
		n := min(count, FilmstripPerSheet)
		out = append(out, n)
		count -= n
	}
	return out
}

// ComposeSheet 把一组帧按 cols 列拼成一张图；每格大小取 frameW × frameH，帧比格子大的部分裁掉。
//
// 行数按本张的帧数算（最后一张不补满空行）。
func ComposeSheet(frames []image.Image, cols, frameW, frameH int) *image.RGBA {
	if cols <= 0 || frameW <= 0 || frameH <= 0 || len(frames) == 0 {
		return image.NewRGBA(image.Rect(0, 0, 0, 0))
	}
	rows := (len(frames) + cols - 1) / cols
	sheet := image.NewRGBA(image.Rect(0, 0, cols*frameW, rows*frameH))
	for i, f := range frames {
		x, y := (i%cols)*frameW, (i/cols)*frameH
		cell := image.Rect(x, y, x+frameW, y+frameH)
		draw.Draw(sheet, cell, f, f.Bounds().Min, draw.Src)
	}
	return sheet
}

// ---------------------------------------------------------------- 波形

const (
	// PeaksSampleRate / PeaksIntervalMs 波形：8kHz 单声道，每 10ms 一个峰值。
	PeaksSampleRate = 8000
	PeaksIntervalMs = 10
	peakWindow      = PeaksSampleRate * PeaksIntervalMs / 1000
)

// PeaksPCMArgs 把第一条音轨解码成 8kHz 单声道 s16le 写到标准输出。
func PeaksPCMArgs(input string) []string {
	return []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-i", input,
		"-map", "0:a:0", "-vn", "-sn", "-dn",
		"-ac", "1", "-ar", strconv.Itoa(PeaksSampleRate), "-f", "s16le", "pipe:1"}
}

// QuantizePeak 把一个窗口的峰值（|样本| 的最大值，0~32768）线性量化到 0~255，四舍五入。
func QuantizePeak(peak int) byte {
	if peak <= 0 {
		return 0
	}
	q := (peak*255 + 16384) / 32768
	if q > 255 {
		q = 255
	}
	return byte(q)
}

// PeaksFromPCM 从 8kHz 单声道 s16le 流里每 10ms（80 个样本）取一个峰值。
//
// ⚠️ 流式：一小时的音频解码后约 57MB，而峰值只有 36 万个字节。末尾不足一个窗口的部分也算一个；
// 不足 2 字节的残片丢弃。
func PeaksFromPCM(r io.Reader) ([]byte, error) {
	buf := make([]byte, 2*peakWindow*64)
	var out []byte
	peak, n := 0, 0
	carry := -1 // 上一块末尾剩下的一个字节
	for {
		m, err := io.ReadFull(r, buf)
		data := buf[:m]
		if carry >= 0 && m > 0 {
			v := int(int16(uint16(carry) | uint16(data[0])<<8))
			peak, n = feedPeak(&out, peak, n, v)
			data = data[1:]
			carry = -1
		}
		for len(data) >= 2 {
			v := int(int16(binary.LittleEndian.Uint16(data)))
			peak, n = feedPeak(&out, peak, n, v)
			data = data[2:]
		}
		if len(data) == 1 {
			carry = int(data[0])
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if n > 0 {
		out = append(out, QuantizePeak(peak))
	}
	if out == nil {
		out = []byte{}
	}
	return out, nil
}

func feedPeak(out *[]byte, peak, n, v int) (int, int) {
	if v < 0 {
		v = -v
	}
	if v > peak {
		peak = v
	}
	n++
	if n == peakWindow {
		*out = append(*out, QuantizePeak(peak))
		return 0, 0
	}
	return peak, n
}

// ---------------------------------------------------------------- 识别用音轨与节拍

// SpeechAudioArgs 抽识别用音轨：16kHz 单声道 32kbps MP3（SCD-32）。
// 识别按秒计价、上游有体积上限，原片的音轨动辄几十 MB，抽出来的一小时约 14MB。
func SpeechAudioArgs(input, output string) []string {
	return []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-i", input,
		"-map", "0:a:0", "-vn", "-sn", "-dn", "-map_metadata", "-1",
		"-ac", "1", "-ar", "16000", "-c:a", "libmp3lame", "-b:a", "32k", output}
}

// BeatsPCMArgs 把第一条音轨解码成节拍检测要的 22050Hz 单声道 f32le（SCT-17）。
func BeatsPCMArgs(input string) []string {
	return []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-i", input,
		"-map", "0:a:0", "-vn", "-sn", "-dn",
		"-ac", "1", "-ar", strconv.Itoa(BeatsSampleRate), "-f", "f32le", "pipe:1"}
}
