//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"
	"github.com/li904338203/tabtv/backend/internal/generation/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/usererr"
)

// fakeProvider 可控地模拟上游的四种结局。
type fakeProvider struct {
	mu        sync.Mutex
	submits   int
	outcome   gendomain.SubmitOutcome
	queryDone bool
	queryOK   bool
	canQuery  bool
	httpCode  int
	message   string
}

func (f *fakeProvider) Submit(context.Context, genapp.Job) (genapp.SubmitResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submits++
	return genapp.SubmitResult{
		Outcome: f.outcome, ProviderTaskID: "up-task-1",
		HTTPStatus: f.httpCode, Message: nonEmpty(f.message, "fake"),
	}, nil
}

func (f *fakeProvider) Query(context.Context, genapp.Job, string) (genapp.QueryResult, error) {
	return genapp.QueryResult{Done: f.queryDone, Success: f.queryOK, Progress: 100}, nil
}

func (f *fakeProvider) CanQueryByIdempotencyKey() bool { return f.canQuery }

func (f *fakeProvider) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.submits
}

func newExecutor(t *testing.T, r *repo.Repo, p *fakeProvider, ledger *billingrepo.Repo) *genapp.Executor {
	t.Helper()
	e := genapp.NewExecutor(r, p, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.PollInterval = 10 * time.Millisecond
	if ledger != nil {
		e.SetRefunder(refunderAdapter{ledger})
	}
	return e
}

type refunderAdapter struct{ r *billingrepo.Repo }

func (a refunderAdapter) Refund(ctx context.Context, payerID int64, refType, refID, reason string) error {
	_, err := a.r.Refund(ctx, billingrepo.RefundInput{
		PayerID: payerID, RefType: refType, RefID: refID, Reason: reason,
		CompensationValidFor: 30 * 24 * time.Hour,
	})
	return err
}

// MQ 是 at-least-once，同一条消息会重复到达；幂等靠 CAS 而不是"先查再改"。
func TestExecute_重复消息只执行一次(t *testing.T) {
	svc, r, ledger, _ := setup(t, 10)
	ctx := context.Background()
	fund(t, ledger, 100)
	res, err := accept(svc, "dup-msg")
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeProvider{outcome: gendomain.SubmitCompleted}
	e := newExecutor(t, r, p, ledger)

	first, err := e.Execute(ctx, res.Generation.ID)
	if err != nil || first != genapp.OutcomeDone {
		t.Fatalf("首次执行应完成: %v %v", first, err)
	}
	second, err := e.Execute(ctx, res.Generation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second != genapp.OutcomeDuplicate {
		t.Fatalf("重复消息应被识别为重复，实际 %v", second)
	}
	if p.count() != 1 {
		t.Fatalf("上游只该被提交 1 次，实际 %d 次", p.count())
	}
}

// 并发上限必须跨实例生效：进程内计数在多 Worker 下各数各的。
func TestExecute_并发上限跨实例(t *testing.T) {
	svc, r, ledger, pool := setup(t, 1)
	ctx := context.Background()
	fund(t, ledger, 100)

	var ids []int64
	for i := 0; i < 5; i++ {
		res, err := accept(svc, "limit-"+string(rune('a'+i)))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, res.Generation.ID)
	}
	// 先把 2 个任务置为 running，占满上限 2。
	for _, id := range ids[:2] {
		if _, err := pool.Exec(ctx,
			`update generation set status='running' where id=$1`, id); err != nil {
			t.Fatal(err)
		}
	}

	// 两个"不同实例"的执行器共用同一个库。
	p := &fakeProvider{outcome: gendomain.SubmitCompleted}
	e1 := newExecutor(t, r, p, ledger)
	e1.MaxRunningPerPayer = 2
	e2 := newExecutor(t, r, p, ledger)
	e2.MaxRunningPerPayer = 2

	for i, e := range []*genapp.Executor{e1, e2} {
		got, err := e.Execute(ctx, ids[2+i])
		if err != nil {
			t.Fatal(err)
		}
		// ⚠️ 达到上限不是失败：任务留在 queued 等下一轮，
		// 判失败的话用户批量提交十个会有七个直接失败。
		if got != genapp.OutcomeRequeue {
			t.Fatalf("实例 %d 应因并发上限重回队列，实际 %v", i+1, got)
		}
		var status string
		_ = pool.QueryRow(ctx, `select status from generation where id=$1`, ids[2+i]).Scan(&status)
		if status != "queued" {
			t.Errorf("被限流的任务应留在 queued，实际 %s", status)
		}
	}
	if p.count() != 0 {
		t.Errorf("限流时不该调用上游，实际 %d 次", p.count())
	}
}

// 提交结果不确定时**绝不重发** —— 重发可能让用户被上游计费两次。
func TestExecute_不确定窗口不重发且退款(t *testing.T) {
	svc, r, ledger, pool := setup(t, 10)
	ctx := context.Background()
	fund(t, ledger, 100)
	res, err := accept(svc, "uncertain")
	if err != nil {
		t.Fatal(err)
	}
	if bal, _ := ledger.Balance(ctx, payer); bal != 90 {
		t.Fatalf("受理应扣 10，实际余额 %d", bal)
	}

	p := &fakeProvider{outcome: gendomain.SubmitUncertain, canQuery: false}
	e := newExecutor(t, r, p, ledger)
	got, err := e.Execute(ctx, res.Generation.ID)
	if err != nil {
		t.Fatal(err)
	}
	// ⚠️ 终态之外还要**留档**（P06-03）：请求可能已经到了上游、可能已在那边计费，
	// 而我们没有任何凭据去认领它。库里的行只能说"失败了"，说不出
	// "去上游查一下有没有一个孤儿任务" —— 那正是死信队列要承载的信息。
	if got != genapp.OutcomeDead {
		t.Fatalf("不支持反查时应判定终态并转入死信，实际 %v", got)
	}
	if p.count() != 1 {
		t.Fatalf("绝不该重发，实际提交 %d 次", p.count())
	}

	var status, code string
	_ = pool.QueryRow(ctx, `select status, coalesce(error_code,'') from generation where id=$1`,
		res.Generation.ID).Scan(&status, &code)
	if status != "failed" {
		t.Errorf("应判失败，实际 %s", status)
	}
	// 单独的错误码，运维要能捞出这批"可能已在上游消费"的任务。
	if code != gendomain.ErrorCodeSubmitUncertain {
		t.Errorf("错误码应为 %s，实际 %s", gendomain.ErrorCodeSubmitUncertain, code)
	}
	if bal, _ := ledger.Balance(ctx, payer); bal != 100 {
		t.Errorf("失败应退款，实际余额 %d", bal)
	}
}

// 上游原话不能给用户看（2026-10-31）：库里 error_message 与积分流水原因都是归类后的说法，
// 原话只进 error_detail（运维台）。
func TestExecute_上游原话不进用户可见字段(t *testing.T) {
	svc, r, ledger, pool := setup(t, 10)
	ctx := context.Background()
	fund(t, ledger, 100)
	res, err := accept(svc, "rejected-raw")
	if err != nil {
		t.Fatal(err)
	}
	raw := `上游返回 HTTP 400: {"error":{"message":"Invalid size","host":"10.9.8.7"}}`
	p := &fakeProvider{outcome: gendomain.SubmitRejected, httpCode: 400, message: raw}
	if _, err := newExecutor(t, r, p, ledger).Execute(ctx, res.Generation.ID); err != nil {
		t.Fatal(err)
	}
	var msg, detail string
	_ = pool.QueryRow(ctx, `select coalesce(error_message,''), coalesce(error_detail,'')
		from generation where id=$1`, res.Generation.ID).Scan(&msg, &detail)
	if msg != usererr.Rejected {
		t.Errorf("error_message 应是归类后的说法，实际 %q", msg)
	}
	if detail != raw {
		t.Errorf("error_detail 应是上游原话，实际 %q", detail)
	}
	var reasons string
	_ = pool.QueryRow(ctx, `select coalesce(string_agg(reason, '|'), '') from credit_transaction
		where ref_id=$1`, itoa(res.Generation.ID)).Scan(&reasons)
	if strings.Contains(reasons, "10.9.8.7") {
		t.Errorf("积分流水原因不能带上游原话：%q", reasons)
	}
}

// 支持按幂等键反查的协议：放回队列下一轮先查，不判失败。
func TestExecute_不确定窗口可反查时重回队列(t *testing.T) {
	svc, r, ledger, pool := setup(t, 10)
	ctx := context.Background()
	fund(t, ledger, 100)
	res, _ := accept(svc, "uncertain-q")

	p := &fakeProvider{outcome: gendomain.SubmitUncertain, canQuery: true}
	e := newExecutor(t, r, p, ledger)
	got, _ := e.Execute(ctx, res.Generation.ID)
	if got != genapp.OutcomeRetry {
		t.Fatalf("应重回队列，实际 %v", got)
	}
	var status string
	_ = pool.QueryRow(ctx, `select status from generation where id=$1`, res.Generation.ID).Scan(&status)
	if status != "queued" {
		t.Errorf("应回到 queued，实际 %s", status)
	}
	if bal, _ := ledger.Balance(ctx, payer); bal != 90 {
		t.Errorf("还要重试，不该退款，实际余额 %d", bal)
	}
}

// 晚到的失败不得覆盖已成功的任务，也不得触发第二次退款。
func TestFinish_终态不被覆盖且不重复退款(t *testing.T) {
	svc, r, ledger, pool := setup(t, 10)
	ctx := context.Background()
	fund(t, ledger, 100)
	res, _ := accept(svc, "late-result")
	id := res.Generation.ID

	if _, err := pool.Exec(ctx, `update generation set status='running' where id=$1`, id); err != nil {
		t.Fatal(err)
	}
	ok, err := r.Finish(ctx, repo.FinishInput{ID: id, Status: "success", Progress: 100})
	if err != nil || !ok {
		t.Fatalf("首次写终态应成功: ok=%v err=%v", ok, err)
	}
	// 晚到的失败。
	ok, err = r.Finish(ctx, repo.FinishInput{ID: id, Status: "failed", ErrorCode: "late"})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("晚到的结果不该覆盖终态 —— 用户会看到成功的产物变成失败")
	}
	var status string
	_ = pool.QueryRow(ctx, `select status from generation where id=$1`, id).Scan(&status)
	if status != "success" {
		t.Fatalf("应保持 success，实际 %s", status)
	}
	if bal, _ := ledger.Balance(ctx, payer); bal != 90 {
		t.Errorf("成功的任务不该退款，实际余额 %d", bal)
	}
}

