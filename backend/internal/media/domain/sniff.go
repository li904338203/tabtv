// Package domain 负责媒体的类型识别与校验。
//
// ⚠️ GO-28：**必须校验实际文件头，声明的 MIME 不可信**。
// 客户端可以把 .exe 声明成 image/png；只信 Content-Type 等于没校验。
package domain

import (
	"bytes"
	"fmt"
	"strings"
)

type Kind string

const (
	KindImage Kind = "image"
	KindAudio Kind = "audio"
	KindVideo Kind = "video"
	// KindSplat 是 3D 高斯泼溅文件（PLY / SPZ，3D 导演台 DRD-28）。
	//
	// ⚠️ **不进通用签名表**：只在上传入口显式要求泼溅（ExpectKind = KindSplat）时才认。
	// 进了通用表的话，`/media/references` 不带 mediaType 时会收下 .ply 或任意 gzip 文件，
	// 并把它们作为参考图列进「我的素材」。识别见 SniffSplat 与 splat.go 的 VerifySplat。
	KindSplat Kind = "splat"
)

// 泼溅文件的 MIME。没有注册过的官方类型，按惯例用 x- 前缀；只在本系统内部使用。
const (
	MIMEPly = "application/x-ply"
	MIMESpz = "application/x-spz"
)

// signature 是一条文件头特征。
type signature struct {
	mime   string
	kind   Kind
	offset int
	magic  []byte
	// extra 用于需要二次确认的容器格式（如 RIFF 的 WEBP/WAVE）。
	extraOffset int
	extra       []byte
}

// signatures 覆盖 07 §5.2 列出的格式。
// 顺序有意义：更具体的特征排在前面。
var signatures = []signature{
	{mime: "image/png", kind: KindImage, magic: []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}},
	{mime: "image/jpeg", kind: KindImage, magic: []byte{0xFF, 0xD8, 0xFF}},
	{mime: "image/gif", kind: KindImage, magic: []byte("GIF87a")},
	{mime: "image/gif", kind: KindImage, magic: []byte("GIF89a")},
	// RIFF 容器：前 4 字节相同，第 8 字节起区分 WEBP / WAVE。
	{mime: "image/webp", kind: KindImage, magic: []byte("RIFF"), extraOffset: 8, extra: []byte("WEBP")},
	{mime: "audio/wav", kind: KindAudio, magic: []byte("RIFF"), extraOffset: 8, extra: []byte("WAVE")},
	// ISO BMFF：第 4 字节起是 ftyp。
	{mime: "video/mp4", kind: KindVideo, offset: 4, magic: []byte("ftyp")},
	{mime: "video/webm", kind: KindVideo, magic: []byte{0x1A, 0x45, 0xDF, 0xA3}},
	{mime: "audio/ogg", kind: KindAudio, magic: []byte("OggS")},
	{mime: "audio/mpeg", kind: KindAudio, magic: []byte{'I', 'D', '3'}},
	// 无 ID3 头的 MP3：Layer III 的帧同步字。MPEG-1（FB / 带 CRC 的 FA）、MPEG-2（F3 / F2）与
	// MPEG-2.5（E3 / E2，8 / 11.025 / 12 kHz 的低采样率）都是同一家族。
	// ⚠️ 漏认低采样率的话，上游明明给了一段能播的 MP3，归档却报「无法识别的文件类型」，任务一路重试到死线
	//    （SCT-42 实测：配乐假上游回的 8 kHz MP3 —— 电话音质的 TTS 与音效也常是这个采样率）。
	{mime: "audio/mpeg", kind: KindAudio, magic: []byte{0xFF, 0xFB}},
	{mime: "audio/mpeg", kind: KindAudio, magic: []byte{0xFF, 0xFA}},
	{mime: "audio/mpeg", kind: KindAudio, magic: []byte{0xFF, 0xF3}},
	{mime: "audio/mpeg", kind: KindAudio, magic: []byte{0xFF, 0xF2}},
	{mime: "audio/mpeg", kind: KindAudio, magic: []byte{0xFF, 0xE3}},
	{mime: "audio/mpeg", kind: KindAudio, magic: []byte{0xFF, 0xE2}},
}

// SniffLen 是识别所需的最小前缀长度。
const SniffLen = 16

var ErrUnknownType = fmt.Errorf("无法识别的文件类型")

// Detected 是识别结果。
type Detected struct {
	MIME string
	Kind Kind
}

// Sniff 按文件头识别真实类型。
func Sniff(head []byte) (Detected, error) {
	for _, s := range signatures {
		end := s.offset + len(s.magic)
		if len(head) < end {
			continue
		}
		if !bytes.Equal(head[s.offset:end], s.magic) {
			continue
		}
		if len(s.extra) > 0 {
			xe := s.extraOffset + len(s.extra)
			if len(head) < xe || !bytes.Equal(head[s.extraOffset:xe], s.extra) {
				continue
			}
		}
		return Detected{MIME: s.mime, Kind: s.kind}, nil
	}
	return Detected{}, ErrUnknownType
}

