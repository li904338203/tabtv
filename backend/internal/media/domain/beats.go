package domain

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
	"sort"
)

// 节拍检测（docs/agent/smartcut SCT-17，dev/01 SCD-31）。
//
// 给配乐算速度、拍点与强拍，供素材混剪卡点（SCT-26 / 28）与剪辑台画节拍（SCT-20）用。
// 纯计算：不碰库、不发网络、不调上游（`go list -deps` 绊线钉住），输入是 ffmpeg 解码出的 PCM 流。
//
// 流程（Ellis 2007「Beat Tracking by Dynamic Programming」的做法）：
//  1. 起音包络：STFT（窗 2048、步长 512、Hann）→ 40 带梅尔谱 → 取对数 → 逐带正向差分求和（谱通量）；
//  2. 速度：包络去均值后自相关，在 60~200 BPM 的滞后范围内乘以中心 120 BPM、宽一个八度的对数正态先验；
//     倍速 / 半速用 TPS2（周期 τ 另加 2τ 处的证据）单独打分，避免判成两倍或一半；
//  3. 拍点：动态规划（得分 = 起音强度 + 前一拍得分 − 紧度 ×(log(间隔/周期))²，紧度 100），回溯；
//  4. 强拍：按 4 拍一小节，四种相位里取「拍点上起音强度之和」最大的一种（近似，downbeatApprox = true）；
//  5. 置信度：自相关主峰与「非倍数关系的」次峰之比，映射到 0~1；再乘以「落在真实起音上的拍」的比例。

const (
	// BeatsEngine 写进 media_analysis.engine：算法变了就换版本号，旧结果按版本失效。
	BeatsEngine = "beats-v1"
	// BeatsSampleRate 输入 PCM 的采样率（ffmpeg `-ac 1 -ar 22050 -f f32le`）。
	BeatsSampleRate = 22050

	beatWindow    = 2048
	beatHop       = 512
	beatMelBands  = 40
	beatMelFMin   = 30.0
	beatMinBPM    = 60.0
	beatMaxBPM    = 200.0
	beatPriorBPM  = 120.0
	beatPriorOct  = 1.0 // 先验的宽度：一个八度（对数域标准差）
	beatTightness = 100.0
	// beatPowerFloor 功率下限（取对数前）：数字静音不是 −∞。
	beatPowerFloor = 1e-10
	// beatMinSeconds 太短的音频不估速度：两个最慢的拍都放不下时，自相关没有意义。
	beatMinSeconds = 3.0
	// beatMinOnsetBeats / beatOnsetRiseDB：拍点里至少要有几拍是真的起音（能量上升 ≥ 3dB），
	// 否则判为「没有节奏」（持续的长音、环境声）——跟踪器总能沿着周期摆出拍子，摆出来不等于有拍。
	beatMinOnsetBeats = 3
	beatOnsetRiseDB   = 3.0
)

// BeatResult 是 media_analysis(kind='beats').result 的形状（dev/02 §2），JSON 键与契约一致。
// 时间一律整数毫秒；没有检出节拍时各列表为空（不是 null）。
type BeatResult struct {
	BPM            float64 `json:"bpm"`
	BeatsMs        []int   `json:"beatsMs"`
	DownbeatsMs    []int   `json:"downbeatsMs"`
	DownbeatApprox bool    `json:"downbeatApprox"`
	Confidence     float64 `json:"confidence"`
}

func emptyBeats() BeatResult {
	return BeatResult{BeatsMs: []int{}, DownbeatsMs: []int{}, DownbeatApprox: true}
}

// DetectBeats 从 22050Hz 单声道 float32 小端 PCM 流里检测节拍（ffmpeg `-ac 1 -ar 22050 -f f32le` 的输出）。
//
// ⚠️ 流式：按 512 个样本一块做 STFT，只累积每帧两个数（谱通量与总能量），不把整段 PCM 读进内存 ——
// 30 分钟的配乐解码成 float32 约 150MB，而帧序列只有约 600KB。
// 空输入 / 全静音 / 太短 / 没有节奏时返回空拍点、置信度 0，不报错。末尾不足 4 字节的残片丢弃。
func DetectBeats(r io.Reader) (BeatResult, error) {
	d := newOnsetDetector()
	chunk := make([]byte, 4*4096)
	samples := make([]float32, 4096)
	for {
		n, err := io.ReadFull(r, chunk)
		m := n / 4
		for i := 0; i < m; i++ {
			samples[i] = math.Float32frombits(binary.LittleEndian.Uint32(chunk[4*i:]))
		}
		d.push(samples[:m])
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return BeatResult{}, err
		}
	}
	return d.result(), nil
}

