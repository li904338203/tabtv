package protocol

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"
)

// docs/agent/smartcut SCT-38：火山 MediaKit 去字幕。
//
// ⚠️ fixture 照 dev/01 §7 记下的形状写（2026-10-27 读到的官方文档）；状态词与错误结构待核实（SCR-7）——
// 核实之后改的是 fixture 与 erase_volc.go 的词表，不是把测试改成迁就实现。

func mediaKitFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "volc_mediakit", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func eraseJob(params map[string]any, media ...Media) Job {
	if params == nil {
		params = map[string]any{}
	}
	return Job{OperationCode: "video_desubtitle", ResultType: "video", IdempotencyKey: "idem-erase", Params: params,
		Media: media}
}

func TestMediaKitErase_提交取task_id(t *testing.T) {
	p := mustGet(t, "volc_mediakit_erase")
	clip := Media{Kind: "video", Order: 1, Value: "https://relay.example.com/scratch/range/7001/1-2000-5000.mp4?sig=1",
		DurationMs: 3000}
	req, err := p.BuildSubmit(eraseJob(map[string]any{"mode": "subtitle"}, clip))
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != "POST" || req.Path != MediaKitErasePath {
		t.Fatalf("提交地址: %s %s", req.Method, req.Path)
	}
	var body map[string]any
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["video_url"] != clip.Value || body["mode"] != "subtitle" || len(body) != 2 {
		t.Fatalf("请求体只带地址与模式: %s", req.Body)
	}
	// 高级参数只在给了时才带。
	req, _ = p.BuildSubmit(eraseJob(map[string]any{"erase_ratio_location": []any{0.0, 0.8, 1.0, 1.0}, "model_version": "v2"}, clip))
	_ = json.Unmarshal(req.Body, &body)
	if body["model_version"] != "v2" || body["erase_ratio_location"] == nil {
		t.Fatalf("给了的高级参数要原样带上: %s", req.Body)
	}

	for name, want := range map[string]string{"submit_ok.json": "erase-7f3a91", "submit_ok_data.json": "erase-7f3a92"} {
		res, err := p.ParseSubmit(200, mediaKitFixture(t, name))
		if err != nil || !res.Async || res.TaskID != want {
			t.Fatalf("%s：应取到任务 ID %s: %+v %v", name, want, res, err)
		}
	}
	if q, ok := p.BuildQuery(Job{}, "erase-7f3a91"); !ok || q.Method != "GET" || q.Path != MediaKitTaskPath {
		t.Fatalf("查询地址: %+v", q)
	}
	if d, _ := Lookup("volc_mediakit_erase"); !d.Async || !d.RequiresQueryPath {
		t.Fatalf("异步、必须配查询路径: %+v", d)
	}
}

func TestMediaKitErase_查询处理中(t *testing.T) {
	p := mustGet(t, "volc_mediakit_erase")
	for _, body := range [][]byte{
		mediaKitFixture(t, "query_processing.json"),
		[]byte(`{"task_id":"x","status":"queued"}`),
		// 没见过的状态词按处理中（多查几次由死线兜底），不按失败：上游多一个排队态不该把任务全判失败。
		[]byte(`{"task_id":"x","status":"waiting_for_gpu"}`),
		[]byte(`{"data":{"task_id":"x","status":"running"}}`),
	} {
		res, err := p.ParseQuery(200, body)
		if err != nil || res.State != StatePending {
			t.Fatalf("应为处理中: %s → %+v %v", body, res, err)
		}
	}
}

func TestMediaKitErase_完成取result_video_url(t *testing.T) {
	p := mustGet(t, "volc_mediakit_erase")
	res, err := p.ParseQuery(200, mediaKitFixture(t, "query_completed.json"))
	if err != nil || res.State != StateSuccess || len(res.Outputs) != 1 {
		t.Fatalf("应完成并有一个产物: %+v %v", res, err)
	}
	out := res.Outputs[0]
	// ⚠️ 只认 result.video_url：根上回显的是我们发过去的输入地址，取了它就把原片当成结果存回去。
	if out.Kind != "url" || !strings.HasPrefix(out.Value, "https://mediakit-output.") || out.MIME != "video/mp4" {
		t.Fatalf("产物要取 result.video_url: %+v", out)
	}
	wrapped := []byte(`{"code":0,"data":{"status":"success","result":{"video_url":"https://out.example.com/a.mp4"}}}`)
	if res, _ := p.ParseQuery(200, wrapped); res.State != StateSuccess || res.Outputs[0].Value != "https://out.example.com/a.mp4" {
		t.Fatalf("外面包一层 data 也认: %+v", res)
	}
	res, err = p.ParseQuery(200, mediaKitFixture(t, "query_completed_no_url.json"))
	if err != nil || res.State != StateFailed || !strings.Contains(res.Message, "result.video_url") {
		t.Fatalf("完成却没有结果地址：判失败并说清缺什么，不去扫兜底路径: %+v %v", res, err)
	}
}

