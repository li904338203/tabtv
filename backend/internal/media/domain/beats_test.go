package domain

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/cmplx"
	"math/rand"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------- FFT

func directDFT(x []complex128) []complex128 {
	n := len(x)
	out := make([]complex128, n)
	for k := 0; k < n; k++ {
		var s complex128
		for j, v := range x {
			s += v * cmplx.Exp(complex(0, -2*math.Pi*float64(k*j)/float64(n)))
		}
		out[k] = s
	}
	return out
}

func TestFFT_与直接DFT一致(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for _, n := range []int{2, 8, 64, 256} {
		re, im := make([]float64, n), make([]float64, n)
		x := make([]complex128, n)
		for i := range re {
			re[i], im[i] = r.Float64()*2-1, r.Float64()*2-1
			x[i] = complex(re[i], im[i])
		}
		want := directDFT(x)
		newFFTPlan(n).transform(re, im)
		for k := range want {
			if math.Abs(re[k]-real(want[k])) > 1e-9 || math.Abs(im[k]-imag(want[k])) > 1e-9 {
				t.Fatalf("n=%d k=%d：FFT %v+%vi，DFT %v", n, k, re[k], im[k], want[k])
			}
		}
	}
}

// 已知频率的正弦波：功率谱的峰落在对应频点；Parseval：Σ|x|² = (1/N)·Σ|X|²。
func TestFFT_正弦波峰值与能量守恒(t *testing.T) {
	const n = 2048
	p := newRealPowerPlan(n)
	for _, bin := range []int{1, 37, 100, 511, 1000} {
		x := make([]float64, n)
		var energy float64
		for i := range x {
			x[i] = math.Sin(2 * math.Pi * float64(bin*i) / n)
			energy += x[i] * x[i]
		}
		power := make([]float64, n/2+1)
		p.power(x, power)
		peak := 0
		for k := range power {
			if power[k] > power[peak] {
				peak = k
			}
		}
		if peak != bin {
			t.Errorf("正弦波 %d：峰在 %d", bin, peak)
		}
		// 实数序列的全谱能量 = X[0]² + X[n/2]² + 2·Σ_{0<k<n/2}|X[k]|²
		spec := power[0] + power[n/2]
		for k := 1; k < n/2; k++ {
			spec += 2 * power[k]
		}
		if math.Abs(spec/n-energy) > 1e-6*energy {
			t.Errorf("正弦波 %d：Parseval 不成立 %v vs %v", bin, spec/n, energy)
		}
	}
	// 随机实数序列：与直接 DFT 的功率谱一致。
	r := rand.New(rand.NewSource(2))
	const m = 64
	x := make([]float64, m)
	cx := make([]complex128, m)
	for i := range x {
		x[i] = r.Float64()*2 - 1
		cx[i] = complex(x[i], 0)
	}
	want := directDFT(cx)
	got := make([]float64, m/2+1)
	newRealPowerPlan(m).power(x, got)
	for k := range got {
		w := real(want[k])*real(want[k]) + imag(want[k])*imag(want[k])
		if math.Abs(got[k]-w) > 1e-9*math.Max(1, w) {
			t.Fatalf("k=%d：%v vs %v", k, got[k], w)
		}
	}
}

// ---------------------------------------------------------------- 合成节拍音频

type synthOpts struct {
	bpm         float64
	seconds     float64 // 总长
	startSec    float64 // 开头静音
	jitterMs    float64 // 每拍 ±jitter 的均匀抖动
	noiseDB     float64 // 白噪声（相对打击峰值 1）；0 = 没有
	accentPhase int     // ≥ 0 时每 4 拍加重一次（下标 ≡ accentPhase mod 4 的那一拍）
	accentGain  float64
	muteFrom    int // [muteFrom, muteTo) 的拍不发声（中途停顿）
	muteTo      int
	offbeatGain float64 // > 0 时每两拍之间加一个弱的反拍
	seed        int64
}

