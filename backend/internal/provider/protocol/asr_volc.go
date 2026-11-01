package protocol

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// 火山「大模型录音文件识别」（docs/agent/smartcut SCT-33，dev/01 §7）。两个版本各是一个协议：
//
//   - `volc_asr`：极速版，`POST /api/v3/auc/bigmodel/recognize/flash`，同步，音频可内联（`audio.data` base64）
//     也可给地址（`audio.url`）；
//   - `volc_asr_standard`：标准版，`POST .../submit` + `POST .../query`（正文 `{}`），异步，**只收地址**。
//
// 拆成两个协议而不是一个协议带开关：「同步还是异步」「收不收内联」都是协议级的事实（Async /
// MediaURLRequiring），一个协议说不清两种。
//
// 2026-10-28 核对的官方文档：
//   - 极速版 https://www.volcengine.com/docs/6561/1631584 ；
//   - 标准版 https://www.volcengine.com/docs/6561/1354868 。
//
// ⚠️ **状态在响应头 `X-Api-Status-Code`**，HTTP 200 不代表成功（协议实现 HeaderAware）：
//
//	20000000 完成 · 20000001 处理中 · 20000002 排队 · 20000003 静音（按成功、空结果）
//	45000xxx 参数类（不可重试；45000002「音频为空」按静音处理）· 55000xxx 服务端（可重试）
//
// ⚠️ 认证：新控制台是 `X-Api-Key` 单密钥，走现有的 `header` 认证（自定义头 `X-Auth-Header-Name: X-Api-Key`）。
// `X-Api-Resource-Id` 取模型的上游型号（极速版固定 `volc.bigasr.auc_turbo`；标准版 `volc.bigasr.auc` 或
// `volc.seedasr.auc`），没配时按版本给缺省。`X-Api-Request-Id` 由我们按任务的幂等键算（requestIDFor）。
// ⚠️ 要 `show_utterances=true` 才有句与字；口播精剪要保留口癖时**不开** `enable_ddc`（顺滑会删口癖）。

func init() {
	register(volcASR{})
	register(volcASRStandard{})
}

const (
	volcFlashPath    = "/api/v3/auc/bigmodel/recognize/flash"
	volcSubmitPath   = "/api/v3/auc/bigmodel/submit"
	volcQueryPath    = "/api/v3/auc/bigmodel/query"
	volcFlashRes     = "volc.bigasr.auc_turbo"
	volcStandardRes  = "volc.bigasr.auc"
	volcStatusHeader = "X-Api-Status-Code"
)

// ---------------------------------------------------------------- 极速版

type volcASR struct{}

func (volcASR) Code() string            { return "volc_asr" }
func (volcASR) Async() bool             { return false }
func (volcASR) RequiresQueryPath() bool { return false }

func (volcASR) BuildSubmit(job Job) (Request, error) {
	return volcRequest(job, volcFlashPath, volcFlashRes, false)
}

func (p volcASR) ParseSubmit(status int, body []byte) (Result, error) {
	return p.ParseSubmitWithHeaders(status, nil, body)
}

func (volcASR) ParseSubmitWithHeaders(status int, h http.Header, body []byte) (Result, error) {
	if status >= 400 {
		return Result{}, httpError(status, body)
	}
	switch code, msg := volcStatus(h, body); volcClass(code) {
	case volcDone, volcSilent:
		return volcOutputs(code, body)
	case volcPending:
		// 极速版是同步接口，不该回「处理中」；当成上游暂时忙，稍后重发（同一个请求 ID，上游按它去重）。
		return Result{}, &StatusError{Status: 503, Msg: "火山识别还在处理（极速版不应如此），稍后重试：" + msg}
	default:
		return Result{}, volcError(code, msg)
	}
}

func (volcASR) BuildQuery(Job, string) (Request, bool) { return Request{}, false }
func (volcASR) ParseQuery(int, []byte) (Result, error) { return Result{}, nil }

// ParseQueryWithHeaders 极速版是同步的，不会进入轮询（同 ParseQuery）；实现它只为凑齐 HeaderAware。
func (volcASR) ParseQueryWithHeaders(int, http.Header, []byte) (Result, error) { return Result{}, nil }

// ---------------------------------------------------------------- 标准版

type volcASRStandard struct{}

func (volcASRStandard) Code() string { return "volc_asr_standard" }
func (volcASRStandard) Async() bool  { return true }

// RequiresQueryPath 查询地址是固定的（BuildQuery 给出缺省），不强制运营再配一遍。
func (volcASRStandard) RequiresQueryPath() bool { return false }

// RequiresMediaURL 标准版只收 `audio.url`：本地存储签不出公网地址时走公网中转，否则提交前就报出原因。
func (volcASRStandard) RequiresMediaURL() bool { return true }

func (volcASRStandard) BuildSubmit(job Job) (Request, error) {
	return volcRequest(job, volcSubmitPath, volcStandardRes, true)
}

func (p volcASRStandard) ParseSubmit(status int, body []byte) (Result, error) {
	return p.ParseSubmitWithHeaders(status, nil, body)
}

