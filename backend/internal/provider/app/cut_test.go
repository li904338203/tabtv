package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"
	"github.com/li904338203/tabtv/backend/pkg/h264"
)

// docs/agent/smartcut SCT-32：输入区间截取（SCD-34）。

func needFFmpeg(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip("本机没有 " + bin)
		}
	}
}

// sourceVideo 生成一段带声音的测试视频（25fps、有音轨），返回字节。
func sourceVideo(t *testing.T, seconds int) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "src.mp4")
	out, err := exec.Command("ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc=size=160x120:rate=25:duration=%d", seconds),
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:duration=%d", seconds),
		"-c:v", "mpeg4", "-q:v", "2", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", path).CombinedOutput()
	if err != nil {
		t.Fatalf("生成测试视频失败: %v %s", err, out)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// probeSeconds 用 ffprobe 量一段媒体的时长（秒）与画面帧数。
func probeSeconds(t *testing.T, data []byte) (float64, int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "probe.mp4")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of",
		"default=noprint_wrappers=1:nokey=1", path).Output()
	if err != nil {
		t.Fatalf("ffprobe 失败: %v", err)
	}
	secs, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		t.Fatalf("解析时长失败: %q", out)
	}
	frames, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-count_frames",
		"-show_entries", "stream=nb_read_frames", "-of", "default=noprint_wrappers=1:nokey=1", path).Output()
	if err != nil {
		t.Fatalf("ffprobe 数帧失败: %v", err)
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(frames)))
	return secs, n
}

// openMedia 额外实现 MediaOpener（流式打开整段素材），并记下开过几次。
type openMedia struct {
	stubMedia
	src    []byte
	mu     sync.Mutex
	opened int
}

func (m *openMedia) Open(context.Context, int64) (io.ReadCloser, string, error) {
	m.mu.Lock()
	m.opened++
	m.mu.Unlock()
	return io.NopCloser(bytes.NewReader(m.src)), "video/mp4", nil
}

// memScratch 是对象存储替身：PresignGet 给出一个上游够得着的地址，记下放了什么、删了什么。
type memScratch struct {
	base    string
	mu      sync.Mutex
	objects map[string][]byte
	deleted []string
}

func newMemScratch(base string) *memScratch {
	return &memScratch{base: base, objects: map[string][]byte{}}
}

func (s *memScratch) PutFile(_ context.Context, key string, body io.ReadSeeker, _ int64, _ string) error {
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = b
	return nil
}
func (s *memScratch) PresignGet(_ context.Context, key string) (string, error) {
	return s.base + "/" + key + "?X-Amz-Signature=abc", nil
}
func (s *memScratch) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, key)
	s.deleted = append(s.deleted, key)
	return nil
}
func (s *memScratch) snapshot() (map[string][]byte, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	objs := map[string][]byte{}
	for k, v := range s.objects {
		objs[k] = v
	}
	return objs, append([]string(nil), s.deleted...)
}

func TestCutArgs(t *testing.T) {
	video := cutArgs("/tmp/a/src", "/tmp/a/cut.mp4", "video", 2000, 5000, h264.Encoder{})
	want := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		// -ss 在 -i 之前 + 重新编码 = 精确到帧；-t 是区间长度不是终点。
		"-ss", "2.000", "-i", "/tmp/a/src", "-t", "3.000",
		"-map", "0:v:0", "-map", "0:a:0?", "-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "18", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "128k", "-movflags", "+faststart", "/tmp/a/cut.mp4"}
	if !reflect.DeepEqual(video, want) {
		t.Fatalf("视频截取参数:\n got %q\nwant %q", video, want)
	}
	audio := cutArgs("/tmp/a/src", "/tmp/a/cut.mp3", "audio", 1250, 61005, h264.Encoder{})
	wantA := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-ss", "1.250", "-i", "/tmp/a/src", "-t", "59.755",
		"-map", "0:a:0", "-vn", "-c:a", "libmp3lame", "-b:a", "192k", "/tmp/a/cut.mp3"}
	if !reflect.DeepEqual(audio, wantA) {
		t.Fatalf("音频截取参数:\n got %q\nwant %q", audio, wantA)
	}
	if got := scratchKey(7001, resolvedInput{Order: 2, MediaKind: "audio", RangeMs: []int{0, 3000}}); got !=
		"scratch/range/7001/2-0-3000.mp3" {
		t.Fatalf("临时对象的键: %q", got)
	}
}

