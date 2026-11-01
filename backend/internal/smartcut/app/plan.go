package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/li904338203/tabtv/backend/internal/smartcut/domain"
	"github.com/li904338203/tabtv/backend/internal/smartcut/repo"
	timelinedomain "github.com/li904338203/tabtv/backend/internal/timeline/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// AI 剪辑主链路（docs/agent/smartcut SCT-27，SCD-24 / SCD-25，dev/01 §3.4）：
//
//	校验 → 选模型 → 限流并插入调用记录（running / sync）→ 同步素材（listOnly）→ 理解素材（卡片、拼图、节拍、识别）
//	→ 规划（按方案数并行调模型；不合规回灌修一次）→ 编译（纯函数，修了仍不合规走兜底）→ 写入（先快照，再按 baseVersion CAS）
//	→ 调用记录结束。每进一个阶段写调用记录并发 smartcut.updated。
//
// ⚠️ 模型返回之后的落库一律用 settleContext（脱离请求的取消）：用户关了页面，结果也要落定、调用记录也要结束，
// 否则它永远停在 running、并发额度一直占着（CLAUDE.md §5.7）。

// 口径（SC-PARAM-19 / 21 / 29，设计建议值）。
const (
	// OperationPlan 是 AI 剪辑的操作码（目录、请求日志都用它）。
	OperationPlan = "smartcut_plan"
	// MaxInstructionChars 剪辑要求的上限（按字符）。
	MaxInstructionChars = 2000
	// MaxPlanVariants 一次最多出几套方案。
	MaxPlanVariants = 3
	// MaxTargetSec 目标时长上限（与时间线总长上限同一个数）。
	MaxTargetSec = timelinedomain.MaxTimelineMs / 1000
	// planSettleTimeout 模型返回之后落库用的上限。
	planSettleTimeout = 30 * time.Second
	// repairRawRunes 回灌修复时带上一次输出的前多少字（再长只是重复让模型读一遍）。
	repairRawRunes = 3000
)

// modePrompts 各模式的创作要求模板（prompt_template 登记，Static；兜底在目录域）。
var modePrompts = map[string]string{
	domain.ModeMix:       "smartcut.mix.system",
	domain.ModeBoard:     "smartcut.board.system",
	domain.ModeTalk:      "smartcut.talk.system",
	domain.ModeNarr:      "smartcut.narr.system",
	domain.ModeEdit:      "smartcut.edit.system",
	domain.ModeTranslate: "smartcut.translate.system",
}

// planModes 是 /plan 接受的模式（追加指令与翻译各有自己的入口）。
var planModes = map[string]bool{domain.ModeMix: true, domain.ModeBoard: true, domain.ModeTalk: true, domain.ModeNarr: true}

// PlanRequest 一次 AI 剪辑（契约 SmartcutPlanRequest）。
type PlanRequest struct {
	WorkspaceID   int64
	ProjectID     int64
	TimelineID    int64
	Actor         Actor
	MemberLevelID int64
	Mode          string
	Instruction   string
	TargetSec     int
	AspectRatio   string
	Resolution    string
	Variants      int
	BaseVersion   int
	AllowSlow     bool
	AllowRepeat   bool
	// WithSubtitles 为 nil 时按模式（混剪看要求里有没有提到字幕；其余默认生成）。
	WithSubtitles *bool
	IgnoreBeat    bool
	KeepFillers   bool
	Force         bool
}

// VariantResult 一套方案的结果（契约 SmartcutVariant）。
type VariantResult struct {
	Index        int                 `json:"index"`
	Status       string              `json:"status"`
	Summary      string              `json:"summary"`
	TotalMs      int                 `json:"totalMs"`
	UsedClips    []string            `json:"usedClips"`
	Reasons      []domain.ClipReason `json:"reasons"`
	Warnings     []string            `json:"warnings"`
	UsedFallback bool                `json:"usedFallback"`
	Shortage     *domain.Shortage    `json:"shortage"`
	Notes        []string            `json:"notes"`
	Error        string              `json:"error,omitempty"`
}

// PlanResult 结果（契约 SmartcutPlanResult）。
type PlanResult struct {
	CallID         int64
	AppliedVariant *int
	Variants       []VariantResult
	Timeline       json.RawMessage
	SnapshotID     int64
	Changes        []string
}

// storedVariant 调用记录里存的一套方案：结果摘要 + 解析后的方案（方案切换时按当前素材重新编译）。
type storedVariant struct {
	VariantResult
	Draft    *domain.Draft `json:"draft,omitempty"`
	Fallback bool          `json:"fallback,omitempty"`
}

type storedResult struct {
	Variants []storedVariant `json:"variants"`
	Changes  []string        `json:"changes,omitempty"`
}

// planRun 一次进行中的调用。
type planRun struct {
	callID    int64
	ws, pid   int64
	tid       int64
	nodeID    string
	kind      string
	model     string
	started   time.Time
	finished  bool
	stage     string
	editor    Editor
	actor     Actor
	images    int
	fallback  bool
	errDetail string
}

// planReady 端口齐全。
func (s *Service) planReady() error {
	p := s.plan
	if p.Calls == nil || p.Catalog == nil || p.Text == nil || p.Prompts == nil || p.Timelines == nil ||
		p.Material == nil || p.NextID == nil {
		return httpx.Errf(errcode.Internal, "AI 剪辑链路未接入")
	}
	return nil
}

