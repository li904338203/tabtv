package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	timelinedomain "github.com/li904338203/tabtv/backend/internal/timeline/domain"
)

// 语音识别的选材与缓存键（docs/agent/smartcut SCT-33，SC-F-120~124）。

// TranscribeLanguages 是识别时可指定的语言（契约里那四种）；空串 = 交给模型判断。
var TranscribeLanguages = map[string]bool{"": true, "zh-CN": true, "en": true, "ja": true, "ko": true}

// TranscribeSources 从时间线里取要识别的素材（去重、按出现顺序：画面轨在前，其后是各条配音轨）。
//
//   - 不指定片段：画面轨的全部片段 + 配音轨（role=voice）的片段。配乐与音效轨不识别 —— 歌词与音效做成字幕没有意义，
//     还要白花钱。
//   - 指定片段：只认这些片段（任何轨上的都行，用户明确选了配乐也照识别）；不存在的片段 ID 原样返回给调用方报错。
//
// 图片在这里分不出来（片段只有媒体 ID），由调用方按媒体类型再滤一道。
func TranscribeSources(c timelinedomain.Content, clipIDs []string) (mediaIDs []int64, missing []string) {
	clips, missing := transcribeClips(c, clipIDs)
	seen := map[int64]bool{}
	for _, cl := range clips {
		if cl.MediaID > 0 && !seen[cl.MediaID] {
			seen[cl.MediaID] = true
			mediaIDs = append(mediaIDs, cl.MediaID)
		}
	}
	return mediaIDs, missing
}

// transcribeClips 选片段（规则见 TranscribeSources，识别字幕也用它）：画面轨在前、其后是各条音频轨，轨内按位置；
// 另返回没找到的片段 ID。
func transcribeClips(c timelinedomain.Content, clipIDs []string) (clips []timelinedomain.ContentClip, missing []string) {
	want := map[string]bool{}
	for _, id := range clipIDs {
		want[id] = true
	}
	found := map[string]bool{}
	tracks := make([]timelinedomain.ContentTrack, 0, len(c.Tracks))
	for _, t := range c.Tracks {
		if t.Kind == timelinedomain.TrackVideo {
			tracks = append([]timelinedomain.ContentTrack{t}, tracks...)
		} else if t.Kind == timelinedomain.TrackAudio {
			tracks = append(tracks, t)
		}
	}
	for _, t := range tracks {
		for _, cl := range t.Clips {
			switch {
			case len(want) > 0:
				if want[cl.ID] {
					found[cl.ID] = true
					clips = append(clips, cl)
				}
			case t.Kind == timelinedomain.TrackVideo || t.Role == timelinedomain.RoleVoice:
				clips = append(clips, cl)
			}
		}
	}
	for _, id := range clipIDs {
		if !found[id] {
			missing = append(missing, id)
		}
	}
	return clips, missing
}

// ASRParamsKey 是识别参数的缓存键（`media_analysis.params_key`）：是否保留口癖、语言。
// 同一份素材换了参数是另一份结果（保留口癖的给口播精剪、顺滑过的给字幕），各自缓存、各自收费。
func ASRParamsKey(keepFillers bool, language string) string {
	f := "f0"
	if keepFillers {
		f = "f1"
	}
	lang := strings.TrimSpace(language)
	if lang == "" {
		lang = "auto"
	}
	return f + "." + lang
}

// ASRKeepsFillers 这组识别参数是否保留了口癖（ASRParamsKey 的 f1 段）。口播精剪只认保留了口癖的结果（SCK-5）。
func ASRKeepsFillers(paramsKey string) bool { return strings.HasPrefix(paramsKey, "f1.") }

// ASRParamsLang 这组识别参数指定的语言（ASRParamsKey 的语言段）；自动判断（auto）或认不出时为空。
func ASRParamsLang(paramsKey string) string {
	_, lang, ok := strings.Cut(paramsKey, ".")
	if !ok || lang == "auto" {
		return ""
	}
	return lang
}

// ASRIdempotencyKey 是一次识别受理的幂等键：`asr:{素材}:{模型与参数的摘要}:{第几次}`。
//
// ⚠️ 第几次按这份素材这组参数**失败过几次**算，失败之后换新键（SC-AC-50）：只按「有没有结果」算的话，
// 失败后重试会算出同一把键、命中那次失败的任务，用户再也识别不了（CLAUDE.md §5.7 记过这个坑）。
// ⚠️ 模型码与参数先哈希：模型码最长 64 位，原样拼进去会超过幂等键的 128 字符上限。
func ASRIdempotencyKey(mediaID int64, engine, paramsKey string, attempt int) string {
	sum := sha256.Sum256([]byte(engine + "|" + paramsKey))
	return "asr:" + strconv.FormatInt(mediaID, 10) + ":" + hex.EncodeToString(sum[:6]) + ":" + strconv.Itoa(attempt)
}

// SpeechBillMs 识别的计费时长（毫秒）：素材时长与抽出来的识别用音轨时长取小的那个（有一个未知就用另一个）。
//
// ⚠️ 报价与扣费必须用同一个数：报价时音轨多半还没抽好，只有素材时长；受理时有了音轨的实测时长 —— 而重新编码会在
// 尾部补几十毫秒的帧对齐，60.000 秒的素材抽出来是 60.03 秒，向上取整就多收 1 秒（SCT-42 实测：报 60 扣 61）。
// 取小的那个：补出来的那一截不是用户的内容；音轨比画面短（后半段没声音）时按音轨收，上游也只按音频计费。
func SpeechBillMs(mediaMs, speechMs int) int {
	switch {
	case mediaMs <= 0:
		return speechMs
	case speechMs <= 0:
		return mediaMs
	}
	return min(mediaMs, speechMs)
}

// BillableSeconds 是识别按秒计价的时长：毫秒向上取整到秒，至少 1 秒（时长未知的素材按 1 秒报，受理时再按音轨的实测时长算）。
func BillableSeconds(ms int) int {
	if ms <= 0 {
		return 1
	}
	return (ms + 999) / 1000
}
