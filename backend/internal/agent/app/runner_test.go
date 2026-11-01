package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
)

// ⚠️ 断言的是**请求入参**（假 ChatModel 收到的 Tools），不是代码分支（AG-05）。
func TestRunTurn_规划态请求体不含付费与画布写工具(t *testing.T) {
	h := newHarness(domain.StatePlanning, domain.TriggerUser, 2, 100)
	// 让模型先调两次只读工具，再给文本收尾 —— 一轮里有三次请求，每一次都要查。
	h.chat.replies = []fakeReply{
		{out: ChatOutput{ToolCalls: []ToolCall{toolCall("c1", "read_canvas", `{}`)}}},
		{out: ChatOutput{ToolCalls: []ToolCall{toolCall("c2", "read_doc", `{"path":"/plan.md"}`)}}},
		{out: ChatOutput{Text: "我先了解一下需求。"}},
	}
	if _, err := h.run(); err != nil {
		t.Fatal(err)
	}
	calls := h.chat.calls()
	if len(calls) != 3 {
		t.Fatalf("应调用模型 3 次，实际 %d", len(calls))
	}
	forbidden := map[string]bool{"generate": true, "canvas_apply": true, "cancel_task": true}
	for i, in := range calls {
		if len(in.Tools) == 0 {
			t.Fatalf("第 %d 次请求一个工具都没有", i+1)
		}
		for _, td := range in.Tools {
			if forbidden[td.Name] {
				t.Errorf("第 %d 次请求下发了 %s", i+1, td.Name)
			}
			if s, _ := domain.SpecOf(domain.ToolName(td.Name)); s.Paid || s.CanvasWrite {
				t.Errorf("第 %d 次请求下发了付费 / 画布写工具 %s", i+1, td.Name)
			}
		}
		if !strings.Contains(in.System, domain.PlanningModeNote) {
			t.Errorf("第 %d 次请求的系统提示词缺少规划阶段说明", i+1)
		}
	}
}

func TestRunTurn_无工具调用一次即结束会话回idle(t *testing.T) {
	h := newHarness(domain.StatePlanning, domain.TriggerUser, 2, 100)
	h.chat.replies = []fakeReply{{deltas: []string{"你好，", "我在。"}, out: ChatOutput{Text: "你好，我在。"}}}
	out, err := h.run()
	if err != nil || out != OutcomeDone {
		t.Fatalf("outcome=%v err=%v", out, err)
	}
	if n := len(h.chat.calls()); n != 1 {
		t.Fatalf("应只调用一次模型，实际 %d", n)
	}
	if s := h.session(); s.State != domain.StateIdle || s.RunnerLeaseUntil != nil {
		t.Errorf("会话应回 idle 且释放租约：%+v", s)
	}
	tr := h.turn()
	if tr.Status != domain.TurnSuccess || tr.CallCount != 1 || tr.ValidOutputs != 1 {
		t.Errorf("轮次结算不对：%+v", tr)
	}
	if h.ledger.consumed["agent_turn:200"] != 2 || len(h.ledger.refunded) != 0 {
		t.Errorf("应扣 2 积分且不退：%+v %+v", h.ledger.consumed, h.ledger.refunded)
	}
	if as := h.store.messagesByRole(repo.RoleAssistant); len(as) != 1 || as[0].Content != "你好，我在。" {
		t.Errorf("助手消息不对：%+v", as)
	}
	// 最后一条事件是 agent.state=idle。
	last := h.events.events[len(h.events.events)-1].(*AgentStateEvent)
	if last.State != string(domain.StateIdle) || last.TurnID != "200" {
		t.Errorf("最后一条事件不对：%+v", last)
	}
}

func TestRunTurn_未下发工具名被拒并回给模型(t *testing.T) {
	h := newHarness(domain.StatePlanning, domain.TriggerUser, 2, 100)
	gen := h.tools[domain.ToolGenerate].(*fakeTool)
	h.chat.replies = []fakeReply{
		// 规划态幻觉出 generate（本次没下发）。
		{out: ChatOutput{ToolCalls: []ToolCall{toolCall("c1", "generate", `{"prompt":"猫"}`)}}},
		{out: ChatOutput{Text: "抱歉，我先提交规划。"}},
	}
	if _, err := h.run(); err != nil {
		t.Fatal(err)
	}
	if gen.runs != 0 {
		t.Fatalf("未下发的工具不得执行")
	}
	failed := h.store.stepsByStatus(repo.StepFailed)
	if len(failed) != 1 || failed[0].Tool != "generate" {
		t.Fatalf("应有一行 failed 的步骤：%+v", failed)
	}
	tools := h.store.messagesByRole(repo.RoleTool)
	if len(tools) != 1 || tools[0].ToolCallID != "c1" || !strings.Contains(tools[0].Content, "不可用") {
		t.Fatalf("应回给模型一条错误结果：%+v", tools)
	}
	// 第二次请求里带着那条错误结果。
	second := h.chat.calls()[1]
	found := false
	for _, m := range second.Messages {
		if m.Role == "tool" && m.ToolCallID == "c1" {
			found = true
		}
	}
	if !found {
		t.Errorf("第二次请求应带上工具错误结果")
	}
	if tr := h.turn(); tr.Status != domain.TurnSuccess {
		t.Errorf("本轮应继续并成功：%+v", tr)
	}
}

