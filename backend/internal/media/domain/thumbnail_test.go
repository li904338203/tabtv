package domain

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// transparentPNGBytes 带透明通道的 PNG（左半透明、右半不透明）。
func transparentPNGBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := w / 2; x < w; x++ {
			img.Set(x, y, color.RGBA{200, 80, 40, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// 开放任意宽度等于给人一个 URL 参数把 CPU 打满。
func TestThumbnail_宽度白名单(t *testing.T) {
	src := pngBytes(t, 1000, 500)
	for _, w := range AllowedThumbWidths() {
		out, _, ok, err := Thumbnail(bytes.NewReader(src), "image/png", w)
		if err != nil || !ok {
			t.Fatalf("w=%d 应生成缩略图: ok=%v err=%v", w, ok, err)
		}
		img, _, err := image.Decode(bytes.NewReader(out))
		if err != nil {
			t.Fatal(err)
		}
		if got := img.Bounds().Dx(); got != w {
			t.Errorf("w=%d 实际宽度 %d", w, got)
		}
		// 比例要保持，否则列表里的图会被拉变形。
		if got := img.Bounds().Dy(); got != w/2 {
			t.Errorf("w=%d 高度应为 %d，实际 %d", w, w/2, got)
		}
		if len(out) >= len(src) {
			t.Errorf("w=%d 缩略图 %d 字节不该大于原图 %d 字节", w, len(out), len(src))
		}
	}

	for _, w := range []int{0, 1, 100, 321, 1024, -320} {
		if _, _, ok, _ := Thumbnail(bytes.NewReader(src), "image/png", w); ok {
			t.Errorf("w=%d 不在白名单，不该生成", w)
		}
	}
	if !IsAllowedThumbWidth(320) || IsAllowedThumbWidth(300) {
		t.Error("IsAllowedThumbWidth 判断不对")
	}
}

// 缩略图是优化，不能让本来能显示的东西显示不了。
func TestThumbnail_非位图原样返回(t *testing.T) {
	cases := []struct {
		name, mime string
		data       []byte
	}{
		{"视频", "video/mp4", []byte("\x00\x00\x00\x18ftypmp42")},
		{"音频", "audio/mpeg", []byte("ID3\x03")},
		{"GIF（缩完只剩一帧，语义变了）", "image/gif", []byte("GIF89a")},
		{"WebP（标准库无解码器）", "image/webp", []byte("RIFF....WEBP")},
		{"声称是 PNG 但解不出来", "image/png", []byte("not really a png")},
	}
	for _, c := range cases {
		out, _, ok, err := Thumbnail(bytes.NewReader(c.data), c.mime, 320)
		if err != nil {
			t.Errorf("%s 不该报错，实际 %v", c.name, err)
		}
		if ok || out != nil {
			t.Errorf("%s 应原样返回（ok=false）", c.name)
		}
	}
}

// 本来就比目标窄：缩了反而变模糊，还白白多一次编码。
func TestThumbnail_小于目标宽度时不缩(t *testing.T) {
	if _, _, ok, _ := Thumbnail(bytes.NewReader(pngBytes(t, 200, 100)), "image/png", 320); ok {
		t.Error("原图比目标窄时不该缩放")
	}
	// 恰好等于也不缩。
	if _, _, ok, _ := Thumbnail(bytes.NewReader(pngBytes(t, 320, 100)), "image/png", 320); ok {
		t.Error("原图恰好等于目标宽度时不该缩放")
	}
}

// 带透明的 PNG 保持 PNG：转成 JPEG 会把透明变成色块。（不透明的 PNG 出 JPEG，见下一条。）
func TestThumbnail_保持编码格式(t *testing.T) {
	out, _, ok, err := Thumbnail(bytes.NewReader(transparentPNGBytes(t, 800, 400)), "image/png", 160)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if _, err := png.Decode(bytes.NewReader(out)); err != nil {
		t.Errorf("PNG 的缩略图应仍是 PNG: %v", err)
	}

	out2, _, ok2, err2 := Thumbnail(bytes.NewReader(jpegBytes(t, 800, 400)), "image/jpeg", 160)
	if err2 != nil || !ok2 {
		t.Fatal(err2)
	}
	if _, err := jpeg.Decode(bytes.NewReader(out2)); err != nil {
		t.Errorf("JPEG 的缩略图应仍是 JPEG: %v", err)
	}
}

// 不透明的 PNG 取更小的编码（2026-11-02）：照片类画面出 JPEG（体积小一个数量级），
// 带透明的仍是 PNG —— 转成 JPEG 透明区会变成色块。
func TestThumbnail_不透明PNG出JPEG_透明的仍是PNG(t *testing.T) {
	// 模拟照片：伪随机噪声，PNG 压不动、JPEG 能大幅压缩。
	opaque := image.NewRGBA(image.Rect(0, 0, 1000, 600))
	seed := uint32(1)
	for y := 0; y < 600; y++ {
		for x := 0; x < 1000; x++ {
			seed = seed*1664525 + 1013904223
			opaque.Set(x, y, color.RGBA{uint8(seed >> 24), uint8(seed >> 16), uint8(seed >> 8), 255})
		}
	}
	var src bytes.Buffer
	if err := png.Encode(&src, opaque); err != nil {
		t.Fatal(err)
	}
	out, mime, ok, err := Thumbnail(bytes.NewReader(src.Bytes()), "image/png", 320)
	if err != nil || !ok || mime != "image/jpeg" {
		t.Fatalf("不透明 PNG 应出 JPEG：mime=%s ok=%v err=%v", mime, ok, err)
	}
	if _, err := jpeg.Decode(bytes.NewReader(out)); err != nil {
		t.Fatalf("应是合法 JPEG：%v", err)
	}

	_, mime, ok, _ = Thumbnail(bytes.NewReader(transparentPNGBytes(t, 1000, 500)), "image/png", 320)
	if !ok || mime != "image/png" {
		t.Errorf("透明 PNG 应保持 PNG，实际 %s", mime)
	}
}
