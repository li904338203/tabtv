package protocol

import (
	"fmt"
	"strings"
)

func init() { register(openAIVideo{}) }

// openAIVideo 是 OpenAI 兼容视频接口的协议（`POST /v1/videos` 提交、`GET /v1/videos/{id}` 查询）。
//
// 请求体是「**OpenAI 视频信封 + metadata 里放全部规格与参考**」：
//
//	{"model":"seedance-2.0-discount","prompt":"...","seconds":5,
//	 "metadata":{"resolution":"720p","ratio":"16:9","mode":"references",
//	             "image_urls":["https://..."],"video_urls":["https://..."],
//	             "input_video_duration":8,"audio_urls":["https://..."],
//	             "generate_audio":false}}
//
// 首尾帧走另一种模式：`"mode":"frames"` + `first_frame_url` / `last_frame_url`（**两者必须同时给**）。
//
// ⚠️ 规格全在 `metadata` 里、有 frames 模式。它不是 `generic_async` —— 后者发的是本平台
// 自有信封，假定上游兼容本平台，而这里是 OpenAI 的形状。
//
// ⚠️ **图生视频在这里是「参考图」语义，不是「首帧」**：frames 模式要求首尾帧同时给，
// 单张首帧表达不了。所以 `img2video` 发成 references + 一张 image_urls ——
// 画面会参考这张图，但不保证从它起镜。脚本线的 `firstFrameInput` 同理无法表达，
// 那张图按普通参考图发出去。
//
// ⚠️ `seconds` **永远显式发**：上游默认 15 秒，而我们的计价默认 5 秒
// （DefaultVideoSeconds）—— 不发的话扣 5 秒的钱、出 15 秒的片，对账时才发现。
type openAIVideo struct{}

// OpenAI 兼容视频接口 的数量与时长上限（上游文档给的协议层上限，型号能力仍由能力矩阵按型号收紧）。
const (
	openAIVideoMaxImages       = 30
	openAIVideoMaxVideos       = 10
	openAIVideoMaxAudios       = 10
	openAIVideoMaxVideoTotalMs = 30_000
	openAIVideoMinSeconds      = 4
	openAIVideoMaxSeconds      = 30
)

// RequiresMediaURL 实现 MediaURLRequiring：这家只收地址形态的参考媒体（2026-10-24 实测，见 marketing dev/01 R-13）。
//
// 内联 base64 在这里**没有一种大小是能用的**，而且报错与原因无关，排查时会一路怀疑别的：
//   - 2.43MB 的 PNG：「上传素材过大，请重新上传:413」；
//   - 压到 0.37MB 的 JPEG：「提示词过长，请缩短提示词后重试」—— 提示词只有 150 字也一样，data URI 被算进了提示词；
//   - 压到 2.5KB：「image2video requires prompt media references」。
//
// 同样的 150 字提示词换成公网图片地址就成功了，575 字的原提示词也照常受理（上限 5000 字）。
// 提交是 202，失败都在排队之后，所以必须在提交之前拦下。
func (openAIVideo) RequiresMediaURL() bool { return true }

func (openAIVideo) Code() string            { return "openai_video" }
func (openAIVideo) Async() bool             { return true }
func (openAIVideo) RequiresQueryPath() bool { return true }

// openAIVideoLimits 是本协议的参数范围（04 文档 P-61）：BuildSubmit 与报价共用这一份。
//
// RatioRequired：不带画幅（或 adaptive，本协议会丢掉它）时上游按 auto 处理，seedance-2.5 的能力集不支持
// auto，提交直接 400（会话 107398945801502720）。BuildSubmit 仍按原样丢掉 adaptive（已经受理的历史任务
// 照旧能发），新的报价 / 受理在扣费之前就要求给明确的画幅。
var openAIVideoLimits = VideoLimits{MinSeconds: openAIVideoMinSeconds, MaxSeconds: openAIVideoMaxSeconds,
	Resolutions: []string{"480p", "720p", "1080p"}, Ratios: commonVideoRatios, RatioRequired: true}

// VideoLimits 实现 VideoLimitsDeclaring：本协议支持的操作共用一份范围。
func (v openAIVideo) VideoLimits(operation string) (VideoLimits, bool) {
	if _, ok := v.SupportedOperations()[operation]; !ok {
		return VideoLimits{}, false
	}
	return openAIVideoLimits, true
}

func (openAIVideo) SupportedOperations() map[string]OperationSupport {
	return map[string]OperationSupport{
		"text2video": {Roles: []InputRole{RoleText}},
		"img2video":  {Roles: []InputRole{RoleText, RoleImage}, MaxImages: 1},
		// frames 模式：恰好两张，第一张首帧、第二张尾帧。
		"first_last_frame": {Roles: []InputRole{RoleText, RoleImage}, MaxImages: 2},
		// ⚠️ **收音频**：这家的 references 模式有 audio_urls（与 Seedance 原生不同）。
		"ref2video": {Roles: []InputRole{RoleText, RoleImage, RoleVideo, RoleAudio},
			MaxImages: openAIVideoMaxImages, MaxVideos: openAIVideoMaxVideos},
		"video2video": {Roles: []InputRole{RoleText, RoleVideo}, MaxVideos: 1},
	}
}

