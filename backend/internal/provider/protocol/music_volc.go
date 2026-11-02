package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
)

func init() { register(volcMusic{}) }

// 火山豆包音乐的纯音乐生成（docs/agent/smartcut SCT-39，dev/01 §7）：火山 OpenAPI 的 Action 风格接口，V4 签名。
//
//	POST /?Action=GenBGM&Version=2024-08-12     {"Text":"…","Duration":30}   → {"Result":{"TaskID":"…"}}
//	POST /?Action=QuerySong&Version=2024-08-12  {"TaskID":"…"}               → {"Result":{"Status":…,"SongDetail":{"AudioUrl":"…"}}}
//
// Provider 配 `volc_v4` 认证（AK:SK），地域与服务放在约定的自定义请求头里（X-Volc-Region / X-Volc-Service，R-13）。
//
// ⚠️ 2026-10-28 没能核对官方文档的当前版本（文档站与 API Explorer 都是前端渲染，没找到这个服务）。Action、版本、
// `Text` / `Duration` / `TaskID` / `AudioUrl` 出自 dev/01 §7（2026-10-27 读到的那一版）；**时长范围、查询的状态值、
// 地域与服务取值待核实**（SCR-7，dev/01 R-20）。所以：
//   - 时长取两处文档的交集 30~60 秒（一处写最长 60 秒、一处写 30~120 秒）：报价与受理按它截、按它拦；
//   - 状态值字符串与数字都认（字符串照 ExtractState 的词表；数字 2 完成、3 失败），其余当处理中、由死线兜底；
//   - 产物只认几条固定路径上的 `AudioUrl`，不走兜底扫描。
//
// ⚠️ 只生成纯音乐（不带人声）：参数里明确要「带人声」时在报价阶段就拒（ParamChecking），不是悄悄出一段纯音乐。
// ⚠️ 商用条款官方没写明（SCR-7）：确认之前这个模型在后台保持停用。
type volcMusic struct{}

const (
	volcMusicSubmitPath = "/?Action=GenBGM&Version=2024-08-12"
	volcMusicQueryPath  = "/?Action=QuerySong&Version=2024-08-12"
	// VolcMusicMinSeconds / VolcMusicMaxSeconds 单次时长（两处文档的交集，待核实）。
	VolcMusicMinSeconds = 30
	VolcMusicMaxSeconds = 60
)

func (volcMusic) Code() string            { return "volc_music" }
func (volcMusic) Async() bool             { return true }
func (volcMusic) RequiresQueryPath() bool { return false }

// SupportedOperations 实现 CapabilityDeclaring：只做文字生成配乐。
func (volcMusic) SupportedOperations() map[string]OperationSupport {
	return map[string]OperationSupport{"text2music": {Roles: []InputRole{RoleText}}}
}

// DurationRange 实现 DurationDeclaring。
func (volcMusic) DurationRange(operation string) (int, int, bool) {
	if operation != "text2music" {
		return 0, 0, false
	}
	return VolcMusicMinSeconds, VolcMusicMaxSeconds, true
}

// CheckParams 实现 ParamChecking：时长范围；明确要带人声的拒（这家只出纯音乐）。
func (volcMusic) CheckParams(operation string, durationSec int, params map[string]any) error {
	if operation != "text2music" {
		return nil
	}
	if v, ok := params["instrumental"].(bool); ok && !v {
		return fmt.Errorf("豆包音乐只生成纯音乐（不带人声）；要带人声的配乐请换一个模型")
	}
	return checkSeconds("豆包音乐", durationSec, VolcMusicMinSeconds, VolcMusicMaxSeconds)
}

func (p volcMusic) BuildSubmit(job Job) (Request, error) {
	if job.OperationCode != "text2music" {
		return Request{}, fmt.Errorf("豆包音乐不支持 %s", job.OperationCode)
	}
	text := strings.TrimSpace(job.Prompt)
	if text == "" {
		return Request{}, fmt.Errorf("配乐描述不能为空")
	}
	sec := paramInt(job.Params, "duration", VolcMusicMinSeconds)
	if err := p.CheckParams(job.OperationCode, sec, job.Params); err != nil {
		return Request{}, err
	}
	return Request{Method: "POST", Path: volcMusicSubmitPath, Body: mustJSON(map[string]any{"Text": text, "Duration": sec})}, nil
}

func (volcMusic) ParseSubmit(status int, body []byte) (Result, error) {
	root, err := volcOpenAPIResult(status, body, "提交配乐")
	if err != nil {
		return Result{}, err
	}
	id := scalarString(root["TaskID"])
	if id == "" {
		return Result{}, fmt.Errorf("豆包音乐没有返回任务 ID: %s", bodySnippet(body))
	}
	return Result{Async: true, TaskID: id}, nil
}

