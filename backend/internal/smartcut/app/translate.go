package app

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/smartcut/domain"
	"github.com/li904338203/tabtv/backend/internal/smartcut/repo"
	timelinedomain "github.com/li904338203/tabtv/backend/internal/timeline/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 字幕翻译（docs/agent/smartcut SCT-36，SC-F-125 / 126，POST .../translate）。
//
// 翻译算 AI 剪辑的一部分：免费，与 AI 剪辑共用调用记录与限流（kind=translate，SCK-2）—— 不计入限流的话，
// 一条上千条的字幕轨就能把文本模型打满（SCR-9）。按 domain.TranslateBatch 条一批调用、各自回灌修一次；
// 译文靠序号对回原条。部分条目没译出来时说清是哪几条、那几条不改（SC-F-126）；一条都没译出来时不改时间线。

// TranslateRequest 一次字幕翻译。
type TranslateRequest struct {
	WorkspaceID     int64
	ProjectID       int64
	TimelineID      int64
	Actor           Actor
	MemberLevelID   int64
	SubtitleTrackID int64
	TargetLang      string
	// Mode newTrack（默认）/ replace。
	Mode        string
	BaseVersion int
}

// TranslateFailure 没译出来的一条：序号（按起点排序，从 1 起）与原因。
type TranslateFailure struct {
	Index  int    `json:"index"`
	Reason string `json:"reason"`
}

// TranslateResult 翻译结果（契约 SmartcutTranslateResult）。没有写入时 Timeline 为空。
type TranslateResult struct {
	CallID     int64
	TrackID    int64
	Translated int
	Failed     []TranslateFailure
	Timeline   json.RawMessage
}

// translateWriteAttempts 写入时版本对不上（剪辑台里刚存过）的重放次数。
const translateWriteAttempts = 3

// langNames 译文语言的叫法（给模型看）。
var langNames = map[string]string{"zh-CN": "简体中文", "en": "英文", "ja": "日文", "ko": "韩文"}

