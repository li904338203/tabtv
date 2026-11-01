package typeset

import (
	"bytes"
	"encoding/json"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func postTypeset(t *testing.T, h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodPost, "/smartcut/typeset", strings.NewReader(body)))
	return w
}

func TestHandlerRejectsWithFieldPath(t *testing.T) {
	testFonts(t)
	h := Handler(New(Options{}))
	long, _ := json.Marshal(strings.Repeat("字", MaxTextRunes+1))
	for _, c := range []struct {
		name, body, path string
		code             int
	}{
		{"缺文字", `{"width":1280,"height":720}`, "text", 40001},
		{"空文字", `{"text":"  ","width":1280,"height":720}`, "text", 40001},
		{"超过 500 字", `{"text":` + string(long) + `,"width":1280,"height":720}`, "text", 40001},
		{"缺宽", `{"text":"字","height":720}`, "width", 40001},
		{"宽过大", `{"text":"字","width":4097,"height":720}`, "width", 40001},
		{"高过小", `{"text":"字","width":1280,"height":10}`, "height", 40001},
		{"种类", `{"text":"字","width":1280,"height":720,"kind":"title"}`, "kind", 40001},
		{"字号过大", `{"text":"字","width":1280,"height":720,"style":{"fontSizePct":21}}`, "style.fontSizePct", 40001},
		{"字号过小", `{"text":"字","width":1280,"height":720,"style":{"fontSizePct":1}}`, "style.fontSizePct", 40001},
		{"颜色写法", `{"text":"字","width":1280,"height":720,"style":{"color":"red"}}`, "style.color", 40001},
		{"四位颜色", `{"text":"字","width":1280,"height":720,"style":{"color":"#FFFF"}}`, "style.color", 40001},
		{"描边过粗", `{"text":"字","width":1280,"height":720,"style":{"stroke":{"widthPct":6}}}`, "style.stroke.widthPct", 40001},
		{"底框透明度", `{"text":"字","width":1280,"height":720,"style":{"background":{"opacity":2}}}`, "style.background.opacity", 40001},
		{"底框内边距", `{"text":"字","width":1280,"height":720,"style":{"background":{"paddingPct":11}}}`, "style.background.paddingPct", 40001},
		{"对齐", `{"text":"字","width":1280,"height":720,"style":{"align":"justify"}}`, "style.align", 40001},
		{"最大宽度", `{"text":"字","width":1280,"height":720,"style":{"maxWidthPct":5}}`, "style.maxWidthPct", 40001},
		{"字幕距底", `{"text":"字","width":1280,"height":720,"kind":"subtitle","style":{"bottomPct":60}}`, "style.bottomPct", 40001},
		{"位置框", `{"text":"字","width":1280,"height":720,"box":{"xPct":120}}`, "box.xPct", 40001},
		{"锚点", `{"text":"字","width":1280,"height":720,"box":{"anchor":"middle"}}`, "box.anchor", 40001},
		{"放不进画面", `{"text":"` + strings.Repeat("字", 400) + `","width":64,"height":3840,"style":{"fontSizePct":20,"maxWidthPct":100}}`, "text", 40001},
		{"不是 JSON", `{"text":`, "", 40003},
	} {
		w := postTypeset(t, h, c.body)
		var got struct {
			Code    int            `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s：响应不是错误 JSON：%d %s", c.name, w.Code, w.Body.String())
		}
		if w.Code != http.StatusBadRequest || got.Code != c.code {
			t.Errorf("%s：HTTP %d / code %d，期望 400 / %d（%s）", c.name, w.Code, got.Code, c.code, got.Message)
			continue
		}
		if c.path != "" && got.Details["path"] != c.path {
			t.Errorf("%s：details.path = %v，期望 %s（%s）", c.name, got.Details["path"], c.path, got.Message)
		}
	}
}

func TestHandlerReturnsPNGWithHeaders(t *testing.T) {
	testFonts(t)
	h := Handler(New(Options{}))
	w := postTypeset(t, h, `{"text":"字幕مرحبا😀","width":1920,"height":1080,"kind":"subtitle",
		"style":{"color":"#FFD400","stroke":{"color":"#000","widthPct":0.4},"background":{"color":"#00000080","paddingPct":1}}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d：%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q", ct)
	}
	img, err := png.Decode(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatalf("不是 PNG：%v", err)
	}
	parts := strings.Split(w.Header().Get(HeaderBox), ",")
	if len(parts) != 4 {
		t.Fatalf("%s = %q", HeaderBox, w.Header().Get(HeaderBox))
	}
	var box [4]int
	for i, p := range parts {
		if box[i], err = strconv.Atoi(p); err != nil {
			t.Fatalf("%s = %q", HeaderBox, w.Header().Get(HeaderBox))
		}
	}
	if box[2] != img.Bounds().Dx() || box[3] != img.Bounds().Dy() {
		t.Errorf("包围框 %v 与 PNG 尺寸 %v 不一致", box, img.Bounds())
	}
	if box[0] < 0 || box[1] < 0 || box[0]+box[2] > 1920 || box[1]+box[3] > 1080 {
		t.Errorf("包围框伸出了画面：%v", box)
	}
	missing, err := url.PathUnescape(w.Header().Get(HeaderMissing))
	if err != nil || missing != "مرحبا" {
		t.Errorf("%s 解出来是 %q（%v），期望阿拉伯文那几个字", HeaderMissing, missing, err)
	}
	// 没有缺字时头仍然在，值为空。
	w = postTypeset(t, h, `{"text":"都有字形","width":640,"height":360}`)
	if w.Code != http.StatusOK || w.Header().Get(HeaderMissing) != "" || len(w.Header().Values(HeaderMissing)) != 1 {
		t.Errorf("无缺字时：HTTP %d，%s = %q", w.Code, HeaderMissing, w.Header().Values(HeaderMissing))
	}
}

func TestHandlerFontsMissingIs503(t *testing.T) {
	h := Handler(New(Options{FontDir: t.TempDir()}))
	w := postTypeset(t, h, `{"text":"字","width":640,"height":360}`)
	var got struct {
		Code int `json:"code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusServiceUnavailable || got.Code != 50301 {
		t.Fatalf("字体缺失应回 503 / 50301：HTTP %d %s", w.Code, w.Body.String())
	}
}
