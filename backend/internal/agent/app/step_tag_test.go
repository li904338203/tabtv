package app

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
)

// MKT-T-22：步骤的计划项键与错误码由 Runner 落库 —— 停后续 / 部分成功按码判（MKT-T-27），不按文案。
func TestRunTurn_步骤记计划项键与错误码(t *testing.T) {
	h := newHarness(domain.StatePlanning, domain.TriggerUser, 2, 100)
	rc := h.tools[domain.ToolReadCanvas].(*fakeTool)
	rc.err = &StepError{PlanItemKey: "v2", ErrorCode: "INSUFFICIENT_CREDITS", Err: errors.New("余额不足，还差 10 积分")}
	h.chat.replies = []fakeReply{
		{out: ChatOutput{ToolCalls: []ToolCall{toolCall("c1", "read_canvas", `{}`)}}},
		{out: ChatOutput{Text: "余额不足。"}},
	}
	if _, err := h.run(); err != nil {
		t.Fatal(err)
	}
	failed := h.store.stepsByStatus(repo.StepFailed)
	if len(failed) != 1 || failed[0].PlanItemKey != "v2" || failed[0].ErrorCode != "INSUFFICIENT_CREDITS" {
		t.Fatalf("失败步骤应记键与错误码：%+v", failed)
	}
	tools := h.store.messagesByRole(repo.RoleTool)
	var m map[string]string
	if len(tools) != 1 || json.Unmarshal([]byte(tools[0].Content), &m) != nil || m["error"] != "余额不足，还差 10 积分" {
		t.Fatalf("回给模型的仍是那句话：%+v", tools)
	}
}

func TestRunTurn_拒绝的错误码与成功的计划项键(t *testing.T) {
	h := newHarness(domain.StatePlanning, domain.TriggerUser, 2, 100)
	rc := h.tools[domain.ToolReadCanvas].(*fakeTool)
	rc.err = &StepError{PlanItemKey: "v1", Err: &domain.ToolRejection{Code: domain.ToolErrPlanItemMismatch,
		Message: "不一致", Details: map[string]any{"item": "v1"}}}
	h.chat.replies = []fakeReply{
		{out: ChatOutput{ToolCalls: []ToolCall{toolCall("c1", "read_canvas", `{}`)}}},
		{out: ChatOutput{Text: "好"}},
	}
	if _, err := h.run(); err != nil {
		t.Fatal(err)
	}
	failed := h.store.stepsByStatus(repo.StepFailed)
	if len(failed) != 1 || failed[0].PlanItemKey != "v1" || failed[0].ErrorCode != domain.ToolErrPlanItemMismatch {
		t.Fatalf("没给码时取拒绝的码：%+v", failed)
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(h.store.messagesByRole(repo.RoleTool)[0].Content), &m)
	if m["error"] != domain.ToolErrPlanItemMismatch || m["item"] != "v1" {
		t.Fatalf("包在 StepError 里的拒绝仍回结构化结果：%v", m)
	}

	h2 := newHarness(domain.StatePlanning, domain.TriggerUser, 2, 100)
	h2.tools[domain.ToolReadCanvas].(*fakeTool).result = ToolResult{Content: json.RawMessage(`{"ok":true}`),
		PlanItemKey: "v3"}
	h2.chat.replies = []fakeReply{
		{out: ChatOutput{ToolCalls: []ToolCall{toolCall("c1", "read_canvas", `{}`)}}},
		{out: ChatOutput{Text: "好"}},
	}
	if _, err := h2.run(); err != nil {
		t.Fatal(err)
	}
	ok := h2.store.stepsByStatus(repo.StepSuccess)
	if len(ok) != 1 || ok[0].PlanItemKey != "v3" || ok[0].ErrorCode != "" {
		t.Fatalf("成功步骤记键、不记码：%+v", ok)
	}
}