// Translate 翻译一条字幕轨。
func (s *Service) Translate(ctx context.Context, req TranslateRequest) (TranslateResult, error) {
	if err := s.planReady(); err != nil {
		return TranslateResult{}, err
	}
	if req.Mode == "" {
		req.Mode = domain.TranslateNewTrack
	}
	switch {
	case !slices.Contains(TranslateLangs, req.TargetLang):
		return TranslateResult{}, httpx.Errf(errcode.InvalidArgument, "目标语言只能是 "+strings.Join(TranslateLangs, " / ")).
			WithDetails(map[string]any{"path": "targetLang"})
	case req.Mode != domain.TranslateNewTrack && req.Mode != domain.TranslateReplace:
		return TranslateResult{}, httpx.Errf(errcode.InvalidArgument, "mode 只能是 newTrack / replace").
			WithDetails(map[string]any{"path": "mode"})
	case req.BaseVersion < 1:
		return TranslateResult{}, httpx.Errf(errcode.InvalidArgument, "baseVersion 必须来自服务端，不能为 0").
			WithDetails(map[string]any{"path": "baseVersion"})
	}
	if err := s.projectWritable(ctx, req.WorkspaceID, req.ProjectID, "翻译字幕"); err != nil {
		return TranslateResult{}, err
	}
	nodeID, err := s.boundNode(ctx, req.WorkspaceID, req.ProjectID, req.TimelineID)
	if err != nil {
		return TranslateResult{}, err
	}
	editor := Editor{UserID: req.Actor.UserID, Kind: req.Actor.editorKind()}
	base, err := s.plan.Timelines.Content(ctx, req.WorkspaceID, req.TimelineID)
	if err != nil {
		return TranslateResult{}, err
	}
	if base.Version != req.BaseVersion {
		return TranslateResult{}, s.plan.Timelines.Conflict(ctx, req.TimelineID, editor)
	}
	trackID := strconv.FormatInt(req.SubtitleTrackID, 10)
	source, err := translateSource(base, trackID, req)
	if err != nil {
		return TranslateResult{}, err
	}

	choice, unavailable, err := s.plan.Catalog.SmartcutPlanChoice(ctx, req.MemberLevelID, 0)
	if err != nil {
		return TranslateResult{}, httpx.Wrap(errcode.Internal, err)
	}
	if unavailable != nil {
		return TranslateResult{}, unavailableErr(unavailable)
	}
	r := &planRun{callID: s.plan.NextID(), ws: req.WorkspaceID, pid: req.ProjectID, tid: req.TimelineID, nodeID: nodeID,
		kind: repo.KindTranslate, model: choice.ModelCode, editor: editor, actor: req.Actor, stage: repo.StageSync}
	spec, _ := json.Marshal(map[string]any{"subtitleTrackId": trackID, "targetLang": req.TargetLang, "mode": req.Mode})
	if err := s.insertCall(ctx, r, repo.Call{Spec: spec, BaseVersion: req.BaseVersion}); err != nil {
		return TranslateResult{}, err
	}
	s.advance(ctx, r, repo.StagePlan)
	system, err := s.systemPrompt(ctx, domain.ModeTranslate, choice.ModelCode)
	if err != nil {
		return TranslateResult{}, s.failCall(ctx, r, err)
	}
	r.started = s.now()
	texts, failed, callErr := s.translateBatches(ctx, choice.ModelCode, system, req.TargetLang, source)

	sctx, cancel := settleContext(ctx)
	defer cancel()
	out := TranslateResult{CallID: r.callID, TrackID: req.SubtitleTrackID, Translated: len(texts), Failed: failed}
	record := func() json.RawMessage {
		raw, _ := json.Marshal(map[string]any{"translated": out.Translated, "failed": out.Failed})
		return raw
	}
	if len(texts) == 0 {
		// 一条都没译出来：不改时间线（SC-F-126）。全是调用失败时按调用失败报（用户重试即可）。
		if callErr != nil {
			return TranslateResult{}, s.failCall(sctx, r, mapCallErr(callErr))
		}
		s.finishCall(sctx, r, repo.StatusFailed, record(), nil, "一条都没有译出来")
		return out, nil
	}

	s.advance(sctx, r, repo.StageApply)
	for attempt := 1; ; attempt++ {
		content, track := domain.ApplyTranslation(base, trackID, texts, req.Mode, req.TargetLang)
		saved, err := s.plan.Timelines.Save(sctx, SavePlanInput{WorkspaceID: r.ws, TimelineID: r.tid, Editor: editor,
			Content: content, Reason: "translate", SnapshotReason: "translate", CallID: r.callID})
		if err == nil {
			if track != trackID {
				out.TrackID, _ = strconv.ParseInt(saved.IDMap[track], 10, 64)
			}
			out.Timeline = saved.Timeline
			s.finishCall(sctx, r, repo.StatusSuccess, record(), nil, "")
			return out, nil
		}
		if !isVersionConflict(err) || attempt >= translateWriteAttempts {
			return TranslateResult{}, s.failCall(sctx, r, translateConflict(err, r.callID))
		}
		// 翻译期间时间线又存过一次：原字幕没变就在最新的内容上重放，变了就如实报冲突（译文对不上了）。
		latest, rerr := s.plan.Timelines.Content(sctx, r.ws, r.tid)
		if rerr != nil {
			return TranslateResult{}, s.failCall(sctx, r, rerr)
		}
		if !sameSubtitles(base, latest, trackID) {
			return TranslateResult{}, s.failCall(sctx, r, translateConflict(err, r.callID))
		}
		base = latest
	}
}

// translateConflict 版本冲突时的说明（别的错误原样返回）。
func translateConflict(err error, callID int64) error {
	if !isVersionConflict(err) {
		return err
	}
	return withCallIDSaying(err, callID, "翻译期间字幕有改动，译文没有写入（再翻译一次即可）：")
}