// ParseSubmitWithHeaders 提交成功时正文是空对象。任务 ID 就是我们给的请求 ID —— 协议拿不到当时的请求头，
// 查询时按同一个规则再算一遍（BuildQuery 不看这里给的 TaskID），这里尽量记下上游回显的那个，便于排障。
func (volcASRStandard) ParseSubmitWithHeaders(status int, h http.Header, body []byte) (Result, error) {
	if status >= 400 {
		return Result{}, httpError(status, body)
	}
	code, msg := volcStatus(h, body)
	switch volcClass(code) {
	case volcDone, volcPending, volcUnknown:
		taskID := ""
		if h != nil {
			taskID = firstNonEmptyHeader(h, "X-Api-Request-Id", "X-Tt-Logid")
		}
		if taskID == "" {
			taskID = "submitted"
		}
		return Result{Async: true, TaskID: taskID}, nil
	default:
		return Result{}, volcError(code, msg)
	}
}

func (volcASRStandard) BuildQuery(job Job, _ string) (Request, bool) {
	req, err := volcRequest(job, volcQueryPath, volcStandardRes, true)
	if err != nil {
		// 查询不发媒体，构造不会失败；万一失败也给出一个带请求 ID 的空正文请求。
		req = Request{Method: "POST", Path: volcQueryPath}
	}
	req.Body = []byte(`{}`)
	return req, true
}

func (p volcASRStandard) ParseQuery(status int, body []byte) (Result, error) {
	return p.ParseQueryWithHeaders(status, nil, body)
}

// ParseQueryWithHeaders 处理中返回 pending；参数类错误是终态失败（重查也一样）；服务端错误返回 error，
// 让执行器下一轮再查（由死线兜底）。
func (volcASRStandard) ParseQueryWithHeaders(status int, h http.Header, body []byte) (Result, error) {
	if status >= 400 {
		return Result{}, httpError(status, body)
	}
	code, msg := volcStatus(h, body)
	switch volcClass(code) {
	case volcDone, volcSilent:
		res, err := volcOutputs(code, body)
		if err != nil {
			return Result{}, err
		}
		res.Async, res.State = true, StateSuccess
		return res, nil
	case volcPending:
		return Result{Async: true, State: StatePending}, nil
	case volcServer:
		return Result{}, volcError(code, msg)
	default:
		return Result{Async: true, State: StateFailed, Message: volcError(code, msg).Error()}, nil
	}
}

// ---------------------------------------------------------------- 共用

// volcRequest 构造提交（或查询）请求。查询也带同样的三个头：资源 ID、请求 ID、序号。
func volcRequest(job Job, path, defaultRes string, urlOnly bool) (Request, error) {
	res := strings.TrimSpace(job.UpstreamModel)
	if res == "" {
		res = defaultRes
	}
	header := map[string]string{
		"X-Api-Resource-Id": res,
		"X-Api-Request-Id":  requestIDFor(job),
		"X-Api-Sequence":    "-1",
	}
	if path == volcQueryPath {
		return Request{Method: "POST", Path: path, Body: []byte(`{}`), Header: header}, nil
	}
	m := firstAudio(job.Media)
	if m == nil {
		return Request{}, fmt.Errorf("语音识别需要一段音频输入")
	}
	audio := map[string]any{}
	if mime, b64, ok := splitDataURI(m.Value); ok {
		if urlOnly {
			return Request{}, fmt.Errorf("火山标准版只收音频地址，不收内联音频")
		}
		audio["data"], audio["format"] = b64, audioFormat(firstNonEmptyStr(m.MIME, mime))
	} else {
		audio["url"], audio["format"] = m.Value, audioFormat(m.MIME)
	}
	keepFillers := paramFlag(job.Params, ASRParamKeepFillers, false)
	request := map[string]any{
		"model_name":      "bigmodel",
		"enable_itn":      true,
		"enable_punc":     true,
		"enable_ddc":      !keepFillers,
		"show_utterances": true,
	}
	// 只有标准版文档列了 language；极速版自动判断（多给一个不认识的字段，上游可能按参数错误拒收）。
	if lang := volcLanguage(paramString(job.Params, ASRParamLanguage, "")); lang != "" && urlOnly {
		request["language"] = lang
	}
	body := mustJSON(map[string]any{"user": map[string]any{"uid": "seeaitv"}, "audio": audio, "request": request})
	return Request{Method: "POST", Path: path, Body: body, Header: header}, nil
}

// volcLanguage 把我们的语言码换成火山的（zh-CN / en-US / ja-JP / ko-KR）；不认识的不发。
func volcLanguage(lang string) string {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "zh", "zh-cn":
		return "zh-CN"
	case "en", "en-us":
		return "en-US"
	case "ja", "ja-jp":
		return "ja-JP"
	case "ko", "ko-kr":
		return "ko-KR"
	}
	return ""
}

type volcKind int

const (
	volcUnknown volcKind = iota
	volcDone
	volcPending
	volcSilent
	volcClient
	volcServer
)

