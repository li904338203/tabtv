package domain

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// 泼溅识别（3D 导演台 DRD-28）。样本在测试里现场构造（同 sniff_test.go，仓库没有 testdata 目录）。

const plyHeader = "ply\nformat binary_little_endian 1.0\nelement vertex 3\nproperty float x\n" +
	"property float y\nproperty float z\nproperty float opacity\nend_header\n"

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// legacySPZ 是 v1~v3 的容器：整个文件 gzip，解压后是 NGSP 头。
func legacySPZ(t *testing.T, version uint32) []byte {
	t.Helper()
	inner := make([]byte, 16)
	copy(inner, spzMagic)
	binary.LittleEndian.PutUint32(inner[4:8], version)
	binary.LittleEndian.PutUint32(inner[8:12], 100)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(append(inner, make([]byte, 64)...)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// v4SPZ 是 v4 起的容器：32 字节不压缩的头，后面是目录与（这里随便填的）数据流。
func v4SPZ(version uint32, numStreams byte, tocOffset uint32, total int) []byte {
	b := make([]byte, total)
	copy(b, spzMagic)
	binary.LittleEndian.PutUint32(b[4:8], version)
	binary.LittleEndian.PutUint32(b[8:12], 100)
	b[15] = numStreams
	binary.LittleEndian.PutUint32(b[16:20], tocOffset)
	return b
}

func plainGzip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte("这只是一个普通的压缩文本，不是泼溅文件"))
	_ = zw.Close()
	return buf.Bytes()
}

func head16(b []byte) []byte {
	if len(b) > SniffLen {
		return b[:SniffLen]
	}
	return b
}

func TestSniffSplat_PLY与SPZ(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		mime string
	}{
		{"PLY（LF）", []byte(plyHeader), MIMEPly},
		{"PLY（CRLF）", []byte("ply\r\nformat ascii 1.0\r\nelement vertex 1\r\nend_header\r\n"), MIMEPly},
		{"SPZ v2（gzip 容器）", legacySPZ(t, 2), MIMESpz},
		{"SPZ v4（不压缩的头）", v4SPZ(4, 2, 32, 32+2*16+10), MIMESpz},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := SniffSplat(head16(c.data))
			if err != nil || d.MIME != c.mime || d.Kind != KindSplat {
				t.Fatalf("第一段应识别为 %s，得到 %+v %v", c.mime, d, err)
			}
			if err := VerifySplat(writeTemp(t, "f", c.data), d.MIME); err != nil {
				t.Fatalf("第二段应通过：%v", err)
			}
		})
	}
}

func TestSniffSplat_普通gzip不是SPZ(t *testing.T) {
	data := plainGzip(t)
	d, err := SniffSplat(head16(data))
	if err != nil {
		t.Fatalf("第一段只看 gzip 头，应初判为 SPZ 候选：%v", err)
	}
	if err := VerifySplat(writeTemp(t, "x.spz", data), d.MIME); !errors.Is(err, ErrNotSplat) {
		t.Fatalf("普通 gzip 解压后不是 NGSP，第二段应拒绝，得到 %v", err)
	}
}

func TestSniffSplat_结构不对的被第二段拒绝(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		mime string
	}{
		{"PLY 没有 element vertex", []byte("ply\nformat ascii 1.0\nend_header\n"), MIMEPly},
		{"PLY 点数为 0", []byte("ply\nformat ascii 1.0\nelement vertex 0\nend_header\n"), MIMEPly},
		{"PLY 没有 end_header", []byte("ply\nformat ascii 1.0\nelement vertex 3\n"), MIMEPly},
		{"以 ply 开头的普通文本", []byte("ply\nthis is just a note\n"), MIMEPly},
		{"SPZ v4 目录越界", v4SPZ(4, 2, 32, 40), MIMESpz},
		{"SPZ 不压缩的头却是旧版本", v4SPZ(2, 1, 32, 64), MIMESpz},
		{"SPZ 头不完整", []byte("NGSP\x04\x00"), MIMESpz},
		{"gzip 容器里版本为 0", legacySPZ(t, 0), MIMESpz},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := VerifySplat(writeTemp(t, "f", c.data), c.mime); !errors.Is(err, ErrNotSplat) {
				t.Fatalf("应被第二段拒绝，得到 %v", err)
			}
		})
	}
}

func TestSniffSplat_伪装扩展名按文件头(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0x0D, 'I', 'H', 'D', 'R'}
	if _, err := SniffSplat(png); !errors.Is(err, ErrUnknownType) {
		t.Fatalf("改了扩展名的 PNG 不是泼溅，应 ErrUnknownType，得到 %v", err)
	}
	// 文件头是泼溅、声明却是图片：伪装上传，交叉校验要拒。
	if _, err := VerifySplatDeclared([]byte(plyHeader)[:SniffLen], "image/png"); err == nil {
		t.Fatal("声明为 image/png 的 PLY 应被拒")
	}
	// 浏览器不认识 .ply / .spz：空串与 octet-stream 都要放行。
	for _, declared := range []string{"", "application/octet-stream", "model/ply", MIMESpz} {
		if _, err := VerifySplatDeclared([]byte(plyHeader)[:SniffLen], declared); err != nil {
			t.Errorf("声明 %q 不该拦：%v", declared, err)
		}
	}
}

func TestSniff_通用识别不认泼溅(t *testing.T) {
	for name, data := range map[string][]byte{
		"PLY": []byte(plyHeader), "SPZ gzip": legacySPZ(t, 2), "SPZ v4": v4SPZ(4, 1, 32, 64),
	} {
		if _, err := Sniff(head16(data)); !errors.Is(err, ErrUnknownType) {
			t.Errorf("%s：通用识别不能认泼溅（否则会作为参考图进「我的素材」），得到 %v", name, err)
		}
	}
}

func TestUploadMaxBytes_只能收紧(t *testing.T) {
	l := DefaultLimits(KindSplat)
	if l.MaxBytes != 100<<20 || !l.Allowed[MIMEPly] || !l.Allowed[MIMESpz] {
		t.Fatalf("泼溅默认上限 100 MiB、允许 PLY 与 SPZ，得到 %+v", l)
	}
	video := DefaultLimits(KindVideo)
	if got := video.Tighten(50 << 20).MaxBytes; got != 50<<20 {
		t.Errorf("收紧到 50 MiB，得到 %d", got)
	}
	if got := video.Tighten(500 << 20).MaxBytes; got != 100<<20 {
		t.Errorf("不能放宽：仍应是 100 MiB，得到 %d", got)
	}
	if got := video.Tighten(0).MaxBytes; got != 100<<20 {
		t.Errorf("0 表示不收紧，得到 %d", got)
	}
}
