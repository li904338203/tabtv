//go:build integration

// 营销会话按模型上限选商品图（MKT-T-20、MKR-2）：上限取自**报价用的那份能力矩阵**。
//
// ⚠️ 另读一份的话，「卡片上说最多 3 张」与「受理时能力矩阵说最多 2 张」会对不上，
// 而两边各自看都没错。这里钉住：InputLimit 给的数 = 报价能放行的最大张数。
package integration

import (
	"context"
	"testing"

	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	catalogdomain "github.com/li904338203/tabtv/backend/internal/catalog/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
)

func TestInputLimit_与报价能力矩阵同一份(t *testing.T) {
	r, pool, next := setupCatalog(t)
	ctx := context.Background()
	code := seedModel(t, pool, next, modelSeed{code: "img-limit", providerStatus: "enabled",
		modelStatus: "enabled", domain: "image", op: "img2img", opEnabled: true})
	mustExec(t, pool, `update provider_config set auth_type = 'none' where code = 'p-img-limit'`)
	mustExec(t, pool, `insert into model_input_capability (id, model_id, operation_id, input_role, min_count, max_count)
		select $1, m.id, o.id, 'image', 1, 3 from model m, operation o
		where m.code = $2 and o.code = 'img2img'`, next(), code)
	mustExec(t, pool, `insert into pricing (id, operation_id, model_id, credits, unit, status)
		select $1, o.id, m.id, 4, 'per_image', 'enabled' from operation o, model m
		where o.code = 'img2img' and m.code = $2`, next(), code)
	svc := catalogapp.New(r, next)

	max, found, err := svc.InputLimit(ctx, code, "img2img", catalogdomain.RoleImage)
	if err != nil || !found || max != 3 {
		t.Fatalf("image 上限应为 3：max=%d found=%v err=%v", max, found, err)
	}
	quote := func(n int) error {
		_, err := svc.QuoteWithFacts(ctx, catalogapp.QuoteRequest{ModelCode: code, OperationCode: "img2img",
			Count: 1, RoleCounts: catalogdomain.RoleCount{catalogdomain.RoleImage: n}})
		return err
	}
	if err := quote(max); err != nil {
		t.Fatalf("恰好上限张数应报得出价：%v", err)
	}
	if err := quote(max + 1); codeOf(err) != errcode.CapabilityUnsupported {
		t.Fatalf("上限 +1 张应被报价的能力矩阵拦下（42203），得到 %v", err)
	}

	// 声明了矩阵但没有这个角色 = 不收（0, found）；模型 × 操作查不到 = found=false（交给报价说明原因）。
	if max, found, err := svc.InputLimit(ctx, code, "img2img", catalogdomain.RoleVideo); err != nil || !found || max != 0 {
		t.Fatalf("没声明的角色应为 (0, true)：%d %v %v", max, found, err)
	}
	if _, found, err := svc.InputLimit(ctx, code, "text2video", catalogdomain.RoleImage); err != nil || found {
		t.Fatalf("没绑定的操作应 found=false：%v %v", found, err)
	}
	if _, found, err := svc.InputLimit(ctx, "no-such-model", "img2img", catalogdomain.RoleImage); err != nil || found {
		t.Fatalf("不存在的模型应 found=false：%v %v", found, err)
	}
}