func TestRunTurn_调用上限到达结束本轮并说明(t *testing.T) {
	h := newHarness(domain.StatePlanning, domain.TriggerUser, 2, 100)
	h.chat.fallback = &fakeReply{out: ChatOutput{ToolCalls: []ToolCall{toolCall("c", "read_canvas", `{}`)}}}
	if _, err := h.run(); err != nil {
		t.Fatal(err)
	}
	if n := len(h.chat.calls()); n != 5 {
		t.Fatalf("应恰好调用上限 5 次，实际 %d", n)
	}
	tr := h.turn()
	if tr.CallCount != 5 || tr.Status != domain.TurnSuccess || tr.ErrorCode != ErrCodeCallLimit {
		t.Errorf("轮次不对：%+v", tr)
	}
	notes := h.store.messagesByRole(repo.RoleSystemNote)
	if len(notes) != 1 || !strings.Contains(notes[0].Content, "调用上限") {
		t.Errorf("消息流里应有说明：%+v", notes)
	}
	if s := h.session(); s.State != domain.StateIdle {
		t.Errorf("会话应回 idle：%s", s.State)
	}
}

func TestRunTurn_停止请求命中即结束(t *testing.T) {
	h := newHarness(domain.StatePlanning, domain.TriggerUser, 2, 100)
	// 流出两段文本后用户按了停止，流一直不结束（直到被取消）。
	h.chat.replies = []fakeReply{{
		deltas:  []string{"我先想", "一想……"},
		waitCtx: true,
		beforeReturn: func() {
			h.store.mu.Lock()
			now := time.Now()
			h.store.sessions[hSession].StopRequestedAt = &now
			h.store.mu.Unlock()
		},
	}}
	start := time.Now()
	if _, err := h.run(); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("停止后应立即结束，用了 %s", time.Since(start))
	}
	as := h.store.messagesByRole(repo.RoleAssistant)
	if len(as) != 1 || as[0].Content != "我先想一想……" {
		t.Fatalf("已有文本应保留：%+v", as)
	}
	tr := h.turn()
	if tr.Status != domain.TurnSuccess || tr.ErrorCode != ErrCodeStopped {
		t.Errorf("有文本的停止应按成功结算：%+v", tr)
	}
	if s := h.session(); s.State != domain.StateIdle {
		t.Errorf("会话应回 idle：%s", s.State)
	}
}

func TestRunTurn_停止请求在工具前命中(t *testing.T) {
	h := newHarness(domain.StateExecuting, domain.TriggerUser, 2, 100)
	gen := h.tools[domain.ToolReadCanvas].(*fakeTool)
	h.chat.replies = []fakeReply{{
		out: ChatOutput{ToolCalls: []ToolCall{toolCall("c1", "read_canvas", `{}`)}},
		beforeReturn: func() {
			h.store.mu.Lock()
			now := time.Now()
			h.store.sessions[hSession].StopRequestedAt = &now
			h.store.mu.Unlock()
		},
	}}
	if _, err := h.run(); err != nil {
		t.Fatal(err)
	}
	if gen.runs != 0 {
		t.Errorf("停止后不得再执行工具")
	}
	if tr := h.turn(); tr.Status != domain.TurnCancelled || h.ledger.refunded["agent_turn:200"] != 2 {
		t.Errorf("无有效输出的停止应取消并退款：%+v refunded=%v", tr, h.ledger.refunded)
	}
}

