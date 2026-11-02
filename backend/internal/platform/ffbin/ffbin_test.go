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

// 静态检查：internal/ 下的非测试代码不再写死 `"libx264"`（TT-T-27）。随包 ffmpeg 是 LGPL 构建、没有 x264，
// 写死的那一处会在用户排完队之后报「Unknown encoder」。编码参数一律经 pkg/h264.EncodeArgs。
func TestNoHardcodedX264(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), `"libx264"`) {
			t.Errorf("%s 写死了 libx264（改用 h264.EncodeArgs）", path)
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
