package app

import (
	"encoding/json"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
)

// 按规则拒绝（MKT-T-20，如 too_many_images）：步骤 failed，工具结果是结构化的
// {error: 码, message, ...数字}，而不是把整段错误塞进一个字符串 —— 模型要据 max 重选。
func TestRunTurn_工具按规则拒绝回结构化结果(t *testing.T) {
	h := newHarness(domain.StatePlanning, domain.TriggerUser, 2, 100)
	rc := h.tools[domain.ToolReadCanvas].(*fakeTool)
	rc.err = &domain.ToolRejection{Code: domain.ToolErrTooManyImages, Message: "最多 3 张",
		Details: map[string]any{"item": "v1", "max": 3}}
	h.chat.replies = []fakeReply{
		{out: ChatOutput{ToolCalls: []ToolCall{toolCall("c1", "read_canvas", `{}`)}}},
		{out: ChatOutput{Text: "好的，我减到 3 张。"}},
	}
	if _, err := h.run(); err != nil {
		t.Fatal(err)
	}
	if failed := h.store.stepsByStatus(repo.StepFailed); len(failed) != 1 {
		t.Fatalf("应有一行 failed 的步骤：%+v", failed)
	}
	tools := h.store.messagesByRole(repo.RoleTool)
	if len(tools) != 1 {
		t.Fatalf("应回一条工具结果：%+v", tools)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(tools[0].Content), &m); err != nil {
		t.Fatalf("工具结果应是 JSON 对象：%s", tools[0].Content)
	}
	if m["error"] != "too_many_images" || m["item"] != "v1" || m["max"].(float64) != 3 || m["message"] != "最多 3 张" {
		t.Fatalf("结构化结果不对：%v", m)
	}
}