func volcClass(code string) volcKind {
	switch {
	case code == "20000000":
		return volcDone
	case code == "20000001" || code == "20000002":
		return volcPending
	case code == "20000003" || code == "45000002":
		// 静音 / 音频为空：识别本身是成功的，结果为空（照常结算，界面提示「没有识别到语音」）。
		return volcSilent
	case strings.HasPrefix(code, "45"):
		return volcClient
	case strings.HasPrefix(code, "55"):
		return volcServer
	case code == "":
		return volcUnknown
	}
	return volcClient
}

// volcStatus 取状态码与消息：响应头优先；没有头（经过会丢头的中转）时退到正文里的同名字段，再退到「有结果就算完成」。
func volcStatus(h http.Header, body []byte) (code, msg string) {
	if h != nil {
		code, msg = strings.TrimSpace(h.Get(volcStatusHeader)), strings.TrimSpace(h.Get("X-Api-Message"))
	}
	if code != "" {
		return code, msg
	}
	var root struct {
		Code    json.Number `json:"code"`
		Message string      `json:"message"`
		Result  any         `json:"result"`
	}
	if json.Unmarshal(body, &root) == nil {
		if c := root.Code.String(); c != "" && c != "0" {
			return c, root.Message
		}
		if root.Result != nil {
			return "20000000", ""
		}
	}
	return "", msg
}

func volcError(code, msg string) error {
	if msg == "" {
		msg = "未说明原因"
	}
	switch volcClass(code) {
	case volcServer:
		return &StatusError{Status: 503, Msg: fmt.Sprintf("火山识别服务暂时不可用（%s）：%s", code, msg)}
	case volcUnknown:
		return &StatusError{Status: 502, Msg: "火山识别的响应里没有状态码：" + msg}
	}
	return &StatusError{Status: 400, Msg: fmt.Sprintf("火山识别拒绝了请求（%s）：%s", code, msg)}
}

// volcOutputs 解析结果。静音时上游可能不回结果，按空结果处理。
func volcOutputs(code string, body []byte) (Result, error) {
	var root struct {
		AudioInfo struct {
			Duration int `json:"duration"`
		} `json:"audio_info"`
		Result json.RawMessage `json:"result"`
	}
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &root); err != nil {
			if volcClass(code) == volcSilent {
				return Result{Outputs: []Output{ASRResult{Granularity: GranularityChar}.output()}}, nil
			}
			return Result{}, fmt.Errorf("火山识别的响应不是 JSON: %w", err)
		}
	}
	type word struct {
		Text      string `json:"text"`
		StartTime int    `json:"start_time"`
		EndTime   int    `json:"end_time"`
	}
	type utterance struct {
		Text      string `json:"text"`
		StartTime int    `json:"start_time"`
		EndTime   int    `json:"end_time"`
		Words     []word `json:"words"`
	}
	type result struct {
		Text       string      `json:"text"`
		Utterances []utterance `json:"utterances"`
		Additions  struct {
			Language string `json:"language"`
		} `json:"additions"`
	}
	var r result
	raw := strings.TrimSpace(string(root.Result))
	switch {
	case raw == "" || raw == "null":
	case strings.HasPrefix(raw, "["):
		// 部分版本把结果包成数组（多声道时一声道一项），取第一项。
		var list []result
		if err := json.Unmarshal(root.Result, &list); err != nil {
			return Result{}, fmt.Errorf("火山识别结果解析失败: %w", err)
		}
		if len(list) > 0 {
			r = list[0]
		}
	default:
		if err := json.Unmarshal(root.Result, &r); err != nil {
			return Result{}, fmt.Errorf("火山识别结果解析失败: %w", err)
		}
	}
	out := ASRResult{Text: strings.TrimSpace(r.Text), Language: r.Additions.Language,
		DurationMs: root.AudioInfo.Duration, Utterances: make([]ASRUtterance, 0, len(r.Utterances))}
	hasWords := false
	for _, u := range r.Utterances {
		item := ASRUtterance{Text: strings.TrimSpace(u.Text), StartMs: u.StartTime, EndMs: u.EndTime}
		for _, w := range u.Words {
			if strings.TrimSpace(w.Text) == "" {
				continue
			}
			item.Words = append(item.Words, ASRWord{Text: w.Text, StartMs: w.StartTime, EndMs: w.EndTime})
		}
		hasWords = hasWords || len(item.Words) > 0
		out.Utterances = append(out.Utterances, item)
	}
	switch {
	case hasWords:
		out.Granularity = GranularityChar
	case len(out.Utterances) > 0:
		out.Granularity = GranularitySegment
	default:
		// 静音或没有分句：标成字级（这家本来给得到字级），空结果不影响口播精剪的判断。
		out.Granularity = GranularityChar
	}
	if out.Text == "" && len(out.Utterances) > 0 {
		parts := make([]string, 0, len(out.Utterances))
		for _, u := range out.Utterances {
			parts = append(parts, u.Text)
		}
		out.Text = strings.Join(parts, "")
	}
	return Result{Outputs: []Output{out.output()}}, nil
}

func firstNonEmptyHeader(h http.Header, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(h.Get(k)); v != "" {
			return v
		}
	}
	return ""
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