func validatePlan(req PlanRequest) error {
	if !planModes[req.Mode] {
		return httpx.Errf(errcode.InvalidArgument, "mode 只能是 mix / board / talk / narr").
			WithDetails(map[string]any{"path": "mode"})
	}
	if utf8.RuneCountInString(req.Instruction) > MaxInstructionChars {
		return httpx.Errf(errcode.InvalidArgument, "剪辑要求不能超过 "+strconv.Itoa(MaxInstructionChars)+" 字").
			WithDetails(map[string]any{"path": "instruction"})
	}
	if req.TargetSec < 0 || req.TargetSec > MaxTargetSec {
		return httpx.Errf(errcode.InvalidArgument, fmt.Sprintf("目标时长只能是 0（自动）~%d 秒", MaxTargetSec)).
			WithDetails(map[string]any{"path": "spec.targetSec"})
	}
	if req.Variants < 0 || req.Variants > MaxPlanVariants {
		return httpx.Errf(errcode.InvalidArgument, "一次最多出 "+strconv.Itoa(MaxPlanVariants)+" 套方案").
			WithDetails(map[string]any{"path": "variants"})
	}
	if req.BaseVersion < 1 && !req.Force {
		return httpx.Errf(errcode.InvalidArgument, "baseVersion 必须来自服务端，不能为 0").
			WithDetails(map[string]any{"path": "baseVersion"})
	}
	return nil
}

// specOf 请求 → 编译规格。字幕缺省：混剪看要求里有没有提到字幕，其余模式默认生成（SC-F-93 / 96 / 100 / 102）。
func specOf(req PlanRequest) domain.Spec {
	subs := req.Mode != domain.ModeMix || strings.Contains(req.Instruction, "字幕")
	if req.WithSubtitles != nil {
		subs = *req.WithSubtitles
	}
	return domain.Spec{TargetMs: req.TargetSec * 1000, AllowSlow: req.AllowSlow, AllowRepeat: req.AllowRepeat,
		WithSubtitles: subs, IgnoreBeat: req.IgnoreBeat, KeepFillers: req.KeepFillers}
}

// specRecord 调用记录里的 spec（方案切换时还原规格）。
type specRecord struct {
	domain.Spec
	AspectRatio string `json:"aspectRatio,omitempty"`
	Resolution  string `json:"resolution,omitempty"`
	Instruction string `json:"instruction,omitempty"`
}

// Plan 是一次 AI 剪辑（POST /projects/{pid}/smartcuts/{tid}/plan）。结果直接写进时间线（SC-F-82），写入前存快照。
func (s *Service) Plan(ctx context.Context, req PlanRequest) (PlanResult, error) {
	if err := s.planReady(); err != nil {
		return PlanResult{}, err
	}
	if req.Variants == 0 {
		req.Variants = 1
	}
	req.Instruction = strings.TrimSpace(req.Instruction)
	if err := validatePlan(req); err != nil {
		return PlanResult{}, err
	}
	if err := s.projectWritable(ctx, req.WorkspaceID, req.ProjectID, "AI 剪辑"); err != nil {
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
		// 剪辑台里还有没存下来的改动，或者别人刚改过：拿旧时间线剪出来的结果，写入时必然冲突。
		return PlanResult{}, s.plan.Timelines.Conflict(ctx, req.TimelineID, editor)
	}
	spec := specOf(req)

	// 模式与输入是否匹配：在限流之前判（不占额度），用只读的素材条。
	pre, err := s.Inputs(ctx, req.WorkspaceID, req.ProjectID, req.TimelineID)
	if err != nil {
		return PlanResult{}, err
	}
	if req.Mode == domain.ModeTalk {
		// 口播用素材的哪几段在发起这一刻定下来，跟着规格存进调用记录（切换方案时照这份重新编号）。
		spec.TalkSpans = talkSpansFrom(base, pre.Items)
	}
	if _, err := s.gatherFacts(ctx, req.WorkspaceID, req.ProjectID, req.Mode, pre.Items, spec, ""); err != nil {
		return PlanResult{}, s.withTranscribeQuote(ctx, req, pre.Items, err)
	}
	if err := beatRequirement(req, pre.Items); err != nil {
		return PlanResult{}, err
	}

	choice, unavailable, err := s.plan.Catalog.SmartcutPlanChoice(ctx, req.MemberLevelID, 0)
	if err != nil {
		return PlanResult{}, httpx.Wrap(errcode.Internal, err)
	}
	if unavailable != nil {
		return PlanResult{}, unavailableErr(unavailable)
	}
	specRaw, _ := json.Marshal(specRecord{Spec: spec, AspectRatio: req.AspectRatio, Resolution: req.Resolution})
	r := &planRun{callID: s.plan.NextID(), ws: req.WorkspaceID, pid: req.ProjectID, tid: req.TimelineID, nodeID: nodeID,
		kind: repo.KindPlan, model: choice.ModelCode, editor: editor, actor: req.Actor, stage: repo.StageSync}
	if err := s.insertCall(ctx, r, repo.Call{Mode: req.Mode, Instruction: req.Instruction, Spec: specRaw,
		VariantCount: req.Variants, BaseVersion: req.BaseVersion}); err != nil {
		return PlanResult{}, err
	}

	// 同步素材（listOnly）：剪辑台开着时不动轨道，只更新素材条并记下这次看到的指纹。
	synced, err := s.Sync(ctx, req.WorkspaceID, req.ProjectID, req.TimelineID, SyncListOnly)
	if err != nil {
		return PlanResult{}, s.failCall(ctx, r, err)
	}
	s.advance(ctx, r, repo.StageUnderstand)
	facts, err := s.gatherFacts(ctx, req.WorkspaceID, req.ProjectID, req.Mode, synced.Inputs, spec, choice.ModelCode)
	if err != nil {
		return PlanResult{}, s.failCall(ctx, r, err)
	}
	facts.in.Base = base
	mosaics, err := s.mosaicsFor(ctx, req.WorkspaceID, facts, choice.MaxImages)
	if err != nil {
		return PlanResult{}, s.failCall(ctx, r, err)
	}
	images := make([]string, len(mosaics))
	for i, m := range mosaics {
		images[i] = m.DataURI
	}
	r.images = len(images)
	_ = s.plan.Calls.SetModel(ctx, r.callID, choice.ModelCode, synced.Fingerprint)

	s.advance(ctx, r, repo.StagePlan)
	system, err := s.systemPrompt(ctx, req.Mode, choice.ModelCode)
	if err != nil {
		return PlanResult{}, s.failCall(ctx, r, err)
	}
	refs := planRefs(facts)
	r.started = s.now()
	runs := make([]variantRun, req.Variants)
	var wg sync.WaitGroup
	for k := 0; k < req.Variants; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			if req.Mode == domain.ModeTalk {
				// 口播稿过长时分段规划（SCT-34）。
				runs[k] = s.runTalkVariant(ctx, choice.ModelCode, system, req, facts, mosaics, images, k, req.Variants)
				return
			}
			user := planUserMessage(req, facts, mosaics, k, req.Variants)
			runs[k] = s.runVariant(ctx, choice.ModelCode, system, user, images, req.Mode, refs)
		}(k)
	}
	wg.Wait()

	sctx, cancel := settleContext(ctx)
	defer cancel()
	s.advance(sctx, r, repo.StageCompile)
	stored, applied, err := s.compileRuns(r, facts, runs)
	if err != nil {
		return PlanResult{}, s.failCall(sctx, r, err)
	}
	s.saveCaptions(sctx, r, facts, runs)

	s.advance(sctx, r, repo.StageApply)
	variant := stored.Variants[applied]
	content := variant.content
	saved, err := s.plan.Timelines.Save(sctx, SavePlanInput{WorkspaceID: r.ws, TimelineID: r.tid, Editor: editor,
		Content: content, Reason: "plan", SnapshotReason: "ai_plan", CallID: r.callID, Fingerprint: synced.Fingerprint})
	if err != nil && req.Force && isVersionConflict(err) {
		// 「仍然应用」：以当前版本覆盖（仍先存快照）。时间线在等模型的这几分钟里又变了，按最新内容重新编译这一套。
		saved, err = s.forceSave(sctx, r, facts, variant.storedVariant, "plan", "ai_plan", synced.Fingerprint)
	}
	if err != nil {
		if isVersionConflict(err) {
			// 方案已经生成好了：记成功（未应用），把 callId 带给前端，「仍然应用」不必重新调模型（SC-F-86）。
			// 也带上本来要写入的是第几套：Agent / MCP 据此重读后按同一套重新编译写入一次（PlanAndApply）。
			s.finishCall(sctx, r, repo.StatusSuccess, stored.result(), nil, "")
			return PlanResult{}, withVariant(withCallID(err, r.callID), applied)
		}
		return PlanResult{}, s.failCall(sctx, r, err)
	}
	status := repo.StatusSuccess
	if r.fallback {
		// 兜底：时间线照样写入（不留空时间线），但调用记录是失败，并带原文片段（SC-AC-32）。
		status = repo.StatusFailed
	}
	s.finishCall(sctx, r, status, stored.result(), &applied, r.errDetail)
	return PlanResult{CallID: r.callID, AppliedVariant: &applied, Variants: stored.summaries(), Timeline: saved.Timeline,
		SnapshotID: saved.SnapshotID}, nil
}