// 超时用**存下来的** deadline 判，不按当前配置重算。
func TestRecoverStale_超时判失败并退款(t *testing.T) {
	svc, r, ledger, pool := setup(t, 10)
	ctx := context.Background()
	fund(t, ledger, 100)
	res, _ := accept(svc, "stale")
	id := res.Generation.ID

	if _, err := pool.Exec(ctx, `
		update generation set status='running', deadline_at = now() - interval '1 minute',
		    provider_task_id='up-1', heartbeat_at = now() where id=$1`, id); err != nil {
		t.Fatal(err)
	}

	p := &fakeProvider{}
	e := newExecutor(t, r, p, ledger)
	if n := e.RecoverStale(ctx, time.Minute, 10); n != 1 {
		t.Fatalf("应恢复 1 个任务，实际 %d", n)
	}
	var status, code string
	_ = pool.QueryRow(ctx, `select status, coalesce(error_code,'') from generation where id=$1`, id).
		Scan(&status, &code)
	if status != "failed" || code != gendomain.ErrorCodeTimeout {
		t.Fatalf("应判超时失败，实际 %s/%s", status, code)
	}
	if bal, _ := ledger.Balance(ctx, payer); bal != 100 {
		t.Errorf("超时应退款，实际余额 %d", bal)
	}
	// 再扫一次不该重复处理（终态不可覆盖 → 不会重复退款）。
	e.RecoverStale(ctx, time.Minute, 10)
	if bal, _ := ledger.Balance(ctx, payer); bal != 100 {
		t.Errorf("重复扫描不该重复退款，实际余额 %d", bal)
	}
}

