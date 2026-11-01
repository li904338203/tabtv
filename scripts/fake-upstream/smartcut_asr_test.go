package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func newASRServer(t *testing.T) (*server, *httptest.Server) {
	t.Helper()
	s := &server{addr: "http://fake"}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/auc/bigmodel/recognize/flash", s.volcFlash)
	mux.HandleFunc("/api/v3/auc/bigmodel/submit", s.volcSubmit)
	mux.HandleFunc("/api/v3/auc/bigmodel/query", s.volcQuery)
	mux.HandleFunc("/v1/audio/transcriptions", s.openAITranscribe)
	mux.HandleFunc("/_probe/asr/next", s.asr.handleNext)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return s, srv
}

const testRequestID = "11111111-2222-5333-8444-555555555555"

func volcPost(t *testing.T, url string, body any, key bool) (string, string) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	if key {
		req.Header.Set("X-Api-Key", "k")
	}
	req.Header.Set("X-Api-Resource-Id", "volc.bigasr.auc_turbo")
	req.Header.Set("X-Api-Request-Id", testRequestID)
	req.Header.Set("X-Api-Sequence", "-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("火山一律回 HTTP 200，实际 %d", resp.StatusCode)
	}
	return resp.Header.Get("X-Api-Status-Code"), string(out)
}

func asrBody(audio map[string]any) map[string]any {
	return map[string]any{"user": map[string]any{"uid": "u"}, "audio": audio,
		"request": map[string]any{"model_name": "bigmodel", "show_utterances": true, "enable_ddc": false}}
}

func TestVolcFlash_校验形状并按探针回结果(t *testing.T) {
	s, srv := newASRServer(t)
	data := base64.StdEncoding.EncodeToString([]byte("ID3audio"))
	if code, body := volcPost(t, srv.URL+"/api/v3/auc/bigmodel/recognize/flash",
		asrBody(map[string]any{"data": data, "format": "mp3"}), true); code != "20000000" || !strings.Contains(body, "utterances") {
		t.Fatalf("完成: %s %s", code, body)
	}
	if code, _ := volcPost(t, srv.URL+"/api/v3/auc/bigmodel/recognize/flash",
		asrBody(map[string]any{"data": data}), false); code != "45000001" {
		t.Fatalf("缺认证头应回参数错误: %s", code)
	}
	if code, _ := volcPost(t, srv.URL+"/api/v3/auc/bigmodel/recognize/flash",
		asrBody(map[string]any{"data": "data:audio/mpeg;base64," + data}), true); code != "45000001" {
		t.Fatalf("没去掉 data URI 前缀应回参数错误: %s", code)
	}
	_, _ = http.Post(srv.URL+"/_probe/asr/next?outcome=silent", "text/plain", nil)
	if code, body := volcPost(t, srv.URL+"/api/v3/auc/bigmodel/recognize/flash",
		asrBody(map[string]any{"data": data}), true); code != "20000003" || !strings.Contains(body, `"text":""`) {
		t.Fatalf("探针设了静音: %s %s", code, body)
	}
	if got := len(s.asr.recs); got != 4 || s.asr.recs[0].Audio != "data" || s.asr.recs[0].AudioBytes != 8 {
		t.Fatalf("请求记录: %+v", s.asr.recs)
	}
}

func TestVolcStandard_先处理中后完成且查询要带同一个请求ID(t *testing.T) {
	_, srv := newASRServer(t)
	if code, _ := volcPost(t, srv.URL+"/api/v3/auc/bigmodel/submit",
		asrBody(map[string]any{"data": "AAAA"}), true); code != "45000001" {
		t.Fatalf("标准版不收内联: %s", code)
	}
	if code, _ := volcPost(t, srv.URL+"/api/v3/auc/bigmodel/submit",
		asrBody(map[string]any{"url": "https://relay.example.com/a.mp3", "format": "mp3"}), true); code != "20000000" {
		t.Fatalf("提交: %s", code)
	}
	if code, _ := volcPost(t, srv.URL+"/api/v3/auc/bigmodel/query", map[string]any{}, true); code != "20000001" {
		t.Fatalf("第一次查询处理中: %s", code)
	}
	if code, body := volcPost(t, srv.URL+"/api/v3/auc/bigmodel/query", map[string]any{}, true); code != "20000000" ||
		!strings.Contains(body, "words") {
		t.Fatalf("第二次查询完成: %s %s", code, body)
	}
}

func TestOpenAITranscribe_whisper回词级其它回文字(t *testing.T) {
	_, srv := newASRServer(t)
	post := func(model, format string, grans ...string) (int, string) {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		_ = w.WriteField("model", model)
		_ = w.WriteField("response_format", format)
		for _, g := range grans {
			_ = w.WriteField("timestamp_granularities[]", g)
		}
		fw, _ := w.CreateFormFile("file", "audio.mp3")
		_, _ = fw.Write([]byte("ID3audio"))
		_ = w.Close()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/audio/transcriptions", &buf)
		req.Header.Set("Content-Type", w.FormDataContentType())
		req.Header.Set("Authorization", "Bearer sk")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if st, body := post("whisper-1", "verbose_json", "word", "segment"); st != 200 || !strings.Contains(body, `"words"`) {
		t.Fatalf("whisper-1 词级: %d %s", st, body)
	}
	if st, _ := post("whisper-1", "verbose_json", "word"); st != 400 {
		t.Fatalf("少了 segment 粒度应 400: %d", st)
	}
	if st, body := post("gpt-4o-transcribe", "json"); st != 200 || strings.Contains(body, "words") {
		t.Fatalf("其它模型只回文字: %d %s", st, body)
	}
}

// 口播精剪的字级结果（outcome=talk，SCT-42 第 286 步）：与 api/fixtures/smartcut/asr/volc_talk_fillers.json 的 body
// 逐字一致 —— 后端的 fixture 测试证明解析器收得下它，这里证明假上游回的就是那一份。
func TestVolcTalk_与fixture一致(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "api", "fixtures", "smartcut", "asr", "volc_talk_fillers.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	var want, got any
	if err := json.Unmarshal(fx.Body, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(volcTalk), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("volcTalk 与 fixture 的 body 不一致")
	}
	code, _, body := volcOutcome("talk")
	if code != "20000000" || body != volcTalk {
		t.Fatalf("outcome=talk 应回成功与口播结果，实际 %s", code)
	}
}
