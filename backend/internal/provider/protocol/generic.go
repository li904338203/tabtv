package protocol

import (
	"encoding/json"
)

func init() {
	register(genericSync{})
	register(genericAsync{})
	register(devFake{})
}

// envelope 是本平台的自有信封。
//
// ⚠️ **`generic_*` 假定上游是兼容本平台信封的中转**，不是任意厂商的原生 API
// （这一条老系统吃过亏）：拿它去接原生 API 一定拼不出来，因为请求体形状对不上。
// 换一家厂商基本都要**新加一个协议**，而不是复用 generic。
type envelope struct {
	Operation      string         `json:"operation"`
	Model          string         `json:"model"`
	ResultType     string         `json:"resultType"`
	IdempotencyKey string         `json:"idempotencyKey,omitempty"`
	Prompt         string         `json:"prompt,omitempty"`
	System         string         `json:"system,omitempty"`
	Images         []string       `json:"images,omitempty"`
	Params         map[string]any `json:"params,omitempty"`
	Options        map[string]any `json:"options,omitempty"`
}

func buildEnvelope(job Job) []byte {
	return mustJSON(envelope{
		Operation: job.OperationCode, Model: job.UpstreamModel,
		ResultType: job.ResultType, IdempotencyKey: job.IdempotencyKey,
		Prompt: job.Prompt, System: job.System, Images: job.Images,
		Params: job.Params, Options: job.Options,
	})
}

// ---------------------------------------------------------------- generic_sync

type genericSync struct{}

func (genericSync) Code() string            { return "generic_sync" }
func (genericSync) Async() bool             { return false }
func (genericSync) RequiresQueryPath() bool { return false }

func (genericSync) BuildSubmit(job Job) (Request, error) {
	return Request{Method: "POST", Path: "", Body: buildEnvelope(job)}, nil
}

func (genericSync) ParseSubmit(status int, body []byte) (Result, error) {
	if status >= 400 {
		return Result{}, httpError(status, body)
	}
	outs := ExtractOutputs(body)
	if len(outs) == 0 {
		// ⚠️ "成功但没有产物"要**说清楚**：老系统这里只记一句
		// 「任务完成但未返回产物」，排查时看起来像上游的问题，
		// 实际上多半是解析没接上（见 ExtractOutputs 的注释）。
		return Result{Message: "上游返回成功但未解析出产物，请核对该协议的产物路径"}, nil
	}
	return Result{Outputs: outs}, nil
}

func (genericSync) BuildQuery(Job, string) (Request, bool) { return Request{}, false }
func (genericSync) ParseQuery(int, []byte) (Result, error) { return Result{}, nil }

// ---------------------------------------------------------------- generic_async

type genericAsync struct{}

func (genericAsync) Code() string { return "generic_async" }
func (genericAsync) Async() bool  { return true }

// RequiresQueryPath 异步协议**必须**配查询路径。
//
// ⚠️ 不强制的话，模型能存进去、后台一切正常，跑起来才发现没法轮询
// —— 而那时任务已经扣过费、已经提交给上游了。
func (genericAsync) RequiresQueryPath() bool { return true }

func (genericAsync) BuildSubmit(job Job) (Request, error) {
	return Request{Method: "POST", Path: "", Body: buildEnvelope(job)}, nil
}

func (genericAsync) ParseSubmit(status int, body []byte) (Result, error) {
	if status >= 400 {
		return Result{}, httpError(status, body)
	}
	taskID := ExtractTaskID(body)
	if taskID == "" {
		// 没拿到任务 ID 但 HTTP 是 2xx：可能上游其实是同步返回的。
		if outs := ExtractOutputs(body); len(outs) > 0 {
			return Result{Outputs: outs}, nil
		}
		return Result{}, httpError(status, body)
	}
	return Result{Async: true, TaskID: taskID}, nil
}

func (genericAsync) BuildQuery(job Job, taskID string) (Request, bool) {
	// 查询路径由 Provider 配置提供，这里只给出任务 ID 的位置；
	// 调用方把 {taskId} 占位符替换掉。
	return Request{Method: "GET", Path: "{taskId}", Header: map[string]string{}}, true
}

func (genericAsync) ParseQuery(status int, body []byte) (Result, error) {
	if status >= 400 {
		return Result{}, httpError(status, body)
	}
	state := ExtractState(body)
	res := Result{Async: true, State: state}
	switch state {
	case StateSuccess:
		res.Outputs = ExtractOutputs(body)
		if len(res.Outputs) == 0 {
			res.State = StateFailed
			res.Message = "上游报告成功但未解析出产物，请核对该协议的产物路径"
		}
	case StateFailed:
		res.Message = extractMessage(body)
	}
	return res, nil
}

func extractMessage(body []byte) string {
	var tree map[string]any
	if err := json.Unmarshal(body, &tree); err != nil {
		return ""
	}
	for _, k := range []string{"message", "error", "error_message", "reason", "detail"} {
		switch v := tree[k].(type) {
		case string:
			if v != "" {
				return v
			}
		case map[string]any:
			if s, ok := v["message"].(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------- dev_fake

type devFake struct{}

func (devFake) Code() string            { return "dev_fake" }
func (devFake) Async() bool             { return false }
func (devFake) RequiresQueryPath() bool { return false }

func (devFake) BuildSubmit(job Job) (Request, error) {
	return Request{Method: "POST", Path: "", Body: buildEnvelope(job)}, nil
}

func (devFake) ParseSubmit(int, []byte) (Result, error) {
	return Result{Outputs: []Output{{Kind: "text", Value: "dev 假适配器产物"}}}, nil
}

func (devFake) BuildQuery(Job, string) (Request, bool) { return Request{}, false }
func (devFake) ParseQuery(int, []byte) (Result, error) { return Result{}, nil }
