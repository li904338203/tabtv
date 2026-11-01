//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
)

// agent_chat 用的就是画布里的文本模型：它自己没声明输入能力时沿用同模型的 text2text。
// 不沿用的话「只绑了 agent_chat + 配了价」的推理模型报价一律 42203，
// 输入框里推理模型永远是灰的、发送键点不动（2026-10-16 实测）。
func TestQuote_agentChat未声明能力时沿用text2text(t *testing.T) {
	r, pool, next := setupCatalog(t)
	ctx := context.Background()
	code := seedModel(t, pool, next, modelSeed{code: "chat-model", providerStatus: "enabled",
		modelStatus: "enabled", domain: "text", op: "text2text", opEnabled: true})
	bindOperation(t, pool, next, code, "agent_chat")
	mustExec(t, pool, `update provider_config set auth_type = 'none' where code = 'p-chat-model'`)
	mustExec(t, pool, `insert into pricing (id, operation_id, model_id, credits, unit, status)
		select $1, o.id, m.id, 36, 'per_time', 'enabled' from operation o, model m
		where o.code = 'agent_chat' and m.code = 'chat-model'`, next())
	svc := catalogapp.New(r, next)
	req := catalogapp.QuoteRequest{ModelCode: code, OperationCode: "agent_chat", Count: 1}

	// text2text 也没声明能力：仍然拒（沿用不是放行）。
	if _, err := svc.QuoteWithFacts(ctx, req); codeOf(err) != errcode.CapabilityUnsupported {
		t.Fatalf("两边都没声明能力时应 42203，得到 %v", err)
	}
	// 画布那份声明了：agent_chat 沿用它，按 agent_chat 自己的价格报。
	mustExec(t, pool, `insert into model_input_capability (id, model_id, operation_id, input_role, min_count, max_count)
		select $1, m.id, o.id, 'text', 0, 8 from model m, operation o
		where m.code = 'chat-model' and o.code = 'text2text'`, next())
	d, err := svc.QuoteWithFacts(ctx, req)
	if err != nil {
		t.Fatalf("应沿用 text2text 的能力并报价成功：%v", err)
	}
	if d.Credits != 36 {
		t.Errorf("应按 agent_chat 的价格 36 报价，得到 %+v", d)
	}
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}
