package domain

import (
	"image"
	_ "image/gif"  // 注册解码器：只为读头部拿尺寸
	_ "image/jpeg" // 同上
	_ "image/png"  // 同上
	"io"
)

// 图片尺寸探测（NEW-13 的前置：蒙版必须与原图同尺寸）。

// ImageSize 读出图片的宽高。
//
// ⚠️ 用 `DecodeConfig` **只读头部**，不解整张图：一张 4000×4000 的 PNG
// 解出来是 64MB 的像素缓冲，而我们只想知道两个数字。
// 上传高峰期并发几路就能把进程撑爆。
//
// ⚠️ 解不出来时返回 `(0,0,false)` 而**不是错误**：格式不认识
// （WebP 标准库就没有解码器）不该让上传失败 —— 那张图浏览器能显示、
// 上游也能用，只是我们记不下尺寸。记不下的后果是蒙版校验放行，
// 由上游去报错，这比拒绝一张好图轻得多。
func ImageSize(r io.Reader, mime string) (width, height int, ok bool) {
	switch mime {
	case "image/png", "image/jpeg", "image/gif":
	default:
		return 0, 0, false
	}
	cfg, _, err := image.DecodeConfig(r)
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return 0, 0, false
	}
	return cfg.Width, cfg.Height, true
}

// MaskMismatch 说明蒙版与原图对不上。
type MaskMismatch struct {
	SourceWidth, SourceHeight int
	MaskWidth, MaskHeight     int
}

func (m MaskMismatch) Error() string {
	return "蒙版尺寸与原图不一致：原图 " +
		itoa(m.SourceWidth) + "×" + itoa(m.SourceHeight) +
		"，蒙版 " + itoa(m.MaskWidth) + "×" + itoa(m.MaskHeight) +
		"。请重新在原图上涂抹"
}

// CheckMaskSize 校验蒙版与原图是否同尺寸。
//
// ⚠️ 这是**涂抹重绘唯一能对齐的依据**。蒙版画的是"改哪一块"，
// 尺寸对不上的话上游会把它拉伸后贴上去 —— 用户圈的是人脸，
// 改掉的是半张背景，而任务是"成功"的。
//
// ⚠️ 任一侧尺寸未知（格式不认识、没记下来）时**放行**：
// 拿不到数字就没法比较，拒绝等于把一批本来能用的格式挡在门外。
// 上游自己会校验，那一层报错至少是准的。
func CheckMaskSize(srcW, srcH, maskW, maskH int) error {
	if srcW <= 0 || srcH <= 0 || maskW <= 0 || maskH <= 0 {
		return nil
	}
	if srcW == maskW && srcH == maskH {
		return nil
	}
	return MaskMismatch{
		SourceWidth: srcW, SourceHeight: srcH,
		MaskWidth: maskW, MaskHeight: maskH,
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