// beatRequirement 要求里写了「卡点」却没有配乐：说明原因并给出路（连一个音频节点 / 不卡点），不悄悄放弃卡点（SC-F-91）。
func beatRequirement(req PlanRequest, items []domain.Input) error {
	if req.Mode != domain.ModeMix || req.IgnoreBeat || !strings.Contains(req.Instruction, "卡点") {
		return nil
	}
	for _, it := range items {
		if it.Status == domain.InputReady && it.Kind == domain.InputAudio && it.AudioRole == domain.RoleMusic {
			return nil
		}
	}
	return unsuitableErr(&domain.ErrUnsuitable{Reason: domain.UnsuitableNoMusic,
		Detail: "要求卡点，但素材里没有配乐：先在画布上连一个音频节点，或者选择不卡点"})
}

// ---------------------------------------------------------------- 素材事实

// planFacts 是编译与给模型看的全部事实（按模式）。
type planFacts struct {
	in        domain.PlanInput
	gaps      []BoardGap
	sentences []domain.Sentence
	// skipped 没收进卡片的就绪素材（时长未知等），进警告。
	skipped []string
	// described 有描述（生成提示词或缓存的看图描述）的素材编号：这些不进拼图。
	described map[string]bool
}

// gatherFacts 按模式准备编译要的事实。modelCode 为空时只取生成提示词作描述（预检用）。
func (s *Service) gatherFacts(ctx context.Context, ws, pid int64, mode string, items []domain.Input, spec domain.Spec,
	modelCode string) (planFacts, error) {
	f := planFacts{in: domain.PlanInput{Mode: mode, Spec: spec, Beats: map[string]domain.Beats{},
		Transcripts: map[string]domain.Transcript{}, Peaks: map[string]domain.Peaks{}}, described: map[string]bool{}}
	byMedia := map[int64][]string{}
	var mediaIDs []int64
	for _, it := range items {
		if it.Status != domain.InputReady || !domain.Placeable(it.Kind) {
			continue
		}
		if id, err := strconv.ParseInt(it.MediaID, 10, 64); err == nil && id > 0 {
			if _, seen := byMedia[id]; !seen {
				mediaIDs = append(mediaIDs, id)
			}
			byMedia[id] = append(byMedia[id], it.No)
		}
	}
	desc := map[string]string{}
	if len(mediaIDs) > 0 {
		got, err := s.plan.Material.Descriptions(ctx, ws, pid, mediaIDs, modelCode)
		if err != nil {
			return f, httpx.Wrap(errcode.Internal, err)
		}
		for id, text := range got {
			for _, no := range byMedia[id] {
				if t := strings.TrimSpace(text); t != "" {
					desc[no] = t
					f.described[no] = true
				}
			}
		}
	}
	f.in.Cards, f.skipped = domain.CardsFromInputs(items, desc)

	ids := func(pred func(domain.Card) bool) []int64 {
		var out []int64
		for _, c := range f.in.Cards {
			if pred(c) && c.MediaID > 0 {
				out = append(out, c.MediaID)
			}
		}
		return out
	}
	switch mode {
	case domain.ModeMix, domain.ModeBoard:
		music := ids(func(c domain.Card) bool { return c.Kind == domain.InputAudio })
		if len(music) > 0 {
			beats, err := s.plan.Material.Beats(ctx, ws, music)
			if err != nil {
				return f, httpx.Wrap(errcode.Internal, err)
			}
			for _, c := range f.in.Cards {
				if b, ok := beats[c.MediaID]; ok && c.Kind == domain.InputAudio {
					f.in.Beats[c.No] = b
				}
			}
		}
		if mode == domain.ModeMix {
			pictures := 0
			for _, c := range f.in.Cards {
				if c.IsPicture() {
					pictures++
				}
			}
			if pictures == 0 {
				return f, unsuitableErr(&domain.ErrUnsuitable{Reason: domain.UnsuitableNoPicture,
					Detail: "素材条里还没有就绪的视频或图片：先在画布上连几个有产出的节点"})
			}
			break
		}
		if err := s.boardFacts(ctx, ws, items, &f); err != nil {
			return f, err
		}
	case domain.ModeTalk:
		videos := ids(isVideo)
		why, err := s.speechFacts(ctx, ws, videos, &f, talkNeed, true)
		if err != nil {
			return f, err
		}
		// 每份口播素材都要有能用的识别结果（SC-F-122）：缺一份就剪不全，而模型看不到那一份说了什么。
		if detail := talkMissing(f, why); detail != "" {
			return f, unsuitableErr(&domain.ErrUnsuitable{Reason: domain.UnsuitableASRMissing, Detail: detail})
		}
		f.sentences = domain.TalkSentences(f.in)
		if len(f.sentences) == 0 {
			return f, unsuitableErr(&domain.ErrUnsuitable{Reason: domain.UnsuitableASRMissing,
				Detail: "口播精剪要先识别语音：素材条里的视频还没有识别结果"})
		}
	case domain.ModeNarr:
		voice := ids(isVoice)
		if len(voice) == 0 {
			return f, unsuitableErr(&domain.ErrUnsuitable{Reason: domain.UnsuitableNoVoice,
				Detail: "解说成片需要一条配音：先在画布上用音频节点生成配音，再连到智能剪辑"})
		}
		why, err := s.speechFacts(ctx, ws, voice, &f, narrNeed, false)
		if err != nil {
			return f, err
		}
		// 每条配音都要有能用的识别结果：缺一条，那一段的字幕与画面就排不出来。
		if detail := speechMissing(f, why, isVoice, "解说成片要先识别配音"); detail != "" {
			return f, unsuitableErr(&domain.ErrUnsuitable{Reason: domain.UnsuitableASRMissing, Detail: detail})
		}
		// 解说稿：连进来的文本素材（SC-F-18）优先；没有时从配音那次文字转语音的快照还原（不读画布上节点现在的文字）。
		var script []string
		for _, c := range f.in.Cards {
			if c.Kind == domain.InputText && strings.TrimSpace(c.Text) != "" {
				script = append(script, strings.TrimSpace(c.Text))
			}
		}
		f.in.Script = strings.Join(script, "\n")
		if f.in.Script == "" {
			spoken, err := s.plan.Material.VoiceScripts(ctx, ws, voice)
			if err != nil {
				return f, httpx.Wrap(errcode.Internal, err)
			}
			f.in.VoiceScripts = map[string]string{}
			for _, c := range f.in.Cards {
				if t, ok := spoken[c.MediaID]; ok && isVoice(c) {
					f.in.VoiceScripts[c.No] = t
				}
			}
		}
		f.sentences = domain.NarrSentences(f.in)
		if len(f.sentences) == 0 {
			// 识别过、却一句都排不出来（只有口癖）：再识别一次也一样，出路是换一条配音。
			return f, unsuitableErr(&domain.ErrUnsuitable{Reason: domain.UnsuitableNoVoice,
				Detail: "配音里没有识别出能用的句子（只有口癖或静音）：换一条配音再试"})
		}
	}
	return f, nil
}

