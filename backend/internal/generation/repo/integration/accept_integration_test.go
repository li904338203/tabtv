//go:build integration

// 生成受理的真库测试。
//
// ⚠️ 受理的核心是**原子性**：写任务、扣费、写 outbox 必须同生共死。
// 这只能在真库里证明 —— 用 mock 只能证明"我调用了这三个函数"。
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"
	"github.com/li904338203/tabtv/backend/internal/generation/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
)

const (
	ws    = int64(6001)
	payer = int64(6002)
	user  = int64(6003)
	proj  = int64(6004)
)

// fakeQuoter 固定报价，把目录域隔离在外 —— 这里要测的是受理的事务边界。
type fakeQuoter struct {
	credits int
	err     error
}

func (f fakeQuoter) QuoteForAccept(context.Context, genapp.QuoteAsk) (genapp.QuoteAnswer, error) {
	if f.err != nil {
		return genapp.QuoteAnswer{}, f.err
	}
	return genapp.QuoteAnswer{
		Credits: f.credits, EstimatedSeconds: 30, ResultType: "image",
		ModelID: 1, OperationID: 2, ProviderCode: "dev", APIProtocol: "dev_fake",
		PriceUnit: "per_image", PriceCredits: f.credits,
	}, nil
}

type fakeInputs struct{ issues []string }

func (f fakeInputs) ResolveForAccept(context.Context, int64, int64, string, string,
	json.RawMessage) (genapp.ResolvedInputs, error) {
	return genapp.ResolvedInputs{
		Inputs:       json.RawMessage(`[{"kind":"text","order":1,"text":"一只猫"}]`),
		PromptSuffix: "水墨风格",
		// ⚠️ 角色计数由解析方给出：能力矩阵按"服务端真的解析出几个输入"校验，
		// 信客户端报的数等于让 min/max 形同虚设。
		RoleCounts: map[string]int{"text": 1},
		Issues:     f.issues,
	}, nil
}

func setup(t *testing.T, credits int) (*genapp.Service, *repo.Repo, *billingrepo.Repo, *pgxpool.Pool) {
	t.Helper()
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	r := repo.New(pool)
	ledger := billingrepo.New(pool, gen.Next)

	svc := genapp.New(r, gen.Next)
	svc.SetQuoter(fakeQuoter{credits: credits})
	svc.SetInputResolver(fakeInputs{})
	svc.SetCredits(ledger)
	return svc, r, ledger, pool
}

func fund(t *testing.T, ledger *billingrepo.Repo, amount int) {
	t.Helper()
	if _, err := ledger.Grant(context.Background(), billingrepo.GrantInput{
		PayerID: payer, Source: "purchase", SourceRefID: "test-fund", Amount: amount,
	}); err != nil {
		t.Fatal(err)
	}
}

func accept(svc *genapp.Service, key string) (genapp.AcceptResult, error) {
	return svc.Accept(context.Background(), genapp.AcceptInput{
		WorkspaceID: ws, PayerAccountID: payer, UserID: user, ProjectID: proj,
		NodeID: "n_image_1", OperationCode: "text2img", ModelCode: "dev_image",
		IdempotencyKey: key, Count: 1, Params: json.RawMessage(`{"prompt":"一只猫","未来参数":1}`),
	})
}

// 受理成功后：任务 queued、扣费一次、outbox 一条，三者都在。
func TestAccept_一个事务写齐三件事(t *testing.T) {
	svc, _, ledger, pool := setup(t, 10)
	ctx := context.Background()
	fund(t, ledger, 100)

	res, err := accept(svc, "key-1")
	if err != nil {
		t.Fatal(err)
	}
	g := res.Generation
	if g.Status != gendomain.StatusQueued {
		t.Errorf("应为 queued，实际 %s", g.Status)
	}
	if g.CreditsCost != 10 {
		t.Errorf("费用应为 10，实际 %d", g.CreditsCost)
	}
	// 死线必须在受理时存下来，恢复扫描不得按当前配置重算。
	if g.DeadlineAt == nil {
		t.Error("受理时必须算好死线并存下来")
	}

	if bal, _ := ledger.Balance(ctx, payer); bal != 90 {
		t.Errorf("应扣 10，实际余额 %d", bal)
	}
	var outboxCount int
	_ = pool.QueryRow(ctx, `
		select count(*) from outbox_message
		where aggregate_type='generation' and aggregate_id=$1 and published_at is null`,
		g.ID).Scan(&outboxCount)
	if outboxCount != 1 {
		t.Errorf("应有 1 条待投递消息，实际 %d", outboxCount)
	}

	// 快照：用户参数原样保留，配置快照放在 _snapshot 下，两者不混在一层。
	var params map[string]any
	_ = json.Unmarshal(g.ParamsSnapshot, &params)
	if params["prompt"] != "一只猫" {
		t.Error("用户参数应原样保留")
	}
	if _, ok := params["未来参数"]; !ok {
		t.Error("未知参数不该被吃掉")
	}
	snap, ok := params["_snapshot"].(map[string]any)
	if !ok {
		t.Fatal("配置快照应在 _snapshot 下")
	}
	if snap["promptSuffix"] != "水墨风格" {
		t.Error("素材提示词应冻结进快照")
	}
	if snap["priceCredits"] == nil {
		t.Error("受理那一刻的价格必须冻结，否则事后调价会让历史费用无法解释")
	}
}

