package protocol

import "fmt"

func init() { register(seedanceVideo{}) }

// seedanceVideo 是火山方舟 Seedance 的视频协议。
//
// 请求体是「**多模态 content 数组 + 顶层规格参数**」：
//
//	{"model":"doubao-seedance-...",
//	 "content":[{"type":"text","text":"..."},
//	            {"type":"image_url","image_url":{"url":"..."},"role":"first_frame"},
//	            {"type":"video_url","video_url":{"url":"..."}}],
//	 "resolution":"1080p","ratio":"16:9","duration":5,"watermark":false}
//
// ⚠️ 规格参数一律走**顶层字段**，绝不退回「提示词 --resolution 720p --duration 5」
// 那种把参数拼进提示词的写法：模型会把这段当创作描述读，出来的画面里经常
// 真的出现参数文字，而且参数本身也未必生效。
//
// ⚠️ 与可灵的三段式 `{contents,settings,options}` 不是一回事 —— 连数组名都是
// `content` 而不是 `contents`，抄错一个字母上游就整包读不到输入。
type seedanceVideo struct{}

func (seedanceVideo) Code() string            { return "seedance_video" }
func (seedanceVideo) Async() bool             { return true }
func (seedanceVideo) RequiresQueryPath() bool { return true }

// seedanceLimits 是本协议的参数范围（04 文档 P-61）：BuildSubmit 与报价共用这一份。
var seedanceLimits = VideoLimits{MinSeconds: 3, MaxSeconds: 30, Resolutions: commonVideoResolutions, Ratios: commonVideoRatios}

// VideoLimits 实现 VideoLimitsDeclaring：本协议支持的操作共用一份范围。
func (v seedanceVideo) VideoLimits(operation string) (VideoLimits, bool) {
	if _, ok := v.SupportedOperations()[operation]; !ok {
		return VideoLimits{}, false
	}
	return seedanceLimits, true
}

func (seedanceVideo) SupportedOperations() map[string]OperationSupport {
	return map[string]OperationSupport{
		"text2video":       {Roles: []InputRole{RoleText}},
		"img2video":        {Roles: []InputRole{RoleText, RoleImage}, MaxImages: 1},
		"first_last_frame": {Roles: []InputRole{RoleText, RoleImage}, MaxImages: 2},
		// ⚠️ 明确**不含 RoleAudio**：通用 ref2video 连线规则允许 audio，
		// 而这家收不了（GAP-22 的原型实例）。能力矩阵据此在扣费前就拒。
		"ref2video":   {Roles: []InputRole{RoleText, RoleImage, RoleVideo}},
		"video2video": {Roles: []InputRole{RoleText, RoleVideo}, MaxVideos: 1},
	}
}

func (seedanceVideo) BuildSubmit(job Job) (Request, error) {
	if err := rejectAudioInputs(job, "Seedance 视频协议"); err != nil {
		return Request{}, err
	}
	content, err := seedanceContents(job)
	if err != nil {
		return Request{}, err
	}
	resolution, err := resolutionOf(job.Params, seedanceLimits.Resolutions, "720p")
	if err != nil {
		return Request{}, err
	}
	duration, err := durationOf(job.Params, seedanceLimits.MinSeconds, seedanceLimits.MaxSeconds)
	if err != nil {
		return Request{}, err
	}
	// ⚠️ `adaptive` **原样透传**：Seedance 认这个值（表示跟随参考图），
	// 不像可灵要折成 16:9。折了的话带竖图的参考生成会被强行拉成横屏。
	ratio, err := ratioOf(job.Params, seedanceLimits.Ratios)
	if err != nil {
		return Request{}, err
	}

	body := map[string]any{
		"model":      job.UpstreamModel,
		"content":    content,
		"resolution": resolution,
		"duration":   duration,
		"watermark":  paramBool(job.Params, false, "watermark", "watermarkEnabled"),
	}
	if ratio != "" {
		body["ratio"] = ratio
	}
	// ⚠️ 原生音轨**默认关**：静音视频进画布后还能另外配音，
	// 反过来带了音轨再想去掉只能重新生成。
	if on, ok := job.Params["audio"].(bool); ok && on {
		body["generate_audio"] = true
	} else if on, ok := nested(job.Params, "size", "audio").(bool); ok && on {
		body["generate_audio"] = true
	}
	if seed := paramInt(job.Params, "seed", -1); seed >= 0 {
		body["seed"] = seed
	}
	return Request{Method: "POST", Body: mustJSON(body)}, nil
}

