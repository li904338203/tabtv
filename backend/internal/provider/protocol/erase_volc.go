package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
)

func init() { register(volcMediaKitErase{}) }

// 火山 AI MediaKit 的视频去字幕（docs/agent/smartcut SCT-38，dev/01 §7）：
//
//	POST /api/v1/tools/erase-video-subtitle-pro   {"video_url":"https://…","mode":"subtitle"}   → {"task_id":"…"}
//	GET  /api/v1/tasks/{task_id}                   → {"status":"completed","result":{"video_url":"https://…"}}
//
// 认证是 Bearer API key（Provider 配 `bearer`）。按产出时长计费，我们只把片段取用的那一段截出来发过去
// （输入区间，SCD-34），计价也按这一段。
//
// ⚠️ 2026-10-28 没能核对官方文档的当前版本（文档站是前端渲染，抓取拿不到正文）。端点、Bearer、`video_url`、
// 结果 `result.video_url` 24 小时有效出自 dev/01 §7（2026-10-27 读到的那一版）；**状态词、查询路径、错误结构
// 待核实**（SCR-7，dev/01 R-19）。所以这里：
//   - 状态词宽松地认（ExtractState 的词表：完成一组、失败一组，其余一律当处理中，由死线兜底）——
//     上游多一个排队态时按失败处理会把正常任务全判失败，按处理中最多多查几次；
//   - 产物**只认** `result.video_url`（兼容外面再包一层 `data`），不走兜底扫描：兜底会扫到回显的输入地址，
//     把原片当成结果存回去，而任务显示「成功」；
//   - 错误不猜字段名：常见的几处取得到就用，取不到就带原文。
//
// ⚠️ 只收公网地址（MediaURLRequiring）：本地 MinIO 签出来的是内网地址，要走公网中转（marketing R-15）；
// 两样都没有时，剪辑台的去字幕在受理之前就拦下（wiring.smartcutClipGen），不扣费、不发上游。
// ⚠️ 结果链接 24 小时就失效：Worker 查到完成就下载归档（provider/app.materialize），不存上游地址。
type volcMediaKitErase struct{}

// MediaKitErasePath / MediaKitTaskPath 是缺省的提交与查询路径（运营配的路径优先；查询路径必须配，见 RequiresQueryPath）。
const (
	MediaKitErasePath = "/api/v1/tools/erase-video-subtitle-pro"
	MediaKitTaskPath  = "/api/v1/tasks/{taskId}"
)

func (volcMediaKitErase) Code() string { return "volc_mediakit_erase" }
func (volcMediaKitErase) Async() bool  { return true }

// RequiresQueryPath 查询路径待核实（SCR-7）：让运营照着当前的官方文档显式配一遍，而不是悄悄用一个没核实过的缺省值。
func (volcMediaKitErase) RequiresQueryPath() bool { return true }

// RequiresMediaURL 实现 MediaURLRequiring：`video_url` 只收公网 http(s)。
func (volcMediaKitErase) RequiresMediaURL() bool { return true }

// SupportedOperations 实现 CapabilityDeclaring：只做去字幕，一段视频。
func (volcMediaKitErase) SupportedOperations() map[string]OperationSupport {
	return map[string]OperationSupport{"video_desubtitle": {Roles: []InputRole{RoleVideo}, MaxVideos: 1}}
}

func (volcMediaKitErase) BuildSubmit(job Job) (Request, error) {
	if job.OperationCode != "video_desubtitle" {
		return Request{}, fmt.Errorf("火山 MediaKit 去字幕不支持 %s", job.OperationCode)
	}
	videos := mediaByKind(job, "video")
	if len(videos) != 1 {
		return Request{}, fmt.Errorf("去字幕需要恰好一段视频，当前 %d 段", len(videos))
	}
	u := strings.TrimSpace(videos[0])
	if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		// 走到这里说明 Worker 没按 RequiresMediaURL 给地址（内联了）：发出去上游一定拒，不如当场说清楚。
		return Request{}, fmt.Errorf("去字幕只收公网地址的视频，不收内联数据")
	}
	body := map[string]any{"video_url": u}
	// mode：去字幕（subtitle，缺省）还是去掉画面上的全部文字（text）。取值原样透传（上游字段值待核实）。
	if mode := strings.TrimSpace(paramString(job.Params, "mode", "")); mode != "" {
		body["mode"] = mode
	}
	// 区域与模型版本是可选的高级参数：只有参数里给了才带（剪辑台不下发，运营调试时用）。
	if loc, ok := job.Params["erase_ratio_location"]; ok && loc != nil {
		body["erase_ratio_location"] = loc
	}
	if v := strings.TrimSpace(paramString(job.Params, "model_version", "")); v != "" {
		body["model_version"] = v
	}
	return Request{Method: "POST", Path: MediaKitErasePath, Body: mustJSON(body)}, nil
}

