package domain

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// 泼溅文件的第二段识别（3D 导演台 DRD-28）。第一段 SniffSplat 只看了 16 字节，只能初判：
// 以 `ply` 开头的任意文本、任意 gzip 压缩包都能过第一段。这里在文件落到临时文件之后读结构。
//
// SPZ 的格式依据：nianticlabs/spz 的 src/cc/load-spz.h（`NGSP_MAGIC = 0x5053474e`、
// `LATEST_SPZ_HEADER_VERSION = 4`）与 load-spz.cc（v4 起的 32 字节头 `NgspFileHeader`；
// 旧格式整文件 gzip，按 gzip 魔数 1F 8B 区分），2026-10-26 核对。

// spzMagic 是 SPZ 头的魔数 0x5053474e 按小端写出的字节，即 `NGSP`。
var spzMagic = []byte{'N', 'G', 'S', 'P'}

// plyHeaderLimit 是 PLY 头部的读取上限：3DGS 的 PLY 头部通常不到 2 KiB，64 KiB 读不到
// `end_header` 的不是我们要的文件（也免得把一个几百 MB 的文本文件整个读进来）。
const plyHeaderLimit = 64 << 10

// spzV4HeaderSize 是 v4 起不压缩的文件头大小（load-spz.cc 的 static_assert）。
const spzV4HeaderSize = 32

// ErrNotSplat 表示文件过了第一段的文件头初判，但结构不是泼溅文件。
var ErrNotSplat = errors.New("不是有效的 3D 高斯泼溅文件（PLY / SPZ）")

// VerifySplat 是泼溅识别的第二段：按第一段给出的 MIME 读文件结构。
func VerifySplat(path, mime string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	switch mime {
	case MIMEPly:
		return verifyPLY(f)
	case MIMESpz:
		info, err := f.Stat()
		if err != nil {
			return err
		}
		return verifySPZ(f, info.Size())
	}
	return fmt.Errorf("%w：未知的泼溅类型 %s", ErrNotSplat, mime)
}

// verifyPLY 读到 `end_header` 为止：第一行是 `ply`，要有 `format` 行，且有点数大于 0 的 `element vertex`。
func verifyPLY(r io.Reader) error {
	sc := bufio.NewScanner(io.LimitReader(r, plyHeaderLimit))
	sc.Buffer(make([]byte, 0, 4096), plyHeaderLimit)
	line := 0
	hasFormat, vertices := false, int64(0)
	for sc.Scan() {
		text := strings.TrimRight(sc.Text(), "\r")
		line++
		if line == 1 {
			if text != "ply" {
				return fmt.Errorf("%w：PLY 第一行必须是 ply", ErrNotSplat)
			}
			continue
		}
		fields := strings.Fields(text)
		switch {
		case len(fields) == 0:
			continue
		case fields[0] == "format":
			if len(fields) < 3 || !validPLYFormat(fields[1]) {
				return fmt.Errorf("%w：PLY 的 format 行不合法：%q", ErrNotSplat, text)
			}
			hasFormat = true
		case fields[0] == "element" && len(fields) >= 3 && fields[1] == "vertex":
			n, err := strconv.ParseInt(fields[2], 10, 64)
			if err != nil || n <= 0 {
				return fmt.Errorf("%w：PLY 的点数不合法：%q", ErrNotSplat, text)
			}
			vertices = n
		case fields[0] == "end_header":
			if !hasFormat {
				return fmt.Errorf("%w：PLY 头部缺少 format 行", ErrNotSplat)
			}
			if vertices == 0 {
				return fmt.Errorf("%w：PLY 头部缺少 element vertex", ErrNotSplat)
			}
			return nil
		}
	}
	return fmt.Errorf("%w：前 64 KiB 内没有读到 PLY 的 end_header", ErrNotSplat)
}

func validPLYFormat(f string) bool {
	switch f {
	case "ascii", "binary_little_endian", "binary_big_endian":
		return true
	}
	return false
}

// verifySPZ 区分两种容器：
//   - v4 起：开头 32 字节不压缩，魔数 `NGSP`、版本 ≥ 4，目录（每条流 16 字节）要落在文件里；
//   - v1~v3：整个文件是 gzip，解压后前 8 字节是魔数与版本（≥ 1）。普通 gzip 解压出来不是 NGSP，在这里被拒。
func verifySPZ(r io.ReadSeeker, size int64) error {
	head := make([]byte, spzV4HeaderSize)
	n, err := io.ReadFull(r, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return err
	}
	head = head[:n]
	if bytes.HasPrefix(head, spzMagic) {
		if n < spzV4HeaderSize {
			return fmt.Errorf("%w：SPZ 文件头不完整", ErrNotSplat)
		}
		version := binary.LittleEndian.Uint32(head[4:8])
		numStreams := int64(head[15])
		tocOffset := int64(binary.LittleEndian.Uint32(head[16:20]))
		if version < 4 {
			return fmt.Errorf("%w：不压缩的 SPZ 头只出现在 v4 起，实际版本 %d", ErrNotSplat, version)
		}
		if numStreams == 0 || tocOffset < spzV4HeaderSize || tocOffset+numStreams*16 > size {
			return fmt.Errorf("%w：SPZ 的数据目录不在文件范围内", ErrNotSplat)
		}
		return nil
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return err
	}
	zr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("%w：gzip 头不合法", ErrNotSplat)
	}
	defer func() { _ = zr.Close() }()
	inner := make([]byte, 8)
	if _, err := io.ReadFull(zr, inner); err != nil {
		return fmt.Errorf("%w：解压后读不到 SPZ 头", ErrNotSplat)
	}
	if !bytes.Equal(inner[:4], spzMagic) {
		return fmt.Errorf("%w：解压后不是 SPZ（普通 gzip 压缩包）", ErrNotSplat)
	}
	if binary.LittleEndian.Uint32(inner[4:8]) < 1 {
		return fmt.Errorf("%w：SPZ 版本不合法", ErrNotSplat)
	}
	return nil
}
