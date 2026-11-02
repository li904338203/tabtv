// Package ffbin 是 ffmpeg / ffprobe 路径的**唯一**来源（dev/04 TTD-50，TT-T-26）。
//
// 来源系统在 8 个文件里各自 `exec.Command("ffmpeg")`，靠 PATH 找程序：服务器上装好 ffmpeg 就行。
// 桌面应用要用随包附带的那一份 —— 各自去找的话，漏改一处就会在某台没装 ffmpeg 的机器上静默失败，
// 而症状分别是「渲染失败」「没有封面」「剪辑台没有波形」，看起来毫不相干。
//
// 查找顺序：设置里指定的路径（设置 → 常规）→ 随包 resources/bin → 开发态 build/third_party/ffmpeg/<平台>/bin → PATH。
// ⚠️ 静态检查 ffbin_test.go 保证 internal/ 下不再出现 `exec.Command*("ffmpeg"` 字面量。
package ffbin

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/li904338203/tabtv/backend/internal/platform/resources"
)

var (
	mu       sync.RWMutex
	override = map[string]string{}
	cache    = map[string]string{}
)

// SetOverride 指定某个程序的路径（设置表的 ffmpeg.path / ffprobe.path）；空串取消指定。
func SetOverride(name, path string) {
	mu.Lock()
	if path == "" {
		delete(override, name)
	} else {
		override[name] = path
	}
	delete(cache, name)
	mu.Unlock()
	// 换了 ffmpeg 就换了一套编码器，下次用时重新探测。
	// ⚠️ 放在释放 mu 之后：H264() 持有 selMu 时会经 FFmpeg() 去拿 mu，反过来嵌套就会死锁。
	if name == "ffmpeg" {
		resetSelection()
	}
}

func exe(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// Path 返回程序的可执行路径；找不到时返回程序名本身（交给 exec 报「找不到」，错误信息里带着名字）。
func Path(name string) string {
	mu.RLock()
	if p, ok := override[name]; ok {
		mu.RUnlock()
		return p
	}
	if p, ok := cache[name]; ok {
		mu.RUnlock()
		return p
	}
	mu.RUnlock()

	p := resolve(name)
	mu.Lock()
	cache[name] = p
	mu.Unlock()
	return p
}

func resolve(name string) string {
	candidates := []string{}
	if root := resources.Root(); root != "" {
		candidates = append(candidates, filepath.Join(root, "bin", exe(name)))
	}
	if dir := resources.ThirdParty("ffmpeg"); dir != "" {
		candidates = append(candidates, filepath.Join(dir, "bin", exe(name)), filepath.Join(dir, exe(name)))
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return name
}

// FFmpeg / FFprobe 是两个常用程序的路径。
func FFmpeg() string  { return Path("ffmpeg") }
func FFprobe() string { return Path("ffprobe") }

// Available 报告程序能不能找到并执行。
func Available(name string) bool {
	p := Path(name)
	if filepath.IsAbs(p) {
		st, err := os.Stat(p)
		return err == nil && !st.IsDir()
	}
	_, err := exec.LookPath(p)
	return err == nil
}
