package protocol

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"
)

// docs/agent/smartcut SCT-39：配乐（火山豆包音乐、ElevenLabs Music）与按画面配音效（可灵视频配音效）。
//
// ⚠️ 可灵与 ElevenLabs 的 fixture 照 2026-10-28 核对过的官方文档写；豆包音乐的照 dev/01 §7（待核实，SCR-7）。

func audioFixture(t *testing.T, dir, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func bodyOf(t *testing.T, req Request) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(req.Body, &m); err != nil {
		t.Fatalf("请求体不是 JSON: %s", req.Body)
	}
	return m
}

// classified 提交失败时分类用的状态码（StatusError 给的，或者就是 HTTP 码）与可重试性。
func classified(t *testing.T, err error, httpStatus int) (int, bool) {
	t.Helper()
	if err == nil {
		t.Fatal("应失败")
	}
	got := httpStatus
	var se *StatusError
	if errors.As(err, &se) {
		got = se.Status
	}
	return got, gendomain.ClassifyHTTP(got) == gendomain.RetryTransient
}

func musicJob(params map[string]any) Job {
	return Job{OperationCode: "text2music", ResultType: "audio", IdempotencyKey: "music-key-1", Prompt: "轻快的钢琴，适合旅行 vlog",
		Params: params}
}

// ---------------------------------------------------------------- 火山豆包音乐

func TestVolcMusic_请求体与时长(t *testing.T) {
	p := mustGet(t, "volc_music")
	req, err := p.BuildSubmit(musicJob(map[string]any{"duration": 45, "instrumental": true}))
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != "POST" || req.Path != "/?Action=GenBGM&Version=2024-08-12" {
		t.Fatalf("提交地址: %s %s", req.Method, req.Path)
	}
	if b := bodyOf(t, req); b["Text"] != "轻快的钢琴，适合旅行 vlog" || b["Duration"] != float64(45) || len(b) != 2 {
		t.Fatalf("请求体: %s", req.Body)
	}
	for _, c := range []struct {
		name   string
		sec    int
		params map[string]any
		want   string
	}{
		{"超过 60 秒", 90, nil, "30~60 秒"},
		{"不足 30 秒", 10, nil, "30~60 秒"},
		{"明确要带人声", 45, map[string]any{"instrumental": false}, "纯音乐"},
	} {
		if err := CheckParamsOf("volc_music", "text2music", c.sec, c.params); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s：报价阶段要拒并说清: %v", c.name, err)
		}
	}
	if err := CheckParamsOf("volc_music", "text2music", 30, map[string]any{"instrumental": true}); err != nil {
		t.Fatalf("30 秒纯音乐放行: %v", err)
	}
	if _, err := p.BuildSubmit(musicJob(map[string]any{"duration": 90})); err == nil {
		t.Fatal("Worker 侧同一份规则再拦一次")
	}
	res, err := p.ParseSubmit(200, audioFixture(t, "volc_music", "submit_ok.json"))
	if err != nil || !res.Async || res.TaskID != "7431928470212345678" {
		t.Fatalf("取任务 ID: %+v %v", res, err)
	}
	q, ok := p.BuildQuery(Job{}, "7431928470212345678")
	if !ok || q.Method != "POST" || q.Path != "/?Action=QuerySong&Version=2024-08-12" || bodyOf(t, q)["TaskID"] != "7431928470212345678" {
		t.Fatalf("查询请求: %+v %s", q, q.Body)
	}
}

