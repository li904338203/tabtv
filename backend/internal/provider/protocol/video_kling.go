package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
)

func init() { register(klingVideo{}) }

// klingVideo 是可灵的视频协议。
//
// 请求体是**三段式**：
//
//	{"contents":[{"type":"prompt","text":"..."},
//	             {"type":"first_frame","url":"..."},
//	             {"type":"refer_image","url":"...","id":"image_1"}],
//	 "settings":{"resolution":"720p","duration":5,"aspect_ratio":"16:9","audio":"off","multi_shot":false},
//	 "options":{"external_task_id":"...","watermark_info":{"enabled":false}}}
//
// ⚠️ 与 Seedance 的「多模态 content 数组 + 顶层规格字段」**不是一回事**，
// 与 bblabu 的「平铺参考数组」更不是。这就是"换一家厂商基本都要加协议"的实例。
//
// ⚠️ 提交地址按操作切换（见 SubmitPath）：普通模型的文生/图生是**两个端点**，
// omni 模型才是一个端点收全部模式。
type klingVideo struct{}

func (klingVideo) Code() string            { return "kling_video" }
func (klingVideo) Async() bool             { return true }
func (klingVideo) RequiresQueryPath() bool { return true }

// klingLimits 是本协议的参数范围（04 文档 P-61）：BuildSubmit 与报价共用这一份。
// ⚠️ 不声明画幅：只有非 omni 端点的文生视频才带画幅（其余模式由参考图决定，传了也不发），
// 在这里统一检查会把本来能跑的请求拦掉。画幅仍在 BuildSubmit 里按 klingRatios 校验。
var klingLimits = VideoLimits{MinSeconds: 3, MaxSeconds: 15, Resolutions: []string{"720p", "1080p", "4k"}}

// klingRatios 非 omni 端点文生视频的画幅。
var klingRatios = []string{"16:9", "9:16", "1:1"}

// VideoLimits 实现 VideoLimitsDeclaring：本协议支持的操作共用一份范围。
func (v klingVideo) VideoLimits(operation string) (VideoLimits, bool) {
	if _, ok := v.SupportedOperations()[operation]; !ok {
		return VideoLimits{}, false
	}
	return klingLimits, true
}

func (klingVideo) SupportedOperations() map[string]OperationSupport {
	return map[string]OperationSupport{
		// 普通端点：文生、图生、首尾帧。
		"text2video":       {Roles: []InputRole{RoleText}},
		"img2video":        {Roles: []InputRole{RoleText, RoleImage}, MaxImages: 1},
		"first_last_frame": {Roles: []InputRole{RoleText, RoleImage}, MaxImages: 2},
		// omni 端点：参考图/参考视频。⚠️ 不收音频。
		"ref2video":   {Roles: []InputRole{RoleText, RoleImage, RoleVideo}},
		"video2video": {Roles: []InputRole{RoleText, RoleVideo}, MaxVideos: 1},
	}
}

// SubmitPath 按操作改写配置里的提交地址。
//
// ⚠️ 这是**配置里写不出来的差异**：同一个可灵模型，文生视频与图生视频是两个端点。
// 让运营为每个操作各配一个模型行是可以，但那样同一个型号会被拆成三行，
// 价格、能力矩阵、可用性都要维护三遍。
func (klingVideo) SubmitPath(job Job, configured string) (string, error) {
	if strings.Contains(configured, "/omni-video/") {
		// omni 一个端点收全部模式，不改写。
		return configured, nil
	}
	switch job.OperationCode {
	case "text2video":
		configured = strings.Replace(configured, "/image-to-video/", "/text-to-video/", 1)
	case "img2video", "first_last_frame":
		configured = strings.Replace(configured, "/text-to-video/", "/image-to-video/", 1)
	default:
		return "", fmt.Errorf("当前可灵模型（非 omni 端点）不支持 %s", job.OperationCode)
	}
	if !strings.Contains(configured, "/text-to-video/") &&
		!strings.Contains(configured, "/image-to-video/") {
		// ⚠️ 配置里没有这两个片段就没法改写。报出来而不是照原样发 ——
		// 照原样发的结果是图生视频打到文生端点，上游忽略图片、出一段无关的视频。
		return "", fmt.Errorf("可灵模型的 submitPath 必须含 /text-to-video/ 或 /image-to-video/，当前是 %q", configured)
	}
	return configured, nil
}

