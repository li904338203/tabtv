// Package domain 是画布 Agent 的全部「规则」：会话状态迁移、按状态下发哪些工具、
// 额度够不够、一轮该不该退款、卡片提交是否合法。
//
// ⚠️ 这里**只有纯函数**：不碰库、不发网络、不取当前时间，也**不 import 任何其他域**
// （权限动作用字符串表达，与 collaboration/domain.Action 的取值逐字一致，由 app 层转换）。
// 理由同 workflow/domain：NEW-01「校验失败不调用模型或扣分」、AG-05「规划态不下发付费工具」
// 唯一能保证的做法就是让判定发生在任何副作用之前，并且每条规则都能被表驱动测试钉住。
// 后续 app / tools / scanner 只调用这里的函数，不各自写判断。
package domain

import (
	"errors"
	"fmt"
)

// State 是会话状态（与 agent_session.state 的 CHECK 约束逐字一致）。
type State string

const (
	// StateIdle 本轮结束、等用户再说话。会话没有终点，`done` 不单列。
	StateIdle State = "idle"
	// StatePlanning 规划中：只有只读 / 问答 / 文档 / 提交规划工具。
	StatePlanning State = "planning"
	// StateAwaitingInput 挂起：等用户回答选择卡 / 问答卡 / 删除确认卡。
	StateAwaitingInput State = "awaiting_input"
	// StateAwaitingApproval 挂起：等用户批准规划或追加额度。
	StateAwaitingApproval State = "awaiting_approval"
	// StateExecuting 执行中：付费与画布写工具只在这里下发。
	StateExecuting State = "executing"
	// StateWaitingTask 挂起：等已提交的生成任务进入终态。
	StateWaitingTask State = "waiting_task"
	// StateFailed 不可恢复错误；可「重试这一步」回到上一个稳定状态。
	StateFailed State = "failed"
	// StateCancelled 终态：任何事件都不迁出。
	StateCancelled State = "cancelled"
)

// AllStates 按声明顺序列出全部状态（测试遍历「表外组合」用）。
var AllStates = []State{
	StateIdle, StatePlanning, StateAwaitingInput, StateAwaitingApproval,
	StateExecuting, StateWaitingTask, StateFailed, StateCancelled,
}

// Valid 报告状态值是否合法。
func (s State) Valid() bool {
	for _, x := range AllStates {
		if x == s {
			return true
		}
	}
	return false
}

// IsRunning 报告会话是否处于「运行态」：还没有挂起去等用户。
//
// planning / executing 正在调用模型；waiting_task 在等任务，由扫描循环推进 ——
// 三者都需要 Worker 关注（与 idx_agent_session_scan 的部分索引条件一致）。
func IsRunning(s State) bool {
	return s == StatePlanning || s == StateExecuting || s == StateWaitingTask
}

// HasActiveTurn 报告该状态下是否有一轮正在（或马上要）跑：只有 planning / executing。
// waiting_task 在 IsRunning 里算「运行中」，但那时没有轮在跑，只是在等任务。
func HasActiveTurn(s State) bool {
	return s == StatePlanning || s == StateExecuting
}

// IsTerminal 报告是否终态。只有 cancelled 是终态：failed 还能「重试这一步」。
func IsTerminal(s State) bool { return s == StateCancelled }

// IsSuspendedForUser 报告是否在等用户处理卡片。
func IsSuspendedForUser(s State) bool {
	return s == StateAwaitingInput || s == StateAwaitingApproval
}

// Scene 是会话场景（与 agent_session.scene 的 CHECK 约束逐字一致，MKD-2）。
//
// ⚠️ 场景落在会话上、建后不可改：它决定「这段对话怎么解释」（系统提示词、可选技能、拼哪些片段），
// 改了历史消息的解释会变。没有任何更新路径写它。
type Scene string

const (
	// SceneGeneral 通用会话（创造页、画布面板、MCP）。缺省值：已有会话迁移后都是它。
	SceneGeneral Scene = "general"
	// SceneMarketing 营销会话：只能从营销入口连同新项目一起建。
	SceneMarketing Scene = "marketing"
)

// Scenes 按声明顺序列出全部场景。
var Scenes = []Scene{SceneGeneral, SceneMarketing}

