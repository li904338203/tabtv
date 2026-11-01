package main

// 智能剪辑的语音识别（docs/agent/smartcut SCT-33；SCT-42 的 E2E 复用）。
//
// 三条路径，各按自家的形状校验一遍再回（只校验**形状**，证明「我们拼对了」）：
//
//   - 火山极速版 `POST /api/v3/auc/bigmodel/recognize/flash`：同步；audio.data（base64）或 audio.url；
//   - 火山标准版 `POST /api/v3/auc/bigmodel/submit` + `POST /api/v3/auc/bigmodel/query`：异步、只收 audio.url；
//     任务 ID 就是请求头 `X-Api-Request-Id`（查询时必须带同一个），第一次查询回「处理中」，第二次回结果；
//   - OpenAI 转写 `POST /v1/audio/transcriptions`：multipart；whisper-1 + verbose_json + 词与句两种粒度时回词级，
//     其它模型回整段文字。
//
// 火山的状态放在响应头 `X-Api-Status-Code`，HTTP 一律 200（与真实上游一致：只看 HTTP 码的话分不出处理中与失败）。
// 形状不对时回 45000001 并在 X-Api-Message 里说清缺什么。
//
// 下一次回什么由探针设：`POST /_probe/asr/next?outcome=silent|param_error|busy|error|talk`（一次有效；talk = 口播精剪的
// 字级结果，error 只对 OpenAI 转写生效），
// 请求记录在 `GET /_probe/asr`（协议、资源 ID、请求 ID、认证头、音频形态、是否开顺滑、OpenAI 的格式与粒度）。

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const asrProbeKeep = 50

// asrRecord 是一次识别请求里与断言有关的部分。
type asrRecord struct {
	Seq            int64    `json:"seq"`
	Protocol       string   `json:"protocol"`
	ResourceID     string   `json:"resourceId,omitempty"`
	RequestID      string   `json:"requestId,omitempty"`
	APIKey         bool     `json:"apiKey"`
	Audio          string   `json:"audio"` // data / url / file
	AudioBytes     int      `json:"audioBytes"`
	Format         string   `json:"format,omitempty"`
	EnableDDC      *bool    `json:"enableDdc,omitempty"`
	ShowUtterances *bool    `json:"showUtterances,omitempty"`
	Language       string   `json:"language,omitempty"`
	Model          string   `json:"model,omitempty"`
	ResponseFormat string   `json:"responseFormat,omitempty"`
	Granularities  []string `json:"granularities,omitempty"`
	Outcome        string   `json:"outcome"`
	At             string   `json:"at"`
}

type asrState struct {
	mu    sync.Mutex
	seq   atomic.Int64
	recs  []asrRecord
	next  string
	tasks map[string]int // 标准版：请求 ID → 已查询次数
}

func (a *asrState) record(r asrRecord) {
	r.Seq, r.At = a.seq.Add(1), time.Now().UTC().Format(time.RFC3339Nano)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.recs = append(a.recs, r)
	if len(a.recs) > asrProbeKeep {
		a.recs = append([]asrRecord(nil), a.recs[len(a.recs)-asrProbeKeep:]...)
	}
}

// takeNext 取走探针设的下一次结果（一次有效）。
func (a *asrState) takeNext() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := a.next
	a.next = ""
	return n
}

func (a *asrState) handleProbe(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	writeJSON(w, map[string]any{"items": append([]asrRecord{}, a.recs...)})
}

