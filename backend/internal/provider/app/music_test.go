package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"
	"github.com/li904338203/tabtv/backend/internal/provider/runcfg"
	"github.com/li904338203/tabtv/backend/internal/provider/transport"
)

// docs/agent/smartcut SCT-39：配乐与按画面配音效的上游往返（httptest 按各家形状回）。

// sineMP3 一段真的 MP3（ffprobe 量得出时长）。
func sineMP3(t *testing.T, seconds int) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sine.mp3")
	out, err := exec.Command("ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", fmt.Sprintf("sine=frequency=330:duration=%d", seconds), "-c:a", "libmp3lame", "-b:a", "64k", path).CombinedOutput()
	if err != nil {
		t.Fatalf("生成测试音频失败: %v %s", err, out)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// tosServer 结果所在的另一个域名（对象存储）：回固定字节，记下有没有带认证头。
func tosServer(t *testing.T, data []byte, mime string) (*httptest.Server, func() string) {
	t.Helper()
	var mu sync.Mutex
	auth := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = r.Header.Get("Authorization") + r.Header.Get("Xi-Api-Key")
		mu.Unlock()
		w.Header().Set("Content-Type", mime)
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv, func() string { mu.Lock(); defer mu.Unlock(); return auth }
}

// 豆包音乐：提交与查询都是 Action 风格的 POST，每一次都按收到的字节重算 V4 签名（含查询串里的 Action）；
// 查到完成后从另一个域名取回音频（不带我们的密钥）。
func TestVolcMusic_提交带V4签名头(t *testing.T) {
	cred := transport.VolcCredential{AccessKey: "AKmusic", SecretKey: "SKmusic", Region: "cn-beijing", Service: "imagination"}
	mp3 := []byte("ID3-fake-bgm")
	tos, tosAuth := tosServer(t, mp3, "audio/mpeg")
	var mu sync.Mutex
	var actions []string
	var submitted map[string]any
	polls := 0
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		at, err := time.Parse("20060102T150405Z", r.Header.Get("X-Date"))
		want, serr := transport.SignVolcV4(r.Method, srv.URL+r.URL.RequestURI(),
			map[string]string{"Content-Type": r.Header.Get("Content-Type")}, body, cred, at)
		if err != nil || serr != nil || want["Authorization"] != r.Header.Get("Authorization") ||
			r.Header.Get("X-Content-Sha256") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"ResponseMetadata":{"Error":{"Code":"SignatureDoesNotMatch","Message":"bad signature"}}}`)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		action := r.URL.Query().Get("Action")
		actions = append(actions, action)
		switch action {
		case "GenBGM":
			_ = json.Unmarshal(body, &submitted)
			fmt.Fprint(w, `{"ResponseMetadata":{"Action":"GenBGM"},"Result":{"TaskID":"bgm-1"}}`)
		case "QuerySong":
			polls++
			if polls == 1 {
				fmt.Fprint(w, `{"Result":{"TaskID":"bgm-1","Status":1}}`)
				return
			}
			fmt.Fprintf(w, `{"Result":{"TaskID":"bgm-1","Status":2,"SongDetail":{"AudioUrl":%q}}}`, tos.URL+"/bgm-1.mp3")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	p, _ := newProvider(t, RunConfig{ModelCode: "bgm-m", ProtocolCode: "volc_music", BaseURL: srv.URL,
		SubmitPath: "/?Action=GenBGM&Version=2024-08-12", AuthType: runcfg.AuthVolcV4, APIKey: "AKmusic:SKmusic",
		SignRegion: "cn-beijing", SignService: "imagination"})
	job := jobWith("volc_music", map[string]any{"prompt": "轻快的钢琴", "duration": 30, "instrumental": true}, nil)
	job.OperationCode, job.ResultType = "text2music", "audio"
	res, err := p.Submit(context.Background(), job)
	if err != nil || res.Outcome != gendomain.SubmitAccepted || res.ProviderTaskID != "bgm-1" {
		t.Fatalf("签名对上、受理: %+v %v", res, err)
	}
	if submitted["Text"] != "轻快的钢琴" || submitted["Duration"] != float64(30) {
		t.Fatalf("提交体: %+v", submitted)
	}
	if q, err := p.Query(context.Background(), job, "bgm-1"); err != nil || q.Done {
		t.Fatalf("第一次查询处理中: %+v %v", q, err)
	}
	q, err := p.Query(context.Background(), job, "bgm-1")
	if err != nil || !q.Done || !q.Success || len(q.Outputs) != 1 {
		t.Fatalf("第二次查询完成: %+v %v", q, err)
	}
	if got := readAll(t, q.Outputs[0]); string(got) != string(mp3) || tosAuth() != "" {
		t.Fatalf("从结果地址取回、不带密钥: %q %q", got, tosAuth())
	}
	if strings.Join(actions, ",") != "GenBGM,QuerySong,QuerySong" {
		t.Fatalf("三次请求都签过名: %v", actions)
	}
}

// ElevenLabs：同步返回音频字节，按 audio/mpeg 归档（字节是真的 MP3，量得出 3 秒）；认证头 xi-api-key；
// 提交地址补上 output_format=mp3。
func TestElevenLabsMusic_字节按音频归档(t *testing.T) {
	needFFmpeg(t)
	mp3 := sineMP3(t, 3)
	var key, format string
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, format = r.Header.Get("xi-api-key"), r.URL.Query().Get("output_format")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &sent)
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("song-id", "song-1")
		_, _ = w.Write(mp3)
	}))
	defer srv.Close()
	p, _ := newProvider(t, RunConfig{ModelCode: "el-m", ProtocolCode: "elevenlabs_music", BaseURL: srv.URL,
		SubmitPath: "/v1/music", AuthType: "header", HeaderName: "xi-api-key", APIKey: "el-key", UpstreamModel: "music_v1"})
	job := jobWith("elevenlabs_music", map[string]any{"prompt": "史诗感的弦乐", "duration": 3}, nil)
	job.OperationCode, job.ResultType = "text2music", "audio"
	res, err := p.Submit(context.Background(), job)
	if err != nil || res.Outcome != gendomain.SubmitCompleted || len(res.Outputs) != 1 {
		t.Fatalf("同步完成: %+v %v", res, err)
	}
	if key != "el-key" || format != "mp3_44100_128" || sent["music_length_ms"] != float64(3000) || sent["force_instrumental"] != true {
		t.Fatalf("认证头 / 输出格式 / 请求体: %q %q %+v", key, format, sent)
	}
	out := res.Outputs[0]
	if !strings.HasPrefix(out.MIME, "audio/mpeg") {
		t.Fatalf("按音频归档: %q", out.MIME)
	}
	if secs, _ := probeAudioSeconds(t, readAll(t, out)); secs < 2.9 || secs > 3.2 {
		t.Fatalf("归档的字节量得出时长（3 秒），实际 %.3f", secs)
	}
}

// probeAudioSeconds 用 ffprobe 量一段音频的时长（秒）。
func probeAudioSeconds(t *testing.T, data []byte) (float64, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "probe.mp3")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of",
		"default=noprint_wrappers=1:nokey=1", path).Output()
	if err != nil {
		t.Fatalf("ffprobe 失败: %v", err)
	}
	var secs float64
	_, err = fmt.Sscanf(strings.TrimSpace(string(out)), "%f", &secs)
	return secs, err
}

// 可灵视频配音效：截出片段用到的 8 秒放进存储、发签名地址（Bearer）；查询两次后完成，取 audios[0].url_mp3。
func TestKlingV2A_区间片段发出并取回音频(t *testing.T) {
	needFFmpeg(t)
	mp3 := []byte("ID3-fake-sfx")
	tos, tosAuth := tosServer(t, mp3, "audio/mpeg")
	var mu sync.Mutex
	var auth string
	var sent map[string]any
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/audio/video-to-audio":
			auth = r.Header.Get("Authorization")
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &sent)
			fmt.Fprint(w, `{"code":0,"message":"SUCCEED","data":{"task_id":"v2a-1","task_status":"submitted"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/audio/video-to-audio/v2a-1":
			polls++
			if polls == 1 {
				fmt.Fprint(w, `{"code":0,"data":{"task_id":"v2a-1","task_status":"processing"}}`)
				return
			}
			fmt.Fprintf(w, `{"code":0,"data":{"task_id":"v2a-1","task_status":"succeed","task_result":{
				"videos":[{"url":%q}],"audios":[{"url_mp3":%q,"url_wav":%q}]}}}`,
				tos.URL+"/v.mp4", tos.URL+"/a.mp3", tos.URL+"/a.wav")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	p, _ := newProvider(t, RunConfig{ModelCode: "v2a-m", ProtocolCode: "kling_v2a", BaseURL: srv.URL,
		SubmitPath: "/v1/audio/video-to-audio", AuthType: "bearer", APIKey: "kling-key"})
	p.SetMediaResolver(&openMedia{src: sourceVideo(t, 10)})
	store := newMemScratch("https://scratch.example.com")
	p.SetScratch(store, nil)
	job := jobWith("kling_v2a", map[string]any{"prompt": "脚步声"},
		[]map[string]any{{"kind": "reference", "order": 1, "mediaId": 31, "mediaKind": "video", "rangeMs": []int{1000, 9000}}})
	job.OperationCode, job.ResultType = "video2audio", "audio"
	res, err := p.Submit(context.Background(), job)
	if err != nil || res.Outcome != gendomain.SubmitAccepted || res.ProviderTaskID != "v2a-1" {
		t.Fatalf("受理: %+v %v", res, err)
	}
	key := "scratch/range/7001/1-1000-9000.mp4"
	objs, _ := store.snapshot()
	if secs, _ := probeSeconds(t, objs[key]); secs < 7.96 || secs > 8.04 {
		t.Fatalf("截出来的是 8 秒，实际 %.3f", secs)
	}
	mu.Lock()
	if auth != "Bearer kling-key" || sent["video_url"] != "https://scratch.example.com/"+key+"?X-Amz-Signature=abc" ||
		sent["sound_effect_prompt"] != "脚步声" {
		t.Fatalf("认证与请求体: %q %+v", auth, sent)
	}
	mu.Unlock()
	if q, err := p.Query(context.Background(), job, "v2a-1"); err != nil || q.Done {
		t.Fatalf("第一次查询处理中: %+v %v", q, err)
	}
	q, err := p.Query(context.Background(), job, "v2a-1")
	if err != nil || !q.Done || !q.Success || len(q.Outputs) != 1 {
		t.Fatalf("完成: %+v %v", q, err)
	}
	if got := readAll(t, q.Outputs[0]); string(got) != string(mp3) || tosAuth() != "" {
		t.Fatalf("取回 url_mp3、不带密钥: %q %q", got, tosAuth())
	}
}
