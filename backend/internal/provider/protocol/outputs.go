// Package protocol 是各上游协议的请求构造与响应解析。
//
// ⚠️ **协议是硬枚举**：加一个协议要同步改四处（迁移里的 CHECK、DTO 的校验、
// 服务端的 domain 校验集、这里的注册表），漏一处的症状各不相同 ——
// 最隐蔽的是只漏了这里：后台一切正常，**Worker 执行时才报「不支持的协议」**。
// 因此注册表是**唯一事实来源**，启动时同步进库（GAP-09）。
package protocol

import (
	"encoding/json"
	"strings"
)

// Output 是从上游响应里解析出的一个产物。
type Output struct {
	// Kind 取值：url / base64 / text。
	Kind string
	// URL 形态：需要下载；Base64 形态：直接解码；Text 形态：就是内容本身。
	Value string
	MIME  string
	// Meta 是产物的结构化附加信息（jsonb），原样落进 generation_output.meta（SCD-32）。
	// 语音识别的句与字时间就在这里：全文进文本、时间进 meta。为空时落 `{}`，与没有这个字段之前一致。
	Meta json.RawMessage
}

// candidatePaths 是产物可能出现的位置。
//
// ⚠️ 老系统在这里栽过：路径表里**没有 `content`**，而 Seedance 的视频在
// `content.video_url` —— 于是一个产物都扫不到，落到「任务完成但未返回产物」，
// 排查时看起来像上游的问题，其实是解析没接上。
//
// 教训不是「把 content 加进去就完了」，而是：**每接一个新协议都要单独核对
// 这张表扫不扫得到它的产物**，并补一条 fixture 测试。
var candidatePaths = []string{
	"outputs", "output", "data", "result", "results",
	"images", "image", "video", "videos", "audio",
	"content", // ⚠️ Seedance 的视频在这里，缺了就一个产物都找不到
	"artifacts", "files",
}

// urlKeys / base64Keys 是叶子节点上可能的字段名。
var (
	urlKeys    = []string{"url", "video_url", "image_url", "audio_url", "file_url", "download_url", "uri"}
	base64Keys = []string{"b64_json", "base64", "data", "b64"}
	textKeys   = []string{"text", "content", "output_text"}
)

// ExtractOutputs 从任意上游响应里尽力扫出产物。
//
// ⚠️ 这是**兜底**，不是主路径：每个协议应当先按自己的响应结构精确取，
// 取不到才回落到这里。全靠兜底的话，上游改个字段名我们就静默少了产物。
func ExtractOutputs(raw json.RawMessage) []Output {
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil
	}
	root, ok := tree.(map[string]any)
	if !ok {
		// 顶层就是数组的情况（有些上游直接回一个列表）。
		if arr, isArr := tree.([]any); isArr {
			return collectFrom(arr)
		}
		return nil
	}

	var out []Output
	for _, key := range candidatePaths {
		v, exists := root[key]
		if !exists {
			continue
		}
		out = append(out, collectFrom(v)...)
		if len(out) > 0 {
			// 找到第一个有产物的路径就停：继续扫会把同一个产物
			// 在不同别名下重复收一遍（很多上游同时给 `data` 与 `images`）。
			return out
		}
	}
	// 顶层直接挂着 url/b64 的情况。
	return collectFrom(root)
}

func collectFrom(v any) []Output {
	switch t := v.(type) {
	case string:
		if o, ok := fromString(t); ok {
			return []Output{o}
		}
	case []any:
		var out []Output
		for _, item := range t {
			out = append(out, collectFrom(item)...)
		}
		return out
	case map[string]any:
		for _, k := range urlKeys {
			if s, ok := t[k].(string); ok && s != "" {
				return []Output{{Kind: "url", Value: s, MIME: mimeFromURL(s)}}
			}
		}
		for _, k := range base64Keys {
			if s, ok := t[k].(string); ok && looksBase64(s) {
				return []Output{{Kind: "base64", Value: s}}
			}
		}
		for _, k := range textKeys {
			if s, ok := t[k].(string); ok && s != "" {
				return []Output{{Kind: "text", Value: s}}
			}
			// content 可能是数组（Anthropic 的 content blocks）。
			if arr, ok := t[k].([]any); ok {
				if got := collectFrom(arr); len(got) > 0 {
					return got
				}
			}
		}
	}
	return nil
}