func (k klingVideo) BuildSubmit(job Job) (Request, error) {
	if err := rejectAudioInputs(job, "可灵视频协议"); err != nil {
		return Request{}, err
	}
	omni := isKlingOmni(job)
	body := map[string]any{}

	if omni || job.OperationCode != "text2video" {
		contents, err := klingContents(job, omni)
		if err != nil {
			return Request{}, err
		}
		body["contents"] = contents
	} else {
		p, err := videoPrompt(job)
		if err != nil {
			return Request{}, err
		}
		body["prompt"] = p
	}

	resolution, err := resolutionOf(job.Params, klingLimits.Resolutions, "720p")
	if err != nil {
		return Request{}, err
	}
	duration, err := durationOf(job.Params, klingLimits.MinSeconds, klingLimits.MaxSeconds)
	if err != nil {
		return Request{}, err
	}
	settings := map[string]any{"resolution": resolution, "duration": duration}
	if !omni && job.OperationCode == "text2video" {
		ratio, err := ratioOf(job.Params, klingRatios)
		if err != nil {
			return Request{}, err
		}
		if ratio == "" {
			ratio = "16:9"
		}
		settings["aspect_ratio"] = ratio
	}
	if !strings.Contains(klingPathHint(job), "turbo") {
		settings["audio"] = klingAudioMode(job)
		settings["multi_shot"] = job.OperationCode != "video2video" &&
			paramBool(job.Params, false, "multi_shot", "multiShot")
	}
	body["settings"] = settings

	body["options"] = map[string]any{
		// external_task_id 让上游侧也能按我们的 ID 找到这次任务，排查时省一次对照。
		"external_task_id": "seeaitv-" + job.IdempotencyKey,
		"watermark_info": map[string]any{
			"enabled": paramBool(job.Params, false, "watermark", "watermarkEnabled"),
		},
	}
	return Request{Method: "POST", Body: mustJSON(body)}, nil
}

// klingContents 拼 contents 数组。
//
// ⚠️ **首帧与参考图是两种角色**，不能混：首帧锁住起始画面，参考图只保住识别特征。
// 脚本线的分镜视频要「分镜图当首帧 + 资产图当参考图」，两者缺一都不行；
// 画布上的 ref2video 没有首帧概念，全部按参考图发。用 `firstFrameInput` 区分。
func klingContents(job Job, omni bool) ([]any, error) {
	prompt, err := videoPrompt(job)
	if err != nil {
		return nil, err
	}
	contents := []any{map[string]any{"type": "prompt", "text": prompt}}
	images := mediaByKind(job, "image")
	videos := mediaByKind(job, "video")

	add := func(typ, url, id string) {
		item := map[string]any{"type": typ, "url": url}
		if id != "" {
			item["id"] = id
		}
		contents = append(contents, item)
	}

	switch {
	case job.OperationCode == "img2video":
		if err := requireCount(images, 1, "图生视频需要一张首帧图片"); err != nil {
			return nil, err
		}
		add("first_frame", images[0], "")
	case job.OperationCode == "first_last_frame":
		if err := requireCount(images, 2, "首尾帧视频需要两张图片（第一张是首帧、第二张是尾帧）"); err != nil {
			return nil, err
		}
		add("first_frame", images[0], "")
		add("last_frame", images[1], "")
	case job.OperationCode == "ref2video" && omni:
		start := 0
		if paramBool(job.Params, false, "firstFrameInput", "first_frame_input") && len(images) > 0 {
			add("first_frame", images[0], "")
			start = 1
		}
		for i := start; i < len(images); i++ {
			add("refer_image", images[i], fmt.Sprintf("image_%d", i-start+1))
		}
		for i, v := range videos {
			add("feature_video", v, fmt.Sprintf("video_%d", i+1))
		}
		if len(images) == 0 && len(videos) == 0 {
			return nil, fmt.Errorf("全能参考视频至少需要一张参考图或一段参考视频")
		}
	case job.OperationCode == "video2video" && omni:
		if err := requireCount(videos, 1, "视频生视频需要一个视频输入"); err != nil {
			return nil, err
		}
		add("base_video", videos[0], "video_1")
	case job.OperationCode == "text2video":
		// omni 端点下的纯文生：只有提示词。
	default:
		return nil, fmt.Errorf("当前可灵模型不支持 %s（omni=%v）", job.OperationCode, omni)
	}
	return contents, nil
}

