//go:build integration

// 3D 导演台 AI 搭景的模型选择（docs/agent/director，DRT-19，DRD-22）。
//
// 免费靠配置成立：scene_build 照常走能力合取式，兜底价 0 积分；改成非 0 时显式拒绝。
package integration

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	"github.com/li904338203/tabtv/backend/internal/catalog/repo"
)

// sceneBuildModel 建一个绑了 scene_build 的看图文本模型；images > 0 时在 scene_build 上声明 image 0..images。
func sceneBuildModel(t *testing.T, pool *pgxpool.Pool, next func() int64, code string, images int) string {
	t.Helper()
	code = seedModel(t, pool, next, modelSeed{code: code, providerStatus: "enabled", modelStatus: "enabled",
		domain: "text", op: repo.OpSceneBuild, opEnabled: true})
	mustExec(t, pool, `update provider_config set auth_type = 'none' where code = $1`, "p-"+code)
	declare(t, pool, next, code, repo.OpSceneBuild, "text", 1, 1)
	if images > 0 {
		declare(t, pool, next, code, repo.OpSceneBuild, "image", 0, images)
	}
	return code
}

func declare(t *testing.T, pool *pgxpool.Pool, next func() int64, model, op, role string, min, max int) {
	t.Helper()
	mustExec(t, pool, `insert into model_input_capability (id, model_id, operation_id, input_role, min_count, max_count)
		select $1, m.id, o.id, $3, $5, $6 from model m, operation o where m.code = $2 and o.code = $4`,
		next(), model, role, op, min, max)
}

func sceneBuildFallbackPrice(t *testing.T, pool *pgxpool.Pool, next func() int64, credits int) {
	t.Helper()
	mustExec(t, pool, `insert into pricing (id, operation_id, model_id, credits, unit, status)
		select $1, o.id, null, $2, 'per_time', 'enabled' from operation o where o.code = 'scene_build'
		on conflict (operation_id, model_id) do update set credits = excluded.credits`, next(), credits)
}

func TestListModelsForOperation_scene_build(t *testing.T) {
	r, pool, next := setupCatalog(t)
	ctx := context.Background()
	ok := sceneBuildModel(t, pool, next, "vision-ok", 2)
	off := sceneBuildModel(t, pool, next, "vision-off", 2)
	mustExec(t, pool, `update model set status = 'disabled' where code = $1`, off)
	seedModel(t, pool, next, modelSeed{code: "text-only", providerStatus: "enabled", modelStatus: "enabled",
		domain: "text", op: "text2text", opEnabled: true})

	got, err := r.ListModelsForOperation(ctx, repo.OpSceneBuild)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != ok {
		t.Fatalf("只列启用的、绑了 scene_build 的模型，得到 %v", got)
	}
}

// ⚠️ 同 agent_chat：只为搭景绑的看图模型出现在文本节点的下拉里的话，选了却报不出 text2text 的价。
func TestListUsableModels_不列scene_build专用模型(t *testing.T) {
	r, pool, next := setupCatalog(t)
	ctx := context.Background()
	only := sceneBuildModel(t, pool, next, "vision-only", 2)
	both := sceneBuildModel(t, pool, next, "vision-both", 2)
	bindOperation(t, pool, next, both, "text2text")

	list, err := r.ListUsableModels(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	codes := []string{}
	for _, m := range list {
		codes = append(codes, m.Code)
	}
	if slices.Contains(codes, only) {
		t.Fatalf("只绑了 scene_build 的模型不该出现在 /models 里：%v", codes)
	}
	if !slices.Contains(codes, both) {
		t.Fatalf("另外绑了画布操作的模型照常出现：%v", codes)
	}
}

func TestSceneBuildPricing_兜底价为0(t *testing.T) {
	r, pool, next := setupCatalog(t)
	ctx := context.Background()
	svc := catalogapp.New(r, next)
	code := sceneBuildModel(t, pool, next, "vision-1", 2)

	// 没有价格：与别的能力一样，报「缺少可用价格」（不可用，要运营处理）。
	if _, u, err := svc.SceneBuildChoice(ctx, 0, 0); err != nil || u == nil || u.UserFixable {
		t.Fatalf("没有价格时应不可用且不是用户能解决的，得到 %+v %v", u, err)
	}

	sceneBuildFallbackPrice(t, pool, next, 0)
	c, u, err := svc.SceneBuildChoice(ctx, 0, 1)
	if err != nil || u != nil {
		t.Fatalf("0 积分兜底价时应可用，得到 %+v %v", u, err)
	}
	if c.ModelCode != code || c.MaxImages != 2 {
		t.Fatalf("应选中 %s、参考图上限 2，得到 %+v", code, c)
	}
	// 张数超过模型声明的上限：这个模型不通过（能力矩阵在报价时就拦）。
	if _, u, _ := svc.SceneBuildChoice(ctx, 0, 3); u == nil {
		t.Fatal("参考图超过模型上限时不该选中它")
	}

	// 运营改成非 0：显式拒绝，原因写明按约定免费（DRD-22），不悄悄照收或悄悄忽略。
	sceneBuildFallbackPrice(t, pool, next, 5)
	if _, u, err := svc.SceneBuildChoice(ctx, 0, 0); err != nil || u == nil || !strings.Contains(u.Reason, "按约定免费") {
		t.Fatalf("价格大于 0 时应不可用并写明按约定免费，得到 %+v %v", u, err)
	}

	// 解绑全部 scene_build 模型（DR-AC-32）。
	sceneBuildFallbackPrice(t, pool, next, 0)
	mustExec(t, pool, `update model_operation set is_enabled = false`)
	if _, u, _ := svc.SceneBuildChoice(ctx, 0, 0); u == nil || !strings.Contains(u.Reason, "绑定") {
		t.Fatalf("没有绑定模型时应不可用并说明，得到 %+v", u)
	}
}

// 输入能力沿用同一模型的 text2text（CapabilityInheritance）：运营不必为搭景再配一份能力矩阵。
func TestSceneBuild_能力矩阵继承text2text(t *testing.T) {
	r, pool, next := setupCatalog(t)
	ctx := context.Background()
	svc := catalogapp.New(r, next)
	code := seedModel(t, pool, next, modelSeed{code: "vision-inherit", providerStatus: "enabled", modelStatus: "enabled",
		domain: "text", op: "text2text", opEnabled: true})
	mustExec(t, pool, `update provider_config set auth_type = 'none' where code = $1`, "p-"+code)
	bindOperation(t, pool, next, code, repo.OpSceneBuild)
	declare(t, pool, next, code, "text2text", "text", 1, 1)
	declare(t, pool, next, code, "text2text", "image", 0, 3)
	sceneBuildFallbackPrice(t, pool, next, 0)

	c, u, err := svc.SceneBuildChoice(ctx, 0, 2)
	if err != nil || u != nil || c.ModelCode != code || c.MaxImages != 3 {
		t.Fatalf("scene_build 没声明能力时应沿用 text2text 的（图片上限 3），得到 %+v %+v %v", c, u, err)
	}
}
