//go:build integration

// 供应商级并发上限的真库测试（P06-03，补 P05-03 的挂账）。
//
// ⚠️ 必须用真库：上限是「库里此刻有几条 running」，跨 Worker 实例天然一致。
// 用进程内计数器测的话，测的是另一套东西 —— 而那套东西恰恰是不能用的
// （多个 Worker 各算各的，合起来照样超）。
package integration

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/generation/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
)

// seedProviderModel 建一条 Provider + 模型，并设定供应商并发上限。
func seedProviderModel(t *testing.T, pool *pgxpool.Pool,
	providerID, modelID int64, code string, maxConcurrent int) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		insert into api_protocol (code, name, is_async, requires_query_path, is_registered)
		values ('dev_fake','开发假适配',false,false,true)
		on conflict (code) do nothing`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into provider_config (id, code, name, adapter_type, base_url, auth_type,
		    api_key_cipher, connect_timeout_ms, request_timeout_ms, idle_timeout_ms,
		    max_retries, max_concurrent, status)
		values ($1,$2,$2,'http','https://up.example.com','bearer','v1:x',5000,30000,10000,1,$3,'enabled')`,
		providerID, code, maxConcurrent); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into model (id, code, name, provider_id, domain, upstream_model_code,
		    api_protocol, submit_path, est_seconds, status)
		values ($1,$2,$2,$3,'image','up-v1','dev_fake','/v1/images',10,'enabled')`,
		modelID, "m-"+code, providerID); err != nil {
		t.Fatal(err)
	}
}

func insertQueued(t *testing.T, pool *pgxpool.Pool, id, modelID int64, status string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		insert into generation (id, workspace_id, payer_account_id, created_by_user_id,
		    project_id, node_id, operation_id, model_id, result_type,
		    params_snapshot, inputs_snapshot, request_count, credits_cost,
		    idempotency_key, status)
		values ($1,$2,$3,$4,$5,$6,1,$7,'image','{}'::jsonb,'[]'::jsonb,1,0,$8,$9)`,
		id, ws, payer, user, proj, "n1", modelID, "idem-"+itoa(id), status); err != nil {
		t.Fatal(err)
	}
}

func itoa(v int64) string {
	const digits = "0123456789"
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = digits[v%10]
		v /= 10
	}
	return string(buf[i:])
}

func TestClaimForRun_供应商级并发上限跨实例生效(t *testing.T) {
	pool := testsupport.Fresh(t)
	r := repo.New(pool)
	ctx := context.Background()

	// 上限 2：这家上游同时最多跑两个。
	seedProviderModel(t, pool, 9001, 9101, "slow-vendor", 2)
	insertQueued(t, pool, 9201, 9101, "running")
	insertQueued(t, pool, 9202, 9101, "running")
	insertQueued(t, pool, 9203, 9101, "queued")

	// ⚠️ 付费主体级上限设成 0（不限），确保拦下来的确实是**供应商级**那一条。
	_, err := r.ClaimForRun(ctx, 9203, 0)
	if err != repo.ErrConcurrencyLimit {
		t.Fatalf("达到供应商上限应返回 ErrConcurrencyLimit，实际 %v", err)
	}

	// ⚠️ 达到上限**不是失败**：任务必须还留在 queued 等下一轮。
	// 判失败的话，用户批量提交十个会有七个直接失败。
	var status string
	if err := pool.QueryRow(ctx, `select status from generation where id=9203`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "queued" {
		t.Fatalf("被限流的任务应留在 queued，实际 %s", status)
	}

	// 腾出一个名额后立刻可领。
	if _, err := pool.Exec(ctx,
		`update generation set status='success' where id=9201`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ClaimForRun(ctx, 9203, 0); err != nil {
		t.Fatalf("有空位后应当能领取: %v", err)
	}
}

func TestClaimForRun_并发上限只约束同一家上游(t *testing.T) {
	pool := testsupport.Fresh(t)
	r := repo.New(pool)
	ctx := context.Background()

	seedProviderModel(t, pool, 9001, 9101, "vendor-a", 1)
	seedProviderModel(t, pool, 9002, 9102, "vendor-b", 1)
	insertQueued(t, pool, 9201, 9101, "running") // A 家满了
	insertQueued(t, pool, 9202, 9102, "queued")  // B 家还空着

	if _, err := r.ClaimForRun(ctx, 9202, 0); err != nil {
		t.Fatalf("另一家上游不该被 A 家的占用挡住: %v", err)
	}
}

func TestClaimForRun_上限为0表示不限(t *testing.T) {
	pool := testsupport.Fresh(t)
	r := repo.New(pool)
	ctx := context.Background()

	// 默认值 0 —— 加这一列之前的行为必须原样保留。
	seedProviderModel(t, pool, 9001, 9101, "unlimited", 0)
	for i := int64(0); i < 5; i++ {
		insertQueued(t, pool, 9300+i, 9101, "running")
	}
	insertQueued(t, pool, 9400, 9101, "queued")

	if _, err := r.ClaimForRun(ctx, 9400, 0); err != nil {
		t.Fatalf("上限 0 应当不限: %v", err)
	}
}

func TestClaimForRun_查不到Provider时不拦(t *testing.T) {
	// ⚠️ 并发上限是**节流**，不是校验入口。模型被删时拦下来的话，
	// 任务会一直卡在 queued 且没有任何错误信息，而积分已经扣了。
	// 照常放行，由适配器报"取模型配置失败"并退款 —— 用户至少知道发生了什么。
	pool := testsupport.Fresh(t)
	r := repo.New(pool)

	insertQueued(t, pool, 9500, 777777, "queued") // model_id 不存在
	if _, err := r.ClaimForRun(context.Background(), 9500, 0); err != nil {
		t.Fatalf("查不到模型时不该拦住领取: %v", err)
	}
}