// synthBeats 生成合成节拍：每拍一个「低频鼓 + 宽带点击」（峰值约 1），返回样本、每拍的真实时间（秒，含停顿里
// 不发声的格点）与是否发声。
func synthBeats(o synthOpts) ([]float32, []float64, []bool) {
	r := rand.New(rand.NewSource(o.seed))
	n := int(o.seconds * BeatsSampleRate)
	x := make([]float64, n)
	if o.noiseDB < 0 {
		sigma := math.Pow(10, o.noiseDB/20)
		for i := range x {
			x[i] = r.NormFloat64() * sigma
		}
	}
	addHit := func(at, gain float64) {
		start := int(at * BeatsSampleRate)
		for i := 0; i < BeatsSampleRate/4 && start+i < n; i++ {
			if start+i < 0 {
				continue
			}
			tt := float64(i) / BeatsSampleRate
			kick := 0.6 * math.Sin(2*math.Pi*120*tt) * math.Exp(-tt/0.05)
			click := 0.4 * (r.Float64()*2 - 1) * math.Exp(-tt/0.005)
			x[start+i] += gain * (kick + click)
		}
	}
	period := 60 / o.bpm
	var times []float64
	var sounding []bool
	for k := 0; ; k++ {
		at := o.startSec + float64(k)*period
		if at > o.seconds-0.3 {
			break
		}
		at += (r.Float64()*2 - 1) * o.jitterMs / 1000
		on := !(k >= o.muteFrom && k < o.muteTo)
		times = append(times, at)
		sounding = append(sounding, on)
		if !on {
			continue
		}
		gain := 1.0
		if o.accentPhase >= 0 && k%4 == o.accentPhase {
			gain = o.accentGain
		}
		addHit(at, gain)
		if o.offbeatGain > 0 {
			addHit(at+period/2, o.offbeatGain)
		}
	}
	out := make([]float32, n)
	for i, v := range x {
		out[i] = float32(v)
	}
	return out, times, sounding
}

func pcmBytes(x []float32) []byte {
	b := make([]byte, 4*len(x))
	for i, v := range x {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(v))
	}
	return b
}

// matchBeats 配对口径：对每个真实拍点找最近的检出拍点（各拍相距 ≥ 300ms，最近邻即一一对应），
// 不做整体平移（不减去平均偏差）。返回逐拍偏差（毫秒，检出 − 真实）。
func matchBeats(detected []int, truth []float64) []float64 {
	errs := make([]float64, len(truth))
	for i, tr := range truth {
		trMs := tr * 1000
		best := math.Inf(1)
		for _, d := range detected {
			if e := float64(d) - trMs; math.Abs(e) < math.Abs(best) {
				best = e
			}
		}
		errs[i] = best
	}
	return errs
}

func errStats(errs []float64) (mean, maxAbs float64) {
	for _, e := range errs {
		mean += e
		maxAbs = math.Max(maxAbs, math.Abs(e))
	}
	return mean / float64(len(errs)), maxAbs
}

// ---------------------------------------------------------------- 速度与拍点

// 合成节拍（含 ±5ms 抖动与 −20dB 白噪声，每档 3 个随机种子）：BPM 误差 ≤ 1；拍点偏差 ≤ 33ms（30fps 下一帧）；
// 检出的拍数与真实拍数一致（不多不少，也就没有判成两倍或一半）。
func TestDetectBeats_合成节拍的速度与拍点(t *testing.T) {
	for _, bpm := range []float64{60, 90, 120, 128, 174} {
		for seed := int64(1); seed <= 3; seed++ {
			x, truth, _ := synthBeats(synthOpts{bpm: bpm, seconds: 30, startSec: 0.5, jitterMs: 5, noiseDB: -20,
				accentPhase: -1, muteFrom: -1, muteTo: -1, seed: seed})
			got := detectBeatsSamples(x)
			if math.Abs(got.BPM-bpm) > 1 {
				t.Errorf("%v BPM（种子 %d）：检出 %v BPM", bpm, seed, got.BPM)
				continue
			}
			errs := matchBeats(got.BeatsMs, truth)
			mean, maxAbs := errStats(errs)
			if maxAbs > 33 {
				t.Errorf("%v BPM（种子 %d）：最大偏差 %.1fms（平均 %+.1fms）", bpm, seed, maxAbs, mean)
			}
			if len(got.BeatsMs) != len(truth) {
				t.Errorf("%v BPM（种子 %d）：检出 %d 拍，实际 %d 拍", bpm, seed, len(got.BeatsMs), len(truth))
			}
			if got.Confidence < 0.5 {
				t.Errorf("%v BPM（种子 %d）：纯节拍的置信度应当高，实际 %v", bpm, seed, got.Confidence)
			}
			t.Logf("%5.0f BPM 种子 %d：检出 %.2f BPM，误差 %+.2f；拍点平均偏差 %+5.1fms、最大 %4.1fms；置信度 %.3f；%d 拍",
				bpm, seed, got.BPM, got.BPM-bpm, mean, maxAbs, got.Confidence, len(got.BeatsMs))
		}
	}
}

