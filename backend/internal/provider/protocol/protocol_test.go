package protocol

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"strings"
	"testing"
)

// ⚠️ 这条是老系统栽过的坑：路径表里没有 `content`，
// 而 Seedance 的视频在 `content.video_url` —— 一个产物都扫不到，
// 落到「任务完成但未返回产物」，排查时看起来像上游的问题。
func TestExtractOutputs_覆盖各家的产物位置(t *testing.T) {
	cases := map[string]string{
		"OpenAI images data[].url": `{"data":[{"url":"https://cdn.x/a.png"}]}`,
		"顶层 outputs 数组":            `{"outputs":["https://cdn.x/b.mp4"]}`,
		"result.url":               `{"result":{"url":"https://cdn.x/c.jpg"}}`,
		// ⚠️ Seedance 在这里。
		"content.video_url":  `{"content":{"video_url":"https://cdn.x/d.mp4"}}`,
		"videos 数组":          `{"videos":[{"video_url":"https://cdn.x/e.mp4"}]}`,
		"顶层直接挂 url":          `{"url":"https://cdn.x/f.png"}`,
		"data 数组套 image_url": `{"data":[{"image_url":"https://cdn.x/g.png"}]}`,
	}
	for name, body := range cases {
		outs := ExtractOutputs(json.RawMessage(body))
		if len(outs) == 0 {
			t.Errorf("%s: 一个产物都没扫到 —— 这正是「任务完成但未返回产物」的成因", name)
			continue
		}
		if outs[0].Kind != "url" {
			t.Errorf("%s: 期望 url 形态，实际 %s", name, outs[0].Kind)
		}
		if !strings.HasPrefix(outs[0].Value, "https://cdn.x/") {
			t.Errorf("%s: 取错了值 %q", name, outs[0].Value)
		}
	}
}

func TestExtractOutputs_base64与dataURL(t *testing.T) {
	blob := strings.Repeat("A", 400)
	outs := ExtractOutputs(json.RawMessage(`{"data":[{"b64_json":"` + blob + `"}]}`))
	if len(outs) != 1 || outs[0].Kind != "base64" {
		t.Fatalf("b64_json 应识别为 base64，实际 %+v", outs)
	}

	outs = ExtractOutputs(json.RawMessage(`{"outputs":["data:image/webp;base64,` + blob + `"]}`))
	if len(outs) != 1 || outs[0].Kind != "base64" || outs[0].MIME != "image/webp" {
		t.Fatalf("data URL 应带出 MIME，实际 %+v", outs)
	}
}

// 短字符串也符合 base64 字符集，判低了会把状态值当成产物。
func TestExtractOutputs_不把状态值当产物(t *testing.T) {
	outs := ExtractOutputs(json.RawMessage(`{"result":{"data":"succeeded"}}`))
	for _, o := range outs {
		if o.Kind == "base64" {
			t.Fatalf("短字符串不该被当成 base64 产物：%+v", o)
		}
	}
}

// 很多上游同时给 data 与 images，扫全部路径会把同一个产物收两遍。
func TestExtractOutputs_不重复收取(t *testing.T) {
	body := `{"data":[{"url":"https://cdn.x/a.png"}],"images":[{"url":"https://cdn.x/a.png"}]}`
	outs := ExtractOutputs(json.RawMessage(body))
	if len(outs) != 1 {
		t.Fatalf("同一产物不该被收多遍，实际 %d 个", len(outs))
	}
}

func TestExtractOutputs_多产物(t *testing.T) {
	body := `{"data":[{"url":"https://cdn.x/1.png"},{"url":"https://cdn.x/2.png"},
	          {"url":"https://cdn.x/3.png"}]}`
	outs := ExtractOutputs(json.RawMessage(body))
	if len(outs) != 3 {
		t.Fatalf("四张图的模型一次出多个产物，应全部取到，实际 %d", len(outs))
	}
}

