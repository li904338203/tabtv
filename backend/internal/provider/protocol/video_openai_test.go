package protocol

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openAIVideoFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "openai_video", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func openAIVideoMeta(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	m, ok := body["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("规格与参考都应在 metadata 里: %+v", body)
	}
	return m
}

// withDurations 给 job 里的视频按顺序填时长（毫秒）。
func withDurations(job Job, ms ...int) Job {
	i := 0
	for k := range job.Media {
		if job.Media[k].Kind == "video" && i < len(ms) {
			job.Media[k].DurationMs = ms[i]
			i++
		}
	}
	return job
}

// ---------------------------------------------------------------- 请求体：每种操作

func TestOpenAIVideo_文生视频(t *testing.T) {
	body := buildBody(t, "openai_video", videoJob("text2video", map[string]any{
		"resolution": "1080p", "aspect_ratio": "9:16", "duration": "12",
	}))
	if body["model"] != "model-v1" || body["prompt"] != "一只狐狸在雪地里奔跑" {
		t.Fatalf("model/prompt 不对: %+v", body)
	}
	// ⚠️ seconds 必须显式发：上游默认 15 秒，计价按用户选的时长。
	if body["seconds"] != float64(12) {
		t.Fatalf("seconds 应为 12: %v", body["seconds"])
	}
	meta := openAIVideoMeta(t, body)
	if meta["resolution"] != "1080p" || meta["ratio"] != "9:16" {
		t.Fatalf("规格不对: %+v", meta)
	}
	if _, ok := meta["mode"]; ok {
		t.Fatal("纯文生不带 mode")
	}
	for _, k := range []string{"image_urls", "video_urls", "audio_urls", "first_frame_url"} {
		if _, ok := meta[k]; ok {
			t.Fatalf("纯文生不该带 %s", k)
		}
	}
	if meta["generate_audio"] != false {
		t.Fatalf("原生音轨默认关: %v", meta["generate_audio"])
	}
}

func TestOpenAIVideo_默认值与计价一致(t *testing.T) {
	body := buildBody(t, "openai_video", videoJob("text2video", nil))
	if body["seconds"] != float64(DefaultVideoSeconds) {
		t.Fatalf("默认时长应为 %d（与计价同一个默认值），实际 %v", DefaultVideoSeconds, body["seconds"])
	}
	if openAIVideoMeta(t, body)["resolution"] != "720p" {
		t.Fatal("resolution 是上游必填，缺省要给 720p")
	}
}

func TestOpenAIVideo_图生视频按参考图发(t *testing.T) {
	job := videoJob("img2video", nil, "image:https://cdn.x/a.png")
	meta := openAIVideoMeta(t, buildBody(t, "openai_video", job))
	if meta["mode"] != "references" {
		t.Fatalf("单图只能走 references（frames 要求首尾帧同时给）: %v", meta["mode"])
	}
	urls, _ := meta["image_urls"].([]any)
	if len(urls) != 1 || urls[0] != "https://cdn.x/a.png" {
		t.Fatalf("image_urls 不对: %v", meta["image_urls"])
	}
	if _, ok := meta["first_frame_url"]; ok {
		t.Fatal("单图不能只发 first_frame_url（上游要求首尾帧同时给）")
	}
}

func TestOpenAIVideo_首尾帧走frames且顺序即语义(t *testing.T) {
	job := videoJob("first_last_frame", nil, "image:https://cdn.x/first.png", "image:https://cdn.x/last.png")
	job.Prompt = ""
	body := buildBody(t, "openai_video", job)
	meta := openAIVideoMeta(t, body)
	if meta["mode"] != "frames" ||
		meta["first_frame_url"] != "https://cdn.x/first.png" || meta["last_frame_url"] != "https://cdn.x/last.png" {
		t.Fatalf("首尾帧不对: %+v", meta)
	}
	if _, ok := meta["image_urls"]; ok {
		t.Fatal("frames 模式不该再带 image_urls")
	}
	// 有完整首尾帧时提示词可空：不发兜底英文。
	if _, ok := body["prompt"]; ok {
		t.Fatalf("提示词为空时不该塞兜底: %v", body["prompt"])
	}
}

