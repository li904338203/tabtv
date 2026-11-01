package domain

import (
	"bytes"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
)

// 缩略图。
//
// ⚠️ 这件事要解决的是**内存**不是流量：作品墙原图单张 1.7MB，滚完几百件就是几百 MB，
// 标签页会被浏览器直接杀掉。长边缩到 320px 后只剩几十 KB。
//
// ⚠️ 宽度**只接受三档**。开放任意值等于给人一个 URL 参数把 CPU 打满：
// 一个循环请求 w=1..2000 的脚本就能让服务端一直在解码-缩放-编码。
var allowedWidths = map[int]bool{160: true, 320: true, 640: true}

func IsAllowedThumbWidth(w int) bool { return allowedWidths[w] }

// AllowedThumbWidths 供错误信息与文档使用，顺序固定。
func AllowedThumbWidths() []int { return []int{160, 320, 640} }

// ThumbMaxDecodePixels 本进程整张解码的像素上限（24MP，RGBA 约 96MB）。超过的交给 ffmpeg 流式缩放。
const ThumbMaxDecodePixels = 24_000_000

// Thumbnail 生成缩略图。
//
// ⚠️ **非位图原样返回**（第二个返回值为 false）：视频、音频、以及解不出来的格式
// 一律回原文件。缩略图是优化，**不能让本来能显示的东西显示不了** ——
// 解码失败就报错的话，一张浏览器能渲染、Go 解不了的图片会在列表里变成破图。
//
// 目前能缩的是 PNG / JPEG。GIF（动图缩完只剩一帧，语义变了）、
// WebP（标准库无解码器）暂不处理，走原样返回这条路。
//
// 第二个返回值是缩略图的类型：**不透明的 PNG 取 PNG / JPEG 里更小的那个**（2026-11-02）—— 生成图几乎都是
// 不透明的照片类 PNG，320 宽按 PNG 无损编码有三百多 KB，JPEG 只有几十 KB。带透明通道的仍是 PNG。
func Thumbnail(r io.Reader, mime string, width int) ([]byte, string, bool, error) {
	if !allowedWidths[width] {
		return nil, "", false, nil
	}
	switch mime {
	case "image/png", "image/jpeg":
	default:
		return nil, "", false, nil
	}

	// ⚠️ 先只读头部看尺寸（IMGB-1）：库里记的宽高可能是空的（老数据、探测失败），
	// 不看就整张解码的话，一张 85MP 的图要 ~340MB。超门槛的不缩（调用方走 ffmpeg 或回原图）。
	var head bytes.Buffer
	cfg, _, err := image.DecodeConfig(io.TeeReader(r, &head))
	if err != nil {
		return nil, "", false, nil
	}
	if int64(cfg.Width)*int64(cfg.Height) > ThumbMaxDecodePixels {
		return nil, "", false, nil
	}
	src, _, err := image.Decode(io.MultiReader(&head, r))
	if err != nil {
		// 解不出来不是错误，是"这张不缩"。
		return nil, "", false, nil
	}
	b := src.Bounds()
	if b.Dx() <= width {
		// 本来就比目标窄：缩了反而变模糊，而且白白多一次编码。
		return nil, "", false, nil
	}
	height := b.Dy() * width / b.Dx()
	if height < 1 {
		height = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	// 最近邻足够用：缩略图只要"能认出是什么"，
	// 引入第三方重采样库换来的画质提升在 160~640px 上肉眼几乎看不出。
	for y := 0; y < height; y++ {
		sy := b.Min.Y + y*b.Dy()/height
		for x := 0; x < width; x++ {
			sx := b.Min.X + x*b.Dx()/width
			dst.Set(x, y, src.At(sx, sy))
		}
	}

	if mime == "image/png" {
		var pngBuf bytes.Buffer
		if err := png.Encode(&pngBuf, dst); err != nil {
			return nil, "", false, err
		}
		if !dst.Opaque() {
			// 带透明的 PNG 保持 PNG：转成 JPEG 会把透明变成色块。
			return pngBuf.Bytes(), "image/png", true, nil
		}
		// 不透明：两种都编一遍取小的。照片类画面 JPEG 小一个数量级，大片纯色 / 渐变的图 PNG 反而更小 ——
		// 按经验写死一种的话，总有一类图变大。
		jpg, err := encodeJPEG(dst)
		if err != nil {
			return nil, "", false, err
		}
		if len(jpg) < pngBuf.Len() {
			return jpg, "image/jpeg", true, nil
		}
		return pngBuf.Bytes(), "image/png", true, nil
	}
	jpg, err := encodeJPEG(dst)
	if err != nil {
		return nil, "", false, err
	}
	return jpg, "image/jpeg", true, nil
}

// encodeJPEG 质量 82：再高体积涨得快，再低在 640px 上能看出块效应。透明处垫白底。
func encodeJPEG(dst *image.RGBA) ([]byte, error) {
	bg := image.NewRGBA(dst.Bounds())
	draw.Draw(bg, bg.Bounds(), image.White, image.Point{}, draw.Src)
	draw.Draw(bg, bg.Bounds(), dst, image.Point{}, draw.Over)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, bg, &jpeg.Options{Quality: 82}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