func TestExtractOutputs_MIME按后缀且忽略查询串(t *testing.T) {
	// 签名链接后面常挂一长串参数，不去掉的话后缀判断全错。
	outs := ExtractOutputs(json.RawMessage(
		`{"url":"https://cdn.x/a.mp4?X-Amz-Signature=abc&expires=123"}`))
	if len(outs) != 1 || outs[0].MIME != "video/mp4" {
		t.Fatalf("应按后缀判 MIME 且忽略查询串，实际 %+v", outs)
	}
}

func TestExtractTaskID(t *testing.T) {
	cases := map[string]string{
		`{"task_id":"t-1"}`:          "t-1",
		`{"id":"t-2"}`:               "t-2",
		`{"data":{"task_id":"t-3"}}`: "t-3",
		`{"request_id":"t-4"}`:       "t-4",
		`{"nothing":1}`:              "",
	}
	for body, want := range cases {
		if got := ExtractTaskID(json.RawMessage(body)); got != want {
			t.Errorf("%s: 期望 %q，实际 %q", body, want, got)
		}
	}
}

// ⚠️ 未知状态按 pending：上游加一个新状态值时，按失败处理会把正常任务全判失败。
func TestExtractState(t *testing.T) {
	success := []string{`{"status":"succeeded"}`, `{"status":"SUCCESS"}`,
		`{"state":"completed"}`, `{"data":{"status":"done"}}`}
	for _, b := range success {
		if got := ExtractState(json.RawMessage(b)); got != StateSuccess {
			t.Errorf("%s 应判成功，实际 %s", b, got)
		}
	}
	failed := []string{`{"status":"failed"}`, `{"status":"error"}`, `{"state":"cancelled"}`}
	for _, b := range failed {
		if got := ExtractState(json.RawMessage(b)); got != StateFailed {
			t.Errorf("%s 应判失败，实际 %s", b, got)
		}
	}
	pending := []string{
		`{"status":"queued"}`, `{"status":"running"}`, `{"status":"starting"}`,
		`{"status":"某个新状态"}`, // 上游新增的状态：按 pending，不能判失败
		`{}`, `not json`,
	}
	for _, b := range pending {
		if got := ExtractState(json.RawMessage(b)); got != StatePending {
			t.Errorf("%s 应判 pending（未知状态不判失败），实际 %s", b, got)
		}
	}
}

// ---------------------------------------------------------------- 各协议

