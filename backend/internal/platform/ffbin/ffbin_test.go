package ffbin

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 静态检查：internal/ 下不允许再直接以命令名调用 ffmpeg / ffprobe（必须经 ffbin），
// 否则随包的 ffmpeg 在那一处用不上，而那一处的失败看起来与 ffmpeg 毫不相干（TTD-50）。
func TestNoHardcodedCommand(t *testing.T) {
	pattern := regexp.MustCompile(`exec\.(Command|CommandContext|LookPath)\([^)]*"(ffmpeg|ffprobe)"`)
	root := filepath.Join("..", "..")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		if strings.Contains(path, filepath.Join("platform", "ffbin")) {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if loc := pattern.FindIndex(b); loc != nil {
			t.Errorf("%s 直接调用了 ffmpeg/ffprobe：%s（改用 ffbin.FFmpeg()/FFprobe()）", path, b[loc[0]:loc[1]])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestOverride(t *testing.T) {
	SetOverride("ffmpeg", "/opt/custom/ffmpeg")
	defer SetOverride("ffmpeg", "")
	if FFmpeg() != "/opt/custom/ffmpeg" {
		t.Fatal("设置里指定的路径应优先")
	}
}