// translateSource 要翻译的那条字幕轨上的文字（按起点排序）；轨道不对、没有字幕、写不下时说清楚。
func translateSource(c timelinedomain.Content, trackID string, req TranslateRequest) ([]string, error) {
	var track *timelinedomain.ContentTrack
	subtitleTracks := 0
	for i := range c.Tracks {
		if c.Tracks[i].Kind == timelinedomain.TrackSubtitle {
			subtitleTracks++
		}
		if c.Tracks[i].ID == trackID {
			track = &c.Tracks[i]
		}
	}
	if track == nil || track.Kind != timelinedomain.TrackSubtitle {
		return nil, httpx.Errf(errcode.NotFound, "这条时间线上没有这条字幕轨").WithDetails(map[string]any{"path": "subtitleTrackId"})
	}
	if track.Lang == req.TargetLang {
		return nil, httpx.Errf(errcode.InvalidArgument, "这条字幕轨已经是"+langNames[req.TargetLang]+"了").
			WithDetails(map[string]any{"path": "targetLang"})
	}
	order := domain.SubtitleOrder(c, trackID)
	if len(order) == 0 {
		return nil, httpx.Errf(errcode.InvalidArgument, "这条字幕轨上还没有字幕").WithDetails(map[string]any{"path": "subtitleTrackId"})
	}
	if req.Mode == domain.TranslateNewTrack {
		if subtitleTracks >= timelinedomain.MaxSubtitleTracks {
			return nil, httpx.Errf(errcode.InvalidArgument, fmt.Sprintf("字幕轨已满（最多 %d 条）：先删掉一条，或者选「替换原文」",
				timelinedomain.MaxSubtitleTracks)).WithDetails(map[string]any{"path": "mode"})
		}
		if len(c.Subtitles)+len(order) > timelinedomain.MaxSubtitles {
			return nil, httpx.Errf(errcode.InvalidArgument, fmt.Sprintf("字幕总数会超过 %d 条：先删掉一些，或者选「替换原文」",
				timelinedomain.MaxSubtitles)).WithDetails(map[string]any{"path": "mode"})
		}
	}
	out := make([]string, len(order))
	for n, i := range order {
		out[n] = c.Subtitles[i].Text
	}
	return out, nil
}

// translateBatches 分批翻译。返回译文（按序号，从 1 起）、没译出来的条目，以及第一处调用失败（全是调用失败时
// 调用方按它报错）。
func (s *Service) translateBatches(ctx context.Context, model, system, lang string, source []string) (
	map[int]string, []TranslateFailure, error) {
	texts := map[int]string{}
	failed := []TranslateFailure{}
	var firstErr error
	allCallErr := true
	for start := 0; start < len(source); start += domain.TranslateBatch {
		batch := source[start:min(start+domain.TranslateBatch, len(source))]
		fail := func(reason string) {
			for k := range batch {
				failed = append(failed, TranslateFailure{Index: start + k + 1, Reason: reason})
			}
		}
		if ctx.Err() != nil {
			fail("请求已取消")
			continue
		}
		run := s.runVariant(ctx, model, system, translateUserMessage(lang, batch), nil, domain.ModeTranslate,
			domain.Refs{Items: len(batch)})
		switch {
		case run.callErr != nil:
			if firstErr == nil {
				firstErr = run.callErr
			}
			fail(callErrText(run.callErr))
			continue
		case run.invalid != nil:
			allCallErr = false
			fail("AI 的译文无法解析：" + run.invalid.Reason)
			continue
		}
		allCallErr = false
		got := map[int]string{}
		for _, it := range run.parsed.Draft.Items {
			got[it.N] = it.Text
		}
		for k := range batch {
			if t, ok := got[k+1]; ok {
				texts[start+k+1] = t
			} else {
				failed = append(failed, TranslateFailure{Index: start + k + 1, Reason: "AI 没有给出这一条的译文"})
			}
		}
	}
	if !allCallErr {
		firstErr = nil
	}
	return texts, failed, firstErr
}

// translateUserMessage 一批字幕：目标语言 + 按序号排的原文（序号与输出的 n 一一对应）。
func translateUserMessage(lang string, batch []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## 目标语言\n\n%s（%s）\n\n## 字幕（按序号）\n\n", langNames[lang], lang)
	for i, t := range batch {
		fmt.Fprintf(&b, "%d. %s\n", i+1, strings.ReplaceAll(strings.TrimSpace(t), "\n", " "))
	}
	return strings.TrimRight(b.String(), "\n")
}

// sameSubtitles 两份内容里这条字幕轨上的文字是否一样（按起点排序逐条比）：一样时译文仍然对得上。
func sameSubtitles(a, b timelinedomain.Content, trackID string) bool {
	oa, ob := domain.SubtitleOrder(a, trackID), domain.SubtitleOrder(b, trackID)
	if len(oa) != len(ob) {
		return false
	}
	for k := range oa {
		if a.Subtitles[oa[k]].Text != b.Subtitles[ob[k]].Text || a.Subtitles[oa[k]].ID != b.Subtitles[ob[k]].ID {
			return false
		}
	}
	return true
}