// SniffSplat 是泼溅识别的第一段：只看文件头，**只能初判**（DRD-28）。
//
//   - PLY 以 `ply\n` 或 `ply\r\n` 开头；
//   - SPZ 有两种容器（nianticlabs/spz 的 load-spz.h / load-spz.cc，2026-10-26 核对）：
//     v1~v3 整个文件是 gzip（以 1F 8B 开头，解压后才是 NGSP 头）；
//     v4 起头部不压缩，开头就是魔数 0x5053474e（小端，文件里的字节是 `NGSP`），属性流用 zstd 压缩。
//
// 16 字节读不到 PLY 的 `element vertex`，也解不开 gzip：第二段在落到临时文件之后由 VerifySplat 做。
func SniffSplat(head []byte) (Detected, error) {
	switch {
	case bytes.HasPrefix(head, []byte("ply\n")), bytes.HasPrefix(head, []byte("ply\r\n")):
		return Detected{MIME: MIMEPly, Kind: KindSplat}, nil
	case bytes.HasPrefix(head, []byte{0x1F, 0x8B}), bytes.HasPrefix(head, spzMagic):
		return Detected{MIME: MIMESpz, Kind: KindSplat}, nil
	}
	return Detected{}, ErrUnknownType
}

// VerifySplatDeclared 按文件头识别泼溅，并与声明的 MIME 交叉校验。
//
// 浏览器不认识 .ply / .spz，声明多半是空串或 application/octet-stream，所以这里只拦**明显矛盾**的：
// 声明成图片 / 音频 / 视频而文件头是泼溅 —— 那是伪装上传的典型特征。
func VerifySplatDeclared(head []byte, declared string) (Detected, error) {
	d, err := SniffSplat(head)
	if err != nil {
		return Detected{}, err
	}
	switch m := normalizeMIME(declared); {
	case strings.HasPrefix(m, "image/"), strings.HasPrefix(m, "audio/"), strings.HasPrefix(m, "video/"):
		return d, fmt.Errorf("文件实际类型为 %s，与声明的 %s 不符", d.MIME, declared)
	}
	return d, nil
}

// VerifyDeclared 校验声明的 MIME 与实际文件头是否一致。
//
// ⚠️ 不一致时返回**实际类型**而不是直接拒绝的决定权交给调用方：
// 有些场景（浏览器把 jpg 声明成 image/jpg）属于良性偏差，
// 但 declared=image/png 而实际是 video/mp4 一定要拒。
func VerifyDeclared(head []byte, declared string) (Detected, error) {
	d, err := Sniff(head)
	if err != nil {
		return Detected{}, err
	}
	if declared == "" {
		return d, nil
	}
	if normalizeMIME(declared) != d.MIME {
		return d, fmt.Errorf("文件实际类型为 %s，与声明的 %s 不符", d.MIME, declared)
	}
	return d, nil
}

func normalizeMIME(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.IndexByte(s, ';'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	switch s {
	case "image/jpg", "image/pjpeg":
		return "image/jpeg"
	case "audio/mp3", "audio/mpeg3", "audio/x-mpeg-3":
		return "audio/mpeg"
	case "audio/wave", "audio/x-wav":
		return "audio/wav"
	case "video/x-m4v", "video/quicktime":
		return "video/mp4"
	}
	return s
}

// Limits 是一类媒体的上传限制。
type Limits struct {
	MaxBytes int64
	Allowed  map[string]bool
}

// DefaultLimits 按 07 §5.2：普通参考 50 MiB、脚本资产图 20 MiB。
// 素材封面/试听比参考图小得多，单独给更紧的上限。
func DefaultLimits(kind Kind) Limits {
	switch kind {
	case KindImage:
		return Limits{MaxBytes: 10 << 20, Allowed: map[string]bool{
			"image/png": true, "image/jpeg": true, "image/webp": true, "image/gif": true,
		}}
	case KindAudio:
		return Limits{MaxBytes: 20 << 20, Allowed: map[string]bool{
			"audio/mpeg": true, "audio/wav": true, "audio/ogg": true,
		}}
	case KindVideo:
		return Limits{MaxBytes: 100 << 20, Allowed: map[string]bool{
			"video/mp4": true, "video/webm": true,
		}}
	case KindSplat:
		// 3D 导演台的泼溅背景（DR-PARAM-11，设计建议值）；上传入口还可以用 MaxBytes 再收紧。
		return Limits{MaxBytes: 100 << 20, Allowed: map[string]bool{MIMEPly: true, MIMESpz: true}}
	}
	return Limits{}
}

// Tighten 把上限收紧到 maxBytes：只能收紧，不能放宽（maxBytes <= 0 或大于默认上限时不变）。
//
// ⚠️ 只能收紧：调用方（导演台预演 50 MiB）给的是「这个入口更严」的要求；
// 允许放宽的话，任何一个入口写错一个数就能绕过整类媒体的上限。
func (l Limits) Tighten(maxBytes int64) Limits {
	if maxBytes > 0 && (l.MaxBytes <= 0 || maxBytes < l.MaxBytes) {
		l.MaxBytes = maxBytes
	}
	return l
}

func (l Limits) Check(d Detected, size int64) error {
	if l.MaxBytes > 0 && size > l.MaxBytes {
		return fmt.Errorf("文件大小 %.1f MB 超过上限 %.1f MB",
			float64(size)/(1<<20), float64(l.MaxBytes)/(1<<20))
	}
	if len(l.Allowed) > 0 && !l.Allowed[d.MIME] {
		return fmt.Errorf("不支持的文件类型: %s", d.MIME)
	}
	return nil
}
