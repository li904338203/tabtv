package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
)

// 模型 ↔ 工具循环（dev/01 §4.2 第 4 步）。
//
// 每次调用模型之前：查停止请求、实时查发起人角色、续租约、按**当前状态**重算下发的工具。
// 工具调用逐个执行，执行前**再校验一次白名单**（模型可能凭历史幻觉出一个未下发的工具名，
// 那一次调用得到错误结果、本轮继续 —— 不让一次幻觉毁掉整轮）。

const (
	deltaFlushInterval = 50 * time.Millisecond
	deltaFlushRunes    = 64
	maxToolNameRunes   = 48 // agent_step.tool varchar(48)
)

// TruncationNote 是上下文首次被截断时推给用户的提示（system_note，不进模型上下文）。
const TruncationNote = "对话已经很长，较早的内容只能以摘要形式提供给助手，细节可能遗漏。建议新开一个对话继续。"

func (r *Runner) loop(ctx context.Context, run *turnRun, role collab.Role) {
	noted := false
	for {
		if r.shuttingDown(ctx, run) {
			return
		}
		if run.calls >= r.cfg.MaxCalls {
			run.callLimit = true
			r.note(ctx, run, fmt.Sprintf("本轮已达到调用上限（%d 次），先停在这里。需要继续的话请再发一条消息。",
				r.cfg.MaxCalls))
			return
		}
		if r.stopRequested(ctx, run) {
			run.stopped = true
			return
		}
		var ok bool
		role, ok = r.checkRole(ctx, run)
		if !ok {
			return
		}
		if !r.renew(ctx, run.sess.ID, run) {
			return
		}

		cctx, wl, err := r.assemble(ctx, run)
		if err != nil {
			run.errCode, run.runErr = ErrCodeModel, err
			return
		}
		if cctx.Truncated && run.sess.Summary == "" && !noted {
			noted = true
			r.note(ctx, run, TruncationNote)
		}
		if cctx.Truncated && cctx.Summary != run.sess.Summary {
			if err := r.d.Store.UpdateSummary(ctx, run.sess.ID, cctx.Summary); err != nil {
				r.d.Logger.Warn("缓存早先对话摘要失败", "sessionId", run.sess.ID, "error", err)
			} else {
				run.sess.Summary = cctx.Summary
			}
		}

		// 场景专属工具（MKT-T-18）只在对应场景下发：通用会话的工具列表逐字不变。
		names := domain.AllowedToolsIn(run.sess.Scene, run.state, wl)
		var defs []ToolDef
		if r.d.Tools != nil {
			defs = r.d.Tools.Schemas(names)
		}
		// view_media 的附图只进紧接着的这一次请求（MKT-T-19），发完即清。
		msgs := injectViewImages(cctx.Messages, run.images.pending)
		run.images.pending = nil
		out, streamed, err := r.callModel(ctx, run, ChatInput{
			ModelCode: run.model, System: cctx.System, Messages: msgs, Tools: defs,
			MaxOutputTokens: r.cfg.MaxOutputTokens,
		})
		run.calls++
		if run.leaseLost.Load() {
			return
		}
		if err != nil && r.shuttingDown(ctx, run) {
			// 被退出信号打断的调用：半截文本不落档（接手的 Worker 会重新调一次，
			// 落了的话上下文里会多出半句没说完的话）。
			return
		}
		if run.stopped {
			// 已经流出来的文本保留（AG-19）：用户按停止时看到的那半句话不该凭空消失。
			if strings.TrimSpace(streamed.text) != "" {
				run.valid++
				r.appendAssistant(ctx, run, streamed.msgID, streamed.text, nil)
			}
			return
		}
		if err != nil {
			// ⚠️ 不重发（dev/01 §7.2）：超时 / 中途断开时上游可能已经计费。
			// 已流出的半截文本留档但不算有效输出（它可能是半句话）。
			if strings.TrimSpace(streamed.text) != "" {
				r.appendAssistant(ctx, run, streamed.msgID, streamed.text, nil)
			}
			run.errCode, run.runErr = ErrCodeModel, err
			return
		}
		text := out.Text
		if text == "" {
			text = streamed.text
		}
		if strings.TrimSpace(text) != "" {
			run.valid++
		}
		calls := ensureToolCallIDs(out.ToolCalls, streamed.msgID)
		r.appendAssistant(ctx, run, streamed.msgID, text, calls)
		if out.FinishReason == FinishLength {
			// 输出达到长度上限被截断（04 文档 P-09）。
			if len(calls) == 0 {
				r.note(ctx, run, "回复达到了单次输出的长度上限，后面的内容被截断了。可以让助手接着写，或者把要求拆小一些。")
				return
			}
			// 截断时最后一个工具调用的参数多半不完整：照着执行可能产出残缺的文档或计划。
			// 这一批调用一律不执行，告诉模型原因，让它拆小之后重来（计入调用上限）。
			for _, tc := range calls {
				r.refuseTool(ctx, run, tc, truncatedCallMessage)
			}
			continue
		}
		if len(calls) == 0 {
			return // 模型结束本轮
		}
		for _, tc := range calls {
			if r.shuttingDown(ctx, run) {
				return
			}
			if r.stopRequested(ctx, run) {
				run.stopped = true
				return
			}
			if role, ok = r.checkRole(ctx, run); !ok {
				return
			}
			r.execTool(ctx, run, role, wl, tc)
			if run.suspend != nil {
				return
			}
		}
	}
}

