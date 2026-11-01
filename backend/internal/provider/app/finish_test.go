package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	"github.com/li904338203/tabtv/backend/internal/provider/protocol"
)

func pngOf(t *testing.T, w, h int, fill func(x, y int) color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, fill(x, y))
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func expressionJob(t *testing.T) genapp.Job {
	t.Helper()
	params, _ := json.Marshal(map[string]any{
		"tool":     "expression_edit",
		"toolCrop": map[string]any{"x": 100, "y": 10, "w": 80, "h": 80},
		"toolOptions": map[string]any{"sourceMediaId": "7", "roles": []map[string]any{
			{"box": map[string]any{"x": 120, "y": 20, "w": 40, "h": 40}, "arousal": 2, "warmth": 2}}},
	})
	return genapp.Job{Params: params, Inputs: json.RawMessage(`[{"mediaId":7,"mediaKind":"image"}]`)}
}

// 情绪调节发给上游的是框选人物所在的那一块（R-5）。
func TestCropToolInput_只发裁切区域(t *testing.T) {
	src := pngOf(t, 200, 100, func(x, y int) color.RGBA { return color.RGBA{uint8(x), uint8(y), 7, 255} })
	p := &HTTPProvider{media: stubMedia{data: src, mime: "image/png"}}
	pjob := protocol.Job{Media: []protocol.Media{{Kind: "image", Value: "https://x/a.png"}}}
	if err := p.cropToolInput(context.Background(), expressionJob(t), &pjob); err != nil {
		t.Fatal(err)
	}
	v := pjob.Media[0].Value
	if !strings.HasPrefix(v, "data:image/png;base64,") {
		t.Fatalf("应换成内联的局部图：%.40s", v)
	}
	raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(v, "data:image/png;base64,"))
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil || img.Bounds().Dx() != 80 || img.Bounds().Dy() != 80 {
		t.Fatalf("局部图尺寸 = 裁切区域：%v %v", img.Bounds(), err)
	}
	if c := color.RGBAModel.Convert(img.At(0, 0)).(color.RGBA); c.R != 100 || c.G != 10 {
		t.Fatalf("局部图左上角 = 原图 (100,10)：%v", c)
	}
	// 不是情绪调节时不动
	other := protocol.Job{Media: []protocol.Media{{Kind: "image", Value: "https://x/a.png"}}}
	if err := p.cropToolInput(context.Background(), genapp.Job{Params: json.RawMessage(`{"tool":"relight"}`)}, &other); err != nil ||
		other.Media[0].Value != "https://x/a.png" {
		t.Fatalf("打光不裁：%v %v", other.Media[0].Value, err)
	}
}

// IMG-AC-16 的 Worker 侧：局部编辑结果放回原位后只贴回选中人物的框，框外与源图逐像素相同，输出是原图分辨率。
func TestFinishTool_情绪贴回框外不变(t *testing.T) {
	src := pngOf(t, 200, 100, func(x, y int) color.RGBA { return color.RGBA{uint8(x), uint8(y), 7, 255} })
	// 上游回的局部结果尺寸与裁切区域不同（模型按自己的尺寸出图）：要先缩回 80×80。
	edited := pngOf(t, 160, 160, func(int, int) color.RGBA { return color.RGBA{255, 0, 0, 255} })
	p := &HTTPProvider{media: stubMedia{data: src, mime: "image/png"}}
	outs, err := p.finishTool(context.Background(), expressionJob(t), []genapp.ProviderOutput{
		{Type: "image", MIME: "image/png", Body: bytes.NewReader(edited), Size: int64(len(edited))}})
	if err != nil || len(outs) != 1 {
		t.Fatalf("贴回：%v %d", err, len(outs))
	}
	got, err := png.Decode(outs[0].Body)
	if err != nil {
		t.Fatal(err)
	}
	if b := got.Bounds(); b.Dx() != 200 || b.Dy() != 100 {
		t.Fatalf("输出是原图分辨率：%v", b)
	}
	if c := color.RGBAModel.Convert(got.At(140, 40)).(color.RGBA); c.R != 255 || c.G != 0 {
		t.Fatalf("框中心应是编辑结果：%v", c)
	}
	// 框外（含裁切区域里、框外的部分）都与源图相同
	for _, pt := range []image.Point{{0, 0}, {50, 50}, {199, 99}, {105, 85}, {175, 15}} {
		want := color.RGBA{uint8(pt.X), uint8(pt.Y), 7, 255}
		if c := color.RGBAModel.Convert(got.At(pt.X, pt.Y)).(color.RGBA); c != want {
			t.Fatalf("框外 %v 应与源图相同：%v ≠ %v", pt, c, want)
		}
	}
}

func TestFinishTool_非情绪原样返回_缺裁切判失败(t *testing.T) {
	p := &HTTPProvider{}
	in := []genapp.ProviderOutput{{Type: "image"}}
	outs, err := p.finishTool(context.Background(), genapp.Job{Params: json.RawMessage(`{"tool":"multi_angle"}`)}, in)
	if err != nil || len(outs) != 1 {
		t.Fatalf("多角度不做后处理：%v", err)
	}
	if _, err := p.finishTool(context.Background(), genapp.Job{
		Params: json.RawMessage(`{"tool":"expression_edit","toolOptions":{"roles":[{"box":{"x":0,"y":0,"w":30,"h":30}}]}}`),
		Inputs: json.RawMessage(`[{"mediaId":7,"mediaKind":"image"}]`)}, in); err == nil {
		t.Fatal("没有裁切区域要判失败，不能把没贴回的图当成功")
	}
}