func TestRunTurn_挂起时落库状态并退出(t *testing.T) {
	h := newHarness(domain.StatePlanning, domain.TriggerUser, 2, 100)
	ask := h.tools[domain.ToolAskUser].(*fakeTool)
	ask.result = ToolResult{Content: json.RawMessage(`{"cardId":"77"}`),
		Suspend: &Suspension{Event: domain.EvAskUser, CardID: 77}}
	h.chat.replies = []fakeReply{
		{out: ChatOutput{Text: "我需要确认风格。", ToolCalls: []ToolCall{
			toolCall("c1", "ask_user", `{}`), toolCall("c2", "read_canvas", `{}`)}}},
	}
	if _, err := h.run(); err != nil {
		t.Fatal(err)
	}
	if n := len(h.chat.calls()); n != 1 {
		t.Fatalf("挂起后不得再调用模型，实际 %d 次", n)
	}
	if h.tools[domain.ToolReadCanvas].(*fakeTool).runs != 0 {
		t.Errorf("挂起后同一批里后面的工具不应执行")
	}
	if s := h.session(); s.State != domain.StateAwaitingInput || s.RunnerLeaseUntil != nil {
		t.Errorf("会话应落 awaiting_input 并释放租约：%+v", s)
	}
	last := h.events.events[len(h.events.events)-1].(*AgentStateEvent)
	if last.PendingCardID != "77" || last.State != string(domain.StateAwaitingInput) {
		t.Errorf("状态事件不对：%+v", last)
	}
	if tr := h.turn(); tr.Status != domain.TurnSuccess {
		t.Errorf("挂起轮按成功结算：%+v", tr)
	}
	// 下一轮组上下文时，没执行的 c2 要补一条结果（否则上游 400）。
	msgs := toChatMessages(h.store.msgs)
	answered := 0
	for _, m := range msgs {
		if m.Role == "tool" {
			answered++
		}
	}
	if answered != 2 {
		t.Errorf("两个工具调用都应有结果，实际 %d", answered)
	}
}

func TestRunTurn_模型失败无输出退款且会话failed(t *testing.T) {
	h := newHarness(domain.StatePlanning, domain.TriggerUser, 3, 100)
	h.chat.replies = []fakeReply{{err: errors.New("连不上上游")}}
	if _, err := h.run(); err == nil {
		t.Fatal("应返回错误用于日志")
	}
	if tr := h.turn(); tr.Status != domain.TurnFailed || tr.ErrorCode != ErrCodeModel {
		t.Errorf("轮次应 failed：%+v", tr)
	}
	if h.ledger.refunded["agent_turn:200"] != 3 || h.ledger.balance != 100 {
		t.Errorf("应全额退款：%+v balance=%d", h.ledger.refunded, h.ledger.balance)
	}
	if s := h.session(); s.State != domain.StateFailed {
		t.Errorf("会话应 failed：%s", s.State)
	}
	// 重投：轮次已终态，不再扣也不再退。
	if out, _ := h.run(); out != OutcomeDuplicate {
		t.Errorf("重投应为 duplicate：%v", out)
	}
}

func TestRunTurn_余额不足不调模型会话回idle(t *testing.T) {
	h := newHarness(domain.StatePlanning, domain.TriggerUser, 5, 1)
	if _, err := h.run(); err != nil {
		t.Fatal(err)
	}
	if len(h.chat.calls()) != 0 {
		t.Fatal("余额不足不得调用模型")
	}
	if tr := h.turn(); tr.Status != domain.TurnFailed || tr.ErrorCode != ErrCodeInsufficientCredits {
		t.Errorf("轮次不对：%+v", tr)
	}
	if s := h.session(); s.State != domain.StateIdle {
		t.Errorf("会话应回 idle：%s", s.State)
	}
	last := h.events.events[len(h.events.events)-1].(*AgentStateEvent)
	if last.Reason != ErrCodeInsufficientCredits {
		t.Errorf("事件原因不对：%+v", last)
	}
}

func TestRunTurn_系统轮不扣费(t *testing.T) {
	h := newHarness(domain.StateExecuting, domain.TriggerSystem, 5, 100)
	h.chat.replies = []fakeReply{{err: errors.New("断开")}}
	_, _ = h.run()
	if len(h.ledger.consumed) != 0 || len(h.ledger.refunded) != 0 {
		t.Errorf("系统轮不得扣费也不得调用退款：%+v %+v", h.ledger.consumed, h.ledger.refunded)
	}
}

func TestRunTurn_发起人被移出会话取消(t *testing.T) {
	h := newHarness(domain.StatePlanning, domain.TriggerUser, 2, 100)
	h.members.member = false
	if _, err := h.run(); err != nil {
		t.Fatal(err)
	}
	if s := h.session(); s.State != domain.StateCancelled {
		t.Errorf("会话应 cancelled：%s", s.State)
	}
	if tr := h.turn(); tr.Status != domain.TurnCancelled || len(h.ledger.consumed) != 0 {
		t.Errorf("没开始的轮应取消且不扣费：%+v", tr)
	}
}

func TestRunTurn_租约被占时丢弃(t *testing.T) {
	h := newHarness(domain.StatePlanning, domain.TriggerUser, 2, 100)
	until := time.Now().Add(time.Minute)
	h.store.sessions[hSession].RunnerID = "other"
	h.store.sessions[hSession].RunnerLeaseUntil = &until
	if out, _ := h.run(); out != OutcomeBusy {
		t.Fatalf("应为 busy：%v", out)
	}
	if len(h.chat.calls()) != 0 || len(h.ledger.consumed) != 0 {
		t.Error("租约被占时不得做任何事")
	}
}

