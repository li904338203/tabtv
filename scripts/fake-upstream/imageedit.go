package main

// 图片编辑接口（openai_images 的 multipart `/images/edits`）的请求记录与「纯红」回图（IMGT-19，2026-10-31 方案变更后）。
//
// 图片节点工具条的四个工具都走现有图片大模型的图生图：受理把选项翻译成提示词 + aspect_ratio / quality / count，
// 情绪调节还要由 Worker 把源图裁成人物所在的那一块发上游、再把结果贴回原图。要验的是：
//   - 发出去的 prompt / size / n / quality 对不对（探针 `/_probe/image-edits`）；
//   - 情绪调节发出去的图片是不是**裁切区域那一块**（探针记每个 image 文件位解出来的像素尺寸）；
//   - 贴回是不是只动了人物框（回图是与收到的图**同尺寸的纯红 PNG**：框内红、框外不变才说明贴对了）。
//
// ⚠️ 向后兼容：只有路径里带 `/imagetools/` 的请求（`/v1/imagetools/images/edits`，E2E 段给自己的模型配的 submitPath）
// 才回纯红、按 n 回多张；老路径 `/v1/images/edits` 照旧回 1×1 PNG（其它段依赖它）。两条路径都记进探针。

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	_ "image/jpeg" // 收到的参考图可能是 JPEG
	"image/png"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// imageEditMaxRecords 探针最多留多少条（假上游会被长时间开着，别无限增长）。
const imageEditMaxRecords = 500

// imageEditFile 是一个文件位：字段名、文件名、字节数与解出来的像素尺寸（解不开为 0）。
type imageEditFile struct {
	Field    string `json:"field"`
	Filename string `json:"filename"`
	Bytes    int    `json:"bytes"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
}

// imageEditRecord 是一次编辑请求。
type imageEditRecord struct {
	Path    string          `json:"path"`
	Auth    string          `json:"auth"`
	Model   string          `json:"model"`
	Prompt  string          `json:"prompt"`
	Size    string          `json:"size"`
	N       int             `json:"n"`
	Quality string          `json:"quality"`
	Red     bool            `json:"red"`
	Files   []imageEditFile `json:"files"`
}

type imageEditProbe struct {
	mu   sync.Mutex
	reqs []imageEditRecord
}

func (p *imageEditProbe) add(r imageEditRecord) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqs = append(p.reqs, r)
	if over := len(p.reqs) - imageEditMaxRecords; over > 0 {
		p.reqs = append([]imageEditRecord(nil), p.reqs[over:]...)
	}
}

// handle 回全部记录（GET）；DELETE 清空。
func (p *imageEditProbe) handle(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.Method == http.MethodDelete {
		p.reqs = nil
	}
	list := append([]imageEditRecord{}, p.reqs...)
	writeJSON(w, map[string]any{"requests": list})
}

// authSummary 只记认证方式与长度，不记密钥原文（探针可能被贴进运行日志）。
func authSummary(h string) string {
	if h == "" {
		return ""
	}
	scheme, _, _ := strings.Cut(h, " ")
	return scheme + " <" + strconv.Itoa(len(h)) + " 字节>"
}

func (s *server) imageEdit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "编辑接口必须是 multipart: "+err.Error(), http.StatusBadRequest)
		return
	}
	red := strings.Contains(r.URL.Path, "/imagetools/")
	rec := imageEditRecord{
		Path: r.URL.Path, Auth: authSummary(r.Header.Get("Authorization")),
		Model: r.FormValue("model"), Prompt: r.FormValue("prompt"), Size: r.FormValue("size"),
		Quality: r.FormValue("quality"), Red: red,
	}
	rec.N, _ = strconv.Atoi(r.FormValue("n"))
	var first *imageEditFile
	for field, hdrs := range r.MultipartForm.File {
		for _, hdr := range hdrs {
			f, err := hdr.Open()
			if err != nil {
				continue
			}
			b, _ := io.ReadAll(f)
			_ = f.Close()
			fi := imageEditFile{Field: field, Filename: hdr.Filename, Bytes: len(b)}
			if cfg, _, err := image.DecodeConfig(bytes.NewReader(b)); err == nil {
				fi.Width, fi.Height = cfg.Width, cfg.Height
			}
			rec.Files = append(rec.Files, fi)
		}
	}
	for i := range rec.Files {
		if rec.Files[i].Field == "image" {
			first = &rec.Files[i]
			break
		}
	}
	s.imageEdits.add(rec)
	if first == nil {
		http.Error(w, "缺少 image 文件位", http.StatusBadRequest)
		return
	}
	log.Printf("  编辑接口 %s 收到 %d 个文件（image %dx%d），n=%d size=%s prompt=%q",
		r.URL.Path, len(rec.Files), first.Width, first.Height, rec.N, rec.Size, rec.Prompt)
	if !red {
		writeJSON(w, map[string]any{"data": []any{map[string]any{"b64_json": onePixelPNG}}})
		return
	}
	if first.Width <= 0 || first.Height <= 0 {
		http.Error(w, "image 文件位解不出像素尺寸", http.StatusBadRequest)
		return
	}
	n := rec.N
	if n <= 0 {
		n = 1
	}
	b64 := base64.StdEncoding.EncodeToString(redPNG(first.Width, first.Height))
	data := make([]any, 0, n)
	for i := 0; i < n; i++ {
		data = append(data, map[string]any{"b64_json": b64})
	}
	writeJSON(w, map[string]any{"data": data})
}

// redPNG 是一张 w×h 的纯红不透明 PNG。
func redPNG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	red := color.RGBA{255, 0, 0, 255}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, red)
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
