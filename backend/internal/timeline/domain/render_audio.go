package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// 成片声音的预渲（docs/agent/smartcut SCT-12，SCD-27）。
//
// 声音先单独渲成一份 48k 立体声 WAV，视频那一遍直接映射它。拆出来有三个理由：
//   - 每个音源都要按**成片时间**放（adelay），不再首尾相接 —— 原来的 concat 让配乐一律从 0 秒开始、
//     画面原声每过一个转场就晚一个转场的时长（SCB-2、SCB-8），而画面看着完全正常；
//   - 人声闪避、两遍 loudnorm 都要整段声音，塞进视频那一遍的滤镜图里，任何一处出错整次渲染一起失败；
//   - 单独一份 WAV 才好测响度（导出自检，SCT-15）。
//
// 这里只拼参数（纯函数），跑进程在 timeline/app 的 runner：
//  1. 第一遍 MeasureArgs：整段算一遍，loudnorm 只测量，结果在 stderr 的 JSON 里（ParseLoudnorm）；
//  2. 第二遍 RenderArgs：带上测量值，线性归一到 -14 LUFS，写 WAV。

// 整片响度目标（SC-PARAM-27，设计建议值）。
const (
	LoudnessTargetI   = -14.0
	LoudnessTargetTP  = -1.5
	LoudnessTargetLRA = 11.0
)

// AudioSampleRate 预渲 WAV 的采样率。
const AudioSampleRate = 48000

// 人声闪避的标定（SC-PARAM-28，CD-SCT-12）。
//
// ⚠️ sidechaincompress 压低多少取决于**键信号有多响**：压缩量 ≈ (键电平 − 阈值) × (1 − 1/ratio)。
// 直接拿人声当键的话，同一个「压低 12dB」对一段 -28dBFS 的口播压 8dB、对一段 -40dBFS 的压不到 1dB
// （本机实测）—— 用户设的深度就成了一个看录音音量的随机数。
//
// 所以键先过一道「找平」：低阈值、高压缩比、大补偿的 acompressor，把 -45dBFS 以上的人声都压到几乎同一电平；
// 低于 -45dBFS 的（底噪、极轻的气声）照原样 +36dB，仍比找平后的人声低 20dB 左右，不会在停顿里把配乐压下去。
// 找平之后键的有效电平约 -10dB（duckKeyLevelDB，本机按 4Hz 起伏的粉噪标定），阈值按「深度 / (1 − 1/ratio)」倒推。
const (
	duckRatio      = 20.0
	duckKeyLevelDB = -10.0
	// duckLeveler 键的找平：阈值 -45dBFS（0.0056）、20:1、起效 5ms、恢复 100ms、补偿 64 倍（+36dB）、硬拐点。
	duckLeveler = "acompressor=threshold=0.0056:ratio=20:attack=5:release=100:makeup=64:knee=1"
)

// duckThreshold 闪避深度（dB）对应的 sidechaincompress 阈值（线性，夹在合法范围 0.000976563~1 内）。
func duckThreshold(depthDB int) float64 {
	d := float64(depthDB)
	if d <= 0 {
		d = DefaultDuckDepthDb
	}
	t := math.Pow(10, (duckKeyLevelDB-d/(1-1/duckRatio))/20)
	return math.Max(0.000976563, math.Min(1, t))
}

// AudioPlan 是一次声音预渲的输入与滤镜图（到 [mix] 为止，两遍共用）。
type AudioPlan struct {
	// Inputs 每个音源一组输入参数（`-ss 起点 -t 源长 -i 路径`）。同一个文件可以被多组引用（输入级裁剪，只解码要用的那一段）。
	Inputs []string
	// Graph 滤镜图，最后一路输出标签是 [mix]：已经补齐到成片总长。
	Graph string
	// TotalMs 成片总长（与画面同一个算法：SnapDuration）。
	TotalMs int
}

// audioSource 是一个音源：一段配乐 / 人声 / 音效，或一段画面的原声。
type audioSource struct {
	path      string
	channels  int
	inMs      int
	srcLen    int
	speed     int
	volume    int // 片段 × 轨道音量（百分比）
	fadeInMs  int
	fadeOutMs int
	startMs   int // 成片起点
	playLen   int
}

