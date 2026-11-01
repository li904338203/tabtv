package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"
	"github.com/li904338203/tabtv/backend/internal/provider/transport"
)

// ---------------------------------------------------------------- 测试脚手架

type stubResolver struct{ cfg RunConfig }

func (s stubResolver) ResolveForRun(context.Context, string) (RunConfig, error) {
	return s.cfg, nil
}

type errResolver struct{ err error }

func (e errResolver) ResolveForRun(context.Context, string) (RunConfig, error) {
	return RunConfig{}, e.err
}

type stubMedia struct {
	url    string
	mime   string
	data   []byte
	urlErr error
}

func (s stubMedia) SignedURL(context.Context, int64) (string, string, error) {
	return s.url, s.mime, s.urlErr
}
func (s stubMedia) Inline(context.Context, int64) ([]byte, string, error) {
	return s.data, s.mime, nil
}

type capturedLogs struct {
	mu      sync.Mutex
	entries []LogEntry
}

func (c *capturedLogs) Write(_ context.Context, e LogEntry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = append(c.entries, e)
	return nil
}

func (c *capturedLogs) all() []LogEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]LogEntry(nil), c.entries...)
}

// newProvider 建一个指向测试服务器的适配器。
//
// ⚠️ 测试服务器跑在 127.0.0.1 上，而出网策略默认禁止内网 ——
// 这里显式开 AllowPrivate，与 dev 环境一致。production 由 config 强制关闭。
func newProvider(t *testing.T, cfg RunConfig) (*HTTPProvider, *capturedLogs) {
	t.Helper()
	policy := transport.URLPolicy{AllowPrivate: true}
	p := NewHTTPProvider(transport.NewClient(policy), stubResolver{cfg}, policy,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	logs := &capturedLogs{}
	p.SetRequestLogger(logs)
	return p, logs
}

func jobWith(protocolCode string, params map[string]any, inputs any) genapp.Job {
	snap := map[string]any{
		"modelCode": "m-1", "operationCode": "text2image",
		"apiProtocol": protocolCode, "resultType": "image",
	}
	if params == nil {
		params = map[string]any{}
	}
	params["_snapshot"] = snap
	raw, _ := json.Marshal(params)
	var in json.RawMessage = json.RawMessage("[]")
	if inputs != nil {
		b, _ := json.Marshal(inputs)
		in = b
	}
	return genapp.Job{
		GenerationID: 7001, IdempotencyKey: "idem-1", OperationCode: "text2image",
		ModelCode: "m-1", ResultType: "image", Params: raw, Inputs: in,
	}
}

func readAll(t *testing.T, o genapp.ProviderOutput) []byte {
	t.Helper()
	b, err := io.ReadAll(o.Body)
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := o.Body.(io.Closer); ok {
		_ = c.Close()
	}
	return b
}

// ---------------------------------------------------------------- 同步 JSON

func TestSubmit_文本同步返回(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"一段文案"}}]}`)
	}))
	defer srv.Close()

	p, logs := newProvider(t, RunConfig{
		ModelCode: "m-1", UpstreamModel: "gpt-x", ProtocolCode: "openai_chat",
		ProviderCode: "prov", BaseURL: srv.URL, SubmitPath: "/chat/completions",
		AuthType: "bearer", APIKey: "sk-secret-value",
	})

	res, err := p.Submit(context.Background(),
		jobWith("openai_chat", map[string]any{"prompt": "写一段文案"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != gendomain.SubmitCompleted {
		t.Fatalf("应当同步完成，得到 %s / %s", res.Outcome, res.Message)
	}
	if len(res.Outputs) != 1 || res.Outputs[0].Text != "一段文案" {
		t.Fatalf("产物不对: %+v", res.Outputs)
	}
	if gotAuth != "Bearer sk-secret-value" {
		t.Fatalf("认证头没带上: %q", gotAuth)
	}
	if !strings.Contains(gotBody, "写一段文案") {
		t.Fatalf("提示词没发出去: %s", gotBody)
	}

	// ⚠️ 请求日志绝不能成为新的泄漏面：密钥不在请求体里，
	// 但这条断言钉住"整条日志里不出现明文密钥"这个更强的性质。
	entries := logs.all()
	if len(entries) != 1 {
		t.Fatalf("应当落一条请求日志，得到 %d", len(entries))
	}
	whole := entries[0].RequestBody + entries[0].ResponseBody + entries[0].URL
	if strings.Contains(whole, "sk-secret-value") {
		t.Fatal("请求日志里出现了明文密钥")
	}
	if entries[0].GenerationID != 7001 || entries[0].HTTPStatus != 200 ||
		entries[0].ErrorType != "none" {
		t.Fatalf("日志字段不对: %+v", entries[0])
	}
}

func TestSubmit_提示词顺序是上游文本在前素材词在后(t *testing.T) {
	// ⚠️ 识别信息（角色、场景、剧情）必须排在审美词前面：
	// 反过来的话模型会先照着风格词画，把用户真正要的内容挤掉。
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		fmt.Fprint(w, `{"data":[{"url":"`+"http://127.0.0.1:1/x.png"+`"}]}`)
	}))
	defer srv.Close()

	p, _ := newProvider(t, RunConfig{
		ModelCode: "m-1", ProtocolCode: "openai_images", BaseURL: srv.URL,
		SubmitPath: "/images/generations",
	})
	job := jobWith("openai_images", map[string]any{"prompt": "本节点的补充"},
		[]map[string]any{
			{"kind": "text", "order": 2, "text": "第二段"},
			{"kind": "text", "order": 1, "text": "第一段"},
		})
	// 素材词冻结在快照里。
	var params map[string]any
	_ = json.Unmarshal(job.Params, &params)
	params["_snapshot"].(map[string]any)["promptSuffix"] = "赛博朋克风格"
	job.Params, _ = json.Marshal(params)

	_, _ = p.Submit(context.Background(), job)

	var sent struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatal(err)
	}
	want := "第一段\n第二段\n本节点的补充\n赛博朋克风格"
	if sent.Prompt != want {
		t.Fatalf("提示词顺序不对\n得到: %q\n期望: %q", sent.Prompt, want)
	}
}

