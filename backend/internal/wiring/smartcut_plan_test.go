package wiring

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// 拼图（SC-PARAM-24）：3 格宽、每行一段素材；帧等比缩放居中，竖屏不被拉扁。
func TestComposeMosaic_三格一行等比居中(t *testing.T) {
	portrait := image.NewRGBA(image.Rect(0, 0, 90, 160))
	for y := 0; y < 160; y++ {
		for x := 0; x < 90; x++ {
			portrait.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	landscape := image.NewRGBA(image.Rect(0, 0, 160, 90))
	out, err := ComposeMosaic([][]image.Image{{portrait, portrait, portrait}, {landscape}})
	if err != nil {
		t.Fatal(err)
	}
	img, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 480 || b.Dy() != 320 {
		t.Fatalf("两行拼图是 480×320：%v", b)
	}
	// 竖屏帧居中：格子正中是红的，左右边是留边的深灰。
	if r, _, _, _ := img.At(80, 80).RGBA(); r>>8 < 200 {
		t.Fatalf("格子正中应是画面：%v", img.At(80, 80))
	}
	if r, _, _, _ := img.At(5, 80).RGBA(); r>>8 > 80 {
		t.Fatalf("竖屏帧两边应留边，不拉伸：%v", img.At(5, 80))
	}
	if len(out) > 1<<20 {
		t.Fatalf("单张拼图应远小于 1 MiB：%d", len(out))
	}
}