// BuildAudioPlan 把快照里的声音拼成一张滤镜图。没有任何声音时返回 false（视频那一遍 `-an`）。
//
// 总线（SC-F-61）：人声（人声轨 + 标成人声的画面原声）、配乐（每条轨各一路，各自的闪避设置）、音效、普通原声。
// 人声被拆出一路键去压开启了闪避的配乐；音效与普通原声不参与闪避。
//
// files 按 mediaId 给出本机文件（去重下载）与它有没有音轨、几个声道。不在 files 里、或没有音轨的素材当无声跳过：
// 预检已经保证素材存在，这里不为一段原声让整次渲染失败。
func BuildAudioPlan(snap RenderSnapshot, files map[int64]MediaFile) (AudioPlan, bool) {
	total := SnapDuration(snap)
	if total <= 0 {
		return AudioPlan{}, false
	}
	var (
		inputs  []string
		chains  []string
		voice   []string
		sfx     []string
		orig    []string
		music   [][]string // 每条配乐轨各一路（各自的闪避设置）
		ducks   []Duck
		nextIdx int
	)
	add := func(s audioSource) (string, bool) {
		if s.volume <= 0 || s.playLen <= 0 || s.srcLen <= 0 || s.startMs >= total || s.path == "" {
			return "", false
		}
		inputs = append(inputs, "-ss", sec(s.inMs), "-t", sec(s.srcLen), "-i", s.path)
		label := fmt.Sprintf("[s%d]", nextIdx)
		chains = append(chains, fmt.Sprintf("[%d:a]%s%s", nextIdx, strings.Join(sourceChain(s), ","), label))
		nextIdx++
		return label, true
	}

	// 画面原声：按成片起点放；转场重叠区间里前一段淡出、后一段淡入（与用户设的淡入淡出取较大者）。
	if !snap.VideoMuted {
		starts, _ := placeVideo(snapSeams(snap.Clips))
		for i, c := range snap.Clips {
			if c.IsImage() {
				continue
			}
			f, ok := files[c.MediaID]
			if !ok || !f.HasAudio {
				continue // 生成出来的视频十有八九没有音轨：跳过，由最后的 apad 补齐，不再补等长静音
			}
			s := sourceOf(c, f, c.Volume*snap.VideoVolume/100, starts[i])
			if i > 0 && c.Transition.IsActive() {
				s.fadeInMs = max(s.fadeInMs, c.Transition.DurationMs)
			}
			if i+1 < len(snap.Clips) && snap.Clips[i+1].Transition.IsActive() {
				s.fadeOutMs = max(s.fadeOutMs, snap.Clips[i+1].Transition.DurationMs)
			}
			s.fadeInMs, s.fadeOutMs = clampFades(s.fadeInMs, s.fadeOutMs, s.playLen)
			label, ok := add(s)
			if !ok {
				continue
			}
			if snap.VideoRole == RoleVoice {
				voice = append(voice, label)
			} else {
				orig = append(orig, label)
			}
		}
	}

	// 音频轨：按成片起点放（SCD-15 按起点放置）。
	for _, t := range snap.AudioTracks {
		if t.Muted || t.Volume <= 0 {
			continue
		}
		var labels []string
		for _, c := range t.Clips {
			f, ok := files[c.MediaID]
			if !ok || !f.HasAudio {
				continue
			}
			s := sourceOf(c, f, c.Volume*t.Volume/100, c.StartMs)
			s.fadeInMs, s.fadeOutMs = clampFades(s.fadeInMs, s.fadeOutMs, s.playLen)
			if label, ok := add(s); ok {
				labels = append(labels, label)
			}
		}
		if len(labels) == 0 {
			continue
		}
		switch t.Role {
		case RoleVoice:
			voice = append(voice, labels...)
		case RoleSfx:
			sfx = append(sfx, labels...)
		default: // music，以及旧快照 / 认不出的用途：按配乐处理
			music = append(music, labels)
			ducks = append(ducks, t.DuckOf())
		}
	}
	if nextIdx == 0 {
		return AudioPlan{}, false
	}

	graph := append([]string(nil), chains...)
	var finals []string
	mixInto := func(labels []string, out string) string {
		if len(labels) == 1 {
			return labels[0]
		}
		graph = append(graph, fmt.Sprintf("%samix=inputs=%d:normalize=0:duration=longest%s",
			strings.Join(labels, ""), len(labels), out))
		return out
	}

	voiceBus := ""
	if len(voice) > 0 {
		voiceBus = mixInto(voice, "[bv]")
	}
	// 闪避：只对开启了闪避的配乐轨接侧链；人声总线拆成「进混音的一路 + 每条要闪避的配乐轨一路键」。
	ducked := 0
	if voiceBus != "" {
		for _, d := range ducks {
			if d.Enabled {
				ducked++
			}
		}
	}
	var keys []string
	if ducked > 0 {
		outs := "[bvx]"
		for k := 0; k < ducked; k++ {
			keys = append(keys, fmt.Sprintf("[bk%d]", k))
		}
		graph = append(graph, fmt.Sprintf("%sasplit=%d%s%s", voiceBus, ducked+1, outs, strings.Join(keys, "")))
		voiceBus = outs
	}
	if voiceBus != "" {
		finals = append(finals, voiceBus)
	}
	keyIdx := 0
	for i, labels := range music {
		bus := mixInto(labels, fmt.Sprintf("[bm%d]", i))
		if ducked > 0 && ducks[i].Enabled {
			// ⚠️ 键必须补齐到成片总长：sidechaincompress 在侧链输入结束时就结束，最后一句人声之后的配乐
			// 会被整段截掉（本机实测：人声 4~6 秒、配乐到 10 秒，不补的话成片 6 秒之后没有配乐）。
			// 用有界的 apad=whole_dur，不用无界的 apad，理由见 padTo。
			key := keys[keyIdx]
			keyIdx++
			leveled := fmt.Sprintf("[bkk%d]", i)
			out := fmt.Sprintf("[bmd%d]", i)
			graph = append(graph,
				fmt.Sprintf("%s%s,%s%s", key, padTo(total), duckLeveler, leveled),
				fmt.Sprintf("%s%ssidechaincompress=threshold=%.6f:ratio=%g:attack=%d:release=%d%s",
					bus, leveled, duckThreshold(ducks[i].DepthDb), duckRatio, DuckAttackMs, DuckReleaseMs, out))
			bus = out
		}
		finals = append(finals, bus)
	}
	if len(sfx) > 0 {
		finals = append(finals, mixInto(sfx, "[bs]"))
	}
	if len(orig) > 0 {
		finals = append(finals, mixInto(orig, "[bo]"))
	}

	// ⚠️ 一律 normalize=0：amix 默认按路数把每一路压低（两路各低 6dB），多一条轨整片就悄悄变小声。
	// 补齐到成片总长再截断：声音比画面短的补静音，比画面长的（配乐放得比片子长）截掉。
	tail := fmt.Sprintf("%s,atrim=0:%s[mix]", padTo(total), sec(total))
	if len(finals) == 1 {
		graph = append(graph, finals[0]+tail)
	} else {
		graph = append(graph, fmt.Sprintf("%samix=inputs=%d:normalize=0:duration=longest,%s",
			strings.Join(finals, ""), len(finals), tail))
	}
	return AudioPlan{Inputs: inputs, Graph: strings.Join(graph, ";"), TotalMs: total}, true
}

