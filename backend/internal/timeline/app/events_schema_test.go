package app

import (
	"encoding/json"
	"os"
	"testing"
)

// 智能剪辑新增 / 改动的三个事件 schema 自检（docs/agent/smartcut SCT-01）。
//
// 这里只验 schema 文件本身：能解析、`type` 常量对、required 都在 properties 里、
// additionalProperties 为 false。载荷与 schema 逐字段核对放在各自的发布方旁边
// （SCT-06 的 timeline.updated、SCT-07 / 27 的 smartcut.updated、SCT-15 的 render.update），
// 照 generation/app/events_test.go 的做法：直接读这些文件。

type eventSchema struct {
	Required             []string                   `json:"required"`
	Properties           map[string]json.RawMessage `json:"properties"`
	AdditionalProperties *bool                      `json:"additionalProperties"`
}

func TestEventSchemas_智能剪辑三个事件自身合法(t *testing.T) {
	cases := []struct {
		file, typ string
		mustHave  []string
	}{
		{"timeline-updated.schema.json", "timeline.updated",
			[]string{"projectId", "timelineId", "version", "reason", "editorKind"}},
		{"smartcut-updated.schema.json", "smartcut.updated",
			[]string{"projectId", "timelineId", "canvasNodeId"}},
		// ⚠️ render.update 要投到 project:{id}，而 render_job 没有 project_id：发布方要经时间线取（SCD-19）。
		{"render-update.schema.json", "render.update",
			[]string{"projectId", "renderJobId", "timelineId", "status", "progress"}},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			raw, err := os.ReadFile("../../../../api/events/" + c.file)
			if err != nil {
				t.Fatal(err)
			}
			var s eventSchema
			if err := json.Unmarshal(raw, &s); err != nil {
				t.Fatalf("schema 不是合法 JSON：%v", err)
			}
			if s.AdditionalProperties == nil || *s.AdditionalProperties {
				t.Error("事件 schema 必须 additionalProperties:false，否则多出来的字段没人发现")
			}
			for _, k := range s.Required {
				if _, ok := s.Properties[k]; !ok {
					t.Errorf("required 里的 %q 不在 properties 里", k)
				}
			}
			for _, k := range c.mustHave {
				found := false
				for _, r := range s.Required {
					found = found || r == k
				}
				if !found {
					t.Errorf("%q 应当是必填字段", k)
				}
			}
			var typ struct {
				Const string `json:"const"`
			}
			if err := json.Unmarshal(s.Properties["type"], &typ); err != nil || typ.Const != c.typ {
				t.Errorf("type 常量应为 %q，实际 %q（%v）", c.typ, typ.Const, err)
			}
		})
	}
}

// render.update 的阶段要覆盖音频预渲（mixing）与导出自检（checking），与 RenderJob.stage 同一组值。
func TestEventSchemas_渲染阶段含预渲与自检(t *testing.T) {
	raw, err := os.ReadFile("../../../../api/events/render-update.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Properties struct {
			Stage struct {
				Enum []any `json:"enum"`
			} `json:"stage"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"probing": false, "assembling": false, "mixing": false,
		"encoding": false, "archiving": false, "checking": false}
	for _, v := range s.Properties.Stage.Enum {
		if str, ok := v.(string); ok {
			if _, known := want[str]; known {
				want[str] = true
			}
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("render.update 的 stage 缺少 %q", k)
		}
	}
}
