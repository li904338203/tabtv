//go:build integration

// 扫描循环（AGT-25）的真库测试：会话 / 轮次 / 步骤 / 卡片 / 额度条目用 agent repo，
// 生成任务直接写 generation 表、经 wiring.AgentTaskStatuses 读（与 Worker 同一条路径），
// 账本用真库（billing/repo + wiring.AgentLedger）。
//
// ⚠️ 「只唤醒一次」「只退一次」都必须在真库里证明：保证它们的是会话行锁、CAS 与账本唯一索引，
// 内存假实现测的只是「我以为它会怎样」。
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	genrepo "github.com/li904338203/tabtv/backend/internal/generation/repo"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

func (e *runnerEnv) scanner(cfg agentapp.ScannerConfig, ledger agentapp.Ledger) *agentapp.Scanner {
	if ledger == nil {
		ledger = e.ledger
	}
	if cfg.RequeueQuiet == 0 {
		cfg.RequeueQuiet = 2 * time.Second
	}
	return agentapp.NewScanner(agentapp.ScannerDeps{
		Repo: e.r, Tasks: wiring.AgentTaskStatuses{Repo: genrepo.New(e.pool)}, Ledger: ledger,
		NextID: e.next, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, cfg)
}

func (e *runnerEnv) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func (e *runnerEnv) tick(t *testing.T, s *agentapp.Scanner) agentapp.Stats {
	t.Helper()
	st, err := s.Tick(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// age 把会话的 updated_at 与全部轮次的 created_at 往前拨，越过重投节流。
func (e *runnerEnv) age(t *testing.T, sessionID int64) {
	t.Helper()
	e.exec(t, `update agent_session set updated_at = now() - interval '1 minute' where id = $1`, sessionID)
	e.exec(t, `update agent_turn set created_at = created_at - interval '1 minute' where session_id = $1`, sessionID)
}

// insertGen 直接写一条生成任务（扫描只读它的状态 / 产物 / 账本）。
func (e *runnerEnv) insertGen(t *testing.T, status string, credits int, outputs int) int64 {
	t.Helper()
	id := e.next()
	e.exec(t, `insert into generation (id, workspace_id, payer_account_id, created_by_user_id, project_id,
		operation_id, model_id, result_type, credits_cost, idempotency_key, status, error_message)
		values ($1, $2, $3, $4, $5, 1, 1, 'image', $6, $7, $8, nullif($9, ''))`,
		id, ws, payer, user, project, credits, fmt.Sprintf("it-%d", id), status, errMsgFor(status))
	for i := 0; i < outputs; i++ {
		e.exec(t, `insert into generation_output (id, generation_id, idx, media_object_id, output_type)
			values ($1, $2, $3, $4, 'image')`, e.next(), id, i, e.next())
	}
	return id
}

func errMsgFor(status string) string {
	if status == "failed" {
		return "上游拒绝"
	}
	return ""
}

func (e *runnerEnv) setGen(t *testing.T, id int64, status string, outputs int) {
	t.Helper()
	e.exec(t, `update generation set status = $2 where id = $1`, id, status)
	for i := 0; i < outputs; i++ {
		e.exec(t, `insert into generation_output (id, generation_id, idx, media_object_id, output_type)
			values ($1, $2, $3, $4, 'image')`, e.next(), id, i, e.next())
	}
}

// waitingStep 记一个已受理生成任务、正在等它的步骤。
func (e *runnerEnv) waitingStep(t *testing.T, sessionID, turnID, genID int64) repo.Step {
	t.Helper()
	ctx := context.Background()
	st, err := e.r.InsertStep(ctx, e.pool, repo.Step{ID: e.next(), SessionID: sessionID, TurnID: turnID,
		Tool: "generate", Display: json.RawMessage(`{"title":"猫的海报","keep":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := e.r.SetStepWaiting(ctx, e.pool, st.ID, genID); err != nil || !ok {
		t.Fatalf("SetStepWaiting ok=%v err=%v", ok, err)
	}
	return st
}

// seedWaiting 建一个 waiting_task 会话：上一轮已成功结束，等 genIDs 这几个任务。
func (e *runnerEnv) seedWaiting(t *testing.T, genIDs ...int64) (repo.Session, repo.Turn, []repo.Step) {
	t.Helper()
	s, tr := e.seed(t, domain.StateWaitingTask, domain.TriggerUser)
	if _, err := e.r.FinishTurn(context.Background(), tr.ID, domain.TurnSuccess, 1, 1, "", ""); err != nil {
		t.Fatal(err)
	}
	var steps []repo.Step
	for _, g := range genIDs {
		steps = append(steps, e.waitingStep(t, s.ID, tr.ID, g))
	}
	return s, tr, steps
}

func (e *runnerEnv) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *runnerEnv) systemTurns(t *testing.T, sessionID int64) []repo.Turn {
	t.Helper()
	rows, err := e.pool.Query(context.Background(),
		`select id from agent_turn where session_id = $1 and trigger = 'system' order by created_at`, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	var out []repo.Turn
	for _, id := range ids {
		tr, err := e.r.GetTurn(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, tr)
	}
	return out
}

// outboxFor 数会话里指向某一轮的 outbox 消息。
func (e *runnerEnv) outboxFor(t *testing.T, sessionID, turnID int64) int {
	t.Helper()
	return e.count(t, `select count(*)::int from outbox_message
		where aggregate_type = 'agent' and aggregate_id = $1 and payload->>'turnId' = $2`,
		sessionID, strconv.FormatInt(turnID, 10))
}

func (e *runnerEnv) outboxAll(t *testing.T, sessionID int64) int {
	t.Helper()
	return e.count(t, `select count(*)::int from outbox_message where aggregate_type = 'agent' and aggregate_id = $1`,
		sessionID)
}

func (e *runnerEnv) ledgerTxCount(t *testing.T) int {
	t.Helper()
	return e.count(t, `select count(*)::int from credit_transaction where payer_account_id = $1`, payer)
}

// chargeRunning 把一轮做成「已扣费、已 running」（Runner 崩在模型调用中的样子）。
func (e *runnerEnv) chargeRunning(t *testing.T, tr repo.Turn, credits int) {
	t.Helper()
	ctx := context.Background()
	if _, err := e.r.SetTurnQuote(ctx, tr.ID, "m-agent", credits); err != nil {
		t.Fatal(err)
	}
	if err := e.ledger.Consume(ctx, payer, user, credits, agentapp.RefTypeTurn,
		strconv.FormatInt(tr.ID, 10), "Agent 对话"); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.r.MarkTurnRunning(ctx, tr.ID); err != nil || !ok {
		t.Fatalf("MarkTurnRunning ok=%v err=%v", ok, err)
	}
}

// expireLease 模拟 Worker 崩溃：租约还在某个已死的 runner 名下、但已过期。
func (e *runnerEnv) expireLease(t *testing.T, sessionID int64) {
	t.Helper()
	e.exec(t, `update agent_session set runner_id = 'dead-worker', runner_lease_until = now() - interval '1 second'
		where id = $1`, sessionID)
}

// ---------------------------------------------------------------- A 唤醒

func TestScanner_所等任务全部终态才唤醒(t *testing.T) {
	e := newRunnerEnv(t, 0)
	g1 := e.insertGen(t, "success", 0, 1)
	g2 := e.insertGen(t, "running", 0, 0)
	s, _, _ := e.seedWaiting(t, g1, g2)
	// 本用例只验唤醒；默认 20 秒的重投节流在全量集成负载下会被跨过
	// （整条用例跑过 18 秒），第二次 tick 就把 pending 的 system 轮又投一次。
	sc := e.scanner(agentapp.ScannerConfig{RequeueQuiet: 10 * time.Minute}, nil)

	// 只有 1 个终态：不唤醒。
	if st := e.tick(t, sc); st.Woken != 0 {
		t.Fatalf("只有一个任务终态时不应唤醒：%+v", st)
	}
	if got, _ := e.r.GetByID(context.Background(), s.ID); got.State != domain.StateWaitingTask {
		t.Fatalf("会话应仍在 waiting_task，实际 %s", got.State)
	}
	if n := len(e.systemTurns(t, s.ID)); n != 0 {
		t.Fatalf("不应建 system 轮，实际 %d", n)
	}

	// 2 个都终态：恰好建 1 个 system 轮并入队。
	e.setGen(t, g2, "success", 2)
	if st := e.tick(t, sc); st.Woken != 1 {
		t.Fatalf("应唤醒 1 个会话：%+v", st)
	}
	e.tick(t, sc) // 再扫一次不重复唤醒
	got, _ := e.r.GetByID(context.Background(), s.ID)
	if got.State != domain.StateExecuting {
		t.Fatalf("唤醒后应为 executing，实际 %s", got.State)
	}
	sys := e.systemTurns(t, s.ID)
	if len(sys) != 1 || sys[0].Status != domain.TurnPending {
		t.Fatalf("应恰好 1 个 pending 的 system 轮，实际 %+v", sys)
	}
	if n := e.outboxFor(t, s.ID, sys[0].ID); n != 1 {
		t.Fatalf("system 轮应入队 1 次，实际 %d", n)
	}
	// 唤醒轮不计费：Runner 跑它时不扣费（系统轮报价跳过）。
	if n := e.ledgerTxCount(t); n != 0 {
		t.Fatalf("唤醒不应产生账本流水，实际 %d", n)
	}
}

func TestScanner_唤醒前回写步骤终态(t *testing.T) {
	e := newRunnerEnv(t, 0)
	g1 := e.insertGen(t, "success", 0, 3)
	g2 := e.insertGen(t, "failed", 0, 0)
	g3 := e.insertGen(t, "cancelled", 0, 0)
	s, _, steps := e.seedWaiting(t, g1, g2, g3)
	sc := e.scanner(agentapp.ScannerConfig{}, nil)
	if st := e.tick(t, sc); st.Woken != 1 || st.StepsFinished != 3 {
		t.Fatalf("应唤醒并回写 3 个步骤：%+v", st)
	}
	want := []string{repo.StepSuccess, repo.StepFailed, repo.StepCancelled}
	for i, step := range steps {
		got, err := e.r.GetStep(context.Background(), step.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != want[i] || got.FinishedAt == nil {
			t.Fatalf("步骤 %d 应为 %s，实际 %s", i, want[i], got.Status)
		}
		var disp struct {
			Title  string `json:"title"`
			Keep   int    `json:"keep"`
			Result struct {
				Status  string            `json:"status"`
				Outputs []json.RawMessage `json:"outputs"`
				Error   string            `json:"error"`
			} `json:"result"`
		}
		if err := json.Unmarshal(got.Display, &disp); err != nil {
			t.Fatal(err)
		}
		if disp.Title != "猫的海报" || disp.Keep != 1 {
			t.Fatalf("原展示字段应保留：%s", got.Display)
		}
		if disp.Result.Status != want[i] {
			t.Fatalf("展示里的结局应为 %s：%s", want[i], got.Display)
		}
		if i == 0 && len(disp.Result.Outputs) != 3 {
			t.Fatalf("多产出应全部列出（3 个），实际 %d：%s", len(disp.Result.Outputs), got.Display)
		}
		if i == 1 && disp.Result.Error != "上游拒绝" {
			t.Fatalf("失败原因应带出来：%s", got.Display)
		}
	}
	// 唤醒说明写在 system 轮上。
	n := e.count(t, `select count(*)::int from agent_message m join agent_turn t on t.id = m.turn_id
		where m.session_id = $1 and m.role = 'system_event' and t.trigger = 'system'`, s.ID)
	if n != 1 {
		t.Fatalf("应写 1 条唤醒说明，实际 %d", n)
	}
}

func TestScanner_多实例并发只唤醒一次(t *testing.T) {
	e := newRunnerEnv(t, 0)
	g1 := e.insertGen(t, "success", 0, 1)
	g2 := e.insertGen(t, "failed", 0, 0)
	s, _, _ := e.seedWaiting(t, g1, g2)

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		sc := e.scanner(agentapp.ScannerConfig{}, nil) // 4 个实例
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := sc.Tick(context.Background(), 0); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	sys := e.systemTurns(t, s.ID)
	if len(sys) != 1 {
		t.Fatalf("4 个实例同时扫描应只建 1 个 system 轮，实际 %d", len(sys))
	}
	if n := e.outboxAll(t, s.ID); n != 1 {
		t.Fatalf("应只有 1 条 outbox，实际 %d", n)
	}
}

// ---------------------------------------------------------------- B 租约过期

func TestScanner_租约过期用原turnID重入队(t *testing.T) {
	e := newRunnerEnv(t, 100)
	s, tr := e.seed(t, domain.StateExecuting, domain.TriggerUser)
	e.chargeRunning(t, tr, 10)
	e.exec(t, `update agent_session set runner_id = 'live', runner_lease_until = now() + interval '1 minute' where id = $1`, s.ID)
	e.age(t, s.ID)
	sc := e.scanner(agentapp.ScannerConfig{}, nil)

	// 租约还在：不动。
	if st := e.tick(t, sc); st.Requeued != 0 {
		t.Fatalf("租约有效时不应重入队：%+v", st)
	}
	e.expireLease(t, s.ID)
	e.age(t, s.ID)
	if st := e.tick(t, sc); st.Requeued != 1 {
		t.Fatalf("租约过期应重入队 1 次：%+v", st)
	}
	e.tick(t, sc) // 节流期内再扫不重复投
	if n := e.outboxFor(t, s.ID, tr.ID); n != 1 {
		t.Fatalf("应用原 turnID 入队恰好 1 次，实际 %d", n)
	}
	if n := e.count(t, `select count(*)::int from agent_turn where session_id = $1`, s.ID); n != 1 {
		t.Fatalf("不应新建轮次，实际 %d 轮", n)
	}
}

func TestScanner_重入队的轮不重复扣费(t *testing.T) {
	e := newRunnerEnv(t, 100)
	s, tr := e.seed(t, domain.StatePlanning, domain.TriggerUser)
	e.chargeRunning(t, tr, 10)
	e.expireLease(t, s.ID)
	e.age(t, s.ID)
	sc := e.scanner(agentapp.ScannerConfig{}, nil)
	if st := e.tick(t, sc); st.Requeued != 1 {
		t.Fatalf("应重入队：%+v", st)
	}
	// 新的 Worker 接手这一轮（消息里是原 turnID）。
	out, err := e.runner(10, true).RunTurn(context.Background(), s.ID, tr.ID)
	if err != nil || out != agentapp.OutcomeDone {
		t.Fatalf("接手运行 outcome=%v err=%v", out, err)
	}
	consume, refund, net := e.ledgerCounts(t, tr.ID)
	if consume != 1 || refund != 0 || net != -10 {
		t.Fatalf("账本应仍只有 1 笔扣费：consume=%d refund=%d net=%d", consume, refund, net)
	}
	_, got := e.state(t, s.ID, tr.ID)
	if got.Status != domain.TurnSuccess {
		t.Fatalf("接手后应跑完，实际 %s", got.Status)
	}
}

func TestScanner_运行态无未结束轮回idle(t *testing.T) {
	e := newRunnerEnv(t, 0)
	s, tr := e.seed(t, domain.StateExecuting, domain.TriggerUser)
	if _, err := e.r.FinishTurn(context.Background(), tr.ID, domain.TurnSuccess, 1, 1, "", ""); err != nil {
		t.Fatal(err)
	}
	e.age(t, s.ID)
	sc := e.scanner(agentapp.ScannerConfig{}, nil)
	if st := e.tick(t, sc); st.Orphaned != 1 {
		t.Fatalf("应让孤立的运行态会话离开运行态：%+v", st)
	}
	if got, _ := e.r.GetByID(context.Background(), s.ID); got.State != domain.StateIdle {
		t.Fatalf("应回 idle，实际 %s", got.State)
	}
}

// ---------------------------------------------------------------- Q 排队轮

func TestScanner_无活租约的排队用户轮重新入队(t *testing.T) {
	e := newRunnerEnv(t, 100)
	// 用户在上一轮运行中发的消息：上一轮已结束、会话回 idle，第二轮还 pending。
	s, first := e.seed(t, domain.StatePlanning, domain.TriggerUser)
	queued, err := e.r.InsertTurn(context.Background(), e.pool, repo.Turn{ID: e.next(), SessionID: s.ID,
		Trigger: domain.TriggerUser, IdempotencyKey: "msg-2"})
	if err != nil {
		t.Fatal(err)
	}
	// 第一轮跑完；第二轮的消息在它跑的时候到达 → Busy 丢弃（这里直接不投，等价）。
	out, err := e.runner(10, true).RunTurn(context.Background(), s.ID, first.ID)
	if err != nil || out != agentapp.OutcomeDone {
		t.Fatalf("第一轮 outcome=%v err=%v", out, err)
	}
	if got, _ := e.r.GetByID(context.Background(), s.ID); got.State != domain.StateIdle {
		t.Fatalf("第一轮结束应回 idle，实际 %s", got.State)
	}
	// 迟到的那条消息被取到：会话已不在运行态 → Runner 不作废它，留给扫描。
	if out, err := e.runner(10, true).RunTurn(context.Background(), s.ID, queued.ID); err != nil ||
		out != agentapp.OutcomeBusy {
		t.Fatalf("会话离开运行态后取到排队轮应 Busy，outcome=%v err=%v", out, err)
	}
	if _, q := e.state(t, s.ID, queued.ID); q.Status != domain.TurnPending {
		t.Fatalf("排队轮应仍 pending，实际 %s", q.Status)
	}

	// 有人持有租约时不动。
	e.exec(t, `update agent_session set runner_id = 'live', runner_lease_until = now() + interval '1 minute' where id = $1`, s.ID)
	e.age(t, s.ID)
	sc := e.scanner(agentapp.ScannerConfig{}, nil)
	if st := e.tick(t, sc); st.QueuedResumed != 0 {
		t.Fatalf("租约有效时不应动排队轮：%+v", st)
	}
	e.exec(t, `update agent_session set runner_id = null, runner_lease_until = null where id = $1`, s.ID)
	e.age(t, s.ID)
	if st := e.tick(t, sc); st.QueuedResumed != 1 {
		t.Fatalf("应重新入队排队轮：%+v", st)
	}
	e.tick(t, sc)
	got, _ := e.r.GetByID(context.Background(), s.ID)
	if got.State != domain.StatePlanning {
		t.Fatalf("按「直接发消息」应回 planning，实际 %s", got.State)
	}
	if n := e.outboxFor(t, s.ID, queued.ID); n != 1 {
		t.Fatalf("排队轮应用原 turnID 入队 1 次，实际 %d", n)
	}
	// 这一次能跑完，且只为它扣 1 次费。
	if out, err := e.runner(10, true).RunTurn(context.Background(), s.ID, queued.ID); err != nil ||
		out != agentapp.OutcomeDone {
		t.Fatalf("重新入队后 outcome=%v err=%v", out, err)
	}
	if consume, _, _ := e.ledgerCounts(t, queued.ID); consume != 1 {
		t.Fatalf("排队轮应扣费 1 次，实际 %d", consume)
	}
}

func TestScanner_已取消会话的排队轮作废(t *testing.T) {
	e := newRunnerEnv(t, 0)
	s, tr := e.seed(t, domain.StateCancelled, domain.TriggerUser)
	e.age(t, s.ID)
	sc := e.scanner(agentapp.ScannerConfig{}, nil)
	if st := e.tick(t, sc); st.QueuedDropped != 1 {
		t.Fatalf("应作废已取消会话的排队轮：%+v", st)
	}
	if _, got := e.state(t, s.ID, tr.ID); got.Status != domain.TurnCancelled {
		t.Fatalf("应为 cancelled，实际 %s", got.Status)
	}
	if n := e.outboxAll(t, s.ID); n != 0 {
		t.Fatalf("不应入队，实际 %d", n)
	}
}

// ---------------------------------------------------------------- C 超时轮

type failingLedger struct {
	agentapp.Ledger
	mu    sync.Mutex
	calls int
}

func (f *failingLedger) Refund(context.Context, int64, string, string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return errors.New("账本暂时不可用")
}

func staleCfg() agentapp.ScannerConfig {
	return agentapp.ScannerConfig{StaleTurnAfter: time.Hour}
}

func TestScanner_超时轮退款并判失败且只退一次(t *testing.T) {
	e := newRunnerEnv(t, 100)
	s, tr := e.seed(t, domain.StateExecuting, domain.TriggerUser)
	e.chargeRunning(t, tr, 10)
	e.exec(t, `update agent_turn set started_at = now() - interval '2 hours' where id = $1`, tr.ID)
	e.expireLease(t, s.ID)
	e.age(t, s.ID)
	sc := e.scanner(staleCfg(), nil)

	st := e.tick(t, sc)
	if st.StaleFailed != 1 || st.Requeued != 0 {
		t.Fatalf("超时轮应判死且不重入队（C 优先于 B）：%+v", st)
	}
	sess, got := e.state(t, s.ID, tr.ID)
	if got.Status != domain.TurnFailed || got.ErrorCode != agentapp.ErrCodeTurnStale {
		t.Fatalf("应标 failed/turn_stale，实际 %s/%s", got.Status, got.ErrorCode)
	}
	if sess.State != domain.StateIdle {
		t.Fatalf("会话应回 idle，实际 %s", sess.State)
	}
	e.tick(t, sc)
	e.tick(t, sc)
	consume, refund, net := e.ledgerCounts(t, tr.ID)
	if consume != 1 || refund != 1 || net != 0 {
		t.Fatalf("应退款恰好 1 笔：consume=%d refund=%d net=%d", consume, refund, net)
	}
	if n := e.outboxAll(t, s.ID); n != 0 {
		t.Fatalf("判死的轮不应再入队，实际 %d", n)
	}
}

func TestScanner_超时轮先退款后判失败(t *testing.T) {
	e := newRunnerEnv(t, 100)
	s, tr := e.seed(t, domain.StateExecuting, domain.TriggerUser)
	e.chargeRunning(t, tr, 10)
	e.exec(t, `update agent_turn set started_at = now() - interval '2 hours' where id = $1`, tr.ID)
	e.expireLease(t, s.ID)

	// 退款失败：不判失败（判了下次就捞不到、钱永远扣着）。
	bad := &failingLedger{Ledger: e.ledger}
	if _, err := e.scanner(staleCfg(), bad).Tick(context.Background(), 0); err == nil {
		t.Fatal("退款失败应报出来")
	}
	if bad.calls != 1 {
		t.Fatalf("应先尝试退款，实际调用 %d 次", bad.calls)
	}
	if _, got := e.state(t, s.ID, tr.ID); got.Status != domain.TurnRunning {
		t.Fatalf("退款没成时轮次应仍 running，实际 %s", got.Status)
	}
	// 账本恢复后：退款 1 笔、判失败。
	if st := e.tick(t, e.scanner(staleCfg(), nil)); st.StaleFailed != 1 {
		t.Fatalf("账本恢复后应判死：%+v", st)
	}
	if _, refund, _ := e.ledgerCounts(t, tr.ID); refund != 1 {
		t.Fatalf("应退款 1 笔，实际 %d", refund)
	}
}

func TestScanner_租约有效的长轮不判死(t *testing.T) {
	e := newRunnerEnv(t, 100)
	s, tr := e.seed(t, domain.StateExecuting, domain.TriggerUser)
	e.chargeRunning(t, tr, 10)
	e.exec(t, `update agent_turn set started_at = now() - interval '2 hours' where id = $1`, tr.ID)
	e.exec(t, `update agent_session set runner_id = 'live', runner_lease_until = now() + interval '1 minute' where id = $1`, s.ID)
	if st := e.tick(t, e.scanner(staleCfg(), nil)); st.StaleFailed != 0 {
		t.Fatalf("租约还在续期的轮不能判死：%+v", st)
	}
	if _, refund, _ := e.ledgerCounts(t, tr.ID); refund != 0 {
		t.Fatalf("不应退款，实际 %d", refund)
	}
}

func TestStaleTurnAfterFor_由配置推导(t *testing.T) {
	cases := []struct {
		calls int
		want  time.Duration
	}{
		{0, 25 * time.Minute},  // 默认 20 次 × 60s + 5min
		{3, 20 * time.Minute},  // 下限
		{20, 25 * time.Minute}, // 默认值：已超过 20 分钟
		{40, 45 * time.Minute},
	}
	for _, c := range cases {
		if got := agentapp.StaleTurnAfterFor(c.calls); got != c.want {
			t.Errorf("maxCalls=%d：期望 %s，得到 %s", c.calls, c.want, got)
		}
	}
}

// ---------------------------------------------------------------- D 卡片过期

func TestScanner_卡片过期会话回idle不扣费(t *testing.T) {
	e := newRunnerEnv(t, 100)
	s, tr := e.seed(t, domain.StateAwaitingInput, domain.TriggerUser)
	if _, err := e.r.FinishTurn(context.Background(), tr.ID, domain.TurnSuccess, 1, 1, "", ""); err != nil {
		t.Fatal(err)
	}
	card, err := e.r.InsertCard(context.Background(), e.pool, repo.Card{ID: e.next(), SessionID: s.ID,
		StepID: e.next(), Kind: domain.CardChoice, Payload: json.RawMessage(`{"resumeState":"planning"}`)})
	if err != nil {
		t.Fatal(err)
	}
	before := e.ledgerTxCount(t)
	sc := e.scanner(agentapp.ScannerConfig{CardExpire: 168 * time.Hour}, nil)

	// 6 天：还没过期。
	e.exec(t, `update agent_card set created_at = now() - interval '6 days' where id = $1`, card.ID)
	if st := e.tick(t, sc); st.CardsExpired != 0 {
		t.Fatalf("未到期不应过期：%+v", st)
	}
	e.exec(t, `update agent_card set created_at = now() - interval '8 days' where id = $1`, card.ID)
	if st := e.tick(t, sc); st.CardsExpired != 1 {
		t.Fatalf("应过期 1 张：%+v", st)
	}
	got, err := e.r.GetCard(context.Background(), s.ID, card.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.CardExpired {
		t.Fatalf("卡片应为 expired，实际 %s", got.Status)
	}
	if sess, _ := e.r.GetByID(context.Background(), s.ID); sess.State != domain.StateIdle {
		t.Fatalf("会话应回 idle，实际 %s", sess.State)
	}
	if after := e.ledgerTxCount(t); after != before {
		t.Fatalf("卡片过期不应有账本流水：%d → %d", before, after)
	}
}

// ---------------------------------------------------------------- E 额度归还

func TestScanner_生成失败后额度条目标refunded(t *testing.T) {
	e := newRunnerEnv(t, 100)
	ctx := context.Background()
	g := e.insertGen(t, "failed", 10, 0)
	// 生成域受理时扣了 10（refType=generation）。
	if _, err := e.bill.Consume(ctx, billingrepo.ConsumeInput{PayerID: payer, Amount: 10, RefType: "generation",
		RefID: strconv.FormatInt(g, 10), UserID: user, Reason: "生成"}); err != nil {
		t.Fatal(err)
	}
	s, tr := e.seed(t, domain.StateIdle, domain.TriggerUser)
	if _, err := e.r.FinishTurn(ctx, tr.ID, domain.TurnSuccess, 1, 1, "", ""); err != nil {
		t.Fatal(err)
	}
	step := e.waitingStep(t, s.ID, tr.ID, g)
	entryID := e.next()
	if err := reserve(t, e.r, s.ID, entryID, step.ID, 10); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.r.MarkCharged(ctx, entryID, g); err != nil || !ok {
		t.Fatalf("MarkCharged ok=%v err=%v", ok, err)
	}
	used := func() int {
		tx, err := e.r.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		n, err := e.r.UsedInTx(ctx, tx, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	sc := e.scanner(agentapp.ScannerConfig{}, nil)

	// 生成失败但退款还没到账：额度不归还（钱没回来就先放额度，Agent 能再花一次）。
	if st := e.tick(t, sc); st.BudgetRefunded != 0 {
		t.Fatalf("未退款时不应归还额度：%+v", st)
	}
	if n := used(); n != 10 {
		t.Fatalf("已占用额度应仍为 10，实际 %d", n)
	}
	// 生成域退款到账 → 条目 refunded，步骤补 failed。
	if _, err := e.bill.Refund(ctx, billingrepo.RefundInput{PayerID: payer, RefType: "generation",
		RefID: strconv.FormatInt(g, 10), Reason: "生成失败", CompensationValidFor: 30 * 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	if st := e.tick(t, sc); st.BudgetRefunded != 1 {
		t.Fatalf("退款后应归还额度：%+v", st)
	}
	e.tick(t, sc)
	entries, err := e.r.BudgetBySession(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Status != repo.BudgetRefunded {
		t.Fatalf("条目应为 refunded：%+v", entries)
	}
	if n := used(); n != 0 {
		t.Fatalf("UsedInTx 不应再计入它，实际 %d", n)
	}
	got, err := e.r.GetStep(ctx, step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != repo.StepFailed {
		t.Fatalf("步骤应补 failed，实际 %s", got.Status)
	}
}

// 04 文档 P-17：Runner 结算时退款失败只打一行日志，此前没有任何地方会再退 —— 钱扣着、轮次已判失败。
func TestScanner_失败轮没退成的对话费补退且只退一次(t *testing.T) {
	e := newRunnerEnv(t, 100)
	_, tr := e.seed(t, domain.StateIdle, domain.TriggerUser)
	e.chargeRunning(t, tr, 10)
	if _, err := e.r.FinishTurn(context.Background(), tr.ID, domain.TurnFailed, 1, 0, agentapp.ErrCodeModel,
		"上游 502"); err != nil {
		t.Fatal(err)
	}
	sc := e.scanner(staleCfg(), nil)
	if st := e.tick(t, sc); st.TurnRefunded != 0 {
		t.Fatalf("刚结束的轮给 Runner 留结算时间，不抢着退：%+v", st)
	}
	e.exec(t, `update agent_turn set finished_at = now() - interval '5 minutes' where id = $1`, tr.ID)
	if st := e.tick(t, sc); st.TurnRefunded != 1 {
		t.Fatalf("应补退 1 笔：%+v", st)
	}
	e.tick(t, sc)
	consume, refund, net := e.ledgerCounts(t, tr.ID)
	if consume != 1 || refund != 1 || net != 0 {
		t.Fatalf("补退恰好 1 笔：consume=%d refund=%d net=%d", consume, refund, net)
	}

	// 成功的轮、没扣过费的轮都不碰。
	_, ok := e.seed(t, domain.StateIdle, domain.TriggerUser)
	e.chargeRunning(t, ok, 10)
	if _, err := e.r.FinishTurn(context.Background(), ok.ID, domain.TurnSuccess, 1, 1, "", ""); err != nil {
		t.Fatal(err)
	}
	e.exec(t, `update agent_turn set finished_at = now() - interval '5 minutes' where id = $1`, ok.ID)
	if st := e.tick(t, sc); st.TurnRefunded != 0 {
		t.Fatalf("成功的轮不退：%+v", st)
	}
}