// talkNeed 口播精剪对识别结果的要求：保留了口癖、有字级或词级时间（SCK-5、SC-F-122）。
var talkNeed = TranscriptNeed{KeepFillers: true, Fine: true}

// narrNeed 解说成片对识别结果的要求：有字级或词级时间（字幕按原稿逐字对齐到配音，只有整句时间的话字幕时间是
// 按字数猜的，与声音对不上而没有报错）。口癖不要求：解说是照稿念的，顺滑过的结果对齐得更干净。
var narrNeed = TranscriptNeed{Fine: true}

func isVideo(c domain.Card) bool { return c.Kind == domain.InputVideo }
func isVoice(c domain.Card) bool {
	return c.Kind == domain.InputAudio && c.AudioRole == domain.RoleVoice
}

// talkMissing 列出缺能用的识别结果的口播素材（「编号 N：原因」）；都有时返回空串。
func talkMissing(f planFacts, why map[int64]string) string {
	return speechMissing(f, why, isVideo, "口播精剪要先识别语音")
}

// speechMissing 列出缺能用的识别结果的素材（「编号 N：原因」，原因来自识别过却不能用的那一份，没有就是还没识别）；
// 都有时返回空串。
func speechMissing(f planFacts, why map[int64]string, want func(domain.Card) bool, lead string) string {
	var parts []string
	for _, c := range f.in.Cards {
		if !want(c) || c.MediaID <= 0 {
			continue
		}
		if _, ok := f.in.Transcripts[c.No]; ok {
			continue
		}
		reason := why[c.MediaID]
		if reason == "" {
			reason = "还没有识别"
		}
		parts = append(parts, "编号 "+c.No+"："+reason)
	}
	if len(parts) == 0 {
		return ""
	}
	return lead + "（" + strings.Join(parts, "；") + "）"
}

