package protocol

import (
	"fmt"
	"strings"
)

func init() { register(bblabuVideo{}) }

// bblabuVideo 是中转平台的统一视频协议。
//
// 请求体是「**平铺参考数组**」，与方舟原生完全不是一个形状：
//
//	{"model":"Seedance-2.5","prompt":"@图1 中的人物挥手，动作参考 @视频1",
//	 "ratio":"16:9","duration":6,"resolution":"480p",
//	 "referenceImages":["https://..."],"referenceVideos":["https://..."]}
//
// ⚠️ 这套接口**没有首帧/尾帧的概念**：参考输入只有两个数组，
// 不像方舟原生能给每张图标 `role`。所以 `first_last_frame` 在这里**无法表达**，
// 必须明确拒绝 —— 把两张图当普通参考图发上去的话，用户以为在做首尾帧过渡，
// 拿到的却是一段普通的参考生成，而且看不出哪里不对。
type bblabuVideo struct{}

func (bblabuVideo) Code() string            { return "bblabu_video" }
func (bblabuVideo) Async() bool             { return true }
func (bblabuVideo) RequiresQueryPath() bool { return true }

// bblabuLimits 是本协议的参数范围（04 文档 P-61）：BuildSubmit 与报价共用这一份。
var bblabuLimits = VideoLimits{MinSeconds: 3, MaxSeconds: 30, Resolutions: commonVideoResolutions, Ratios: commonVideoRatios}

// VideoLimits 实现 VideoLimitsDeclaring：本协议支持的操作共用一份范围。
func (v bblabuVideo) VideoLimits(operation string) (VideoLimits, bool) {
	if _, ok := v.SupportedOperations()[operation]; !ok {
		return VideoLimits{}, false
	}
	return bblabuLimits, true
}

func (bblabuVideo) SupportedOperations() map[string]OperationSupport {
	return map[string]OperationSupport{
		"text2video": {Roles: []InputRole{RoleText}},
		"img2video":  {Roles: []InputRole{RoleText, RoleImage}, MaxImages: 1},
		"ref2video":  {Roles: []InputRole{RoleText, RoleImage, RoleVideo}},
		// ⚠️ **没有 first_last_frame**。这一条就是 AC-10 要在扣费前拦住的那个组合。
		"video2video": {Roles: []InputRole{RoleText, RoleVideo}, MaxVideos: 1},
	}
}

func (bblabuVideo) BuildSubmit(job Job) (Request, error) {
	if err := rejectAudioInputs(job, "中转视频协议"); err != nil {
		return Request{}, err
	}
	images := mediaByKind(job, "image")
	videos := mediaByKind(job, "video")

	switch job.OperationCode {
	case "text2video":
		// 纯文生视频，参考数组留空。
	case "img2video":
		if err := requireCount(images, 1, "图生视频需要一张参考图片"); err != nil {
			return Request{}, err
		}
	case "ref2video":
		if len(images) == 0 && len(videos) == 0 {
			return Request{}, fmt.Errorf("全能参考视频至少需要一张参考图或一段参考视频")
		}
	case "video2video":
		if err := requireCount(videos, 1, "视频生视频需要一个视频输入"); err != nil {
			return Request{}, err
		}
	case "first_last_frame":
		return Request{}, fmt.Errorf(
			"中转视频接口没有首帧/尾帧的位置，无法表达首尾帧过渡；请改用支持该模式的模型")
	default:
		return Request{}, fmt.Errorf("当前中转视频模型不支持 %s", job.OperationCode)
	}

	prompt, err := videoPrompt(job)
	if err != nil {
		return Request{}, err
	}
	duration, err := durationOf(job.Params, bblabuLimits.MinSeconds, bblabuLimits.MaxSeconds)
	if err != nil {
		return Request{}, err
	}
	resolution, err := resolutionOf(job.Params, bblabuLimits.Resolutions, "720p")
	if err != nil {
		return Request{}, err
	}
	ratio, err := ratioOf(job.Params, bblabuLimits.Ratios)
	if err != nil {
		return Request{}, err
	}

	body := map[string]any{
		"model":      job.UpstreamModel,
		"prompt":     bblabuPrompt(prompt, len(images), len(videos)),
		"duration":   duration,
		"resolution": resolution,
	}
	// ⚠️ `adaptive` 是可灵的概念，这家不认（实测上游回「画面比例仅支持 …，不支持 adaptive」）。
	// 这里**兜底丢弃而不是报错**：历史节点的画布数据里可能还存着它 ——
	// 从可灵 Omni 切过来时它在选项列表里。报错的话用户得先去每个历史节点
	// 手动改一遍比例才能生成；丢弃则由上游用自己的默认值。
	if ratio != "" && !strings.EqualFold(ratio, "adaptive") {
		body["ratio"] = ratio
	}
	if len(images) > 0 {
		body["referenceImages"] = images
	}
	if len(videos) > 0 {
		body["referenceVideos"] = videos
	}
	return Request{Method: "POST", Body: mustJSON(body)}, nil
}

// bblabuPrompt 补参考记号。
//
// 中转靠 `@图1` / `@视频1` 这种下标记号，把提示词与两个数组里的第几项对应起来
// （官方示例：「@图1 中的人物向镜头挥手，动作参考 @视频1」）。
//
// ⚠️ 画布和脚本线产出的提示词里不会有这种记号 —— 用户不知道要写，脚本线更是程序拼的。
// 所以在提示词里**一个记号都没有**时自动补一串前缀；已经写了的原样保留，
// 免得把用户自己排好的引用顺序打乱。
func bblabuPrompt(prompt string, imageCount, videoCount int) string {
	if strings.Contains(prompt, "@图") || strings.Contains(prompt, "@视频") {
		return prompt
	}
	if imageCount == 0 && videoCount == 0 {
		return prompt
	}
	var refs strings.Builder
	for i := 1; i <= imageCount; i++ {
		fmt.Fprintf(&refs, "@图%d ", i)
	}
	for i := 1; i <= videoCount; i++ {
		fmt.Fprintf(&refs, "@视频%d ", i)
	}
	return refs.String() + prompt
}

func (bblabuVideo) ParseSubmit(status int, body []byte) (Result, error) {
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
	task := bblabuTask(root)
	for _, k := range []string{"taskId", "task_id", "id"} {
		if s, ok := task[k].(string); ok && s != "" {
			return Result{Async: true, TaskID: s}, nil
		}
	}
	return Result{}, fmt.Errorf("中转平台未返回任务 ID")
}

func (bblabuVideo) BuildQuery(Job, string) (Request, bool) {
	return Request{Method: "GET", Path: "{taskId}"}, true
}

func (bblabuVideo) ParseQuery(status int, body []byte) (Result, error) {
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
	return videoStateResult(bblabuTask(root), "中转平台")
}

// bblabuTask 中转有时把任务包在 `data` 里，有时平铺在顶层。
func bblabuTask(root map[string]any) map[string]any {
	if d, ok := root["data"].(map[string]any); ok {
		return d
	}
	return root
}
