package domain

import (
	"errors"
	"testing"
)

type row struct {
	from State
	ev   Event
	to   State
	why  string
}

// expectedRows 是迁移表的**期望副本**：dev/01 §4.1 每一行 + AGT-20 补齐的挂起时发消息规则。
// 表与这里任一侧改了另一侧没改，TestCanTransition_迁移表逐行 都会红。
var expectedRows = []row{
	// dev/01 §4.1
	{StateIdle, EvUserMessage, StatePlanning, "用户发消息（未批准过规划）"},
	{StateIdle, EvUserMessageApproved, StateExecuting, "用户发消息（已批准且在额度内）"},
	{StatePlanning, EvAskUser, StateAwaitingInput, "模型调用 ask_user"},
	{StatePlanning, EvSubmitPlan, StateAwaitingApproval, "模型调用 submit_plan"},
	{StateAwaitingInput, EvInputResumePlanning, StatePlanning, "提交卡片回到发卡前（planning）"},
	{StateAwaitingInput, EvInputResumeExecuting, StateExecuting, "提交卡片回到发卡前（executing）"},
	{StateAwaitingApproval, EvApprove, StateExecuting, "用户批准"},
	{StateAwaitingApproval, EvAdjust, StatePlanning, "需要调整"},
	{StateExecuting, EvOverBudget, StateAwaitingApproval, "付费工具超额度"},
	{StateExecuting, EvWaitTask, StateWaitingTask, "等待任务"},
	{StateWaitingTask, EvTasksDone, StateExecuting, "所等任务全部终态"},
	{StatePlanning, EvTurnEnd, StateIdle, "运行态结束本轮"},
	{StateExecuting, EvTurnEnd, StateIdle, "运行态结束本轮"},
	{StatePlanning, EvFatal, StateFailed, "运行态不可恢复错误"},
	{StateExecuting, EvFatal, StateFailed, "运行态不可恢复错误"},
	{StateWaitingTask, EvFatal, StateFailed, "运行态不可恢复错误"},
	{StateFailed, EvRetryToIdle, StateIdle, "重试回到上一个稳定状态"},
	{StateFailed, EvRetryToPlanning, StatePlanning, "重试回到上一个稳定状态"},
	{StateFailed, EvRetryToExecuting, StateExecuting, "重试回到上一个稳定状态"},
	// 补齐：executing 也能 ask_user（工具表 ask_user 允许 planning、executing）
	{StateExecuting, EvAskUser, StateAwaitingInput, "执行中提问"},
	// 补齐：挂起时发消息（design/03 §2.3）
	{StateAwaitingApproval, EvMessageOnPlanCard, StatePlanning, "规划确认卡未处理时发消息"},
	{StateAwaitingApproval, EvMessageOnBudgetCard, StateExecuting, "追加额度卡未处理时发消息"},
	{StateWaitingTask, EvUserMessage, StateExecuting, "等任务时追问"},
	{StateAwaitingApproval, EvReject, StateIdle, "拒绝"},
	{StateFailed, EvUserMessage, StatePlanning, "失败后直接发消息"},
	{StateFailed, EvUserMessageApproved, StateExecuting, "失败后直接发消息（已批准）"},
	// AGT-25：卡片过期，会话回 idle（不扣费、不取消已完成的生成）
	{StateAwaitingInput, EvCardExpired, StateIdle, "卡片过期"},
	{StateAwaitingApproval, EvCardExpired, StateIdle, "卡片过期"},
}

func init() {
	// 「任意」→ cancelled：取消与失去权限对每个非终态都成立。
	for _, s := range AllStates {
		if s == StateCancelled {
			continue
		}
		expectedRows = append(expectedRows,
			row{s, EvCancel, StateCancelled, "用户取消"},
			row{s, EvLostPermission, StateCancelled, "发起人失去权限"})
	}
}

func TestCanTransition_迁移表逐行(t *testing.T) {
	for _, r := range expectedRows {
		got, ok := CanTransition(r.from, r.ev)
		if !ok || got != r.to {
			t.Errorf("%s：%s + %s 期望 → %s，得到 %s ok=%v", r.why, r.from, r.ev, r.to, got, ok)
		}
	}
	// 反向：表里不能多出期望之外的行。
	n := 0
	for _, evs := range transitions {
		n += len(evs)
	}
	if n != len(expectedRows) {
		t.Errorf("迁移表有 %d 行，期望 %d 行 —— 表与测试副本不一致", n, len(expectedRows))
	}
}