func TestOpenAIChat_有图片时走content数组(t *testing.T) {
	p, _ := Get("openai_chat")
	req, err := p.BuildSubmit(Job{
		UpstreamModel: "gpt-4o", Prompt: "描述这张图",
		Images: []string{"https://x/a.png"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(req.Body, &body)
	msgs := body["messages"].([]any)
	content, ok := msgs[0].(map[string]any)["content"].([]any)
	if !ok {
		t.Fatal("有图片时 content 必须是数组 —— 只发文字的话模型会回「没有附带图片」")
	}
	var hasImage bool
	for _, part := range content {
		if part.(map[string]any)["type"] == "image_url" {
			hasImage = true
		}
	}
	if !hasImage {
		t.Fatal("图片没被发出去")
	}
}

func TestOpenAIChat_无图片时content是字符串(t *testing.T) {
	p, _ := Get("openai_chat")
	req, _ := p.BuildSubmit(Job{UpstreamModel: "gpt-4o", Prompt: "写首诗"})
	var body map[string]any
	_ = json.Unmarshal(req.Body, &body)
	msgs := body["messages"].([]any)
	if _, ok := msgs[0].(map[string]any)["content"].(string); !ok {
		t.Fatal("无图片时应是简单字符串形态")
	}
}

func TestOpenAIChat_options透传(t *testing.T) {
	p, _ := Get("openai_chat")
	req, _ := p.BuildSubmit(Job{
		UpstreamModel: "gpt-4o", Prompt: "x",
		Options: map[string]any{"temperature": 0.2, "max_tokens": float64(100)},
	})
	var body map[string]any
	_ = json.Unmarshal(req.Body, &body)
	if body["temperature"] != 0.2 || body["max_tokens"] != float64(100) {
		t.Fatalf("options 应原样透传，实际 %v", body)
	}
}

func TestOpenAIChat_解析响应(t *testing.T) {
	p, _ := Get("openai_chat")
	res, err := p.ParseSubmit(200, []byte(
		`{"choices":[{"message":{"role":"assistant","content":"一只猫"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Outputs) != 1 || res.Outputs[0].Value != "一只猫" {
		t.Fatalf("文本产物解析不对: %+v", res.Outputs)
	}

	// 4xx 要带回响应体，否则排查时只知道"失败了"。
	_, err = p.ParseSubmit(429, []byte(`{"error":{"message":"rate limit"}}`))
	if err == nil || !strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("错误应带回上游原文，实际 %v", err)
	}
}

func TestParseSSE_半包与结束标记(t *testing.T) {
	var buf strings.Builder
	var got strings.Builder
	onDelta := func(s string) { got.WriteString(s) }

	// 一个 data 行被切成两半到达。
	done := ParseSSE(&buf, []byte(`data: {"choices":[{"delta":{"content":"一"`), onDelta)
	if done || got.String() != "" {
		t.Fatal("半包不该产出内容")
	}
	done = ParseSSE(&buf, []byte("}}]}\n"), onDelta)
	if done || got.String() != "一" {
		t.Fatalf("补齐后应产出「一」，实际 %q", got.String())
	}

	// 心跳与注释行不该中断流。
	ParseSSE(&buf, []byte(": ping\n\n"), onDelta)
	ParseSSE(&buf, []byte(`data: {"choices":[{"delta":{"content":"只猫"}}]}`+"\n"), onDelta)
	if got.String() != "一只猫" {
		t.Fatalf("应累积为「一只猫」，实际 %q", got.String())
	}

	if done = ParseSSE(&buf, []byte("data: [DONE]\n"), onDelta); !done {
		t.Fatal("应识别结束标记")
	}
}

// 单条事件解析失败不能中断整个流。
func TestParseSSE_坏事件被跳过(t *testing.T) {
	var buf, got strings.Builder
	ParseSSE(&buf, []byte("data: {坏的\ndata: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n"),
		func(s string) { got.WriteString(s) })
	if got.String() != "ok" {
		t.Fatalf("坏事件应被跳过而不是中断，实际 %q", got.String())
	}
}

func TestOpenAIImages_请求与解析(t *testing.T) {
	p, _ := Get("openai_images")
	req, _ := p.BuildSubmit(Job{
		UpstreamModel: "dall-e-3", Prompt: "猫",
		Params: map[string]any{"count": "2", "aspect_ratio": "16:9"},
	})
	var body map[string]any
	_ = json.Unmarshal(req.Body, &body)
	// 参数定义里的默认值是 varchar，会以字符串形态传进来。
	if body["n"] != float64(2) {
		t.Errorf("字符串形态的数量应被解析，实际 %v", body["n"])
	}
	// gpt-image 系列的尺寸（照老系统）：16:9 折成横版 1536x1024，不是 DALL·E 3 的 1792x1024。
	if body["size"] != "1536x1024" {
		t.Errorf("画幅应翻成尺寸，实际 %v", body["size"])
	}
	// 没选画质就不发，让上游用默认值
	if _, ok := body["quality"]; ok {
		t.Errorf("没选画质时不该发 quality，实际 %v", body["quality"])
	}

	res, _ := p.ParseSubmit(200, []byte(
		`{"data":[{"url":"https://cdn/1.png"},{"url":"https://cdn/2.png"}]}`))
	if len(res.Outputs) != 2 {
		t.Fatalf("应解析出 2 个产物，实际 %d", len(res.Outputs))
	}
}

// ⚠️ TTS 回的是裸字节：按 JSON 解析会失败，而失败信息指不到真正原因。
func TestOpenAITTS_裸字节响应(t *testing.T) {
	p, _ := Get("openai_tts")
	raw := []byte{0xFF, 0xFB, 0x90, 0x00, 0x01, 0x02}
	res, err := p.ParseSubmit(200, raw)
	if err != nil {
		t.Fatalf("裸字节不该按 JSON 解析: %v", err)
	}
	if len(res.Outputs) != 1 || res.Outputs[0].Kind != "base64" {
		t.Fatalf("应转成 base64 产物，实际 %+v", res.Outputs)
	}
	decoded, err := base64.StdEncoding.DecodeString(res.Outputs[0].Value)
	if err != nil || string(decoded) != string(raw) {
		t.Fatal("字节内容应无损")
	}
	if res.Outputs[0].MIME != "audio/mpeg" {
		t.Errorf("应带上 MIME，实际 %q", res.Outputs[0].MIME)
	}
}

// Anthropic 的形态与 OpenAI 完全不同 —— 这正是"换一家厂商就要加协议"的原因。
func TestAnthropic_图片在文本前且用source结构(t *testing.T) {
	p, _ := Get("anthropic_messages")
	blob := strings.Repeat("A", 100)
	req, _ := p.BuildSubmit(Job{
		UpstreamModel: "claude-x", Prompt: "描述",
		Images: []string{"data:image/png;base64," + blob},
	})
	var body map[string]any
	_ = json.Unmarshal(req.Body, &body)
	content := body["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if content[0].(map[string]any)["type"] != "image" {
		t.Fatal("图片应排在文本之前")
	}
	src := content[0].(map[string]any)["source"].(map[string]any)
	if src["type"] != "base64" || src["media_type"] != "image/png" {
		t.Fatalf("应使用 source 结构，实际 %v", src)
	}
	// max_tokens 在 Anthropic 是必填，缺了直接 400。
	if body["max_tokens"] == nil {
		t.Fatal("max_tokens 必须有默认值")
	}
	if req.Header["anthropic-version"] == "" {
		t.Fatal("版本头必填，漏了回 400")
	}
}

func TestAnthropic_解析content块(t *testing.T) {
	p, _ := Get("anthropic_messages")
	res, err := p.ParseSubmit(200, []byte(
		`{"content":[{"type":"text","text":"前半"},{"type":"text","text":"后半"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs[0].Value != "前半后半" {
		t.Fatalf("多个 content 块应拼接，实际 %q", res.Outputs[0].Value)
	}
}

func TestGenericAsync_提交与轮询(t *testing.T) {
	p, _ := Get("generic_async")
	if !p.Async() || !p.RequiresQueryPath() {
		t.Fatal("异步协议必须标记为异步且要求查询路径 —— 不强制的话跑起来才发现没法轮询")
	}
	res, err := p.ParseSubmit(200, []byte(`{"task_id":"up-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Async || res.TaskID != "up-1" {
		t.Fatalf("应回带任务 ID，实际 %+v", res)
	}

	// 上游其实同步返回了产物：也要能接住，不能因为"没有 task id"就判失败。
	res, _ = p.ParseSubmit(200, []byte(`{"data":[{"url":"https://cdn/a.png"}]}`))
	if len(res.Outputs) != 1 {
		t.Fatalf("同步返回的产物也要接住，实际 %+v", res)
	}

	// 轮询：成功但没有产物要判失败并说清楚。
	res, _ = p.ParseQuery(200, []byte(`{"status":"succeeded"}`))
	if res.State != StateFailed || !strings.Contains(res.Message, "产物路径") {
		t.Fatalf("成功但无产物应判失败并指明原因，实际 %+v", res)
	}

	res, _ = p.ParseQuery(200, []byte(`{"status":"failed","message":"上游拒绝"}`))
	if res.State != StateFailed || res.Message != "上游拒绝" {
		t.Fatalf("应带回失败原因，实际 %+v", res)
	}
}

func TestGenericSync_成功但无产物要说清楚(t *testing.T) {
	p, _ := Get("generic_sync")
	res, err := p.ParseSubmit(200, []byte(`{"status":"ok"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Outputs) != 0 || !strings.Contains(res.Message, "产物路径") {
		t.Fatalf("应提示去核对产物路径，而不是只说「未返回产物」，实际 %+v", res)
	}
}

// generic 发的是本平台自有信封，不是任意厂商的原生 API。
func TestGeneric_信封形状(t *testing.T) {
	p, _ := Get("generic_sync")
	req, _ := p.BuildSubmit(Job{
		OperationCode: "text2img", UpstreamModel: "m1", ResultType: "image",
		IdempotencyKey: "k1", Prompt: "猫", Params: map[string]any{"size": "1:1"},
	})
	var env map[string]any
	_ = json.Unmarshal(req.Body, &env)
	for _, k := range []string{"operation", "model", "resultType", "idempotencyKey", "prompt", "params"} {
		if _, ok := env[k]; !ok {
			t.Errorf("信封缺字段 %s", k)
		}
	}
}

// 注册表是「代码里有的协议」的唯一事实来源，启动时同步进表（GAP-09）。
func TestAll_注册表完整(t *testing.T) {
	got := map[string]Descriptor{}
	for _, d := range All() {
		got[d.Code] = d
	}
	for _, code := range []string{
		"dev_fake", "openai_chat", "anthropic_messages", "openai_images",
		"openai_tts", "generic_sync", "generic_async",
	} {
		if _, ok := got[code]; !ok {
			t.Errorf("协议 %s 没注册 —— 后台下拉里就会少一个，而代码其实支持", code)
		}
	}
	if !got["generic_async"].Async || !got["generic_async"].RequiresQueryPath {
		t.Error("generic_async 的异步标记不对")
	}
	if got["openai_chat"].Async {
		t.Error("openai_chat 不是异步协议")
	}
	// 排序稳定：下拉顺序不该每次刷新都变。
	list := All()
	for i := 1; i < len(list); i++ {
		if list[i-1].Code > list[i].Code {
			t.Fatal("协议列表应按码排序")
		}
	}
}

func TestOpenAIImages_有参考图时走编辑接口的multipart(t *testing.T) {
	// ⚠️ 这条钉住的是："把图塞进 /images/generations 的 JSON body 里"
	// 上游会直接忽略 —— 出图成功但和参考图毫无关系，用户只会觉得模型不听话。
	req, err := mustGet(t, "openai_images").BuildSubmit(Job{
		OperationCode: "inpaint", UpstreamModel: "gpt-image-1", Prompt: "换成夜景",
		Images: []string{"https://cdn.x/a.png", "https://cdn.x/mask.png"},
		Params: map[string]any{"count": 2, "aspect_ratio": "16:9"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.Path != "/images/edits" {
		t.Fatalf("有参考图应走编辑接口，得到 %s", req.Path)
	}
	if !req.IsMultipart() {
		t.Fatal("编辑接口必须走 multipart")
	}
	if len(req.Body) != 0 {
		t.Fatal("multipart 请求的 Body 应由调用方取到字节后再拼")
	}
	if req.Fields["prompt"] != "换成夜景" || req.Fields["n"] != "2" ||
		req.Fields["size"] != "1536x1024" {
		t.Fatalf("表单字段不对: %+v", req.Fields)
	}
	if len(req.Files) != 2 || req.Files[0].Field != "image" || req.Files[1].Field != "mask" {
		t.Fatalf("文件位不对: %+v", req.Files)
	}
	if req.Files[0].Ref != "https://cdn.x/a.png" {
		t.Fatal("文件位要带上原始引用，供调用方去取字节")
	}
}

// CD-80：分镜图要同时参照这一镜的角色、场景、道具 —— 每张参考图一个 `image` 字段，一张都不能丢。
// 此前只发第一张、第二张当蒙版：带着五六张资产图的分镜图会把第二个角色的设定图当成「要重绘的区域」。
func TestOpenAIImages_多张参考图逐张发出且不当成蒙版(t *testing.T) {
	refs := []string{"https://cdn.x/1.png", "https://cdn.x/2.png", "https://cdn.x/3.png",
		"https://cdn.x/4.png", "https://cdn.x/5.png", "https://cdn.x/6.png"}
	media := make([]Media, 0, len(refs))
	for i, r := range refs {
		media = append(media, Media{Kind: "image", Order: i + 1, Value: r})
	}
	req, err := mustGet(t, "openai_images").BuildSubmit(Job{
		OperationCode: "img2img", UpstreamModel: "gpt-image-2", Prompt: "第 1 镜",
		Images: refs, Media: media, Params: map[string]any{"aspect_ratio": "16:9"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Files) != len(refs) {
		t.Fatalf("应发出 %d 张参考图，实际 %d 个文件位: %+v", len(refs), len(req.Files), req.Files)
	}
	for i, f := range req.Files {
		if f.Field != "image" || f.Ref != refs[i] {
			t.Fatalf("第 %d 个文件位应是第 %d 张参考图（字段 image、顺序不变），得到 %+v", i+1, i+1, f)
		}
	}
	if req.Fields["input_fidelity"] != "high" {
		t.Fatalf("参考图式编辑默认要保住参考图的身份与细节: %+v", req.Fields)
	}
}

// 蒙版认显式角色，不按位置猜；有蒙版时只发第一张参考图（蒙版说的是「这张图的哪一块要改」）。
func TestOpenAIImages_蒙版按角色识别(t *testing.T) {
	req, err := mustGet(t, "openai_images").BuildSubmit(Job{
		OperationCode: "inpaint", UpstreamModel: "gpt-image-1", Prompt: "把天空换成晚霞",
		Images: []string{"https://cdn.x/src.png", "https://cdn.x/mask.png", "https://cdn.x/extra.png"},
		Media: []Media{
			{Kind: "image", Order: 1, Value: "https://cdn.x/src.png"},
			{Kind: "image", Order: 2, Value: "https://cdn.x/mask.png", Role: "mask"},
			{Kind: "image", Order: 3, Value: "https://cdn.x/extra.png"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Files) != 2 || req.Files[0].Ref != "https://cdn.x/src.png" ||
		req.Files[1].Field != "mask" || req.Files[1].Ref != "https://cdn.x/mask.png" {
		t.Fatalf("应是原图 + 蒙版两个文件位: %+v", req.Files)
	}
	if _, ok := req.Fields["input_fidelity"]; ok {
		t.Fatal("涂抹重绘不该带参考图式编辑的 input_fidelity")
	}
}

func TestOpenAIImages_无参考图仍走生成接口(t *testing.T) {
	req, err := mustGet(t, "openai_images").BuildSubmit(Job{
		UpstreamModel: "gpt-image-1", Prompt: "一只猫",
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.Path != "/images/generations" || req.IsMultipart() {
		t.Fatalf("无参考图应走 JSON 生成接口，得到 %s multipart=%v", req.Path, req.IsMultipart())
	}
}

func TestEncodeMultipart_可复现且带boundary(t *testing.T) {
	fields := map[string]string{"b": "2", "a": "1", "c": "3"}
	files := []MaterializedFile{{Field: "image", Filename: "x.png",
		ContentType: "image/png", Data: []byte("PNGDATA")}}

	body1, ct1, err := EncodeMultipart(fields, files)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ct1, "multipart/form-data; boundary=") {
		t.Fatalf("Content-Type 必须带 boundary，得到 %q", ct1)
	}

	// ⚠️ 字段顺序必须稳定：map 遍历每次都不同，不定序的话
	// 请求日志与 fixture 测试都不可复现。
	got := fieldOrder(string(body1))
	if strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("字段应按名字排序，得到 %v", got)
	}
	if !strings.Contains(string(body1), "PNGDATA") {
		t.Fatal("文件内容没有写进去")
	}
	if !strings.Contains(string(body1), `filename="x.png"`) {
		t.Fatal("文件名没有写进去")
	}

	// 解析一遍确认上游真能解出来。
	boundary := ct1[strings.Index(ct1, "boundary=")+len("boundary="):]
	r := multipart.NewReader(bytes.NewReader(body1), boundary)
	form, err := r.ReadForm(1 << 20)
	if err != nil {
		t.Fatalf("上游解不出这个 multipart: %v", err)
	}
	if form.Value["a"][0] != "1" || len(form.File["image"]) != 1 {
		t.Fatalf("解出来的表单不对: %+v %+v", form.Value, form.File)
	}
}

// CD-82：文件位要带真实的图片类型。
//
// ⚠️ 起因：`CreateFormFile` 把每个文件位写成 application/octet-stream。多张参考图时中转把它们转成
// `data:` 地址、要求图片类型，回 400「unsupported MIME type 'application/octet-stream'」——
// 分镜图组一整批全部失败，而参考图其实都带上了。
func TestEncodeMultipart_文件位带真实图片类型(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	jpeg := []byte("\xFF\xD8\xFF\xE0\x00\x10JFIF\x00")
	webp := []byte("RIFF\x24\x00\x00\x00WEBPVP8 ")
	files := []MaterializedFile{
		// 对象存储下载回来声明成 octet-stream：以字节为准
		{Field: "image", Filename: "reference-1.png", ContentType: "application/octet-stream", Data: png},
		// 声明错了（data: 地址写的 png，其实是 jpeg）：以字节为准，扩展名跟着改
		{Field: "image", Filename: "reference-2.png", ContentType: "image/png", Data: jpeg},
		{Field: "image", Filename: "reference-3.png", ContentType: "", Data: webp},
		// 嗅不出图片时用声明的类型
		{Field: "mask", Filename: "mask.png", ContentType: "image/png", Data: []byte("不是图片的字节")},
	}
	body, ct, err := EncodeMultipart(map[string]string{"prompt": "p"}, files)
	if err != nil {
		t.Fatal(err)
	}
	boundary := ct[strings.Index(ct, "boundary=")+len("boundary="):]
	form, err := multipart.NewReader(bytes.NewReader(body), boundary).ReadForm(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	images := form.File["image"]
	if len(images) != 3 {
		t.Fatalf("应有 3 张参考图，得到 %d", len(images))
	}
	want := []struct{ ct, name string }{
		{"image/png", "reference-1.png"}, {"image/jpeg", "reference-2.jpg"}, {"image/webp", "reference-3.webp"},
	}
	for i, w := range want {
		if got := images[i].Header.Get("Content-Type"); got != w.ct {
			t.Fatalf("第 %d 张的类型应为 %s，得到 %s", i+1, w.ct, got)
		}
		if images[i].Filename != w.name {
			t.Fatalf("第 %d 张的文件名应为 %s，得到 %s", i+1, w.name, images[i].Filename)
		}
	}
	if got := form.File["mask"][0].Header.Get("Content-Type"); got != "image/png" {
		t.Fatalf("嗅不出图片时应用声明的类型，得到 %s", got)
	}
	if strings.Contains(string(body), "application/octet-stream") {
		t.Fatal("请求体里不该再出现 application/octet-stream")
	}
}

func fieldOrder(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\r\n") {
		const marker = `Content-Disposition: form-data; name="`
		if !strings.HasPrefix(line, marker) {
			continue
		}
		rest := line[len(marker):]
		name := rest[:strings.Index(rest, `"`)]
		if strings.Contains(rest, "filename=") {
			continue
		}
		out = append(out, name)
	}
	return out
}

func mustGet(t *testing.T, code string) Protocol {
	t.Helper()
	p, ok := Get(code)
	if !ok {
		t.Fatalf("协议 %s 未注册", code)
	}
	return p
}

func TestOpenAIImages_有参考图时改写提交地址(t *testing.T) {
	// ⚠️ 联调矩阵抓到的真 bug：BuildSubmit 把 Path 设成 /images/edits，
	// 但 app 层优先用配置里的 submitPath（协议校验又要求它必填），
	// 于是 multipart 被发到 /images/generations —— 上游解不出文件、
	// 出一张与参考图毫无关系的图，**而任务是成功的**。
	p := mustGet(t, "openai_images")
	withImage := Job{UpstreamModel: "gpt-image-1", Images: []string{"https://cdn.x/a.png"}}
	got, err := ResolveSubmitPath(p, withImage, "/images/generations")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/images/edits" {
		t.Fatalf("有参考图应改写到编辑接口，得到 %q", got)
	}
	// 没有参考图时不动。
	if got, _ := ResolveSubmitPath(p, Job{}, "/images/generations"); got != "/images/generations" {
		t.Fatalf("无参考图不该改写，得到 %q", got)
	}
	// 配置指向自建中转时按配置来，不替运营做决定。
	if got, _ := ResolveSubmitPath(p, withImage, "/v1/custom/edit"); got != "/v1/custom/edit" {
		t.Fatalf("自定义地址不该被改写，得到 %q", got)
	}
}

// 比例 → 尺寸、画质 → 上游取值，都照老系统（`ConfigurableHttpProviderAdapter`）。
func TestOpenAIImages_比例与画质照老系统(t *testing.T) {
	sizes := []struct{ ratio, want string }{
		{"1:1", "1024x1024"}, {"16:9", "1536x1024"}, {"9:16", "1024x1536"},
		{"4:3", "1536x1024"}, {"3:4", "1024x1536"}, {"5:4", "1536x1024"}, {"4:5", "1024x1536"},
		{"21:9", "1536x1024"}, {"9:21", "1024x1536"}, {"3:2", "1536x1024"}, {"2:3", "1024x1536"},
		{"1:2", "1024x1536"}, {"2:1", "1536x1024"},
		// 自适应与解析不了的交给上游 auto，而不是悄悄落成方形
		{"adaptive", "auto"}, {"", "auto"}, {"abc", "auto"}, {"0:9", "auto"},
	}
	for _, c := range sizes {
		if got := sizeFromAspect(c.ratio); got != c.want {
			t.Errorf("比例 %q 应折成 %s，得到 %s", c.ratio, c.want, got)
		}
	}
	qualities := []struct{ in, want string }{
		{"standard", "medium"}, {"low", "low"}, {"high", "high"}, {"HIGH", "high"},
		{"", ""}, {"ultra", ""}, // 认不出的不发：发一个上游不认的枚举会让整次生成被拒
	}
	for _, c := range qualities {
		if got := imageQuality(c.in); got != c.want {
			t.Errorf("画质 %q 应为 %q，得到 %q", c.in, c.want, got)
		}
	}

	p, _ := Get("openai_images")
	req, _ := p.BuildSubmit(Job{UpstreamModel: "gpt-image-2", Prompt: "猫",
		Params: map[string]any{"aspect_ratio": "9:16", "quality": "standard"}})
	var body map[string]any
	_ = json.Unmarshal(req.Body, &body)
	if body["size"] != "1024x1536" || body["quality"] != "medium" {
		t.Fatalf("文生图应带竖版尺寸与 medium 画质，得到 size=%v quality=%v", body["size"], body["quality"])
	}
	edit, _ := p.BuildSubmit(Job{UpstreamModel: "gpt-image-2", Prompt: "换成夜景",
		Images: []string{"https://cdn.x/a.png"}, Params: map[string]any{"quality": "high"}})
	if edit.Fields["quality"] != "high" {
		t.Fatalf("图生图（编辑接口）也要带画质，得到 %+v", edit.Fields)
	}
}

// 内容审核拒绝给一句看得懂的话（docs/agent/imagetools，用户 2026-10-31 截图）：不再把整段上游 JSON 甩给用户。
func TestHTTPError_内容审核拒绝说人话(t *testing.T) {
	body := []byte(`{"error":{"code":"upstream_text_reply","message":"非常抱歉，该提示可能违反了我们的内容政策。如果你认为此判断有误，请重试或修改提示语。","type":"invalid_request_error"}}`)
	err := httpError(400, body)
	if !strings.Contains(err.Error(), "上游内容审核未通过") || !strings.Contains(err.Error(), "违反了我们的内容政策") ||
		strings.Contains(err.Error(), "invalid_request_error") {
		t.Fatalf("文案：%v", err)
	}
	if e := httpError(500, []byte(`{"error":{"message":"server busy"}}`)); strings.Contains(e.Error(), "内容审核") {
		t.Fatalf("普通错误不改：%v", e)
	}
}