// ValidScene 判断场景取值是否合法。
func ValidScene(s Scene) bool {
	for _, x := range Scenes {
		if x == s {
			return true
		}
	}
	return false
}

// ParseScene 解析请求里的场景：空串 = general（通用会话的请求体不带这个字段，行为逐字不变）。
func ParseScene(s string) (Scene, bool) {
	if s == "" {
		return SceneGeneral, true
	}
	sc := Scene(s)
	return sc, ValidScene(sc)
}

// Event 是驱动状态迁移的事件。
//
// ⚠️ 「回到发卡前的状态」「回到上一个稳定状态」这类**依赖上下文**的迁移，
// 拆成目标明确的多个事件（如 EvInputResumePlanning / EvInputResumeExecuting），
// 由 MessageEvent / SubmissionEvent / RetryEvent 按上下文选出 ——
// 这样迁移表本身仍是一张不带参数的静态表，「表外组合一律拒绝」可以被穷举测试。
type Event string

const (
	// EvUserMessage 用户发消息（未批准过规划，或在 waiting_task 期间追问）。
	EvUserMessage Event = "user_message"
	// EvUserMessageApproved 用户发消息，且已批准规划、额度有余 → 直接执行。
	EvUserMessageApproved Event = "user_message_approved"
	// EvAskUser 模型调用 ask_user（发选择卡 / 问答卡 / 删除确认卡）。
	EvAskUser Event = "ask_user"
	// EvSubmitPlan 模型调用 submit_plan（发规划确认卡）。
	EvSubmitPlan Event = "submit_plan"
	// EvInputResumePlanning 选择卡被提交、或用户直接发消息，回到发卡前的 planning。
	EvInputResumePlanning Event = "input_resume_planning"
	// EvInputResumeExecuting 同上，发卡前是 executing。
	EvInputResumeExecuting Event = "input_resume_executing"
	// EvApprove 用户批准（规划确认卡：写入额度；追加额度卡：追加额度）。
	EvApprove Event = "approve"
	// EvAdjust 用户在规划确认卡上选「需要调整」：旧规划作废，回到规划。
	EvAdjust Event = "adjust"
	// EvReject 用户拒绝（规划确认卡「不做了」/ 追加额度卡「不用了」）：本轮结束。
	EvReject Event = "reject"
	// EvMessageOnPlanCard 规划确认卡未处理时用户直接发消息：回到规划，**不进入执行**。
	EvMessageOnPlanCard Event = "message_on_plan_card"
	// EvMessageOnBudgetCard 追加额度卡未处理时用户直接发消息：回到执行，额度检查照常生效。
	EvMessageOnBudgetCard Event = "message_on_budget_card"
	// EvOverBudget 付费工具超出额度：发追加额度卡。
	EvOverBudget Event = "over_budget"
	// EvWaitTask 已提交任务、本轮要等它们（含「本轮结束时仍有未终态任务」）。
	EvWaitTask Event = "wait_task"
	// EvTasksDone 所等任务全部终态（扫描循环或即时钩子）。
	EvTasksDone Event = "tasks_done"
	// EvTurnEnd 模型结束本轮且无待办。
	EvTurnEnd Event = "turn_end"
	// EvCancel 用户取消会话。
	EvCancel Event = "cancel"
	// EvLostPermission 发起人失去工作区权限（被移出 / 降为只读）。
	EvLostPermission Event = "lost_permission"
	// EvFatal 不可恢复错误。
	EvFatal Event = "fatal"
	// EvRetryToIdle / EvRetryToPlanning / EvRetryToExecuting：failed 上「重试这一步」，
	// 回到调用方给出的上一个稳定状态。
	EvRetryToIdle      Event = "retry_to_idle"
	EvRetryToPlanning  Event = "retry_to_planning"
	EvRetryToExecuting Event = "retry_to_executing"
	// EvCardExpired 待处理的卡片超过 SEEAITV_AGENT_CARD_EXPIRE 无人处理（扫描循环，AGT-25）：
	// 会话回 idle，**不扣费、不取消已完成的生成**。用户之后再发消息就是从 idle 开始的一轮。
	EvCardExpired Event = "card_expired"
)