// ---------------------------------------------------------------- 产物落地

func TestSubmit_URL产物被取回而不是存地址(t *testing.T) {
	// ⚠️ 上游地址几小时就失效，存进产物表的话用户第二天点开就是裂图，
	// 而那时上游早已把文件清掉，补都补不回来。
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("\x89PNG-BYTES"))
	}))
	defer cdn.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"data":[{"url":%q}]}`, cdn.URL+"/out.png")
	}))
	defer srv.Close()

	p, _ := newProvider(t, RunConfig{
		ModelCode: "m-1", ProtocolCode: "openai_images", BaseURL: srv.URL,
		SubmitPath: "/images/generations",
	})
	res, err := p.Submit(context.Background(), jobWith("openai_images", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != gendomain.SubmitCompleted || len(res.Outputs) != 1 {
		t.Fatalf("应当拿到一个产物: %+v / %s", res.Outputs, res.Message)
	}
	o := res.Outputs[0]
	if o.Type != "image" || o.MIME != "image/png" || o.Size != int64(len("\x89PNG-BYTES")) {
		t.Fatalf("产物元信息不对: %+v", o)
	}
	if got := string(readAll(t, o)); got != "\x89PNG-BYTES" {
		t.Fatalf("产物字节不对: %q", got)
	}
}

func TestSubmit_跨源下载不带凭据(t *testing.T) {
	// ⚠️ GO-31：产物地址常常是 CDN 或对象存储签名链接，
	// 把 Provider 的 API key 一起发过去等于把密钥泄漏给第三方。
	var cdnAuth string
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("IMG"))
	}))
	defer cdn.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"data":[{"url":%q}]}`, cdn.URL+"/out.png")
	}))
	defer srv.Close()

	p, _ := newProvider(t, RunConfig{
		ModelCode: "m-1", ProtocolCode: "openai_images", BaseURL: srv.URL,
		SubmitPath: "/images/generations", AuthType: "bearer", APIKey: "sk-leak-me",
	})
	if _, err := p.Submit(context.Background(), jobWith("openai_images", nil, nil)); err != nil {
		t.Fatal(err)
	}
	if cdnAuth != "" {
		t.Fatalf("跨源下载带上了凭据: %q", cdnAuth)
	}
}

