//go:build integration

// 服务端追加段的长度判定与计价（MKT-T-21、MKD-17）。
//
// ⚠️ 附加段**算长度、不算钱**：不算长度的话四段营销片段 + 约束段把输入撑过模型上限时，
// 受理照样扣费排队、上游才回 400；算进钱的话 per_kchar 的文本模型会因为平台加的约束段
// 多收用户的钱。两件事必须分开钉住。
package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
)

// seedKcharModel 建一个按千字计价、带输入上限的文本模型。
func seedKcharModel(t *testing.T, pool *pgxpool.Pool, next func() int64, maxChars int) string {
	t.Helper()
	code := seedModel(t, pool, next, modelSeed{code: "kchar-model", providerStatus: "enabled",
		modelStatus: "enabled", domain: "text", op: "text2text", opEnabled: true})
	mustExec(t, pool, `update provider_config set auth_type = 'none' where code = 'p-kchar-model'`)
	mustExec(t, pool, `update model set max_input_chars = $1 where code = $2`, maxChars, code)
	mustExec(t, pool, `insert into model_input_capability (id, model_id, operation_id, input_role, min_count, max_count)
		select $1, m.id, o.id, 'text', 0, 8 from model m, operation o
		where m.code = $2 and o.code = 'text2text'`, next(), code)
	mustExec(t, pool, `insert into pricing (id, operation_id, model_id, credits, unit, status)
		select $1, o.id, m.id, 3, 'per_kchar', 'enabled' from operation o, model m
		where o.code = 'text2text' and m.code = $2`, next(), code)
	return code
}

func TestPricing_长度含附加段(t *testing.T) {
	r, pool, next := setupCatalog(t)
	ctx := context.Background()
	code := seedKcharModel(t, pool, next, 500)
	svc := catalogapp.New(r, next)
	text := strings.Repeat("字", 400)

	// 只看用户描述：400 ≤ 500，放行。
	if _, err := svc.QuoteWithFacts(ctx, catalogapp.QuoteRequest{ModelCode: code,
		OperationCode: "text2text", Count: 1, Text: text}); err != nil {
		t.Fatalf("400 字应在上限内：%v", err)
	}
	// 加上 101 字附加段：501 > 500，扣费前拒，并说清附加段占了多少。
	_, err := svc.QuoteWithFacts(ctx, catalogapp.QuoteRequest{ModelCode: code,
		OperationCode: "text2text", Count: 1, Text: text, AppendixChars: 101})
	if codeOf(err) != errcode.InvalidArgument {
		t.Fatalf("附加段使总长超上限时应 40001，得到 %v", err)
	}
	if !strings.Contains(err.Error(), "501") || !strings.Contains(err.Error(), "101") {
		t.Fatalf("错误应说清总长与附加段字数：%v", err)
	}
	// 恰好等于上限放行（> 不是 >=）。
	if _, err := svc.QuoteWithFacts(ctx, catalogapp.QuoteRequest{ModelCode: code,
		OperationCode: "text2text", Count: 1, Text: text, AppendixChars: 100}); err != nil {
		t.Fatalf("恰好 500 字应放行：%v", err)
	}
}

func TestPricing_perKchar不含附加段(t *testing.T) {
	r, pool, next := setupCatalog(t)
	ctx := context.Background()
	code := seedKcharModel(t, pool, next, 100000)
	svc := catalogapp.New(r, next)
	text := strings.Repeat("字", 900) // ceil(900/1000)=1 千字

	base, err := svc.QuoteWithFacts(ctx, catalogapp.QuoteRequest{ModelCode: code,
		OperationCode: "text2text", Count: 1, Text: text})
	if err != nil {
		t.Fatal(err)
	}
	// 附加段 650 字：按长度算会跨到 2 千字，但计价不含它。
	with, err := svc.QuoteWithFacts(ctx, catalogapp.QuoteRequest{ModelCode: code,
		OperationCode: "text2text", Count: 1, Text: text, AppendixChars: 650})
	if err != nil {
		t.Fatal(err)
	}
	if base.Credits != 3 || with.Credits != base.Credits {
		t.Fatalf("per_kchar 不应因附加段变化：无附加段 %d，有附加段 %d", base.Credits, with.Credits)
	}
}