// 倍速与半速：纯点击序列在 k、2k、3k…都有同样强的自相关，只靠先验会把 174 判成 87、把 60 判成 120（反过来不会）；
// 带弱反拍的 100 BPM 不该被判成 200（反拍是细分，不是拍）。
func TestDetectBeats_不判成两倍或一半(t *testing.T) {
	cases := []struct {
		name    string
		opts    synthOpts
		bpm     float64
		notBPMs []float64
	}{
		{"174 不判成 87（半速）", synthOpts{bpm: 174}, 174, []float64{87}},
		// 180：只靠先验加权的自相关（不加 2 倍周期的证据）判成 90 —— 钉住 TPS2。
		{"180 不判成 90（半速）", synthOpts{bpm: 180}, 180, []float64{90}},
		// 190：真周期（13.6 帧）被两个整数滞后瓜分，不在相邻滞后上平滑自相关就判成 95 —— 钉住平滑。
		{"190 不判成 95（半速）", synthOpts{bpm: 190}, 190, []float64{95}},
		{"60 不判成 120（倍速）", synthOpts{bpm: 60}, 60, []float64{120, 30}},
		{"120 不判成 60 或 240", synthOpts{bpm: 120}, 120, []float64{60, 240}},
		{"100 BPM 带弱反拍不判成 200", synthOpts{bpm: 100, offbeatGain: 0.25}, 100, []float64{200, 50}},
	}
	for _, c := range cases {
		o := c.opts
		o.seconds, o.startSec, o.jitterMs, o.noiseDB, o.accentPhase, o.muteFrom, o.muteTo, o.seed =
			30, 0.5, 5, -20, -1, -1, -1, 7
		x, _, _ := synthBeats(o)
		got := detectBeatsSamples(x)
		if math.Abs(got.BPM-c.bpm) > 1 {
			t.Errorf("%s：检出 %v BPM", c.name, got.BPM)
		}
		for _, bad := range c.notBPMs {
			if math.Abs(got.BPM-bad) <= 2 {
				t.Errorf("%s：判成了 %v", c.name, got.BPM)
			}
		}
	}
}

// 开头 2 秒静音、中途停顿一小节：开头不放拍；停顿前后的拍一个不丢（偏差 ≤ 33ms）；停顿里按周期外推出拍。
// 口径：停顿里的格点照样要有检出拍，但它们是外推的（跟踪器在没有起音的地方按整数帧摆拍），
// 偏差放宽到 2 帧（46ms）。
func TestDetectBeats_开头静音与中途停顿不丢拍(t *testing.T) {
	for _, bpm := range []float64{90, 120} {
		x, truth, sounding := synthBeats(synthOpts{bpm: bpm, seconds: 30, startSec: 2, jitterMs: 5, noiseDB: -40,
			accentPhase: -1, muteFrom: 16, muteTo: 20, seed: 3})
		got := detectBeatsSamples(x)
		if math.Abs(got.BPM-bpm) > 1 {
			t.Fatalf("%v BPM：检出 %v BPM", bpm, got.BPM)
		}
		for _, d := range got.BeatsMs {
			if float64(d) < 2000-33 {
				t.Errorf("%v BPM：开头静音里不该有拍（%dms）", bpm, d)
			}
		}
		errs := matchBeats(got.BeatsMs, truth)
		var gapMax, playMax float64
		for i, e := range errs {
			if sounding[i] {
				playMax = math.Max(playMax, math.Abs(e))
			} else {
				gapMax = math.Max(gapMax, math.Abs(e))
			}
		}
		if playMax > 33 || gapMax > 46 {
			t.Errorf("%v BPM：发声拍最大偏差 %.1fms、停顿里外推的拍最大偏差 %.1fms", bpm, playMax, gapMax)
		}
		if len(got.BeatsMs) != len(truth) {
			t.Errorf("%v BPM：检出 %d 拍，格点 %d 个（含停顿里的 4 个）", bpm, len(got.BeatsMs), len(truth))
		}
		t.Logf("%v BPM：发声拍最大偏差 %.1fms，停顿里外推的拍最大偏差 %.1fms", bpm, playMax, gapMax)
	}
}