// speechFacts 识别结果（必须有）与波形（口播的能量校正，可没有）。返回识别过却不能用的原因（按媒体 ID）。
func (s *Service) speechFacts(ctx context.Context, ws int64, mediaIDs []int64, f *planFacts, need TranscriptNeed,
	peaks bool) (map[int64]string, error) {
	if len(mediaIDs) == 0 {
		return nil, nil
	}
	trs, why, err := s.plan.Material.Transcripts(ctx, ws, mediaIDs, need)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	for _, c := range f.in.Cards {
		if tr, ok := trs[c.MediaID]; ok && c.MediaID > 0 {
			f.in.Transcripts[c.No] = tr
		}
	}
	if !peaks {
		return why, nil
	}
	ps, err := s.plan.Material.Peaks(ctx, ws, mediaIDs)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	for _, c := range f.in.Cards {
		if p, ok := ps[c.MediaID]; ok && c.MediaID > 0 {
			f.in.Peaks[c.No] = p
		}
	}
	return why, nil
}

// boardFacts 分镜成片：素材条里的镜头组按连线顺序装配（SCT-28）。
func (s *Service) boardFacts(ctx context.Context, ws int64, items []domain.Input, f *planFacts) error {
	var groups []ShotGroup
	for _, it := range items {
		if it.Kind != domain.InputShotGroup || it.Status != domain.InputReady {
			continue
		}
		script, _ := strconv.ParseInt(it.ScriptID, 10, 64)
		group, _ := strconv.ParseInt(it.GroupID, 10, 64)
		if script <= 0 {
			continue
		}
		title := it.Title
		if title == "" {
			title = "编号 " + it.No
		}
		groups = append(groups, ShotGroup{No: it.No, SourceNodeID: it.SourceNodeID, Title: title, ScriptID: script,
			GroupID: group})
	}
	if len(groups) == 0 || s.plan.Board == nil {
		return unsuitableErr(&domain.ErrUnsuitable{Reason: domain.UnsuitableNoShots,
			Detail: "分镜成片要连一个已经出了视频的脚本或视频组节点"})
	}
	shots, gaps, err := s.plan.Board.Assemble(ctx, ws, groups)
	if err != nil {
		return err
	}
	f.in.Shots, f.gaps = domain.NumberShots(shots), gaps
	if len(f.in.Shots) == 0 {
		return unsuitableErr(&domain.ErrUnsuitable{Reason: domain.UnsuitableNoShots,
			Detail: "连着的脚本 / 视频组还没有一镜出了视频：" + gapText(gaps)})
	}
	return nil
}

func gapText(gaps []BoardGap) string {
	var parts []string
	for i, g := range gaps {
		if i == 5 {
			parts = append(parts, fmt.Sprintf("等 %d 处", len(gaps)))
			break
		}
		parts = append(parts, g.text())
	}
	return strings.Join(parts, "；")
}

func (g BoardGap) text() string {
	s := g.Reason
	if g.ShotNo > 0 {
		s = fmt.Sprintf("第 %d 镜：%s", g.ShotNo, g.Reason)
	}
	if g.Source != "" {
		s += "（" + g.Source + "）"
	}
	return s
}

// mosaicsFor 看图：没有描述的视频 / 图片（多半是上传的）做成缩略图拼图交给模型看（SC-F-80、SC-AC-37 只内联不外链）。
func (s *Service) mosaicsFor(ctx context.Context, ws int64, f planFacts, maxSheets int) ([]Mosaic, error) {
	if maxSheets <= 0 || (f.in.Mode != domain.ModeMix && f.in.Mode != domain.ModeNarr) {
		return nil, nil
	}
	var items []MosaicItem
	for _, c := range f.in.Cards {
		if c.IsPicture() && !f.described[c.No] {
			items = append(items, MosaicItem{No: c.No, MediaID: c.MediaID, Kind: c.Kind})
		}
	}
	if len(items) == 0 {
		return nil, nil
	}
	got, err := s.plan.Material.Mosaics(ctx, ws, items, maxSheets)
	if err != nil {
		// 看图是加分项：拼图做不出来照样能按时长与标题剪，不为它让整次 AI 剪辑失败。
		s.plan.Log.WarnContext(ctx, "AI 剪辑的缩略图拼图没做出来，这次不看图", "error", err)
		return nil, nil
	}
	return got, nil
}

func planRefs(f planFacts) domain.Refs {
	return domain.Refs{Cards: f.in.Cards, Shots: domain.ShotRefs(f.in.Shots), Sentences: domain.SentenceRefs(f.sentences)}
}

// ---------------------------------------------------------------- 调模型

// variantRun 一套方案的规划结果。
type variantRun struct {
	parsed   domain.ParseResult
	ok       bool
	invalid  *domain.ErrDraftInvalid
	callErr  error
	repaired bool
}

