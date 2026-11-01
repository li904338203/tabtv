package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
)

// 轮次运行器（Worker 侧，dev/01 §4.2）：
//
//	租约 → 计费 → 组装上下文 → 模型↔工具循环 → 结算 → 释放
//
// 计费顺序照抄脚本线（script/app.GenerateStoryboard）：报价 → 记报价 → 扣费 → 调模型 →
// 按有效输出结算，无有效输出全额退款。区别只在 refType=agent_turn、refID=turnID，
// 以及「一轮 = 一次用户动作」：系统唤醒的轮（trigger=system）不收费。

const (
	// TurnQueue 是 Worker 消费的队列名（不含前缀）。
	TurnQueue = "agent.turns"
	// TurnTopic 是该队列绑定的 routing key（= outbox topic）。
	TurnTopic = domain.TopicTurnRequested
	// RefTypeTurn 是一轮对话费在账本里的 refType。
	RefTypeTurn = "agent_turn"

	// LeaseTTL 运行租约；崩溃后最迟这么久别的 Worker 能接手。
	LeaseTTL = 90 * time.Second
	// leaseRenewEvery 续租间隔（长于一次流式调用也不会丢租约）。
	leaseRenewEvery = 30 * time.Second
	// settleTimeout 结算（落终态、退款、迁移状态）的时限。
	settleTimeout = 30 * time.Second
	// stopCheckEvery 模型调用期间查「停止请求」的间隔：每个 delta 查一次库太密。
	stopCheckEvery = time.Second
	// recentMessageLimit 组装上下文时取的最近消息条数上限（再按字符预算截）。
	recentMessageLimit = 400
)

// 错误码（agent_turn.error_code）。
const (
	ErrCodeInsufficientCredits = "insufficient_credits"
	ErrCodeQuoteFailed         = "quote_failed"
	ErrCodeNoModel             = "model_not_selected"
	ErrCodeLedger              = "ledger_error"
	ErrCodeModel               = "model_error"
	ErrCodeStopped             = "stopped"
	ErrCodeLostPermission      = "lost_permission"
	ErrCodeSessionNotRunnable  = "session_not_runnable"
	ErrCodeEmptyReply          = "empty_reply"
	ErrCodeCallLimit           = "call_limit"
)

// Outcome 是一次 RunTurn 的结局，消费者据此决定怎么 ack。
type Outcome int

const (
	// OutcomeDone 跑完（成功或失败都已落库）。
	OutcomeDone Outcome = iota
	// OutcomeDuplicate 轮次已是终态 / 不存在：重复消息，ack 丢弃。
	OutcomeDuplicate
	// OutcomeBusy 会话租约在别的 Worker 手上：ack 丢弃（at-least-once 的正常结局）。
	OutcomeBusy
	// OutcomeRequeue 还没碰任何状态就遇到了基础设施错误：放回队列稍后再试。
	OutcomeRequeue
	// OutcomeLeaseLost 运行中租约被别人抢走：立即停手，不结算（由持有者 / 扫描循环负责）。
	OutcomeLeaseLost
)

func (o Outcome) String() string {
	switch o {
	case OutcomeDone:
		return "done"
	case OutcomeDuplicate:
		return "duplicate"
	case OutcomeBusy:
		return "busy"
	case OutcomeRequeue:
		return "requeue"
	case OutcomeLeaseLost:
		return "lease_lost"
	}
	return "unknown"
}

// RunnerConfig 是运行器的参数（来自 config.Agent）。
type RunnerConfig struct {
	MaxCalls     int
	ContextChars int
	// MaxOutputTokens 推理模型单次输出的 token 上限缺省值（04 文档 P-09）。
	// 只对**必须带上限**的协议生效（Anthropic 的 max_tokens 必填，原先写死 4096），不必带的协议忽略它。
	// 0 = 用协议自己的缺省值。
	MaxOutputTokens int
}