// FinishLength 是「输出达到长度上限被截断」的归一结束原因（与 provider/protocol.FinishLength 同值）。
const FinishLength = "length"

// truncatedCallMessage 是被截断的那一批工具调用的结果。
const truncatedCallMessage = "上一条回复达到了单次输出的长度上限，被截断了，这批工具调用的参数可能不完整，所以都没有执行。" +
	"请把内容拆小后重试：例如长文档分几次 write_doc，一次少提交几个计划项。"

// ensureToolCallIDs 给缺 id 的工具调用补一个（04 文档 P-11）。
//
// 有的中转不回 tool_call 的 id。不补的话这条助手消息落库后，下一次请求回填工具结果时
// 两家协议都会以「缺少 ToolCallID」拒绝构造请求，这个会话从此每一轮都失败。
// 补出来的 id 只要在本条消息里唯一、写库后保持不变即可。
func ensureToolCallIDs(calls []ToolCall, msgID int64) []ToolCall {
	for i := range calls {
		if strings.TrimSpace(calls[i].ID) == "" {
			calls[i].ID = fmt.Sprintf("call_%d_%d", msgID, i+1)
		}
	}
	return calls
}

// refuseTool 记一次不执行的工具调用：留一个失败步骤，并回一条工具结果。
// 每个 tool_call 都必须有结果，否则下一次请求两家协议都会拒。
func (r *Runner) refuseTool(ctx context.Context, run *turnRun, tc ToolCall, msg string) {
	stepID := r.d.NextID()
	args := tc.Arguments
	if len(args) == 0 {
		b, _ := json.Marshal(map[string]string{"_raw": truncRunes(tc.RawArguments, 2000)})
		args = b
	}
	if _, err := r.d.Store.InsertStep(ctx, repo.Step{ID: stepID, SessionID: run.sess.ID, TurnID: run.turn.ID,
		Tool: truncRunesExact(tc.Name, maxToolNameRunes), Attempt: 1, Args: args}); err != nil {
		r.d.Logger.Error("写工具步骤失败", "sessionId", run.sess.ID, "tool", tc.Name, "error", err)
	}
	r.finishStep(ctx, run, stepID, tc, repo.StepFailed, nil, msg)
	b, _ := json.Marshal(map[string]string{"error": msg})
	r.appendTool(ctx, run, tc.ID, string(b))
}

// checkRole 实时查发起人角色。不是成员 → lostPerm（会话取消）。查询出错按「本轮出错」处理。
func (r *Runner) checkRole(ctx context.Context, run *turnRun) (collab.Role, bool) {
	role, member, err := r.d.Members.Role(ctx, run.sess.WorkspaceID, run.sess.CreatedByUserID)
	if err != nil {
		run.errCode, run.runErr = ErrCodeModel, fmt.Errorf("查询发起人权限失败: %w", err)
		return "", false
	}
	if !member {
		run.lostPerm = true
		return "", false
	}
	return role, true
}

