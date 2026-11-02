package app

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"testing"
	"time"


	"github.com/li904338203/tabtv/backend/internal/generation/domain"
	"github.com/li904338203/tabtv/backend/internal/generation/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/eventbus"
)

// 状态事件要与 api/events/generation-update.schema.json 对得上。
//
// ⚠️ 事件类型没有代码生成（README 说有，其实没有），此前后端用 map 拼了六个字段，
// 契约要求的 occurredAt / workspaceId / outputs / error 一个都没有 —— 失败事件里没有原因，
// 而这件事没有任何测试会发现。这里直接读 schema 文件去核：required 一个不能少、
// properties 之外一个不能多、枚举与 ID 形态逐项对，改了任何一边另一边就红。

type schemaNode struct {
	Required   []string              `json:"required"`
	Properties map[string]schemaNode `json:"properties"`
	Enum       []string              `json:"enum"`
	Const      string                `json:"const"`
	Items      *schemaNode           `json:"items"`
	Ref        string                `json:"$ref"`
}

func loadUpdateSchema(t *testing.T) (schemaNode, *regexp.Regexp) {
	t.Helper()
	raw, err := os.ReadFile("../../../../api/events/generation-update.schema.json")
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

// checkObject 按 schema 核一个对象：必填齐全、没有多余字段。
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

func encodeUpdate(t *testing.T, u GenerationUpdate) map[string]any {
	t.Helper()
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestGenerationUpdate_与事件契约一致(t *testing.T) {
	schema, idPattern := loadUpdateSchema(t)
	now := time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC)
	g := repo.Generation{ID: 9001, WorkspaceID: 42, ProjectID: 7, NodeID: "node-1"}
	inGroup := repo.Generation{ID: 9002, WorkspaceID: 42, ProjectID: 7, GroupItemID: 55}

	cases := []struct {
		name   string
		update GenerationUpdate
	}{
		{"成功：图片 + 文本产物", NewGenerationUpdate(g, domain.StatusSuccess, []repo.Output{
			{Idx: 0, OutputType: "image", MediaObjectID: 700001},
			{Idx: 1, OutputType: "text", TextContent: "一段描述"},
		}, "", "", now)},
		{"失败：带原因", NewGenerationUpdate(g, domain.StatusFailed, nil,
			"provider_failed", "上游返回错误", now)},
		{"取消：组子项、没有画布节点", NewGenerationUpdate(inGroup, domain.StatusCancelled, nil,
			domain.ErrorCodeCancelled, "用户取消", now)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := encodeUpdate(t, c.update)
			checkObject(t, "事件", m, schema)

			if m["type"] != schema.Properties["type"].Const {
				t.Errorf("type 应为 %q，得到 %v", schema.Properties["type"].Const, m["type"])
			}
			if !slices.Contains(schema.Properties["status"].Enum, m["status"].(string)) {
				t.Errorf("status %v 不在契约枚举里", m["status"])
			}
			for _, k := range []string{"generationId", "workspaceId", "projectId"} {
				if s, _ := m[k].(string); !idPattern.MatchString(s) {
					t.Errorf("%s 应是十进制字符串 ID（GO-34），得到 %#v", k, m[k])
				}
			}
			if v := m["groupItemId"]; v != nil && !idPattern.MatchString(v.(string)) {
				t.Errorf("groupItemId 应是 null 或字符串 ID，得到 %#v", v)
			}
			if v := m["nodeId"]; v != nil {
				if s, ok := v.(string); !ok || s == "" {
					t.Errorf("nodeId 应是非空字符串或 null（空串会被当成一个叫 \"\" 的节点），得到 %#v", v)
				}
			}
			if _, err := time.Parse(time.RFC3339Nano, m["occurredAt"].(string)); err != nil {
				t.Errorf("occurredAt 不是 date-time：%v", err)
			}

			outputs, ok := m["outputs"].([]any)
			if !ok {
				t.Fatalf("outputs 必须是数组（失败时也是空数组，不是 null），得到 %#v", m["outputs"])
			}
			for i, o := range outputs {
				item := o.(map[string]any)
				checkObject(t, "产物", item, *schema.Properties["outputs"].Items)
				typ := item["outputType"].(string)
				if !slices.Contains(schema.Properties["outputs"].Items.Properties["outputType"].Enum, typ) {
					t.Errorf("产物 %d 的 outputType %q 不在契约枚举里", i, typ)
				}
				_, hasURL := item["url"]
				if (typ == "text") == hasURL {
					t.Errorf("产物 %d（%s）：媒体产物必须有 url、文本产物不该有，得到 %#v", i, typ, item)
				}
			}

			if c.update.Status == domain.StatusSuccess {
				if m["error"] != nil {
					t.Errorf("成功事件的 error 应为 null，得到 %#v", m["error"])
				}
				if len(outputs) == 0 {
					t.Error("成功事件的 outputs 不该为空")
				}
				return
			}
			errObj, ok := m["error"].(map[string]any)
			if !ok {
				t.Fatalf("失败 / 取消事件必须带 error（此前没有，失败的节点上只有一个红叉），得到 %#v", m["error"])
			}
			checkObject(t, "error", errObj, schema.Properties["error"])
			if errObj["reason"] == "" || errObj["message"] == "" {
				t.Errorf("error 的 reason / message 不该为空：%#v", errObj)
			}
			if len(outputs) != 0 {
				t.Errorf("失败 / 取消事件不该带产物：%#v", outputs)
			}
		})
	}
}

// 媒体产物的地址只有一处拼法：事件里的与产物列表里的必须是同一个。
func TestGenerationUpdate_产物地址与产物端点同一个(t *testing.T) {
	u := NewGenerationUpdate(repo.Generation{ID: 1, WorkspaceID: 2, ProjectID: 3}, domain.StatusSuccess,
		[]repo.Output{{Idx: 0, OutputType: "video", MediaObjectID: 123}}, "", "", time.Now())
	if got, want := u.Outputs[0].URL, domain.OutputMediaURL(123); got != want || want != "/api/v1/media/123" {
		t.Fatalf("产物地址应为受保护的媒体端点 /api/v1/media/123，得到 %q", got)
	}
}

// ---------------------------------------------------------------- 经进程内事件总线发布（TT-T-06）

func TestBusEventPublisher_topic为workspace且payload原样(t *testing.T) {
	bus := eventbus.NewLocal()
	var topics, payloads []string
	bus.SetEmitter(func(topic string, payload []byte) {
		topics = append(topics, topic)
		payloads = append(payloads, string(payload))
	})
	p := NewBusEventPublisher(bus)
	payload := json.RawMessage(`{"type":"generation.update","generationId":"9001"}`)
	if err := p.PublishState(context.Background(), 42, payload); err != nil {
		t.Fatal(err)
	}
	if len(topics) != 1 || topics[0] != "workspace:42" {
		t.Fatalf("应发到 workspace:42 一次，实际 %v", topics)
	}
	if payloads[0] != string(payload) {
		t.Errorf("payload 应原样，实际 %s", payloads[0])
	}
}