// RunnerDeps 是运行器的依赖。Members / Quoter / Ledger / ChatModel / Store 必须给；
// 其余为 nil 时对应能力缺省（无事件、无文档记忆、无画布摘要、按免费档报价）。
type RunnerDeps struct {
	Store        RunnerStore
	Chat         ChatModel
	Quoter       Quoter
	Ledger       Ledger
	Members      Members
	MemberLevels MemberLevels
	Events       Events
	Documents    Documents
	Canvas       Canvas
	Tools        ToolSet
	SkillRender  SkillRenderer     // 渲染技能正文（AGT-23）；nil = 原样拼进系统提示词
	VideoModels  VideoModels       // 系统提示词「当前视频模型」一节的事实来源；nil = 不写这一节
	Ops          OperationCatalog  // 所选视频模型能用的生成方式（写进「当前视频模型」）；nil = 不列
	Models       ModelDirectory    // 智能匹配时可选的视频 / 图片模型；nil = 不列
	Scene        SceneContext      // 营销会话的场景段与营销输入（MKT-T-18）；nil = 营销会话只写场景标题并告警
	Images       SceneImages       // 营销会话给推理模型看商品图（MKT-T-19）；nil = 不附图、view_media 报不可用
	Reasoning    ReasoningDefaults // 会话未选推理模型时的兜底；nil = 不兜底（本轮 model_not_selected）
	NextID       func() int64
	Logger       *slog.Logger
	// Now 测试注入时钟（delta 合并）；nil = time.Now。
	Now func() time.Time
}

// Runner 跑一轮。
type Runner struct {
	d        RunnerDeps
	cfg      RunnerConfig
	runnerID string
}