// stopRequested 查会话上是否有**本轮创建之后**的停止请求。
//
// ⚠️ 只认本轮创建之后的请求：上一轮按的停止不能把下一轮刚开始就停掉。
func (r *Runner) stopRequested(ctx context.Context, run *turnRun) bool {
	s, err := r.d.Store.GetSession(ctx, run.sess.ID)
	if err != nil {
		return false
	}
	return s.StopRequestedAt != nil && !s.StopRequestedAt.Before(run.turn.CreatedAt)
}

// assemble 组装本次调用的上下文，返回技能工具白名单。
func (r *Runner) assemble(ctx context.Context, run *turnRun) (ContextOutput, []domain.ToolName, error) {
	msgs, err := r.d.Store.RecentMessages(ctx, run.sess.ID, recentMessageLimit)
	if err != nil {
		return ContextOutput{}, nil, fmt.Errorf("读取会话消息失败: %w", err)
	}
	// 「是否还有更早的」按取出的原始条数判，要在过滤之前算。
	hasOlder := len(msgs) >= recentMessageLimit
	msgs = ConversationOrder(msgs, run.turn.ID)
	// 每次调用前重读已加载的技能：load_skill 在本轮中途写了 skill_codes，
	// 只用轮次开始时的快照的话，技能主说明与白名单要到下一轮才生效（AGT-23）。
	if s, err := r.d.Store.GetSession(ctx, run.sess.ID); err == nil {
		run.sess.SkillCodes = s.SkillCodes
	}
	skills, wl := r.loadSkills(ctx, run)
	in := ContextInput{
		State: run.state, Skills: skills, Messages: msgs,
		HasOlder: hasOlder, PrevSummary: run.sess.Summary,
		Budget: r.cfg.ContextChars,
		Scene:  run.sess.Scene,
	}
	// ③④ 营销会话的场景段与营销输入（MKT-T-18）；通用会话两段为空。
	// 首条用户消息附商品图（MKT-T-19）：张数写进营销输入（「能看到 #1~#k」），与实际附上的必须一致 ——
	// 首条消息被截掉时两边都是 0。
	visible := 0
	if run.sess.Scene == domain.SceneMarketing {
		r.loadSceneImages(ctx, run)
		if len(run.images.first) > 0 {
			if id, ok := firstUserKept(msgs, in.HasOlder, in.Budget); ok {
				in.FirstUserMessageID, in.FirstUserImages = id, run.images.first
				visible = len(run.images.first)
			}
		}
	}
	in.SceneNote, in.SceneInput = sceneSections(ctx, r.d.Scene, r.d.Logger, run.sess, run.state, run.model,
		visible)
	mp := r.modelSections(ctx, run)
	in.VideoModel, in.Prefs = mp.video, mp.prefs
	if r.d.Documents != nil {
		d, err := r.d.Documents.Get(ctx, run.sess.WorkspaceID, run.sess.ProjectID, collab.RoleViewer,
			MemoryDocPath)
		switch {
		case err == nil:
			in.Memory = d.Content
		case !errors.Is(err, ErrDocumentNotFound):
			r.d.Logger.Debug("读取项目记忆失败，跳过", "projectId", run.sess.ProjectID, "error", err)
		}
	}
	if r.d.Canvas != nil {
		if v, err := r.d.Canvas.Read(ctx, run.sess.WorkspaceID, run.sess.ProjectID); err == nil {
			in.CanvasData = v.CanvasData
		} else {
			r.d.Logger.Debug("读取画布失败，摘要留空", "projectId", run.sess.ProjectID, "error", err)
		}
	}
	return BuildContext(in), wl, nil
}