func TestOpenAIVideo_首尾帧必须恰好两张(t *testing.T) {
	for _, n := range []int{1, 3} {
		media := []string{}
		for i := 0; i < n; i++ {
			media = append(media, "image:https://cdn.x/i.png")
		}
		msg := buildErr(t, "openai_video", videoJob("first_last_frame", nil, media...))
		if !strings.Contains(msg, "恰好两张") {
			t.Errorf("%d 张应被拒: %s", n, msg)
		}
	}
}

func TestOpenAIVideo_全能参考带图视频音频(t *testing.T) {
	job := withDurations(videoJob("ref2video", map[string]any{"audio": true},
		"image:https://cdn.x/a.png", "video:https://cdn.x/v1.mp4", "audio:https://cdn.x/s.mp3",
		"image:https://cdn.x/b.png", "video:https://cdn.x/v2.mp4"), 8000, 6500)
	meta := openAIVideoMeta(t, buildBody(t, "openai_video", job))
	if meta["mode"] != "references" {
		t.Fatalf("mode: %v", meta["mode"])
	}
	imgs, _ := meta["image_urls"].([]any)
	vids, _ := meta["video_urls"].([]any)
	auds, _ := meta["audio_urls"].([]any)
	if len(imgs) != 2 || imgs[0] != "https://cdn.x/a.png" || imgs[1] != "https://cdn.x/b.png" {
		t.Fatalf("图片顺序要保留: %v", imgs)
	}
	if len(vids) != 2 || vids[1] != "https://cdn.x/v2.mp4" || len(auds) != 1 {
		t.Fatalf("视频/音频不对: %v %v", vids, auds)
	}
	// 8.0 + 6.5 = 14.5 秒 → 向上取整 15。
	if meta["input_video_duration"] != float64(15) {
		t.Fatalf("input_video_duration 应为 15: %v", meta["input_video_duration"])
	}
	if meta["generate_audio"] != true {
		t.Fatal("用户开了音频要发 generate_audio=true")
	}
}

func TestOpenAIVideo_参考视频合计超过30秒被拒(t *testing.T) {
	job := withDurations(videoJob("ref2video", nil,
		"video:https://cdn.x/v1.mp4", "video:https://cdn.x/v2.mp4"), 20000, 10500)
	if msg := buildErr(t, "openai_video", job); !strings.Contains(msg, "30") {
		t.Fatalf("合计 30.5 秒应被拒: %s", msg)
	}
	ok := withDurations(videoJob("ref2video", nil,
		"video:https://cdn.x/v1.mp4", "video:https://cdn.x/v2.mp4"), 20000, 10000)
	if openAIVideoMeta(t, buildBody(t, "openai_video", ok))["input_video_duration"] != float64(30) {
		t.Fatal("恰好 30 秒应放行")
	}
}

func TestOpenAIVideo_参考视频时长未知时拒绝而不猜(t *testing.T) {
	job := videoJob("video2video", nil, "video:https://cdn.x/v1.mp4")
	if msg := buildErr(t, "openai_video", job); !strings.Contains(msg, "时长未知") {
		t.Fatalf("时长未知应被拒: %s", msg)
	}
}

func TestOpenAIVideo_视频生视频(t *testing.T) {
	job := withDurations(videoJob("video2video", nil, "video:https://cdn.x/src.mp4"), 9200)
	meta := openAIVideoMeta(t, buildBody(t, "openai_video", job))
	vids, _ := meta["video_urls"].([]any)
	if meta["mode"] != "references" || len(vids) != 1 || meta["input_video_duration"] != float64(10) {
		t.Fatalf("视频生视频不对: %+v", meta)
	}
	two := withDurations(videoJob("video2video", nil,
		"video:https://cdn.x/a.mp4", "video:https://cdn.x/b.mp4"), 1000, 1000)
	if msg := buildErr(t, "openai_video", two); !strings.Contains(msg, "恰好一个") {
		t.Fatalf("两段视频应被拒: %s", msg)
	}
}

