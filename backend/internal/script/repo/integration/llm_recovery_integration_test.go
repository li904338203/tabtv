//go:build integration

// 同步模型调用的失败退款与恢复扫描（真库：退款是否发生、是否只发生一次，都要看真账本）。
//
// ⚠️ 起因（2026-10-14 实测）：API 全局 30 秒超时在模型写分镜写到一半时掐断请求，
// 失败路径的退款与终态又用了那个**已取消的**请求 ctx 去写库 —— 四次调用 162 积分全部没退，
// 调用永远停在 running；而注释里说的「漏掉的由对账作业补」并不存在。
package integration

import (
	"context"
	"strconv"
	"testing"
	"time"

	scriptapp "github.com/li904338203/tabtv/backend/internal/script/app"
	"github.com/li904338203/tabtv/backend/internal/script/domain"
	"github.com/li904338203/tabtv/backend/internal/script/repo"
)

// waitingModel 一直等到请求被取消（模拟「模型还没写完，请求先超时 / 浏览器断开」）。
type waitingModel struct{}

func (waitingModel) Complete(ctx context.Context, _ scriptapp.TextRequest) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

func callStatus(t *testing.T, r *repo.Repo, id int64) string {
	t.Helper()
	c, err := r.GetCall(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return c.Status
}

func lastCallID(t *testing.T, svc *scriptapp.Service, scriptID int64) int64 {
	t.Helper()
	calls, err := svc.Calls(context.Background(), ws, scriptID)
	if err != nil || len(calls) == 0 {
		t.Fatalf("应有调用记录：%v %d", err, len(calls))
	}
	return calls[0].ID
}

func TestStoryboard_请求被取消时照样退款并判失败(t *testing.T) {
	svc, r, bill, _ := setup(t, &fakeModel{}, 36)
	svc.SetTextModel(waitingModel{})
	fund(t, bill, 100)
	sc := newScript(t, svc, "三打白骨精")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := svc.GenerateStoryboard(ctx, scriptapp.StoryboardInput{
		WorkspaceID: ws, PayerAccountID: payer, UserID: user,
		ScriptID: sc.Script.ID, ModelCode: "gpt-x",
	}); err == nil {
		t.Fatal("请求被取消，分镜应当失败")
	}

	if bal, _ := bill.Balance(context.Background(), payer); bal != 100 {
		t.Fatalf("请求被取消也必须退款（此前退款用的是已取消的 ctx，钱扣了不退），余额 %d", bal)
	}
	if got := callStatus(t, r, lastCallID(t, svc, sc.Script.ID)); got != string(domain.LLMFailed) {
		t.Fatalf("调用应当判 failed（此前永远停在 running），得到 %s", got)
	}
}

func TestRecoverStaleCalls_停在running太久的判失败并退款且只退一次(t *testing.T) {
	svc, r, bill, pool := setup(t, &fakeModel{}, 0)
	fund(t, bill, 100)
	sc := newScript(t, svc, "三打白骨精")
	ctx := context.Background()

	// 两条 running 的调用：一条 30 分钟前（已经没有请求在等它了）、一条刚开始（可能还在等模型）。
	open := func(id int64, key string, ago time.Duration) {
		t.Helper()
		if err := r.InsertCall(ctx, repo.LLMCall{
			ID: id, WorkspaceID: ws, PayerAccountID: payer, ScriptID: sc.Script.ID,
			Purpose: string(domain.PurposeStoryboard), ModelID: 1, IdempotencyKey: key, CreditsCost: 36,
		}); err != nil {
			t.Fatal(err)
		}
		if err := (ledger{bill}).Consume(ctx, payer, user, 36, "script_llm", strconv.FormatInt(id, 10), "分镜生成"); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `update script_llm_call set created_at = now() - make_interval(secs => $2) where id = $1`,
			id, ago.Seconds()); err != nil {
			t.Fatal(err)
		}
	}
	open(9101, "stale", 30*time.Minute)
	open(9102, "fresh", time.Minute)
	if bal, _ := bill.Balance(ctx, payer); bal != 28 {
		t.Fatalf("前置：两次各扣 36，余额应为 28，得到 %d", bal)
	}

	n, err := svc.RecoverStaleCalls(ctx)
	if err != nil || n != 1 {
		t.Fatalf("应当只处理那条 30 分钟前的：n=%d err=%v", n, err)
	}
	if got := callStatus(t, r, 9101); got != string(domain.LLMFailed) {
		t.Fatalf("停太久的应判 failed，得到 %s", got)
	}
	if got := callStatus(t, r, 9102); got != string(domain.LLMRunning) {
		t.Fatalf("刚开始的可能还在等模型，不该动，得到 %s", got)
	}
	if bal, _ := bill.Balance(ctx, payer); bal != 64 {
		t.Fatalf("应只退回停太久的那 36，余额应为 64，得到 %d", bal)
	}

	// 再扫一次：不重复退款
	if n, err := svc.RecoverStaleCalls(ctx); err != nil || n != 0 {
		t.Fatalf("第二轮不该再处理：n=%d err=%v", n, err)
	}
	if bal, _ := bill.Balance(ctx, payer); bal != 64 {
		t.Fatalf("不能重复退款，余额应仍为 64，得到 %d", bal)
	}
}
