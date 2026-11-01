package domain

import (
	"bytes"
	"strings"
	"testing"
)

func TestImageSize_读出宽高(t *testing.T) {
	w, h, ok := ImageSize(bytes.NewReader(pngBytes(t, 640, 360)), "image/png")
	if !ok || w != 640 || h != 360 {
		t.Fatalf("PNG 尺寸不对: %d×%d ok=%v", w, h, ok)
	}
	w, h, ok = ImageSize(bytes.NewReader(jpegBytes(t, 200, 100)), "image/jpeg")
	if !ok || w != 200 || h != 100 {
		t.Fatalf("JPEG 尺寸不对: %d×%d ok=%v", w, h, ok)
	}
}

// ⚠️ 格式不认识（WebP 标准库就没有解码器）不该让上传失败 ——
// 那张图浏览器能显示、上游也能用，只是我们记不下尺寸。
func TestImageSize_不认识的格式安静跳过(t *testing.T) {
	if _, _, ok := ImageSize(bytes.NewReader([]byte("RIFF....WEBP")), "image/webp"); ok {
		t.Fatal("WebP 应返回 ok=false 而不是报错")
	}
	if _, _, ok := ImageSize(bytes.NewReader([]byte("not an image")), "image/png"); ok {
		t.Fatal("解不出来应返回 ok=false")
	}
	if _, _, ok := ImageSize(bytes.NewReader(nil), "video/mp4"); ok {
		t.Fatal("非图片应返回 ok=false")
	}
}

// ⚠️ 用 DecodeConfig 只读头部：一张 4000×4000 的 PNG 解出来是
// 64MB 的像素缓冲，而我们只想知道两个数字。
func TestImageSize_只读头部不解整张图(t *testing.T) {
	full := pngBytes(t, 800, 600)
	// 只给前 512 字节 —— 够 DecodeConfig 读头，不够解整张图。
	head := full
	if len(full) > 512 {
		head = full[:512]
	}
	w, h, ok := ImageSize(bytes.NewReader(head), "image/png")
	if !ok || w != 800 || h != 600 {
		t.Fatalf("只有头部也该读出尺寸: %d×%d ok=%v", w, h, ok)
	}
}

// ---------------------------------------------------------------- 蒙版校验

// ⚠️ 尺寸对不上的话上游会把蒙版拉伸后贴上去 —— 用户圈的是人脸，
// 改掉的是半张背景，而任务是"成功"的。
func TestCheckMaskSize_尺寸不一致要拒(t *testing.T) {
	err := CheckMaskSize(1024, 768, 512, 384)
	if err == nil {
		t.Fatal("尺寸不一致应被拒绝")
	}
	// 错误里要带上两组数字，用户才知道该怎么改。
	msg := err.Error()
	for _, want := range []string{"1024", "768", "512", "384"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误应说清两边的尺寸，实际 %s", msg)
		}
	}
}

func TestCheckMaskSize_一致时放行(t *testing.T) {
	if err := CheckMaskSize(1024, 768, 1024, 768); err != nil {
		t.Fatalf("同尺寸应放行: %v", err)
	}
}

// ⚠️ 拿不到数字就没法比较，拒绝等于把一批本来能用的格式挡在门外。
// 上游自己会校验，那一层报错至少是准的。
func TestCheckMaskSize_尺寸未知时放行(t *testing.T) {
	cases := [][4]int{
		{0, 0, 512, 512},     // 原图尺寸没记下来
		{1024, 768, 0, 0},    // 蒙版尺寸没记下来
		{0, 0, 0, 0},         // 两边都不知道
		{1024, 0, 1024, 768}, // 只记了一半
	}
	for _, c := range cases {
		if err := CheckMaskSize(c[0], c[1], c[2], c[3]); err != nil {
			t.Fatalf("%v 应放行，实际 %v", c, err)
		}
	}
}
