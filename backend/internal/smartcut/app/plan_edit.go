package app

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/li904338203/tabtv/backend/internal/smartcut/domain"
	"github.com/li904338203/tabtv/backend/internal/smartcut/repo"
	timelinedomain "github.com/li904338203/tabtv/backend/internal/timeline/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 追加指令（docs/agent/smartcut SCT-29，SC-F-110 / 111）：在已剪好的时间线上接着提要求。
// AI 只输出对现有片段的原子操作；`origin=user` 的片段在应用层跳过并说明（不能只靠提示词约束模型）。
// 与 AI 剪辑共用限流、调用记录、写入路径（快照 ai_edit + CAS + 事件）。

// EditRequest 一次追加指令（POST .../edit）。
type EditRequest struct {
	WorkspaceID   int64
	ProjectID     int64
	TimelineID    int64
	Actor         Actor
	MemberLevelID int64
	Instruction   string
	BaseVersion   int
	Force         bool
}

// Edit 应用一次追加指令。模型给不出可执行的修改（修过一次仍不合规）或修改全落在用户改过的片段上时，
// 不写时间线，结果里说明原因（这不是错误：时间线原样保留，用户换个说法再试）。
func (s *Service) Edit(ctx context.Context, req EditRequest) (PlanResult, error) {
	if err := s.planReady(); err != nil {
		return PlanResult{}, err
	}
	req.Instruction = strings.TrimSpace(req.Instruction)
	switch {
	case req.Instruction == "":
		return PlanResult{}, httpx.Errf(errcode.InvalidArgument, "先说说想怎么改").WithDetails(map[string]any{"path": "instruction"})
	case utf8.RuneCountInString(req.Instruction) > MaxInstructionChars:
		return PlanResult{}, httpx.Errf(errcode.InvalidArgument, fmt.Sprintf("要求不能超过 %d 字", MaxInstructionChars)).
			WithDetails(map[string]any{"path": "instruction"})
	case req.BaseVersion < 1 && !req.Force:
		return PlanResult{}, httpx.Errf(errcode.InvalidArgument, "baseVersion 必须来自服务端，不能为 0").
			WithDetails(map[string]any{"path": "baseVersion"})
	}
	if err := s.projectWritable(ctx, req.WorkspaceID, req.ProjectID, "修改智能剪辑"); err != nil {
		return PlanResult{}, err
	}
	nodeID, err := s.boundNode(ctx, req.WorkspaceID, req.ProjectID, req.TimelineID)
	if err != nil {
		return PlanResult{}, err
	}
	editor := Editor{UserID: req.Actor.UserID, Kind: req.Actor.editorKind()}
	base, err := s.plan.Timelines.Content(ctx, req.WorkspaceID, req.TimelineID)
	if err != nil {
		return PlanResult{}, err
	}
	if !req.Force && base.Version != req.BaseVersion {
		return PlanResult{}, s.plan.Timelines.Conflict(ctx, req.TimelineID, editor)
	}
	if len(videoClips(base)) == 0 {
		return PlanResult{}, unsuitableErr(&domain.ErrUnsuitable{Reason: domain.UnsuitableNoPicture,
			Detail: "时间线上还没有片段：先剪一版，再追加要求"})
	}
	choice, unavailable, err := s.plan.Catalog.SmartcutPlanChoice(ctx, req.MemberLevelID, 0)
	if err != nil {
		return PlanResult{}, httpx.Wrap(errcode.Internal, err)
	}
	if unavailable != nil {
		return PlanResult{}, unavailableErr(unavailable)
	}
	r := &planRun{callID: s.plan.NextID(), ws: req.WorkspaceID, pid: req.ProjectID, tid: req.TimelineID, nodeID: nodeID,
		kind: repo.KindEdit, model: choice.ModelCode, editor: editor, actor: req.Actor, stage: repo.StageSync}
	if err := s.insertCall(ctx, r, repo.Call{Instruction: req.Instruction, BaseVersion: req.BaseVersion}); err != nil {
		return PlanResult{}, err
	}
	s.advance(ctx, r, repo.StageUnderstand)
	inputs, err := s.Inputs(ctx, req.WorkspaceID, req.ProjectID, req.TimelineID)
	if err != nil {
		return PlanResult{}, s.failCall(ctx, r, err)
	}
	facts, err := s.gatherFacts(ctx, req.WorkspaceID, req.ProjectID, domain.ModeEdit, inputs.Items, domain.Spec{}, choice.ModelCode)
	if err != nil {
		return PlanResult{}, s.failCall(ctx, r, err)
	}
	facts.in.Base = base
	clipRefs, textRefs := domain.EditRefs(base, facts.in.Cards)
	clipNames, textNames := domain.EditRefNames(clipRefs, textRefs)
	refs := domain.Refs{Cards: facts.in.Cards, Clips: clipNames, Texts: textNames}

	s.advance(ctx, r, repo.StagePlan)
	system, err := s.systemPrompt(ctx, domain.ModeEdit, choice.ModelCode)
	if err != nil {
		return PlanResult{}, s.failCall(ctx, r, err)
	}
	r.started = s.now()
	run := s.runVariant(ctx, choice.ModelCode, system, editUserMessage(req.Instruction, clipRefs, textRefs, facts),
		nil, domain.ModeEdit, refs)

	sctx, cancel := settleContext(ctx)
	defer cancel()
	s.advance(sctx, r, repo.StageCompile)
	v := compiledVariant{storedVariant: storedVariant{VariantResult: VariantResult{Index: 0, Status: repo.StatusFailed,
		UsedClips: []string{}, Reasons: []domain.ClipReason{}, Warnings: []string{}, Notes: []string{}}}}
	switch {
	case run.callErr != nil:
		return PlanResult{}, s.failCall(sctx, r, mapCallErr(run.callErr))
	case run.invalid != nil:
		// 没有兜底：追加指令改的是用户已经剪好的东西，猜一个改法不如原样不动、说清楚。
		v.Error = "AI 没能给出可执行的修改：" + run.invalid.Reason
		r.errDetail = v.Error + "（原文开头：" + run.invalid.Snippet + "）"
		stored := compiledResult{Variants: []compiledVariant{v}}
		s.finishCall(sctx, r, repo.StatusFailed, stored.result(), nil, r.errDetail)
		return PlanResult{CallID: r.callID, Variants: stored.summaries(), Changes: []string{}}, nil
	}
	d := run.parsed.Draft
	res, err := domain.ApplyEdit(base, d, facts.in.Cards)
	if err != nil {
		var ue *domain.ErrUnsuitable
		if asUnsuitable(err, &ue) {
			return PlanResult{}, s.failCall(sctx, r, unsuitableErr(ue))
		}
		return PlanResult{}, s.failCall(sctx, r, httpx.Wrap(errcode.Internal, err))
	}
	v = fromCompiled(0, res, facts, run.parsed)
	v.Draft = &d
	stored := compiledResult{Variants: []compiledVariant{v}, Changes: nonNil(res.Changes)}
	if len(res.Changes) == 0 {
		// 全部落在用户改过的片段上（或模型给的修改都没落地）：不写、不存快照，说清为什么。
		s.finishCall(sctx, r, repo.StatusSuccess, stored.result(), nil, "")
		return PlanResult{CallID: r.callID, Variants: stored.summaries(), Changes: []string{}}, nil
	}

	s.advance(sctx, r, repo.StageApply)
	saved, err := s.plan.Timelines.Save(sctx, SavePlanInput{WorkspaceID: r.ws, TimelineID: r.tid, Editor: editor,
		Content: res.Content, Reason: "edit", SnapshotReason: "ai_edit", CallID: r.callID})
	if err != nil && req.Force && isVersionConflict(err) {
		saved, err = s.forceSave(sctx, r, facts, v.storedVariant, "edit", "ai_edit", "")
	}
	if err != nil {
		if isVersionConflict(err) {
			s.finishCall(sctx, r, repo.StatusSuccess, stored.result(), nil, "")
			return PlanResult{}, withCallID(err, r.callID)
		}
		return PlanResult{}, s.failCall(sctx, r, err)
	}
	applied := 0
	s.finishCall(sctx, r, repo.StatusSuccess, stored.result(), &applied, "")
	return PlanResult{CallID: r.callID, AppliedVariant: &applied, Variants: stored.summaries(), Timeline: saved.Timeline,
		SnapshotID: saved.SnapshotID, Changes: stored.Changes}, nil
}

