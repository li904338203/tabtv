package app

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"testing"
	"time"
)

// agent.state / delta / step / card 要与 api/events/agent-*.schema.json 对得上。
//
// ⚠️ 事件类型没有代码生成：直接读 schema 文件去核 —— required 一个不能少、
// properties 之外一个不能多（additionalProperties:false）、const / 枚举 / ID 形态逐项对，
// 改了任何一边另一边就红（同 generation/app/events_test.go、canvas/app/events_test.go）。

type agentSchemaProp struct {
	Const string   `json:"const"`
	Enum  []string `json:"enum"`
	Ref   string   `json:"$ref"`
	Type  any      `json:"type"`
}

type agentSchema struct {
	Required             []string                   `json:"required"`
	AdditionalProperties *bool                      `json:"additionalProperties"`
	Properties           map[string]agentSchemaProp `json:"properties"`
	Defs                 map[string]struct {
		Pattern string `json:"pattern"`
	} `json:"$defs"`
}

func loadAgentSchema(t *testing.T, name string) agentSchema {
	t.Helper()
	raw, err := os.ReadFile("../../../../api/events/" + name + ".schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var s agentSchema
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	if s.AdditionalProperties == nil || *s.AdditionalProperties {
		t.Fatalf("%s 必须是 additionalProperties:false", name)
	}
	return s
}

func checkAgentEvent(t *testing.T, schemaName string, ev any) map[string]any {
	t.Helper()
	s := loadAgentSchema(t, schemaName)
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range s.Required {
		if _, ok := m[k]; !ok {
			t.Errorf("%s 缺少契约必填字段 %q", schemaName, k)
		}
	}
	idPattern := regexp.MustCompile(s.Defs["id"].Pattern)
	for k, v := range m {
		p, ok := s.Properties[k]
		if !ok {
			t.Errorf("%s 多出契约里没有的字段 %q", schemaName, k)
			continue
		}
		if p.Const != "" && v != p.Const {
			t.Errorf("%s.%s 应为 %q，得到 %v", schemaName, k, p.Const, v)
		}
		if len(p.Enum) > 0 {
			if s, _ := v.(string); !slices.Contains(p.Enum, s) {
				t.Errorf("%s.%s=%v 不在契约枚举 %v 里", schemaName, k, v, p.Enum)
			}
		}
		if p.Ref == "#/$defs/id" {
			if s, _ := v.(string); !idPattern.MatchString(s) {
				t.Errorf("%s.%s 应是十进制字符串 ID，得到 %#v", schemaName, k, v)
			}
		}
	}
	if s, _ := m["occurredAt"].(string); s != "" {
		if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
			t.Errorf("%s.occurredAt 不是 date-time: %v", schemaName, s)
		}
	}
	return m
}

func TestAgentEvents_符合schema(t *testing.T) {
	now := nowStr(time.Date(2026, 10, 15, 9, 30, 0, 0, time.UTC))

	t.Run("agent.state 全字段", func(t *testing.T) {
		checkAgentEvent(t, "agent-state", AgentStateEvent{Type: EventAgentState, OccurredAt: now,
			SessionID: idStr(7), State: "awaiting_approval", TurnID: idStr(8), ApprovedBudget: 60,
			Spent: 20, PendingCardID: idStr(9), Reason: "stop_requested"})
	})
	t.Run("agent.state 可选字段省略", func(t *testing.T) {
		m := checkAgentEvent(t, "agent-state", AgentStateEvent{Type: EventAgentState, OccurredAt: now,
			SessionID: idStr(7), State: "idle"})
		for _, k := range []string{"turnId", "pendingCardId", "reason"} {
			if _, ok := m[k]; ok {
				t.Errorf("空的 %s 不应出现在载荷里（schema 不允许空串 ID）", k)
			}
		}
	})
	t.Run("agent.delta", func(t *testing.T) {
		checkAgentEvent(t, "agent-delta", AgentDeltaEvent{Type: EventAgentDelta, OccurredAt: now,
			SessionID: idStr(7), TurnID: idStr(8), MessageID: idStr(10), Text: "悟空"})
	})
	t.Run("agent.step", func(t *testing.T) {
		checkAgentEvent(t, "agent-step", AgentStepEvent{Type: EventAgentStep, OccurredAt: now,
			SessionID: idStr(7), TurnID: idStr(8), StepID: idStr(11), Tool: "read_canvas",
			Status: "success", Display: json.RawMessage(`{"title":"读取画布"}`)})
		checkAgentEvent(t, "agent-step", AgentStepEvent{Type: EventAgentStep, OccurredAt: now,
			SessionID: idStr(7), TurnID: idStr(8), StepID: idStr(11), Tool: "generate", Status: "waiting"})
	})
	t.Run("agent.card", func(t *testing.T) {
		for _, st := range []string{"pending", "submitted", "voided", "expired"} {
			checkAgentEvent(t, "agent-card", AgentCardEvent{Type: EventAgentCard, OccurredAt: now,
				SessionID: idStr(7), CardID: idStr(12), Kind: "plan_confirm", Status: st})
		}
	})
	t.Run("topic", func(t *testing.T) {
		if got := ProjectTopic(42); got != "project:42" {
			t.Errorf("ProjectTopic = %q", got)
		}
	})
}
