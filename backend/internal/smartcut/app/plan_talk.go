package app

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"

	"github.com/li904338203/tabtv/backend/internal/smartcut/domain"
	timelinedomain "github.com/li904338203/tabtv/backend/internal/timeline/domain"
)

// 口播精剪（docs/agent/smartcut SCT-34，SC-F-98~100）：识别稿按时间线用到的那几段裁剪，过长时分段规划。

// TranscriptCandidate 一份成功的识别结果：识别参数（`media_analysis.params_key`）与原样结果。
type TranscriptCandidate struct {
	ParamsKey string
	Result    json.RawMessage
}

// PickTranscript 按用途从一份素材的成功识别结果（最新的在前）里挑第一份能用的；都不能用时返回最新那份不能用的原因。
//
//   - need.Fine：要有字级或词级时间。segment / text 只能按整句剪，而口播的剪点要落在字与字之间（SC-F-122）。
//   - need.KeepFillers：识别时保留了口癖。顺滑过的结果里口癖已经没了，剪不掉也看不出剪没剪（SCK-5）。
//   - 一句都没识别出来的（静音素材）不能用：交给模型的稿子是空的。
//
// 结果里没有语言时补上识别时指定的语言（`f1.zh-CN` → zh-CN；auto 不补）。
//
// 规则放在这里而不是适配器：「什么样的识别结果能用于口播」是智能剪辑的口径，适配器只负责把行取出来。
func PickTranscript(cands []TranscriptCandidate, need TranscriptNeed) (domain.Transcript, string, bool) {
	why := ""
	for _, c := range cands {
		var tr domain.Transcript
		if json.Unmarshal(c.Result, &tr) != nil {
			continue
		}
		reason := ""
		switch {
		case need.Fine && tr.Granularity != "char" && tr.Granularity != "word":
			reason = "识别结果只有整句或整段文字（没有字级时间），换一个字级或词级的识别模型重新识别"
		case need.KeepFillers && !domain.ASRKeepsFillers(c.ParamsKey):
			reason = "识别时去掉了口癖，要选「保留口癖」重新识别"
		case len(tr.Utterances) == 0:
			reason = "识别结果里没有语音"
		}
		if reason == "" {
			// 上游没回语言时（火山极速版自动判断、不回语言）用识别时指定的语言：字幕轨的语言取它（SC-F-100）。
			if tr.Language == "" {
				tr.Language = domain.ASRParamsLang(c.ParamsKey)
			}
			return tr, "", true
		}
		if why == "" {
			why = reason
		}
	}
	return domain.Transcript{}, why, false
}