// detectBeatsSamples 与 DetectBeats 同一条流水线，入参是已经在内存里的样本（测试用）。
func detectBeatsSamples(x []float32) BeatResult {
	d := newOnsetDetector()
	d.push(x)
	return d.result()
}

// ---------------------------------------------------------------- 起音包络（流式 STFT）

type melBand struct {
	start   int
	weights []float64
}

// onsetDetector 是流式的 STFT：帧 t 的窗覆盖 [t·512 − 1536, t·512 + 512)，开头先垫 1536 个 0。
// 每来 512 个新样本算一帧；该帧的时间记在「新样本块的起点」t·512（见 frameMs 的说明）。
type onsetDetector struct {
	window   []float64
	plan     *realPowerPlan
	mel      []melBand
	frame    []float64
	pending  int
	windowed []float64
	power    []float64
	prevLog  []float64
	flux     []float64 // 每帧：逐带对数梅尔正向差分的均值（dB）
	energy   []float64 // 每帧：梅尔能量总和的对数（dB），给强拍与修剪用
	// coverage[t]：开头几帧里真实样本占窗能量（Σw²）的比例；窗填满之后为 1。
	coverage []float64
	frames   int
	started  bool
	done     bool
}

func newOnsetDetector() *onsetDetector {
	d := &onsetDetector{
		window:   hannWindow(beatWindow),
		plan:     newRealPowerPlan(beatWindow),
		mel:      melFilterbank(beatWindow, BeatsSampleRate, beatMelBands, beatMelFMin, BeatsSampleRate/2.0),
		frame:    make([]float64, beatWindow),
		windowed: make([]float64, beatWindow),
		power:    make([]float64, beatWindow/2+1),
		prevLog:  make([]float64, beatMelBands),
	}
	// 开头先垫了 1536 个 0：前 3 帧的窗里只有一部分是真实样本。不补偿的话，一段以底噪开头的录音
	// 会因为「窗在被填满」看起来像一次起音（实测在 23ms 多出一拍）。按真实样本覆盖的窗能量比例放大回来，
	// 平稳的声音就没有上升；开头真有一下鼓点，照样看得出上升。
	var full float64
	for _, w := range d.window {
		full += w * w
	}
	for t := 0; (t+1)*beatHop < beatWindow; t++ {
		var part float64
		for i := beatWindow - (t+1)*beatHop; i < beatWindow; i++ {
			part += d.window[i] * d.window[i]
		}
		d.coverage = append(d.coverage, part/full)
	}
	return d
}

func (d *onsetDetector) push(x []float32) {
	for _, v := range x {
		f := float64(v)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			f = 0
		}
		d.frame[beatWindow-beatHop+d.pending] = f
		d.pending++
		if d.pending == beatHop {
			d.process()
			d.pending = 0
		}
	}
}

// finish 把末尾不足一块的样本补 0 算完最后一帧。
func (d *onsetDetector) finish() {
	if d.done {
		return
	}
	d.done = true
	if d.pending > 0 {
		for i := beatWindow - beatHop + d.pending; i < beatWindow; i++ {
			d.frame[i] = 0
		}
		d.process()
		d.pending = 0
	}
}

func (d *onsetDetector) process() {
	for i, v := range d.frame {
		d.windowed[i] = v * d.window[i]
	}
	d.plan.power(d.windowed, d.power)
	scale := 1.0
	if d.frames < len(d.coverage) {
		scale = 1 / d.coverage[d.frames]
	}
	d.frames++
	var flux, total float64
	for b, band := range d.mel {
		e := 0.0
		for j, w := range band.weights {
			e += w * d.power[band.start+j]
		}
		e *= scale
		total += e
		l := 10 * math.Log10(math.Max(e, beatPowerFloor))
		// ⚠️ 第 0 帧不算通量：把「流开始之前」当静音的话，凡是以底噪开头的音频都会在 0ms 凭空多一个
		// 巨大的起音 —— 它会多出一拍、把强拍相位整体错开，还会经自相关把「第一下鼓点的位置」误当成周期
		// （实测 60 / 90 BPM 都被判成 117 BPM）。开头那一下鼓点照样能检出：它在后面几帧里继续变强（窗在填满）。
		if diff := l - d.prevLog[b]; d.started && diff > 0 {
			flux += diff
		}
		d.prevLog[b] = l
	}
	d.started = true
	d.flux = append(d.flux, flux/float64(len(d.mel)))
	d.energy = append(d.energy, 10*math.Log10(math.Max(total, beatPowerFloor)))
	copy(d.frame, d.frame[beatHop:])
}

