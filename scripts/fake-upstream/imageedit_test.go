package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newImageEditServer(t *testing.T) (*server, *httptest.Server) {
	t.Helper()
	s := &server{addr: "http://fake"}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/images/edits", s.imageEdit)
	mux.HandleFunc("/v1/imagetools/images/edits", s.imageEdit)
	mux.HandleFunc("/_probe/image-edits", s.imageEdits.handle)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return s, srv
}

func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = byte(i * 7)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// postEdit 发一次 multipart 编辑请求，返回状态码与 data[].b64_json。
func postEdit(t *testing.T, url string, fields map[string]string, files map[string][]byte) (int, []string) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	for field, b := range files {
		fw, _ := mw.CreateFormFile(field, field+".png")
		_, _ = fw.Write(b)
	}
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, url, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer sk-secret-value")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			B64 string `json:"b64_json"`
		} `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	list := make([]string, 0, len(out.Data))
	for _, d := range out.Data {
		list = append(list, d.B64)
	}
	return resp.StatusCode, list
}

func probeEdits(t *testing.T, base string) []imageEditRecord {
	t.Helper()
	resp, err := http.Get(base + "/_probe/image-edits")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Requests []imageEditRecord `json:"requests"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Requests
}

// 老路径保持原行为：恒回一张 1×1 PNG（其它 E2E 段依赖它），但同样记进探针。
func TestImageEditLegacyPathStillOnePixel(t *testing.T) {
	_, srv := newImageEditServer(t)
	code, data := postEdit(t, srv.URL+"/v1/images/edits",
		map[string]string{"prompt": "p", "n": "3", "size": "1024x1024"}, map[string][]byte{"image": testPNG(t, 40, 30)})
	if code != 200 || len(data) != 1 || data[0] != onePixelPNG {
		t.Fatalf("老路径应恒回 1 张 1×1：code=%d n=%d", code, len(data))
	}
	recs := probeEdits(t, srv.URL)
	if len(recs) != 1 || recs[0].Red || recs[0].N != 3 || recs[0].Files[0].Width != 40 || recs[0].Files[0].Height != 30 {
		t.Fatalf("探针记录不对：%+v", recs)
	}
}

// 工具条路径：按 n 回多张、每张与收到的图同尺寸的纯红；探针记 prompt / size / 像素尺寸，认证头不记原文。
func TestImageEditToolsPathRedSameSize(t *testing.T) {
	_, srv := newImageEditServer(t)
	code, data := postEdit(t, srv.URL+"/v1/imagetools/images/edits",
		map[string]string{"prompt": "表情改为欣喜若狂", "n": "2", "size": "1024x1536", "quality": "high", "model": "m1"},
		map[string][]byte{"image": testPNG(t, 192, 324)})
	if code != 200 || len(data) != 2 {
		t.Fatalf("应回 2 张：code=%d n=%d", code, len(data))
	}
	for _, b64 := range data {
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.Decode(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		if b := img.Bounds(); b.Dx() != 192 || b.Dy() != 324 {
			t.Fatalf("尺寸应 192×324，实际 %v", b)
		}
		if c := color.RGBAModel.Convert(img.At(100, 200)).(color.RGBA); c != (color.RGBA{255, 0, 0, 255}) {
			t.Fatalf("应纯红，实际 %v", c)
		}
	}
	recs := probeEdits(t, srv.URL)
	if len(recs) != 1 {
		t.Fatalf("应记 1 条：%+v", recs)
	}
	r := recs[0]
	if !r.Red || r.Prompt != "表情改为欣喜若狂" || r.Size != "1024x1536" || r.N != 2 || r.Quality != "high" ||
		r.Model != "m1" || len(r.Files) != 1 || r.Files[0].Width != 192 || r.Files[0].Height != 324 {
		t.Fatalf("探针记录不对：%+v", r)
	}
	if strings.Contains(r.Auth, "sk-secret-value") || !strings.HasPrefix(r.Auth, "Bearer ") {
		t.Fatalf("认证头只记方式与长度：%q", r.Auth)
	}
	// DELETE 清空。
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/_probe/image-edits", nil)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
	if n := len(probeEdits(t, srv.URL)); n != 0 {
		t.Fatalf("DELETE 后应为空，实际 %d", n)
	}
}

func TestImageEditMissingImage400(t *testing.T) {
	_, srv := newImageEditServer(t)
	code, _ := postEdit(t, srv.URL+"/v1/imagetools/images/edits", map[string]string{"prompt": "p"}, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("缺 image 应 400，实际 %d", code)
	}
}

func TestImageEditProbeCapped(t *testing.T) {
	var p imageEditProbe
	for i := 0; i < imageEditMaxRecords+10; i++ {
		p.add(imageEditRecord{N: i})
	}
	if len(p.reqs) != imageEditMaxRecords || p.reqs[0].N != 10 {
		t.Fatalf("应只留最近 %d 条：len=%d first=%d", imageEditMaxRecords, len(p.reqs), p.reqs[0].N)
	}
}
