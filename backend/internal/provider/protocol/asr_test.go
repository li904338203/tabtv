package protocol

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// docs/agent/smartcut SCT-33：语音识别的两家协议。

func asrFixture(t *testing.T, dir, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func volcHeader(code string) http.Header {
	h := http.Header{}
	if code != "" {
		h.Set("X-Api-Status-Code", code)
		h.Set("X-Api-Message", "msg-"+code)
	}
	return h
}

func statusOf(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Status
	}
	return 0
}

func onlyText(t *testing.T, res Result) (string, map[string]any) {
	t.Helper()
	if len(res.Outputs) != 1 || res.Outputs[0].Kind != "text" {
		t.Fatalf("识别结果应是恰好一个文本产物: %+v", res.Outputs)
	}
	var meta map[string]any
	if err := json.Unmarshal(res.Outputs[0].Meta, &meta); err != nil {
		t.Fatalf("meta 不是 JSON: %s", res.Outputs[0].Meta)
	}
	return res.Outputs[0].Value, meta
}

// 极速版：状态读响应头。完成 / 静音是成功；处理中与服务端错误按 503（可重试）；参数类按 400（不可重试）；
// HTTP 200 不代表成功。
func TestVolcASR_极速版的状态读响应头(t *testing.T) {
	p := volcASR{}
	ok := asrFixture(t, "volc_asr", "flash_success.json")
	cases := []struct {
		name       string
		code       string
		body       []byte
		wantText   string
		wantStatus int // 0 = 成功
	}{
		{"完成", "20000000", ok, "嗯，大家好，今天讲剪辑。", 0},
		{"静音按成功空结果", "20000003", asrFixture(t, "volc_asr", "silent.json"), "", 0},
		{"静音且正文为空对象", "20000003", asrFixture(t, "volc_asr", "empty.json"), "", 0},
		{"音频为空按静音", "45000002", []byte(`{}`), "", 0},
		{"参数错误不可重试", "45000001", []byte(`{}`), "", 400},
		{"格式错误不可重试", "45000151", []byte(`{}`), "", 400},
		{"服务端忙可重试", "55000031", []byte(`{}`), "", 503},
		{"同步接口回处理中也按可重试", "20000001", []byte(`{}`), "", 503},
		{"没有响应头但正文有结果", "", ok, "嗯，大家好，今天讲剪辑。", 0},
		{"没有响应头、正文带错误码", "", asrFixture(t, "volc_asr", "http200_error.json"), "", 400},
	}
	for _, c := range cases {
		res, err := p.ParseSubmitWithHeaders(200, volcHeader(c.code), c.body)
		if c.wantStatus != 0 {
			if statusOf(err) != c.wantStatus {
				t.Errorf("%s：应按 %d 分类，实际 %v", c.name, c.wantStatus, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s：不该失败: %v", c.name, err)
			continue
		}
		if text, _ := onlyText(t, res); text != c.wantText {
			t.Errorf("%s：文字 %q，期望 %q", c.name, text, c.wantText)
		}
	}
	// HTTP 非 2xx：照旧交给提交侧按状态码分类（不是 StatusError）。
	if _, err := p.ParseSubmitWithHeaders(500, volcHeader(""), []byte("oops")); err == nil || statusOf(err) != 0 {
		t.Fatalf("HTTP 500 应当是普通的 HTTP 错误: %v", err)
	}
}

// 标准版：提交只拿任务；查询处理中 / 排队是 pending，完成与静音成功，参数类终态失败，服务端错误让执行器下一轮再查。
func TestVolcASR_标准版提交与查询(t *testing.T) {
	p := volcASRStandard{}
	sub, err := p.ParseSubmitWithHeaders(200, volcHeader("20000000"), []byte(`{}`))
	if err != nil || !sub.Async || sub.TaskID == "" {
		t.Fatalf("提交应拿到异步任务: %+v %v", sub, err)
	}
	if _, err := p.ParseSubmitWithHeaders(200, volcHeader("45000001"), []byte(`{}`)); statusOf(err) != 400 {
		t.Fatalf("提交参数错误应 400: %v", err)
	}
	for _, code := range []string{"20000001", "20000002"} {
		q, err := p.ParseQueryWithHeaders(200, volcHeader(code), []byte(`{}`))
		if err != nil || q.State != StatePending {
			t.Fatalf("%s 应是处理中: %+v %v", code, q, err)
		}
	}
	q, err := p.ParseQueryWithHeaders(200, volcHeader("20000000"), asrFixture(t, "volc_asr", "flash_success.json"))
	if err != nil || q.State != StateSuccess {
		t.Fatalf("完成: %+v %v", q, err)
	}
	if text, meta := onlyText(t, q); text == "" || meta["granularity"] != "char" {
		t.Fatalf("完成要带字级结果: %q %v", text, meta)
	}
	if q, err := p.ParseQueryWithHeaders(200, volcHeader("20000003"), []byte(`{}`)); err != nil || q.State != StateSuccess {
		t.Fatalf("静音按成功: %+v %v", q, err)
	}
	if q, err := p.ParseQueryWithHeaders(200, volcHeader("45000151"), []byte(`{}`)); err != nil || q.State != StateFailed ||
		!strings.Contains(q.Message, "45000151") {
		t.Fatalf("参数类是终态失败并带错误码: %+v %v", q, err)
	}
	if _, err := p.ParseQueryWithHeaders(200, volcHeader("55000031"), []byte(`{}`)); err == nil {
		t.Fatal("服务端错误应返回 error（执行器下一轮再查）")
	}
}

func volcJob(value, mime string, params map[string]any) Job {
	if params == nil {
		params = map[string]any{}
	}
	return Job{OperationCode: "speech2text", ModelCode: "volc-asr", IdempotencyKey: "asr:31:abc:1",
		Media: []Media{{Kind: "audio", Order: 1, Value: value, MIME: mime}}, Params: params}
}

func TestVolcASR_请求体与请求头(t *testing.T) {
	data := "data:audio/mpeg;base64," + base64.StdEncoding.EncodeToString([]byte("ID3fake"))
	req, err := volcASR{}.BuildSubmit(volcJob(data, "audio/mpeg", map[string]any{ASRParamKeepFillers: true, ASRParamLanguage: "zh-CN"}))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]map[string]any
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["audio"]["data"] != base64.StdEncoding.EncodeToString([]byte("ID3fake")) || body["audio"]["format"] != "mp3" ||
		body["audio"]["url"] != nil {
		t.Fatalf("内联音频要去掉 data URI 前缀放进 audio.data: %+v", body["audio"])
	}
	r := body["request"]
	if r["model_name"] != "bigmodel" || r["show_utterances"] != true || r["enable_ddc"] != false || r["enable_punc"] != true {
		t.Fatalf("要句与字、保留口癖时不开顺滑: %+v", r)
	}
	if _, has := r["language"]; has {
		t.Fatal("极速版不发 language（文档没列，多给的字段可能被当成参数错误）")
	}
	if req.Path != volcFlashPath || req.Header["X-Api-Resource-Id"] != volcFlashRes || req.Header["X-Api-Sequence"] != "-1" {
		t.Fatalf("路径与头: %s %+v", req.Path, req.Header)
	}
	again, _ := volcASR{}.BuildSubmit(volcJob(data, "audio/mpeg", nil))
	if rid := req.Header["X-Api-Request-Id"]; len(rid) != 36 || rid != again.Header["X-Api-Request-Id"] {
		t.Fatalf("请求 ID 由幂等键算出、每次相同: %q %q", rid, again.Header["X-Api-Request-Id"])
	}
	var b2 map[string]map[string]any
	_ = json.Unmarshal(again.Body, &b2)
	if b2["request"]["enable_ddc"] != true {
		t.Fatal("不保留口癖（做字幕）时开顺滑")
	}

	// 标准版：只收地址，language 换成火山的写法；查询与提交同一个请求 ID、正文是 {}。
	job := volcJob("https://relay.example.com/a.mp3?sig=1", "audio/mpeg", map[string]any{ASRParamLanguage: "en"})
	job.UpstreamModel = "volc.seedasr.auc"
	sreq, err := volcASRStandard{}.BuildSubmit(job)
	if err != nil {
		t.Fatal(err)
	}
	var sb map[string]map[string]any
	_ = json.Unmarshal(sreq.Body, &sb)
	if sb["audio"]["url"] != "https://relay.example.com/a.mp3?sig=1" || sb["request"]["language"] != "en-US" ||
		sreq.Header["X-Api-Resource-Id"] != "volc.seedasr.auc" || sreq.Path != volcSubmitPath {
		t.Fatalf("标准版提交: %s %+v %+v", sreq.Path, sb, sreq.Header)
	}
	qreq, ok := volcASRStandard{}.BuildQuery(job, "whatever")
	if !ok || qreq.Path != volcQueryPath || string(qreq.Body) != "{}" ||
		qreq.Header["X-Api-Request-Id"] != sreq.Header["X-Api-Request-Id"] {
		t.Fatalf("查询要带同一个请求 ID: %+v", qreq)
	}
	if _, err := (volcASRStandard{}).BuildSubmit(volcJob(data, "audio/mpeg", nil)); err == nil {
		t.Fatal("标准版不收内联音频")
	}
	if !RequiresMediaURL(volcASRStandard{}) || RequiresMediaURL(volcASR{}) {
		t.Fatal("只有标准版只收地址")
	}
	if _, err := (volcASR{}).BuildSubmit(Job{}); err == nil {
		t.Fatal("没有音频要报错")
	}
}

func TestOpenAITranscribe_请求(t *testing.T) {
	data := "data:audio/mpeg;base64," + base64.StdEncoding.EncodeToString([]byte("ID3fake"))
	job := volcJob(data, "audio/mpeg", map[string]any{ASRParamLanguage: "zh-CN"})
	job.UpstreamModel = "whisper-1"
	req, err := openAITranscribe{}.BuildSubmit(job)
	if err != nil {
		t.Fatal(err)
	}
	if req.Path != openAITranscribePath || req.Fields["model"] != "whisper-1" || req.Fields["response_format"] != "verbose_json" ||
		req.Fields["language"] != "zh" || !reflect.DeepEqual(req.RepeatedFields["timestamp_granularities[]"], []string{"word", "segment"}) {
		t.Fatalf("whisper-1 要词级时间戳: %+v %+v", req.Fields, req.RepeatedFields)
	}
	if len(req.Files) != 1 || req.Files[0].Field != "file" || req.Files[0].Filename != "audio.mp3" || req.Files[0].Ref != data {
		t.Fatalf("文件位: %+v", req.Files)
	}
	job.UpstreamModel = "gpt-4o-transcribe"
	other, _ := openAITranscribe{}.BuildSubmit(job)
	if other.Fields["response_format"] != "json" || other.RepeatedFields != nil {
		t.Fatalf("其它模型只要整段文字: %+v", other.Fields)
	}
	big := "data:audio/mpeg;base64," + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, openAIUploadLimit+3))
	if _, err := (openAITranscribe{}).BuildSubmit(volcJob(big, "audio/mpeg", nil)); err == nil || !strings.Contains(err.Error(), "25 MB") {
		t.Fatalf("超过 25 MB 要在提交前拒: %v", err)
	}

	// 同名字段出现两次（multipart 的 map 表达不了，单独一组）。
	body, ct, err := EncodeMultipartWith(req.Fields, req.RepeatedFields, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, params, _ := mime.ParseMediaType(ct)
	form, err := multipart.NewReader(bytes.NewReader(body), params["boundary"]).ReadForm(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	if got := form.Value["timestamp_granularities[]"]; !reflect.DeepEqual(got, []string{"word", "segment"}) {
		t.Fatalf("重复字段: %v", got)
	}
}

func TestOpenAITranscribe_响应(t *testing.T) {
	p := openAITranscribe{}
	res, err := p.ParseSubmit(200, asrFixture(t, "openai_transcribe", "whisper_verbose.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, meta := onlyText(t, res); meta["granularity"] != "word" {
		t.Fatalf("whisper-1 是词级: %v", meta)
	}
	res, err = p.ParseSubmit(200, asrFixture(t, "openai_transcribe", "gpt4o_json.json"))
	if err != nil {
		t.Fatal(err)
	}
	if text, meta := onlyText(t, res); text != "大家好，今天讲剪辑。" || meta["granularity"] != "text" {
		t.Fatalf("其它模型只有文字: %q %v", text, meta)
	}
	if _, err := p.ParseSubmit(200, asrFixture(t, "openai_transcribe", "http200_error.json")); statusOf(err) != 400 {
		t.Fatalf("HTTP 200 带错误对象按 400: %v", err)
	}
	if _, err := p.ParseSubmit(400, []byte(`{"error":{"message":"bad"}}`)); err == nil || statusOf(err) != 0 {
		t.Fatalf("HTTP 4xx 交给提交侧分类: %v", err)
	}
}

// 共享 fixture（api/fixtures/smartcut/asr）：两家原始响应 → 统一的识别结果，期望值手算写死。
func TestASRFixtures(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "..", "api", "fixtures", "smartcut", "asr", "*.json"))
	if err != nil || len(files) < 5 {
		t.Fatalf("fixture 不全: %v %v", files, err)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var fx struct {
			Name     string            `json:"name"`
			Protocol string            `json:"protocol"`
			Phase    string            `json:"phase"`
			Status   int               `json:"status"`
			Headers  map[string]string `json:"headers"`
			Body     json.RawMessage   `json:"body"`
			Expect   struct {
				Text string          `json:"text"`
				Meta json.RawMessage `json:"meta"`
			} `json:"expect"`
		}
		if err := json.Unmarshal(raw, &fx); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		p, ok := Get(fx.Protocol)
		if !ok {
			t.Fatalf("%s：协议 %s 没注册", f, fx.Protocol)
		}
		h := http.Header{}
		for k, v := range fx.Headers {
			h.Set(k, v)
		}
		var res Result
		if fx.Phase == "query" {
			res, err = ParseQueryResponse(p, fx.Status, h, fx.Body)
		} else {
			res, err = ParseSubmitResponse(p, fx.Status, h, fx.Body)
		}
		if err != nil {
			t.Fatalf("%s（%s）：%v", fx.Name, f, err)
		}
		text, meta := onlyText(t, res)
		var want map[string]any
		_ = json.Unmarshal(fx.Expect.Meta, &want)
		if text != fx.Expect.Text || !reflect.DeepEqual(meta, want) {
			got, _ := json.Marshal(meta)
			t.Errorf("%s：\n 文字 %q / 期望 %q\n meta %s\n 期望 %s", fx.Name, text, fx.Expect.Text, got, fx.Expect.Meta)
		}
	}
}

