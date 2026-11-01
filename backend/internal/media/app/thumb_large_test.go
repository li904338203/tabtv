package app

import (
	"bytes"
	"context"
	"image"
	_ "image/jpeg"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/media/domain"
	"github.com/li904338203/tabtv/backend/internal/media/repo"
)

// IMGB-1：大图不整张解码。造一张 7000×4000（28MP）的 JPEG：本进程的 Thumbnail 不碰它，
// 交给 ffmpeg 缩成 320 宽；缩的过程中本进程的堆增长远小于整张解码要的 ~112MB。
func TestLargeThumb_大图交给ffmpeg且不进本进程内存(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("本机没有 ffmpeg")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "big.jpg")
	if out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "color=c=gray:s=7000x4000", "-frames:v", "1", src).CombinedOutput(); err != nil {
		t.Fatalf("造大图失败：%v %s", err, out)
	}
	data, _ := os.ReadFile(src)
	if _, _, ok, _ := domain.Thumbnail(bytes.NewReader(data), "image/jpeg", 320); ok {
		t.Fatal("超过 24MP 的图本进程不该缩")
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	s := &Service{}
	rc, m, err := s.largeThumb(context.Background(), io.NopCloser(bytes.NewReader(data)),
		repo.MediaObject{ContentType: "image/jpeg", Width: 7000, Height: 4000}, 320)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	thumb, _ := io.ReadAll(rc)
	runtime.ReadMemStats(&after)
	cfg, format, err := image.DecodeConfig(bytes.NewReader(thumb))
	if err != nil || cfg.Width != 320 || format != "jpeg" || m.SizeBytes != int64(len(thumb)) {
		t.Fatalf("缩略图：%+v %s %v", cfg, format, err)
	}
	if grown := int64(after.HeapAlloc) - int64(before.HeapAlloc); grown > 64<<20 {
		t.Fatalf("堆增长 %d MB，超过 64MB（不该在本进程解码大图）", grown>>20)
	}
}

// 缩不了时流式回原图（完整、不截断），读完关掉时删临时目录。
func TestLargeThumb_缩不了回完整原图(t *testing.T) {
	s := &Service{}
	orig := bytes.Repeat([]byte("x"), 40<<20)
	rc, _, err := s.largeThumb(context.Background(), io.NopCloser(bytes.NewReader(orig)),
		repo.MediaObject{ContentType: "image/png", SizeBytes: int64(len(orig))}, 320)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	if len(got) != len(orig) {
		t.Fatalf("应回完整原图（此前会被截断）：%d ≠ %d", len(got), len(orig))
	}
	r, ok := rc.(*removeOnClose)
	_ = rc.Close()
	if ok {
		if _, err := os.Stat(r.dir); !os.IsNotExist(err) {
			t.Fatal("关掉后要删临时目录")
		}
	}
}