// 长时间没被领取的任务**不改状态**，只告警 —— 它本身没问题，需要的是重新投递。
func TestRecoverStale_孤儿排队任务不被判失败(t *testing.T) {
	svc, r, ledger, pool := setup(t, 10)
	ctx := context.Background()
	fund(t, ledger, 100)
	res, _ := accept(svc, "orphan")
	id := res.Generation.ID
	if _, err := pool.Exec(ctx, `
		update generation set created_at = now() - interval '1 hour',
		    deadline_at = now() + interval '1 hour' where id=$1`, id); err != nil {
		t.Fatal(err)
	}

	e := newExecutor(t, r, &fakeProvider{}, ledger)
	e.RecoverStale(ctx, time.Minute, 10)

	var status string
	_ = pool.QueryRow(ctx, `select status from generation where id=$1`, id).Scan(&status)
	if status != "queued" {
		t.Fatalf("孤儿排队任务应保持 queued 等待重新投递，实际 %s", status)
	}
	if bal, _ := ledger.Balance(ctx, payer); bal != 90 {
		t.Errorf("不该退款，实际余额 %d", bal)
	}
}

// 领取是 CAS：并发领取只有一个成功。
func TestClaimForRun_并发只有一个成功(t *testing.T) {
	svc, r, ledger, _ := setup(t, 10)
	ctx := context.Background()
	fund(t, ledger, 100)
	res, _ := accept(svc, "claim-race")

	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, notClaimable := 0, 0
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, err := r.ClaimForRun(ctx, res.Generation.ID, 0)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, repo.ErrNotClaimable):
				notClaimable++
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("并发领取只应有 1 个成功，实际 %d", ok)
	}
	if notClaimable != n-1 {
		t.Fatalf("其余应判不可领取，实际 %d", notClaimable)
	}
}