// AllEvents 按声明顺序列出全部事件。
var AllEvents = []Event{
	EvUserMessage, EvUserMessageApproved, EvAskUser, EvSubmitPlan,
	EvInputResumePlanning, EvInputResumeExecuting, EvApprove, EvAdjust, EvReject,
	EvMessageOnPlanCard, EvMessageOnBudgetCard, EvOverBudget, EvWaitTask, EvTasksDone,
	EvTurnEnd, EvCancel, EvLostPermission, EvFatal,
	EvRetryToIdle, EvRetryToPlanning, EvRetryToExecuting,
	EvCardExpired,
}

// transitions 是迁移表的**唯一定义**（dev/01 §4.1 + AGT-20 补齐的挂起时发消息规则）。
//
// ⚠️ 用 map 不用 switch：测试要能遍历「状态 × 事件」的全部组合，断言表外一律 false。
// ⚠️ 改规则只改这张表与它的测试；不允许在 runner 里加特例。
var transitions = map[State]map[Event]State{
	StateIdle: {
		EvUserMessage:         StatePlanning,
		EvUserMessageApproved: StateExecuting,
		EvCancel:              StateCancelled,
		EvLostPermission:      StateCancelled,
	},
	StatePlanning: {
		EvAskUser:        StateAwaitingInput,
		EvSubmitPlan:     StateAwaitingApproval,
		EvTurnEnd:        StateIdle,
		EvCancel:         StateCancelled,
		EvLostPermission: StateCancelled,
		EvFatal:          StateFailed,
	},
	StateAwaitingInput: {
		EvInputResumePlanning:  StatePlanning,
		EvInputResumeExecuting: StateExecuting,
		EvCardExpired:          StateIdle,
		EvCancel:               StateCancelled,
		EvLostPermission:       StateCancelled,
	},
	StateAwaitingApproval: {
		// ⚠️ 只有两条路进 executing：EvApprove（真批准）与 EvMessageOnBudgetCard
		// （追加额度卡场景，额度检查仍会拦住付费工具）。规划确认卡场景下
		// MessageEvent / SubmissionEvent 绝不会选出后者 —— 未批准不得执行（AG-05）。
		EvApprove:             StateExecuting,
		EvAdjust:              StatePlanning,
		EvReject:              StateIdle,
		EvMessageOnPlanCard:   StatePlanning,
		EvMessageOnBudgetCard: StateExecuting,
		EvCardExpired:         StateIdle,
		EvCancel:              StateCancelled,
		EvLostPermission:      StateCancelled,
	},
	StateExecuting: {
		EvAskUser:        StateAwaitingInput,
		EvOverBudget:     StateAwaitingApproval,
		EvWaitTask:       StateWaitingTask,
		EvTurnEnd:        StateIdle,
		EvCancel:         StateCancelled,
		EvLostPermission: StateCancelled,
		EvFatal:          StateFailed,
	},
	StateWaitingTask: {
		EvTasksDone: StateExecuting,
		// 等任务期间用户追问：跑一个 executing 轮，所等任务不受影响；
		// 本轮结束时若仍有未终态任务，由调用方发 EvWaitTask 回到 waiting_task。
		EvUserMessage:    StateExecuting,
		EvCancel:         StateCancelled,
		EvLostPermission: StateCancelled,
		EvFatal:          StateFailed,
	},
	StateFailed: {
		EvRetryToIdle:      StateIdle,
		EvRetryToPlanning:  StatePlanning,
		EvRetryToExecuting: StateExecuting,
		// 补充规则：失败后用户直接发消息等同于从 idle 开始一轮，否则只能点「重试」才走得动。
		EvUserMessage:         StatePlanning,
		EvUserMessageApproved: StateExecuting,
		EvCancel:              StateCancelled,
		EvLostPermission:      StateCancelled,
	},
	// StateCancelled：终态，没有任何出边。
}

// CanTransition 查迁移表。ok=false 表示该 (状态, 事件) 不允许。
func CanTransition(from State, ev Event) (State, bool) {
	to, ok := transitions[from][ev]
	return to, ok
}

// ErrSessionCancelled 会话已取消，不再接受任何动作。
var ErrSessionCancelled = errors.New("会话已取消")