// padTo 把声音补齐到 totalMs（不截断：比它长的由后面的 atrim 截）。
//
// ⚠️ 必须是有界的 `apad=whole_dur=…`，不能是无界的 `apad` + `atrim`：本机 ffmpeg 8.1.2 上后者有一定概率
// 永远不结束 —— atrim 早已输出完，apad 还在不停地造静音，进程占满一个核卡死（实测 20 次卡住 5 次，
// 一次卡了 10 分钟直到被杀）。渲染卡死的表现是任务一直「混音中」直到撞死线，而换成 whole_dur 后 50 次没有一次卡住。
func padTo(totalMs int) string { return "apad=whole_dur=" + sec(totalMs) }

// sourceOf 一段片段的音源（时间、变速、淡入淡出照片段）。volume 是已经乘过轨道音量的百分比。
func sourceOf(c SnapClip, f MediaFile, volume, startMs int) audioSource {
	speed := c.Speed
	if speed <= 0 {
		speed = DefaultSpeed
	}
	return audioSource{path: f.Path, channels: f.AudioChannels, inMs: c.SourceInMs,
		srcLen: c.SourceOutMs - c.SourceInMs, speed: speed, volume: volume, fadeInMs: c.FadeInMs,
		fadeOutMs: c.FadeOutMs, startMs: startMs, playLen: c.PlayLenOf()}
}