func TestRunTurn_执行态有等待任务时进waiting_task(t *testing.T) {
	h := newHarness(domain.StateExecuting, domain.TriggerUser, 2, 100)
	g := h.tools[domain.ToolGenerate].(*fakeTool)
	g.result = ToolResult{Content: json.RawMessage(`{"generationId":"9"}`), GenerationID: 9}
	h.chat.replies = []fakeReply{
		{out: ChatOutput{ToolCalls: []ToolCall{toolCall("c1", "generate", `{}`)}}},
		{out: ChatOutput{Text: "已提交生成，完成后继续。"}},
	}
	if _, err := h.run(); err != nil {
		t.Fatal(err)
	}
	if w := h.store.stepsByStatus(repo.StepWaiting); len(w) != 1 || w[0].GenerationID != 9 {
		t.Fatalf("步骤应标 waiting：%+v", w)
	}
	if s := h.session(); s.State != domain.StateWaitingTask {
		t.Errorf("会话应进 waiting_task：%s", s.State)
	}
}

func TestDeltaCoalescer_50ms或64字符合并(t *testing.T) {
	now := time.Unix(0, 0)
	var got []string
	c := newDeltaCoalescer(func() time.Time { return now }, func(s string) { got = append(got, s) })

	// 时间不走：攒够 64 个字符才发。
	for i := 0; i < 63; i++ {
		c.Add("字")
	}
	if len(got) != 0 {
		t.Fatalf("不足 64 字符不应发：%v", got)
	}
	c.Add("字")
	if len(got) != 1 || len([]rune(got[0])) != 64 {
		t.Fatalf("满 64 字符应合并成一条：%v", got)
	}
	// 不足 64 字符，但距上次发出满 50ms。
	c.Add("ab")
	now = now.Add(49 * time.Millisecond)
	c.Add("c")
	if len(got) != 1 {
		t.Fatalf("不足 50ms 不应发")
	}
	now = now.Add(time.Millisecond)
	c.Add("d")
	if len(got) != 2 || got[1] != "abcd" {
		t.Fatalf("满 50ms 应把缓冲一起发出：%v", got)
	}
	c.Add("尾巴")
	c.Flush()
	if len(got) != 3 || got[2] != "尾巴" {
		t.Fatalf("Flush 应发出剩余内容：%v", got)
	}
	c.Flush()
	if len(got) != 3 {
		t.Fatalf("空缓冲 Flush 不应发")
	}
}

type fakeReasoningDefaults struct{ code string }

func (f fakeReasoningDefaults) DefaultReasoningModel(context.Context, int64) (string, error) {
	return f.code, nil
}

// 空状态的建议胶囊会建一个偏好为空的会话：不兜底的话这一轮以 model_not_selected 失败，
// 而用户什么都没做错（2026-10-16 实测踩到）。
func TestRunTurn_会话未选推理模型时用平台默认模型(t *testing.T) {
	h := newHarness(domain.StatePlanning, domain.TriggerUser, 36, 100)
	h.store.sessions[hSession].Prefs = json.RawMessage(`{}`)
	h.runner.d.Reasoning = fakeReasoningDefaults{code: "m-default"}
	h.chat.replies = []fakeReply{{out: ChatOutput{Text: "好的"}}}
	if out, err := h.run(); err != nil || out != OutcomeDone {
		t.Fatalf("outcome=%v err=%v", out, err)
	}
	if tr := h.turn(); tr.Status != domain.TurnSuccess {
		t.Fatalf("应按默认模型跑完：%+v", tr)
	}
	if h.ledger.consumed["agent_turn:200"] != 36 {
		t.Errorf("应按默认模型的价格扣费：%+v", h.ledger.consumed)
	}
}

func TestRunTurn_没有任何可用推理模型时明确失败不扣费(t *testing.T) {
	h := newHarness(domain.StatePlanning, domain.TriggerUser, 36, 100)
	h.store.sessions[hSession].Prefs = json.RawMessage(`{}`)
	h.runner.d.Reasoning = fakeReasoningDefaults{code: ""}
	_, _ = h.run()
	if tr := h.turn(); tr.Status != domain.TurnFailed || tr.ErrorCode != ErrCodeNoModel {
		t.Fatalf("应以 model_not_selected 失败：%+v", tr)
	}
	if len(h.ledger.consumed) != 0 {
		t.Errorf("不应扣费：%+v", h.ledger.consumed)
	}
}
