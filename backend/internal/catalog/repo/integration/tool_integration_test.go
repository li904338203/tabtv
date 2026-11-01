//go:build integration

// 图片工具栏与用户端模型列表的真库测试（P09-04）。
//
// ⚠️ 必须用真库：这两个查询的正确性全在 SQL 里 ——
// 两张表的启用字段名不一样（operation 是 `status='enabled'`，
// model_operation 是布尔 `is_enabled`），写错哪一个都是 42703，
// 而 42703 **只在真跑起来时才报**，编译期看不出来（P09-04 实测踩到）。
package integration

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/catalog/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
)

func setupCatalog(t *testing.T) (*repo.Repo, *pgxpool.Pool, func() int64) {
	t.Helper()
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	return repo.New(pool), pool, gen.Next
}

// ⚠️ `Fresh` 会把**所有表**清空，seed 的操作目录也没了 ——
// 所以每个用例自带它要的那几行，不依赖 seed 跑过。
func seedOp(t *testing.T, pool *pgxpool.Pool, next func() int64,
	code, domain, inTypes, outType, status string) int64 {
	t.Helper()
	id := next()
	if _, err := pool.Exec(context.Background(), `
		insert into operation (id, code, name, domain, in_node_types, out_node_type, status)
		values ($1,$2,$2,$3,$4,$5,$6)
		on conflict (code) do update set in_node_types = excluded.in_node_types,
			status = excluded.status`,
		id, code, domain, inTypes, outType, status); err != nil {
		t.Fatal(err)
	}
	var got int64
	if err := pool.QueryRow(context.Background(),
		`select id from operation where code = $1`, code).Scan(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

type modelSeed struct {
	code           string
	providerStatus string
	modelStatus    string
	domain         string
	op             string
	opEnabled      bool
}

func seedModel(t *testing.T, pool *pgxpool.Pool, next func() int64, s modelSeed) string {
	t.Helper()
	ctx := context.Background()
	providerID := next()
	if _, err := pool.Exec(ctx, `
		insert into provider_config (id, code, name, adapter_type, base_url, status)
		values ($1,$2,$2,'http','http://127.0.0.1:1/v1',$3)`,
		providerID, "p-"+s.code, s.providerStatus); err != nil {
		t.Fatal(err)
	}
	// ⚠️ `model.api_protocol` 是**真外键**（不是本项目常用的逻辑外键），
	// 指向 `api_protocol` 注册表。`Fresh` 把那张表也清空了，
	// 所以要先把协议那一行补回来 —— 否则报 23503，而错误信息里
	// 只说"违反外键约束"，看不出是哪张表被清空了。
	if _, err := pool.Exec(ctx, `
		insert into api_protocol (code, name, is_async)
		values ('openai_images','OpenAI 图片',false)
		on conflict (code) do nothing`); err != nil {
		t.Fatal(err)
	}
	modelID := next()
	if _, err := pool.Exec(ctx, `
		insert into model (id, code, name, provider_id, domain, upstream_model_code,
			api_protocol, submit_path, est_seconds, status, sort)
		values ($1,$2,$2,$3,$4,'up','openai_images','/images/generations',10,$5,0)`,
		modelID, s.code, providerID, s.domain, s.modelStatus); err != nil {
		t.Fatal(err)
	}
	opID := seedOp(t, pool, next, s.op, s.domain, "text,style", "image", "enabled")
	if _, err := pool.Exec(ctx, `
		insert into model_operation (id, model_id, operation_id, is_enabled)
		values ($1,$2,$3,$4)`, next(), modelID, opID, s.opEnabled); err != nil {
		t.Fatal(err)
	}
	return s.code
}

func bindOperation(t *testing.T, pool *pgxpool.Pool, next func() int64,
	modelCode, opCode string) {
	t.Helper()
	ctx := context.Background()
	var modelID int64
	if err := pool.QueryRow(ctx, `select id from model where code = $1`, modelCode).
		Scan(&modelID); err != nil {
		t.Fatal(err)
	}
	opID := seedOp(t, pool, next, opCode, "image", "image,style", "image", "enabled")
	if _, err := pool.Exec(ctx, `
		insert into model_operation (id, model_id, operation_id, is_enabled)
		values ($1,$2,$3,true) on conflict (model_id, operation_id) do nothing`,
		next(), modelID, opID); err != nil {
		t.Fatal(err)
	}
}

// ⚠️ 少一个条件就是"停用的模型还在下拉里"：用户选了它、点生成，
// 才在受理时被拒 —— 而他会以为是自己哪里写错了。
func TestIntegration_用户端模型列表只给真正能选的(t *testing.T) {
	r, pool, next := setupCatalog(t)
	ctx := context.Background()

	ready := seedModel(t, pool, next, modelSeed{
		code: "ok-model", providerStatus: "enabled", modelStatus: "enabled",
		domain: "image", op: "text2img", opEnabled: true,
	})
	seedModel(t, pool, next, modelSeed{
		code: "disabled-model", providerStatus: "enabled", modelStatus: "disabled",
		domain: "image", op: "text2img", opEnabled: true,
	})
	seedModel(t, pool, next, modelSeed{
		code: "provider-down", providerStatus: "disabled", modelStatus: "enabled",
		domain: "image", op: "text2img", opEnabled: true,
	})
	// 绑了操作但没启用：它一个能力都跑不了，不该出现在下拉里。
	seedModel(t, pool, next, modelSeed{
		code: "no-op", providerStatus: "enabled", modelStatus: "enabled",
		domain: "image", op: "text2img", opEnabled: false,
	})

	list, err := r.ListUsableModels(ctx, "image", false)
	if err != nil {
		t.Fatal(err)
	}
	codes := map[string]bool{}
	for _, m := range list {
		codes[m.Code] = true
	}
	if !codes[ready] {
		t.Fatalf("可用模型应在列表里，实际 %+v", list)
	}
	for _, bad := range []string{"disabled-model", "provider-down", "no-op"} {
		if codes[bad] {
			t.Fatalf("%s 不该出现在用户端列表里", bad)
		}
	}
}

// ⚠️ 一个模型绑好几个操作时，不带筛选的列表里它只该出现**一次** ——
// 按操作的 domain 分组的话会重复，用户会以为配重了。
func TestIntegration_模型在列表里不重复(t *testing.T) {
	r, pool, next := setupCatalog(t)
	ctx := context.Background()

	code := seedModel(t, pool, next, modelSeed{
		code: "multi-op", providerStatus: "enabled", modelStatus: "enabled",
		domain: "image", op: "text2img", opEnabled: true,
	})
	bindOperation(t, pool, next, code, "img2img")

	list, err := r.ListUsableModels(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range list {
		if m.Code == code {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("绑了两个操作的模型只该出现一次，实际 %d 次", n)
	}
}

// 停用的操作不该把模型带进列表：那条能力本来就跑不了。
func TestIntegration_停用的操作不算数(t *testing.T) {
	r, pool, next := setupCatalog(t)
	ctx := context.Background()

	seedModel(t, pool, next, modelSeed{
		code: "op-off", providerStatus: "enabled", modelStatus: "enabled",
		domain: "image", op: "text2img", opEnabled: true,
	})
	if _, err := pool.Exec(ctx,
		`update operation set status='disabled' where code='text2img'`); err != nil {
		t.Fatal(err)
	}
	list, err := r.ListUsableModels(ctx, "image", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range list {
		if m.Code == "op-off" {
			t.Fatal("操作停用后模型不该还在列表里")
		}
	}
}

// ⚠️ 这一条是「工具栏说能用、点下去 422」的修法：
// `text2img` 的 in_node_types 是 `text,style`，**根本不收图**，
// 而能力矩阵可能是放行的（管理员完全可能给它配一条 image 角色）。
// 两件事要分开判，少判这一条工具栏就白做了。
func TestIntegration_操作的输入类型能读出来(t *testing.T) {
	r, pool, next := setupCatalog(t)

	seedOp(t, pool, next, "text2img", "image", "text,style", "image", "enabled")
	seedOp(t, pool, next, "img2img", "image", "image,style", "image", "enabled")
	// 停用的操作不该被读出来：它连绑都不该被绑上。
	seedOp(t, pool, next, "legacy_op", "image", "image", "image", "disabled")

	got, err := r.OperationInputs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["legacy_op"]; ok {
		t.Fatal("停用的操作不该出现")
	}
	if has(got["text2img"], "image") {
		t.Fatalf("text2img 不该接受 image：%v", got["text2img"])
	}
	if !has(got["img2img"], "image") {
		t.Fatalf("img2img 应接受 image：%v", got["img2img"])
	}
	// 顺带验一下逗号切分没把空串带进来。
	for _, v := range got["text2img"] {
		if v == "" {
			t.Fatal("切分出了空项")
		}
	}
}

func has(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