func TestSubmit_同源下载带凭据(t *testing.T) {
	// 同源的产物地址（上游自己的下载端点）通常**需要**认证，
	// 一律不带的话会全部 401 —— 所以不能简单地"下载一律不带头"。
	var downloadAuth string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/files/") {
			downloadAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("IMG"))
			return
		}
		fmt.Fprintf(w, `{"data":[{"url":%q}]}`, srv.URL+"/files/out.png")
	}))
	defer srv.Close()

	p, _ := newProvider(t, RunConfig{
		ModelCode: "m-1", ProtocolCode: "openai_images", BaseURL: srv.URL,
		SubmitPath: "/images/generations", AuthType: "bearer", APIKey: "sk-ok",
	})
	if _, err := p.Submit(context.Background(), jobWith("openai_images", nil, nil)); err != nil {
		t.Fatal(err)
	}
	if downloadAuth != "Bearer sk-ok" {
		t.Fatalf("同源下载应当带凭据，得到 %q", downloadAuth)
	}
}

// relay.example.com 的 Grok 视频：查询结果里的产物是**相对地址** `/v1/videos/{id}/content`，而那个端点要带 key。
//
// ⚠️ 修之前一律「只允许 http/https」：任务在上游**已经成功、已经计费**，我们这边判失败退款
// （2026-10-09 实测两条，每条上游约 0.7 美元）。响应形状照实测原样写。
func TestQuery_相对地址的产物按Provider地址补全并带凭据(t *testing.T) {
	var contentAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			fmt.Fprint(w, `{"request_id":"vid-1"}`)
		case r.URL.Path == "/v1/videos/vid-1/content":
			contentAuth = r.Header.Get("Authorization")
			if contentAuth != "Bearer sk-ok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("MP4"))
		case r.URL.Path == "/v1/videos/vid-1":
			fmt.Fprint(w, `{"id":"vid-1","model":"grok-imagine-video-1.5","progress":100,"status":"completed",`+
				`"video":{"duration":5,"respect_moderation":true,"url":"/v1/videos/vid-1/content"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	p, _ := newProvider(t, RunConfig{
		ModelCode: "grok-1", ProtocolCode: "grok_video", BaseURL: srv.URL + "/",
		SubmitPath: "/v1/videos/generations", QueryPath: "/v1/videos/{taskId}",
		AuthType: "bearer", APIKey: "sk-ok",
	})
	job := jobWith("grok_video", map[string]any{"prompt": "孙悟空", "duration": 5}, nil)
	job.OperationCode, job.ResultType = "text2video", "video"

	res, err := p.Submit(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	q, err := p.Query(context.Background(), job, res.ProviderTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if !q.Done || !q.Success || len(q.Outputs) != 1 {
		t.Fatalf("上游已完成，相对地址的产物应当取得回来: %+v / %s", q, q.Message)
	}
	if got := string(readAll(t, q.Outputs[0])); got != "MP4" || q.Outputs[0].Type != "video" {
		t.Fatalf("产物不对: %q %+v", got, q.Outputs[0])
	}
	if contentAuth != "Bearer sk-ok" {
		t.Fatalf("补全后与 Provider 同源，内容端点要带凭据，得到 %q", contentAuth)
	}
}

func TestSubmit_TTS裸字节落成音频产物(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("ID3-AUDIO"))
	}))
	defer srv.Close()

	p, _ := newProvider(t, RunConfig{
		ModelCode: "tts-1", ProtocolCode: "openai_tts", BaseURL: srv.URL,
		SubmitPath: "/audio/speech",
	})
	job := jobWith("openai_tts", map[string]any{"prompt": "念这段"}, nil)
	job.ResultType = "audio"

	res, err := p.Submit(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != gendomain.SubmitCompleted || len(res.Outputs) != 1 {
		t.Fatalf("应当拿到音频产物: %+v / %s", res.Outputs, res.Message)
	}
	if res.Outputs[0].Type != "audio" {
		t.Fatalf("产物类型不对: %+v", res.Outputs[0])
	}
	if got := string(readAll(t, res.Outputs[0])); got != "ID3-AUDIO" {
		t.Fatalf("音频字节不对: %q", got)
	}
	// ⚠️ 非 JSON 的响应体不能原样塞进 jsonb 列。
	if !json.Valid(res.Raw) {
		t.Fatalf("raw 必须是合法 JSON: %s", res.Raw)
	}
}

// ---------------------------------------------------------------- 异步

func TestSubmitQuery_异步任务从排队到成功(t *testing.T) {
	var polls int
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			fmt.Fprint(w, `{"task_id":"up-42"}`)
		case strings.HasPrefix(r.URL.Path, "/query/"):
			polls++
			if polls == 1 {
				fmt.Fprint(w, `{"status":"processing"}`)
				return
			}
			// ⚠️ 产物在 content.video_url —— 老系统的路径表里没有 content，
			// 于是一个产物都扫不到，落到"任务完成但未返回产物"。
			fmt.Fprintf(w, `{"status":"succeeded","content":{"video_url":%q}}`,
				srv.URL+"/files/out.mp4")
		default:
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("MP4"))
		}
	}))
	defer srv.Close()

	p, _ := newProvider(t, RunConfig{
		ModelCode: "v-1", ProtocolCode: "generic_async", BaseURL: srv.URL,
		SubmitPath: "/submit", QueryPath: "/query/{taskId}",
	})
	job := jobWith("generic_async", nil, nil)
	job.ResultType = "video"

	res, err := p.Submit(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != gendomain.SubmitAccepted || res.ProviderTaskID != "up-42" {
		t.Fatalf("应当拿到上游任务 ID: %+v", res)
	}

	q1, err := p.Query(context.Background(), job, res.ProviderTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if q1.Done {
		t.Fatal("上游还在处理，不该判完成")
	}

	q2, err := p.Query(context.Background(), job, res.ProviderTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if !q2.Done || !q2.Success || len(q2.Outputs) != 1 {
		t.Fatalf("应当成功并带产物: %+v / %s", q2, q2.Message)
	}
	if q2.Outputs[0].Type != "video" {
		t.Fatalf("产物类型不对: %+v", q2.Outputs[0])
	}
	if got := string(readAll(t, q2.Outputs[0])); got != "MP4" {
		t.Fatalf("产物字节不对: %q", got)
	}
}

func TestQuery_任务ID被转义(t *testing.T) {
	// ⚠️ 任务 ID 是上游给的，可能含 / 或 ?，直接拼进路径会拼成另一个端点。
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		fmt.Fprint(w, `{"status":"processing"}`)
	}))
	defer srv.Close()

	p, _ := newProvider(t, RunConfig{
		ModelCode: "v-1", ProtocolCode: "generic_async", BaseURL: srv.URL,
		SubmitPath: "/submit", QueryPath: "/query/{taskId}",
	})
	if _, err := p.Query(context.Background(), jobWith("generic_async", nil, nil),
		"a/b?c=1"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/query/a%2Fb%3Fc=1" {
		t.Fatalf("任务 ID 没被转义: %s", gotPath)
	}
}

// ---------------------------------------------------------------- 失败分类

func TestSubmit_连不上算暂时性可重发(t *testing.T) {
	// 请求根本没到上游，重发安全 —— 用 503 表达暂时性。
	p, _ := newProvider(t, RunConfig{
		ModelCode: "m-1", ProtocolCode: "openai_chat",
		BaseURL: "http://127.0.0.1:1", SubmitPath: "/chat/completions",
		ConnectTimeout: time.Second, RequestTimeout: 2 * time.Second,
	})
	res, err := p.Submit(context.Background(), jobWith("openai_chat", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != gendomain.SubmitRejected || res.HTTPStatus != 503 {
		t.Fatalf("连不上应判暂时性拒绝: %+v", res)
	}
}

func TestSubmit_超时算不确定绝不重发(t *testing.T) {
	// ⚠️ 超时意味着**不知道上游收没收到**。重发可能在上游创建第二个任务，
	// 用户被计费两次而我们只认得一个 —— 这是最贵的一种错误。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(600 * time.Millisecond)
		fmt.Fprint(w, `{"choices":[]}`)
	}))
	defer srv.Close()

	p, _ := newProvider(t, RunConfig{
		ModelCode: "m-1", ProtocolCode: "openai_chat", BaseURL: srv.URL,
		SubmitPath: "/chat/completions", RequestTimeout: 100 * time.Millisecond,
	})
	res, err := p.Submit(context.Background(), jobWith("openai_chat", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != gendomain.SubmitUncertain {
		t.Fatalf("超时必须判不确定，得到 %s", res.Outcome)
	}
}

func TestSubmit_上游4xx原样带回状态码(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"rate limited"}}`)
	}))
	defer srv.Close()

	p, _ := newProvider(t, RunConfig{
		ModelCode: "m-1", ProtocolCode: "openai_chat", BaseURL: srv.URL,
		SubmitPath: "/chat/completions",
	})
	res, err := p.Submit(context.Background(), jobWith("openai_chat", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	// 429 要原样带回：执行器按它判可重试，判成 400 就永远不重试了。
	if res.Outcome != gendomain.SubmitRejected || res.HTTPStatus != 429 {
		t.Fatalf("应当带回上游状态码: %+v", res)
	}
	if !strings.Contains(res.Message, "rate limited") {
		t.Fatalf("失败原因要带上上游说法: %s", res.Message)
	}
}

func TestSubmit_协议没实现时说清楚(t *testing.T) {
	// ⚠️ 老系统最隐蔽的症状：后台一切正常、模型能启用，
	// Worker 执行时才报"不支持的协议"。这里把它变成一句能直接排查的话。
	p, _ := newProvider(t, RunConfig{
		ModelCode: "m-1", ProtocolCode: "runway_video", BaseURL: "https://x.test",
	})
	res, err := p.Submit(context.Background(), jobWith("runway_video", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != gendomain.SubmitRejected || res.HTTPStatus != 400 {
		t.Fatalf("未实现的协议应判永久失败: %+v", res)
	}
	if !strings.Contains(res.Message, "runway_video") {
		t.Fatalf("要指名是哪个协议: %s", res.Message)
	}
}

func TestSubmit_取配置失败判永久失败(t *testing.T) {
	policy := transport.URLPolicy{AllowPrivate: true}
	p := NewHTTPProvider(transport.NewClient(policy),
		errResolver{fmt.Errorf("模型不存在")}, policy,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	res, err := p.Submit(context.Background(), jobWith("openai_chat", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != gendomain.SubmitRejected || res.HTTPStatus != 400 {
		t.Fatalf("配置取不到重试也没用，应判永久失败: %+v", res)
	}
}

func TestSubmit_成功但无产物不算成功(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"status":"ok"}`)
	}))
	defer srv.Close()

	p, _ := newProvider(t, RunConfig{
		ModelCode: "m-1", ProtocolCode: "generic_sync", BaseURL: srv.URL, SubmitPath: "/run",
	})
	res, err := p.Submit(context.Background(), jobWith("generic_sync", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != gendomain.SubmitCompleted && res.Outcome != gendomain.SubmitRejected {
		t.Fatalf("结论不该是别的: %s", res.Outcome)
	}
	if res.Outcome == gendomain.SubmitCompleted {
		t.Fatal("没有产物却判成功：用户会看到一个空结果还被扣了费")
	}
	if !strings.Contains(res.Message, "产物") {
		t.Fatalf("要说清楚是产物没解析出来: %s", res.Message)
	}
}

// ---------------------------------------------------------------- 参考媒体

func TestSubmit_参考图优先用外链(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"看到了"}}]}`)
	}))
	defer srv.Close()

	p, _ := newProvider(t, RunConfig{
		ModelCode: "m-1", ProtocolCode: "openai_chat", BaseURL: srv.URL,
		SubmitPath: "/chat/completions",
	})
	p.SetMediaResolver(stubMedia{url: "https://cdn.example.com/a.png", mime: "image/png"})

	_, err := p.Submit(context.Background(), jobWith("openai_chat",
		map[string]any{"prompt": "这是什么"},
		[]map[string]any{{"kind": "generation", "order": 1, "mediaId": 99, "mediaKind": "image"}}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "https://cdn.example.com/a.png") {
		t.Fatalf("应当把外链发给上游: %s", body)
	}
	if strings.Contains(body, "base64") {
		t.Fatal("有可用外链时不该内联 base64：一张图好几 MB，每次生成都要重传")
	}
}

func TestSubmit_外链是内网地址时回落内联(t *testing.T) {
	// ⚠️ 本地 MinIO 签出来的是 127.0.0.1，上游一个字节也拉不到。
	// 我们无从知道上游的网络，但内网地址上游一定够不着。
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"看到了"}}]}`)
	}))
	defer srv.Close()

	p, _ := newProvider(t, RunConfig{
		ModelCode: "m-1", ProtocolCode: "openai_chat", BaseURL: srv.URL,
		SubmitPath: "/chat/completions",
	})
	// 注意：出网策略在这个测试里是 AllowPrivate=true（服务器在 127.0.0.1），
	// 但参考媒体的可达性判定用的是**固定的公网规则**，与出网策略分开。
	p.SetMediaResolver(stubMedia{
		url: "http://127.0.0.1:9000/bucket/a.png", mime: "image/png", data: []byte("PNGDATA"),
	})

	_, err := p.Submit(context.Background(), jobWith("openai_chat", nil,
		[]map[string]any{{"kind": "reference", "order": 1, "mediaId": 99, "mediaKind": "image"}}))
	if err != nil {
		t.Fatal(err)
	}
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("PNGDATA"))
	if !strings.Contains(body, want) {
		t.Fatalf("应当回落成内联 base64: %s", body)
	}
}

func TestSubmit_内联超限时指名真实原因(t *testing.T) {
	p, _ := newProvider(t, RunConfig{
		ModelCode: "m-1", ProtocolCode: "openai_chat", BaseURL: "https://x.test",
		SubmitPath: "/chat/completions",
	})
	p.MaxInlineBytes = 8
	p.SetMediaResolver(stubMedia{
		url: "http://127.0.0.1:9000/a.png", mime: "image/png",
		data: []byte("0123456789ABCDEF"),
	})
	res, err := p.Submit(context.Background(), jobWith("openai_chat", nil,
		[]map[string]any{{"kind": "reference", "order": 1, "mediaId": 99, "mediaKind": "image"}}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != gendomain.SubmitRejected {
		t.Fatalf("取不到输入应判失败: %+v", res)
	}
	// 真实原因是"部署没配可外链的对象存储"，不说清楚会一路怀疑模型和参数。
	if !strings.Contains(res.Message, "对象存储") {
		t.Fatalf("要指名真实原因: %s", res.Message)
	}
}

// ---------------------------------------------------------------- multipart

func TestSubmit_图片编辑走multipart并带上文件(t *testing.T) {
	var gotType string
	var gotFields map[string]string
	var gotFile []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotType = r.Header.Get("Content-Type")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			w.WriteHeader(500)
			return
		}
		gotFields = map[string]string{}
		for k, v := range r.MultipartForm.Value {
			gotFields[k] = v[0]
		}
		f, hdr, err := r.FormFile("image")
		if err != nil {
			w.WriteHeader(500)
			return
		}
		defer f.Close()
		gotFile, _ = io.ReadAll(f)
		_ = hdr
		fmt.Fprint(w, `{"data":[{"b64_json":"`+
			base64.StdEncoding.EncodeToString([]byte("EDITED"))+`"}]}`)
	}))
	defer srv.Close()

	p, _ := newProvider(t, RunConfig{
		ModelCode: "img-1", ProtocolCode: "openai_images", BaseURL: srv.URL,
		// ⚠️ submitPath 留空，让协议自己的路径生效（编辑接口与生成接口不同）。
	})
	p.SetMediaResolver(stubMedia{
		url: "http://127.0.0.1:9000/a.png", mime: "image/png", data: []byte("SRCIMG"),
	})

	res, err := p.Submit(context.Background(), jobWith("openai_images",
		map[string]any{"prompt": "改成夜景"},
		[]map[string]any{{"kind": "generation", "order": 1, "mediaId": 5, "mediaKind": "image"}}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(gotType, "multipart/form-data; boundary=") {
		t.Fatalf("Content-Type 必须带 boundary: %q", gotType)
	}
	if gotFields["prompt"] != "改成夜景" {
		t.Fatalf("表单字段不对: %+v", gotFields)
	}
	if string(gotFile) != "SRCIMG" {
		t.Fatalf("参考图字节没发出去: %q", gotFile)
	}
	if res.Outcome != gendomain.SubmitCompleted || len(res.Outputs) != 1 {
		t.Fatalf("应当拿到产物: %+v / %s", res.Outputs, res.Message)
	}
	if got := string(readAll(t, res.Outputs[0])); got != "EDITED" {
		t.Fatalf("产物字节不对: %q", got)
	}
}

// ---------------------------------------------------------------- 分派与工具

func TestRouting_按协议码分派(t *testing.T) {
	policy := transport.URLPolicy{AllowPrivate: true}
	httpP := NewHTTPProvider(transport.NewClient(policy),
		errResolver{fmt.Errorf("不该走到真实适配器")}, policy,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := NewRouting(httpP, genapp.DevProvider{})

	dev, err := r.Submit(context.Background(), jobWith("dev_fake", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if dev.Outcome != gendomain.SubmitCompleted {
		t.Fatalf("dev_fake 应走假适配器: %+v", dev)
	}

	real, err := r.Submit(context.Background(), jobWith("openai_chat", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if real.Outcome != gendomain.SubmitRejected {
		t.Fatalf("非 dev_fake 应走真实适配器: %+v", real)
	}
}

func TestRouting_未挂假适配器时dev_fake也走真实路径(t *testing.T) {
	// ⚠️ production 下 dev 为 nil：一个配错协议码的模型会得到"协议不可用"，
	// 而不是一个假产物 —— 假产物会被当成真结果归档、展示、计费。
	policy := transport.URLPolicy{AllowPrivate: true}
	httpP := NewHTTPProvider(transport.NewClient(policy),
		stubResolver{RunConfig{ProtocolCode: "dev_fake", BaseURL: "https://x.test"}},
		policy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := NewRouting(httpP, nil)

	res, err := r.Submit(context.Background(), jobWith("dev_fake", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome == gendomain.SubmitCompleted {
		t.Fatal("没挂假适配器却产出了假产物")
	}
}

func TestJoinURL_不吃掉base里的路径段(t *testing.T) {
	// ⚠️ 很多中转的 base 是 https://x.com/v1，吃掉 /v1 就全 404。
	cases := []struct{ base, path, want string }{
		{"https://x.com/v1", "/chat/completions", "https://x.com/v1/chat/completions"},
		{"https://x.com/v1/", "chat/completions", "https://x.com/v1/chat/completions"},
		{"https://x.com", "/v1/messages", "https://x.com/v1/messages"},
		{"https://x.com/v1", "https://other.com/abs", "https://other.com/abs"},
		{"https://x.com/v1", "", "https://x.com/v1"},
	}
	for _, c := range cases {
		got, err := joinURL(c.base, c.path)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("joinURL(%q,%q) = %q，期望 %q", c.base, c.path, got, c.want)
		}
	}
}

func TestUserParams_保留未知字段且剔掉快照(t *testing.T) {
	// ⚠️ GO-35：协议可能要用到我们还没建模的参数，
	// 按 struct 解一遍再塞回去会把它们悄悄丢掉。
	raw := json.RawMessage(`{"aspect_ratio":"16:9","未来字段":{"a":1},"_snapshot":{"modelCode":"m"}}`)
	got := userParams(raw)
	if _, ok := got["_snapshot"]; ok {
		t.Fatal("快照不该混进用户参数")
	}
	if got["aspect_ratio"] != "16:9" {
		t.Fatalf("已知字段丢了: %+v", got)
	}
	if _, ok := got["未来字段"]; !ok {
		t.Fatalf("未知字段被丢掉了: %+v", got)
	}
}

func TestOutputType_以MIME为准结果类型兜底(t *testing.T) {
	// ⚠️ 结果类型是我们期望的，MIME 是上游实际给的。
	// 不一致时按实际的存，否则一个回了 jpeg 的"视频"任务会存成视频、播不出来。
	cases := []struct{ mime, result, want string }{
		{"image/jpeg", "video", "image"},
		{"video/mp4", "image", "video"},
		{"", "audio", "audio"},
		{"application/octet-stream", "video", "video"},
	}
	for _, c := range cases {
		if got := outputType(c.mime, c.result); got != c.want {
			t.Errorf("outputType(%q,%q) = %q，期望 %q", c.mime, c.result, got, c.want)
		}
	}
}

// CD-80：蒙版靠显式角色识别（协议不再按「第二张就是蒙版」猜），角色必须从输入快照一路带到协议。
func TestParseInputs_带上蒙版角色(t *testing.T) {
	in, err := parseInputs(json.RawMessage(`[
		{"kind":"reference","order":1,"mediaId":4,"mediaKind":"image"},
		{"kind":"reference","order":2,"mediaId":5,"mediaKind":"image","role":"mask"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(in) != 2 || in[0].Role != "" || in[1].Role != "mask" {
		t.Fatalf("角色没有解析出来: %+v", in)
	}
}
