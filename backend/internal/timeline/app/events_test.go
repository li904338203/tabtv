package app

import (
	"encoding/json"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/li904338203/tabtv/backend/internal/timeline/repo"
)

// timeline.updated 的载荷逐字段对 schema（api/events/timeline-updated.schema.json）：
// 必填都在、没有多出来的字段、枚举值在表里、ID 是数字串。事件没有代码生成，靠这里绊住。

type eventProp struct {
	Enum  []any `json:"enum"`
	Const any   `json:"const"`
}

func loadTimelineUpdatedSchema(t *testing.T) (map[string]eventProp, []string, *regexp.Regexp) {
	t.Helper()
	raw, err := os.ReadFile("../../../../api/events/timeline-updated.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Required   []string             `json:"required"`
		Properties map[string]eventProp `json:"properties"`
		Defs       map[string]struct {
			Pattern string `json:"pattern"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s.Properties, s.Required, regexp.MustCompile(s.Defs["id"].Pattern)
}

func TestTimelineUpdated_与事件契约一致(t *testing.T) {
	props, required, idPattern := loadTimelineUpdatedSchema(t)
	now := time.Date(2026, 10, 28, 9, 30, 0, 0, time.UTC)
	cases := []struct {
		name   string
		tl     repo.Timeline
		reason string
	}{
		{"用户整份保存", repo.Timeline{ID: 11, ProjectID: 77, Version: 3, CanvasNodeID: "n_sc",
			LastEditorUserID: 501, LastEditorKind: "user", LastEditorName: "林澈"}, ReasonSave},
		{"旧时间线逐项编辑", repo.Timeline{ID: 11, ProjectID: 77, Version: 4,
			LastEditorUserID: 501, LastEditorKind: "user"}, ReasonEdit},
		{"编辑者说不清", repo.Timeline{ID: 11, ProjectID: 77, Version: 5}, ReasonAssemble},
		{"快照恢复", repo.Timeline{ID: 11, ProjectID: 77, Version: 6, LastEditorKind: "user", LastEditorUserID: 1}, ReasonRestore},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, err := json.Marshal(NewTimelineUpdated(c.tl, c.reason, now))
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatal(err)
			}
			for _, k := range required {
				if _, ok := m[k]; !ok {
					t.Errorf("缺少契约必填字段 %q", k)
				}
			}
			for k, v := range m {
				p, ok := props[k]
				if !ok {
					t.Errorf("多出契约里没有的字段 %q（schema 是 additionalProperties:false）", k)
					continue
				}
				if p.Const != nil && v != p.Const {
					t.Errorf("%s 应为 %v，得到 %v", k, p.Const, v)
				}
				if len(p.Enum) > 0 && !inEnum(v, p.Enum) {
					t.Errorf("%s 的值 %v 不在契约的枚举里 %v", k, v, p.Enum)
				}
			}
			for _, k := range []string{"projectId", "timelineId", "editorId"} {
				if s, ok := m[k].(string); ok && !idPattern.MatchString(s) {
					t.Errorf("%s 应是数字串，得到 %q", k, s)
				}
			}
			if v, _ := m["version"].(float64); int(v) != c.tl.Version {
				t.Errorf("version 应为 %d", c.tl.Version)
			}
		})
	}
	// 编辑者说不清时 editorKind 记 system（schema 要求一个确定的来源），editorId / editorName 为 null。
	ev := NewTimelineUpdated(repo.Timeline{ID: 1, ProjectID: 2, Version: 1}, ReasonEdit, now)
	if ev.EditorKind != repo.EditorSystem || ev.EditorID != nil || ev.EditorName != nil || ev.CanvasNodeID != nil {
		t.Errorf("编辑者说不清时：%+v", ev)
	}
}

// 写入原因的常量与 schema 的枚举一一对应：多一个少一个都会让某条写路径的事件过不了校验。
func TestTimelineUpdated_原因与契约枚举一致(t *testing.T) {
	props, _, _ := loadTimelineUpdatedSchema(t)
	ours := []any{ReasonSave, ReasonEdit, ReasonPlan, ReasonSync, ReasonRestore, ReasonClipFill,
		ReasonAssemble, ReasonSubtitles, ReasonTranslate}
	if len(ours) != len(props["reason"].Enum) {
		t.Fatalf("代码里 %d 个原因，契约里 %d 个", len(ours), len(props["reason"].Enum))
	}
	for _, r := range ours {
		if !inEnum(r, props["reason"].Enum) {
			t.Errorf("原因 %v 不在契约里", r)
		}
	}
}

func inEnum(v any, enum []any) bool {
	for _, e := range enum {
		if e == v {
			return true
		}
	}
	return false
}