func TestVolcMusic_查询完成取AudioUrl(t *testing.T) {
	p := mustGet(t, "volc_music")
	if res, err := p.ParseQuery(200, audioFixture(t, "volc_music", "query_processing.json")); err != nil || res.State != StatePending {
		t.Fatalf("处理中: %+v %v", res, err)
	}
	res, err := p.ParseQuery(200, audioFixture(t, "volc_music", "query_completed.json"))
	if err != nil || res.State != StateSuccess || len(res.Outputs) != 1 ||
		!strings.HasPrefix(res.Outputs[0].Value, "https://imagination-music.") || res.Outputs[0].MIME != "audio/mpeg" {
		t.Fatalf("完成取 SongDetail.AudioUrl: %+v %v", res, err)
	}
	res, _ = p.ParseQuery(200, audioFixture(t, "volc_music", "query_failed.json"))
	if res.State != StateFailed || !strings.Contains(res.Message, "sensitive content") {
		t.Fatalf("失败带原因: %+v", res)
	}
	// 状态值待核实：字符串也认；完成却没有地址判失败并说清缺什么。
	if res, _ := p.ParseQuery(200, []byte(`{"Result":{"Status":"success","AudioUrl":"https://a/b.mp3"}}`)); res.State != StateSuccess ||
		res.Outputs[0].Value != "https://a/b.mp3" {
		t.Fatalf("字符串状态与 Result 上的地址: %+v", res)
	}
	if res, _ := p.ParseQuery(200, []byte(`{"Result":{"Status":2}}`)); res.State != StateFailed || !strings.Contains(res.Message, "AudioUrl") {
		t.Fatalf("完成没有地址: %+v", res)
	}
	if res, _ := p.ParseQuery(200, []byte(`{"Result":{"Status":7}}`)); res.State != StatePending {
		t.Fatalf("没见过的状态当处理中: %+v", res)
	}
}

func TestVolcMusic_失败分类表驱动(t *testing.T) {
	p := mustGet(t, "volc_music")
	cases := []struct {
		name      string
		status    int
		body      []byte
		wantHTTP  int
		retryable bool
	}{
		{"签名不对（HTTP 200 里的错误）", 200, audioFixture(t, "volc_music", "error_signature.json"), 400, false},
		{"签名不对（HTTP 401）", 401, audioFixture(t, "volc_music", "error_signature.json"), 400, false},
		{"限流", 429, audioFixture(t, "volc_music", "error_flowlimit.json"), 429, true},
		{"内部错误", 500, audioFixture(t, "volc_music", "error_internal.json"), 503, true},
		{"5xx 且错误码没认出来", 502, []byte(`{"ResponseMetadata":{"Error":{"Code":"Whatever","Message":"x"}}}`), 502, true},
		{"5xx 且不是 JSON", 502, []byte(`bad gateway`), 502, true},
		{"没有 Result", 200, []byte(`{"ResponseMetadata":{}}`), 200, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := p.ParseSubmit(c.status, c.body)
			got, retryable := classified(t, err, c.status)
			if c.name == "没有 Result" {
				// 2xx 却解析不出来：提交侧判永久失败（我们这边的问题）。
				var se *StatusError
				if errors.As(err, &se) {
					t.Fatalf("不该是 StatusError: %v", err)
				}
				return
			}
			if got != c.wantHTTP || retryable != c.retryable {
				t.Fatalf("分类 %d（可重试 %v），期望 %d（%v）：%v", got, retryable, c.wantHTTP, c.retryable, err)
			}
		})
	}
}

// ---------------------------------------------------------------- ElevenLabs Music