// talkSpansFrom 口播素材用哪几段（成片顺序）：按时间线画面轨上这几份素材的片段取区间。
//
//   - 用户剪过的（片段来源是 sync / user）：照片段的取用区间 —— 剪掉的开头，模型就看不到、成片里也不会再出现；
//     同一份素材相邻且接得上的片段并成一段（用户在中间切一刀不该把一句话切成两半）。
//   - 已经被 AI 剪过的素材（有 origin=ai 的片段）：整段重新剪。按 AI 上一轮剪出来的几十个小段再剪一遍，
//     只能越剪越少、上一轮误删的句子永远回不来；要撤回上一轮用「撤销这轮 AI」。
//   - 不在时间线上的口播素材：整段，排在后面（按素材条顺序）。
//
// 与缺省（每份整段、按素材条顺序）一样时返回 nil：规格里不多存一串。
func talkSpansFrom(base timelinedomain.Content, items []domain.Input) []domain.TalkSpan {
	dur := map[int64]int{}
	var order []int64
	for _, it := range items {
		if it.Status != domain.InputReady || it.Kind != domain.InputVideo {
			continue
		}
		id, err := strconv.ParseInt(it.MediaID, 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		if _, dup := dur[id]; dup {
			continue
		}
		d := math.MaxInt32
		if it.DurationMs != nil && *it.DurationMs > 0 {
			d = *it.DurationMs
		}
		dur[id] = d
		order = append(order, id)
	}
	if len(order) == 0 {
		return nil
	}
	var clips []timelinedomain.ContentClip
	for _, t := range base.Tracks {
		if t.Kind == timelinedomain.TrackVideo {
			clips = t.Clips
		}
	}
	aiCut := map[int64]bool{}
	for _, c := range clips {
		if c.Origin == "ai" {
			aiCut[c.MediaID] = true
		}
	}
	whole := func(id int64) domain.TalkSpan { return domain.TalkSpan{MediaID: id, InMs: 0, OutMs: dur[id]} }
	var spans []domain.TalkSpan
	placed := map[int64]bool{}
	for _, c := range clips {
		if _, ok := dur[c.MediaID]; !ok {
			continue
		}
		if aiCut[c.MediaID] {
			if !placed[c.MediaID] {
				spans = append(spans, whole(c.MediaID))
				placed[c.MediaID] = true
			}
			continue
		}
		placed[c.MediaID] = true
		sp := domain.TalkSpan{MediaID: c.MediaID, InMs: c.SourceInMs, OutMs: c.SourceOutMs}
		if n := len(spans); n > 0 && spans[n-1].MediaID == sp.MediaID && spans[n-1].InMs <= sp.InMs && spans[n-1].OutMs >= sp.InMs {
			spans[n-1].OutMs = max(spans[n-1].OutMs, sp.OutMs)
			continue
		}
		spans = append(spans, sp)
	}
	for _, id := range order {
		if !placed[id] {
			spans = append(spans, whole(id))
		}
	}
	if len(spans) == len(order) {
		same := true
		for i, sp := range spans {
			if sp != whole(order[i]) {
				same = false
				break
			}
		}
		if same {
			return nil
		}
	}
	return spans
}

// runTalkVariant 口播的一套方案：识别稿不长时就是一次调用；超过 domain.TalkChunkRunes 字时分段，每段独立调一次
// （各自回灌修一次），每段只对自己的句子作决定，结果按句号拼起来。任何一段修过仍不合规或调用失败，这一套就按
// 不合规 / 失败处理（整套走兜底或报错）—— 拼一半 AI、一半兜底的结果，用户看不出哪部分是 AI 的决定。
//
// 单独一段一句不留是合法的（那一段整个是重录，Refs.Partial）；拼起来一句都没有才算不合规。
func (s *Service) runTalkVariant(ctx context.Context, model, system string, req PlanRequest, f planFacts,
	mosaics []Mosaic, images []string, k, n int) variantRun {
	chunks := domain.TalkChunks(f.sentences, domain.TalkChunkRunes)
	if len(chunks) <= 1 {
		return s.runVariant(ctx, model, system, planUserMessage(req, f, mosaics, k, n), images, req.Mode, planRefs(f))
	}
	merged := domain.Draft{Mode: domain.ModeTalk}
	var out variantRun
	for i, chunk := range chunks {
		sub := f
		sub.sentences = chunk
		user := planUserMessage(req, sub, mosaics, k, n) +
			fmt.Sprintf("\n\n## 分段\n\n识别稿较长，分 %d 段交给你，这是第 %d 段（%s~%s）：只对这一段里的句子作决定；"+
				"这一段全都不该留时 keep 写空数组、在 drop 里说明原因。",
				len(chunks), i+1, chunk[0].Ref, chunk[len(chunk)-1].Ref)
		refs := planRefs(sub)
		refs.Partial = true
		run := s.runVariant(ctx, model, system, user, images, req.Mode, refs)
		if run.callErr != nil || run.invalid != nil {
			return run
		}
		d := run.parsed.Draft
		merged.Keep = append(merged.Keep, d.Keep...)
		merged.Drop = append(merged.Drop, d.Drop...)
		if merged.Subtitle == nil {
			merged.Subtitle = d.Subtitle
		}
		if merged.Music == "" {
			merged.Music = d.Music
		}
		out.parsed.Dropped = append(out.parsed.Dropped, run.parsed.Dropped...)
		out.parsed.Ignored = append(out.parsed.Ignored, run.parsed.Ignored...)
		out.repaired = out.repaired || run.repaired
	}
	if len(merged.Keep) == 0 {
		return variantRun{invalid: &domain.ErrDraftInvalid{Reason: "模型没有给出要保留的句子（各段都是空的）"}, repaired: out.repaired}
	}
	out.parsed.Draft, out.ok = merged, true
	return out
}
