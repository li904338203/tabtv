package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/li904338203/tabtv/backend/internal/media/repo"
)

// 大图缩略图（docs/agent/imagetools IMGB-1）。
//
// 本进程不整张解码超过 24MP 的图：交给 ffmpeg（另一个进程、按需解码），并限并发 ——
// 一次 85MP 的解码 ffmpeg 也要一百多 MB，几个请求同时来照样会把机器挤满。
// 没有 ffmpeg 或缩失败时**流式回原图**（不读进内存）：缩略图是优化，不能让本来能显示的图显示不了，
// 也不能为了它把进程撑爆。

// largeThumbSem 大图缩略的并发上限。
var largeThumbSem = make(chan struct{}, 2)

// largeThumbTimeout 单次缩放的上限：畸形文件会让 ffmpeg 一直读。
const largeThumbTimeout = 60 * time.Second

func (s *Service) largeThumb(ctx context.Context, body io.ReadCloser, m repo.MediaObject, width int) (
	io.ReadCloser, repo.MediaObject, error) {
	defer body.Close()
	dir, err := os.MkdirTemp("", "seeaitv-thumb-*")
	if err != nil {
		return nil, repo.MediaObject{}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	src := filepath.Join(dir, "src")
	f, err := os.Create(src)
	if err != nil {
		cleanup()
		return nil, repo.MediaObject{}, err
	}
	if _, err := io.Copy(f, body); err != nil {
		_ = f.Close()
		cleanup()
		return nil, repo.MediaObject{}, err
	}
	_ = f.Close()

	if thumb, err := ffmpegThumb(ctx, src, dir, m.ContentType, width); err == nil {
		cleanup()
		m.SizeBytes = int64(len(thumb))
		return io.NopCloser(bytes.NewReader(thumb)), m, nil
	} else if s.log != nil {
		s.log.Warn("大图缩略失败，回原图", "mediaId", m.ID, "error", err)
	}
	// 回原图：从临时文件流式读，读完关掉时删目录。
	rf, err := os.Open(src)
	if err != nil {
		cleanup()
		return nil, repo.MediaObject{}, err
	}
	return &removeOnClose{File: rf, dir: dir}, m, nil
}

// ffmpegThumb 用 ffmpeg 把图缩到 width 宽（等比、高取偶数），格式跟着原图（PNG 仍是 PNG、JPEG 仍是 JPEG）。
func ffmpegThumb(ctx context.Context, src, dir, mime string, width int) ([]byte, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, fmt.Errorf("本机没有 ffmpeg")
	}
	select {
	case largeThumbSem <- struct{}{}:
		defer func() { <-largeThumbSem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, largeThumbTimeout)
	defer cancel()
	ext, codec := ".jpg", "mjpeg"
	if mime == "image/png" {
		ext, codec = ".png", "png"
	}
	out := filepath.Join(dir, "thumb"+ext)
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-i", src,
		"-vf", fmt.Sprintf("scale=%d:-2", width), "-frames:v", "1", "-c:v", codec}
	if codec == "mjpeg" {
		args = append(args, "-q:v", "4")
	}
	args = append(args, out)
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg 缩略失败：%w %s", err, tail(stderr.String(), 300))
	}
	data, err := os.ReadFile(out)
	if err != nil || len(data) == 0 {
		return nil, fmt.Errorf("ffmpeg 没有输出")
	}
	return data, nil
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// removeOnClose 关掉时连同临时目录一起删。
type removeOnClose struct {
	*os.File
	dir string
}

func (r *removeOnClose) Close() error {
	err := r.File.Close()
	_ = os.RemoveAll(r.dir)
	return err
}