// 60 秒素材取 [2000, 5000]：内联发出去的是 3 秒（误差不超过一帧）、75 帧；时长报区间长度；临时目录删干净。
func TestComposeInputs_区间截出那一段内联发出(t *testing.T) {
	needFFmpeg(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	src := sourceVideo(t, 60)
	p, _ := newProvider(t, RunConfig{ModelCode: "m-1"})
	media := &openMedia{src: src}
	p.SetMediaResolver(media)
	inputs := []resolvedInput{{Kind: "reference", Order: 1, MediaID: 31, MediaKind: "video", RangeMs: []int{2000, 5000}}}

	_, got, err := p.composeInputs(context.Background(), RunConfig{}, 7001, map[string]any{}, inputs, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].DurationMs != 3000 || got[0].MIME != "video/mp4" {
		t.Fatalf("媒体项不对: %+v", got)
	}
	prefix := "data:video/mp4;base64,"
	if !strings.HasPrefix(got[0].Value, prefix) {
		t.Fatalf("没有签名存储时应内联: %.40s", got[0].Value)
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(got[0].Value, prefix))
	if err != nil {
		t.Fatal(err)
	}
	secs, frames := probeSeconds(t, data)
	if secs < 3.0-0.04 || secs > 3.0+0.04 || frames != 75 {
		t.Fatalf("截出来应是 3 秒 75 帧（误差不超过一帧），实际 %.3f 秒 %d 帧", secs, frames)
	}
	if media.opened != 1 {
		t.Fatalf("整段素材应流式打开一次（不走 Inline 读进内存），实际 %d 次", media.opened)
	}
	left, _ := filepath.Glob(filepath.Join(tmp, "seeaitv-cut-*"))
	if len(left) != 0 {
		t.Fatalf("临时目录要删掉: %v", left)
	}
}

// 只收地址的异步协议（去字幕）：截好放进对象存储、发签名地址；受理之后留着（上游稍后来取），
// 查询到终态才删。
func TestSubmit_区间片段放进存储并在查询终态后删除(t *testing.T) {
	needFFmpeg(t)
	var submitted map[string]any
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/tools/erase-video-subtitle-pro":
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &submitted)
			fmt.Fprint(w, `{"task_id":"t1"}`)
		case r.URL.Path == "/api/v1/tasks/t1":
			fmt.Fprintf(w, `{"status":"completed","result":{"video_url":%q}}`, srv.URL+"/out.mp4")
		case r.URL.Path == "/out.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("MP4"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	p, _ := newProvider(t, eraseConfig(srv.URL))
	p.SetMediaResolver(&openMedia{src: sourceVideo(t, 60)})
	store := newMemScratch("https://scratch.example.com")
	p.SetScratch(store, nil)
	job := eraseJob(map[string]any{},
		[]map[string]any{{"kind": "reference", "order": 1, "mediaId": 31, "mediaKind": "video", "rangeMs": []int{2000, 5000}}})

	res, err := p.Submit(context.Background(), job)
	if err != nil || res.Outcome != gendomain.SubmitAccepted {
		t.Fatalf("应受理: %+v %v", res, err)
	}
	key := "scratch/range/7001/1-2000-5000.mp4"
	objs, deleted := store.snapshot()
	if len(objs) != 1 || objs[key] == nil || len(deleted) != 0 {
		t.Fatalf("受理之后片段要留着给上游取: objs=%d deleted=%v", len(objs), deleted)
	}
	if secs, _ := probeSeconds(t, objs[key]); secs < 2.96 || secs > 3.04 {
		t.Fatalf("放进存储的应是 3 秒的片段，实际 %.3f", secs)
	}
	if u, _ := submitted["video_url"].(string); !strings.HasPrefix(u, "https://scratch.example.com/"+key) {
		t.Fatalf("发给上游的应是片段的签名地址: %+v", submitted)
	}

	q, err := p.Query(context.Background(), job, "t1")
	if err != nil || !q.Done || !q.Success {
		t.Fatalf("应成功: %+v %v", q, err)
	}
	_ = readAll(t, q.Outputs[0])
	objs, deleted = store.snapshot()
	if len(objs) != 0 || len(deleted) != 1 || deleted[0] != key {
		t.Fatalf("到终态之后要删掉片段: objs=%d deleted=%v", len(objs), deleted)
	}
}

