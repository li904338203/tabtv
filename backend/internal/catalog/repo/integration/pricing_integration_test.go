//go:build integration

// 停用 / 删除价格的保护（02 §3.3：下架价格不能让已启用的能力失去可用报价）。
//
// ⚠️ 用真库：判定要跨 model / model_operation / pricing 三张表，
// 而此前的缺口恰恰是「只拦了删兜底价」—— 删掉某个模型唯一的精确价、
// 或把它停用都会放行，之后用户那边一点生成就是「该能力缺少可用价格」。
package integration

import (
	"context"
	"errors"
	"strings"
	"testing"

	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	catalogdomain "github.com/li904338203/tabtv/backend/internal/catalog/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

func codeOf(err error) errcode.Code {
	var e *httpx.BizError
	if errors.As(err, &e) {
		return e.Code
	}
	return 0
}

func price(op, model string, credits int, enabled bool) catalogdomain.Price {
	return catalogdomain.Price{OperationCode: op, ModelCode: model, Credits: credits,
		Unit: catalogdomain.UnitPerImage, Enabled: enabled}
}

func TestPricing_最后一条有效价格删不掉也停不了(t *testing.T) {
	r, pool, next := setupCatalog(t)
	svc := catalogapp.New(r, next)
	ctx := context.Background()
	seedModel(t, pool, next, modelSeed{code: "m-live", providerStatus: "enabled",
		modelStatus: "enabled", domain: "image", op: "text2img", opEnabled: true})
	if err := svc.SavePrice(ctx, 0, price("text2img", "m-live", 8, true)); err != nil {
		t.Fatal(err)
	}

	// 没有兜底价：这条精确价是 m-live 的最后一条有效价格
	err := svc.DeletePrice(ctx, 0, "text2img", "m-live")
	if codeOf(err) != errcode.ResourceInUse || !strings.Contains(err.Error(), "m-live") {
		t.Fatalf("删除应被拒并点名 m-live，实际 %v", err)
	}
	err = svc.SavePrice(ctx, 0, price("text2img", "m-live", 8, false))
	if codeOf(err) != errcode.ResourceInUse {
		t.Fatalf("停用应被拒，实际 %v", err)
	}

	// 列表上提前说清谁在靠它：后台据此在点之前就置灰
	rows, err := svc.ListPrices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || strings.Join(rows[0].InUseBy, ",") != "m-live" {
		t.Fatalf("列表应标出 m-live 在用：%+v", rows)
	}

	// 配上兜底价之后，精确价可以删（m-live 回落兜底，照样报得出价）……
	if err := svc.SavePrice(ctx, 0, price("text2img", "", 10, true)); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeletePrice(ctx, 0, "text2img", "m-live"); err != nil {
		t.Fatalf("有兜底价托底时应可删除：%v", err)
	}
	// ……而兜底价现在成了它唯一的价格
	err = svc.DeletePrice(ctx, 0, "text2img", "")
	if codeOf(err) != errcode.ResourceInUse || !strings.Contains(err.Error(), "m-live") {
		t.Fatalf("删兜底价应被拒并点名 m-live，实际 %v", err)
	}

	// 模型停用之后就没有人靠它了：可以删
	if _, err := pool.Exec(ctx, `update model set status = 'disabled' where code = 'm-live'`); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeletePrice(ctx, 0, "text2img", ""); err != nil {
		t.Fatalf("模型停用后应可删除兜底价：%v", err)
	}
}

// 时间线渲染没有任何模型绑定，只靠兜底价计费。不单独登记的话，删掉它时
// "没有模型受影响"于是放行 —— 而那一刻起所有人都渲不了片子。
func TestPricing_不绑模型的能力的兜底价受保护(t *testing.T) {
	r, pool, next := setupCatalog(t)
	svc := catalogapp.New(r, next)
	ctx := context.Background()
	seedOp(t, pool, next, catalogdomain.OpTimelineRender, "video", "", "video", "enabled")
	render := catalogdomain.Price{OperationCode: catalogdomain.OpTimelineRender,
		Credits: 1, Unit: catalogdomain.UnitPerSecond, Enabled: true}
	if err := svc.SavePrice(ctx, 0, render); err != nil {
		t.Fatal(err)
	}
	if n, err := svc.QuoteOperation(ctx, catalogdomain.OpTimelineRender, 1, 12); err != nil || n != 12 {
		t.Fatalf("渲染报价应为 12 积分：n=%d err=%v", n, err)
	}
	err := svc.DeletePrice(ctx, 0, catalogdomain.OpTimelineRender, "")
	if codeOf(err) != errcode.ResourceInUse || !strings.Contains(err.Error(), "不绑模型") {
		t.Fatalf("删渲染兜底价应被拒并说明是不绑模型的能力，实际 %v", err)
	}
	// 没登记的操作不能走兜底价报价：那会把精确价整个忽略掉
	if _, err := svc.QuoteOperation(ctx, "text2img", 1, 0); err == nil {
		t.Fatal("绑模型的操作不应能走 QuoteOperation")
	}
}
