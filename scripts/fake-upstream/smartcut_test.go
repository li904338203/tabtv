package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newClipGenServer(t *testing.T) (*server, *httptest.Server) {
	t.Helper()
	s := &server{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/tools/erase-video-subtitle-pro", s.mediaKitErase)
	mux.HandleFunc("/api/v1/tasks/", s.mediaKitTask)
	mux.HandleFunc("/_probe/smartcut/media", s.clipGen.handleProbe)
	mux.HandleFunc("/_probe/smartcut/media/next", s.clipGen.handleNext)
	srv := httptest.NewServer(mux)
	s.addr = srv.URL
	t.Cleanup(srv.Close)
	return s, srv
}

func eraseCall(t *testing.T, method, url string, body any, bearer bool) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, rd)
	if bearer {
		req.Header.Set("Authorization", "Bearer mk-key")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// 去字幕（SCT-38）：提交 → 查询两次（处理中 → 完成，结果在 result.video_url）；收到的地址记在探针里。
func TestMediaKitErase_提交查询两次完成(t *testing.T) {
	_, srv := newClipGenServer(t)
	in := "https://relay.example.com/scratch/range/7001/1-2000-5000.mp4?sig=1"
	code, sub := eraseCall(t, http.MethodPost, srv.URL+"/api/v1/tools/erase-video-subtitle-pro",
		map[string]any{"video_url": in, "mode": "subtitle"}, true)
	id, _ := sub["task_id"].(string)
	if code != 200 || id == "" {
		t.Fatalf("提交应回任务 ID: %d %v", code, sub)
	}
	if _, q := eraseCall(t, http.MethodGet, srv.URL+"/api/v1/tasks/"+id, nil, true); q["status"] != "processing" {
		t.Fatalf("第一次查询处理中: %v", q)
	}
	_, q := eraseCall(t, http.MethodGet, srv.URL+"/api/v1/tasks/"+id, nil, true)
	result, _ := q["result"].(map[string]any)
	if q["status"] != "completed" || !strings.HasSuffix(result["video_url"].(string), "/files/"+id+".mp4") || q["video_url"] != in {
		t.Fatalf("第二次查询完成、结果在 result.video_url、根上回显输入: %v", q)
	}
	_, probe := eraseCall(t, http.MethodGet, srv.URL+"/_probe/smartcut/media", nil, false)
	items, _ := probe["items"].([]any)
	first, _ := items[0].(map[string]any)
	if len(items) != 3 || first["videoUrl"] != in || first["mode"] != "subtitle" || first["bearer"] != true {
		t.Fatalf("探针记下收到的地址: %v", probe)
	}
}

func TestMediaKitErase_形状不对回错误(t *testing.T) {
	_, srv := newClipGenServer(t)
	url := srv.URL + "/api/v1/tools/erase-video-subtitle-pro"
	if code, _ := eraseCall(t, http.MethodPost, url, map[string]any{"video_url": "https://a/b.mp4"}, false); code != 401 {
		t.Fatalf("没有 Bearer 回 401: %d", code)
	}
	if code, out := eraseCall(t, http.MethodPost, url, map[string]any{"video_url": "data:video/mp4;base64,AAAA"}, true); code != 400 ||
		!strings.Contains(out["error"].(map[string]any)["message"].(string), "不收内联") {
		t.Fatalf("内联数据回 400: %d %v", code, out)
	}
	if code, _ := eraseCall(t, http.MethodPost, url, map[string]any{"video_url": "https://a/b.mp4", "mode": "all"}, true); code != 400 {
		t.Fatalf("mode 不对回 400: %d", code)
	}
	if code, _ := eraseCall(t, http.MethodGet, srv.URL+"/api/v1/tasks/erase-404", nil, true); code != 404 {
		t.Fatalf("不认识的任务回 404: %d", code)
	}
}

func TestMediaKitErase_探针设失败(t *testing.T) {
	_, srv := newClipGenServer(t)
	_, sub := eraseCall(t, http.MethodPost, srv.URL+"/api/v1/tools/erase-video-subtitle-pro",
		map[string]any{"video_url": "https://a/b.mp4"}, true)
	id := sub["task_id"].(string)
	if code, _ := eraseCall(t, http.MethodPost, srv.URL+"/_probe/smartcut/media/next?outcome=failed", nil, false); code != 200 {
		t.Fatal("设下一次失败")
	}
	eraseCall(t, http.MethodGet, srv.URL+"/api/v1/tasks/"+id, nil, true)
	_, q := eraseCall(t, http.MethodGet, srv.URL+"/api/v1/tasks/"+id, nil, true)
	if q["status"] != "failed" || q["error"] == nil {
		t.Fatalf("探针设的失败: %v", q)
	}
}

func newAudioGenServer(t *testing.T) (*server, *httptest.Server) {
	t.Helper()
	s := &server{}
	mux := http.NewServeMux()
	mux.HandleFunc("/volc/", s.volcMusic)
	mux.HandleFunc("/v1/music", s.elevenLabsMusic)
	mux.HandleFunc("/v1/audio/video-to-audio", s.klingV2A)
	mux.HandleFunc("/v1/audio/video-to-audio/", s.klingV2A)
	mux.HandleFunc("/_probe/smartcut/media", s.clipGen.handleProbe)
	mux.HandleFunc("/_probe/smartcut/media/next", s.clipGen.handleNext)
	mux.HandleFunc("/files/", s.download)
	srv := httptest.NewServer(mux)
	s.addr = srv.URL
	s.lastDownloadAuth.Store("")
	t.Cleanup(srv.Close)
	return s, srv
}

// volcPostSigned 按 V4 的形状带上签名头（签名值本身假上游不重算）。
func volcPostSigned(t *testing.T, url string, body any, signed bool) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if signed {
		sum := sha256.Sum256(b)
		req.Header.Set("X-Date", "20261028T120000Z")
		req.Header.Set("X-Content-Sha256", hex.EncodeToString(sum[:]))
		req.Header.Set("Authorization", "HMAC-SHA256 Credential=AK/20261028/cn-beijing/imagination/request, "+
			"SignedHeaders=content-type;host;x-content-sha256;x-date, Signature=abc")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// 豆包音乐（SCT-39）：签名头齐全才收；GenBGM → 查询两次（处理中 → 完成，SongDetail.AudioUrl）；结果是一段真的 MP3。
func TestVolcMusic_签名头与查询两次(t *testing.T) {
	_, srv := newAudioGenServer(t)
	gen := srv.URL + "/volc/?Action=GenBGM&Version=2024-08-12"
	if code, _ := volcPostSigned(t, gen, map[string]any{"Text": "钢琴", "Duration": 30}, false); code != 401 {
		t.Fatalf("没有签名头回 401: %d", code)
	}
	if code, _ := volcPostSigned(t, gen, map[string]any{"Text": "钢琴", "Duration": 90}, true); code != 400 {
		t.Fatalf("超过 60 秒回 400: %d", code)
	}
	code, sub := volcPostSigned(t, gen, map[string]any{"Text": "钢琴", "Duration": 30}, true)
	id, _ := sub["Result"].(map[string]any)["TaskID"].(string)
	if code != 200 || id == "" {
		t.Fatalf("提交取任务 ID: %d %v", code, sub)
	}
	query := srv.URL + "/volc/?Action=QuerySong&Version=2024-08-12"
	if _, q := volcPostSigned(t, query, map[string]any{"TaskID": id}, true); q["Result"].(map[string]any)["Status"] != float64(1) {
		t.Fatalf("第一次查询处理中: %v", q)
	}
	_, q := volcPostSigned(t, query, map[string]any{"TaskID": id}, true)
	res := q["Result"].(map[string]any)
	u, _ := res["SongDetail"].(map[string]any)["AudioUrl"].(string)
	if res["Status"] != float64(2) || !strings.HasSuffix(u, "/files/"+id+".mp3") {
		t.Fatalf("第二次查询完成: %v", q)
	}
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.Header.Get("Content-Type") != "audio/mpeg" || !bytes.Equal(b, decodeB64(twoSecondMP3)) {
		t.Fatalf("结果是真的 MP3: %s %d 字节", resp.Header.Get("Content-Type"), len(b))
	}
}

// ElevenLabs（SCT-39）：要 xi-api-key 与 mp3 的 output_format；直接回 MP3 字节。
func TestElevenLabsMusic_回MP3字节(t *testing.T) {
	_, srv := newAudioGenServer(t)
	post := func(query string, key bool, body any) (*http.Response, []byte) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/music"+query, bytes.NewReader(b))
		if key {
			req.Header.Set("xi-api-key", "k")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp, out
	}
	ok := map[string]any{"prompt": "史诗弦乐", "music_length_ms": 30000, "force_instrumental": true}
	if resp, _ := post("?output_format=mp3_44100_128", false, ok); resp.StatusCode != 401 {
		t.Fatalf("没有 xi-api-key 回 401: %d", resp.StatusCode)
	}
	if resp, _ := post("", true, ok); resp.StatusCode != 422 {
		t.Fatalf("没显式给 mp3 回 422: %d", resp.StatusCode)
	}
	resp, b := post("?output_format=mp3_44100_128", true, ok)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "audio/mpeg" || !bytes.Equal(b, decodeB64(twoSecondMP3)) {
		t.Fatalf("回 MP3 字节: %d %s %d", resp.StatusCode, resp.Header.Get("Content-Type"), len(b))
	}
}

// 可灵视频配音效（SCT-39）：提交 → 查询两次（processing → succeed，取 audios[0].url_mp3）；只收 http(s) 地址。
func TestKlingV2A_提交查询两次完成(t *testing.T) {
	_, srv := newAudioGenServer(t)
	url := srv.URL + "/v1/audio/video-to-audio"
	if code, out := eraseCall(t, http.MethodPost, url, map[string]any{"video_url": "data:video/mp4;base64,AAAA"}, true); code != 400 ||
		out["code"] != float64(1201) {
		t.Fatalf("内联数据回 400 / 1201: %d %v", code, out)
	}
	code, sub := eraseCall(t, http.MethodPost, url, map[string]any{"video_url": "https://relay.example.com/1-0-8000.mp4",
		"sound_effect_prompt": "脚步声"}, true)
	id, _ := sub["data"].(map[string]any)["task_id"].(string)
	if code != 200 || sub["code"] != float64(0) || id == "" {
		t.Fatalf("提交取任务 ID: %d %v", code, sub)
	}
	if _, q := eraseCall(t, http.MethodGet, url+"/"+id, nil, true); q["data"].(map[string]any)["task_status"] != "processing" {
		t.Fatalf("第一次查询处理中: %v", q)
	}
	_, q := eraseCall(t, http.MethodGet, url+"/"+id, nil, true)
	data := q["data"].(map[string]any)
	audios, _ := data["task_result"].(map[string]any)["audios"].([]any)
	if data["task_status"] != "succeed" || len(audios) != 1 ||
		!strings.HasSuffix(audios[0].(map[string]any)["url_mp3"].(string), "/files/"+id+".mp3") {
		t.Fatalf("第二次查询完成、音频在 audios[0].url_mp3: %v", q)
	}
	_, probe := eraseCall(t, http.MethodGet, srv.URL+"/_probe/smartcut/media", nil, false)
	items, _ := probe["items"].([]any)
	if first := items[1].(map[string]any); first["videoUrl"] != "https://relay.example.com/1-0-8000.mp4" || first["prompt"] != "脚步声" {
		t.Fatalf("探针记下收到的地址与描述: %v", probe)
	}
}
