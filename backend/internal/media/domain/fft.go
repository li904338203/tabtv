package domain

import (
	"math"
	"math/bits"
)

// 节拍检测用的 FFT（docs/agent/smartcut SCT-17）。
//
// 自己写 radix-2、不引入数值库：节拍检测只要固定长度（2048）的实数功率谱，一个迭代式的
// 按时间抽取 FFT 加上「实数序列打包成一半长度的复数序列」的技巧就够了，为它多一个依赖不值。

// fftPlan 是某个长度（2 的幂）的复数 FFT 的预计算表：位反转下标与旋转因子。
type fftPlan struct {
	n   int
	rev []int
	cos []float64 // cos(2πk/n)，k < n/2
	sin []float64 // sin(2πk/n)，k < n/2
}

func newFFTPlan(n int) *fftPlan {
	if n < 2 || n&(n-1) != 0 {
		panic("fft: 长度必须是 2 的幂")
	}
	p := &fftPlan{n: n, rev: make([]int, n), cos: make([]float64, n/2), sin: make([]float64, n/2)}
	shift := bits.UintSize - bits.Len(uint(n-1))
	for i := range p.rev {
		p.rev[i] = int(bits.Reverse(uint(i)) >> shift)
	}
	for k := 0; k < n/2; k++ {
		a := 2 * math.Pi * float64(k) / float64(n)
		p.cos[k], p.sin[k] = math.Cos(a), math.Sin(a)
	}
	return p
}

// transform 原地做正向 FFT：X[k] = Σ x[n]·e^{-2πikn/N}。re / im 长度都必须等于 n。
func (p *fftPlan) transform(re, im []float64) {
	n := p.n
	for i, j := range p.rev {
		if i < j {
			re[i], re[j] = re[j], re[i]
			im[i], im[j] = im[j], im[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		half := size >> 1
		step := n / size
		for start := 0; start < n; start += size {
			for k := 0; k < half; k++ {
				// 旋转因子 e^{-2πik/size} = cos − i·sin（表按 n 建，步长 n/size）。
				wr, wi := p.cos[k*step], -p.sin[k*step]
				a, b := start+k, start+k+half
				tr := wr*re[b] - wi*im[b]
				ti := wr*im[b] + wi*re[b]
				re[b], im[b] = re[a]-tr, im[a]-ti
				re[a], im[a] = re[a]+tr, im[a]+ti
			}
		}
	}
}

// realPowerPlan 求长度 n（2 的幂、≥ 4）实数序列的功率谱 |X[k]|²（k = 0..n/2）。
//
// 把实数序列两两打包成 n/2 个复数 z[m] = x[2m] + i·x[2m+1]，做一次 n/2 点的复数 FFT，
// 再拆回 X[k] = (Z[k] + Z*[n/2−k])/2 − i·e^{-2πik/n}·(Z[k] − Z*[n/2−k])/2 —— 计算量是直接做 n 点复数 FFT 的一半。
type realPowerPlan struct {
	n      int
	half   *fftPlan
	cos    []float64 // cos(2πk/n)，k ≤ n/2
	sin    []float64
	re, im []float64
}

func newRealPowerPlan(n int) *realPowerPlan {
	if n < 4 || n&(n-1) != 0 {
		panic("fft: 长度必须是 ≥ 4 的 2 的幂")
	}
	p := &realPowerPlan{n: n, half: newFFTPlan(n / 2), cos: make([]float64, n/2+1), sin: make([]float64, n/2+1),
		re: make([]float64, n/2), im: make([]float64, n/2)}
	for k := 0; k <= n/2; k++ {
		a := 2 * math.Pi * float64(k) / float64(n)
		p.cos[k], p.sin[k] = math.Cos(a), math.Sin(a)
	}
	return p
}

// power 把 x（长度 n）的功率谱写进 out（长度 n/2+1）。
func (p *realPowerPlan) power(x []float64, out []float64) {
	h := p.n / 2
	for m := 0; m < h; m++ {
		p.re[m], p.im[m] = x[2*m], x[2*m+1]
	}
	p.half.transform(p.re, p.im)
	for k := 0; k <= h; k++ {
		// Z[k] 与 Z[h−k] 的共轭（Z[h] 记作 Z[0]）。
		zr, zi := p.re[k%h], p.im[k%h]
		cr, ci := p.re[(h-k)%h], -p.im[(h-k)%h]
		// 偶数部 E = (Z[k] + Z*[h−k])/2，奇数部 O = (Z[k] − Z*[h−k])/(2i)。
		er, ei := (zr+cr)/2, (zi+ci)/2
		odr, odi := (zi-ci)/2, -(zr-cr)/2
		// X[k] = E + e^{-2πik/n}·O
		wr, wi := p.cos[k], -p.sin[k]
		xr := er + wr*odr - wi*odi
		xi := ei + wr*odi + wi*odr
		out[k] = xr*xr + xi*xi
	}
}

// hannWindow 周期 Hann 窗：w[n] = 0.5 − 0.5·cos(2πn/N)（与常见 STFT 实现一致）。
func hannWindow(n int) []float64 {
	w := make([]float64, n)
	for i := range w {
		w[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n))
	}
	return w
}