// clampFades 淡入 + 淡出不超过这一段本身：超了 afade 会把整段压成静音。
func clampFades(in, out, playLen int) (int, int) {
	in = max(0, min(in, playLen))
	out = max(0, min(out, playLen-in))
	return in, out
}

// sourceChain 一个音源的滤镜链：时间戳归零 → 统一成 48k 立体声 → 推到成片起点 → 变速 → 音量 → 淡入淡出。
//
// ⚠️ 推后（adelay）要放在变速（atempo）**之前**：本机 ffmpeg 8.1.2 上 atempo 之后的 adelay 完全不生效
// （试过先 asetpts 归零、先 asetnsamples 重新分帧、按样本数写，结果都一样：这一段从 0 秒就响了，
// 而渲染是"成功"的）。放在前面时推后的那段静音也会被变速，所以按倍速折算成样本数：
// 成片里推后 D 毫秒 = 变速前推后 D × 倍速 的样本。淡入淡出在变速之后做，时刻按成片里的绝对时间写。
func sourceChain(s audioSource) []string {
	chain := []string{"asetpts=PTS-STARTPTS"}
	if s.channels == 1 {
		// ⚠️ 单声道要复制成两个声道，不能交给默认上混：默认矩阵按 -3dB 摊到左右声道（本机实测低 3.0dB），
		// 单声道的配音就比立体声的配乐小一截。
		chain = append(chain, "pan=stereo|c0=c0|c1=c0")
	}
	chain = append(chain,
		fmt.Sprintf("aresample=%d", AudioSampleRate),
		"aformat=sample_fmts=fltp:channel_layouts=stereo")
	if s.startMs > 0 {
		// all=1：每个声道都推后同样的时长（只写一个数时默认只推第一个声道，左右声道就错开了）。
		if s.speed == DefaultSpeed {
			chain = append(chain, fmt.Sprintf("adelay=%d:all=1", s.startMs))
		} else {
			samples := int(math.Round(float64(s.startMs) * AudioSampleRate / 1000 * float64(s.speed) / 100))
			chain = append(chain, fmt.Sprintf("adelay=%dS:all=1", samples))
		}
	}
	// ⚠️ 变速用 atempo 不用 setpts：setpts 只改时间戳、采样率不变，出来是音画不同步而画面看着是对的。
	chain = append(chain, atempoChain(s.speed)...)
	if s.volume != 100 {
		chain = append(chain, fmt.Sprintf("volume=%s", ratio(s.volume, 100)))
	}
	if s.fadeInMs > 0 {
		chain = append(chain, fmt.Sprintf("afade=t=in:st=%s:d=%s", sec(s.startMs), sec(s.fadeInMs)))
	}
	if s.fadeOutMs > 0 {
		chain = append(chain, fmt.Sprintf("afade=t=out:st=%s:d=%s",
			sec(s.startMs+s.playLen-s.fadeOutMs), sec(s.fadeOutMs)))
	}
	return chain
}

// MeasureArgs 第一遍：只测量（loudnorm 的统计打在 stderr 的 JSON 里），不写文件。
//
// ⚠️ 不能加 `-loglevel error`：统计按 info 级别打印，压掉的话第二遍拿不到测量值。
func (p AudioPlan) MeasureArgs() []string {
	args := []string{"-hide_banner", "-nostdin", "-nostats", "-y"}
	args = append(args, p.Inputs...)
	return append(args, "-filter_complex", p.Graph+";"+fmt.Sprintf(
		"[mix]loudnorm=I=%g:TP=%g:LRA=%g:print_format=json[aout]",
		LoudnessTargetI, LoudnessTargetTP, LoudnessTargetLRA),
		"-map", "[aout]", "-f", "null", "-")
}