func (a *asrState) handleNext(w http.ResponseWriter, r *http.Request) {
	out := r.URL.Query().Get("outcome")
	switch out {
	case "silent", "param_error", "busy", "error", "talk":
	default:
		http.Error(w, "outcome 只能是 silent / param_error / busy / error / talk", http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	a.next = out
	a.mu.Unlock()
	writeJSON(w, map[string]any{"next": out})
}

// volcResult 是一份固定的字级识别结果（毫秒）。
const volcResult = `{"audio_info":{"duration":3600},"result":{"text":"嗯，大家好，今天讲剪辑。","utterances":[
 {"text":"嗯，大家好，","start_time":0,"end_time":1700,"definite":true,"words":[
  {"text":"嗯","start_time":120,"end_time":360},{"text":"大","start_time":740,"end_time":860},
  {"text":"家","start_time":860,"end_time":1020},{"text":"好","start_time":1020,"end_time":1300}]},
 {"text":"今天讲剪辑。","start_time":1900,"end_time":3500,"definite":true,"words":[
  {"text":"今","start_time":1900,"end_time":2080},{"text":"天","start_time":2080,"end_time":2260},
  {"text":"讲","start_time":2400,"end_time":2600},{"text":"剪","start_time":2700,"end_time":2950},
  {"text":"辑","start_time":2950,"end_time":3400}]}]}}`

const volcSilent = `{"audio_info":{"duration":3600},"result":{"text":"","utterances":[]}}`

// volcTalk 是口播精剪的字级结果（`outcome=talk`）：3 次「嗯」+ 一句重复，10 秒（SC-AC-42 的端到端输入）。
// 与 api/fixtures/smartcut/asr/volc_talk_fillers.json 的 body 逐字一致（TestVolcTalk_与fixture一致 钉住）。
const volcTalk = `{"audio_info":{"duration":10000},"result":{"text":"嗯，大家好，今天讲剪辑。今天讲剪辑。嗯，第一步，嗯，导入素材。","utterances":[{"text":"嗯，大家好，","start_time":100,"end_time":1200,"definite":true,"words":[{"text":"嗯","start_time":100,"end_time":300,"blank_duration":0},{"text":"大","start_time":600,"end_time":800,"blank_duration":0},{"text":"家","start_time":800,"end_time":1000,"blank_duration":0},{"text":"好","start_time":1000,"end_time":1200,"blank_duration":0}]},{"text":"今天讲剪辑。","start_time":1500,"end_time":2500,"definite":true,"words":[{"text":"今","start_time":1500,"end_time":1700,"blank_duration":0},{"text":"天","start_time":1700,"end_time":1900,"blank_duration":0},{"text":"讲","start_time":1900,"end_time":2100,"blank_duration":0},{"text":"剪","start_time":2100,"end_time":2300,"blank_duration":0},{"text":"辑","start_time":2300,"end_time":2500,"blank_duration":0}]},{"text":"今天讲剪辑。","start_time":3300,"end_time":4300,"definite":true,"words":[{"text":"今","start_time":3300,"end_time":3500,"blank_duration":0},{"text":"天","start_time":3500,"end_time":3700,"blank_duration":0},{"text":"讲","start_time":3700,"end_time":3900,"blank_duration":0},{"text":"剪","start_time":3900,"end_time":4100,"blank_duration":0},{"text":"辑","start_time":4100,"end_time":4300,"blank_duration":0}]},{"text":"嗯，第一步，嗯，导入素材。","start_time":4900,"end_time":7600,"definite":true,"words":[{"text":"嗯","start_time":4900,"end_time":5100,"blank_duration":0},{"text":"第","start_time":5400,"end_time":5600,"blank_duration":0},{"text":"一","start_time":5600,"end_time":5800,"blank_duration":0},{"text":"步","start_time":5800,"end_time":6000,"blank_duration":0},{"text":"嗯","start_time":6300,"end_time":6500,"blank_duration":0},{"text":"导","start_time":6800,"end_time":7000,"blank_duration":0},{"text":"入","start_time":7000,"end_time":7200,"blank_duration":0},{"text":"素","start_time":7200,"end_time":7400,"blank_duration":0},{"text":"材","start_time":7400,"end_time":7600,"blank_duration":0}]}]}}`

// volcRequest 校验火山识别的请求形状；返回记录与不合格的原因（空串 = 合格）。
func volcRequest(r *http.Request, protocol string, urlOnly bool) (asrRecord, string) {
	rec := asrRecord{Protocol: protocol, ResourceID: r.Header.Get("X-Api-Resource-Id"),
		RequestID: r.Header.Get("X-Api-Request-Id"), APIKey: r.Header.Get("X-Api-Key") != ""}
	switch {
	case !rec.APIKey:
		return rec, "缺少 X-Api-Key"
	case rec.ResourceID == "":
		return rec, "缺少 X-Api-Resource-Id"
	case len(rec.RequestID) != 36:
		return rec, "X-Api-Request-Id 要是 UUID"
	case r.Header.Get("X-Api-Sequence") != "-1":
		return rec, "X-Api-Sequence 要是 -1"
	}
	if strings.HasSuffix(r.URL.Path, "/query") {
		return rec, ""
	}
	var body struct {
		User    map[string]any `json:"user"`
		Audio   map[string]any `json:"audio"`
		Request map[string]any `json:"request"`
	}
	raw, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(raw, &body); err != nil {
		return rec, "正文不是 JSON"
	}
	if body.Request["model_name"] != "bigmodel" {
		return rec, "request.model_name 要是 bigmodel"
	}
	if v, ok := body.Request["enable_ddc"].(bool); ok {
		rec.EnableDDC = &v
	}
	if v, ok := body.Request["show_utterances"].(bool); ok {
		rec.ShowUtterances = &v
	}
	rec.Language, _ = body.Request["language"].(string)
	rec.Format, _ = body.Audio["format"].(string)
	data, _ := body.Audio["data"].(string)
	url, _ := body.Audio["url"].(string)
	switch {
	case data != "" && urlOnly:
		return rec, "标准版只收 audio.url"
	case data != "":
		b, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return rec, "audio.data 不是 base64（要去掉 data URI 的前缀）"
		}
		rec.Audio, rec.AudioBytes = "data", len(b)
	case strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://"):
		rec.Audio = "url"
	default:
		return rec, "audio.data 与 audio.url 都没有"
	}
	if rec.ShowUtterances == nil || !*rec.ShowUtterances {
		return rec, "要 show_utterances=true 才有句与字"
	}
	return rec, ""
}

func volcStatus(w http.ResponseWriter, code, msg string) {
	w.Header().Set("X-Api-Status-Code", code)
	w.Header().Set("X-Api-Message", msg)
	w.Header().Set("X-Tt-Logid", "fake-logid")
	w.Header().Set("Content-Type", "application/json")
}

// volcOutcome 把探针设的结果翻成火山的状态码与正文。
func volcOutcome(next string) (code, msg, body string) {
	switch next {
	case "silent":
		return "20000003", "silent audio", volcSilent
	case "param_error":
		return "45000151", "audio format error", "{}"
	case "busy":
		return "55000031", "server busy", "{}"
	case "talk":
		return "20000000", "OK", volcTalk
	}
	return "20000000", "OK", volcResult
}

func (s *server) volcFlash(w http.ResponseWriter, r *http.Request) {
	rec, bad := volcRequest(r, "volc_asr", false)
	if bad != "" {
		rec.Outcome = "bad_request"
		s.asr.record(rec)
		volcStatus(w, "45000001", bad)
		_, _ = io.WriteString(w, "{}")
		return
	}
	code, msg, body := volcOutcome(s.asr.takeNext())
	rec.Outcome = code
	s.asr.record(rec)
	log.Printf("  火山识别（极速版）：%s 音频 %s %d 字节 → %s", rec.ResourceID, rec.Audio, rec.AudioBytes, code)
	volcStatus(w, code, msg)
	_, _ = io.WriteString(w, body)
}

func (s *server) volcSubmit(w http.ResponseWriter, r *http.Request) {
	rec, bad := volcRequest(r, "volc_asr_standard", true)
	if bad != "" {
		rec.Outcome = "bad_request"
		s.asr.record(rec)
		volcStatus(w, "45000001", bad)
		_, _ = io.WriteString(w, "{}")
		return
	}
	// 探针设的结果不在这里取：标准版的成败在查询时才知道。
	s.asr.mu.Lock()
	if s.asr.tasks == nil {
		s.asr.tasks = map[string]int{}
	}
	s.asr.tasks[rec.RequestID] = 0
	s.asr.mu.Unlock()
	rec.Outcome = "submitted"
	s.asr.record(rec)
	volcStatus(w, "20000000", "OK")
	_, _ = io.WriteString(w, "{}")
}

func (s *server) volcQuery(w http.ResponseWriter, r *http.Request) {
	rec, bad := volcRequest(r, "volc_asr_standard", true)
	rec.Audio = "query"
	s.asr.mu.Lock()
	n, known := s.asr.tasks[rec.RequestID]
	if known {
		s.asr.tasks[rec.RequestID] = n + 1
	}
	s.asr.mu.Unlock()
	switch {
	case bad != "":
		volcStatus(w, "45000001", bad)
		_, _ = io.WriteString(w, "{}")
		rec.Outcome = "bad_request"
	case !known:
		volcStatus(w, "45000001", "不认识的 X-Api-Request-Id（查询要带提交时的那一个）")
		_, _ = io.WriteString(w, "{}")
		rec.Outcome = "unknown_task"
	case n == 0:
		volcStatus(w, "20000001", "processing")
		_, _ = io.WriteString(w, "{}")
		rec.Outcome = "20000001"
	default:
		code, msg, body := volcOutcome(s.asr.takeNext())
		volcStatus(w, code, msg)
		_, _ = io.WriteString(w, body)
		rec.Outcome = code
	}
	s.asr.record(rec)
}

// whisperVerbose 是 whisper-1 的 verbose_json（秒）。
const whisperVerbose = `{"task":"transcribe","language":"chinese","duration":3.6,"text":"大家好，今天讲剪辑。",
 "words":[{"word":"大家","start":0.74,"end":1.02},{"word":"好","start":1.02,"end":1.3},
  {"word":"今天","start":1.9,"end":2.26},{"word":"讲","start":2.4,"end":2.6},{"word":"剪辑","start":2.7,"end":3.4}],
 "segments":[{"id":0,"start":0.0,"end":1.7,"text":"大家好，"},{"id":1,"start":1.9,"end":3.5,"text":"今天讲剪辑。"}]}`

func (s *server) openAITranscribe(w http.ResponseWriter, r *http.Request) {
	rec := asrRecord{Protocol: "openai_transcribe", APIKey: strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ")}
	fail := func(status int, msg string) {
		rec.Outcome = "bad_request"
		s.asr.record(rec)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg, "type": "invalid_request_error"}})
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		fail(http.StatusBadRequest, "要 multipart/form-data："+err.Error())
		return
	}
	rec.Model = r.FormValue("model")
	rec.ResponseFormat = r.FormValue("response_format")
	rec.Language = r.FormValue("language")
	rec.Granularities = r.MultipartForm.Value["timestamp_granularities[]"]
	f, hdr, err := r.FormFile("file")
	if err != nil {
		fail(http.StatusBadRequest, "缺少 file")
		return
	}
	defer f.Close()
	b, _ := io.ReadAll(f)
	rec.Audio, rec.AudioBytes, rec.Format = "file", len(b), hdr.Filename
	if !strings.Contains(hdr.Filename, ".") {
		fail(http.StatusBadRequest, "文件名要带扩展名（上游按扩展名认格式）")
		return
	}
	if s.asr.takeNext() == "error" {
		rec.Outcome = "error"
		s.asr.record(rec)
		writeJSON(w, map[string]any{"error": map[string]any{"message": "Audio file is too short.", "type": "invalid_request_error"}})
		return
	}
	if rec.Model == "whisper-1" {
		if rec.ResponseFormat != "verbose_json" || strings.Join(rec.Granularities, ",") != "word,segment" {
			fail(http.StatusBadRequest, "whisper-1 要 verbose_json 与 word、segment 两种粒度")
			return
		}
		rec.Outcome = "verbose"
		s.asr.record(rec)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, whisperVerbose)
		return
	}
	rec.Outcome = "text"
	s.asr.record(rec)
	writeJSON(w, map[string]any{"text": "大家好，今天讲剪辑。"})
}
