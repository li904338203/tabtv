//go:build integration

// 规划时新钉的选项并进 scene_input.pins（MKT-T-20、MKD-5）：同会话同 code 只认第一次的版本，
// 库里再兜一次底；scene_input 的其余键原样保留；通用会话改不到。
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
)

func TestAppendScenePins_同code只认第一次(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	si := json.RawMessage(`{"products":["11","12"],"product":{"name":"保温杯","sellingPoints":"12 小时"},` +
		`"characters":[],"sourceVideo":null,"picks":{"creative":{"code":"cr_a","version":1,"name":"痛点"},"hook":null,"style":null},` +
		`"pins":[],"future":"保留"}`)
	s, err := r.Create(ctx, pool, repo.Session{ID: next(), WorkspaceID: ws, ProjectID: project, PayerAccountID: payer,
		CreatedByUserID: user, Title: "营销", Scene: domain.SceneMarketing, SceneInput: si})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.AppendScenePins(ctx, s.ID, json.RawMessage(`[{"kind":"hook","code":"hk_q","version":2,"source":"plan"}]`)); err != nil {
		t.Fatal(err)
	}
	// 第二次带着同一个 code 的新版本 + 一个新 code：前者不覆盖，后者追加。
	if err := r.AppendScenePins(ctx, s.ID, json.RawMessage(`[{"kind":"hook","code":"hk_q","version":3,"source":"plan"},`+
		`{"kind":"style","code":"st_w","version":1,"source":"plan"}]`)); err != nil {
		t.Fatal(err)
	}
	got, err := r.GetByID(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Products []string `json:"products"`
		Future   string   `json:"future"`
		Pins     []struct {
			Kind, Code string
			Version    int
		} `json:"pins"`
	}
	if err := json.Unmarshal(got.SceneInput, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Pins) != 2 || out.Pins[0].Code != "hk_q" || out.Pins[0].Version != 2 || out.Pins[1].Code != "st_w" {
		t.Fatalf("同 code 只认第一次的版本、新 code 追加：%+v", out.Pins)
	}
	if len(out.Products) != 2 || out.Future != "保留" {
		t.Fatalf("scene_input 其余键应原样保留：%s", got.SceneInput)
	}

	// 通用会话没有 pins：改不到。
	g := mustSession(t, r, pool, next)
	if err := r.AppendScenePins(ctx, g.ID, json.RawMessage(`[{"kind":"hook","code":"hk_q","version":2}]`)); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("通用会话应 ErrNotFound：%v", err)
	}
}

// 04 文档 P-46：受理拼营销片段前，只放行本项目营销会话确认过的选项版本。
func TestPickPinnedInProject_只认本项目营销会话确认过的版本(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	si := json.RawMessage(`{"products":["11"],"characters":[],"sourceVideo":null,` +
		`"picks":{"creative":{"code":"cr_a","version":2,"name":"痛点"},"hook":null,"style":null},` +
		`"pins":[{"kind":"hook","code":"hk_q","version":3,"source":"plan"}]}`)
	if _, err := r.Create(ctx, pool, repo.Session{ID: next(), WorkspaceID: ws, ProjectID: project, PayerAccountID: payer,
		CreatedByUserID: user, Title: "营销", Scene: domain.SceneMarketing, SceneInput: si}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name       string
		project    int64
		kind, code string
		version    int
		want       bool
	}{
		{"建会话时选的", project, "creative", "cr_a", 2, true},
		{"规划时钉的", project, "hook", "hk_q", 3, true},
		{"版本对不上", project, "creative", "cr_a", 1, false},
		{"类型对不上", project, "hook", "cr_a", 2, false},
		{"没确认过的选项（如草稿）", project, "style", "st_draft", 1, false},
		{"别的项目", project + 1, "creative", "cr_a", 2, false},
	}
	for _, c := range cases {
		got, err := r.PickPinnedInProject(ctx, ws, c.project, c.kind, c.code, c.version)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("%s：得到 %v", c.name, got)
		}
	}
	// 通用会话（不是营销场景）里的同名数据不算。
	g, err := r.Create(ctx, pool, repo.Session{ID: next(), WorkspaceID: ws, ProjectID: project + 2, PayerAccountID: payer,
		CreatedByUserID: user, Title: "通用", SceneInput: si})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := r.PickPinnedInProject(ctx, ws, g.ProjectID, "creative", "cr_a", 2); got {
		t.Fatal("通用会话不算营销会话确认过")
	}
}
