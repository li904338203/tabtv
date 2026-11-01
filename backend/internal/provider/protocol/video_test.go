package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// videoJob 造一个视频工作项。media 形如 "image:https://a", "video:https://b"。
func videoJob(op string, params map[string]any, media ...string) Job {
	if params == nil {
		params = map[string]any{}
	}
	job := Job{
		OperationCode: op, UpstreamModel: "model-v1", ResultType: "video",
		IdempotencyKey: "idem-1", Prompt: "一只狐狸在雪地里奔跑", Params: params,
	}
	for i, m := range media {
		kind, value, _ := strings.Cut(m, ":")
		job.Media = append(job.Media, Media{Kind: kind, Order: i + 1, Value: value})
		if kind == "image" {
			job.Images = append(job.Images, value)
		}
	}
	return job
}

func buildBody(t *testing.T, code string, job Job) map[string]any {
	t.Helper()
	req, err := mustGet(t, code).BuildSubmit(job)
	if err != nil {
		t.Fatalf("构造 %s 请求失败: %v", code, err)
	}
	var body map[string]any
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatalf("请求体不是 JSON: %v", err)
	}
	return body
}

func buildErr(t *testing.T, code string, job Job) string {
	t.Helper()
	_, err := mustGet(t, code).BuildSubmit(job)
	if err == nil {
		t.Fatalf("%s 本应拒绝这个组合", code)
	}
	return err.Error()
}

// ---------------------------------------------------------------- 四家形状不同

func TestVideo_四家的请求体形状互不相同(t *testing.T) {
	// ⚠️ 这条测试的意义不是断言某个字段，而是钉住「没有公共子集」这件事：
	// 谁要是想把四家合并成一个 generic 协议，这里会立刻红。
	job := videoJob("img2video", nil, "image:https://cdn.x/a.png")
	job.Params[SubmitPathHintKey] = "/v1/videos/image-to-video/generations"

	kling := buildBody(t, "kling_video", job)
	if _, ok := kling["contents"]; !ok {
		t.Fatal("可灵用 contents 数组")
	}
	if _, ok := kling["settings"]; !ok {
		t.Fatal("可灵的规格参数在 settings 里")
	}

	seedance := buildBody(t, "seedance_video", job)
	if _, ok := seedance["content"]; !ok {
		// ⚠️ 是 content 不是 contents —— 抄错一个字母上游就整包读不到输入。
		t.Fatal("Seedance 用 content 数组")
	}
	if seedance["resolution"] == nil || seedance["duration"] == nil {
		t.Fatal("Seedance 的规格参数在顶层")
	}

	bblabu := buildBody(t, "bblabu_video", job)
	if _, ok := bblabu["referenceImages"]; !ok {
		t.Fatal("中转用平铺的 referenceImages 数组")
	}

	grok := buildBody(t, "grok_video", job)
	img, ok := grok["image"].(map[string]any)
	if !ok || img["image_url"] != "https://cdn.x/a.png" {
		t.Fatalf("Grok 的图片是 {\"image\":{\"image_url\":...}} 嵌套对象: %+v", grok["image"])
	}
}

// ---------------------------------------------------------------- 可灵

func TestKling_首尾帧是两个不同角色(t *testing.T) {
	// ⚠️ 顺序即语义：第一张是首帧、第二张是尾帧。丢了顺序用户会拿到
	// 一段首尾颠倒的过渡视频，而且看不出哪里不对。
	job := videoJob("first_last_frame", map[string]any{
		SubmitPathHintKey: "/v1/videos/image-to-video/generations",
	}, "image:https://cdn.x/first.png", "image:https://cdn.x/last.png")

	body := buildBody(t, "kling_video", job)
	contents, _ := body["contents"].([]any)
	types := contentTypes(contents)
	if strings.Join(types, ",") != "prompt,first_frame,last_frame" {
		t.Fatalf("角色或顺序不对: %v", types)
	}
	if urlAt(contents, 1) != "https://cdn.x/first.png" || urlAt(contents, 2) != "https://cdn.x/last.png" {
		t.Fatal("首尾帧对应的地址错位了")
	}
}

