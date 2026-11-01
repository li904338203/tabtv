package protocol

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/platform/pgtext"
)

// 语音识别（docs/agent/smartcut SCT-33，SCD-32）。
//
// 两家（火山、OpenAI）各按自己的响应结构取结果，翻成同一个识别结果：全文进文本产物（text_content），
// 句与字的时间进产物的 meta（→ generation_output.meta → 智能剪辑写进源素材的分析缓存）。时间一律整数毫秒。
// 形状与 smartcut/domain.Transcript 一致（dev/02 §2 的 `asr` 结果）。

// 时间戳粒度：char / word 有字（词）级时间，口播精剪要它；segment 只有句级时间；text 只有整段文字（只能做普通字幕）。
const (
	GranularityChar    = "char"
	GranularityWord    = "word"
	GranularitySegment = "segment"
	GranularityText    = "text"
)

// 识别参数在任务参数里的键（智能剪辑受理时写进 params，快照冻结）。
const (
	// ASRParamKeepFillers 保留口癖（「嗯」「那个」）：口播精剪要靠识别结果判断口癖，必须保留；做字幕时可以顺滑掉。
	ASRParamKeepFillers = "keep_fillers"
	// ASRParamLanguage 语言（zh-CN / en / ja / ko），不给由模型判断。
	ASRParamLanguage = "language"
)

// ASRWord 一个字（中文）或一个词（英文）。
type ASRWord struct {
	Text    string `json:"text"`
	StartMs int    `json:"startMs"`
	EndMs   int    `json:"endMs"`
}

// ASRUtterance 识别出的一句。
type ASRUtterance struct {
	Text    string    `json:"text"`
	StartMs int       `json:"startMs"`
	EndMs   int       `json:"endMs"`
	Words   []ASRWord `json:"words,omitempty"`
}

// ASRResult 是统一的识别结果。Text 进文本产物，其余进 meta。
type ASRResult struct {
	Text        string         `json:"-"`
	Language    string         `json:"language,omitempty"`
	Granularity string         `json:"granularity"`
	DurationMs  int            `json:"durationMs,omitempty"`
	Utterances  []ASRUtterance `json:"utterances"`
}

// output 把识别结果变成一个文本产物。
//
// ⚠️ 空结果（静音）也是**一个**产物、文字为空：一个产物都没有的话提交侧会判「上游返回成功但未解析出产物」，
// 而静音素材的识别本来就是成功的（照常结算，SC-AC-51）。
// ⚠️ 上游的文字一律过 pgtext.Safe：PG 拒收 0x00，带进来的话整条落库回滚，一路重试到死线。
func (r ASRResult) output() Output {
	r.Text = pgtext.Safe(r.Text)
	if r.Utterances == nil {
		r.Utterances = []ASRUtterance{}
	}
	for i := range r.Utterances {
		r.Utterances[i].Text = pgtext.Safe(r.Utterances[i].Text)
		for j := range r.Utterances[i].Words {
			r.Utterances[i].Words[j].Text = pgtext.Safe(r.Utterances[i].Words[j].Text)
		}
	}
	meta, _ := json.Marshal(r)
	return Output{Kind: "text", Value: r.Text, MIME: "text/plain", Meta: meta}
}

// ASRGranularity 是某个识别模型能给到的粒度（`/smartcut/config` 据此标「能不能用于口播精剪」）。
// 认不出的协议返回空串。
func ASRGranularity(protocolCode, upstreamModel string) string {
	switch protocolCode {
	case "volc_asr", "volc_asr_standard":
		return GranularityChar
	case "openai_transcribe":
		if openAIWordTimestamps(upstreamModel) {
			return GranularityWord
		}
		return GranularityText
	}
	return ""
}

// firstAudio 取第一路音频（识别的输入是抽好的 16k 单声道音轨）；没有音频时退一步取视频。
func firstAudio(media []Media) *Media {
	for i := range media {
		if media[i].Kind == "audio" {
			return &media[i]
		}
	}
	for i := range media {
		if media[i].Kind == "video" {
			return &media[i]
		}
	}
	return nil
}

// audioFormat 由 MIME 定上游要的格式名；认不出按 mp3（识别用音轨就是 MP3）。
func audioFormat(mime string) string {
	m := strings.ToLower(strings.TrimSpace(strings.SplitN(mime, ";", 2)[0]))
	switch m {
	case "audio/wav", "audio/x-wav", "audio/wave":
		return "wav"
	case "audio/ogg", "audio/opus":
		return "ogg"
	case "audio/mp4", "audio/m4a", "audio/x-m4a":
		return "m4a"
	}
	return "mp3"
}

// splitDataURI 把 `data:<mime>;base64,<数据>` 拆成 MIME 与 base64 段；不是 data URI 时 ok = false。
func splitDataURI(v string) (mime, b64 string, ok bool) {
	if !strings.HasPrefix(v, "data:") {
		return "", "", false
	}
	head, data, found := strings.Cut(v[len("data:"):], ",")
	if !found || !strings.HasSuffix(head, ";base64") {
		return "", "", false
	}
	return strings.TrimSuffix(head, ";base64"), data, true
}

// dataURISize 估算 data URI 解出来有多少字节（不真的解码）。
func dataURISize(b64 string) int {
	return base64.StdEncoding.DecodedLen(len(b64)) - strings.Count(b64[max(0, len(b64)-2):], "=")
}

// requestIDFor 由任务的幂等键算出一个固定的请求 ID（UUID 形状）。
//
// ⚠️ 必须是确定的：火山标准版**以我们给的请求 ID 作任务 ID**，查询时要带同一个；协议拿不到上一次提交的请求头，
// 只能每次按同一个规则算出来。同一个任务重发提交时也是同一个 ID —— 上游按它去重，不会建出第二个任务。
func requestIDFor(job Job) string {
	seed := job.IdempotencyKey
	if seed == "" {
		seed = job.ModelCode + "|" + job.Prompt
	}
	sum := sha1.Sum([]byte("seeaitv-asr:" + seed))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x50 // 版本 5（基于名字）
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// paramFlag 取布尔参数（JSON 布尔或 "true"/"false" 字符串），缺省回落。
// 与视频协议的 paramBool 分开：那个只认 JSON 布尔，还带着可灵的嵌套写法。
func paramFlag(params map[string]any, key string, fallback bool) bool {
	switch v := params[key].(type) {
	case bool:
		return v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "1":
			return true
		case "false", "0":
			return false
		}
	}
	return fallback
}

// secondsToMs 把秒（浮点）换成整数毫秒，四舍五入。
func secondsToMs(s float64) int {
	if s <= 0 || math.IsNaN(s) || math.IsInf(s, 0) {
		return 0
	}
	return int(math.Round(s * 1000))
}

// StatusError 让协议说清「这次失败按哪个 HTTP 状态码分类」。
//
// 上游把业务错误藏在 HTTP 200 里时（火山识别的状态在响应头），只看 HTTP 码的话「服务端忙」会被当成
// 「解析失败」判永久失败，而它本该稍后重试；参数错误则相反，重试多少次都一样。
// 提交侧按 Status 分类（generation/domain.ClassifyHTTP），错误文案原样带给用户。
type StatusError struct {
	Status int
	Msg    string
}

func (e *StatusError) Error() string { return e.Msg }