func TestOpenAIVideo_规格越界与兜底(t *testing.T) {
	cases := []struct {
		params map[string]any
		want   string
	}{
		{map[string]any{"duration": 3}, "时长"},
		{map[string]any{"duration": 31}, "时长"},
		{map[string]any{"resolution": "2k"}, "分辨率"},
		{map[string]any{"aspect_ratio": "2:1"}, "比例"},
	}
	for _, c := range cases {
		if msg := buildErr(t, "openai_video", videoJob("text2video", c.params)); !strings.Contains(msg, c.want) {
			t.Errorf("%v 应因%s被拒: %s", c.params, c.want, msg)
		}
	}
	// adaptive 这家不认：丢弃而不是报错（历史节点里可能还存着）。
	meta := openAIVideoMeta(t, buildBody(t, "openai_video",
		videoJob("text2video", map[string]any{"aspect_ratio": "adaptive", "duration": 30})))
	if _, ok := meta["ratio"]; ok {
		t.Fatalf("adaptive 应被丢弃: %v", meta["ratio"])
	}
}

func TestOpenAIVideo_提示词规则(t *testing.T) {
	empty := videoJob("text2video", nil)
	empty.Prompt = "  "
	if msg := buildErr(t, "openai_video", empty); !strings.Contains(msg, "提示词") {
		t.Fatalf("文生缺提示词应被拒: %s", msg)
	}
	withRef := videoJob("ref2video", nil, "image:https://cdn.x/a.png")
	withRef.Prompt = ""
	if body := buildBody(t, "openai_video", withRef); body["prompt"] != nil {
		t.Fatalf("有参考素材时提示词可空、不塞兜底: %v", body["prompt"])
	}
	if msg := buildErr(t, "openai_video", videoJob("ref2video", nil)); !strings.Contains(msg, "至少") {
		t.Fatalf("全能参考没有素材应被拒: %s", msg)
	}
}

func TestOpenAIVideo_请求路径与方法(t *testing.T) {
	p := mustGet(t, "openai_video")
	req, err := p.BuildSubmit(videoJob("text2video", nil))
	if err != nil || req.Method != "POST" || req.Path != "/videos" {
		t.Fatalf("提交应是 POST /v1/videos: %+v %v", req, err)
	}
	q, ok := p.BuildQuery(Job{}, "video_abc")
	if !ok || q.Method != "GET" || q.Path != "/videos/{taskId}" {
		t.Fatalf("查询应是 GET /v1/videos/{taskId}: %+v", q)
	}
}

// ---------------------------------------------------------------- 响应解析

func TestOpenAIVideo_提交响应(t *testing.T) {
	p := mustGet(t, "openai_video")
	res, err := p.ParseSubmit(200, openAIVideoFixture(t, "submit_ok.json"))
	if err != nil || !res.Async || res.TaskID != "video_abc123" {
		t.Fatalf("应取到任务 ID: %+v %v", res, err)
	}
	if _, err := p.ParseSubmit(200, []byte(`{"object":"video"}`)); err == nil {
		t.Fatal("2xx 没有 id 应报错")
	}
}

func TestOpenAIVideo_非2xx带状态码与原文(t *testing.T) {
	// 表驱动：状态码原样带进错误（执行器按 resp.StatusCode 分类可否重试，
	// 这里要保证原文不被吞掉、也不被猜字段改写）。
	p := mustGet(t, "openai_video")
	cases := []struct {
		status int
		body   []byte
	}{
		{400, []byte(`{"detail":"resolution is required"}`)},
		{401, openAIVideoFixture(t, "error_401.json")},
		{429, []byte(`rate limited`)},
		{500, []byte("<html>bad gateway\x00</html>")},
		{503, nil},
	}
	for _, c := range cases {
		_, err := p.ParseSubmit(c.status, c.body)
		if err == nil {
			t.Fatalf("HTTP %d 应报错", c.status)
		}
		msg := err.Error()
		if !strings.Contains(msg, "HTTP "+itoa(c.status)) {
			t.Errorf("错误要带状态码 %d: %s", c.status, msg)
		}
		if strings.ContainsRune(msg, 0) {
			t.Errorf("错误信息里不能有 NUL（PG text 拒收）: %q", msg)
		}
		if _, qerr := p.ParseQuery(c.status, c.body); qerr == nil {
			t.Errorf("查询 HTTP %d 也应报错（由执行器下一轮再查）", c.status)
		}
	}
	_, err := p.ParseSubmit(401, openAIVideoFixture(t, "error_401.json"))
	if !strings.Contains(err.Error(), "invalid api key") {
		t.Fatalf("要带原文: %v", err)
	}
}

