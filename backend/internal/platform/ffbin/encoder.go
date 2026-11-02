package ffbin

import (
	"context"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"github.com/li904338203/tabtv/backend/pkg/h264"
)

// 本机 H.264 编码器的探测结果缓存（TT-T-27）。选择规则与参数映射在 pkg/h264。

func execRunner(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, FFmpeg(), args...).CombinedOutput()
	return string(out), err
}

var (
	selMu  sync.Mutex
	selVal *h264.Selection
)

// H264 返回本机选中的编码器（首次调用时探测并缓存；设置里换了 ffmpeg 后重新探测）。
func H264() h264.Selection {
	selMu.Lock()
	defer selMu.Unlock()
	if selVal == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		s := h264.Detect(ctx, runtime.GOOS, execRunner)
		cancel()
		selVal = &s
	}
	return *selVal
}

// resetSelection 在 ffmpeg 路径变化时清掉缓存。
func resetSelection() {
	selMu.Lock()
	selVal = nil
	selMu.Unlock()
}