// NewRunner 建运行器。runnerID 自动生成（主机名 + pid + 随机串），写进 agent_session.runner_id。
func NewRunner(d RunnerDeps, cfg RunnerConfig) *Runner {
	if cfg.MaxCalls <= 0 {
		cfg.MaxCalls = 20
	}
	if cfg.ContextChars <= 0 {
		cfg.ContextChars = 60000
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Runner{d: d, cfg: cfg, runnerID: newRunnerID()}
}

// RunnerID 返回本实例的租约持有者标识。
func (r *Runner) RunnerID() string { return r.runnerID }

// Fork 复制出一个共用依赖、但租约持有者标识不同的运行器。每个消费者一份（04 文档 P-10）。
//
// ⚠️ 同一进程的几个消费者如果共用一个 runnerID，续租只比对 runner_id，就分不出是谁在跑：
// 数据库抖动超过租约时长时，同一轮会被两个消费者同时跑，双方都察觉不到；
// 一方结束时还会把另一方的租约释放掉。
func (r *Runner) Fork() *Runner {
	c := *r
	c.runnerID = newRunnerID()
	return &c
}

func newRunnerID() string {
	host, _ := os.Hostname()
	var b [4]byte
	_, _ = rand.Read(b[:])
	id := fmt.Sprintf("%s:%d:%s", host, os.Getpid(), hex.EncodeToString(b[:]))
	if len(id) > 64 { // agent_session.runner_id varchar(64)
		id = id[len(id)-64:]
	}
	return id
}

// HandleMessage 解析队列消息并跑一轮（消费者入口）。解不开的消息返回 error，由调用方转死信。
func (r *Runner) HandleMessage(ctx context.Context, body []byte) (Outcome, error) {
	var msg domain.TurnRequestedPayload
	if err := json.Unmarshal(body, &msg); err != nil {
		return OutcomeDone, fmt.Errorf("%w: %v", ErrBadMessage, err)
	}
	sid, err1 := strconv.ParseInt(msg.SessionID, 10, 64)
	tid, err2 := strconv.ParseInt(msg.TurnID, 10, 64)
	if err1 != nil || err2 != nil || sid <= 0 || tid <= 0 {
		return OutcomeDone, fmt.Errorf("%w: sessionId=%q turnId=%q", ErrBadMessage, msg.SessionID, msg.TurnID)
	}
	return r.RunTurn(ctx, sid, tid)
}

// ErrBadMessage 队列消息解不开（转死信）。
var ErrBadMessage = errors.New("agent 轮次消息格式非法")

// errInterrupted 本轮被 Worker 退出打断（只用于日志，ack 方式看 Outcome）。
var errInterrupted = errors.New("Worker 正在退出，本轮交给接手的 Worker 续跑")

// shuttingDown 报告本轮是否正被 Worker 退出打断（04 文档 P-03）。
//
// 运行上下文被取消只有两个来源：租约被抢走（keepLease 先置 leaseLost 再取消），
// 以及消费者的上下文被退出信号取消。「停止」只取消单次模型调用的子上下文，不走这里。
// 所以「上下文已取消且租约没丢」就是 Worker 在退出。
//
// ⚠️ 原先这种情况一律记 model_error，会话进 failed：每次发版都会把正在跑的会话打成失败。
// 反倒是进程被 kill -9 时，租约过期后扫描循环能用原轮次接手。
func (r *Runner) shuttingDown(ctx context.Context, run *turnRun) bool {
	if ctx.Err() == nil || run.leaseLost.Load() {
		return false
	}
	run.interrupted = true
	return true
}

// turnRun 是一轮运行中的可变状态。
type turnRun struct {
	sess    repo.Session
	turn    repo.Turn
	model   string
	level   int64
	charged int

	state     domain.State
	calls     int
	valid     int
	runErr    error
	errCode   string
	stopped   bool
	lostPerm  bool
	callLimit bool
	// interrupted：Worker 正在退出，本轮被中断（04 文档 P-03）。
	// 不结算、交还租约，由接手的 Worker 用原轮次续跑。
	interrupted bool
	suspend     *Suspension
	// images 营销会话的看图状态（MKT-T-19）：上限、首条消息的图、view_media 的临时附图。
	images sceneImageState
	// modelPrompt 「当前视频模型」与「创作偏好」两节（一轮内只算一次：要查目录里好几个模型）。
	modelPrompt *modelPromptSections
	// spec 视频规格的生效值（偏好 + 问答卡里确认过的回答；一轮只查一次）：写进「创作偏好」，也随 RunContext 交给工具。
	spec *VideoSpecState

	leaseLost atomic.Bool
}

// RunTurn 跑完一轮（dev/01 §4.2）。返回的 error 只用于日志，ack 方式看 Outcome。
func (r *Runner) RunTurn(ctx context.Context, sessionID, turnID int64) (Outcome, error) {
	st := r.d.Store
	turn, err := st.GetTurn(ctx, turnID)
	if errors.Is(err, repo.ErrNotFound) {
		return OutcomeDuplicate, nil
	}
	if err != nil {
		return OutcomeRequeue, err
	}
	if turn.SessionID != sessionID {
		return OutcomeDuplicate, fmt.Errorf("轮次 %d 不属于会话 %d", turnID, sessionID)
	}
	if turn.Status.IsTerminal() {
		return OutcomeDuplicate, nil
	}

	// ① 租约。0 行 = 别的 Worker 在跑这个会话。
	ok, err := st.ClaimLease(ctx, sessionID, r.runnerID, LeaseTTL)
	if err != nil {
		return OutcomeRequeue, err
	}
	if !ok {
		return OutcomeBusy, nil
	}
	run := &turnRun{}
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	stopRenew := r.keepLease(runCtx, cancelRun, sessionID, run)
	defer func() {
		stopRenew()
		if !run.leaseLost.Load() {
			sctx, cancel := settleContext(ctx)
			if _, err := st.ReleaseLease(sctx, sessionID, r.runnerID); err != nil {
				r.d.Logger.Warn("释放 agent 租约失败（90s 后自然过期）", "sessionId", sessionID, "error", err)
			}
			cancel()
		}
	}()

	// 取租约之后再读一遍：两次读之间别的 Worker 可能刚跑完它。
	if turn, err = st.GetTurn(runCtx, turnID); err != nil {
		return OutcomeRequeue, err
	}
	if turn.Status.IsTerminal() {
		return OutcomeDuplicate, nil
	}
	sess, err := st.GetSession(runCtx, sessionID)
	if err != nil {
		return OutcomeRequeue, err
	}
	run.sess, run.turn, run.state = sess, turn, sess.State
	if turn.Status == domain.TurnRunning {
		// 崩溃后接手：running 说明扣费已完成（MarkTurnRunning 在 Consume 之后）。
		run.charged, run.model = turn.CreditsCost, turn.ModelCode
	}
	// 会员等级每条路径都要取（04 文档 P-02）：接手 running 的轮会跳过 charge()，
	// 原先只在 charge 里取，于是接手后的付费工具一律按免费档报价、受理。
	if r.d.MemberLevels != nil {
		run.level = r.d.MemberLevels.MemberLevelID(runCtx, sess.PayerAccountID)
	}

	// 发起人仍是成员？不是 → 会话取消（AG-15）。
	role, member, err := r.d.Members.Role(runCtx, sess.WorkspaceID, sess.CreatedByUserID)
	if err != nil {
		return OutcomeRequeue, err
	}
	if !member {
		run.lostPerm = true
		return r.finish(ctx, run), nil
	}
	if !domain.IsRunning(run.state) || run.state == domain.StateWaitingTask {
		// 排队轮（AGT-25）：用户在上一轮运行中发的消息，PostMessage 只排队不迁移状态；
		// 它的消息到这里时上一轮已经结束、会话已离开运行态。不能就地作废（那是一条用户消息，
		// 作废＝他说的话没人回），也不在这里自己迁移（迁移规则只在 PostMessage 与扫描循环里各有
		// 一处，且都要锁会话行）—— 原样留着 pending，交给扫描循环按「直接发消息」重新路由并入队。
		if turn.Status == domain.TurnPending && run.state != domain.StateCancelled {
			return OutcomeBusy, nil
		}
		// API 侧建轮时已把会话迁到 planning / executing；走到这里说明会话被取消或数据不一致。
		run.errCode = ErrCodeSessionNotRunnable
		run.runErr = fmt.Errorf("会话状态 %s 不能运行一轮", run.state)
		return r.finish(ctx, run), nil
	}

	// ② 计费。
	if turn.Status == domain.TurnPending {
		if done := r.charge(runCtx, run); done {
			if r.shuttingDown(runCtx, run) {
				// 报价 / 扣费是被退出信号打断的，不是真的失败：不结算，原样放回队列。
				// 扣费若已成功，重投时账本按 refId 判重，不会扣第二次。
				return OutcomeRequeue, errInterrupted
			}
			return r.finish(ctx, run), nil
		}
	}

	// ③④ 上下文 + 循环。
	r.loop(runCtx, run, role)
	if run.leaseLost.Load() {
		return OutcomeLeaseLost, errors.New("运行中租约丢失，本轮交给新的持有者")
	}
	if run.interrupted {
		// 不结算：轮次保持 running，租约在 defer 里交还，接手的 Worker 走「崩溃后接手」续跑。
		return OutcomeRequeue, errInterrupted
	}
	// ⑤⑥ 结算、迁移状态、推送。
	return r.finish(ctx, run), run.runErr
}

// charge 报价并扣费（只对 pending 的轮）。返回 true 表示本轮到此为止（已记好失败原因）。
func (r *Runner) charge(ctx context.Context, run *turnRun) bool {
	st := r.d.Store
	model := resolveModel(run.turn, run.sess)
	if model == "" && r.d.Reasoning != nil {
		// 用户没选（如空状态的建议胶囊直接发出、会话偏好为空）：取平台第一个可用的推理模型。
		// 不兜底的话这一轮直接以 model_not_selected 失败，而用户并没有做错什么。
		if code, err := r.d.Reasoning.DefaultReasoningModel(ctx, run.level); err == nil {
			model = code
		} else {
			r.d.Logger.Warn("取默认推理模型失败", "sessionId", run.sess.ID, "error", err)
		}
	}
	if model == "" {
		run.errCode, run.runErr = ErrCodeNoModel, errors.New("没有选择推理模型")
		return true
	}
	run.model = model
	credits := 0
	if run.turn.Trigger.Billable() {
		q, err := r.d.Quoter.QuoteTurn(ctx, model, run.level)
		if err != nil {
			run.errCode, run.runErr = ErrCodeQuoteFailed, fmt.Errorf("推理模型报价失败: %w", err)
			return true
		}
		credits = q.Credits
	}
	// 先记报价再扣费：扣费成功而进程崩在下一步时，接手的 Worker 从 credits_cost 知道扣了多少。
	if _, err := st.SetTurnQuote(ctx, run.turn.ID, model, credits); err != nil {
		run.errCode, run.runErr = ErrCodeLedger, fmt.Errorf("记录报价失败: %w", err)
		return true
	}
	if credits > 0 {
		err := r.d.Ledger.Consume(ctx, run.sess.PayerAccountID, run.sess.CreatedByUserID, credits,
			RefTypeTurn, idStr(run.turn.ID), "Agent 对话")
		switch {
		case err == nil, errors.Is(err, ErrLedgerDuplicate):
			// 重复 = 这一轮之前已经扣过（消息重投 / 崩在 MarkRunning 之前），视为已扣。
		case errors.Is(err, ErrInsufficientCredits):
			run.errCode, run.runErr = ErrCodeInsufficientCredits, err
			return true
		default:
			run.errCode, run.runErr = ErrCodeLedger, fmt.Errorf("扣费失败: %w", err)
			return true
		}
	}
	run.charged = credits
	if _, err := st.MarkTurnRunning(ctx, run.turn.ID); err != nil {
		r.d.Logger.Warn("标记轮次 running 失败（继续运行，结算时以终态为准）",
			"turnId", run.turn.ID, "error", err)
	}
	return false
}

// resolveModel 本轮的推理模型：轮上记的 > 会话偏好 reasoning.model。
func resolveModel(t repo.Turn, s repo.Session) string {
	if t.ModelCode != "" {
		return t.ModelCode
	}
	var p struct {
		Reasoning struct {
			Model string `json:"model"`
		} `json:"reasoning"`
	}
	if len(s.Prefs) > 0 && json.Unmarshal(s.Prefs, &p) == nil {
		return p.Reasoning.Model
	}
	return ""
}

// finish 结算本轮：落终态（CAS）→ 退款（只在本次真的改了状态时）→ 迁移会话 → 推送。
//
// ⚠️ 用脱离取消的上下文，且在模型返回**之后**才建（同 script/app.settleContext）：
// Worker 收到退出信号时 ctx 已取消，拿它去退款的话钱扣了、轮次永远停在 running。
func (r *Runner) finish(parent context.Context, run *turnRun) Outcome {
	ctx, cancel := settleContext(parent)
	defer cancel()
	st := r.d.Store

	settleErr := run.runErr
	if settleErr == nil && run.stopped && run.valid == 0 {
		settleErr = errors.New("用户停止了本轮")
	}
	if settleErr == nil && run.lostPerm && run.valid == 0 {
		settleErr = errors.New("发起人已失去工作区权限")
	}
	dec := domain.DecideTurnSettle(run.valid, settleErr)
	status := dec.Status
	code := run.errCode
	switch {
	case run.lostPerm:
		code = ErrCodeLostPermission
		if run.valid == 0 {
			status = domain.TurnCancelled
		}
	case run.stopped:
		code = ErrCodeStopped
		if run.valid == 0 {
			status = domain.TurnCancelled
		}
	case code == ErrCodeSessionNotRunnable:
		// 会话已取消 / 不在运行态：这一轮根本没开始，记 cancelled 而不是 failed。
		status = domain.TurnCancelled
	case run.callLimit && code == "":
		code = ErrCodeCallLimit
	case status == domain.TurnFailed && code == "":
		if run.runErr == nil {
			code = ErrCodeEmptyReply
		} else {
			code = ErrCodeModel
		}
	}
	msg := dec.Reason
	if run.runErr != nil && msg == "" {
		msg = run.runErr.Error()
	}
	changed, err := st.FinishTurn(ctx, run.turn.ID, status, run.calls, run.valid, code, msg)
	if err != nil {
		r.d.Logger.Error("落轮次终态失败（扫描循环会兜底退款）", "turnId", run.turn.ID, "error", err)
	}
	// ⚠️ 只有「本次真的把轮次结束了」才退款：重复消息、扫描循环与 Runner 同时结算时
	// 只有一方生效；0 积分的轮不调用 Refund（账本找不到原流水会报错）。
	if changed && dec.CallRefund(run.charged) {
		if err := r.d.Ledger.Refund(ctx, run.sess.PayerAccountID, RefTypeTurn, idStr(run.turn.ID),
			dec.Reason); err != nil {
			r.d.Logger.Warn("Agent 轮次退款失败，需对账补偿", "turnId", run.turn.ID,
				"payerAccountId", run.sess.PayerAccountID, "credits", run.charged, "error", err)
		}
	}

	to := r.transition(ctx, run)
	reason := code
	ev := AgentStateEvent{Type: EventAgentState, SessionID: idStr(run.sess.ID), State: string(to),
		TurnID: idStr(run.turn.ID), Reason: reason}
	if run.suspend != nil && run.suspend.CardID != 0 {
		ev.PendingCardID = idStr(run.suspend.CardID)
	}
	// 额度以库里为准（挂起工具可能刚改过）。spent 由 API 侧按预留表算，这里不重复一份口径：
	// 事件只是对账信号，前端收到后拉会话详情。
	if s, err := st.GetSession(ctx, run.sess.ID); err == nil {
		ev.ApprovedBudget = s.ApprovedBudget
	}
	r.publish(ctx, run.sess.ProjectID, &ev)
	return OutcomeDone
}

// transition 按本轮结局迁移会话状态（CAS），返回迁移后的状态。
func (r *Runner) transition(ctx context.Context, run *turnRun) domain.State {
	from := run.state
	var ev domain.Event
	switch {
	case run.lostPerm:
		ev = domain.EvLostPermission
	case run.errCode == ErrCodeSessionNotRunnable:
		return from // 不是本轮造成的状态，不动它
	case run.runErr != nil && (run.errCode == ErrCodeModel || run.errCode == ""):
		ev = domain.EvFatal
		// 本轮已经提交的任务还在跑（04 文档 P-14）：先去等它们。任务结束后扫描循环会建一轮（不计费）
		// 把结果交给模型接着做；直接进 failed 的话，这些花了钱的任务结果没人接手。
		if from == domain.StateExecuting && r.hasWaiting(ctx, run.sess.ID) {
			ev = domain.EvWaitTask
		}
	case run.suspend != nil:
		e, err := suspendEvent(from, run.suspend)
		if err != nil {
			r.d.Logger.Warn("忽略不合法的挂起请求，按本轮结束处理", "sessionId", run.sess.ID, "error", err)
			ev = domain.EvTurnEnd
			run.suspend = nil
		} else {
			ev = e
		}
	default:
		// 正常结束、停止、调用上限、余额不足 / 报价失败（本轮没跑起来）都回到 idle；
		// 执行态里还有没终态的任务 → 去等它们。
		ev = domain.EvTurnEnd
		if from == domain.StateExecuting && r.hasWaiting(ctx, run.sess.ID) {
			ev = domain.EvWaitTask
		}
	}
	to, ok := domain.CanTransition(from, ev)
	if !ok {
		r.d.Logger.Warn("会话状态迁移不合法，保持原状态", "sessionId", run.sess.ID,
			"from", from, "event", ev)
		return from
	}
	changed, err := r.d.Store.TransitionState(ctx, run.sess.ID, from, to)
	if err != nil {
		r.d.Logger.Error("迁移会话状态失败", "sessionId", run.sess.ID, "from", from, "to", to, "error", err)
		return from
	}
	if !changed {
		// 期间被 API 改过（例如用户取消）：以库里为准。
		if s, err := r.d.Store.GetSession(ctx, run.sess.ID); err == nil {
			return s.State
		}
		return from
	}
	return to
}

func (r *Runner) hasWaiting(ctx context.Context, sessionID int64) bool {
	ok, err := r.d.Store.HasWaitingSteps(ctx, sessionID)
	if err != nil {
		r.d.Logger.Warn("查询等待中的步骤失败", "sessionId", sessionID, "error", err)
		return false
	}
	return ok
}

// keepLease 在后台每 30s 续一次租约；续不上（被抢走）就取消本轮运行。
func (r *Runner) keepLease(ctx context.Context, cancel context.CancelFunc, sessionID int64,
	run *turnRun) (stop func()) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(leaseRenewEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if !r.renew(ctx, sessionID, run) {
					cancel()
					return
				}
			}
		}
	}()
	return func() { close(done) }
}

