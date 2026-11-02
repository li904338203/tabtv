package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

func init() { register(klingV2A{}) }

// 可灵视频配音效（docs/agent/smartcut SCT-39，dev/01 §7）：按画面生成音效，可灵开放平台的原生接口。
//
//	POST /v1/audio/video-to-audio        {"video_url":"https://…","sound_effect_prompt":"…","external_task_id":"…"}
//	     → {"code":0,"data":{"task_id":"…","task_status":"submitted"}}
//	GET  /v1/audio/video-to-audio/{id}   → {"code":0,"data":{"task_status":"succeed",
//	                                         "task_result":{"audios":[{"url_mp3":"…","url_wav":"…"}]}}}
//
// 2026-10-28 用浏览器核对过的官方文档（https://kling.ai/document-api/apiReference/model/videoToAudio 、
// …/api/get-started/authentication 、…/api/get-started/error-codes）：
//   - 认证 `Authorization: Bearer <API Key>`（Provider 配 `bearer`）；域名 `https://api-singapore.klingai.com`（境外）；
//   - `video_id` 与 `video_url` 二选一（我们只用地址）；只收 .mp4 / .mov、≤100MB、**3.0~20.0 秒**；
//   - `sound_effect_prompt`、`bgm_prompt` 各不超过 200 字；`asmr_mode` 缺省 false；
//   - 状态 `submitted / processing / succeed / failed`（没有 ed）；失败原因在 `task_status_msg`；
//   - 结果里有 `videos`（原片配上声音）与 `audios`（`url_mp3` / `url_wav`）：音效轨要的是后者；产物 30 天后清除。
//
// ⚠️ 与现有的 `kling_video` 不是同一套接口：那边是三段式请求体（contents / settings / options），对接的是中转。
// ⚠️ 业务错误码与 HTTP 码一起给（1000 段鉴权、1100 段账户、1200 段参数、1300 段策略、5000 段服务端）：账户欠费、
// 资源包用完回的是 **429**，按 HTTP 码分类会被当成限流一直重试 —— 所以按错误码重新分类（klingCodeStatus）。
type klingV2A struct{}

const (
	klingV2APath = "/v1/audio/video-to-audio"
	// KlingV2AMinSeconds / KlingV2AMaxSeconds 片段时长（官方：3.0~20.0 秒）。
	KlingV2AMinSeconds = 3
	KlingV2AMaxSeconds = 20
	// KlingV2APromptRunes 音效描述的上限（官方：不超过 200 字）。
	KlingV2APromptRunes = 200
)

func (klingV2A) Code() string { return "kling_v2a" }
func (klingV2A) Async() bool  { return true }

// RequiresQueryPath 查询路径在官方文档里核对过（BuildQuery 给出缺省），不强制运营再配一遍。
func (klingV2A) RequiresQueryPath() bool { return false }

// RequiresMediaURL 实现 MediaURLRequiring：`video_url` 只收地址。
func (klingV2A) RequiresMediaURL() bool { return true }

// SupportedOperations 实现 CapabilityDeclaring：一段视频，可带文字描述。
func (klingV2A) SupportedOperations() map[string]OperationSupport {
	return map[string]OperationSupport{"video2audio": {Roles: []InputRole{RoleVideo, RoleText}, MaxVideos: 1}}
}

// DurationRange 实现 DurationDeclaring。
func (klingV2A) DurationRange(operation string) (int, int, bool) {
	if operation != "video2audio" {
		return 0, 0, false
	}
	return KlingV2AMinSeconds, KlingV2AMaxSeconds, true
}

// CheckParams 实现 ParamChecking：片段时长、描述长度。
//
// ⚠️ durationSec 是向上取整过的秒数：19.4 秒的片段报 20 秒照样放行，20.4 秒报 21 秒拦下；
// 2.6 秒报 3 秒会漏过去 —— 毫秒级的判断在剪辑台受理前按片段的取用区间做（smartcut 的 GenerateSfx）。
func (klingV2A) CheckParams(operation string, durationSec int, params map[string]any) error {
	if operation != "video2audio" {
		return nil
	}
	if p, _ := params["prompt"].(string); utf8.RuneCountInString(strings.TrimSpace(p)) > KlingV2APromptRunes {
		return fmt.Errorf("可灵视频配音效的描述不能超过 %d 字", KlingV2APromptRunes)
	}
	return checkSeconds("可灵视频配音效", durationSec, KlingV2AMinSeconds, KlingV2AMaxSeconds)
}