// RenderArgs 第二遍：带上第一遍的测量值线性归一，写 48k 立体声 16 位 WAV。
//
// 整段近乎无声（测出来的响度是 -inf 或低于 -70）时不做响度归一：把一段底噪拉到 -14 LUFS 只会得到一段很响的噪声。
func (p AudioPlan) RenderArgs(m LoudnormStats, out string) []string {
	args := []string{"-hide_banner", "-nostdin", "-nostats", "-loglevel", "error", "-y"}
	args = append(args, p.Inputs...)
	graph := p.Graph + ";[mix]anull[aout]"
	if m.Usable() {
		graph = p.Graph + ";" + fmt.Sprintf(
			"[mix]loudnorm=I=%g:TP=%g:LRA=%g:measured_I=%.2f:measured_TP=%.2f:measured_LRA=%.2f:"+
				"measured_thresh=%.2f:offset=%.2f:linear=true[aout]",
			LoudnessTargetI, LoudnessTargetTP, LoudnessTargetLRA,
			clampF(m.InputI, -99, 0), clampF(m.InputTP, -99, 99), clampF(m.InputLRA, 0, 99),
			clampF(m.InputThresh, -99, 0), clampF(m.TargetOffset, -99, 99))
	}
	// ⚠️ loudnorm 内部按 192k 处理，不写 -ar 的话输出就是 192k。
	return append(args, "-filter_complex", graph, "-map", "[aout]",
		"-ar", strconv.Itoa(AudioSampleRate), "-ac", "2", "-c:a", "pcm_s16le", out)
}

// LoudnormStats 是第一遍 loudnorm 的测量值（只取第二遍要用的五项）。
type LoudnormStats struct {
	InputI       float64
	InputTP      float64
	InputLRA     float64
	InputThresh  float64
	TargetOffset float64
}

// Usable 测量值能不能拿去做归一：整段静音时 loudnorm 报 -inf，那时不该归一。
func (m LoudnormStats) Usable() bool {
	for _, v := range []float64{m.InputI, m.InputTP, m.InputLRA, m.InputThresh, m.TargetOffset} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return m.InputI > -70
}

// ParseLoudnorm 从第一遍的 stderr 里取 loudnorm 打出的 JSON（取最后一个 `{…}` 块：前面可能有别的滤镜的输出）。
//
// ⚠️ 取不到或缺字段时报错，不能默默当 0：measured_I=0 会让第二遍把整片当成「已经很响」往下压几十 dB，
// 出来一段几乎听不见的声音，而渲染是"成功"的。
func ParseLoudnorm(stderr []byte) (LoudnormStats, error) {
	end := bytes.LastIndexByte(stderr, '}')
	if end < 0 {
		return LoudnormStats{}, fmt.Errorf("响度测量的输出里没有找到统计结果")
	}
	start := bytes.LastIndexByte(stderr[:end], '{')
	if start < 0 {
		return LoudnormStats{}, fmt.Errorf("响度测量的输出里没有找到统计结果")
	}
	var raw map[string]string
	if err := json.Unmarshal(stderr[start:end+1], &raw); err != nil {
		return LoudnormStats{}, fmt.Errorf("响度测量结果解析失败：%w", err)
	}
	var out LoudnormStats
	for _, f := range []struct {
		key string
		dst *float64
	}{
		{"input_i", &out.InputI}, {"input_tp", &out.InputTP}, {"input_lra", &out.InputLRA},
		{"input_thresh", &out.InputThresh}, {"target_offset", &out.TargetOffset},
	} {
		s, ok := raw[f.key]
		if !ok {
			return LoudnormStats{}, fmt.Errorf("响度测量结果缺少 %s", f.key)
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return LoudnormStats{}, fmt.Errorf("响度测量结果 %s=%q 不是数字", f.key, s)
		}
		*f.dst = v
	}
	return out, nil
}