func klingAudioMode(job Job) string {
	on, ok := job.Params["audio"].(bool)
	if !ok {
		on, _ = nested(job.Params, "size", "audio").(bool)
	}
	if !on {
		return "off"
	}
	if job.OperationCode == "video2video" {
		return "original"
	}
	return "native"
}

// isKlingOmni / klingPathHint 从参数里取提交路径线索。
//
// ⚠️ 协议层拿不到 Provider 配置（它只收 Job），所以由 app 层把已解析的提交路径
// 放进 `_submitPath`。这比让协议去查配置干净：协议仍然是纯函数，能用 fixture 测。
func isKlingOmni(job Job) bool { return strings.Contains(klingPathHint(job), "/omni-video/") }

func klingPathHint(job Job) string { return paramString(job.Params, SubmitPathHintKey, "") }

// SubmitPathHintKey 是 app 层塞进参数的提交路径线索键。
// 用下划线前缀与用户参数区分 —— 用户参数里不会有这个名字。
const SubmitPathHintKey = "_submitPath"

func (klingVideo) ParseSubmit(status int, body []byte) (Result, error) {
	if status >= 400 {
		return Result{}, httpError(status, body)
	}
	root, err := decodeObject(body)
	if err != nil {
		return Result{}, err
	}
	if err := requireKlingCode(root); err != nil {
		return Result{}, err
	}
	task, _ := root["data"].(map[string]any)
	if task == nil {
		return Result{}, fmt.Errorf("可灵平台未返回任务信息")
	}
	taskID, _ := task["id"].(string)
	if taskID == "" {
		return Result{}, fmt.Errorf("可灵平台未返回任务 ID")
	}
	return Result{Async: true, TaskID: taskID}, nil
}

func (klingVideo) BuildQuery(Job, string) (Request, bool) {
	return Request{Method: "GET", Path: "{taskId}"}, true
}

func (klingVideo) ParseQuery(status int, body []byte) (Result, error) {
	if status >= 400 {
		return Result{}, httpError(status, body)
	}
	root, err := decodeObject(body)
	if err != nil {
		return Result{}, err
	}
	if err := requireKlingCode(root); err != nil {
		return Result{}, err
	}
	task := root["data"]
	// 查询有时回数组（批量接口复用），取第一项。
	if arr, ok := task.([]any); ok && len(arr) > 0 {
		task = arr[0]
	}
	obj, ok := task.(map[string]any)
	if !ok {
		return Result{}, fmt.Errorf("可灵平台未返回任务状态")
	}
	return videoStateResult(obj, "可灵")
}

// requireKlingCode 可灵把业务错误放在 HTTP 200 的 `code` 里。
//
// ⚠️ 只看 http_status 判不出失败 —— 与我们自己的「HTTP 200 + 业务码」是同一个约定，
// 不检查的话一次失败会被当成"没有任务 ID"，错误信息完全指不到真正的原因。
func requireKlingCode(root map[string]any) error {
	code, ok := root["code"].(float64)
	if !ok || code == 0 {
		return nil
	}
	msg, _ := root["message"].(string)
	return fmt.Errorf("可灵平台请求失败，错误码 %d：%s", int(code), msg)
}

func decodeObject(body []byte) (map[string]any, error) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("上游响应不是 JSON 对象: %w", err)
	}
	return root, nil
}

// videoStateResult 把一个任务对象翻成 Result。四家的状态词表共用
// ExtractState 的判定；产物与失败原因的取法也一致。
func videoStateResult(task map[string]any, who string) (Result, error) {
	raw, _ := json.Marshal(task)
	state := ExtractState(raw)
	res := Result{Async: true, State: state}
	switch state {
	case StateSuccess:
		res.Outputs = ExtractOutputs(raw)
		if len(res.Outputs) == 0 {
			res.State = StateFailed
			res.Message = who + "任务完成但未返回视频"
		}
	case StateFailed:
		res.Message = videoFailure(task, who)
	}
	return res, nil
}

func videoFailure(task map[string]any, who string) string {
	for _, k := range []string{"message", "failure_reason", "error_message", "reason"} {
		if s, ok := task[k].(string); ok && s != "" {
			return truncate400(s)
		}
	}
	if e, ok := task["error"].(map[string]any); ok {
		if s, ok := e["message"].(string); ok && s != "" {
			return truncate400(s)
		}
	}
	return who + "视频生成失败"
}

func truncate400(s string) string {
	r := []rune(s)
	if len(r) <= 400 {
		return s
	}
	return string(r[:400]) + "…"
}