func (p klingV2A) BuildSubmit(job Job) (Request, error) {
	if job.OperationCode != "video2audio" {
		return Request{}, fmt.Errorf("可灵视频配音效不支持 %s", job.OperationCode)
	}
	var video *Media
	for i := range job.Media {
		if job.Media[i].Kind == "video" {
			if video != nil {
				return Request{}, fmt.Errorf("视频配音效只收一段视频")
			}
			video = &job.Media[i]
		}
	}
	if video == nil {
		return Request{}, fmt.Errorf("视频配音效需要一段视频")
	}
	if !strings.HasPrefix(video.Value, "https://") && !strings.HasPrefix(video.Value, "http://") {
		return Request{}, fmt.Errorf("视频配音效只收视频地址，不收内联数据")
	}
	// 时长按毫秒判（官方是 3.0~20.0 秒）：向上取整的秒数会把 2.6 秒放过去。时长未知时放行（上游会说清楚）。
	if ms := video.DurationMs; ms > 0 && (ms < KlingV2AMinSeconds*1000 || ms > KlingV2AMaxSeconds*1000) {
		return Request{}, fmt.Errorf("可灵视频配音效只收 %d~%d 秒的视频（当前 %.1f 秒）", KlingV2AMinSeconds,
			KlingV2AMaxSeconds, float64(ms)/1000)
	}
	if err := p.CheckParams(job.OperationCode, 0, map[string]any{"prompt": job.Prompt}); err != nil {
		return Request{}, err
	}
	body := map[string]any{"video_url": video.Value,
		// 让上游侧也能按我们的 ID 找到这次任务（同 kling_video）。
		"external_task_id": "seeaitv-" + job.IdempotencyKey}
	if prompt := strings.TrimSpace(job.Prompt); prompt != "" {
		body["sound_effect_prompt"] = prompt
	}
	if bgm := strings.TrimSpace(paramString(job.Params, "bgm_prompt", "")); bgm != "" {
		body["bgm_prompt"] = bgm
	}
	if v, ok := job.Params["asmr_mode"].(bool); ok {
		body["asmr_mode"] = v
	}
	return Request{Method: "POST", Path: klingV2APath, Body: mustJSON(body)}, nil
}

func (klingV2A) ParseSubmit(status int, body []byte) (Result, error) {
	data, err := klingV2AData(status, body, "提交视频配音效")
	if err != nil {
		return Result{}, err
	}
	id := scalarString(data["task_id"])
	if id == "" {
		return Result{}, fmt.Errorf("可灵没有返回任务 ID: %s", bodySnippet(body))
	}
	return Result{Async: true, TaskID: id}, nil
}

func (klingV2A) BuildQuery(Job, string) (Request, bool) {
	return Request{Method: "GET", Path: klingV2APath + "/{taskId}"}, true
}

func (klingV2A) ParseQuery(status int, body []byte) (Result, error) {
	data, err := klingV2AData(status, body, "查询视频配音效")
	if err != nil {
		return Result{}, err
	}
	switch strings.ToLower(scalarString(data["task_status"])) {
	case "succeed", "succeeded", "success":
		audios, _ := asObject(data["task_result"])["audios"].([]any)
		for _, a := range audios {
			m := asObject(a)
			if u := firstString(m, "url_mp3"); u != "" {
				return Result{Async: true, State: StateSuccess, Outputs: []Output{{Kind: "url", Value: u, MIME: "audio/mpeg"}}}, nil
			}
			if u := firstString(m, "url_wav"); u != "" {
				return Result{Async: true, State: StateSuccess, Outputs: []Output{{Kind: "url", Value: u, MIME: "audio/wav"}}}, nil
			}
		}
		// ⚠️ 不回落到 task_result.videos：那是原片配上声音的视频，放进音效轨会是一段视频。
		return Result{Async: true, State: StateFailed,
			Message: "可灵任务完成但没有返回音频（task_result.audios）: " + bodySnippet(body)}, nil
	case "failed":
		msg := firstString(data, "task_status_msg")
		if msg == "" {
			msg = bodySnippet(body)
		}
		return Result{Async: true, State: StateFailed, Message: "视频配音效失败：" + msg}, nil
	}
	// submitted / processing，以及没见过的状态：当处理中，由死线兜底。
	return Result{Async: true, State: StatePending}, nil
}

// klingV2AData 解析可灵的响应：`code != 0` 是业务错误（HTTP 码可能是 200 也可能是 4xx / 5xx），按错误码分类。
func klingV2AData(status int, body []byte, what string) (map[string]any, error) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		if status >= 400 {
			return nil, fmt.Errorf("%s失败：上游返回 HTTP %d: %s", what, status, bodySnippet(body))
		}
		return nil, fmt.Errorf("%s失败：响应不是 JSON: %s", what, bodySnippet(body))
	}
	if code := scalarString(root["code"]); code != "" && code != "0" {
		msg := firstString(root, "message")
		return nil, &StatusError{Status: klingCodeStatus(code, status), Msg: fmt.Sprintf("%s失败（错误码 %s）：%s", what, code, msg)}
	}
	if status >= 400 {
		return nil, fmt.Errorf("%s失败：上游返回 HTTP %d: %s", what, status, bodySnippet(body))
	}
	data := asObject(root["data"])
	if data == nil {
		return nil, fmt.Errorf("%s失败：响应里没有 data: %s", what, bodySnippet(body))
	}
	return data, nil
}

// klingCodeStatus 可灵业务错误码 → 分类用的状态码（官方错误码表，2026-10-28 核对）。
//
// ⚠️ 1100~1102（账户异常、欠费、资源包用完或过期）官方回 429：按 HTTP 码分类会被当成限流一直重试，
// 而充值之前重试多少次都一样 —— 这里改成 403（不可重试，原因原样给用户）。真正的限流是 1302 / 1303。
func klingCodeStatus(code string, httpStatus int) int {
	switch code {
	case "1000", "1001", "1002", "1003", "1004":
		return 401
	case "1100", "1101", "1102", "1103", "1304":
		return 403
	case "1302", "1303":
		return 429
	case "1202", "1203":
		return 404
	case "5000":
		return 500
	case "5001":
		return 503
	case "5002":
		return 504
	}
	if strings.HasPrefix(code, "5") || httpStatus >= 500 {
		return 503
	}
	return 400 // 1200 段参数错误、1300 / 1301 策略与内容安全，以及没见过的码
}
