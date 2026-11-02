package ffbin

import (
	"context"
	"os/exec"
	"runtime"
	"testing"

	"github.com/li904338203/tabtv/backend/pkg/h264"
)

// 真实 ffmpeg：本机选出来的编码器按每一档都真的编得出来（没有 ffmpeg 的机器跳过）。
func TestEncodeArgs_真实ffmpeg每档都能编(t *testing.T) {
	if _, err := exec.LookPath(FFmpeg()); err != nil {
		t.Skip("本机没有 ffmpeg")
	}
	sel := h264.Detect(context.Background(), runtime.GOOS, execRunner)
	if sel.Encoder.Name == "" {
		t.Skipf("本机 ffmpeg 没有可用的 H.264 编码器：%+v", sel.Skipped)
	}
	for _, name := range append([]string{sel.Encoder.Name}, available(sel)...) {
		for _, tier := range []h264.Tier{h264.TierFinal, h264.TierIntermediate, h264.TierCut, h264.TierProxy} {
			args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-f", "lavfi", "-i", "testsrc=s=320x240:r=25:d=1"}
			args = append(args, h264.EncodeArgs(h264.Encoder{Name: name}, h264.Intent{Tier: tier, Width: 320, Height: 240, FPS: 25, GOP: 25, MaxRateK: 1200, BufSizeK: 2400})...)
			args = append(args, "-f", "mp4", "-y", t.TempDir()+"/o.mp4")
			if out, err := exec.Command(FFmpeg(), args...).CombinedOutput(); err != nil {
				t.Errorf("%s 档 %d 编码失败：%v\n%s", name, tier, err, out)
			}
		}
	}
}

// available 是除选中者之外、本机也能用的候选（逐个都验一遍参数映射）。
func available(sel h264.Selection) []string {
	out, err := exec.Command(FFmpeg(), "-hide_banner", "-encoders").CombinedOutput()
	if err != nil {
		return nil
	}
	have := h264.ParseEncoders(string(out))
	var list []string
	for _, c := range h264.Candidates(runtime.GOOS) {
		if c != sel.Encoder.Name && have[c] {
			if _, err := exec.Command(FFmpeg(), h264.TrialArgs(c)...).CombinedOutput(); err == nil {
				list = append(list, c)
			}
		}
	}
	return list
}