// loadSkills 读会话已加载的技能。白名单取各技能的并集；任一技能不收窄（空白名单）则整体不收窄。
func (r *Runner) loadSkills(ctx context.Context, run *turnRun) ([]SkillPrompt, []domain.ToolName) {
	var codes []string
	if len(run.sess.SkillCodes) == 0 || json.Unmarshal(run.sess.SkillCodes, &codes) != nil {
		return nil, nil
	}
	var (
		out      []SkillPrompt
		wl       []domain.ToolName
		seen     = map[domain.ToolName]bool{}
		noNarrow = false
	)
	for _, c := range codes {
		s, err := r.d.Store.SkillByCode(ctx, c)
		if err != nil || !s.IsEnabled {
			continue
		}
		body := s.Body
		if r.d.SkillRender != nil {
			// ⚠️ 与 load_skill 用同一份变量（SkillVars）。渲染失败不回落原文：原文里的
			// `{{var}}` 会被模型当字面量照着做而不报错（§5.7）；换成一句说明并记错误日志。
			rendered, err := r.d.SkillRender.RenderSkill(body, SkillVars(run.state, run.model))
			if err != nil {
				r.d.Logger.Error("技能正文渲染失败，未放入系统提示词", "skill", s.Code, "error", err)
				rendered = "（技能「" + s.Name + "」的主说明渲染失败，本次未加载：" + err.Error() + "）"
			}
			body = rendered
		}
		out = append(out, SkillPrompt{Code: s.Code, Name: s.Name, Body: body})
		var tools []string
		_ = json.Unmarshal(s.Tools, &tools)
		if len(tools) == 0 {
			noNarrow = true
			continue
		}
		for _, t := range tools {
			n := domain.ToolName(t)
			if !seen[n] {
				seen[n] = true
				wl = append(wl, n)
			}
		}
	}
	if noNarrow || len(out) == 0 {
		return out, nil
	}
	return out, wl
}

type streamedText struct {
	msgID int64
	text  string
}