func TestASRGranularity(t *testing.T) {
	cases := map[[2]string]string{
		{"volc_asr", ""}: GranularityChar,
		{"volc_asr_standard", "volc.seedasr.auc"}:  GranularityChar,
		{"openai_transcribe", "whisper-1"}:         GranularityWord,
		{"openai_transcribe", "gpt-4o-transcribe"}: GranularityText,
		{"openai_chat", "gpt-4o"}:                  "",
	}
	for in, want := range cases {
		if got := ASRGranularity(in[0], in[1]); got != want {
			t.Errorf("ASRGranularity(%q, %q) = %q，期望 %q", in[0], in[1], got, want)
		}
	}
}

// 上游的文字可能带 0x00：落库前清掉（PG 拒收，整条回滚、一路重试到死线）。
func TestASR_文字过SafeText(t *testing.T) {
	out := ASRResult{Text: "a\x00b", Granularity: GranularityChar,
		Utterances: []ASRUtterance{{Text: "a\x00b", Words: []ASRWord{{Text: "a\x00"}}}}}.output()
	if strings.ContainsRune(out.Value, 0) || bytes.Contains(out.Meta, []byte(`\u0000`)) {
		t.Fatalf("文字与 meta 里都不能有 0x00: %q %s", out.Value, out.Meta)
	}
}
