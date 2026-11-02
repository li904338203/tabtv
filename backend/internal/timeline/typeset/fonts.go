package typeset

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/li904338203/tabtv/backend/internal/platform/resources"

	"github.com/go-text/typesetting/font"
	"github.com/go-text/typesetting/shaping"
)

// 字体文件名与 scripts/fetch-fonts.sh 一致（字体不进 git，由脚本按 sha256 下载）。
//
// 只有常规体：SCT-03 定的文字样式里没有粗细这一项，带一个用不上的粗体只是多 16 MB。
// 将来加粗细字段时，在脚本里加一行思源黑体 Bold、这里加一个面即可。
const (
	FileRegular = "SourceHanSansSC-Regular.otf"
	FileEmoji   = "NotoEmoji-VariableFont.ttf"
)

// EnvFontDir 覆盖字体目录（部署时字体不在源码树里，dev/01 §10）。
const EnvFontDir = "SEEAITV_TYPESET_FONT_DIR"

// FontSet 是排版用到的全部字体。
//
// 思源黑体的语言版 OTF 含中日韩全部字形（默认简中字形）；emoji 用 Noto Emoji 单色。
// 两者都没有的字符画主字体的 .notdef（方框），并进缺字列表（SC-F-53）。
type FontSet struct {
	regular *font.Font
	emoji   *font.Font
	// version 是全部字体文件内容的哈希：换了字体，同一段文字排出来就不一样，缓存键必须跟着变。
	version string
	pool    sync.Pool // *faces
}

// faces 是一次排版借用的一组 Face 加一个整形器。
//
// ⚠️ go-text 的 *font.Font 可以并发使用，但 *font.Face 与 HarfbuzzShaper **不行**（Face 里
// 有字形与 cmap 缓存）。并发请求共用一组 Face 的话，缓存会被写坏，表现是偶发地排错字形 ——
// 所以每次排版从池里借一组、用完还回去。
type faces struct {
	regular *font.Face
	emoji   *font.Face
	shaper  shaping.HarfbuzzShaper
}

func (f *FontSet) acquire() *faces {
	if x, ok := f.pool.Get().(*faces); ok {
		return x
	}
	return &faces{regular: font.NewFace(f.regular), emoji: font.NewFace(f.emoji)}
}

func (f *FontSet) release(x *faces) { f.pool.Put(x) }

// ResolveFontDir 取字体目录：显式传入的 > 环境变量 > 随包资源 resources/fonts > 从当前目录往上找 backend/assets/fonts。
//
// ⚠️ 随包资源必须排在「往上找」之前（TT-T-25）：安装包里没有源码树，往上找必然落空，
//
//	而落空的表现是「带字幕的导出失败：字体未就绪」—— 看起来像用户少装了什么。
//
// 往上找是给 `go test` 与开发运行用的：两者的工作目录不同，但都在源码树里。
func ResolveFontDir(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if d := os.Getenv(EnvFontDir); d != "" {
		return d
	}
	if root := resources.Root(); root != "" {
		if cand := filepath.Join(root, "fonts"); fileExists(filepath.Join(cand, FileRegular)) {
			return cand
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		return "assets/fonts"
	}
	for dir := wd; ; dir = filepath.Dir(dir) {
		for _, rel := range []string{"backend/assets/fonts", "assets/fonts"} {
			cand := filepath.Join(dir, rel)
			if _, err := os.Stat(filepath.Join(cand, FileRegular)); err == nil {
				return cand
			}
		}
		if filepath.Dir(dir) == dir {
			return filepath.Join(wd, "assets/fonts")
		}
	}
}

// LoadFonts 读入并解析字体。缺文件时报错要说清怎么补（不静默降级：
// 没有字体的话所有字幕都会变成方框，而任务是「成功」的）。
func LoadFonts(dir string) (*FontSet, error) {
	h := sha256.New()
	load := func(name string) (*font.Font, error) {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("读取字体 %s 失败：%w（开发时运行 make fonts；安装包里应在 resources/fonts，缺失说明安装包不完整）", path, err)
		}
		face, err := font.ParseTTF(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("解析字体 %s 失败：%w", path, err)
		}
		h.Write([]byte(name))
		h.Write(data)
		return face.Font, nil
	}
	regular, err := load(FileRegular)
	if err != nil {
		return nil, err
	}
	emoji, err := load(FileEmoji)
	if err != nil {
		return nil, err
	}
	return &FontSet{
		regular: regular,
		emoji:   emoji,
		version: hex.EncodeToString(h.Sum(nil))[:16],
	}, nil
}

// Version 是字体内容的哈希（计入缓存键）。
func (f *FontSet) Version() string { return f.version }

// faceFor 为一个字符选字体：主字体有就用主字体，否则 emoji 字体，都没有就记缺字、用主字体画 .notdef。
func (f *faces) faceFor(r rune) (face *font.Face, missing bool) {
	p := f.regular
	if _, ok := p.NominalGlyph(r); ok {
		return p, false
	}
	if _, ok := f.emoji.NominalGlyph(r); ok {
		return f.emoji, false
	}
	return p, true
}

// isJoiner 是不独立成字的码位：变体选择符、零宽连接符、emoji 肤色修饰。
// 它们跟着前一个字符走同一个字体，也不进缺字列表（单独看它们在主字体里多半没有字形）。
func isJoiner(r rune) bool {
	switch {
	case r == 0x200D, r == 0x200C: // ZWJ / ZWNJ
		return true
	case r >= 0xFE00 && r <= 0xFE0F: // 变体选择符
		return true
	case r >= 0xE0100 && r <= 0xE01EF:
		return true
	case r >= 0x1F3FB && r <= 0x1F3FF: // 肤色修饰
		return true
	}
	return false
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