// 幂等重放返回**同一个任务**且不重复扣费。
func TestAccept_幂等重放(t *testing.T) {
	svc, _, ledger, _ := setup(t, 10)
	ctx := context.Background()
	fund(t, ledger, 100)

	first, err := accept(svc, "same-key")
	if err != nil {
		t.Fatal(err)
	}
	second, err := accept(svc, "same-key")
	if err != nil {
		t.Fatalf("重放不该报错 —— 报错会让客户端换个键再试，那才真的是两次扣费: %v", err)
	}
	if second.Generation.ID != first.Generation.ID {
		t.Fatalf("应返回同一个任务，实际 %d vs %d", first.Generation.ID, second.Generation.ID)
	}
	if !second.Replayed {
		t.Error("应标记为重放，调用方据此知道没有新扣费")
	}
	if bal, _ := ledger.Balance(ctx, payer); bal != 90 {
		t.Fatalf("只该扣一次，实际余额 %d", bal)
	}
}

// 并发重复提交：只产生一个任务与一次扣费（AC-04）。
func TestAccept_并发重复提交(t *testing.T) {
	svc, _, ledger, pool := setup(t, 10)
	ctx := context.Background()
	fund(t, ledger, 1000)

	const n = 8
	var wg sync.WaitGroup
	ids := make([]int64, n)
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			res, err := accept(svc, "race-key")
			errs[i] = err
			if err == nil {
				ids[i] = res.Generation.ID
			}
		}(i)
	}
	wg.Wait()

	var uniq = map[int64]bool{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 个请求不该失败: %v", i, err)
		}
		uniq[ids[i]] = true
	}
	if len(uniq) != 1 {
		t.Fatalf("并发提交应只产生一个任务，实际 %d 个", len(uniq))
	}
	var genCount int
	_ = pool.QueryRow(ctx, `select count(*) from generation`).Scan(&genCount)
	if genCount != 1 {
		t.Fatalf("库里应只有 1 个任务，实际 %d", genCount)
	}
	if bal, _ := ledger.Balance(ctx, payer); bal != 990 {
		t.Fatalf("只该扣一次 10 分，实际余额 %d", bal)
	}
}

// ⚠️ 余额不足时**整个受理回滚**：不能留下一个扣不到费却在排队的任务。
func TestAccept_余额不足时不留下任务(t *testing.T) {
	svc, _, ledger, pool := setup(t, 100)
	ctx := context.Background()
	fund(t, ledger, 50) // 不够

	if _, err := accept(svc, "poor-key"); err == nil {
		t.Fatal("余额不足应被拒")
	}
	var genCount, outboxCount int
	_ = pool.QueryRow(ctx, `select count(*) from generation`).Scan(&genCount)
	_ = pool.QueryRow(ctx, `select count(*) from outbox_message`).Scan(&outboxCount)
	if genCount != 0 {
		t.Errorf("扣费失败应回滚任务，实际库里有 %d 个", genCount)
	}
	if outboxCount != 0 {
		t.Errorf("扣费失败应回滚 outbox，实际 %d 条", outboxCount)
	}
	if bal, _ := ledger.Balance(ctx, payer); bal != 50 {
		t.Errorf("余额不该变化，实际 %d", bal)
	}
}

// 输入不合法就不该走到扣费 —— 等上游返回 400 才发现的话，钱已经扣了。
func TestAccept_输入不合法不扣费(t *testing.T) {
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	ledger := billingrepo.New(pool, gen.Next)
	svc := genapp.New(repo.New(pool), gen.Next)
	svc.SetQuoter(fakeQuoter{credits: 10})
	svc.SetInputResolver(fakeInputs{issues: []string{"首尾帧需要恰好 2 张图，当前 1 张"}})
	svc.SetCredits(ledger)
	fund(t, ledger, 100)

	_, err := accept(svc, "bad-input")
	if err == nil {
		t.Fatal("输入有问题应被拒")
	}
	var n int
	_ = pool.QueryRow(context.Background(), `select count(*) from generation`).Scan(&n)
	if n != 0 {
		t.Error("不该留下任务")
	}
	if bal, _ := ledger.Balance(context.Background(), payer); bal != 100 {
		t.Errorf("不该扣费，实际余额 %d", bal)
	}
}

// 报价失败（能力不支持、缺价格）同样不留痕迹。
func TestAccept_报价失败不留痕迹(t *testing.T) {
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	ledger := billingrepo.New(pool, gen.Next)
	svc := genapp.New(repo.New(pool), gen.Next)
	svc.SetQuoter(fakeQuoter{err: errors.New("该模型不支持该操作")})
	svc.SetInputResolver(fakeInputs{})
	svc.SetCredits(ledger)
	fund(t, ledger, 100)

	if _, err := accept(svc, "no-quote"); err == nil {
		t.Fatal("报价失败应被拒")
	}
	var n int
	_ = pool.QueryRow(context.Background(), `select count(*) from generation`).Scan(&n)
	if n != 0 {
		t.Error("不该留下任务")
	}
}