func (openAIVideo) BuildSubmit(job Job) (Request, error) {
	images := mediaByKind(job, "image")
	videos := mediaByKind(job, "video")
	audios := mediaByKind(job, "audio")

	meta := map[string]any{}
	hasRefs := false
	switch job.OperationCode {
	case "text2video":
		// 纯文生：不带任何参考，也不带 mode（上游默认 references 且无素材）。
	case "img2video":
		if err := requireCount(images, 1, "图生视频需要一张参考图片"); err != nil {
			return Request{}, err
		}
		meta["mode"] = "references"
		meta["image_urls"] = images[:1]
		hasRefs = true
	case "first_last_frame":
		// ⚠️ 必须**恰好**两张：frames 模式下首尾帧缺一个上游就拒；多出来的
		// 又没有位置可放，悄悄丢掉会让用户以为第三张也参与了。
		if len(images) != 2 {
			return Request{}, fmt.Errorf("首尾帧视频需要恰好两张图片（第一张首帧、第二张尾帧），当前 %d 张", len(images))
		}
		meta["mode"] = "frames"
		meta["first_frame_url"] = images[0]
		meta["last_frame_url"] = images[1]
		hasRefs = true
	case "ref2video":
		if len(images) == 0 && len(videos) == 0 && len(audios) == 0 {
			return Request{}, fmt.Errorf("全能参考视频至少需要一个参考图片、视频或音频")
		}
		if len(images) > openAIVideoMaxImages {
			return Request{}, fmt.Errorf("OpenAI 兼容视频接口 最多接受 %d 张参考图，当前 %d 张", openAIVideoMaxImages, len(images))
		}
		if len(audios) > openAIVideoMaxAudios {
			return Request{}, fmt.Errorf("OpenAI 兼容视频接口 最多接受 %d 段参考音频，当前 %d 段", openAIVideoMaxAudios, len(audios))
		}
		meta["mode"] = "references"
		if len(images) > 0 {
			meta["image_urls"] = images
		}
		if len(audios) > 0 {
			meta["audio_urls"] = audios
		}
		if err := openAIVideoAddVideos(job, meta, videos); err != nil {
			return Request{}, err
		}
		hasRefs = true
	case "video2video":
		if len(videos) != 1 {
			return Request{}, fmt.Errorf("视频生视频需要恰好一个视频输入，当前 %d 个", len(videos))
		}
		meta["mode"] = "references"
		if err := openAIVideoAddVideos(job, meta, videos); err != nil {
			return Request{}, err
		}
		hasRefs = true
	default:
		return Request{}, fmt.Errorf("当前 OpenAI 兼容视频接口 视频模型不支持 %s", job.OperationCode)
	}

	resolution, err := resolutionOf(job.Params, openAIVideoLimits.Resolutions, "720p")
	if err != nil {
		return Request{}, err
	}
	meta["resolution"] = resolution
	// ⚠️ `adaptive` 这家不认：兜底丢弃而不是报错（历史节点可能还存着它：从可灵 Omni 切过来时它在选项列表里）。
	ratio, err := ratioOf(job.Params, openAIVideoLimits.Ratios)
	if err != nil {
		return Request{}, err
	}
	if ratio != "" && !strings.EqualFold(ratio, "adaptive") {
		meta["ratio"] = ratio
	}
	duration, err := durationOf(job.Params, openAIVideoLimits.MinSeconds, openAIVideoLimits.MaxSeconds)
	if err != nil {
		return Request{}, err
	}
	// ⚠️ 原生音轨默认关（与 Seedance 原生同一条）：静音视频进画布后还能另外配音。
	meta["generate_audio"] = paramBool(job.Params, false, "audio")

	body := map[string]any{
		"model":    job.UpstreamModel,
		"seconds":  duration,
		"metadata": meta,
	}
	// ⚠️ 提示词：纯文生必填；有参考素材或完整首尾帧时上游允许为空 ——
	// 此时**不发兜底英文**（那会覆盖参考素材本身的语义），空就不带这个字段。
	prompt := strings.TrimSpace(job.Prompt)
	switch {
	case prompt != "":
		body["prompt"] = prompt
	case !hasRefs:
		return Request{}, fmt.Errorf("文生视频的提示词不能为空")
	}
	// 路径以模型配置的提交路径为准（provider/app.submitPathOf）；这里只是没配时的回落，
	// 与设置页的缺省值一致：厂商地址写到 /v1 为止，路径写 /videos。
	return Request{Method: "POST", Path: "/videos", Body: mustJSON(body)}, nil
}