// callModel 发一次推理调用；delta 合并后推 agent.delta。
//
// 停止请求由一个后台检查每秒查一次库：命中就取消这次调用的 ctx（中止流式读取）。
// ⚠️ 不只在 delta 回调里查：上游卡住不吐字的时候回调根本不会被调用，用户按了停止
// 要等满空闲超时（60s）才生效。
func (r *Runner) callModel(ctx context.Context, run *turnRun, in ChatInput) (ChatOutput, streamedText, error) {
	msgID := r.d.NextID()
	var (
		mu      sync.Mutex
		sb      strings.Builder
		stopHit atomic.Bool
	)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	co := newDeltaCoalescer(r.d.Now, func(s string) {
		r.publish(ctx, run.sess.ProjectID, &AgentDeltaEvent{Type: EventAgentDelta,
			SessionID: idStr(run.sess.ID), TurnID: idStr(run.turn.ID), MessageID: idStr(msgID), Text: s})
	})
	done := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		t := time.NewTicker(stopCheckEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-cctx.Done():
				return
			case <-t.C:
				if r.stopRequested(ctx, run) {
					stopHit.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	out, err := r.d.Chat.Chat(cctx, in, func(d string) {
		if stopHit.Load() {
			return
		}
		mu.Lock()
		sb.WriteString(d)
		co.Add(d)
		mu.Unlock()
	})
	close(done)
	<-watcherDone
	mu.Lock()
	co.Flush()
	text := sb.String()
	mu.Unlock()
	if stopHit.Load() {
		run.stopped = true
	}
	return out, streamedText{msgID: msgID, text: text}, err
}

func (r *Runner) appendAssistant(ctx context.Context, run *turnRun, id int64, text string, calls []ToolCall) {
	m := repo.Message{ID: id, SessionID: run.sess.ID, TurnID: run.turn.ID, Role: repo.RoleAssistant,
		Content: text}
	if len(calls) > 0 {
		b, _ := json.Marshal(calls)
		m.ToolCalls = b
	}
	if _, err := r.d.Store.AppendMessage(ctx, m); err != nil {
		r.d.Logger.Error("写助手消息失败", "sessionId", run.sess.ID, "error", err)
	}
}

// note 写一条只给用户看的说明（system_note，不进模型上下文）。
func (r *Runner) note(ctx context.Context, run *turnRun, text string) {
	if _, err := r.d.Store.AppendMessage(ctx, repo.Message{ID: r.d.NextID(), SessionID: run.sess.ID,
		TurnID: run.turn.ID, Role: repo.RoleSystemNote, Content: text}); err != nil {
		r.d.Logger.Warn("写系统说明失败", "sessionId", run.sess.ID, "error", err)
	}
}

// execTool 执行一次工具调用：写步骤 → 二次校验 → 执行 → 写结果消息 → 推 agent.step。
//
// ⚠️ 每一次调用都必须留下一条 tool 消息（成功或错误），否则下一次请求里那个
// tool_call 没有回应，两家协议都会拒。
func (r *Runner) execTool(ctx context.Context, run *turnRun, role collab.Role, wl []domain.ToolName,
	tc ToolCall) {
	name := domain.ToolName(tc.Name)
	stepID := r.d.NextID()
	args := tc.Arguments
	stored := args
	if len(stored) == 0 {
		if tc.RawArguments != "" {
			b, _ := json.Marshal(map[string]string{"_raw": tc.RawArguments})
			stored = b
		} else {
			args, stored = json.RawMessage(`{}`), json.RawMessage(`{}`)
		}
	}
	step, err := r.d.Store.InsertStep(ctx, repo.Step{ID: stepID, SessionID: run.sess.ID, TurnID: run.turn.ID,
		Tool: truncRunesExact(tc.Name, maxToolNameRunes), Attempt: 1, Args: stored})
	if err != nil {
		r.d.Logger.Error("写工具步骤失败", "sessionId", run.sess.ID, "tool", tc.Name, "error", err)
	} else {
		r.publish(ctx, run.sess.ProjectID, &AgentStepEvent{Type: EventAgentStep, SessionID: idStr(run.sess.ID), TurnID: idStr(run.turn.ID),
			StepID: idStr(step.ID), Tool: step.Tool, Status: repo.StepRunning})
	}

	fail := func(msg string) {
		r.finishStep(ctx, run, stepID, tc, repo.StepFailed, nil, msg)
		b, _ := json.Marshal(map[string]string{"error": msg})
		r.appendTool(ctx, run, tc.ID, string(b))
	}

	// 二次校验：本次没有下发的工具一律拒（AG-05 的执行侧那一道）。
	if !domain.IsToolAllowedIn(run.sess.Scene, run.state, wl, name) {
		fail(fmt.Sprintf("工具 %s 在当前阶段不可用（本次没有提供给你），请只使用已提供的工具", tc.Name))
		return
	}
	tool, ok := (Tool)(nil), false
	if r.d.Tools != nil {
		tool, ok = r.d.Tools.Lookup(name)
	}
	if !ok {
		fail(fmt.Sprintf("工具 %s 暂未开放", tc.Name))
		return
	}
	spec, _ := domain.SpecOf(name)
	if !role.Can(collab.Action(spec.Action)) {
		fail(fmt.Sprintf("发起人在该工作区的角色（%s）没有「%s」权限，无法使用 %s", role, spec.Action, tc.Name))
		return
	}
	if tc.Arguments == nil && tc.RawArguments != "" {
		fail("参数不是合法 JSON，请修正后重试：" + truncRunes(tc.RawArguments, 200))
		return
	}

	res, err := tool.Run(ctx, RunContext{
		WorkspaceID: run.sess.WorkspaceID, ProjectID: run.sess.ProjectID, SessionID: run.sess.ID,
		TurnID: run.turn.ID, StepID: stepID, Attempt: 1, UserID: run.sess.CreatedByUserID, Role: role,
		PayerAccountID: run.sess.PayerAccountID, MemberLevelID: run.level, State: run.state,
		ModelCode: run.model, Scene: run.sess.Scene, Spec: r.videoSpec(ctx, run),
	}, args)
	if err != nil {
		// 步骤标记（MKT-T-22）：计划项键与错误码先落库，停后续 / 部分成功按码判、不按文案。
		key, code := stepTagOf(err)
		r.tagStep(ctx, stepID, key, code)
		// 按规则拒绝（MKT-T-20，如 too_many_images）：结果带错误码与改法所需的数字（max…），
		// 只给一句话的话模型不知道该减到几张。
		var rej *domain.ToolRejection
		if errors.As(err, &rej) {
			r.finishStep(ctx, run, stepID, tc, repo.StepFailed, nil, rej.Error())
			r.appendTool(ctx, run, tc.ID, string(rej.Content()))
			return
		}
		fail(err.Error())
		return
	}
	// view_media 的附图（MKT-T-19）：收不下（模型不收图 / 一次看太多）就按工具错误回给模型，本轮继续。
	if len(res.AttachImages) > 0 {
		if ok, reason := r.takeViewImages(run, tc.ID, res); !ok {
			fail(reason)
			return
		}
	}
	r.tagStep(ctx, stepID, res.PlanItemKey, "")
	content := res.Content
	if len(content) == 0 {
		content = json.RawMessage(`{"ok":true}`)
	}
	if res.GenerationID != 0 || res.WaitRef {
		if _, err := r.d.Store.SetStepWaiting(ctx, stepID, res.GenerationID); err != nil {
			r.d.Logger.Error("标记步骤等待任务失败", "stepId", stepID, "error", err)
		}
		r.publish(ctx, run.sess.ProjectID, &AgentStepEvent{Type: EventAgentStep, SessionID: idStr(run.sess.ID), TurnID: idStr(run.turn.ID),
			StepID: idStr(stepID), Tool: tc.Name, Status: repo.StepWaiting, Display: res.Display})
	} else {
		r.finishStep(ctx, run, stepID, tc, repo.StepSuccess, res.Display, "")
	}
	r.appendTool(ctx, run, tc.ID, string(content))
	n := res.Outputs
	if n < 1 {
		n = 1
	}
	run.valid += n
	if res.Suspend != nil {
		s := *res.Suspend
		run.suspend = &s
	}
}

func (r *Runner) finishStep(ctx context.Context, run *turnRun, stepID int64, tc ToolCall, status string,
	display json.RawMessage, errMsg string) {
	if _, err := r.d.Store.FinishStep(ctx, stepID, status, display, errMsg); err != nil {
		r.d.Logger.Error("结束工具步骤失败", "stepId", stepID, "error", err)
	}
	r.publish(ctx, run.sess.ProjectID, &AgentStepEvent{Type: EventAgentStep, SessionID: idStr(run.sess.ID), TurnID: idStr(run.turn.ID),
		StepID: idStr(stepID), Tool: truncRunesExact(tc.Name, maxToolNameRunes), Status: status,
		Display: display})
}

// tagStep 写步骤的计划项键与错误码（都为空时不写）。写失败只记日志：标记缺了是统计不准，
// 不能为它让本轮失败（钱已经按受理的结果扣 / 退了）。
func (r *Runner) tagStep(ctx context.Context, stepID int64, planItemKey, errorCode string) {
	if planItemKey == "" && errorCode == "" {
		return
	}
	if err := r.d.Store.TagStep(ctx, stepID, planItemKey, errorCode); err != nil {
		r.d.Logger.Error("记录步骤计划项 / 错误码失败", "stepId", stepID, "planItemKey", planItemKey,
			"errorCode", errorCode, "error", err)
	}
}

func (r *Runner) appendTool(ctx context.Context, run *turnRun, callID, content string) {
	if _, err := r.d.Store.AppendMessage(ctx, repo.Message{ID: r.d.NextID(), SessionID: run.sess.ID,
		TurnID: run.turn.ID, Role: repo.RoleTool, Content: content, ToolCallID: callID}); err != nil {
		r.d.Logger.Error("写工具结果消息失败", "sessionId", run.sess.ID, "error", err)
	}
}

func truncRunesExact(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// deltaCoalescer 把高频的流式增量按 50ms 或 64 字符合并后再发（dev/01 §8）。
//
// ⚠️ 不起定时器：只在新增量到来时判断「距上次发出是否已过 50ms」，结束时 Flush 补发尾巴。
// delta 是纯展示，最后一段晚一点到无所谓；为它起一个 goroutine 反而要处理并发。
type deltaCoalescer struct {
	now   func() time.Time
	emit  func(string)
	buf   strings.Builder
	runes int
	last  time.Time
}

func newDeltaCoalescer(now func() time.Time, emit func(string)) *deltaCoalescer {
	return &deltaCoalescer{now: now, emit: emit, last: now()}
}

// Add 追加一段增量；够 64 字符或距上次发出已满 50ms 就发。
func (c *deltaCoalescer) Add(s string) {
	if s == "" {
		return
	}
	c.buf.WriteString(s)
	c.runes += utf8.RuneCountInString(s)
	if c.runes >= deltaFlushRunes || c.now().Sub(c.last) >= deltaFlushInterval {
		c.Flush()
	}
}

// Flush 发出缓冲里剩下的内容。
func (c *deltaCoalescer) Flush() {
	if c.buf.Len() == 0 {
		return
	}
	c.emit(c.buf.String())
	c.buf.Reset()
	c.runes = 0
	c.last = c.now()
}

// modelPromptSections 是系统提示词里与模型选择有关的两节。
type modelPromptSections struct {
	video string
	prefs string
}

// modelSections 算「当前视频模型」与「创作偏好」两节；一轮内只算一次（要查目录里好几个模型，
// 一轮最多 20 次模型调用，每次都查一遍不值；会话偏好在一轮内也不会变）。
func (r *Runner) modelSections(ctx context.Context, run *turnRun) modelPromptSections {
	if run.modelPrompt != nil {
		return *run.modelPrompt
	}
	var out modelPromptSections
	if r.d.VideoModels != nil {
		code := resolveVideoModel(run.sess)
		var info *VideoModelInfo
		var lookupErr error
		var fits []domain.OperationFit
		var usable []ModelBrief
		if code != "" {
			v, err := r.d.VideoModels.VideoModel(ctx, code)
			if err != nil {
				lookupErr = err
				r.d.Logger.Debug("查询视频模型能力失败", "model", code, "error", err)
			} else {
				info = &v
			}
			if r.d.Ops != nil && info != nil && info.IsVideo {
				if f, err := r.d.Ops.ModelOperations(ctx, code); err == nil {
					fits = f
				}
			}
		} else if r.d.Models != nil {
			if l, err := r.d.Models.UsableModels(ctx, "video", run.level); err == nil {
				usable = l
			}
		}
		out.video = VideoModelSection(code, info, lookupErr)
		if extra := VideoModelExtras(code, fits, usable); extra != "" {
			out.video += "\n" + extra
		}
	}
	var images []ModelBrief
	if r.d.Models != nil && !hasImagePref(run.sess.Prefs) {
		if l, err := r.d.Models.UsableModels(ctx, "image", run.level); err == nil {
			images = l
		}
	}
	out.prefs = PrefsSection(run.sess.Prefs, r.videoSpec(ctx, run), images)
	run.modelPrompt = &out
	return out
}

// maxSpecCards 是取规格回答时最多看几张已回答的问答卡（最新的在前；更早的早已被后来的回答盖过）。
const maxSpecCards = 20

// videoSpec 本轮视频规格的生效值：偏好 → 问答卡里按固定题目 id 问到的回答 → 缺（一轮只算一次）。
//
// ⚠️ 读问答卡失败时不报缺：缺项会让营销会话的 submit_plan 被退回，一次数据库抖动不该逼用户再答一遍。
func (r *Runner) videoSpec(ctx context.Context, run *turnRun) VideoSpecState {
	if run.spec != nil {
		return *run.spec
	}
	cards, err := r.d.Store.AnsweredQuestionCards(ctx, run.sess.ID, maxSpecCards)
	readFailed := err != nil
	if readFailed {
		r.d.Logger.Warn("读取已回答的问答卡失败，视频规格只按偏好算", "session", run.sess.ID, "error", err)
	}
	list := make([]domain.AnsweredQuestions, 0, len(cards))
	for _, c := range cards {
		list = append(list, domain.AnsweredQuestions{Payload: c.Payload, Submission: c.Submission})
	}
	spec := ResolveVideoSpec(ParseGenPrefs(run.sess.Prefs), domain.SpecAnswers(list))
	if readFailed {
		spec.Missing = nil
	}
	run.spec = &spec
	return spec
}

func hasImagePref(prefs json.RawMessage) bool {
	var p struct {
		Image struct {
			Model string `json:"model"`
		} `json:"image"`
	}
	return len(prefs) > 0 && json.Unmarshal(prefs, &p) == nil && strings.TrimSpace(p.Image.Model) != ""
}
