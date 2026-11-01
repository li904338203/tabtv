package protocol

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Job 是交给协议的工作项（已完成归属校验、输入顺序确定、快照不可变）。
type Job struct {
	OperationCode string
	ModelCode     string
	// UpstreamModel 是上游侧的型号标识，与我们的 modelCode 可能不同。
	UpstreamModel  string
	ResultType     string
	IdempotencyKey string
	// Prompt 与 Inputs 由调用方从快照中取出。
	Prompt string
	// System 是系统提示词。
	//
	// ⚠️ 必须与 Prompt **分开**发，不能拼成一段：
	//   - 各家放的位置不同（OpenAI 是一条 role=system 的消息，
	//     Anthropic 是顶层的 `system` 字段，不接受 system 角色的消息）；
	//   - 拼成一段的话，模型会把系统指令当成用户内容的一部分来"创作"，
	//     而且运营调提示词时会把用户内容一起冲掉。
	System string
	// Images 是参考图（URL 或 data: 形态），按连线顺序排列。
	//
	// ⚠️ 图片反推这类操作**必须把画面交给模型**，漏传时模型只会回
	// 「您的消息中没有附带任何图片」——看起来像模型不行，其实是我们没发。
	Images []string
	// Media 是全部参考媒体（含音频、视频），顺序即连线顺序。
	//
	// 视频类协议要靠顺序区分首帧/尾帧、靠 Kind 区分参考图与参考视频，
	// 只有 Images 是不够的；文本/图片/TTS 协议用 Images 就行。
	Media []Media
	// Params 是用户参数（尺寸、时长、音色…）。协议自行按需取用。
	Params map[string]any
	// Options 是模型调用参数（temperature / top_p / max_tokens），透传。
	Options map[string]any
}

// Media 是一个已解析的参考媒体。
//
// ⚠️ Value 已经是**上游能直接用的形态**（https 地址或 data: 内联），
// 不是我们内部的 mediaId —— 适配器不查用户数据，也就无从把 ID 换成内容。
type Media struct {
	Kind  string // image / audio / video
	Order int
	Value string
	MIME  string
	// Role 是这条输入扮演的角色；目前只有 `mask`（蒙版：说的是「改哪一块」而不是「画什么」）。
	Role string
	// DurationMs 是音视频的时长（毫秒，上传时 ffprobe 探出来的）；0 表示未知。
	//
	// 有的上游要调用方自己报参考视频的总时长（UniArt 的 `input_video_duration`），
	// 协议拿不到媒体本身，只能靠这里。
	DurationMs int
}

// Request 是协议构造出的一次上游调用。
type Request struct {
	Method string
	// Path 相对 Provider 的 base URL。
	Path string
	Body []byte
	// ContentType 为空时默认 application/json。
	ContentType string
	Header      map[string]string
	// Stream 为真时走流式读取（SSE）。
	Stream bool
	// Fields / Files 非空表示这是 multipart 请求（图片编辑接口收的是文件不是 JSON）。
	// 此时 Body 为空，由调用方取到文件字节后用 EncodeMultipart 拼出来。
	Fields map[string]string
	// RepeatedFields 是同名要出现多次的表单字段（OpenAI 转写的 `timestamp_granularities[]` 要同时给 word 与 segment）。
	RepeatedFields map[string][]string
	Files          []FilePart
}

// IsMultipart 判断这次请求是否要走 multipart 编码。
func (r Request) IsMultipart() bool { return len(r.Files) > 0 }

// Result 是协议解析出的结果。
type Result struct {
	// Async 为真表示上游只回了任务 ID，要轮询。
	Async  bool
	TaskID string
	// Outputs 同步完成时的产物。
	Outputs []Output
	// State 轮询时的状态。
	State TaskState
	// Message 失败原因或附加说明。
	Message string
}

// Protocol 是一个上游协议的适配。
//
// ⚠️ 适配器**自己不查用户数据、不碰数据库**（08 §6）：
// 它拿到 Job、吐出 Request，再把响应翻成 Result。这样每个协议都能
// 用 fixture 单独测，不必起数据库。
type Protocol interface {
	Code() string
	// Async 表示这个协议的提交是否返回任务 ID。
	Async() bool
	// RequiresQueryPath 异步协议必须配查询路径，否则跑起来才发现没法轮询。
	RequiresQueryPath() bool
	// BuildSubmit 构造提交请求。
	BuildSubmit(job Job) (Request, error)
	// ParseSubmit 解析提交响应。
	ParseSubmit(status int, body []byte) (Result, error)
	// BuildQuery 构造查询请求；同步协议返回 false。
	BuildQuery(job Job, taskID string) (Request, bool)
	// ParseQuery 解析查询响应。
	ParseQuery(status int, body []byte) (Result, error)
}

// HeaderAware 由「结果有一部分在响应头里」的协议实现（docs/agent/smartcut SCD-38）。
//
// 火山语音识别的状态在 `X-Api-Status-Code` 头里，正文在处理中时可能是空的、HTTP 码一律 200 ——
// 只看状态码与正文的话，「还在处理」「静音」「参数错」三种结果长得一模一样。
// 实现了它的协议由调用方改调这两个方法（见 ParseSubmitResponse / ParseQueryResponse），
// 没实现的照旧调 ParseSubmit / ParseQuery，行为不变。
type HeaderAware interface {
	ParseSubmitWithHeaders(status int, header http.Header, body []byte) (Result, error)
	ParseQueryWithHeaders(status int, header http.Header, body []byte) (Result, error)
}