func TestCanTransition_表外组合一律拒绝(t *testing.T) {
	want := map[[2]string]State{}
	for _, r := range expectedRows {
		want[[2]string{string(r.from), string(r.ev)}] = r.to
	}
	for _, s := range AllStates {
		for _, ev := range AllEvents {
			_, ok := CanTransition(s, ev)
			_, expected := want[[2]string{string(s), string(ev)}]
			if ok != expected {
				t.Errorf("%s + %s：ok=%v，期望 %v", s, ev, ok, expected)
			}
		}
	}
	if _, ok := CanTransition("bogus", EvCancel); ok {
		t.Error("未知状态不应迁移")
	}
	if _, ok := CanTransition(StateIdle, "bogus"); ok {
		t.Error("未知事件不应迁移")
	}
}

func TestCanTransition_已取消不可迁出(t *testing.T) {
	for _, ev := range AllEvents {
		if to, ok := CanTransition(StateCancelled, ev); ok {
			t.Errorf("cancelled + %s 不应迁出，得到 %s", ev, to)
		}
	}
	if !IsTerminal(StateCancelled) || IsTerminal(StateFailed) {
		t.Error("只有 cancelled 是终态")
	}
	if _, err := MessageEvent(StateCancelled, MessageContext{}); !errors.Is(err, ErrSessionCancelled) {
		t.Errorf("已取消会话发消息应报 ErrSessionCancelled，得到 %v", err)
	}
}

