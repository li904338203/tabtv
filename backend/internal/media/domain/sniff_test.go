package domain

import (
	"strings"
	"testing"
)

func head(b ...byte) []byte {
	out := make([]byte, 0, SniffLen)
	out = append(out, b...)
	for len(out) < SniffLen {
		out = append(out, 0)
	}
	return out
}

func TestSniff_各格式(t *testing.T) {
	cases := []struct {
		name string
		head []byte
		mime string
		kind Kind
	}{
		{"PNG", head(0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A), "image/png", KindImage},
		{"JPEG", head(0xFF, 0xD8, 0xFF, 0xE0), "image/jpeg", KindImage},
		{"GIF89a", head('G', 'I', 'F', '8', '9', 'a'), "image/gif", KindImage},
		{"WEBP", head('R', 'I', 'F', 'F', 0, 0, 0, 0, 'W', 'E', 'B', 'P'), "image/webp", KindImage},
		{"WAV", head('R', 'I', 'F', 'F', 0, 0, 0, 0, 'W', 'A', 'V', 'E'), "audio/wav", KindAudio},
		{"MP4", head(0, 0, 0, 0x20, 'f', 't', 'y', 'p'), "video/mp4", KindVideo},
		{"WEBM", head(0x1A, 0x45, 0xDF, 0xA3), "video/webm", KindVideo},
		{"OGG", head('O', 'g', 'g', 'S'), "audio/ogg", KindAudio},
		{"MP3-ID3", head('I', 'D', '3', 0x03), "audio/mpeg", KindAudio},
		{"MP3-帧同步", head(0xFF, 0xFB, 0x90), "audio/mpeg", KindAudio},
		{"MP3-MPEG1 带 CRC", head(0xFF, 0xFA, 0x90), "audio/mpeg", KindAudio},
		{"MP3-MPEG2.5（8 kHz）", head(0xFF, 0xE3, 0x18), "audio/mpeg", KindAudio},
	}
	for _, c := range cases {
		got, err := Sniff(c.head)
		if err != nil {
			t.Errorf("%s: 识别失败 %v", c.name, err)
			continue
		}
		if got.MIME != c.mime || got.Kind != c.kind {
			t.Errorf("%s: 得到 %s/%s，期望 %s/%s", c.name, got.MIME, got.Kind, c.mime, c.kind)
		}
	}
}

// RIFF 容器前 4 字节相同，必须靠第 8 字节起区分 WEBP 与 WAVE。
// 只看前 4 字节会把 wav 当成 webp。
func TestSniff_RIFF容器需二次确认(t *testing.T) {
	webp, _ := Sniff(head('R', 'I', 'F', 'F', 0, 0, 0, 0, 'W', 'E', 'B', 'P'))
	wav, _ := Sniff(head('R', 'I', 'F', 'F', 0, 0, 0, 0, 'W', 'A', 'V', 'E'))
	if webp.MIME == wav.MIME {
		t.Fatal("WEBP 与 WAVE 被识别成同一类型")
	}
	// RIFF 但既非 WEBP 也非 WAVE → 无法识别
	if _, err := Sniff(head('R', 'I', 'F', 'F', 0, 0, 0, 0, 'X', 'X', 'X', 'X')); err == nil {
		t.Error("未知 RIFF 子类型应识别失败")
	}
}

func TestSniff_未知类型(t *testing.T) {
	for _, bad := range [][]byte{
		head('M', 'Z'),            // PE 可执行文件
		head(0x7F, 'E', 'L', 'F'), // ELF
		head('<', '?', 'x', 'm', 'l'),
		{},
		head(),
	} {
		if _, err := Sniff(bad); err == nil {
			t.Errorf("非媒体文件应识别失败: %v", bad[:min(4, len(bad))])
		}
	}
}

// ⚠️ GO-28 的核心：声明的 MIME 不可信。
func TestVerifyDeclared_声明与实际不符(t *testing.T) {
	mp4 := head(0, 0, 0, 0x20, 'f', 't', 'y', 'p')
	// 把 mp4 声明成 png —— 典型的伪装上传。
	d, err := VerifyDeclared(mp4, "image/png")
	if err == nil {
		t.Fatal("声明与实际不符应报错")
	}
	// 错误里要给出**实际**类型，便于排查与告警。
	if d.MIME != "video/mp4" {
		t.Errorf("应返回实际类型 video/mp4，得到 %s", d.MIME)
	}
	if !strings.Contains(err.Error(), "video/mp4") {
		t.Errorf("错误应说明实际类型，实际 %v", err)
	}
}

// 浏览器常把 jpg 声明成 image/jpg，属良性偏差，归一化后应通过。
func TestVerifyDeclared_归一化良性偏差(t *testing.T) {
	jpeg := head(0xFF, 0xD8, 0xFF, 0xE0)
	for _, declared := range []string{"image/jpeg", "image/jpg", "IMAGE/JPEG", "image/jpeg; charset=binary"} {
		if _, err := VerifyDeclared(jpeg, declared); err != nil {
			t.Errorf("声明 %q 应被接受: %v", declared, err)
		}
	}
	wav := head('R', 'I', 'F', 'F', 0, 0, 0, 0, 'W', 'A', 'V', 'E')
	for _, declared := range []string{"audio/wav", "audio/x-wav", "audio/wave"} {
		if _, err := VerifyDeclared(wav, declared); err != nil {
			t.Errorf("声明 %q 应被接受: %v", declared, err)
		}
	}
}

func TestVerifyDeclared_空声明放行(t *testing.T) {
	png := head(0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A)
	d, err := VerifyDeclared(png, "")
	if err != nil || d.MIME != "image/png" {
		t.Errorf("空声明应按实际类型放行，得到 %s %v", d.MIME, err)
	}
}

func TestLimits(t *testing.T) {
	l := DefaultLimits(KindImage)
	png := Detected{MIME: "image/png", Kind: KindImage}
	if err := l.Check(png, 1<<20); err != nil {
		t.Errorf("1MB PNG 应通过: %v", err)
	}
	err := l.Check(png, 50<<20)
	if err == nil || !strings.Contains(err.Error(), "MB") {
		t.Errorf("超限应报错并给出具体大小，实际 %v", err)
	}
	// 图片限制下不接受视频
	if err := l.Check(Detected{MIME: "video/mp4", Kind: KindVideo}, 1<<20); err == nil {
		t.Error("图片限制不应接受 mp4")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