func TestElevenLabsMusic_字节按音频输出(t *testing.T) {
	p := mustGet(t, "elevenlabs_music")
	job := musicJob(map[string]any{"duration": 40})
	job.UpstreamModel = "music_v1"
	req, err := p.BuildSubmit(job)
	if err != nil {
		t.Fatal(err)
	}
	b := bodyOf(t, req)
	if b["prompt"] != job.Prompt || b["music_length_ms"] != float64(40000) || b["force_instrumental"] != true || b["model_id"] != "music_v1" {
		t.Fatalf("请求体（缺省纯音乐、毫秒时长）: %s", req.Body)
	}
	req, _ = p.BuildSubmit(musicJob(map[string]any{"duration": 40, "instrumental": false}))
	if bodyOf(t, req)["force_instrumental"] != false {
		t.Fatalf("要带人声时 force_instrumental=false: %s", req.Body)
	}
	if err := CheckParamsOf("elevenlabs_music", "text2music", 301, nil); err == nil {
		t.Fatal("超过 300 秒要拒")
	}
	// output_format 一律补成 mp3：运营配的路径会盖掉 BuildSubmit 给的那条。
	for configured, want := range map[string]string{
		"/v1/music":                             "/v1/music?output_format=mp3_44100_128",
		"/v1/music?output_format=pcm_44100":     "/v1/music?output_format=mp3_44100_128",
		"/v1/music?output_format=mp3_48000_192": "/v1/music?output_format=mp3_48000_192",
		"/v1/music?enable_logging=false":        "/v1/music?enable_logging=false&output_format=mp3_44100_128",
		"":                                      "/v1/music?output_format=mp3_44100_128",
	} {
		got, err := ResolveSubmitPath(p, job, configured)
		if err != nil || got != want {
			t.Fatalf("%q → %q，期望 %q（%v）", configured, got, want, err)
		}
	}

	mp3 := []byte("ID3\x04\x00\x00fake-mp3-frames")
	res, err := p.ParseSubmit(200, mp3)
	if err != nil || len(res.Outputs) != 1 || res.Outputs[0].Kind != "base64" || res.Outputs[0].MIME != "audio/mpeg" {
		t.Fatalf("字节按音频输出: %+v %v", res, err)
	}
	if raw, _ := base64.StdEncoding.DecodeString(res.Outputs[0].Value); string(raw) != string(mp3) {
		t.Fatal("字节原样")
	}
	if _, err := p.ParseSubmit(200, []byte(`{"detail":"oops"}`)); err == nil || !strings.Contains(err.Error(), "JSON") {
		t.Fatalf("200 却是 JSON 不当音频存: %v", err)
	}
	if _, err := p.ParseSubmit(200, nil); err == nil {
		t.Fatal("空正文要报错")
	}
	_, err = p.ParseSubmit(422, audioFixture(t, "elevenlabs_music", "error_422.json"))
	if got, retryable := classified(t, err, 422); got != 422 || retryable || !strings.Contains(err.Error(), "greater than or equal to 3000") {
		t.Fatalf("校验错误不可重试、带原因: %d %v %v", got, retryable, err)
	}
	if _, err := p.ParseSubmit(429, []byte(`{"detail":{"status":"too_many_concurrent_requests"}}`)); err == nil {
		t.Fatal("限流要报错")
	} else if _, retryable := classified(t, err, 429); !retryable {
		t.Fatal("429 可重试")
	}
}

// ---------------------------------------------------------------- 可灵视频配音效

func v2aJob(prompt string, durationMs int, value string) Job {
	return Job{OperationCode: "video2audio", ResultType: "audio", IdempotencyKey: "sfx-key-1", Prompt: prompt,
		Params: map[string]any{}, Media: []Media{{Kind: "video", Order: 1, Value: value, DurationMs: durationMs}}}
}

func TestKlingV2A_只收3到20秒(t *testing.T) {
	p := mustGet(t, "kling_v2a")
	u := "https://relay.example.com/scratch/range/7001/1-0-8000.mp4?sig=1"
	req, err := p.BuildSubmit(v2aJob("脚步声和远处的雷声", 8000, u))
	if err != nil {
		t.Fatal(err)
	}
	b := bodyOf(t, req)
	if req.Path != "/v1/audio/video-to-audio" || b["video_url"] != u || b["sound_effect_prompt"] != "脚步声和远处的雷声" ||
		b["external_task_id"] != "seeaitv-sfx-key-1" {
		t.Fatalf("提交: %s %s", req.Path, req.Body)
	}
	for _, ms := range []int{2600, 20400, 25000} {
		if _, err := p.BuildSubmit(v2aJob("", ms, u)); err == nil || !strings.Contains(err.Error(), "3~20 秒") {
			t.Fatalf("%d 毫秒要拒（按毫秒判，不按取整后的秒）: %v", ms, err)
		}
	}
	if _, err := p.BuildSubmit(v2aJob("", 20000, u)); err != nil {
		t.Fatalf("20.0 秒正好放行: %v", err)
	}
	if err := CheckParamsOf("kling_v2a", "video2audio", 25, nil); err == nil {
		t.Fatal("报价阶段按秒也要拦")
	}
	if err := CheckParamsOf("kling_v2a", "video2audio", 8, map[string]any{"prompt": strings.Repeat("响", 201)}); err == nil {
		t.Fatal("描述超过 200 字要拒")
	}
	if _, err := p.BuildSubmit(v2aJob("", 8000, "data:video/mp4;base64,AAAA")); err == nil {
		t.Fatal("只收地址")
	}
	if !RequiresMediaURL(p) {
		t.Fatal("要声明只收地址：Worker 才会走中转、受理才会在收不到时拒")
	}
	if lo, hi, ok := DurationRangeOf("kling_v2a", "video2audio"); !ok || lo != 3 || hi != 20 {
		t.Fatalf("时长范围: %d~%d %v", lo, hi, ok)
	}
}