// ErrBadResumeState 卡片里记的「发卡前状态」不是 planning / executing。
var ErrBadResumeState = errors.New("卡片记录的恢复状态非法")

// MessageContext 是「用户发消息」时选事件所需的上下文。
type MessageContext struct {
	// PlanApproved：已批准规划且额度有余（idle / failed 时决定进规划还是直接执行）。
	PlanApproved bool
	// PendingCard：awaiting_* 时待处理卡片的类型。
	PendingCard CardKind
	// ResumeState：awaiting_input 时卡片 payload 里记的发卡前状态。
	ResumeState State
}

// MessageRoute 是用户发消息后的去向。
type MessageRoute struct {
	// Transition=false：会话正在运行（planning / executing），消息排队为下一轮，不迁移状态。
	Transition bool
	Event      Event
	To         State
	// VoidCard：原卡片作废（规划确认卡被跳过时，旧规划不能再拿去执行）。
	// awaiting_input 的卡片保持 pending —— 用户补一句话不等于回答了它。
	VoidCard bool
}

// MessageEvent 给出「用户直接发消息（不点卡片）」时的迁移（dev/01 §9）。
//
//   - 规划确认卡未处理 → planning，原卡片作废，**不会直接进入执行**；
//   - 追加额度卡未处理 → executing，额度检查照常（超额会再次发卡）；
//   - 选择 / 问答 / 删除确认卡未处理 → 回到发卡前状态，卡片保持 pending；
//   - waiting_task → executing（追问进度），所等任务不受影响。
func MessageEvent(s State, mc MessageContext) (MessageRoute, error) {
	var ev Event
	route := MessageRoute{Transition: true}
	switch s {
	case StateCancelled:
		return MessageRoute{}, ErrSessionCancelled
	case StatePlanning, StateExecuting:
		return MessageRoute{Transition: false}, nil
	case StateIdle, StateFailed:
		ev = EvUserMessage
		if mc.PlanApproved {
			ev = EvUserMessageApproved
		}
	case StateWaitingTask:
		ev = EvUserMessage
	case StateAwaitingInput:
		e, err := InputResumeEvent(mc.ResumeState)
		if err != nil {
			return MessageRoute{}, err
		}
		ev = e
	case StateAwaitingApproval:
		switch mc.PendingCard {
		case CardPlanConfirm:
			ev, route.VoidCard = EvMessageOnPlanCard, true
		case CardBudget:
			ev = EvMessageOnBudgetCard
		default:
			// 数据不一致（awaiting_approval 却没有待处理的确认类卡）：
			// 保守地回规划 —— 宁可多问一次，也不能没批准就执行。
			ev = EvMessageOnPlanCard
		}
	default:
		return MessageRoute{}, fmt.Errorf("未知会话状态 %q", s)
	}
	to, ok := CanTransition(s, ev)
	if !ok {
		return MessageRoute{}, fmt.Errorf("状态 %s 不接受事件 %s", s, ev)
	}
	route.Event, route.To = ev, to
	return route, nil
}

// InputResumeEvent 把「发卡前状态」换成对应的恢复事件。
func InputResumeEvent(resume State) (Event, error) {
	switch resume {
	case StatePlanning:
		return EvInputResumePlanning, nil
	case StateExecuting:
		return EvInputResumeExecuting, nil
	}
	return "", fmt.Errorf("%w: %q", ErrBadResumeState, resume)
}

// RetryEvent 把调用方给出的「上一个稳定状态」换成 failed 上的重试事件。
func RetryEvent(prev State) (Event, bool) {
	switch prev {
	case StateIdle:
		return EvRetryToIdle, true
	case StatePlanning:
		return EvRetryToPlanning, true
	case StateExecuting:
		return EvRetryToExecuting, true
	}
	return "", false
}

// SuspendStateFor 给出发某类卡片后会话进入的挂起状态。
func SuspendStateFor(k CardKind) (State, bool) {
	switch k {
	case CardChoice, CardQuestions, CardDeleteConfirm:
		return StateAwaitingInput, true
	case CardPlanConfirm, CardBudget:
		return StateAwaitingApproval, true
	}
	return "", false
}