func TestKling_omni参考图与首帧分开(t *testing.T) {
	// 脚本线要「分镜图当首帧 + 资产图当参考图」：首帧锁起始画面，参考图保识别特征。
	job := videoJob("ref2video", map[string]any{
		SubmitPathHintKey: "/v1/videos/omni-video/generations",
		"firstFrameInput": true,
	}, "image:https://cdn.x/shot.png", "image:https://cdn.x/role.png", "video:https://cdn.x/ref.mp4")

	contents, _ := buildBody(t, "kling_video", job)["contents"].([]any)
	types := contentTypes(contents)
	if strings.Join(types, ",") != "prompt,first_frame,refer_image,feature_video" {
		t.Fatalf("角色不对: %v", types)
	}
	// 参考图与参考视频各自从 1 开始编号，提示词里可以按编号引用。
	if idAt(contents, 2) != "image_1" || idAt(contents, 3) != "video_1" {
		t.Fatalf("参考编号不对: %v %v", idAt(contents, 2), idAt(contents, 3))
	}
}

func TestKling_画布上的ref2video全部按参考图发(t *testing.T) {
	// 没有 firstFrameInput 标记时不该凭空把第一张图当首帧 ——
	// 画布上的 ref2video 没有首帧概念。
	job := videoJob("ref2video", map[string]any{
		SubmitPathHintKey: "/v1/videos/omni-video/generations",
	}, "image:https://cdn.x/a.png", "image:https://cdn.x/b.png")

	contents, _ := buildBody(t, "kling_video", job)["contents"].([]any)
	if strings.Join(contentTypes(contents), ",") != "prompt,refer_image,refer_image" {
		t.Fatalf("不该出现 first_frame: %v", contentTypes(contents))
	}
}

func TestKling_提交地址按操作改写(t *testing.T) {
	// ⚠️ 同一个可灵模型，文生与图生是两个端点。不改写的话图生视频会打到
	// 文生端点，上游忽略图片、出一段无关的视频 —— 而任务是"成功"的。
	k := mustGet(t, "kling_video")
	cases := []struct{ op, configured, want string }{
		{"text2video", "/v1/videos/image-to-video/generations", "/v1/videos/text-to-video/generations"},
		{"img2video", "/v1/videos/text-to-video/generations", "/v1/videos/image-to-video/generations"},
		{"first_last_frame", "/v1/videos/text-to-video/generations", "/v1/videos/image-to-video/generations"},
		{"ref2video", "/v1/videos/omni-video/generations", "/v1/videos/omni-video/generations"},
	}
	for _, c := range cases {
		got, err := ResolveSubmitPath(k, videoJob(c.op, nil), c.configured)
		if err != nil {
			t.Fatalf("%s: %v", c.op, err)
		}
		if got != c.want {
			t.Errorf("%s: 得到 %q，期望 %q", c.op, got, c.want)
		}
	}
	// 非 omni 端点不支持 ref2video。
	if _, err := ResolveSubmitPath(k, videoJob("ref2video", nil),
		"/v1/videos/text-to-video/generations"); err == nil {
		t.Fatal("非 omni 端点的 ref2video 应被拒")
	}
	// 配置里没有可改写的片段时要报出来，不能照原样发。
	if _, err := ResolveSubmitPath(k, videoJob("img2video", nil), "/v1/generate"); err == nil {
		t.Fatal("无法改写的 submitPath 应被拒")
	}
}

func TestKling_业务错误码在HTTP200里(t *testing.T) {
	// ⚠️ 只看 http_status 判不出失败。不检查 code 的话，一次失败会被当成
	// "没有任务 ID"，错误信息完全指不到真正的原因。
	_, err := mustGet(t, "kling_video").ParseSubmit(200,
		[]byte(`{"code":1103,"message":"账户余额不足"}`))
	if err == nil || !strings.Contains(err.Error(), "1103") ||
		!strings.Contains(err.Error(), "余额不足") {
		t.Fatalf("要带出上游的错误码与说法: %v", err)
	}
}

func TestKling_轮询解析(t *testing.T) {
	k := mustGet(t, "kling_video")
	sub, err := k.ParseSubmit(200, []byte(`{"code":0,"data":{"id":"kt-1","status":"submitted"}}`))
	if err != nil || !sub.Async || sub.TaskID != "kt-1" {
		t.Fatalf("提交应拿到任务 ID: %+v %v", sub, err)
	}
	running, err := k.ParseQuery(200, []byte(`{"code":0,"data":{"id":"kt-1","status":"processing"}}`))
	if err != nil || running.State != StatePending {
		t.Fatalf("处理中: %+v %v", running, err)
	}
	// data 有时是数组（批量接口复用）。
	done, err := k.ParseQuery(200, []byte(
		`{"code":0,"data":[{"status":"succeed","video":{"url":"https://cdn.x/o.mp4"}}]}`))
	if err != nil || done.State != StateSuccess || len(done.Outputs) != 1 {
		t.Fatalf("成功应带产物: %+v %v", done, err)
	}
	failed, err := k.ParseQuery(200,
		[]byte(`{"code":0,"data":{"status":"failed","message":"内容审核未通过"}}`))
	if err != nil || failed.State != StateFailed || failed.Message != "内容审核未通过" {
		t.Fatalf("失败要带上上游说法: %+v %v", failed, err)
	}
}