// runVariant 调一次模型；不合规（解析失败或有被丢掉的项）时把原因回灌、再修一次（SC-AC-31）。
// 修过之后仍救不回来就记 invalid（由编译阶段走兜底）；修的那一次调用失败时，第一次部分合规的方案照用。
func (s *Service) runVariant(ctx context.Context, model, system, user string, images []string, mode string,
	refs domain.Refs) variantRun {
	raw, err := s.complete(ctx, model, system, user, images)
	if err != nil {
		return variantRun{callErr: err}
	}
	pr, perr := domain.ParseDraft(mode, raw, refs)
	if perr == nil && len(pr.Dropped) == 0 {
		return variantRun{parsed: pr, ok: true}
	}
	repair := user + "\n\n## 上一次的输出\n\n" + headText(strings.TrimSpace(raw), repairRawRunes) + "\n\n" +
		domain.RepairHint(pr.Dropped, perr)
	raw2, err2 := s.complete(ctx, model, system, repair, images)
	if err2 == nil {
		pr2, perr2 := domain.ParseDraft(mode, raw2, refs)
		if perr2 == nil {
			return variantRun{parsed: pr2, ok: true, repaired: true}
		}
		if perr != nil {
			var di *domain.ErrDraftInvalid
			if errors.As(perr2, &di) {
				return variantRun{invalid: di, repaired: true}
			}
			return variantRun{callErr: perr2}
		}
	}
	if perr == nil {
		// 修的那一次没成：第一次的方案部分合规，丢掉的项进警告照用。
		return variantRun{parsed: pr, ok: true, repaired: true}
	}
	var di *domain.ErrDraftInvalid
	if errors.As(perr, &di) {
		return variantRun{invalid: di, repaired: true}
	}
	if err2 != nil {
		return variantRun{callErr: err2}
	}
	return variantRun{callErr: perr}
}