func fromString(s string) (Output, bool) {
	switch {
	case strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://"):
		return Output{Kind: "url", Value: s, MIME: mimeFromURL(s)}, true
	case strings.HasPrefix(s, "data:"):
		// data:image/png;base64,xxxx
		if i := strings.Index(s, ";base64,"); i > 0 {
			return Output{Kind: "base64", Value: s[i+len(";base64,"):], MIME: s[5:i]}, true
		}
	case looksBase64(s):
		return Output{Kind: "base64", Value: s}, true
	}
	return Output{}, false
}

// looksBase64 粗判。
//
// ⚠️ 门槛定在 256 字符：短字符串（id、状态码、模型名）也符合 base64 字符集，
// 判低了会把 `"status":"succeeded"` 当成产物。
func looksBase64(s string) bool {
	if len(s) < 256 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') || c == '+' || c == '/' || c == '=' || c == '\n'
		if !ok {
			return false
		}
	}
	return true
}

func mimeFromURL(u string) string {
	// 去掉查询串再看后缀：签名链接后面常挂一长串参数。
	if i := strings.IndexAny(u, "?#"); i > 0 {
		u = u[:i]
	}
	i := strings.LastIndex(u, ".")
	if i < 0 {
		return ""
	}
	switch strings.ToLower(u[i:]) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".mp4":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".mp3":
		return "audio/mpeg"
	case ".wav":
		return "audio/wav"
	}
	return ""
}

// ExtractTaskID 从异步提交的响应里找上游任务 ID。
func ExtractTaskID(raw json.RawMessage) string {
	var tree map[string]any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return ""
	}
	// 常见位置：顶层 id / task_id / request_id，或嵌在 data 下。
	for _, k := range []string{"task_id", "taskId", "id", "request_id", "requestId", "job_id"} {
		if s, ok := tree[k].(string); ok && s != "" {
			return s
		}
	}
	if data, ok := tree["data"].(map[string]any); ok {
		for _, k := range []string{"task_id", "taskId", "id", "request_id"} {
			if s, ok := data[k].(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// TaskState 是异步任务在上游的状态。
type TaskState string

const (
	StatePending TaskState = "pending"
	StateSuccess TaskState = "success"
	StateFailed  TaskState = "failed"
)

// ExtractState 判定异步任务状态。
//
// ⚠️ **未知状态一律按 pending 处理**，不按失败：上游加一个新状态值
// （比如排队细分成 queued/starting）时，按失败处理会把正常任务全判失败，
// 而按 pending 最多是多轮询几次直到死线。
func ExtractState(raw json.RawMessage) TaskState {
	var tree map[string]any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return StatePending
	}
	for _, k := range []string{"status", "state", "task_status"} {
		v, ok := tree[k].(string)
		if !ok {
			if data, isMap := tree["data"].(map[string]any); isMap {
				v, ok = data[k].(string)
			}
		}
		if !ok || v == "" {
			continue
		}
		switch strings.ToLower(v) {
		// ⚠️ 词表要逐家核对：可灵回的是 `succeed`（没有 ed），
		// 少一个词的后果不是报错，而是任务**永远停在 pending** 直到撞死线 ——
		// 上游其实早就出片了，用户只看到一个转了几分钟然后超时的任务。
		case "succeeded", "succeed", "success", "completed", "complete",
			"finished", "done", "ok":
			return StateSuccess
		case "failed", "fail", "failure", "error", "cancelled", "canceled", "rejected":
			return StateFailed
		}
		return StatePending
	}
	return StatePending
}