// ParseSubmitResponse 解析提交响应：协议要看响应头就把头交给它。
func ParseSubmitResponse(p Protocol, status int, header http.Header, body []byte) (Result, error) {
	if h, ok := p.(HeaderAware); ok {
		return h.ParseSubmitWithHeaders(status, header, body)
	}
	return p.ParseSubmit(status, body)
}

// ParseQueryResponse 解析查询响应：协议要看响应头就把头交给它。
func ParseQueryResponse(p Protocol, status int, header http.Header, body []byte) (Result, error) {
	if h, ok := p.(HeaderAware); ok {
		return h.ParseQueryWithHeaders(status, header, body)
	}
	return p.ParseQuery(status, body)
}

// PathRouter 由「提交地址随操作变化」的协议实现。
//
// ⚠️ 这是**配置里写不出来的差异**：同一个可灵模型，文生视频与图生视频是
// 两个端点。让运营为每个操作各配一个模型行也能绕过去，但那样同一个型号
// 会被拆成三行，价格、能力矩阵、可用性都要维护三遍。
type PathRouter interface {
	// SubmitPath 在配置的提交地址上改写出本次操作真正要打的地址。
	SubmitPath(job Job, configured string) (string, error)
}

// ResolveSubmitPath 取本次提交要用的路径。协议没实现 PathRouter 时原样返回。
func ResolveSubmitPath(p Protocol, job Job, configured string) (string, error) {
	r, ok := p.(PathRouter)
	if !ok {
		return configured, nil
	}
	return r.SubmitPath(job, configured)
}

var registry = map[string]Protocol{}

func register(p Protocol) { registry[p.Code()] = p }

// Get 按协议码取实现。
func Get(code string) (Protocol, bool) {
	p, ok := registry[code]
	return p, ok
}

// Lookup 按协议码取描述。
//
// ⚠️ 目录域的协议目录直接建在这上面：代码里没注册的协议不可能出现在
// 管理端下拉里，注册了的也不可能漏（GAP-09）。**不要在别处再维护一份名单**。
func Lookup(code string) (Descriptor, bool) {
	p, ok := registry[code]
	if !ok {
		return Descriptor{}, false
	}
	return Descriptor{Code: p.Code(), Async: p.Async(),
		RequiresQueryPath: p.RequiresQueryPath()}, true
}

// Descriptor 是给管理端下拉与入库同步用的协议描述。
type Descriptor struct {
	Code              string `json:"code"`
	Async             bool   `json:"isAsync"`
	RequiresQueryPath bool   `json:"requiresQueryPath"`
}

// All 返回全部已注册协议（按码排序，保证下拉顺序稳定）。
//
// ⚠️ 这是「代码里有的协议」的**唯一事实来源**，启动时同步进表 ——
// 旧系统靠人同步四处，结果下拉里漏了三个已实现的协议（GAP-09）。
func All() []Descriptor {
	out := make([]Descriptor, 0, len(registry))
	for _, p := range registry {
		out = append(out, Descriptor{
			Code: p.Code(), Async: p.Async(), RequiresQueryPath: p.RequiresQueryPath(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

// httpError 把非 2xx 翻成带响应体的错误，供上层分类与排查。
func httpError(status int, body []byte) error {
	if reason, ok := ContentPolicyRejection(body); ok {
		// 被上游内容审核拒绝：给一句看得懂的话，原话放在后面供排查（不再把整段 JSON 甩给用户）。
		return fmt.Errorf("上游内容审核未通过，请调整描述、换一个选项或换一张图后重试（%s）", reason)
	}
	msg := string(body)
	if len(msg) > 500 {
		msg = msg[:500] + "…"
	}
	return fmt.Errorf("上游返回 HTTP %d: %s", status, msg)
}

// contentPolicyMarkers 上游「内容审核拒绝」的特征词（OpenAI / 中转 / 其他厂商的常见写法）。
var contentPolicyMarkers = []string{"内容政策", "内容安全", "content_policy", "content policy", "safety system",
	"moderation_blocked", "moderation", "违反"}

// ContentPolicyRejection 判断响应是不是内容审核拒绝；是的话返回上游给的那句话（取 error.message / message，取不到用原文前 200 字）。
func ContentPolicyRejection(body []byte) (string, bool) {
	text := string(body)
	lower := strings.ToLower(text)
	hit := false
	for _, m := range contentPolicyMarkers {
		if strings.Contains(lower, strings.ToLower(m)) {
			hit = true
			break
		}
	}
	if !hit {
		return "", false
	}
	var r struct {
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &r) == nil {
		if r.Error.Message != "" {
			return r.Error.Message, true
		}
		if r.Message != "" {
			return r.Message, true
		}
	}
	if len([]rune(text)) > 200 {
		text = string([]rune(text)[:200]) + "…"
	}
	return text, true
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// 构造请求体失败属于编程错误：参数里混进了不可序列化的值。
		panic("协议构造请求体失败: " + err.Error())
	}
	return b
}

func itoa(v int) string { return strconv.Itoa(v) }

// paramString 从用户参数里取字符串，缺省回落。
func paramString(params map[string]any, key, fallback string) string {
	if v, ok := params[key].(string); ok && v != "" {
		return v
	}
	return fallback
}

// paramInt 从用户参数里取整数。⚠️ JSON 解出来的数字是 float64。
func paramInt(params map[string]any, key string, fallback int) int {
	switch v := params[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		// 参数定义里的默认值是 varchar，会以字符串形态传进来。
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return fallback
}