// complete 调一次模型：带拼图时上限用看图的那一档（SC-PARAM-29）。
func (s *Service) complete(ctx context.Context, model, system, user string, images []string) (string, error) {
	timeout := s.plan.Config.CallTimeout
	if len(images) > 0 {
		timeout = s.plan.Config.VisionTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	raw, err := s.plan.Text.Complete(callCtx, TextRequest{ModelCode: model, System: system, User: user, Images: images,
		Operation: OperationPlan})
	if err != nil {
		if errors.Is(callCtx.Err(), context.DeadlineExceeded) {
			return "", &callTimeout{after: timeout}
		}
		return "", err
	}
	return raw, nil
}

type callTimeout struct{ after time.Duration }

func (e *callTimeout) Error() string { return "AI 剪辑超时（" + e.after.String() + "）" }

// systemPrompt 创作要求（模板渲染）+ 代码固定追加的输出格式契约（SCD-25：改格式等于改解析器，不交给运营后台）。
func (s *Service) systemPrompt(ctx context.Context, mode, model string) (string, error) {
	text, err := s.plan.Prompts.RenderPrompt(ctx, modePrompts[mode], model, nil)
	if err != nil {
		return "", httpx.Wrap(errcode.Internal, err)
	}
	return strings.TrimSpace(text) + "\n\n" + domain.OutputContract(mode), nil
}

// ---------------------------------------------------------------- 编译

// compiledVariant 一套编译好的方案（content 只在本次请求里用，不进调用记录）。
type compiledVariant struct {
	storedVariant
	content timelinedomain.Content
}

type compiledResult struct {
	Variants []compiledVariant
	Changes  []string
}

func (c compiledResult) result() json.RawMessage {
	out := storedResult{Changes: c.Changes}
	for _, v := range c.Variants {
		out.Variants = append(out.Variants, v.storedVariant)
	}
	raw, _ := json.Marshal(out)
	return raw
}

func (c compiledResult) summaries() []VariantResult {
	out := make([]VariantResult, len(c.Variants))
	for i, v := range c.Variants {
		out[i] = v.VariantResult
	}
	return out
}

// compileRuns 把各套方案编译成时间线内容。全部失败时：都是调用失败（超时、上游报错）就报错，
// 有解析不合规的就走确定性兜底（SC-F-85）。返回要写进时间线的那一套的下标（第一套成功的）。
func (s *Service) compileRuns(r *planRun, f planFacts, runs []variantRun) (compiledResult, int, error) {
	out := compiledResult{Variants: make([]compiledVariant, len(runs))}
	applied := -1
	var firstCallErr error
	var invalid *domain.ErrDraftInvalid
	for k, run := range runs {
		v := compiledVariant{storedVariant: storedVariant{VariantResult: VariantResult{Index: k, Status: repo.StatusFailed}}}
		switch {
		case run.callErr != nil:
			v.Error = callErrText(run.callErr)
			if firstCallErr == nil {
				firstCallErr = run.callErr
			}
		case run.invalid != nil:
			v.Error = "AI 的方案无法解析：" + run.invalid.Reason
			if invalid == nil {
				invalid = run.invalid
			}
		default:
			d := run.parsed.Draft
			res, err := domain.Compile(f.in, d)
			if err != nil {
				var ue *domain.ErrUnsuitable
				if errors.As(err, &ue) && len(runs) == 1 {
					return out, 0, unsuitableErr(ue)
				}
				v.Error = err.Error()
				break
			}
			v = fromCompiled(k, res, f, run.parsed)
			v.Draft = &d
			if applied < 0 {
				applied = k
			}
		}
		out.Variants[k] = v
	}
	if applied >= 0 {
		return out, applied, nil
	}
	if invalid == nil {
		// 全是调用失败：不写时间线（用户重试即可），把第一处错误说清。
		return out, 0, mapCallErr(firstCallErr)
	}
	res, err := domain.Fallback(f.in)
	if err != nil {
		var ue *domain.ErrUnsuitable
		if errors.As(err, &ue) {
			return out, 0, unsuitableErr(ue)
		}
		return out, 0, httpx.Wrap(errcode.Internal, err)
	}
	v := fromCompiled(0, res, f, domain.ParseResult{})
	v.Fallback = true
	out.Variants[0] = v
	r.fallback = true
	r.errDetail = "模型输出修过一次仍不合规，已按规则兜底：" + invalid.Reason + "（原文开头：" + invalid.Snippet + "）"
	return out, 0, nil
}

// fromCompiled 编译结果 → 一套方案的摘要。警告里并上：被跳过的段（解析丢掉的）、模型说的素材问题、装配的缺口、没收进卡片的素材。
func fromCompiled(k int, res domain.Result, f planFacts, pr domain.ParseResult) compiledVariant {
	var warnings []string
	for _, g := range f.gaps {
		warnings = append(warnings, g.text())
	}
	warnings = append(warnings, res.Warnings...)
	for _, d := range pr.Dropped {
		warnings = append(warnings, "已跳过："+d)
	}
	for _, w := range pr.Draft.Warnings {
		warnings = append(warnings, "素材问题："+w)
	}
	warnings = append(warnings, f.skipped...)
	notes := append(append([]string(nil), res.Notes...), pr.Draft.Notes...)
	notes = append(notes, pr.Ignored...)
	if res.UsedFallback && (len(notes) == 0 || notes[0] != domain.FallbackNote) {
		notes = append([]string{domain.FallbackNote}, notes...)
	}
	v := compiledVariant{storedVariant: storedVariant{VariantResult: VariantResult{
		Index: k, Status: repo.StatusSuccess, Summary: res.Summary, TotalMs: res.TotalMs, UsedClips: nonNil(res.UsedClips),
		Reasons: res.Reasons, Warnings: nonNil(dedupeStrings(warnings)), UsedFallback: res.UsedFallback,
		Shortage: res.Shortage, Notes: nonNil(dedupeStrings(notes)),
	}}, content: res.Content}
	if v.Reasons == nil {
		v.Reasons = []domain.ClipReason{}
	}
	return v
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func dedupeStrings(list []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range list {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// forceSave 「仍然应用」：按当前时间线重新编译这一套再写入（最多重读两次）。
func (s *Service) forceSave(ctx context.Context, r *planRun, f planFacts, v storedVariant, reason, snapReason,
	fingerprint string) (SavedPlan, error) {
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		var cur timelinedomain.Content
		cur, err = s.plan.Timelines.Content(ctx, r.ws, r.tid)
		if err != nil {
			return SavedPlan{}, err
		}
		f.in.Base = cur
		var content timelinedomain.Content
		content, err = s.recompile(f, v, cur)
		if err != nil {
			return SavedPlan{}, err
		}
		var saved SavedPlan
		saved, err = s.plan.Timelines.Save(ctx, SavePlanInput{WorkspaceID: r.ws, TimelineID: r.tid, Editor: r.editor,
			Content: content, Reason: reason, SnapshotReason: snapReason, CallID: r.callID, Fingerprint: fingerprint})
		if err == nil || !isVersionConflict(err) {
			return saved, err
		}
	}
	return SavedPlan{}, err
}

// recompile 按存下的方案重新编译（追加指令按当前内容重放原子操作）。
func (s *Service) recompile(f planFacts, v storedVariant, base timelinedomain.Content) (timelinedomain.Content, error) {
	f.in.Base = base
	var res domain.Result
	var err error
	switch {
	case v.Fallback:
		res, err = domain.Fallback(f.in)
	case v.Draft == nil:
		return timelinedomain.Content{}, httpx.Errf(errcode.InvalidStateTransition, "这一套方案没有成功，不能应用")
	case v.Draft.Mode == domain.ModeEdit:
		res, err = domain.ApplyEdit(base, *v.Draft, f.in.Cards)
	default:
		res, err = domain.Compile(f.in, *v.Draft)
	}
	if err != nil {
		var ue *domain.ErrUnsuitable
		if errors.As(err, &ue) {
			return timelinedomain.Content{}, unsuitableErr(ue)
		}
		return timelinedomain.Content{}, httpx.Wrap(errcode.Internal, err)
	}
	return res.Content, nil
}

// saveCaptions 模型对看图素材的描述写进缓存（下次不再看图，SCT-27 要做第 5 条）。失败只告警。
func (s *Service) saveCaptions(ctx context.Context, r *planRun, f planFacts, runs []variantRun) {
	byNo := map[string]int64{}
	for _, c := range f.in.Cards {
		byNo[c.No] = c.MediaID
	}
	captions := map[int64]string{}
	for _, run := range runs {
		if !run.ok {
			continue
		}
		for _, n := range run.parsed.Draft.ClipNotes {
			if id := byNo[n.Clip]; id > 0 && !f.described[n.Clip] {
				if _, done := captions[id]; !done {
					captions[id] = n.Note
				}
			}
		}
	}
	if len(captions) == 0 {
		return
	}
	if err := s.plan.Material.SaveCaptions(ctx, r.ws, r.model, captions); err != nil {
		s.plan.Log.WarnContext(ctx, "AI 剪辑的看图描述没写进缓存", "callId", r.callID, "error", err)
	}
}

// ---------------------------------------------------------------- 调用记录、阶段与错误

// insertCall 限流并插入调用记录；超限时映射成 429。
func (s *Service) insertCall(ctx context.Context, r *planRun, c repo.Call) error {
	c.ID, c.WorkspaceID, c.ProjectID, c.TimelineID, c.CanvasNodeID = r.callID, r.ws, r.pid, r.tid, r.nodeID
	c.UserID, c.Source, c.Kind, c.ModelCode = r.actor.UserID, r.actor.source(), r.kind, r.model
	d, err := s.plan.Calls.InsertWithLimits(ctx, c, repo.Limits{Concurrency: s.plan.Config.Concurrency,
		PerHour: s.plan.Config.PerHour})
	if err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	if err := s.limitErr(d); err != nil {
		return err
	}
	r.started = s.now()
	s.publishCall(ctx, r, repo.StageSync, repo.StatusRunning)
	return nil
}

// advance 进入下一阶段：写调用记录并推送（失败只告警：阶段只是进度信号）。
func (s *Service) advance(ctx context.Context, r *planRun, stage string) {
	r.stage = stage
	if _, err := s.plan.Calls.SetStage(ctx, r.callID, stage); err != nil {
		s.plan.Log.WarnContext(ctx, "AI 剪辑阶段没写进调用记录", "callId", r.callID, "stage", stage, "error", err)
	}
	s.publishCall(ctx, r, stage, repo.StatusRunning)
}

// failCall 结束调用记录（failed）并把错误映射成对外错误。时间线一律不改。
func (s *Service) failCall(ctx context.Context, r *planRun, err error) error {
	sctx, cancel := settleContext(ctx)
	defer cancel()
	s.finishCall(sctx, r, repo.StatusFailed, nil, nil, err.Error())
	return err
}

// finishCall 结束调用记录；失败只告警（恢复扫描会在 20 分钟后兜底判失败，释放并发额度）。
func (s *Service) finishCall(ctx context.Context, r *planRun, status string, result json.RawMessage, applied *int,
	msg string) {
	if r.finished {
		return
	}
	r.finished = true
	in := repo.FinishInput{Status: status, Result: result, AppliedVariant: applied, UsedFallback: r.fallback,
		ErrorMessage: msg}
	if !r.started.IsZero() {
		in.DurationMs = int(s.now().Sub(r.started).Milliseconds())
	}
	changed, err := s.plan.Calls.Finish(ctx, r.callID, in)
	switch {
	case err != nil:
		s.plan.Log.WarnContext(ctx, "AI 剪辑调用记录结束失败，留待恢复扫描", "callId", r.callID, "error", err)
	case !changed:
		s.plan.Log.WarnContext(ctx, "AI 剪辑调用记录已被恢复扫描判失败，以库里为准", "callId", r.callID)
	}
	s.publishCall(ctx, r, repo.StageDone, status)
}

// limitErr 限流结论 → 429。详情与导演台、MCP 的限流同形（retry_after_seconds / scope / limit）。
func (s *Service) limitErr(d repo.LimitDecision) error {
	switch d.Reject {
	case repo.RejectConcurrency:
		return httpx.Errf(errcode.ConcurrencyLimited,
			"上一次 AI 剪辑还没结束，请等它完成（同时最多 "+strconv.Itoa(s.plan.Config.Concurrency)+" 个）").
			WithDetails(map[string]any{"retry_after_seconds": 5, "scope": "concurrency", "limit": s.plan.Config.Concurrency})
	case repo.RejectHourly:
		secs := int(d.RetryAfter / time.Second)
		return httpx.Errf(errcode.RateLimited,
			"本小时的 AI 剪辑次数已用完（每小时 "+strconv.Itoa(s.plan.Config.PerHour)+" 次），"+waitText(secs)+"后再试").
			WithDetails(map[string]any{"retry_after_seconds": secs, "scope": "hourly", "limit": s.plan.Config.PerHour})
	}
	return nil
}

func waitText(secs int) string {
	if secs < 60 {
		return strconv.Itoa(secs) + " 秒"
	}
	return strconv.Itoa((secs+59)/60) + " 分钟"
}

// unsuitableErr 42217：说清缺什么，并给出路（SC-UX-09）。
func unsuitableErr(e *domain.ErrUnsuitable) error {
	options := map[string][]string{
		domain.UnsuitableNoPicture:  {"connect_material"},
		domain.UnsuitableNoShots:    {"fill_shot"},
		domain.UnsuitableNoMusic:    {"connect_audio", "ignore_beat"},
		domain.UnsuitableNoVoice:    {"connect_audio"},
		domain.UnsuitableASRMissing: {"transcribe"},
	}[e.Reason]
	if options == nil {
		options = []string{}
	}
	return httpx.Errf(errcode.SmartcutInputUnsuitable, e.Detail).
		WithDetails(map[string]any{"reason": e.Reason, "options": options})
}

func unavailableErr(u *Unavailable) error {
	return httpx.Errf(errcode.SmartcutPlanUnavailable, u.Reason).
		WithDetails(map[string]any{"reason": u.Reason, "userFixable": u.UserFixable})
}

func mapCallErr(err error) error {
	var to *callTimeout
	if errors.As(err, &to) {
		return httpx.Errf(errcode.ProviderTimeout, to.Error()+"，请稍后再试")
	}
	var be *httpx.BizError
	if errors.As(err, &be) {
		return be
	}
	return httpx.Errf(errcode.ProviderError, "AI 剪辑没有完成："+err.Error())
}

func callErrText(err error) string {
	var to *callTimeout
	if errors.As(err, &to) {
		return to.Error()
	}
	return "模型调用失败：" + err.Error()
}

// withCallID 把调用记录 ID 放进冲突详情（「仍然应用」要用它，SC-F-86）。
func withCallID(err error, callID int64) error {
	return withCallIDSaying(err, callID, "AI 剪辑期间时间线有改动，方案没有写入：")
}

// withVariant 在冲突详情里补上本来要写入的是第几套方案（details.variant，从 0 起）。
func withVariant(err error, variant int) error {
	var be *httpx.BizError
	if !errors.As(err, &be) {
		return err
	}
	details := map[string]any{}
	for k, v := range be.Details {
		details[k] = v
	}
	details["variant"] = variant
	return be.WithDetails(details)
}

// withCallIDSaying 同 withCallID，说明的开头由调用方给（翻译：「译文没有写入」）。
func withCallIDSaying(err error, callID int64, lead string) error {
	var be *httpx.BizError
	if !errors.As(err, &be) {
		return err
	}
	details := map[string]any{}
	for k, v := range be.Details {
		details[k] = v
	}
	details["callId"] = strconv.FormatInt(callID, 10)
	msg := be.Message
	if msg == "" {
		msg = be.Error()
	}
	return httpx.Errf(be.Code, lead+msg).WithDetails(details)
}

func headText(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "……"
}

// settleContext 给「模型已经返回（或失败）之后」的落库用：脱离请求的取消。
// ⚠️ 必须在模型返回之后再建：建早了，它自己的计时会在等模型的那几分钟里先到期。
func settleContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), planSettleTimeout)
}
