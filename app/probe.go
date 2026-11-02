//go:build probe

package app

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// 三平台探针（TT-T-03，dev/05 V-1 / V-4）：只在 `-tags probe` 构建里存在。
//
// /api/_probe/video  用 http.ServeContent 吐一段测试视频（支持 Range / 206）
// /api/_probe/log    返回收到过的请求：是否带 Range、回了什么状态
//
// 测试视频首次请求时用 ffmpeg 现生成（约 60 秒 1080p），放在临时目录；
// 也可以用 TABTV_PROBE_VIDEO 指定一个现成文件。

type probeHit struct {
	At     time.Time `json:"at"`
	Range  string    `json:"range"`
	Status int       `json:"status"`
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

var (
	probeMu   sync.Mutex
	probeHits []probeHit
	probeOnce sync.Once
	probeFile string
	probeErr  error
)

func probeVideo() (string, error) {
	probeOnce.Do(func() {
		if p := os.Getenv("TABTV_PROBE_VIDEO"); p != "" {
			probeFile = p
			return
		}
		probeFile = filepath.Join(os.TempDir(), "tabtv-probe.mp4")
		if st, err := os.Stat(probeFile); err == nil && st.Size() > 0 {
			return
		}
		cmd := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "testsrc2=size=1920x1080:rate=30:duration=60",
			"-f", "lavfi", "-i", "sine=frequency=440:duration=60",
			"-c:v", "libx264", "-preset", "veryfast", "-b:v", "6M", "-pix_fmt", "yuv420p",
			"-c:a", "aac", "-shortest", "-movflags", "+faststart", probeFile)
		if out, err := cmd.CombinedOutput(); err != nil {
			probeErr = err
			_ = os.WriteFile(probeFile+".log", out, 0o644)
		}
	})
	return probeFile, probeErr
}

// WithProbe 在 api 前面挂上探针路由。
func WithProbe(api http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/_probe/video", func(w http.ResponseWriter, r *http.Request) {
		path, err := probeVideo()
		if err != nil {
			http.Error(w, "生成测试视频失败："+err.Error(), http.StatusInternalServerError)
			return
		}
		f, err := os.Open(path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer f.Close()
		st, _ := f.Stat()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		w.Header().Set("Content-Type", "video/mp4")
		http.ServeContent(rec, r, "probe.mp4", st.ModTime(), f)
		probeMu.Lock()
		probeHits = append(probeHits, probeHit{At: time.Now(), Range: r.Header.Get("Range"), Status: rec.status})
		probeMu.Unlock()
	})
	mux.HandleFunc("/api/_probe/log", func(w http.ResponseWriter, _ *http.Request) {
		probeMu.Lock()
		defer probeMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(probeHits)
	})
	// 前端探针页把结果 POST 过来：写到 TABTV_PROBE_OUT（缺省临时目录），然后退出进程，
	// 这样 CI 上可以无人值守地跑完一个平台。
	mux.HandleFunc("/api/_probe/report", func(w http.ResponseWriter, r *http.Request) {
		out := os.Getenv("TABTV_PROBE_OUT")
		if out == "" {
			out = filepath.Join(os.TempDir(), "tabtv-probe-result.json")
		}
		var rows any
		_ = json.NewDecoder(r.Body).Decode(&rows)
		probeMu.Lock()
		b, _ := json.MarshalIndent(map[string]any{"rows": rows, "hits": probeHits}, "", "  ")
		probeMu.Unlock()
		_ = os.WriteFile(out, b, 0o644)
		w.WriteHeader(http.StatusNoContent)
		if os.Getenv("TABTV_PROBE_EXIT") == "1" {
			go func() { time.Sleep(300 * time.Millisecond); os.Exit(0) }()
		}
	})
	mux.Handle("/", api)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		println("probe request:", r.Method, r.URL.Path, r.Header.Get("Range"))
		mux.ServeHTTP(w, r)
	})
}
