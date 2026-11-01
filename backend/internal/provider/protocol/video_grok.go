package protocol

import "fmt"

func init() { register(grokVideo{}) }

// grokVideo 是 Grok 的视频协议。
//
// 请求体是**扁平的**，参考输入各占一个具名字段而不是数组：
//
//	{"model":"...","prompt":"...","duration":5,"resolution":"720p","aspect_ratio":"16:9",
//	 "image":{"image_url":"https://..."},"video_url":"https://...","audio_url":"https://..."}
//
// ⚠️ 图片是 `{"image":{"image_url":...}}` 的**嵌套对象**，视频与音频却是
// `video_url` / `audio_url` 两个**顶层字符串** —— 三个参考输入三种写法，
// 这就是"不能替换为通用 JSON 原样透传"的具体含义。
//
// ⚠️ 这是最早四家里**唯一收音频参考**的协议（后接的 UniArt 也收）。
type grokVideo struct{}

func (grokVideo) Code() string            { return "grok_video" }
func (grokVideo) Async() bool             { return true }
func (grokVideo) RequiresQueryPath() bool { return true }

// grokLimits 是本协议的参数范围（04 文档 P-61）：BuildSubmit 与报价共用这一份。
var grokLimits = VideoLimits{MinSeconds: 3, MaxSeconds: 30, Resolutions: commonVideoResolutions, Ratios: commonVideoRatios}

// VideoLimits 实现 VideoLimitsDeclaring：本协议支持的操作共用一份范围。
func (v grokVideo) VideoLimits(operation string) (VideoLimits, bool) {
	if _, ok := v.SupportedOperations()[operation]; !ok {
		return VideoLimits{}, false
	}
	return grokLimits, true
}

func (grokVideo) SupportedOperations() map[string]OperationSupport {
	return map[string]OperationSupport{
		"text2video": {Roles: []InputRole{RoleText}},
		"img2video":  {Roles: []InputRole{RoleText, RoleImage}, MaxImages: 1},
		// ⚠️ 收音频：通用 ref2video 规则允许 audio，这家真的支持，
		// 而 Seedance / 可灵 / bblabu 中转都不支持（UniArt 支持）。同一个操作在不同协议下能力不同，
		// 正是能力矩阵要表达的东西（GAP-22）。
		"ref2video":   {Roles: []InputRole{RoleText, RoleImage, RoleAudio, RoleVideo}, MaxImages: 1},
		"video2video": {Roles: []InputRole{RoleText, RoleVideo}, MaxVideos: 1},
	}
}

func (grokVideo) BuildSubmit(job Job) (Request, error) {
	images := mediaByKind(job, "image")
	videos := mediaByKind(job, "video")
	audios := mediaByKind(job, "audio")

	switch job.OperationCode {
	case "text2video":
	case "img2video":
		if err := requireCount(images, 1, "Grok 图生视频需要一张有效参考图片"); err != nil {
			return Request{}, err
		}
	case "video2video":
		if err := requireCount(videos, 1, "视频生视频需要一个视频输入"); err != nil {
			return Request{}, err
		}
	case "ref2video":
		if len(images) == 0 && len(videos) == 0 && len(audios) == 0 {
			return Request{}, fmt.Errorf("全能参考视频至少需要一个参考输入")
		}
	default:
		return Request{}, fmt.Errorf("当前 Grok 模型不支持 %s", job.OperationCode)
	}

	prompt, err := videoPrompt(job)
	if err != nil {
		return Request{}, err
	}
	duration, err := durationOf(job.Params, grokLimits.MinSeconds, grokLimits.MaxSeconds)
	if err != nil {
		return Request{}, err
	}
	resolution, err := resolutionOf(job.Params, grokLimits.Resolutions, "720p")
	if err != nil {
		return Request{}, err
	}
	ratio, err := ratioOf(job.Params, grokLimits.Ratios)
	if err != nil {
		return Request{}, err
	}

	body := map[string]any{
		"model": job.UpstreamModel, "prompt": prompt,
		"duration": duration, "resolution": resolution,
	}
	// ⚠️ 有参考图时**不强塞比例**：画幅应当跟随参考图，写死一个会把竖图拉成横屏。
	// 没有参考图且用户没选时才回落 16:9。`adaptive` 这家不认，丢弃由上游决定。
	switch {
	case ratio != "" && ratio != "adaptive":
		body["aspect_ratio"] = ratio
	case ratio == "" && len(images) == 0:
		body["aspect_ratio"] = "16:9"
	}
	if seed := paramInt(job.Params, "seed", -1); seed >= 0 {
		body["seed"] = seed
	}
	if len(images) > 0 {
		body["image"] = map[string]any{"image_url": images[0]}
	}
	if len(videos) > 0 {
		body["video_url"] = videos[0]
	}
	if len(audios) > 0 {
		body["audio_url"] = audios[0]
	}
	return Request{Method: "POST", Body: mustJSON(body)}, nil
}

func (grokVideo) ParseSubmit(status int, body []byte) (Result, error) {
	if status >= 400 {
		return Result{}, httpError(status, body)
	}
	taskID := ExtractTaskID(body)
	if taskID == "" {
		// 2xx 但没有任务 ID：有些型号会同步返回结果。
		if outs := ExtractOutputs(body); len(outs) > 0 {
			return Result{Outputs: outs}, nil
		}
		return Result{}, fmt.Errorf("Grok 平台未返回任务 ID")
	}
	return Result{Async: true, TaskID: taskID}, nil
}

func (grokVideo) BuildQuery(Job, string) (Request, bool) {
	return Request{Method: "GET", Path: "{taskId}"}, true
}

func (grokVideo) ParseQuery(status int, body []byte) (Result, error) {
	if status >= 400 {
		return Result{}, httpError(status, body)
	}
	root, err := decodeObject(body)
	if err != nil {
		return Result{}, err
	}
	return videoStateResult(root, "Grok")
}