// ---------------------------------------------------------------- Seedance

func TestSeedance_规格参数走顶层而不是拼进提示词(t *testing.T) {
	// ⚠️ 绝不退回「提示词 --resolution 720p --duration 5」那种写法：
	// 模型会把这段当创作描述读，画面里经常真的出现参数文字。
	body := buildBody(t, "seedance_video", videoJob("text2video", map[string]any{
		"resolution": "1080p", "aspect_ratio": "9:16", "duration": 8,
	}))
	if body["resolution"] != "1080p" || body["ratio"] != "9:16" || body["duration"] != float64(8) {
		t.Fatalf("规格参数应在顶层: %+v", body)
	}
	content, _ := body["content"].([]any)
	text, _ := content[0].(map[string]any)
	if strings.Contains(text["text"].(string), "--") {
		t.Fatalf("参数不该出现在提示词里: %v", text["text"])
	}
}

func TestSeedance_adaptive原样透传(t *testing.T) {
	// Seedance 认 adaptive（跟随参考图），不像可灵要折成 16:9。
	// 折了的话带竖图的参考生成会被强行拉成横屏。
	body := buildBody(t, "seedance_video", videoJob("ref2video",
		map[string]any{"aspect_ratio": "adaptive"}, "image:https://cdn.x/a.png"))
	if body["ratio"] != "adaptive" {
		t.Fatalf("adaptive 应原样透传: %v", body["ratio"])
	}
}

func TestSeedance_拒绝音频参考且说清楚(t *testing.T) {
	// ⚠️ 画布的「图+音→全能参考」会真的带音频进来。宁可指名拒绝，
	// 也不猜一个字段名发上去换一个看不懂的上游 400。
	msg := buildErr(t, "seedance_video", videoJob("ref2video", nil,
		"image:https://cdn.x/a.png", "audio:https://cdn.x/a.mp3"))
	if !strings.Contains(msg, "音频") {
		t.Fatalf("要指名是音频那一路的问题: %s", msg)
	}
}

func TestSeedance_原生音轨默认关(t *testing.T) {
	// 静音视频进画布后还能另外配音；反过来带了音轨再想去掉只能重新生成。
	body := buildBody(t, "seedance_video", videoJob("text2video", nil))
	if _, ok := body["generate_audio"]; ok {
		t.Fatal("没开音轨时不该带 generate_audio")
	}
	on := buildBody(t, "seedance_video", videoJob("text2video", map[string]any{"audio": true}))
	if on["generate_audio"] != true {
		t.Fatalf("显式开了音轨就要带上: %+v", on)
	}
}