// 幂等键必填且受字符集约束。
func TestAccept_幂等键校验(t *testing.T) {
	svc, _, ledger, _ := setup(t, 10)
	fund(t, ledger, 100)
	for _, key := range []string{"", "has space", "has/slash"} {
		if _, err := accept(svc, key); err == nil {
			t.Errorf("幂等键 %q 应被拒", key)
		}
	}
}

// outbox 的投递生命周期：认领 → 标记已发；失败只计数不删除。
func TestOutbox_投递生命周期(t *testing.T) {
	svc, r, ledger, pool := setup(t, 10)
	ctx := context.Background()
	fund(t, ledger, 100)
	if _, err := accept(svc, "outbox-key"); err != nil {
		t.Fatal(err)
	}

	msgs, err := r.ClaimOutbox(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("应认领到 1 条，实际 %d", len(msgs))
	}
	if msgs[0].Topic != gendomain.OutboxTopic {
		t.Errorf("主题应为 %s，实际 %s", gendomain.OutboxTopic, msgs[0].Topic)
	}

	// 失败不删除：删了就等于这条消息永远不会发，而任务已经扣过费、已在排队。
	if err := r.MarkOutboxFailed(ctx, msgs[0].ID, "MQ 不可用"); err != nil {
		t.Fatal(err)
	}
	again, _ := r.ClaimOutbox(ctx, 10)
	if len(again) != 1 || again[0].Attempt != 1 {
		t.Fatalf("失败后仍应可认领且计数 +1，实际 %+v", again)
	}

	if err := r.MarkOutboxPublished(ctx, msgs[0].ID); err != nil {
		t.Fatal(err)
	}
	done, _ := r.ClaimOutbox(ctx, 10)
	if len(done) != 0 {
		t.Errorf("已投递的不该再被认领，实际 %d 条", len(done))
	}
	var n int
	_ = pool.QueryRow(ctx, `select count(*) from outbox_message where published_at is not null`).Scan(&n)
	if n != 1 {
		t.Errorf("应保留已投递记录供排查，实际 %d", n)
	}
}

// 跨工作区拿不到别人的任务。
func TestGet_工作区隔离(t *testing.T) {
	svc, _, ledger, _ := setup(t, 10)
	ctx := context.Background()
	fund(t, ledger, 100)
	res, err := accept(svc, "scope-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, ws, res.Generation.ID); err != nil {
		t.Fatalf("本工作区应能取到: %v", err)
	}
	if _, err := svc.Get(ctx, ws+1, res.Generation.ID); err == nil {
		t.Fatal("别的工作区不该取到")
	}
}

// CD-81：只给了 Text 的受理，提示词也要进快照（Worker 只读 `params.prompt`）。
//
// ⚠️ 起因：组子项只把提示词放在 Text 上，Text 报完价就丢了 —— 快照里没有 prompt，
// 每一条分镜图都是空提示词发出去的，而受理、扣费、排队一路成功，直到上游回 400。
func TestAccept_只给Text时提示词也要进快照(t *testing.T) {
	svc, _, ledger, _ := setup(t, 10)
	fund(t, ledger, 100)
	res, err := svc.Accept(context.Background(), genapp.AcceptInput{
		WorkspaceID: ws, PayerAccountID: payer, UserID: user, ProjectID: proj,
		NodeID: "n_group:item:1", OperationCode: "img2img", ModelCode: "dev_image",
		IdempotencyKey: "text-only", Count: 1, Text: "第 1 镜：唐僧站在山门前",
		Params: json.RawMessage(`{"aspect_ratio":"16:9"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var params map[string]any
	_ = json.Unmarshal(res.Generation.ParamsSnapshot, &params)
	if params["prompt"] != "第 1 镜：唐僧站在山门前" {
		t.Fatalf("快照里应有提示词，得到 %v", params["prompt"])
	}
	if params["aspect_ratio"] != "16:9" {
		t.Fatalf("其余参数应原样保留：%v", params)
	}

	// 参数里已有提示词时以参数为准：调用方放在那里的才是要发的。
	res, err = svc.Accept(context.Background(), genapp.AcceptInput{
		WorkspaceID: ws, PayerAccountID: payer, UserID: user, ProjectID: proj,
		NodeID: "n_image_2", OperationCode: "text2img", ModelCode: "dev_image",
		IdempotencyKey: "both", Count: 1, Text: "别的词",
		Params: json.RawMessage(`{"prompt":"参数里的词"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(res.Generation.ParamsSnapshot, &params)
	if params["prompt"] != "参数里的词" {
		t.Fatalf("参数里的提示词不该被 Text 盖掉，得到 %v", params["prompt"])
	}
}
