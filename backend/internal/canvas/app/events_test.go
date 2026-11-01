package app

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/li904338203/tabtv/backend/internal/canvas/repo"
)

// canvas.ops_applied 要与 api/events/canvas-ops-applied.schema.json 对得上。
//
// ⚠️ 事件类型没有代码生成：直接读 schema 文件去核 —— required 一个不能少、
// properties 之外一个不能多、枚举与 ID 形态逐项对，改了任何一边另一边就红
// （同 generation/app/events_test.go）。

type schemaNode struct {
	Required   []string              `json:"required"`
	Properties map[string]schemaNode `json:"properties"`
	Enum       []string              `json:"enum"`
	Const      string                `json:"const"`
	Items      *schemaNode           `json:"items"`
}

func loadOpsSchema(t *testing.T) (schemaNode, *regexp.Regexp) {
	t.Helper()
	raw, err := os.ReadFile("../../../../api/events/canvas-ops-applied.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		schemaNode
		Defs map[string]struct {
			Pattern string `json:"pattern"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s.schemaNode, regexp.MustCompile(s.Defs["id"].Pattern)
}

func checkObject(t *testing.T, where string, obj map[string]any, s schemaNode) {
	t.Helper()
	for _, k := range s.Required {
		if _, ok := obj[k]; !ok {
			t.Errorf("%s 缺少契约必填字段 %q", where, k)
		}
	}
	for k := range obj {
		if _, ok := s.Properties[k]; !ok {
			t.Errorf("%s 多出契约里没有的字段 %q（schema 是 additionalProperties:false）", where, k)
		}
	}
}

func TestCanvasOpsApplied_与事件契约一致(t *testing.T) {
	schema, idPattern := loadOpsSchema(t)
	now := time.Date(2026, 10, 15, 9, 30, 0, 0, time.UTC)
	ops := json.RawMessage(`[
		{"kind":"add_node","node":{"id":"a_1","type":"text","title":"t","params":{},"agent":{"sessionId":"7"}}},
		{"kind":"add_edge","edge":{"id":"a_2","source":"n_1","target":"a_1","order":1}},
		{"kind":"update_node","nodeId":"a_1","patch":{"title":"x"}},
		{"kind":"move_node","nodeId":"a_1","position":{"x":1,"y":2}},
		{"kind":"add_frame","frame":{"id":"a_3","type":"frame","members":["a_1"]}},
		{"kind":"set_frame_members","nodeId":"a_3","frame":{"members":[]}}]`)
	big := json.RawMessage(`[{"kind":"add_node","node":{"id":"a_1","type":"text","params":{"p":"` +
		strings.Repeat("x", MaxEventOpsBytes) + `"}}}]`)

	cases := []struct {
		name       string
		entry      repo.OpLogEntry
		clientName string
		truncated  bool
	}{
		{"agent 写入，六种操作", repo.OpLogEntry{ProjectID: 77, FromVersion: 3, ToVersion: 4, BatchID: "s7:step1",
			ActorKind: "agent", AgentSessionID: 7, UserID: 1001, Ops: ops}, "", false},
		{"超过 64KB 不带 ops", repo.OpLogEntry{ProjectID: 77, FromVersion: 4, ToVersion: 5, BatchID: "b2",
			ActorKind: "agent", AgentSessionID: 7, UserID: 1001, Ops: big}, "", true},
		{"user 写入，没有会话", repo.OpLogEntry{ProjectID: 77, FromVersion: 5, ToVersion: 6, BatchID: "b3",
			ActorKind: "user", UserID: 1001, Ops: ops}, "", false},
		{"mcp 写入，带授权与客户端名", repo.OpLogEntry{ProjectID: 77, FromVersion: 6, ToVersion: 7, BatchID: "m1",
			ActorKind: "mcp", McpGrantID: 5001, UserID: 1001, Ops: ops}, "Claude Code", false},
		{"mcp 写入，没有客户端名", repo.OpLogEntry{ProjectID: 77, FromVersion: 7, ToVersion: 8, BatchID: "m2",
			ActorKind: "mcp", McpGrantID: 5001, UserID: 1001, Ops: ops}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, err := json.Marshal(NewCanvasOpsApplied(42, c.entry, c.clientName, now))
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatal(err)
			}
			checkObject(t, "事件", m, schema)
			if m["type"] != schema.Properties["type"].Const {
				t.Errorf("type 应为 %q，得到 %v", schema.Properties["type"].Const, m["type"])
			}
			for _, k := range []string{"workspaceId", "projectId"} {
				if s, _ := m[k].(string); !idPattern.MatchString(s) {
					t.Errorf("%s 应是十进制字符串 ID，得到 %#v", k, m[k])
				}
			}
			if _, err := time.Parse(time.RFC3339Nano, m["occurredAt"].(string)); err != nil {
				t.Errorf("occurredAt 不是 date-time: %v", m["occurredAt"])
			}
			actor := m["actor"].(map[string]any)
			checkObject(t, "actor", actor, schema.Properties["actor"])
			if !slices.Contains(schema.Properties["actor"].Properties["kind"].Enum, actor["kind"].(string)) {
				t.Errorf("actor.kind %v 不在契约枚举里", actor["kind"])
			}
			for _, k := range []string{"userId", "agentSessionId", "mcpGrantId"} {
				if v := actor[k]; v != nil && !idPattern.MatchString(v.(string)) {
					t.Errorf("actor.%s 应是 null 或字符串 ID，得到 %#v", k, v)
				}
			}
			// MCP 写入：授权 ID 与客户端名都带上；其余来源两者都是 null。
			if c.entry.ActorKind == "mcp" {
				if actor["mcpGrantId"] != "5001" {
					t.Errorf("mcp 写入应带 mcpGrantId，得到 %#v", actor["mcpGrantId"])
				}
				if want := any(c.clientName); c.clientName == "" && actor["clientName"] != nil ||
					c.clientName != "" && actor["clientName"] != want {
					t.Errorf("clientName 应为 %q（空串为 null），得到 %#v", c.clientName, actor["clientName"])
				}
			} else if actor["mcpGrantId"] != nil || actor["clientName"] != nil {
				t.Errorf("非 mcp 写入的 mcpGrantId / clientName 应为 null：%v", actor)
			}
			if m["truncated"] != c.truncated {
				t.Errorf("truncated 应为 %v，得到 %v", c.truncated, m["truncated"])
			}
			list, has := m["ops"].([]any)
			if c.truncated {
				if has {
					t.Error("truncated 时不该带 ops")
				}
				return
			}
			if !has || len(list) == 0 {
				t.Fatal("没有带 ops")
			}
			item := *schema.Properties["ops"].Items
			for _, raw := range list {
				op := raw.(map[string]any)
				checkObject(t, "ops[]", op, item)
				if !slices.Contains(item.Properties["kind"].Enum, op["kind"].(string)) {
					t.Errorf("op.kind %v 不在契约枚举里", op["kind"])
				}
				if p, ok := op["position"].(map[string]any); ok {
					checkObject(t, "position", p, item.Properties["position"])
				}
			}
		})
	}
}

type capturePublisher struct {
	topic   string
	payload []byte
}

func (c *capturePublisher) Publish(_ context.Context, topic string, payload []byte) error {
	c.topic, c.payload = topic, payload
	return nil
}

func TestPublishOps_发到项目topic(t *testing.T) {
	pub := &capturePublisher{}
	s := &Service{}
	s.SetEvents(pub)
	s.publishOps(context.Background(), 42, repo.OpLogEntry{ProjectID: 77, FromVersion: 1, ToVersion: 2,
		BatchID: "b", ActorKind: "agent", AgentSessionID: 7, UserID: 1, Ops: json.RawMessage(`[]`)}, "")
	if pub.topic != "project:77" {
		t.Errorf("topic 应为 project:77，得到 %q", pub.topic)
	}
	if !json.Valid(pub.payload) {
		t.Errorf("payload 不是合法 JSON: %s", pub.payload)
	}
}

func TestNeedsReload(t *testing.T) {
	b := func(from int) repo.OpLogEntry { return repo.OpLogEntry{FromVersion: from, ToVersion: from + 1} }
	cases := []struct {
		name    string
		since   int
		current int
		batches []repo.OpLogEntry
		want    bool
	}{
		{"已是最新", 5, 5, nil, false},
		{"全部来自操作日志", 3, 6, []repo.OpLogEntry{b(3), b(4), b(5)}, false},
		{"中间夹一次真人保存", 3, 6, []repo.OpLogEntry{b(3), b(5)}, true},
		{"末尾是真人保存", 3, 6, []repo.OpLogEntry{b(3), b(4)}, true},
		{"开头已被清理", 3, 6, []repo.OpLogEntry{b(4), b(5)}, true},
		{"客户端比服务端新", 7, 6, nil, true},
	}
	for _, c := range cases {
		if got := NeedsReload(c.since, c.current, c.batches); got != c.want {
			t.Errorf("%s: 得到 %v，期望 %v", c.name, got, c.want)
		}
	}
	many := make([]repo.OpLogEntry, 0, MaxOpsSinceBatches+1)
	for i := 0; i <= MaxOpsSinceBatches; i++ {
		many = append(many, b(i+1))
	}
	if !NeedsReload(1, MaxOpsSinceBatches+2, many) {
		t.Error("落后超过上限应整页重载")
	}
}

func TestCreatedNodeIDs(t *testing.T) {
	got := CreatedNodeIDs(json.RawMessage(`[
		{"kind":"add_node","node":{"id":"a_1"}},
		{"kind":"add_edge","edge":{"id":"a_2"}},
		{"kind":"add_frame","frame":{"id":"a_3"}},
		{"kind":"move_node","nodeId":"a_1","position":{"x":0,"y":0}}]`))
	if !slices.Equal(got, []string{"a_1", "a_3"}) {
		t.Errorf("得到 %v", got)
	}
}