// melFilterbank 三角梅尔滤波器组（HTK 刻度），稀疏存储：每带只存有权重的那一段频点。
func melFilterbank(nfft, sr, bands int, fmin, fmax float64) []melBand {
	toMel := func(f float64) float64 { return 2595 * math.Log10(1+f/700) }
	toHz := func(m float64) float64 { return 700 * (math.Pow(10, m/2595) - 1) }
	lo, hi := toMel(fmin), toMel(fmax)
	pts := make([]float64, bands+2)
	for i := range pts {
		pts[i] = toHz(lo + (hi-lo)*float64(i)/float64(bands+1))
	}
	binHz := float64(sr) / float64(nfft)
	out := make([]melBand, bands)
	for b := 0; b < bands; b++ {
		f0, fc, f1 := pts[b], pts[b+1], pts[b+2]
		var band melBand
		band.start = -1
		for k := 0; k <= nfft/2; k++ {
			f := float64(k) * binHz
			var w float64
			switch {
			case f > f0 && f < fc:
				w = (f - f0) / (fc - f0)
			case f >= fc && f < f1:
				w = (f1 - f) / (f1 - fc)
			}
			if w <= 0 {
				if band.start >= 0 {
					break
				}
				continue
			}
			if band.start < 0 {
				band.start = k
			}
			band.weights = append(band.weights, w)
		}
		if band.start < 0 {
			// 低频带可能窄于一个频点：取离中心最近的那一个，保证每带都有值。
			band.start = int(math.Round(fc / binHz))
			band.weights = []float64{1}
		}
		out[b] = band
	}
	return out
}

// frameMs 帧 t 的时间：新样本块的起点 t·512（毫秒，四舍五入）。
//
// ⚠️ 为什么不取窗中心：起音进入窗口时先落在窗的右端（Hann 权重很小），谱通量的跳变出现在它第一次
// 进入窗口的那一帧（干净时）或下一帧（有底噪、右端权重太小被淹掉时）。按新样本块起点记时间，
// 两种情况的偏差分别落在约 (−23ms, 0] 与 (0, +17ms]；取窗中心的话会系统性地早约 46ms。
func frameMs(t int) int {
	return int(math.Round(float64(t) * beatHop * 1000 / BeatsSampleRate))
}

// ---------------------------------------------------------------- 汇总

func (d *onsetDetector) result() BeatResult {
	d.finish()
	return analyzeOnsets(d.flux, d.energy, float64(BeatsSampleRate)/beatHop)
}

// analyzeOnsets 由起音包络（谱通量）与逐帧能量算出节拍。fps 是帧率（≈ 43.07）。
func analyzeOnsets(flux, energy []float64, fps float64) BeatResult {
	n := len(flux)
	if n < int(beatMinSeconds*fps) {
		return emptyBeats()
	}
	mean, std := meanStd(flux)
	if std < 1e-9 {
		return emptyBeats()
	}
	period, conf := estimateTempo(flux, mean, fps)
	if period <= 0 {
		return emptyBeats()
	}
	local := make([]float64, n)
	for i, v := range flux {
		local[i] = v / std
	}
	local = gaussianSmooth(local, period/32)
	frames := trackBeats(local, period, beatTightness)
	frames = trimBeats(frames, energy)
	onsets := 0
	for _, f := range frames {
		if beatStrength(f, energy) >= beatOnsetRiseDB {
			onsets++
		}
	}
	if len(frames) < 2 || onsets < beatMinOnsetBeats {
		return emptyBeats()
	}
	// ⚠️ 置信度再乘以「落在真实起音上的拍」的比例：真实周期落在 60~200 BPM 之外时（实测 42 bpm 的心跳），
	// 自相关只看得到范围内的那一个峰，主次峰之比照样是 1；而跟踪器为了凑周期在静音里摆了一大半的拍。
	// 只看比值的话，一份明显不对的结果会带着 1.0 的置信度交给卡点。
	conf *= float64(onsets) / float64(len(frames))
	out := BeatResult{BeatsMs: make([]int, len(frames)), DownbeatsMs: []int{}, DownbeatApprox: true,
		Confidence: math.Round(conf*1000) / 1000}
	for i, f := range frames {
		out.BeatsMs[i] = frameMs(f)
	}
	out.BPM = regressionBPM(out.BeatsMs)
	phase := downbeatPhase(frames, energy)
	for i, f := range frames {
		if i%4 == phase {
			out.DownbeatsMs = append(out.DownbeatsMs, frameMs(f))
		}
	}
	return out
}

