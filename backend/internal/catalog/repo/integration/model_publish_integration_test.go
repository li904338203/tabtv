//go:build integration

// 模型发布检查的真库测试。
//
// ⚠️ 必须用真库：发布检查要跨 model / model_operation / pricing /
// model_input_capability 四张表联查，mock 仓储会把"查不到行"这件事
// 直接变成测试作者的假设，而这里要守的恰恰是"真的能不能查到"。
//
// P06-02 联调时踩到：`SaveModel` 启用分支传给发布检查的模型结构体
// **没带 ID**（ID 只在仓储里生成，更新时要从已有行取），于是三张关联表
// 一行都查不到，任何已存在的模型都永远启用不了 —— 而报错是
// 「至少需要绑定并启用一个操作」，操作明明已经绑好了。
package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	catalogdomain "github.com/li904338203/tabtv/backend/internal/catalog/domain"
	"github.com/li904338203/tabtv/backend/internal/catalog/repo"
)

func modelInput(code, provider, status string) catalogapp.ModelInput {
	return catalogapp.ModelInput{
		Code: code, Name: "测试模型", ProviderCode: provider, Domain: "image",
		UpstreamModelCode: "upstream-v1", APIProtocol: "openai_images",
		SubmitPath: "/images/generations", EstSeconds: 10, Status: status,
	}
}

func TestSaveModel_配齐之后能启用(t *testing.T) {
	r, pool, next := setup(t)
	svc := catalogapp.New(r, next)
	ctx := context.Background()
	// ⚠️ 协议表由启动时的注册表同步填充（GAP-09）。空库里没有这一步，
	// 建模型会撞 model_api_protocol_fkey —— 这本身就说明"协议目录必须有人同步"。
	if err := svc.SyncProtocols(ctx); err != nil {
		t.Fatal(err)
	}
	seedOperation(t, pool, next(), "text2img")

	if err := r.SaveProvider(ctx, next(), repo.Provider{
		Code: "prov-1", Name: "测试上游", AdapterType: "http",
		BaseURL: "https://upstream.example.com/v1", AuthType: "bearer",
		APIKeyCipher: "v1:fake", Headers: []byte(`{}`),
		ConnectTimeoutMS: 5000, RequestTimeoutMS: 30000, IdleTimeoutMS: 10000,
		Status: "enabled",
	}); err != nil {
		t.Fatal(err)
	}

	// 先建成 disabled：还没绑操作，此时启用本来就该被拒。
	if err := svc.SaveModel(ctx, 0, modelInput("m-pub", "prov-1", "disabled")); err != nil {
		t.Fatalf("建模型失败: %v (cause=%v)", err, errors.Unwrap(err))
	}

	// 逐项配齐，每一步都确认**阻塞原因在减少**——
	// 一次只报一条的话，运营得来回试五遍才配得完。
	if _, err := blockerCodes(t, svc, "m-pub"); err != nil {
		t.Fatal(err)
	}

	if err := svc.SaveModelOperation(ctx, 0, "m-pub", "text2img", true, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.SavePrice(ctx, 0, catalogdomain.Price{
		OperationCode: "text2img", ModelCode: "m-pub", Credits: 3,
		Unit: "per_image", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	// 能力矩阵（GAP-22）：扣费前就能拒掉上游不支持的输入组合。
	if err := svc.SaveCapability(ctx, 0, "m-pub", "text2img", catalogdomain.Capability{
		Role: catalogdomain.RoleText, MinCount: 0, MaxCount: 4,
	}); err != nil {
		t.Fatal(err)
	}

	codes, err := blockerCodes(t, svc, "m-pub")
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != 0 {
		t.Fatalf("配齐后不该还有阻塞原因: %v", codes)
	}

	// ⚠️ 核心断言：走**启用**这条路也必须通过。
	// 发布检查端点说可以、启用却说"没绑操作"，是最难排查的一种不一致 ——
	// 两个答案都来自同一段逻辑，差别只在传进去的模型结构体带没带 ID。
	if err := svc.SaveModel(ctx, 0, modelInput("m-pub", "prov-1", "enabled")); err != nil {
		t.Fatalf("配齐后仍然启用不了: %v", err)
	}

	m, err := r.GetModel(ctx, "m-pub")
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != "enabled" {
		t.Fatalf("状态没落库: %s", m.Status)
	}
}

func TestSaveModel_没配齐时启用被拒且指名原因(t *testing.T) {
	r, pool, next := setup(t)
	svc := catalogapp.New(r, next)
	ctx := context.Background()
	if err := svc.SyncProtocols(ctx); err != nil {
		t.Fatal(err)
	}
	seedOperation(t, pool, next(), "text2img")

	if err := r.SaveProvider(ctx, next(), repo.Provider{
		Code: "prov-2", Name: "测试上游", AdapterType: "http",
		BaseURL: "https://upstream.example.com/v1", AuthType: "bearer",
		APIKeyCipher: "v1:fake", Headers: []byte(`{}`),
		ConnectTimeoutMS: 5000, RequestTimeoutMS: 30000, IdleTimeoutMS: 10000,
		Status: "enabled",
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.SaveModel(ctx, 0, modelInput("m-half", "prov-2", "disabled")); err != nil {
		t.Fatal(err)
	}
	if err := svc.SaveModelOperation(ctx, 0, "m-half", "text2img", true, nil); err != nil {
		t.Fatal(err)
	}
	// 只绑了操作，没配价也没配能力矩阵。
	if err := svc.SaveModel(ctx, 0, modelInput("m-half", "prov-2", "enabled")); err == nil {
		t.Fatal("没配价就启用应当被拒")
	}

	codes, err := blockerCodes(t, svc, "m-half")
	if err != nil {
		t.Fatal(err)
	}
	// ⚠️ 阻塞原因要**一次全报**：一次只报一条的话，运营改一个报一个。
	if len(codes) < 2 {
		t.Fatalf("应当一次列出全部阻塞原因，实际只有 %v", codes)
	}
}

func blockerCodes(t *testing.T, svc *catalogapp.Service, code string) ([]string, error) {
	t.Helper()
	list, err := svc.Publishable(context.Background(), code)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(list))
	for _, b := range list {
		out = append(out, b.Code)
	}
	return out, nil
}

// seedOperation 补一条操作目录。空库里 operation 表是空的，
// 而模型必须绑到一个真实存在的操作上。
func seedOperation(t *testing.T, pool *pgxpool.Pool, id int64, code string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		insert into operation (id, code, name, domain, out_node_type, status)
		values ($1,$2,$2,'image','image','enabled')
		on conflict (code) do nothing`, id, code); err != nil {
		t.Fatal(err)
	}
}