// 提交没被受理（上游回 4xx）：片段当场删掉，不留到清理作业。
func TestSubmit_区间片段在提交失败后立即删除(t *testing.T) {
	needFFmpeg(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"bad"}}`)
	}))
	defer srv.Close()
	p, _ := newProvider(t, eraseConfig(srv.URL))
	p.SetMediaResolver(&openMedia{src: sourceVideo(t, 10)})
	store := newMemScratch("https://scratch.example.com")
	p.SetScratch(store, nil)
	job := eraseJob(map[string]any{},
		[]map[string]any{{"kind": "reference", "order": 1, "mediaId": 31, "mediaKind": "video", "rangeMs": []int{0, 1000}}})
	res, _ := p.Submit(context.Background(), job)
	if res.Outcome == gendomain.SubmitAccepted {
		t.Fatalf("上游回 400 不该受理: %+v", res)
	}
	objs, deleted := store.snapshot()
	if len(objs) != 0 || len(deleted) != 1 {
		t.Fatalf("没被受理就当场删: objs=%d deleted=%v", len(objs), deleted)
	}
}

// 只收地址、而主存储签出的是内网地址且没有中转：不截、不发，判永久失败并说清怎么配。
func TestSubmit_区间片段上游够不着时说清原因(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer srv.Close()
	p, _ := newProvider(t, eraseConfig(srv.URL))
	media := &openMedia{src: []byte("never-read")}
	p.SetMediaResolver(media)
	store := newMemScratch("http://127.0.0.1:9000/bucket")
	p.SetScratch(store, nil)
	job := eraseJob(map[string]any{},
		[]map[string]any{{"kind": "reference", "order": 1, "mediaId": 31, "mediaKind": "video", "rangeMs": []int{0, 1000}}})
	res, _ := p.Submit(context.Background(), job)
	if hit || res.Outcome != gendomain.SubmitRejected || !strings.Contains(res.Message, "SEEAITV_RELAY_STORAGE_") {
		t.Fatalf("应在发请求前判永久失败并说清怎么配: hit=%v %+v", hit, res)
	}
	if media.opened != 0 {
		t.Fatal("够不着就不该白截一次")
	}
}

type fakeLister struct {
	keys    []string
	prefix  string
	before  time.Time
	deleted []string
}

func (f *fakeLister) ListOlderThan(_ context.Context, prefix string, before time.Time, limit int) ([]string, error) {
	f.prefix, f.before = prefix, before
	n := limit
	if n > len(f.keys) {
		n = len(f.keys)
	}
	out := f.keys[:n]
	f.keys = f.keys[n:]
	return out, nil
}
func (f *fakeLister) Delete(_ context.Context, key string) error {
	f.deleted = append(f.deleted, key)
	return nil
}

func TestScratchSweeper_只扫截取前缀并删掉过期的(t *testing.T) {
	keys := make([]string, 0, 600)
	for i := 0; i < 600; i++ {
		keys = append(keys, fmt.Sprintf("%s%d/1-0-1000.mp4", ScratchPrefix, i))
	}
	l := &fakeLister{keys: keys}
	s := NewScratchSweeper(l, nil)
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	n, err := s.SweepOnce(context.Background())
	if err != nil || n != 600 || len(l.deleted) != 600 {
		t.Fatalf("应分批删完 600 个: n=%d err=%v", n, err)
	}
	if l.prefix != ScratchPrefix || !l.before.Equal(now.Add(-24*time.Hour)) {
		t.Fatalf("只扫截取前缀、只删 24 小时之前的: %q %v", l.prefix, l.before)
	}
}