func meanStd(x []float64) (float64, float64) {
	var s float64
	for _, v := range x {
		s += v
	}
	mean := s / float64(len(x))
	var v2 float64
	for _, v := range x {
		v2 += (v - mean) * (v - mean)
	}
	return mean, math.Sqrt(v2 / float64(len(x)))
}

// ---------------------------------------------------------------- 速度

// estimateTempo 返回拍周期（帧，可带小数）与置信度（0~1）。没有周期性时返回 0。
//
// TPS(k) = Rs(k) × 先验(k)，Rs 是去均值包络的无偏自相关在相邻滞后上的平滑；
// TPS2(k) = TPS(k) + 0.5·TPS(2k) + 0.25·TPS(2k−1) + 0.25·TPS(2k+1)（Ellis 2007）：
// 周期 k 另得 2k 处的证据。纯粹的点击序列在 k、2k、3k…都有同样强的自相关，只看先验的话
// 174 BPM 会被判成 87 BPM（87 离 120 更近）；TPS2 让真正的周期多拿一份 2k 的分，把它拉回来。
func estimateTempo(flux []float64, mean, fps float64) (float64, float64) {
	n := len(flux)
	// 滞后取整后落在 60~200 BPM 里（200 BPM ≈ 12.9 帧，取 13；60 BPM ≈ 43.1 帧，多留一帧给插值）。
	lagMin := int(math.Ceil(60 * fps / beatMaxBPM))
	lagMax := int(math.Floor(60*fps/beatMinBPM)) + 1
	maxLag := 2*(lagMax+1) + 1
	if n <= maxLag+1 {
		return 0, 0
	}
	o := make([]float64, n)
	for i, v := range flux {
		o[i] = v - mean
	}
	r := make([]float64, maxLag+1)
	for k := 1; k <= maxLag; k++ {
		var s float64
		for t := 0; t+k < n; t++ {
			s += o[t] * o[t+k]
		}
		r[k] = s / float64(n-k)
	}
	prior := func(k float64) float64 {
		z := math.Log2(60*fps/k/beatPriorBPM) / beatPriorOct
		return math.Exp(-0.5 * z * z)
	}
	// rs：自相关在相邻滞后上做 [0.5, 1, 0.5] 平滑。⚠️ 帧长约 23ms，真实周期落在两个整数滞后之间时
	// （190 BPM ≈ 13.6 帧），它的能量被 13、14 两格瓜分，而 2 倍周期（27.2 帧）几乎全落在 27 上 ——
	// 不平滑的话，真周期先天吃亏，再乘上偏向 120 BPM 的先验就被判成半速（实测 190 → 95）。
	rs := func(k int) float64 {
		if k < 1 || k > maxLag {
			return 0
		}
		v := r[k]
		if k-1 >= 1 {
			v += 0.5 * r[k-1]
		}
		if k+1 <= maxLag {
			v += 0.5 * r[k+1]
		}
		return v
	}
	tps := func(k int) float64 {
		if k < 1 || k > maxLag {
			return 0
		}
		return rs(k) * prior(float64(k))
	}
	tps2 := func(k int) float64 { return tps(k) + 0.5*tps(2*k) + 0.25*tps(2*k-1) + 0.25*tps(2*k+1) }

	best, bestV := -1, math.Inf(-1)
	for k := lagMin; k <= lagMax; k++ {
		if v := tps2(k); v > bestV {
			best, bestV = k, v
		}
	}
	if best < 0 || bestV <= 0 {
		return 0, 0
	}
	// 小数周期：在基础 TPS 上做抛物线插值（整数滞后在 174 BPM 一档会差出好几 BPM）。
	period := float64(best)
	if a, b, c := tps(best-1), tps(best), tps(best+1); b >= a && b >= c {
		if den := a - 2*b + c; den < 0 {
			delta := 0.5 * (a - c) / den
			period += math.Max(-0.5, math.Min(0.5, delta))
		}
	}

	// 置信度：主峰 = 所选周期 ±1 帧内的最大 TPS；次峰 = 不在所选周期的 1/2、1、2、3、4 倍附近的
	// TPS 局部极大里最大的一个（倍数关系的峰是同一个节奏的回声，不算「另一个候选」）。
	// conf = 1 − 次峰/主峰，夹到 [0, 1]：纯点击序列接近 1，白噪声接近 0。
	peak := math.Inf(-1)
	for k := best - 1; k <= best+1; k++ {
		peak = math.Max(peak, tps(k))
	}
	related := func(k int) bool {
		for _, m := range []float64{0.5, 1, 2, 3, 4} {
			c := m * period
			if math.Abs(float64(k)-c) <= math.Max(1.5, 0.06*c) {
				return true
			}
		}
		return false
	}
	second := 0.0
	for k := lagMin; k <= lagMax; k++ {
		v := tps(k)
		if v >= tps(k-1) && v >= tps(k+1) && !related(k) && v > second {
			second = v
		}
	}
	conf := 0.0
	if peak > 0 {
		conf = math.Max(0, math.Min(1, 1-second/peak))
	}
	return period, conf
}

