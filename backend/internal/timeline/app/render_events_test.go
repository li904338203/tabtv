package app

import (
	"encoding/json"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/li904338203/tabtv/backend/internal/timeline/repo"
)

// render.update 的载荷逐字段对 schema（api/events/render-update.schema.json，SC-AC-24）：
// 必填都在（含 projectId）、没有多出来的字段、枚举值在表里、ID 是数字串、output / error 的内层形状对。
func TestRenderUpdate_与事件契约一致(t *testing.T) {
	raw, err := os.ReadFile("../../../../api/events/render-update.schema.json")
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
	idPattern := regexp.MustCompile(s.Defs["id"].Pattern)
	var nested struct {
		Properties struct {
			Output struct {
				Required   []string                   `json:"required"`
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"output"`
			Error struct {
				Required   []string                   `json:"required"`
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"error"`
		} `json:"properties"`
	}
	_ = json.Unmarshal(raw, &nested)

	now := time.Date(2026, 10, 28, 9, 30, 0, 0, time.UTC)
	base := repo.RenderJob{ID: 31, WorkspaceID: 9, ProjectID: 77, TimelineID: 11, TimelineVersion: 4}
	cases := map[string]repo.RenderJob{}
	q := base
	q.Status = "queued"
	cases["受理"] = q
	r := base
	r.Status, r.Stage, r.Progress = "running", "mixing", 25
	cases["进行中"] = r
	ok := base
	ok.Status, ok.Progress, ok.OutputMediaID = "success", 100, 555
	cases["成功"] = ok
	f := base
	f.Status, f.ErrorMessage = "failed", "ffmpeg 失败"
	cases["失败"] = f
	c := base
	c.Status = "cancelled"
	cases["取消"] = c
	for name, j := range cases {
		t.Run(name, func(t *testing.T) {
			b, err := json.Marshal(NewRenderUpdate(j, now))
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatal(err)
			}
			for _, k := range s.Required {
				if _, ok := m[k]; !ok {
					t.Errorf("缺少契约必填字段 %q", k)
				}
			}
			for k, v := range m {
				p, ok := s.Properties[k]
				if !ok {
					t.Errorf("多出契约里没有的字段 %q", k)
					continue
				}
				if p.Const != nil && v != p.Const {
					t.Errorf("%s 应为 %v，得到 %v", k, p.Const, v)
				}
				if len(p.Enum) > 0 && !inEnum(v, p.Enum) {
					t.Errorf("%s 的值 %v 不在枚举里 %v", k, v, p.Enum)
				}
			}
			for _, k := range []string{"renderJobId", "workspaceId", "projectId", "timelineId"} {
				if v, _ := m[k].(string); !idPattern.MatchString(v) {
					t.Errorf("%s 应为数字串：%v", k, m[k])
				}
			}
			if out, ok := m["output"].(map[string]any); ok {
				for _, k := range nested.Properties.Output.Required {
					if _, ok := out[k]; !ok {
						t.Errorf("output 缺 %q", k)
					}
				}
				for k := range out {
					if _, ok := nested.Properties.Output.Properties[k]; !ok {
						t.Errorf("output 多出 %q", k)
					}
				}
			}
			if e, ok := m["error"].(map[string]any); ok {
				for _, k := range nested.Properties.Error.Required {
					if _, ok := e[k]; !ok {
						t.Errorf("error 缺 %q", k)
					}
				}
			}
		})
	}
	if ev := NewRenderUpdate(cases["成功"], now); ev.Output == nil || ev.Output.URL != "/api/v1/media/555" {
		t.Fatalf("成功时带成片（受保护地址，不是签名直链）：%+v", ev.Output)
	}
}