func TestOpenAIVideo_状态词表(t *testing.T) {
	p := mustGet(t, "openai_video")
	cases := []struct {
		file string
		want TaskState
	}{
		{"query_queued.json", StatePending},
		{"query_in_progress.json", StatePending},
		{"query_completed_relative.json", StateSuccess},
		{"query_failed.json", StateFailed},
	}
	for _, c := range cases {
		res, err := p.ParseQuery(200, openAIVideoFixture(t, c.file))
		if err != nil || res.State != c.want {
			t.Errorf("%s: 期望 %s，实际 %+v %v", c.file, c.want, res, err)
		}
	}
	// 文档外的状态一律按 pending（多轮询几次，由死线兜底），不判失败。
	res, _ := p.ParseQuery(200, []byte(`{"id":"x","status":"starting"}`))
	if res.State != StatePending {
		t.Fatalf("未知状态应按 pending: %+v", res)
	}
}

func TestOpenAIVideo_失败信息是原文不猜字段(t *testing.T) {
	res, _ := mustGet(t, "openai_video").ParseQuery(200, openAIVideoFixture(t, "query_failed.json"))
	if !strings.Contains(res.Message, "content_policy") || !strings.Contains(res.Message, "prompt rejected") {
		t.Fatalf("失败信息应带响应原文: %s", res.Message)
	}
}

func TestOpenAIVideo_产物地址(t *testing.T) {
	p := mustGet(t, "openai_video")
	cases := []struct {
		file         string
		value        string
		authRequired bool
		fallback     string
	}{
		// 相对地址：下载层按 Provider 地址补全，补全后同源 → 带 key。
		{"query_completed_relative.json", "/v1/videos/video_abc123/content", true,
			"/v1/videos/video_abc123/content"},
		// 绝对地址、不要认证：照常下载，不同源就不带 key。
		{"query_completed_absolute.json", "https://cdn.example.com/out/video_abc123.mp4?sig=x", false, ""},
		// 绝对地址、要认证：给同源兜底，地址不同源时改走内容端点而不外发密钥。
		{"query_completed_absolute_auth.json", "https://api.example.com/files/video_abc123.mp4", true,
			"/v1/videos/video_abc123/content"},
		// 没给 result_url：回落内容端点。
		{"query_completed_no_url.json", "/v1/videos/video_abc123/content", true, ""},
	}
	for _, c := range cases {
		res, err := p.ParseQuery(200, openAIVideoFixture(t, c.file))
		if err != nil || res.State != StateSuccess || len(res.Outputs) != 1 {
			t.Fatalf("%s: 应成功且恰好一个产物: %+v %v", c.file, res, err)
		}
		o := res.Outputs[0]
		if o.Kind != "url" || o.Value != c.value || o.AuthRequired != c.authRequired || o.AuthFallback != c.fallback {
			t.Errorf("%s: 产物不对 %+v", c.file, o)
		}
	}
	// 完成但既没有地址也没有 id：判失败而不是挂起。
	res, _ := p.ParseQuery(200, []byte(`{"status":"completed"}`))
	if res.State != StateFailed || !strings.Contains(res.Message, "未返回视频地址") {
		t.Fatalf("完成却无产物应判失败: %+v", res)
	}
}

// ---------------------------------------------------------------- 能力声明

func TestOpenAIVideo_能力边界(t *testing.T) {
	caps := Capabilities("openai_video")
	if caps == nil {
		t.Fatal("OpenAI 兼容视频接口 要声明能力边界")
	}
	if !SupportsOperation("openai_video", "first_last_frame") {
		t.Fatal("OpenAI 兼容视频接口 有 frames 模式，支持首尾帧")
	}
	if caps["first_last_frame"].MaxImages != 2 || caps["img2video"].MaxImages != 1 {
		t.Fatalf("首尾帧 2 张、图生 1 张: %+v", caps)
	}
	if !SupportsRole("openai_video", "ref2video", RoleAudio) {
		t.Fatal("OpenAI 兼容视频接口 的 references 收音频")
	}
	if SupportsRole("openai_video", "img2video", RoleVideo) || SupportsRole("openai_video", "text2video", RoleImage) {
		t.Fatal("图生不收视频、文生不收图")
	}
	if c := caps["ref2video"]; c.MaxImages != 30 || c.MaxVideos != 10 {
		t.Fatalf("参考上限 30 图 / 10 视频: %+v", c)
	}
	if SupportsOperation("openai_video", "text2img") {
		t.Fatal("不该声明非视频操作")
	}
}