// gaussianSmooth 高斯平滑（σ 以帧计，σ < 0.3 时原样返回）。
func gaussianSmooth(x []float64, sigma float64) []float64 {
	if sigma < 0.3 {
		return x
	}
	radius := int(math.Ceil(3 * sigma))
	kernel := make([]float64, 2*radius+1)
	var sum float64
	for i := range kernel {
		d := float64(i - radius)
		kernel[i] = math.Exp(-0.5 * d * d / (sigma * sigma))
		sum += kernel[i]
	}
	out := make([]float64, len(x))
	for i := range x {
		var s float64
		for j, w := range kernel {
			k := i + j - radius
			if k >= 0 && k < len(x) {
				s += w * x[k]
			}
		}
		out[i] = s / sum
	}
	return out
}

// ---------------------------------------------------------------- 拍点

// trackBeats 动态规划跟踪拍点（Ellis 2007），返回帧下标（升序）。
//
// C(t) = 起音(t) + max_{τ/2 ≤ t−p ≤ 2τ} [ C(p) − 紧度 ×(log((t−p)/τ))² ]。
// 前一拍只在 [τ/2, 2τ] 里找：停顿一小节时它**必须**在停顿里按周期放拍（外推），不会跳过去。
// 开头的静音不放拍：在第一个够强的起音（≥ 最大值的 1%）之前，链条不往回连。
// ⚠️ 前一拍可以落在时间 0 之前（虚拟的前一拍，只算间隔代价、累计得分记 0，回溯到它就停）：
// 不允许的话，开头第一拍只能硬连到一个代价很高的真实帧上，跟踪器宁可把第一拍挪到鼓点之后的
// 一个弱帧、再接一个 0ms 的噪声帧 —— 实测 90 BPM 把第一拍丢了。
// 最后一拍：累计得分的局部极大里、得分超过中位数一半的最后一个；从它回溯。
func trackBeats(local []float64, period, tightness float64) []int {
	n := len(local)
	if n == 0 || period <= 0 {
		return nil
	}
	maxScore := 0.0
	for _, v := range local {
		maxScore = math.Max(maxScore, v)
	}
	thresh := 0.01 * maxScore
	lo := int(math.Max(1, math.Round(period/2)))
	hi := int(math.Round(2 * period))
	cost := make([]float64, hi+1)
	for d := lo; d <= hi; d++ {
		l := math.Log(float64(d) / period)
		cost[d] = tightness * l * l
	}
	cum := make([]float64, n)
	back := make([]int, n)
	first := true
	for i := 0; i < n; i++ {
		best, bestJ := math.Inf(-1), -1
		for d := lo; d <= hi; d++ {
			c := -cost[d] // 虚拟的前一拍（在时间 0 之前）
			j := -1
			if i-d >= 0 {
				c, j = cum[i-d]-cost[d], i-d
			}
			if c > best {
				best, bestJ = c, j
			}
		}
		cum[i] = local[i] + best
		if first && local[i] < thresh {
			back[i] = -1
		} else {
			back[i] = bestJ
			first = false
		}
	}
	last := lastBeat(cum)
	if last < 0 {
		return nil
	}
	var beats []int
	for b := last; b >= 0; b = back[b] {
		beats = append(beats, b)
	}
	sort.Ints(beats)
	return beats
}