// 失败分类：提交侧按 StatusError / HTTP 状态码走 ClassifyHTTP（参数类不可重试、服务端与限流可重试），
// 查询到的失败是任务终态（带原因，退款由生成域做）。
func TestMediaKitErase_失败分类表驱动(t *testing.T) {
	p := mustGet(t, "volc_mediakit_erase")
	cases := []struct {
		name   string
		status int
		body   []byte
		// wantHTTP 是用于分类的状态码（StatusError 给的，或者就是 HTTP 码）。
		wantHTTP  int
		retryable bool
		msg       string
	}{
		{"HTTP 200 里的参数错误", 200, mediaKitFixture(t, "submit_error_param.json"), 400, false, "video_url is not accessible"},
		{"火山 OpenAPI 结构的内部错误", 200, mediaKitFixture(t, "submit_error_internal.json"), 503, true, "service busy"},
		{"限流", 200, mediaKitFixture(t, "submit_error_throttled.json"), 429, true, "QPS limit"},
		{"非 0 的数字错误码", 200, []byte(`{"code":40001,"message":"mode is invalid"}`), 400, false, "mode is invalid"},
		{"5 开头的数字错误码", 200, []byte(`{"code":50002,"message":"internal"}`), 503, true, "internal"},
		{"HTTP 401", 401, []byte(`{"error":{"message":"invalid api key"}}`), 401, false, "HTTP 401"},
		{"HTTP 502", 502, []byte(`bad gateway`), 502, true, "HTTP 502"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := p.ParseSubmit(c.status, c.body)
			if err == nil {
				t.Fatal("应失败")
			}
			got := c.status
			var se *StatusError
			if errors.As(err, &se) {
				got = se.Status
			}
			if got != c.wantHTTP {
				t.Fatalf("分类用的状态码 %d，期望 %d（%v）", got, c.wantHTTP, err)
			}
			if r := gendomain.ClassifyHTTP(got) == gendomain.RetryTransient; r != c.retryable {
				t.Fatalf("可重试 = %v，期望 %v", r, c.retryable)
			}
			if !strings.Contains(err.Error(), c.msg) {
				t.Fatalf("错误要带上游原因: %v", err)
			}
		})
	}
	// 没有任务 ID 也认不出错误：说清楚并带原文（多半是上游换了结构）。
	if _, err := p.ParseSubmit(200, []byte(`{"status":"ok"}`)); err == nil || !strings.Contains(err.Error(), "没有返回任务 ID") {
		t.Fatalf("应说清没有任务 ID: %v", err)
	}

	res, err := p.ParseQuery(200, mediaKitFixture(t, "query_failed.json"))
	if err != nil || res.State != StateFailed || !strings.Contains(res.Message, "input resolution exceeds 2K") {
		t.Fatalf("任务失败要带原因: %+v %v", res, err)
	}
	if res, _ := p.ParseQuery(200, []byte(`{"status":"failed"}`)); res.State != StateFailed || !strings.Contains(res.Message, `"failed"`) {
		t.Fatalf("取不到原因时带原文: %+v", res)
	}
	// 查询本身 HTTP 失败：返回 error（下一轮再查，由死线兜底），不判任务失败。
	if _, err := p.ParseQuery(503, []byte("busy")); err == nil {
		t.Fatal("查询 HTTP 失败应返回 error")
	}
}

func TestMediaKitErase_只收地址形态(t *testing.T) {
	p := mustGet(t, "volc_mediakit_erase")
	if !RequiresMediaURL(p) {
		t.Fatal("去字幕只收公网地址：要实现 MediaURLRequiring，Worker 才不会内联、签不出公网地址时走中转")
	}
	_, err := p.BuildSubmit(eraseJob(nil, Media{Kind: "video", Order: 1, Value: "data:video/mp4;base64,AAAA"}))
	if err == nil || !strings.Contains(err.Error(), "只收公网地址") {
		t.Fatalf("内联数据要当场拒: %v", err)
	}
	if _, err := p.BuildSubmit(eraseJob(nil)); err == nil {
		t.Fatal("没有视频要拒")
	}
	if _, err := p.BuildSubmit(Job{OperationCode: "video2video", Media: []Media{{Kind: "video", Value: "https://a/b.mp4"}}}); err == nil {
		t.Fatal("只做去字幕")
	}
	ops := Capabilities("volc_mediakit_erase")
	if len(ops) != 1 || ops["video_desubtitle"].MaxVideos != 1 || len(ops["video_desubtitle"].Roles) != 1 ||
		ops["video_desubtitle"].Roles[0] != RoleVideo {
		t.Fatalf("能力只声明去字幕、一段视频: %+v", ops)
	}
}