// renew 续租一次。false = 租约已被别人拿走（调用方必须停手）。数据库错误不算丢失。
func (r *Runner) renew(ctx context.Context, sessionID int64, run *turnRun) bool {
	ok, err := r.d.Store.RenewLease(ctx, sessionID, r.runnerID, LeaseTTL)
	if err != nil {
		r.d.Logger.Warn("续 agent 租约失败", "sessionId", sessionID, "error", err)
		return true
	}
	if !ok {
		run.leaseLost.Store(true)
		r.d.Logger.Warn("agent 租约已被别的 Worker 取走，停止本轮", "sessionId", sessionID)
	}
	return ok
}

func (r *Runner) publish(ctx context.Context, projectID int64, ev any) {
	if r.d.Events == nil {
		return
	}
	switch e := ev.(type) {
	case *AgentStateEvent:
		e.OccurredAt = nowStr(r.d.Now())
	case *AgentDeltaEvent:
		e.OccurredAt = nowStr(r.d.Now())
	case *AgentStepEvent:
		e.OccurredAt = nowStr(r.d.Now())
	}
	if err := r.d.Events.Publish(ctx, ProjectTopic(projectID), ev); err != nil {
		r.d.Logger.Debug("agent 事件发布失败（只是对账信号）", "error", err)
	}
}

// settleContext 给「模型已经返回之后」的结账用：脱离调用方的取消，自带 30s 时限。
// ⚠️ 必须在模型返回**之后**再建，建早了它的计时会在等模型的那几分钟里先到期。
func settleContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
}