func lastBeat(cum []float64) int {
	var maxima []int
	for i := range cum {
		left := i == 0 || cum[i] > cum[i-1]
		right := i == len(cum)-1 || cum[i] >= cum[i+1]
		if left && right {
			maxima = append(maxima, i)
		}
	}
	if len(maxima) == 0 {
		return -1
	}
	vals := make([]float64, len(maxima))
	for i, m := range maxima {
		vals[i] = cum[m]
	}
	sort.Float64s(vals)
	med := vals[len(vals)/2]
	if len(vals)%2 == 0 {
		med = (vals[len(vals)/2-1] + vals[len(vals)/2]) / 2
	}
	for i := len(maxima) - 1; i >= 0; i-- {
		if 2*cum[maxima[i]] > med {
			return maxima[i]
		}
	}
	return maxima[len(maxima)-1]
}

// beatStrength 一拍的起音强度：拍点之后两帧内的能量峰，减去拍点之前三帧内的能量谷（dB，≥ 0）。
//
// ⚠️ 不直接用谱通量：通量的大小取决于起音落在窗里的哪个位置（Hann 窗右端权重趋近 0），
// 同样响的两拍通量能差出几十 dB，足以淹掉强拍那几 dB 的加重。能量峰在起音完全进窗之后才取，不受这个影响。
func beatStrength(f int, energy []float64) float64 {
	if f < 0 || f >= len(energy) {
		return 0
	}
	peak := math.Inf(-1)
	for t := f; t <= f+2 && t < len(energy); t++ {
		peak = math.Max(peak, energy[t])
	}
	valley := energy[f]
	for t := f - 3; t <= f-1; t++ {
		if t >= 0 {
			valley = math.Min(valley, energy[t])
		}
	}
	return math.Max(0, peak-valley)
}

// trimBeats 去掉首尾的弱拍（跟踪器在开头的底噪与结尾的余音里补出来的拍）；中间的不动 ——
// 停顿里按周期外推的拍是有意留着的。做法同 librosa：每拍强度用 [0.5, 1, 0.5] 平滑，
// 低于均方根一半的从两头剪掉。
func trimBeats(frames []int, energy []float64) []int {
	if len(frames) == 0 {
		return frames
	}
	s := make([]float64, len(frames))
	for i, f := range frames {
		s[i] = beatStrength(f, energy)
	}
	smooth := make([]float64, len(s))
	var sq float64
	for i := range s {
		v := s[i]
		if i > 0 {
			v += 0.5 * s[i-1]
		}
		if i+1 < len(s) {
			v += 0.5 * s[i+1]
		}
		smooth[i] = v
		sq += v * v
	}
	thr := 0.5 * math.Sqrt(sq/float64(len(smooth)))
	start, end := 0, len(frames)-1
	for start <= end && smooth[start] <= thr {
		start++
	}
	for end >= start && smooth[end] <= thr {
		end--
	}
	if start > end {
		return nil
	}
	return frames[start : end+1]
}

// downbeatPhase 按 4 拍一小节，取「该相位拍点上起音强度之和」最大的相位（0~3）。一律按 4/4 估。
func downbeatPhase(frames []int, energy []float64) int {
	var sums [4]float64
	for i, f := range frames {
		sums[i%4] += beatStrength(f, energy)
	}
	best := 0
	for p := 1; p < 4; p++ {
		if sums[p] > sums[best] {
			best = p
		}
	}
	return best
}

// regressionBPM 用拍点时间对序号做最小二乘，斜率就是平均拍长（毫秒）；保留两位小数。
// 比「60·帧率 / 周期帧数」准：周期帧数受帧长（约 23ms）量化，174 BPM 一档会差出好几 BPM。
func regressionBPM(beatsMs []int) float64 {
	n := len(beatsMs)
	if n < 2 {
		return 0
	}
	meanI := float64(n-1) / 2
	var meanT float64
	for _, t := range beatsMs {
		meanT += float64(t)
	}
	meanT /= float64(n)
	var num, den float64
	for i, t := range beatsMs {
		di := float64(i) - meanI
		num += di * (float64(t) - meanT)
		den += di * di
	}
	if den == 0 || num <= 0 {
		return 0
	}
	return math.Round(60000/(num/den)*100) / 100
}
