//go:build integration

// 型号级参数覆盖（model_param_support，参数合并的第三层）的真库测试（P15-01）。
//
// ⚠️ 起因：这张表建好之后一直没有端点读它，画布视频节点永远是通用的时长 / 清晰度 / 画幅。
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/catalog/app"
	"github.com/li904338203/tabtv/backend/internal/catalog/repo"
)

func TestModelParams_按模型下发第三层覆盖(t *testing.T) {
	r, pool, next := setupCatalog(t)
	ctx := context.Background()

	seedModel(t, pool, next, modelSeed{code: "vid", providerStatus: "enabled",
		modelStatus: "enabled", domain: "video", op: "text2video", opEnabled: true})
	seedModel(t, pool, next, modelSeed{code: "bare", providerStatus: "enabled",
		modelStatus: "enabled", domain: "video", op: "text2video", opEnabled: true})

	param := func(code string, enabled bool) int64 {
		t.Helper()
		id := next()
		if _, err := pool.Exec(ctx, `
			insert into param_def (id, code, name, control_type, config, is_enabled)
			values ($1, $2, $2, 'segmented', '{"options":[{"label":"x","value":"x"}]}', $3)`,
			id, code, enabled); err != nil {
			t.Fatal(err)
		}
		return id
	}
	support := func(paramID int64, allowed, def, override any) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			insert into model_param_support (id, model_id, param_def_id, allowed_values, default_value, override)
			values ($1, (select id from model where code = 'vid'), $2, $3, $4, $5)`,
			next(), paramID, allowed, def, override); err != nil {
			t.Fatal(err)
		}
	}
	support(param("duration", true), `["4","15"]`, "4",
		`{"unit":"秒","options":[{"label":"4 秒","value":"4"},{"label":"15 秒","value":"15"}]}`)
	support(param("resolution", true), `["720p"]`, "1080p", nil)
	// 停用的参数定义不下发：节点上已经不渲染它
	support(param("legacy", false), nil, "a", `{"options":["a"]}`)

	rows, err := r.ListModelParamSupports(ctx, "vid")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].ParamCode != "duration" || rows[1].ParamCode != "resolution" {
		t.Fatalf("应只列启用参数的两行（按 code 排序），得到 %+v", rows)
	}

	if empty, err := r.ListModelParamSupports(ctx, "bare"); err != nil || len(empty) != 0 {
		t.Fatalf("模型存在但没配覆盖时应是空列表且不报错，得到 %+v / %v", empty, err)
	}
	if _, err := r.ListModelParamSupports(ctx, "nope"); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("模型不存在应 ErrNotFound，得到 %v", err)
	}

	got, err := app.New(r, next).ModelParams(ctx, "vid")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	want := `{"duration":{"default":"4","options":[{"label":"4 秒","value":"4"},{"label":"15 秒","value":"15"}],"unit":"秒"},` +
		`"resolution":{"options":[{"label":"720p","value":"720p"}]}}`
	if string(b) != want {
		// resolution 的 default_value=1080p 不在可选项里，必须被丢掉（否则前端切模型时会把它写进参数）
		t.Fatalf("下发的覆盖不对：\n得到 %s\n期望 %s", b, want)
	}
}

// 运镜：按节点上选的 code 取运镜目录里这一项的提示词；下线的运镜在扣费前报错（不静默按没选处理）。
func TestCameraPrompt_按运镜目录取词(t *testing.T) {
	r, pool, next := setupCatalog(t)
	ctx := context.Background()
	svc := app.New(r, next)

	if got, err := svc.CameraPrompt(ctx, json.RawMessage(`{"prompt":"一只猫"}`)); err != nil || got != "" {
		t.Fatalf("没选运镜时应是空串，得到 %q / %v", got, err)
	}
	if _, err := pool.Exec(ctx, `
		insert into camera_move (id, code, name, prompt, sort, is_enabled)
		values ($1, 'push_in', '镜头前推', ' 运镜：镜头平稳地向前推进。 ', 10, true),
		       ($2, 'roll', '滚筒旋转', '运镜：镜头旋转。', 20, false)`, next(), next()); err != nil {
		t.Fatal(err)
	}
	got, err := svc.CameraPrompt(ctx, json.RawMessage(`{"camera":{"code":"push_in","name":"镜头前推"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got != "运镜：镜头平稳地向前推进。" {
		t.Fatalf("得到 %q", got)
	}
	if _, err := svc.CameraPrompt(ctx, json.RawMessage(`{"camera":{"code":"roll","name":"滚筒旋转"}}`)); err == nil ||
		!strings.Contains(err.Error(), "滚筒旋转") {
		t.Fatalf("下线的运镜应报错并说出是哪一个，得到 %v", err)
	}
	// 收藏放开了 camera 这个取值（ck_favorite_type）
	if on, err := svc.ToggleFavorite(ctx, 42, "camera", "push_in", true); err != nil || !on {
		t.Fatalf("运镜应能收藏，得到 %v / %v", on, err)
	}
	list, err := svc.ListMaterials(ctx, "camera", 42, app.MaterialFilter{FavoritedOnly: true}, true)
	if err != nil || len(list) != 1 || list[0].Code != "push_in" {
		t.Fatalf("我的收藏应只列 push_in，得到 %+v / %v", list, err)
	}
}