// 强拍：每 4 拍加重一次（+6dB），加重相位 0 / 1 / 2 / 3 四种都判对 —— 检出的强拍逐个落在加重的那一拍上。
func TestDetectBeats_强拍相位(t *testing.T) {
	for phase := 0; phase < 4; phase++ {
		x, truth, _ := synthBeats(synthOpts{bpm: 120, seconds: 30, startSec: 0.5, jitterMs: 5, noiseDB: -20,
			accentPhase: phase, accentGain: 2, muteFrom: -1, muteTo: -1, seed: int64(10 + phase)})
		got := detectBeatsSamples(x)
		var accented []float64
		for k, tr := range truth {
			if k%4 == phase {
				accented = append(accented, tr)
			}
		}
		if !got.DownbeatApprox || len(got.DownbeatsMs) != len(accented) {
			t.Errorf("相位 %d：检出 %d 个强拍，加重了 %d 拍", phase, len(got.DownbeatsMs), len(accented))
			continue
		}
		_, maxAbs := errStats(matchBeats(got.DownbeatsMs, accented))
		if maxAbs > 33 {
			t.Errorf("相位 %d：强拍没落在加重的拍上（最大偏差 %.1fms）", phase, maxAbs)
		}
	}
}

// 空输入、全静音、太短、没有节奏的长音：空拍点、置信度 0，不报错；JSON 里是 [] 不是 null。
func TestDetectBeats_没有节拍时返回空(t *testing.T) {
	tone := make([]float32, 10*BeatsSampleRate)
	for i := range tone {
		tone[i] = float32(0.5 * math.Sin(2*math.Pi*440*float64(i)/BeatsSampleRate))
	}
	short, _, _ := synthBeats(synthOpts{bpm: 120, seconds: 2, accentPhase: -1, muteFrom: -1, muteTo: -1, seed: 1})
	for name, x := range map[string][]float32{
		"空":        nil,
		"全静音 10 秒": make([]float32, 10*BeatsSampleRate),
		"2 秒太短":    short,
		"持续的长音":    tone,
	} {
		got, err := DetectBeats(bytes.NewReader(pcmBytes(x)))
		if err != nil {
			t.Fatalf("%s：不该报错：%v", name, err)
		}
		if len(got.BeatsMs) != 0 || got.BPM != 0 || got.Confidence != 0 {
			t.Errorf("%s：应当没有节拍，实际 %+v", name, got)
		}
		raw, _ := json.Marshal(got)
		if !strings.Contains(string(raw), `"beatsMs":[]`) || !strings.Contains(string(raw), `"downbeatsMs":[]`) {
			t.Errorf("%s：空列表应序列化成 []：%s", name, raw)
		}
	}
}

// 结果的 JSON 形状与 dev/02 §2 一致：{bpm, beatsMs[], downbeatsMs[], downbeatApprox:true, confidence}。
func TestBeatResult_JSON形状(t *testing.T) {
	x, _, _ := synthBeats(synthOpts{bpm: 120, seconds: 10, startSec: 0.5, accentPhase: 0, accentGain: 2,
		muteFrom: -1, muteTo: -1, seed: 4})
	raw, err := json.Marshal(detectBeatsSamples(x))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	want := []string{"beatsMs", "bpm", "confidence", "downbeatApprox", "downbeatsMs"}
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != strings.Join(want, ",") || m["downbeatApprox"] != true {
		t.Fatalf("键不对：%s", raw)
	}
	for _, k := range []string{"beatsMs", "downbeatsMs"} {
		for _, v := range m[k].([]any) {
			if f := v.(float64); f != math.Trunc(f) {
				t.Fatalf("%s 必须是整数毫秒：%v", k, v)
			}
		}
	}
}

