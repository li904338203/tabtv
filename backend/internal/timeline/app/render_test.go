package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// ⚠️ 水印目标宽度必须对齐偶数：yuv420p 的色度平面是 2×2 采样，
// 奇数宽的叠加层在部分 ffmpeg 版本上报 "width not divisible by 2"。
func TestPixelWidth_对齐偶数且有下限(t *testing.T) {
	// 1080p × 12% = 129.6 → 129 → 128
	if got := pixelWidth(1080, 12); got != 128 {
		t.Fatalf("1080×12%% 应为 128（对齐偶数），得到 %d", got)
	}
	if got := pixelWidth(1920, 12); got != 230 {
		t.Fatalf("1920×12%% 应为 230，得到 %d", got)
	}
	// ⚠️ 下限 16px：竖屏小画幅上按 1% 算出来是几个像素，
	// 缩到那个尺寸的 logo 等于没有水印。
	if got := pixelWidth(480, 1); got != 16 {
		t.Fatalf("过小应抬到 16，得到 %d", got)
	}
	// 画幅/占比缺失时按默认值算，而不是出一个 0 宽度的水印
	// （scale=0:-1 会让 ffmpeg 报错，整次渲染失败）。
	if got := pixelWidth(0, 0); got != 230 {
		t.Fatalf("缺省应按 1920×12%% 算，得到 %d", got)
	}
}

type flagsStub struct {
	off bool
	err error
}

func (f flagsStub) WatermarkOff(context.Context, int64) (bool, error) {
	return f.off, f.err
}

// ⚠️ 查不到时方向不能反：**按"不关"处理（照常加水印）**。
// 反过来的话一次数据库抖动会让那批成片悄悄少了 AI 标识，而事后无从辨认。
func TestWatermarkOff_查失败按加水印处理(t *testing.T) {
	s := &Service{}
	if s.watermarkOff(context.Background(), 1) {
		t.Fatal("没装端口时应按加水印处理")
	}
	s.SetUserFlags(flagsStub{err: errors.New("db down")})
	if s.watermarkOff(context.Background(), 1) {
		t.Fatal("查失败应按加水印处理")
	}
	s.SetUserFlags(flagsStub{off: true})
	if !s.watermarkOff(context.Background(), 1) {
		t.Fatal("用户关了就不加")
	}
	// userID 为 0（取不到当前用户）同样按加水印处理。
	if s.watermarkOff(context.Background(), 0) {
		t.Fatal("取不到用户时应按加水印处理")
	}
}

// ⚠️ 只传 `aspectRatio` 的调用方会拿到什么尺寸 —— 这条以前是错的：
// 一律 1920×1080，于是"选了竖屏、渲出来是横的"，而两边都不报错
// （前端恰好每次都带尺寸，把这个洞掩盖了）。
func TestDefaultSize_跟着画幅走(t *testing.T) {
	cases := map[string][2]int{
		"16:9": {1920, 1080},
		"9:16": {1080, 1920},
		"1:1":  {1080, 1080},
		"4:3":  {1440, 1080},
		"21:9": {2560, 1080},
		"3:4":  {1080, 1440},
		// 未知画幅回落横屏 —— 这里不该报错，`validateFormat` 会拦。
		"": {1920, 1080},
		// custom 没有默认尺寸（Create 在走到这里之前就要求调用方给宽高）。
		"custom": {1920, 1080},
	}
	for aspect, want := range cases {
		w, h := defaultSize(aspect)
		if w != want[0] || h != want[1] {
			t.Fatalf("%q 应为 %d×%d，得到 %d×%d", aspect, want[0], want[1], w, h)
		}
	}
}

// ⚠️ 画幅与尺寸必须自洽：界面按画幅画预览框、ffmpeg 按尺寸出片 ——
// 不一致的话用户看到的预览与拿到的成片是两个形状。
func TestValidateFormat_画幅与尺寸要自洽(t *testing.T) {
	if err := validateFormat("16:9", 30, 1920, 1080); err != nil {
		t.Fatalf("标准 16:9 不该被拦：%v", err)
	}
	// 为编码器对齐的微调是正当的（2% 容差内）。
	if err := validateFormat("16:9", 30, 1920, 1088); err != nil {
		t.Fatalf("1920×1088 应放行（编码器对齐）：%v", err)
	}
	// 声明竖屏却给横屏尺寸 —— 必须拦。
	err := validateFormat("9:16", 30, 1920, 1080)
	if err == nil {
		t.Fatal("9:16 配 1920×1080 应被拦")
	}
	// 错误里要带上两边的数字，否则用户不知道该改哪个。
	if msg := err.Error(); !strings.Contains(msg, "1920×1080") ||
		!strings.Contains(msg, "1080×1920") {
		t.Fatalf("错误应带上实际与期望尺寸：%s", msg)
	}
	// 奇数尺寸仍然要拦（yuv420p 的硬要求）。
	if validateFormat("16:9", 30, 1921, 1080) == nil {
		t.Fatal("奇数宽应被拦")
	}
}

// 「跟随素材」的自定义尺寸：只校验偶数与范围，不做比例自洽；错误带字段路径（SCT-03）。
func TestValidateFormat_自定义尺寸(t *testing.T) {
	if err := validateFormat("custom", 30, 1080, 1350); err != nil {
		t.Fatalf("custom 1080×1350 应放行：%v", err)
	}
	if err := validateFormat("3:4", 30, 1080, 1440); err != nil {
		t.Fatalf("3:4 默认尺寸应放行：%v", err)
	}
	for _, c := range []struct {
		w, h int
		path string
	}{{1081, 1350, "width"}, {1080, 1351, "height"}, {62, 1350, "width"}, {1080, 7682, "height"}} {
		err := validateFormat("custom", 30, c.w, c.h)
		var be *httpx.BizError
		if !errors.As(err, &be) || be.Code != errcode.InvalidArgument || be.Details["path"] != c.path {
			t.Errorf("custom %d×%d 应报 %s，实际 %v", c.w, c.h, c.path, err)
		}
	}
}