func (volcMusic) BuildQuery(_ Job, taskID string) (Request, bool) {
	return Request{Method: "POST", Path: volcMusicQueryPath, Body: mustJSON(map[string]any{"TaskID": taskID})}, true
}

func (volcMusic) ParseQuery(status int, body []byte) (Result, error) {
	res, err := volcOpenAPIResult(status, body, "查询配乐")
	if err != nil {
		// 查询失败不判任务失败（上游可能只是这一次没回）：返回 error，下一轮再查，由死线兜底。
		return Result{}, err
	}
	switch volcMusicState(res["Status"]) {
	case StateSuccess:
		u := volcMusicAudioURL(res)
		if u == "" {
			return Result{Async: true, State: StateFailed,
				Message: "豆包音乐任务完成但没有返回音频地址（AudioUrl）: " + bodySnippet(body)}, nil
		}
		return Result{Async: true, State: StateSuccess, Outputs: []Output{{Kind: "url", Value: u, MIME: "audio/mpeg"}}}, nil
	case StateFailed:
		msg := firstString(asObject(res["FailureReason"]), "Msg", "Message", "msg")
		if msg == "" {
			msg = bodySnippet(body)
		}
		return Result{Async: true, State: StateFailed, Message: "配乐生成失败：" + msg}, nil
	}
	return Result{Async: true, State: StatePending}, nil
}

// volcOpenAPIResult 火山 OpenAPI 的响应：错误在 `ResponseMetadata.Error`（HTTP 码可能是 200 也可能是 4xx / 5xx），
// 正文在 `Result`。错误按错误码折成分类用的状态码（StatusError）。
func volcOpenAPIResult(status int, body []byte, what string) (map[string]any, error) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		if status >= 400 {
			return nil, fmt.Errorf("%s失败：上游返回 HTTP %d: %s", what, status, bodySnippet(body))
		}
		return nil, fmt.Errorf("%s失败：响应不是 JSON: %s", what, bodySnippet(body))
	}
	if e := asObject(asObject(root["ResponseMetadata"])["Error"]); len(e) > 0 {
		code := scalarString(e["Code"])
		msg := firstString(e, "Message", "message")
		cls := volcOpenAPIErrorStatus(code)
		if status >= 500 && cls == 400 {
			cls = status // 服务端错误码没认出来时，HTTP 5xx 照旧可重试
		}
		return nil, &StatusError{Status: cls, Msg: fmt.Sprintf("%s失败（%s）：%s", what, code, msg)}
	}
	if status >= 400 {
		return nil, fmt.Errorf("%s失败：上游返回 HTTP %d: %s", what, status, bodySnippet(body))
	}
	res := asObject(root["Result"])
	if res == nil {
		return nil, fmt.Errorf("%s失败：响应里没有 Result: %s", what, bodySnippet(body))
	}
	return res, nil
}

// volcOpenAPIErrorStatus 火山 OpenAPI 的错误码折成分类用的状态码：限流 429、服务端忙 / 内部错误 503，其余 400。
// ⚠️ 这个服务的错误码表待核实（SCR-7），按 OpenAPI 公共错误码的命名判。
func volcOpenAPIErrorStatus(code string) int {
	c := strings.ToLower(code)
	switch {
	case strings.Contains(c, "flowlimit"), strings.Contains(c, "limitexceeded") && !strings.Contains(c, "quota"),
		strings.Contains(c, "throttl"), strings.Contains(c, "toomany"):
		return 429
	case strings.Contains(c, "internal"), strings.Contains(c, "unavailable"), strings.Contains(c, "timeout"),
		strings.Contains(c, "busy"):
		return 503
	}
	return 400
}

// volcMusicState 查询结果的状态：字符串按 ExtractState 的词表，数字 2 完成、3 失败（待核实），其余当处理中。
func volcMusicState(v any) TaskState {
	switch x := v.(type) {
	case float64:
		switch int(x) {
		case 2:
			return StateSuccess
		case 3:
			return StateFailed
		}
		return StatePending
	case string:
		b, _ := json.Marshal(map[string]any{"status": x})
		return ExtractState(b)
	}
	return StatePending
}

// volcMusicAudioURL 完成任务的音频地址：`SongDetail.AudioUrl`，或者直接在 `Result` 上。
func volcMusicAudioURL(res map[string]any) string {
	for _, m := range []map[string]any{asObject(res["SongDetail"]), res} {
		if u, _ := m["AudioUrl"].(string); strings.TrimSpace(u) != "" {
			return strings.TrimSpace(u)
		}
	}
	return ""
}