// openAIVideoAddVideos 写参考视频与它们的总时长。
//
// ⚠️ `input_video_duration` 是**上游要求调用方自报**的总秒数，且合计不得超过 30 秒。
// 时长未知时**拒绝而不是猜一个数**：报小了上游按错的时长计费/截断，报大了直接被拒，
// 两种都要等到上游才暴露。时长来自上传时的 ffprobe，正常的视频都有。
func openAIVideoAddVideos(job Job, meta map[string]any, videos []string) error {
	if len(videos) == 0 {
		return nil
	}
	if len(videos) > openAIVideoMaxVideos {
		return fmt.Errorf("OpenAI 兼容视频接口 最多接受 %d 段参考视频，当前 %d 段", openAIVideoMaxVideos, len(videos))
	}
	total := 0
	n := 0
	for _, m := range job.Media {
		if m.Kind != "video" {
			continue
		}
		n++
		if m.DurationMs <= 0 {
			return fmt.Errorf("第 %d 段参考视频的时长未知，OpenAI 兼容视频接口 需要上报参考视频总时长；请重新上传该视频后重试", n)
		}
		total += m.DurationMs
	}
	if total > openAIVideoMaxVideoTotalMs {
		return fmt.Errorf("参考视频合计 %.1f 秒，超过 OpenAI 兼容视频接口 的上限 %d 秒",
			float64(total)/1000, openAIVideoMaxVideoTotalMs/1000)
	}
	meta["video_urls"] = videos
	// 向上取整：29.2 秒报 30 秒。上面已保证合计 ≤ 30000ms，所以不会超过 30。
	meta["input_video_duration"] = (total + 999) / 1000
	return nil
}

func (openAIVideo) ParseSubmit(status int, body []byte) (Result, error) {
	if status >= 400 {
		return Result{}, openAIVideoHTTPError(status, body)
	}
	root, err := decodeObject(body)
	if err != nil {
		return Result{}, err
	}
	id, _ := root["id"].(string)
	if id == "" {
		return Result{}, fmt.Errorf("OpenAI 兼容视频接口 未返回任务 ID: %s", bodySnippet(body))
	}
	return Result{Async: true, TaskID: id}, nil
}

func (openAIVideo) BuildQuery(Job, string) (Request, bool) {
	return Request{Method: "GET", Path: "/videos/{taskId}"}, true
}

// ParseQuery 解析查询响应。
//
// ⚠️ 状态词表**只认上游文档里的四个**（queued / in_progress / completed / failed），
// 不复用 ExtractState 的宽词表：那张表把 `error`/`rejected` 之类都当失败，
// 而这家的失败结构未知 —— 这里只信文档。未知状态按 pending（多轮询几次，由死线兜底）。
func (openAIVideo) ParseQuery(status int, body []byte) (Result, error) {
	if status >= 400 {
		return Result{}, openAIVideoHTTPError(status, body)
	}
	root, err := decodeObject(body)
	if err != nil {
		return Result{}, err
	}
	st, _ := root["status"].(string)
	switch strings.ToLower(strings.TrimSpace(st)) {
	case "completed":
		out, ok := openAIVideoOutput(root)
		if !ok {
			return Result{Async: true, State: StateFailed,
				Message: "OpenAI 兼容视频接口 任务完成但未返回视频地址: " + bodySnippet(body)}, nil
		}
		return Result{Async: true, State: StateSuccess, Outputs: []Output{out}}, nil
	case "failed":
		// ⚠️ 失败体的结构未知：**不猜字段**，原文落进错误信息（排查时要的就是原文）。
		return Result{Async: true, State: StateFailed,
			Message: "OpenAI 兼容视频接口 任务失败: " + bodySnippet(body)}, nil
	}
	return Result{Async: true, State: StatePending}, nil
}

// openAIVideoOutput 取完成任务的产物。
//
// `result_url` 可能是绝对地址、也可能是 `/` 开头的相对路径（下载层按 Provider 地址补全，
// 补全后同源 → 带 key）。缺失时回落到内容端点 `/v1/videos/{id}/content`。
// `requires_auth=true` 时标 AuthRequired，并给出同源的内容端点作兜底 ——
// 绝对地址若不同源，宁可改走内容端点，也不把密钥发给别的域名。
func openAIVideoOutput(root map[string]any) (Output, bool) {
	id, _ := root["id"].(string)
	contentPath := ""
	if id != "" {
		contentPath = "/v1/videos/" + id + "/content"
	}
	u, _ := root["result_url"].(string)
	u = strings.TrimSpace(u)
	auth, _ := root["requires_auth"].(bool)
	if u == "" {
		if contentPath == "" {
			return Output{}, false
		}
		// 内容端点是上游自己的，天然要认证、天然同源。
		return Output{Kind: "url", Value: contentPath, MIME: "video/mp4", AuthRequired: true}, true
	}
	out := Output{Kind: "url", Value: u, MIME: mimeFromURL(u), AuthRequired: auth}
	if auth {
		out.AuthFallback = contentPath
	}
	return out, true
}

// openAIVideoHTTPError 非 2xx：状态码 + 原文（经清洗，能安全落库）。
func openAIVideoHTTPError(status int, body []byte) error {
	return fmt.Errorf("上游返回 HTTP %d: %s", status, bodySnippet(body))
}