// 轮询失败不增加 attempt：attempt 记的是"向上游提交了几次"。
func TestClaimForRun_每次领取递增attempt(t *testing.T) {
	svc, r, ledger, pool := setup(t, 10)
	ctx := context.Background()
	fund(t, ledger, 100)
	res, _ := accept(svc, "attempt")
	id := res.Generation.ID

	for i := 1; i <= 2; i++ {
		if _, err := r.ClaimForRun(ctx, id, 0); err != nil {
			t.Fatal(err)
		}
		var attempt int
		_ = pool.QueryRow(ctx, `select attempt from generation where id=$1`, id).Scan(&attempt)
		if attempt != i {
			t.Fatalf("第 %d 次领取后 attempt 应为 %d，实际 %d", i, i, attempt)
		}
		if _, err := r.ReleaseToQueue(ctx, id); err != nil {
			t.Fatal(err)
		}
		// 放回队列**不清空 attempt**：清空等于把重试次数上限作废。
		var after int
		_ = pool.QueryRow(ctx, `select attempt from generation where id=$1`, id).Scan(&after)
		if after != i {
			t.Fatalf("放回队列不该清空 attempt，实际 %d", after)
		}
	}
}

var _ = json.Marshal

// idGen 给测试用的 ID 生成器。
func idGen(t *testing.T) func() int64 {
	t.Helper()
	gen, err := id.New(0)
	if err != nil {
		t.Fatal(err)
	}
	return gen.Next
}

// CD-82：失败路径上的退款没成功时，补退款扫描把它补上，且只补一次。
//
// ⚠️ 起因：退款失败只留一行日志，各处注释说「由对账作业补」而那个作业并不存在 ——
// 一段超长的退款原因让 6 个失败任务的 288 积分一直没退回去，没有任何东西是红的。
func TestRecoverRefunds_失败了却没退成款的补上且只补一次(t *testing.T) {
	svc, r, ledger, pool := setup(t, 10)
	ctx := context.Background()
	fund(t, ledger, 100)
	res, err := accept(svc, "refund-miss")
	if err != nil {
		t.Fatal(err)
	}
	// 上游明确拒绝；执行器上不接退款，模拟「失败路径上的退款没成功」
	p := &fakeProvider{outcome: gendomain.SubmitRejected, httpCode: 400}
	if got, err := newExecutor(t, r, p, nil).Execute(ctx, res.Generation.ID); err != nil || got != genapp.OutcomeDone {
		t.Fatalf("应判失败：%v %v", got, err)
	}
	if bal, _ := ledger.Balance(ctx, payer); bal != 90 {
		t.Fatalf("前置：失败了却没退款，余额应为 90，得到 %d", bal)
	}

	sweeper := newExecutor(t, r, p, ledger)
	// 刚失败的不抢：给失败路径自己的退款留时间
	if n := sweeper.RecoverRefunds(ctx, time.Minute, 10); n != 0 {
		t.Fatalf("刚失败的不该被补，补了 %d 笔", n)
	}
	if _, err := pool.Exec(ctx, `update generation set updated_at = now() - interval '2 minutes'
		where id = $1`, res.Generation.ID); err != nil {
		t.Fatal(err)
	}
	if n := sweeper.RecoverRefunds(ctx, time.Minute, 10); n != 1 {
		t.Fatalf("应补 1 笔，补了 %d 笔", n)
	}
	if bal, _ := ledger.Balance(ctx, payer); bal != 100 {
		t.Fatalf("补退款后余额应回到 100，得到 %d", bal)
	}
	// 再扫一轮不再补：一次性由账本唯一索引兜底，这里连查都查不出来
	if n := sweeper.RecoverRefunds(ctx, time.Minute, 10); n != 0 {
		t.Fatalf("不该补第二次，补了 %d 笔", n)
	}
	if bal, _ := ledger.Balance(ctx, payer); bal != 100 {
		t.Fatalf("余额不该再变，得到 %d", bal)
	}
}

func nonEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
