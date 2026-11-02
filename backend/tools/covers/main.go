// Command covers 把内置内容的封面与营销角色图缩小并转成 JPEG，输出到 seed 数据目录（TT-T-24）。
//
// 一次性工具：源图（来源系统本机的原图，几 MB 一张）不进仓库；仓库里只放这里产出的缩略版本。
// 文件名保持「编码.jpg」：选项 / 技能按 code 对应，营销角色按数据文件 images[] 里的文件名（扩展名改为 .jpg）对应。
//
//	go run ./tools/covers -src <源目录> -out internal/platform/seed/data/images/options [-max 768 -quality 82]
package main

import (
	"flag"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/image/draw"
)

func main() {
	src := flag.String("src", "", "源图目录（png / jpg）")
	out := flag.String("out", "", "输出目录")
	maxSide := flag.Int("max", 768, "长边上限（像素）")
	quality := flag.Int("quality", 82, "JPEG 质量")
	flag.Parse()
	if *src == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "用法：covers -src <源目录> -out <输出目录>")
		os.Exit(2)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fail(err)
	}
	entries, err := os.ReadDir(*src)
	if err != nil {
		fail(err)
	}
	names := []string{}
	for _, e := range entries {
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if !e.IsDir() && (ext == ".png" || ext == ".jpg" || ext == ".jpeg") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var total int64
	for _, name := range names {
		n, err := convert(filepath.Join(*src, name), filepath.Join(*out, strings.TrimSuffix(name, filepath.Ext(name))+".jpg"),
			*maxSide, *quality)
		if err != nil {
			fail(fmt.Errorf("%s: %w", name, err))
		}
		total += n
	}
	fmt.Printf("%d 张，共 %.1f MB → %s\n", len(names), float64(total)/1048576, *out)
}

func convert(in, out string, maxSide, quality int) (int64, error) {
	f, err := os.Open(in)
	if err != nil {
		return 0, err
	}
	img, _, err := image.Decode(f)
	f.Close()
	if err != nil {
		return 0, err
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w > maxSide || h > maxSide {
		if w >= h {
			h, w = h*maxSide/w, maxSide
		} else {
			w, h = w*maxSide/h, maxSide
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Src, nil)
	o, err := os.Create(out)
	if err != nil {
		return 0, err
	}
	if err := jpeg.Encode(o, dst, &jpeg.Options{Quality: quality}); err != nil {
		o.Close()
		return 0, err
	}
	if err := o.Close(); err != nil {
		return 0, err
	}
	st, err := os.Stat(out)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
