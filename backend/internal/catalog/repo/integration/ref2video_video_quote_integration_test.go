//go:build integration

// 全能参考收参考视频（3D 导演台 DRT-08，DRK-12，DRD-21）。
//
// 操作层与连线层都放开了 video，正确性完全靠能力矩阵：它是必配的，某个模型没声明 video 角色时，
// 请求里一出现参考视频，**报价阶段**就要拦下（42203）—— 不拦的话受理照样扣费排队，上游才回 400。
package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	catalogdomain "github.com/li904338203/tabtv/backend/internal/catalog/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
)

// seedRef2VideoModel 建一个绑定 ref2video 的视频模型，能力矩阵按 roles 声明（每种 0~4 个）。
func seedRef2VideoModel(t *testing.T, pool *pgxpool.Pool, next func() int64, code string, roles ...string) string {
	t.Helper()
	code = seedModel(t, pool, next, modelSeed{code: code, providerStatus: "enabled",
		modelStatus: "enabled", domain: "video", op: "ref2video", opEnabled: true})
	mustExec(t, pool, `update provider_config set auth_type = 'none' where code = $1`, "p-"+code)
	for _, role := range roles {
		mustExec(t, pool, `insert into model_input_capability (id, model_id, operation_id, input_role, min_count, max_count)
			select $1, m.id, o.id, $3, 0, 4 from model m, operation o
			where m.code = $2 and o.code = 'ref2video'`, next(), code, role)
	}
	mustExec(t, pool, `insert into pricing (id, operation_id, model_id, credits, unit, status)
		select $1, o.id, m.id, 10, 'per_time', 'enabled' from operation o, model m
		where o.code = 'ref2video' and m.code = $2`, next(), code)
	return code
}

func ref2videoQuote(code string) catalogapp.QuoteRequest {
	return catalogapp.QuoteRequest{ModelCode: code, OperationCode: "ref2video", Count: 1, Text: "按参考视频的走位出片",
		RoleCounts: catalogdomain.RoleCount{catalogdomain.RoleText: 1, catalogdomain.RoleImage: 1, catalogdomain.RoleVideo: 1}}
}

func TestQuote_全能参考视频角色未声明被拦(t *testing.T) {
	r, pool, next := setupCatalog(t)
	code := seedRef2VideoModel(t, pool, next, "ref-no-video", "text", "image", "audio")
	_, err := catalogapp.New(r, next).QuoteWithFacts(context.Background(), ref2videoQuote(code))
	if codeOf(err) != errcode.CapabilityUnsupported {
		t.Fatalf("没声明 video 角色的模型应在报价阶段 42203，得到 %v", err)
	}
	if !strings.Contains(err.Error(), "video") {
		t.Errorf("原因要写明不接受 video：%v", err)
	}
}

func TestQuote_全能参考视频角色已声明放行(t *testing.T) {
	r, pool, next := setupCatalog(t)
	code := seedRef2VideoModel(t, pool, next, "ref-with-video", "text", "image", "audio", "video")
	q, err := catalogapp.New(r, next).QuoteWithFacts(context.Background(), ref2videoQuote(code))
	if err != nil {
		t.Fatalf("声明了 video 角色的模型应放行：%v", err)
	}
	if q.Credits != 10 {
		t.Errorf("按次计价 10 积分，得到 %d", q.Credits)
	}
}
