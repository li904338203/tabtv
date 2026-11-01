package protocol

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// OpenAI 转写（docs/agent/smartcut SCT-33，dev/01 §7）：`POST /audio/transcriptions`（base URL 带 /v1），multipart，同步。
//
//   - 只有 `whisper-1` 支持 `response_format=verbose_json` + `timestamp_granularities[]=word`，给到词级时间
//     （官方说明 2027-02-26 停用，SCR-6）；其它模型（gpt-4o-transcribe 等）只回整段文字，结果标 `granularity=text`，
//     只能做普通字幕、不能用于口播精剪。
//   - 时间单位是秒（浮点），入库换成整数毫秒。
//   - whisper 会吞掉口癖、静音段可能编出文字（只影响字幕质量，口播精剪要求字或词级时间戳 + 口癖）。
//   - 上传上限 25 MB（官方）；内联音频超过时在提交前报出，不发请求。

func init() { register(openAITranscribe{}) }

const (
	openAITranscribePath = "/audio/transcriptions"
	openAIUploadLimit    = 25 << 20
)

// openAIWordTimestamps 判断一个上游型号给不给词级时间戳。
func openAIWordTimestamps(model string) bool {
	return strings.TrimSpace(model) == "whisper-1" || model == ""
}

type openAITranscribe struct{}

func (openAITranscribe) Code() string            { return "openai_transcribe" }
func (openAITranscribe) Async() bool             { return false }
func (openAITranscribe) RequiresQueryPath() bool { return false }

func (openAITranscribe) BuildSubmit(job Job) (Request, error) {
	m := firstAudio(job.Media)
	if m == nil {
		return Request{}, fmt.Errorf("语音识别需要一段音频输入")
	}
	if _, b64, ok := splitDataURI(m.Value); ok && dataURISize(b64) > openAIUploadLimit {
		return Request{}, fmt.Errorf("音频有 %.1f MB，超过 OpenAI 转写的上传上限 25 MB", float64(dataURISize(b64))/(1<<20))
	}
	model := strings.TrimSpace(job.UpstreamModel)
	if model == "" {
		model = "whisper-1"
	}
	fields := map[string]string{"model": model}
	var repeated map[string][]string
	if openAIWordTimestamps(model) {
		fields["response_format"] = "verbose_json"
		// 同时要词与句：句给出分句，词挂到句下面。
		repeated = map[string][]string{"timestamp_granularities[]": {"word", "segment"}}
	} else {
		fields["response_format"] = "json"
	}
	if lang := openAILanguage(paramString(job.Params, ASRParamLanguage, "")); lang != "" {
		fields["language"] = lang
	}
	// ⚠️ 文件名的扩展名要对：上游按扩展名认格式，`audio` 这种没有扩展名的会被拒收。
	name := "audio." + audioFormat(m.MIME)
	return Request{Method: "POST", Path: openAITranscribePath, Fields: fields, RepeatedFields: repeated,
		Files: []FilePart{{Field: "file", Filename: name, Ref: m.Value}}}, nil
}

// openAILanguage 换成 ISO-639-1（zh / en / ja / ko）；不认识的不发（交给模型判断）。
func openAILanguage(lang string) string {
	l := strings.ToLower(strings.TrimSpace(lang))
	if i := strings.IndexByte(l, '-'); i > 0 {
		l = l[:i]
	}
	switch l {
	case "zh", "en", "ja", "ko":
		return l
	}
	return ""
}

func (openAITranscribe) ParseSubmit(status int, body []byte) (Result, error) {
	if status >= 400 {
		return Result{}, httpError(status, body)
	}
	var root struct {
		Text     string  `json:"text"`
		Language string  `json:"language"`
		Duration float64 `json:"duration"`
		Words    []struct {
			Word  string  `json:"word"`
			Start float64 `json:"start"`
			End   float64 `json:"end"`
		} `json:"words"`
		Segments []struct {
			Start float64 `json:"start"`
			End   float64 `json:"end"`
			Text  string  `json:"text"`
		} `json:"segments"`
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		return Result{}, fmt.Errorf("OpenAI 转写的响应不是 JSON: %w", err)
	}
	if root.Error != nil {
		// HTTP 200 + 错误对象（中转常见）：按参数类处理，重试也一样。
		return Result{}, &StatusError{Status: 400, Msg: "OpenAI 转写失败：" + strings.TrimSpace(root.Error.Message)}
	}
	out := ASRResult{Text: strings.TrimSpace(root.Text), Language: root.Language,
		DurationMs: secondsToMs(root.Duration), Utterances: []ASRUtterance{}}
	words := make([]ASRWord, 0, len(root.Words))
	for _, w := range root.Words {
		if t := strings.TrimSpace(w.Word); t != "" {
			words = append(words, ASRWord{Text: t, StartMs: secondsToMs(w.Start), EndMs: secondsToMs(w.End)})
		}
	}
	sort.SliceStable(words, func(i, j int) bool { return words[i].StartMs < words[j].StartMs })
	for _, s := range root.Segments {
		out.Utterances = append(out.Utterances, ASRUtterance{Text: strings.TrimSpace(s.Text),
			StartMs: secondsToMs(s.Start), EndMs: secondsToMs(s.End)})
	}
	switch {
	case len(words) > 0:
		out.Granularity = GranularityWord
		out.Utterances = attachWords(out.Utterances, words)
	case len(out.Utterances) > 0:
		out.Granularity = GranularitySegment
	default:
		out.Granularity = GranularityText
	}
	return Result{Outputs: []Output{out.output()}}, nil
}

// attachWords 把词挂到句下面：词的起点落在哪一句里就归哪一句；落在句与句之间的归前一句。
// 没有分句时整段当一句（起止取首尾两个词）。
func attachWords(utts []ASRUtterance, words []ASRWord) []ASRUtterance {
	if len(utts) == 0 {
		texts := make([]string, 0, len(words))
		for _, w := range words {
			texts = append(texts, w.Text)
		}
		return []ASRUtterance{{Text: strings.Join(texts, " "), StartMs: words[0].StartMs,
			EndMs: words[len(words)-1].EndMs, Words: words}}
	}
	j := 0
	for _, w := range words {
		for j+1 < len(utts) && w.StartMs >= utts[j+1].StartMs {
			j++
		}
		utts[j].Words = append(utts[j].Words, w)
	}
	return utts
}

func (openAITranscribe) BuildQuery(Job, string) (Request, bool) { return Request{}, false }
func (openAITranscribe) ParseQuery(int, []byte) (Result, error) { return Result{}, nil }