func seedanceContents(job Job) ([]any, error) {
	prompt, err := videoPrompt(job)
	if err != nil {
		return nil, err
	}
	content := []any{map[string]any{"type": "text", "text": prompt}}
	images := mediaByKind(job, "image")
	videos := mediaByKind(job, "video")

	addImage := func(url, role string) {
		content = append(content, map[string]any{
			"type": "image_url", "image_url": map[string]any{"url": url}, "role": role,
		})
	}
	addVideo := func(url string) {
		content = append(content, map[string]any{
			"type": "video_url", "video_url": map[string]any{"url": url},
		})
	}

	switch job.OperationCode {
	case "text2video":
		// 纯文生视频，除提示词外不带任何输入。
	case "img2video":
		if err := requireCount(images, 1, "图生视频需要一张首帧图片"); err != nil {
			return nil, err
		}
		addImage(images[0], "first_frame")
	case "first_last_frame":
		if err := requireCount(images, 2, "首尾帧视频需要两张图片（第一张是首帧、第二张是尾帧）"); err != nil {
			return nil, err
		}
		addImage(images[0], "first_frame")
		addImage(images[1], "last_frame")
	case "ref2video":
		// 与可灵 omni 同一套约定：脚本线要「分镜图当首帧 + 资产图当参考图」。
		start := 0
		if paramBool(job.Params, false, "firstFrameInput", "first_frame_input") && len(images) > 0 {
			addImage(images[0], "first_frame")
			start = 1
		}
		for i := start; i < len(images); i++ {
			addImage(images[i], "reference_image")
		}
		for _, v := range videos {
			addVideo(v)
		}
		if len(images) == 0 && len(videos) == 0 {
			return nil, fmt.Errorf("全能参考视频至少需要一张参考图或一段参考视频")
		}
	case "video2video":
		if err := requireCount(videos, 1, "视频生视频需要一个视频输入"); err != nil {
			return nil, err
		}
		addVideo(videos[0])
	default:
		return nil, fmt.Errorf("当前 Seedance 模型不支持 %s", job.OperationCode)
	}
	return content, nil
}

func (seedanceVideo) ParseSubmit(status int, body []byte) (Result, error) {
	if status >= 400 {
		return Result{}, httpError(status, body)
	}
	root, err := decodeObject(body)
	if err != nil {
		return Result{}, err
	}
	if err := requireArkOK(root); err != nil {
		return Result{}, err
	}
	taskID, _ := root["id"].(string)
	if taskID == "" {
		return Result{}, fmt.Errorf("Seedance 平台未返回任务 ID")
	}
	return Result{Async: true, TaskID: taskID}, nil
}

func (seedanceVideo) BuildQuery(Job, string) (Request, bool) {
	return Request{Method: "GET", Path: "{taskId}"}, true
}

func (seedanceVideo) ParseQuery(status int, body []byte) (Result, error) {
	if status >= 400 {
		return Result{}, httpError(status, body)
	}
	root, err := decodeObject(body)
	if err != nil {
		return Result{}, err
	}
	if err := requireArkOK(root); err != nil {
		return Result{}, err
	}
	return videoStateResult(root, "Seedance")
}

// requireArkOK 方舟的业务错误也放在 HTTP 200 的响应体里。
func requireArkOK(root map[string]any) error {
	e, ok := root["error"].(map[string]any)
	if !ok {
		return nil
	}
	msg, _ := e["message"].(string)
	code, _ := e["code"].(string)
	if msg == "" && code == "" {
		return nil
	}
	return fmt.Errorf("上游返回错误 %s：%s", code, truncate400(msg))
}