func (volcMediaKitErase) ParseSubmit(status int, body []byte) (Result, error) {
	if status >= 400 {
		return Result{}, fmt.Errorf("去字幕提交失败：上游返回 HTTP %d: %s", status, uniartSnippet(body))
	}
	root, err := decodeObject(body)
	if err != nil {
		return Result{}, err
	}
	if id := mediaKitTaskID(root); id != "" {
		return Result{Async: true, TaskID: id}, nil
	}
	if code, msg, ok := mediaKitBizError(root); ok {
		return Result{}, &StatusError{Status: mediaKitErrorStatus(code), Msg: "去字幕提交被拒：" + msg}
	}
	return Result{}, fmt.Errorf("去字幕上游没有返回任务 ID: %s", uniartSnippet(body))
}

func (volcMediaKitErase) BuildQuery(Job, string) (Request, bool) {
	return Request{Method: "GET", Path: MediaKitTaskPath}, true
}

// ParseQuery 解析任务状态：完成取 `result.video_url`，失败带原因，其余都当处理中。
func (volcMediaKitErase) ParseQuery(status int, body []byte) (Result, error) {
	if status >= 400 {
		// 查询失败不判任务失败（上游可能只是这一次没回），返回 error 让执行器下一轮再查，由死线兜底。
		return Result{}, fmt.Errorf("查询去字幕任务失败：上游返回 HTTP %d: %s", status, uniartSnippet(body))
	}
	root, err := decodeObject(body)
	if err != nil {
		return Result{}, err
	}
	task := root
	if data, ok := root["data"].(map[string]any); ok {
		task = data
	}
	switch ExtractState(body) {
	case StateSuccess:
		u := mediaKitResultURL(task)
		if u == "" {
			return Result{Async: true, State: StateFailed,
				Message: "去字幕任务完成但没有返回结果地址（result.video_url）: " + uniartSnippet(body)}, nil
		}
		return Result{Async: true, State: StateSuccess,
			Outputs: []Output{{Kind: "url", Value: u, MIME: "video/mp4"}}}, nil
	case StateFailed:
		msg := mediaKitMessage(task)
		if msg == "" {
			msg = uniartSnippet(body)
		}
		return Result{Async: true, State: StateFailed, Message: "去字幕失败：" + msg}, nil
	}
	return Result{Async: true, State: StatePending}, nil
}

// mediaKitTaskID 提交响应里的任务 ID：`task_id`（根上或 `data` 里），字符串或数字都认。
func mediaKitTaskID(root map[string]any) string {
	for _, m := range []map[string]any{root, asObject(root["data"])} {
		switch v := m["task_id"].(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		case float64:
			return fmt.Sprintf("%.0f", v)
		}
	}
	return ""
}

// mediaKitResultURL 完成任务的结果地址：`result.video_url`。
func mediaKitResultURL(task map[string]any) string {
	u, _ := asObject(task["result"])["video_url"].(string)
	return strings.TrimSpace(u)
}

// mediaKitBizError 认 HTTP 200 里的业务错误：`error{code,message}`、火山 OpenAPI 的 `ResponseMetadata.Error`、
// 或非 0 的 `code` + `message`。ok=false = 认不出是错误。
func mediaKitBizError(root map[string]any) (code, msg string, ok bool) {
	if e := asObject(root["error"]); len(e) > 0 {
		return scalarString(e["code"]), firstString(e, "message", "msg"), true
	}
	if e := asObject(asObject(root["ResponseMetadata"])["Error"]); len(e) > 0 {
		return scalarString(e["Code"]), firstString(e, "Message", "message"), true
	}
	if c := scalarString(root["code"]); c != "" && c != "0" && c != "200" {
		return c, firstString(root, "message", "msg"), true
	}
	return "", "", false
}

// mediaKitErrorStatus 把业务错误码折成分类用的 HTTP 状态码（generation/domain.ClassifyHTTP）：
// 限流 → 429、服务端忙 / 内部错误 / 超时 → 503（可重试），其余（参数、鉴权、配额）→ 400（重试多少次都一样）。
// ⚠️ 官方错误码表待核实（SCR-7）：这里按错误码里的关键词与数字段判，核实后再收紧。
func mediaKitErrorStatus(code string) int {
	c := strings.ToLower(code)
	switch {
	case strings.Contains(c, "throttl"), strings.Contains(c, "ratelimit"), strings.Contains(c, "toomanyrequests"):
		return 429
	case strings.Contains(c, "internal"), strings.Contains(c, "unavailable"), strings.Contains(c, "timeout"),
		strings.Contains(c, "busy"), strings.HasPrefix(c, "5"):
		return 503
	}
	return 400
}

// mediaKitMessage 失败任务的原因：常见的几个字段取第一个非空的。
func mediaKitMessage(task map[string]any) string {
	if e := asObject(task["error"]); len(e) > 0 {
		if m := firstString(e, "message", "msg"); m != "" {
			return m
		}
	}
	if m := firstString(task, "error_message", "fail_reason", "message", "msg", "error"); m != "" {
		return m
	}
	return firstString(asObject(task["result"]), "error_message", "message", "error")
}

func asObject(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// firstString 取 m 里第一个非空的字符串字段。
func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// scalarString 把字符串或数字的错误码统一成字符串（数字按整数写）。
func scalarString(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case float64:
		return fmt.Sprintf("%.0f", x)
	case json.Number:
		return x.String()
	}
	return ""
}