// 规划确认卡场景：awaiting_approval 只有 approve 能进 executing。
// 遍历「发消息」与「提交卡片」两条入口的全部可能输入，断言都到不了 executing。
func TestCanTransition_规划确认卡未批准不进执行(t *testing.T) {
	// 1. 迁移表：awaiting_approval 进 executing 的只有 approve 与追加额度卡专用事件。
	for _, ev := range AllEvents {
		to, ok := CanTransition(StateAwaitingApproval, ev)
		if ok && to == StateExecuting && ev != EvApprove && ev != EvMessageOnBudgetCard {
			t.Errorf("事件 %s 让 awaiting_approval 进入了 executing", ev)
		}
	}
	// 2. 发消息：规划确认卡（以及卡片类型缺失的不一致数据）一律回规划并作废卡片。
	for _, mc := range []MessageContext{
		{PendingCard: CardPlanConfirm},
		{PendingCard: CardPlanConfirm, PlanApproved: true},
		{PendingCard: ""},
	} {
		r, err := MessageEvent(StateAwaitingApproval, mc)
		if err != nil {
			t.Fatal(err)
		}
		if r.To != StatePlanning || r.Event == EvMessageOnBudgetCard {
			t.Errorf("%+v：期望回 planning，得到 %+v", mc, r)
		}
		if mc.PendingCard == CardPlanConfirm && !r.VoidCard {
			t.Error("规划确认卡被跳过时应作废")
		}
	}
	// 3. 提交卡片：只有 approve 进 executing。
	card := Card{Kind: CardPlanConfirm, Payload: []byte(`{"title":"t","items":[],"total":0,"balance":0}`)}
	for _, body := range []string{`{"decision":"adjust","comment":"短一点"}`, `{"decision":"reject"}`} {
		ev, err := SubmissionEvent(card, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if to, _ := CanTransition(StateAwaitingApproval, ev); to == StateExecuting {
			t.Errorf("%s 不应进入 executing", body)
		}
	}
	ev, err := SubmissionEvent(card, []byte(`{"decision":"approve"}`))
	if err != nil || ev != EvApprove {
		t.Errorf("approve 应得到 EvApprove，得到 %s %v", ev, err)
	}
}

func TestMessageEvent_挂起时发消息(t *testing.T) {
	cases := []struct {
		name string
		s    State
		mc   MessageContext
		want MessageRoute
	}{
		{"idle 未批准进规划", StateIdle, MessageContext{},
			MessageRoute{Transition: true, Event: EvUserMessage, To: StatePlanning}},
		{"idle 已批准直接执行", StateIdle, MessageContext{PlanApproved: true},
			MessageRoute{Transition: true, Event: EvUserMessageApproved, To: StateExecuting}},
		{"运行中排队不迁移", StateExecuting, MessageContext{}, MessageRoute{}},
		{"规划中排队不迁移", StatePlanning, MessageContext{}, MessageRoute{}},
		{"选择卡挂起回发卡前状态且卡片保持", StateAwaitingInput,
			MessageContext{PendingCard: CardChoice, ResumeState: StateExecuting},
			MessageRoute{Transition: true, Event: EvInputResumeExecuting, To: StateExecuting}},
		{"追加额度卡挂起回执行", StateAwaitingApproval, MessageContext{PendingCard: CardBudget},
			MessageRoute{Transition: true, Event: EvMessageOnBudgetCard, To: StateExecuting}},
		{"等任务时追问", StateWaitingTask, MessageContext{},
			MessageRoute{Transition: true, Event: EvUserMessage, To: StateExecuting}},
		{"失败后发消息", StateFailed, MessageContext{},
			MessageRoute{Transition: true, Event: EvUserMessage, To: StatePlanning}},
	}
	for _, c := range cases {
		got, err := MessageEvent(c.s, c.mc)
		if err != nil {
			t.Errorf("%s：%v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s：得到 %+v，期望 %+v", c.name, got, c.want)
		}
	}
	if _, err := MessageEvent(StateAwaitingInput, MessageContext{ResumeState: StateIdle}); !errors.Is(err, ErrBadResumeState) {
		t.Errorf("恢复状态非法应报错，得到 %v", err)
	}
}

func TestRetryEvent_回到上一个稳定状态(t *testing.T) {
	for _, prev := range []State{StateIdle, StatePlanning, StateExecuting} {
		ev, ok := RetryEvent(prev)
		if !ok {
			t.Fatalf("%s 应可重试", prev)
		}
		if to, ok := CanTransition(StateFailed, ev); !ok || to != prev {
			t.Errorf("重试到 %s 得到 %s ok=%v", prev, to, ok)
		}
	}
	for _, prev := range []State{StateAwaitingApproval, StateWaitingTask, StateCancelled, StateFailed} {
		if _, ok := RetryEvent(prev); ok {
			t.Errorf("%s 不是稳定状态，不应可重试", prev)
		}
	}
}

func TestSuspendStateFor_卡片对应挂起态(t *testing.T) {
	cases := map[CardKind]State{
		CardChoice: StateAwaitingInput, CardQuestions: StateAwaitingInput,
		CardDeleteConfirm: StateAwaitingInput,
		CardPlanConfirm:   StateAwaitingApproval, CardBudget: StateAwaitingApproval,
	}
	for k, want := range cases {
		if got, ok := SuspendStateFor(k); !ok || got != want {
			t.Errorf("%s → %s，期望 %s", k, got, want)
		}
	}
	if _, ok := SuspendStateFor("bogus"); ok {
		t.Error("未知卡片类型不应有挂起态")
	}
}

func TestIsRunning_运行态(t *testing.T) {
	for _, s := range AllStates {
		want := s == StatePlanning || s == StateExecuting || s == StateWaitingTask
		if IsRunning(s) != want {
			t.Errorf("IsRunning(%s) = %v", s, !want)
		}
		if !s.Valid() {
			t.Errorf("%s 应合法", s)
		}
	}
}

func TestParseScene_缺省general且只认两个值(t *testing.T) {
	cases := []struct {
		in   string
		want Scene
		ok   bool
	}{
		{"", SceneGeneral, true},
		{"general", SceneGeneral, true},
		{"marketing", SceneMarketing, true},
		{"Marketing", "", false},
		{"mcp", "", false},
	}
	for _, c := range cases {
		got, ok := ParseScene(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("ParseScene(%q) = %q,%v，期望 %q,%v", c.in, got, ok, c.want, c.ok)
		}
	}
	// 与 ck_agent_session_scene 逐字一致（迁移 00061）。
	if len(Scenes) != 2 || Scenes[0] != "general" || Scenes[1] != "marketing" {
		t.Fatalf("场景清单与 CHECK 约束不一致：%v", Scenes)
	}
}