func TestKlingV2A_完成取音频地址(t *testing.T) {
	p := mustGet(t, "kling_v2a")
	res, err := p.ParseSubmit(200, audioFixture(t, "kling_v2a", "submit_ok.json"))
	if err != nil || !res.Async || res.TaskID != "860001234567890123" {
		t.Fatalf("取任务 ID: %+v %v", res, err)
	}
	if q, ok := p.BuildQuery(Job{}, "860001234567890123"); !ok || q.Method != "GET" || q.Path != "/v1/audio/video-to-audio/{taskId}" {
		t.Fatalf("查询请求: %+v", q)
	}
	if res, _ := p.ParseQuery(200, audioFixture(t, "kling_v2a", "query_processing.json")); res.State != StatePending {
		t.Fatalf("处理中: %+v", res)
	}
	res, err = p.ParseQuery(200, audioFixture(t, "kling_v2a", "query_succeed.json"))
	if err != nil || res.State != StateSuccess || len(res.Outputs) != 1 || res.Outputs[0].Value != "https://p1-kling.klingai.com/kcdn/out/a-1.mp3" ||
		res.Outputs[0].MIME != "audio/mpeg" {
		t.Fatalf("完成取 audios[0].url_mp3（不是配上声音的视频）: %+v %v", res, err)
	}
	if res, _ := p.ParseQuery(200, []byte(`{"code":0,"data":{"task_status":"succeed","task_result":{"videos":[{"url":"https://a/v.mp4"}]}}}`)); res.State != StateFailed {
		t.Fatalf("只有视频没有音频：判失败，不把视频放进音效轨: %+v", res)
	}
	res, _ = p.ParseQuery(200, audioFixture(t, "kling_v2a", "query_failed.json"))
	if res.State != StateFailed || !strings.Contains(res.Message, "risk control") {
		t.Fatalf("失败带 task_status_msg: %+v", res)
	}
}

func TestKlingV2A_失败分类表驱动(t *testing.T) {
	p := mustGet(t, "kling_v2a")
	cases := []struct {
		name      string
		status    int
		body      []byte
		wantHTTP  int
		retryable bool
	}{
		// ⚠️ 官方给欠费回 HTTP 429：按 HTTP 码分类会一直重试。
		{"欠费（官方回 429）", 429, audioFixture(t, "kling_v2a", "error_arrears.json"), 403, false},
		{"限流", 429, audioFixture(t, "kling_v2a", "error_ratelimit.json"), 429, true},
		{"参数错误", 400, audioFixture(t, "kling_v2a", "error_param.json"), 400, false},
		{"服务端错误", 500, audioFixture(t, "kling_v2a", "error_internal.json"), 500, true},
		{"鉴权失败", 401, []byte(`{"code":1002,"message":"Authorization is invalid"}`), 401, false},
		{"HTTP 200 里的业务错误", 200, []byte(`{"code":1301,"message":"content security"}`), 400, false},
		{"不是 JSON 的 502", 502, []byte(`bad gateway`), 502, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := p.ParseSubmit(c.status, c.body)
			if got, retryable := classified(t, err, c.status); got != c.wantHTTP || retryable != c.retryable {
				t.Fatalf("分类 %d（可重试 %v），期望 %d（%v）：%v", got, retryable, c.wantHTTP, c.retryable, err)
			}
		})
	}
}

func TestDurationRangeOf_配乐音效与视频协议(t *testing.T) {
	for _, c := range []struct {
		code, op string
		lo, hi   int
		ok       bool
	}{
		{"volc_music", "text2music", 30, 60, true},
		{"elevenlabs_music", "text2music", 3, 300, true},
		{"kling_v2a", "video2audio", 3, 20, true},
		{"seedance_video", "text2video", 3, 30, true}, // 视频协议照旧取 VideoLimits
		{"volc_music", "video2audio", 0, 0, false},
		{"openai_tts", "text2audio", 0, 0, false},
		{"not_a_protocol", "text2music", 0, 0, false},
	} {
		lo, hi, ok := DurationRangeOf(c.code, c.op)
		if lo != c.lo || hi != c.hi || ok != c.ok {
			t.Fatalf("%s/%s：%d~%d %v，期望 %d~%d %v", c.code, c.op, lo, hi, ok, c.lo, c.hi, c.ok)
		}
	}
}