func asUnsuitable(err error, target **domain.ErrUnsuitable) bool {
	if e, ok := err.(*domain.ErrUnsuitable); ok {
		*target = e
		return true
	}
	return false
}

func videoClips(c timelinedomain.Content) []timelinedomain.ContentClip {
	for _, t := range c.Tracks {
		if t.Kind == timelinedomain.TrackVideo {
			return t.Clips
		}
	}
	return nil
}

// editUserMessage 追加指令的 user 消息：要求 + 当前片段清单（c1~cN，谁改的）+ 文字层（t1~tN）+ 可用来替换 / 插入的素材。
func editUserMessage(instruction string, clips []domain.EditRef, texts []domain.TextRef, f planFacts) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	w("## 追加要求\n\n%s\n\n## 当前时间线（按播放顺序）\n\n", instruction)
	for _, c := range clips {
		w("- %s：", c.Ref)
		if c.CardNo != "" {
			w("素材编号 %s（%s）", c.CardNo, kindName(c.Kind))
		} else {
			w("%s（不在素材条里）", kindName(c.Kind))
		}
		w("，取 %s~%s 秒，成片里 %s 秒", secs(c.SourceInMs), secs(c.SourceOutMs), secs(c.PlayMs))
		if c.Speed != 100 {
			w("，速度 %s 倍", strings.TrimSuffix(strings.TrimSuffix(fmt.Sprintf("%d.%02d", c.Speed/100, c.Speed%100), "0"), ".0"))
		}
		if c.Transition != "" {
			w("，转场 %s", c.Transition)
		}
		if c.Origin == "user" {
			w("。你改过（不要改）")
		}
		w("\n")
	}
	if len(texts) > 0 {
		w("\n## 文字\n\n")
		for _, t := range texts {
			w("- %s（%s~%s 秒）：%s\n", t.Ref, secs(t.StartMs), secs(t.EndMs), clip(t.Text))
		}
	}
	if lines := cardLines(f, func(c domain.Card) bool { return c.IsPicture() }, nil); lines != "" {
		w("\n## 可以用来替换或插入的素材\n\n%s", lines)
	}
	return strings.TrimRight(b.String(), "\n")
}

func kindName(kind string) string {
	switch kind {
	case domain.InputImage:
		return "图片"
	case domain.InputAudio:
		return "音频"
	}
	return "视频"
}