func TestSeedance_产物在content里(t *testing.T) {
	// ⚠️ 老系统的产物路径表里没有 content，一个产物都扫不到，
	// 落到「任务完成但未返回产物」，排查时看着像上游的问题。
	res, err := mustGet(t, "seedance_video").ParseQuery(200, []byte(
		`{"id":"t-1","status":"succeeded","content":{"video_url":"https://cdn.x/o.mp4"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.State != StateSuccess || len(res.Outputs) != 1 ||
		res.Outputs[0].Value != "https://cdn.x/o.mp4" {
		t.Fatalf("产物没解析出来: %+v", res)
	}
}

func TestSeedance_成功但没有产物判失败(t *testing.T) {
	res, err := mustGet(t, "seedance_video").ParseQuery(200, []byte(`{"id":"t-1","status":"succeeded"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.State != StateFailed || !strings.Contains(res.Message, "未返回视频") {
		t.Fatalf("没有产物不能判成功: %+v", res)
	}
}

// ---------------------------------------------------------------- 中转

func TestBblabu_首尾帧被明确拒绝(t *testing.T) {
	// ⚠️ AC-10 的原型：这套接口没有首帧/尾帧的位置。把两张图当普通参考图
	// 发上去的话，用户以为在做首尾帧过渡，拿到的却是普通参考生成。
	msg := buildErr(t, "bblabu_video", videoJob("first_last_frame", nil,
		"image:https://cdn.x/a.png", "image:https://cdn.x/b.png"))
	if !strings.Contains(msg, "首帧") && !strings.Contains(msg, "首尾帧") {
		t.Fatalf("要说清楚是首尾帧表达不了: %s", msg)
	}
	// 协议自己也要能声明这一点，供目录域在配置时就拦下。
	if SupportsOperation("bblabu_video", "first_last_frame") {
		t.Fatal("协议应声明自己不支持 first_last_frame")
	}
}

func TestBblabu_自动补参考记号(t *testing.T) {
	// 中转靠 @图1 / @视频1 把提示词与数组下标对应起来，而画布与脚本线
	// 产出的提示词里不会有这种记号。
	body := buildBody(t, "bblabu_video", videoJob("ref2video", nil,
		"image:https://cdn.x/a.png", "image:https://cdn.x/b.png", "video:https://cdn.x/c.mp4"))
	prompt, _ := body["prompt"].(string)
	if !strings.HasPrefix(prompt, "@图1 @图2 @视频1 ") {
		t.Fatalf("应自动补参考记号: %q", prompt)
	}
}

func TestBblabu_用户写了记号就不动(t *testing.T) {
	// 已经写了的原样保留，免得把用户排好的引用顺序打乱。
	job := videoJob("ref2video", nil, "image:https://cdn.x/a.png")
	job.Prompt = "@图1 中的人物挥手"
	body := buildBody(t, "bblabu_video", job)
	if body["prompt"] != "@图1 中的人物挥手" {
		t.Fatalf("用户写了记号就不该再补: %v", body["prompt"])
	}
}

func TestBblabu_adaptive被丢弃而不是报错(t *testing.T) {
	// ⚠️ adaptive 是可灵的概念，这家不认。但历史节点的画布数据里可能还存着它 ——
	// 报错的话用户得先去每个历史节点手动改一遍比例才能生成。
	body := buildBody(t, "bblabu_video", videoJob("text2video",
		map[string]any{"aspect_ratio": "adaptive"}))
	if _, ok := body["ratio"]; ok {
		t.Fatalf("adaptive 应被丢弃而不是发出去: %v", body["ratio"])
	}
}

// ---------------------------------------------------------------- Grok

func TestGrok_三种参考输入三种写法(t *testing.T) {
	// ⚠️ 图片是嵌套对象、视频与音频是顶层字符串 ——
	// 这就是"不能替换为通用 JSON 原样透传"的具体含义。
	body := buildBody(t, "grok_video", videoJob("ref2video", nil,
		"image:https://cdn.x/a.png", "video:https://cdn.x/b.mp4", "audio:https://cdn.x/c.mp3"))
	img, _ := body["image"].(map[string]any)
	if img == nil || img["image_url"] != "https://cdn.x/a.png" {
		t.Fatalf("图片要放进嵌套对象: %+v", body["image"])
	}
	if body["video_url"] != "https://cdn.x/b.mp4" || body["audio_url"] != "https://cdn.x/c.mp3" {
		t.Fatalf("视频与音频是顶层字符串: %+v", body)
	}
}

func TestGrok_收音频而另三家不收(t *testing.T) {
	// 同一个 ref2video，各家能力不同（UniArt 也收音频，见 video_uniart_test.go） —— 这正是能力矩阵要表达的东西（GAP-22）。
	if !SupportsRole("grok_video", "ref2video", RoleAudio) {
		t.Fatal("Grok 支持音频参考")
	}
	for _, code := range []string{"kling_video", "seedance_video", "bblabu_video"} {
		if SupportsRole(code, "ref2video", RoleAudio) {
			t.Errorf("%s 不支持音频参考，不该声明支持", code)
		}
	}
}

func TestGrok_有参考图时不强塞画幅(t *testing.T) {
	// 画幅应当跟随参考图，写死一个会把竖图拉成横屏。
	withImage := buildBody(t, "grok_video", videoJob("img2video", nil, "image:https://cdn.x/a.png"))
	if _, ok := withImage["aspect_ratio"]; ok {
		t.Fatalf("有参考图且用户没选时不该带画幅: %v", withImage["aspect_ratio"])
	}
	noImage := buildBody(t, "grok_video", videoJob("text2video", nil))
	if noImage["aspect_ratio"] != "16:9" {
		t.Fatalf("纯文生要有个默认画幅: %v", noImage["aspect_ratio"])
	}
}

// ---------------------------------------------------------------- 通用规格

func TestVideo_时长与分辨率越界被拒(t *testing.T) {
	// 越界在这里拒掉，比等上游返回 400 强：那时积分已经扣了。
	if msg := buildErr(t, "kling_video",
		videoJob("text2video", map[string]any{"duration": 60})); !strings.Contains(msg, "时长") {
		t.Fatalf("超长应被拒: %s", msg)
	}
	if msg := buildErr(t, "kling_video",
		videoJob("text2video", map[string]any{"resolution": "8k"})); !strings.Contains(msg, "分辨率") {
		t.Fatalf("不支持的分辨率应被拒: %s", msg)
	}
}

func TestVideo_默认时长与计价一致(t *testing.T) {
	// ⚠️ `per_second` = 单价 × 时长。两边默认值不一致就会出现
	// 「扣的积分和实际生成时长对不上」，而这种错在对账之前谁都不会发现。
	body := buildBody(t, "seedance_video", videoJob("text2video", nil))
	if body["duration"] != float64(DefaultVideoSeconds) {
		t.Fatalf("默认时长应为 %d，实际 %v", DefaultVideoSeconds, body["duration"])
	}
}

func TestVideo_图生视频缺图被拒(t *testing.T) {
	for _, code := range []string{"kling_video", "seedance_video", "bblabu_video", "grok_video"} {
		job := videoJob("img2video", map[string]any{
			SubmitPathHintKey: "/v1/videos/image-to-video/generations",
		})
		if msg := buildErr(t, code, job); msg == "" {
			t.Errorf("%s 缺图应被拒", code)
		}
	}
}

func TestVideo_提示词为空时按输入形态兜底(t *testing.T) {
	// 图生视频的提示词为空时上游多半直接 400，而用户的心智是
	// "我给了图，你动起来就行"。
	job := videoJob("img2video", map[string]any{
		SubmitPathHintKey: "/v1/videos/image-to-video/generations",
	}, "image:https://cdn.x/a.png")
	job.Prompt = "   "
	body := buildBody(t, "seedance_video", job)
	content, _ := body["content"].([]any)
	text, _ := content[0].(map[string]any)
	if s, _ := text["text"].(string); !strings.Contains(s, "Animate") {
		t.Fatalf("应有兜底提示词: %v", text["text"])
	}
	// 纯文生且提示词为空则必须报错 —— 没有任何东西可以生成。
	empty := videoJob("text2video", nil)
	empty.Prompt = ""
	if msg := buildErr(t, "seedance_video", empty); !strings.Contains(msg, "提示词") {
		t.Fatalf("纯文生缺提示词应被拒: %s", msg)
	}
}

// ---------------------------------------------------------------- 能力声明

func TestCapabilities_四家的支持范围不同(t *testing.T) {
	// 这张表是 GAP-22 的机器可读形式：管理端、报价、受理三处都读它。
	cases := []struct {
		code, op string
		want     bool
	}{
		{"bblabu_video", "first_last_frame", false},
		{"kling_video", "first_last_frame", true},
		{"seedance_video", "first_last_frame", true},
		{"grok_video", "first_last_frame", false},
	}
	for _, c := range cases {
		if got := SupportsOperation(c.code, c.op); got != c.want {
			t.Errorf("%s 支持 %s = %v，期望 %v", c.code, c.op, got, c.want)
		}
	}
	// 未声明能力的协议一律放行 —— 不知道就别拦。
	if !SupportsOperation("generic_async", "any_future_operation") {
		t.Fatal("未声明能力的协议不该拦")
	}
	if Capabilities("generic_async") != nil {
		t.Fatal("通用协议不该声明能力边界：它不知道下游挂的是什么")
	}
}

// ---------------------------------------------------------------- 辅助

func contentTypes(contents []any) []string {
	out := make([]string, 0, len(contents))
	for _, c := range contents {
		m, _ := c.(map[string]any)
		s, _ := m["type"].(string)
		out = append(out, s)
	}
	return out
}

func urlAt(contents []any, i int) string {
	m, _ := contents[i].(map[string]any)
	s, _ := m["url"].(string)
	return s
}

func idAt(contents []any, i int) string {
	m, _ := contents[i].(map[string]any)
	s, _ := m["id"].(string)
	return s
}