// 流式读取：与内存里一次算的结果逐项相同；末尾不足 4 字节的残片丢弃；读出错原样返回。
func TestDetectBeats_流式读取(t *testing.T) {
	x, _, _ := synthBeats(synthOpts{bpm: 128, seconds: 12, startSec: 0.3, jitterMs: 5, noiseDB: -20,
		accentPhase: -1, muteFrom: -1, muteTo: -1, seed: 5})
	want := detectBeatsSamples(x)
	raw := append(pcmBytes(x), 0x01, 0x02) // 两个多余字节
	got, err := DetectBeats(&slowReader{data: raw, step: 777})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(want)
	b, _ := json.Marshal(got)
	if string(a) != string(b) {
		t.Fatalf("流式与一次算的结果不同：\n%s\n%s", b, a)
	}
	boom := errors.New("读坏了")
	if _, err := DetectBeats(io.MultiReader(bytes.NewReader(raw[:4000]), &failReader{err: boom})); !errors.Is(err, boom) {
		t.Fatalf("读出错应原样返回，实际 %v", err)
	}
	// NaN / Inf 样本当 0 处理，不让结果变成 NaN。
	bad := append([]float32{}, x...)
	bad[1000], bad[2000] = float32(math.NaN()), float32(math.Inf(1))
	if r := detectBeatsSamples(bad); math.IsNaN(r.BPM) || math.IsNaN(r.Confidence) {
		t.Fatalf("NaN 样本把结果带坏了：%+v", r)
	}
}

type slowReader struct {
	data []byte
	step int
}

func (s *slowReader) Read(p []byte) (int, error) {
	if len(s.data) == 0 {
		return 0, io.EOF
	}
	n := min(len(p), min(s.step, len(s.data)))
	copy(p, s.data[:n])
	s.data = s.data[n:]
	return n, nil
}

type failReader struct{ err error }

func (f *failReader) Read([]byte) (int, error) { return 0, f.err }

// 纯度绊线：节拍检测不碰库、不发网络（与 timeline/domain 的 purity_test 同一条）。
func TestBeats_不依赖数据库与网络(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Skipf("go list 不可用：%v", err)
	}
	for _, dep := range strings.Fields(string(out)) {
		switch {
		case dep == "database/sql", dep == "net", dep == "net/http", strings.Contains(dep, "jackc/pgx"),
			strings.HasPrefix(dep, "github.com/li904338203/tabtv/backend/internal/") && !pureLeaf(dep):
			t.Errorf("media/domain 依赖了 %s", dep)
		}
	}
}

// pureLeaf：只依赖标准库、不碰库不发网络的叶子包（ffbin 定位随包的 ffmpeg，TT-T-26）。
func pureLeaf(dep string) bool {
	switch dep {
	case "github.com/li904338203/tabtv/backend/internal/media/domain",
		"github.com/li904338203/tabtv/backend/internal/platform/ffbin",
		"github.com/li904338203/tabtv/backend/internal/platform/resources":
		return true
	}
	return false
}

// 性能：3 分钟的合成音频走流式读取，记一次耗时与分配（写进执行记录）。PCM 由读取器边读边生成，
// 不在内存里先放一整份，量到的就是检测器自己的开销。
func TestDetectBeats_三分钟音频的耗时与内存(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过")
	}
	x, _, _ := synthBeats(synthOpts{bpm: 128, seconds: 180, startSec: 0.5, jitterMs: 5, noiseDB: -20,
		accentPhase: -1, muteFrom: -1, muteTo: -1, seed: 6})
	raw := pcmBytes(x)
	x = nil
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	got, err := DetectBeats(bytes.NewReader(raw))
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)
	if err != nil || math.Abs(got.BPM-128) > 1 {
		t.Fatalf("3 分钟音频：%v %+v", err, got.BPM)
	}
	t.Logf("3 分钟（%d 个样本、%.1fMB PCM）：耗时 %v，检测器分配 %.1fMB（%d 次），检出 %.2f BPM、%d 拍",
		len(raw)/4, float64(len(raw))/1e6, elapsed.Round(time.Millisecond),
		float64(after.TotalAlloc-before.TotalAlloc)/1e6, after.Mallocs-before.Mallocs, got.BPM, len(got.BeatsMs))
}
