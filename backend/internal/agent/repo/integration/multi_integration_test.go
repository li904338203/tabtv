//go:build integration

// 多段工具（AGT-50）的真库测试：额度条目按业务引用（组 / 整图运行 / 渲染）结算与部分退回、
// 扫描循环按引用唤醒、同一步骤重入不再执行、多段并发不超额度。
//
// ⚠️ 「已扣 / 退回与账本逐笔一致」只能在真库里证明：结算的两个数只从 credit_transaction 来。
package integration

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	agenttools "github.com/li904338203/tabtv/backend/internal/agent/tools"
	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	genrepo "github.com/li904338203/tabtv/backend/internal/generation/repo"
	timelinerepo "github.com/li904338203/tabtv/backend/internal/timeline/repo"
	"github.com/li904338203/tabtv/backend/internal/wiring"
	workflowrepo "github.com/li904338203/tabtv/backend/internal/workflow/repo"
)

// refScanner 是接上了「按业务引用查终态」的扫描循环（与 Worker 同一个 wiring.AgentRefStatuses）。
func (e *runnerEnv) refScanner() *agentapp.Scanner {
	return agentapp.NewScanner(agentapp.ScannerDeps{
		Repo: e.r, Tasks: wiring.AgentTaskStatuses{Repo: genrepo.New(e.pool)}, Ledger: e.ledger,
		Refs: wiring.AgentRefStatuses{Gen: genrepo.New(e.pool), Workflow: workflowrepo.New(e.pool),
			Timeline: timelinerepo.New(e.pool, e.next), Billing: e.bill},
		NextID: e.next, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, agentapp.ScannerConfig{RequeueQuiet: 2 * time.Second})
}

// consumeGen 在账本上为一个生成任务扣费（生成域受理时就是这么记的：ref_type=generation）。
func (e *runnerEnv) consume(t *testing.T, refType string, id int64, amount int) {
	t.Helper()
	if _, err := e.bill.Consume(context.Background(), billingrepo.ConsumeInput{PayerID: payer, Amount: amount,
		RefType: refType, RefID: strconv.FormatInt(id, 10), UserID: user, Reason: "it"}); err != nil {
		t.Fatal(err)
	}
}

func (e *runnerEnv) refund(t *testing.T, refType string, id int64) {
	t.Helper()
	if _, err := e.bill.Refund(context.Background(), billingrepo.RefundInput{PayerID: payer, RefType: refType,
		RefID: strconv.FormatInt(id, 10), Reason: "it", CompensationValidFor: 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
}

// ledgerSums 是账本上这些引用的实扣 / 退回合计（对照口径）。
func (e *runnerEnv) ledgerSums(t *testing.T, refType string, ids []int64) (consumed, refunded int) {
	t.Helper()
	strs := make([]string, len(ids))
	for i, id := range ids {
		strs[i] = strconv.FormatInt(id, 10)
	}
	if err := e.pool.QueryRow(context.Background(), `
		select coalesce(sum(case when type = 'consume' then abs(change) end), 0)::int,
		       coalesce(sum(case when type = 'refund' then abs(change) end), 0)::int
		from credit_transaction where payer_account_id = $1 and ref_type = $2 and ref_id = any($3)`,
		payer, refType, strs).Scan(&consumed, &refunded); err != nil {
		t.Fatal(err)
	}
	return consumed, refunded
}

// multiStep 建一个步骤并在额度内受理一次（run 返回给定的业务引用），之后步骤挂 waiting。
func (e *runnerEnv) multiStep(t *testing.T, s repo.Session, turnID int64, tool string, quote int,
	res agentapp.PaidResult) repo.Step {
	t.Helper()
	ctx := context.Background()
	st, err := e.r.InsertStep(ctx, e.pool, repo.Step{ID: e.next(), SessionID: s.ID, TurnID: turnID, Tool: tool})
	if err != nil {
		t.Fatal(err)
	}
	b := agentapp.NewBudget(e.r, e.next, nil)
	if _, susp, err := b.PaidCall(ctx, paidRC(s, st.ID, 1), quote, func(string) (agentapp.PaidResult, error) {
		return res, nil
	}); err != nil || susp != nil {
		t.Fatalf("PaidCall err=%v susp=%v", err, susp)
	}
	if ok, err := e.r.SetStepWaiting(ctx, e.pool, st.ID, 0); err != nil || !ok {
		t.Fatalf("SetStepWaiting ok=%v err=%v", ok, err)
	}
	return st
}

// 组部分成功：3 项各扣 10，失败 1 项由生成域退回 → 条目仍 charged、refunded=10，
// 「已扣 / 退回」与账本 credit_transaction 逐笔一致；退款没到账之前不结算。
func TestPaidCall_组部分成功回写退回额度(t *testing.T) {
	e := newRunnerEnv(t, 1000)
	ctx := context.Background()
	s := paidSession(t, e.r, e.pool, e.next, 100)
	g1, g2, g3 := e.insertGen(t, "success", 10, 1), e.insertGen(t, "success", 10, 1), e.insertGen(t, "failed", 10, 0)
	ids := []int64{g1, g2, g3}
	for _, g := range ids {
		e.consume(t, "generation", g, 10)
	}
	st := e.multiStep(t, s, e.next(), "group_execute", 30, agentapp.PaidResult{Credits: 30,
		RefType: "script_group", RefID: e.next(), RefIDs: ids})

	sc := e.refScanner()
	e.tick(t, sc)
	entry, err := e.r.BudgetByStep(ctx, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if entry.SettledAt != nil || entry.Refunded != 0 {
		t.Fatalf("失败项的退款还没到账，不应结算：%+v", entry)
	}
	step, _ := e.r.GetStep(ctx, st.ID)
	if step.Status != repo.StepSuccess {
		t.Errorf("组终态（部分成功）后步骤应补 success，实际 %s", step.Status)
	}

	e.refund(t, "generation", g3) // 生成域的退款到账
	stats := e.tick(t, sc)
	if stats.BudgetSettled != 1 {
		t.Fatalf("应结算 1 条，实际 %+v", stats)
	}
	entry, _ = e.r.BudgetByStep(ctx, st.ID)
	consumed, refunded := e.ledgerSums(t, "generation", ids)
	if entry.Status != repo.BudgetCharged || entry.SettledAt == nil {
		t.Fatalf("部分退回的条目应仍是 charged 且已结算：%+v", entry)
	}
	if entry.Quoted != consumed || entry.Refunded != refunded || consumed != 30 || refunded != 10 {
		t.Fatalf("额度账（%d / %d）与账本（%d / %d）应逐笔一致", entry.Quoted, entry.Refunded, consumed, refunded)
	}
	tx, err := e.r.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	used, err := e.r.UsedInTx(ctx, tx, s.ID)
	_ = tx.Rollback(ctx)
	if err != nil || used != 20 {
		t.Fatalf("占用应为 quoted − refunded = 20，实际 %d（%v）", used, err)
	}
	// 再扫一遍不重复结算。
	if st2 := e.tick(t, sc); st2.BudgetSettled != 0 {
		t.Errorf("已结算的条目不应再处理：%+v", st2)
	}
}

// 渲染终态唤醒会话：渲染在跑 → 不唤醒；成功 → 唤醒（system 轮 + 步骤 success），按账本结算。
func TestScanner_渲染终态唤醒会话(t *testing.T) {
	e := newRunnerEnv(t, 1000)
	ctx := context.Background()
	s, tr := e.seed(t, domain.StateExecuting, domain.TriggerUser)
	if _, err := e.r.FinishTurn(ctx, tr.ID, domain.TurnSuccess, 1, 1, "", ""); err != nil {
		t.Fatal(err)
	}
	s.ApprovedBudget = 100
	e.exec(t, `update agent_session set approved_budget = 100 where id = $1`, s.ID)
	renderID := e.next()
	e.exec(t, `insert into render_job (id, workspace_id, timeline_id, timeline_version, snapshot,
		payer_account_id, created_by_user_id, status, credits_cost)
		values ($1, $2, $3, 1, '{}'::jsonb, $4, $5, 'running', 12)`, renderID, ws, e.next(), payer, user)
	e.consume(t, "render", renderID, 12)
	st := e.multiStep(t, s, tr.ID, "render", 12, agentapp.PaidResult{Credits: 12, RefType: "render_job", RefID: renderID})
	e.exec(t, `update agent_session set state = 'waiting_task' where id = $1`, s.ID)

	sc := e.refScanner()
	if stats := e.tick(t, sc); stats.Woken != 0 {
		t.Fatalf("渲染还在跑，不应唤醒：%+v", stats)
	}
	e.exec(t, `update render_job set status = 'success', output_media_id = $2 where id = $1`, renderID, e.next())
	stats := e.tick(t, sc)
	if stats.Woken != 1 {
		t.Fatalf("渲染终态后应唤醒 1 次：%+v", stats)
	}
	got, _ := e.r.GetByID(ctx, s.ID)
	if got.State != domain.StateExecuting {
		t.Errorf("唤醒后应回 executing，实际 %s", got.State)
	}
	step, _ := e.r.GetStep(ctx, st.ID)
	if step.Status != repo.StepSuccess {
		t.Errorf("步骤应 success，实际 %s", step.Status)
	}
	if n := len(e.systemTurns(t, s.ID)); n != 1 {
		t.Errorf("应建 1 个 system 轮，实际 %d", n)
	}
	if n := e.count(t, `select count(*)::int from agent_message where session_id = $1 and role = 'system_event'
		and content like '%渲染任务%'`, s.ID); n != 1 {
		t.Errorf("唤醒说明应写清渲染结局，实际 %d 条", n)
	}
	// 结算：渲染成功不退款，条目 charged、quoted = 账本实扣。
	e.tick(t, sc)
	entry, _ := e.r.BudgetByStep(ctx, st.ID)
	if entry.SettledAt == nil || entry.Quoted != 12 || entry.Refunded != 0 || entry.Status != repo.BudgetCharged {
		t.Errorf("渲染成功后应按账本结算：%+v", entry)
	}
}

// 整图运行终态唤醒会话：运行没结束不唤醒（哪怕已派发的子任务都结束了）；结束后唤醒，
// 结算时把运行的全部子任务写回 ref_ids，并按账本记退回。
func TestScanner_整图运行终态唤醒会话(t *testing.T) {
	e := newRunnerEnv(t, 1000)
	ctx := context.Background()
	s, tr := e.seed(t, domain.StateExecuting, domain.TriggerUser)
	if _, err := e.r.FinishTurn(ctx, tr.ID, domain.TurnSuccess, 1, 1, "", ""); err != nil {
		t.Fatal(err)
	}
	e.exec(t, `update agent_session set approved_budget = 100 where id = $1`, s.ID)
	s.ApprovedBudget = 100
	runID := e.next()
	e.exec(t, `insert into workflow_run (id, workspace_id, project_id, payer_account_id, created_by_user_id,
		graph_snapshot, status) values ($1, $2, $3, $4, $5, '{}'::jsonb, 'running')`, runID, ws, project, payer, user)
	gOK, gBad := e.insertGen(t, "success", 20, 1), e.insertGen(t, "failed", 15, 0)
	reused := e.insertGen(t, "success", 99, 1) // 复用的节点：这次没扣费，不进这一笔
	for _, n := range []struct {
		node   string
		gen    int64
		status string
		reused bool
	}{{"a", gOK, "success", false}, {"b", gBad, "failed", false}, {"c", reused, "success", true},
		{"d", 0, "pending", false}} {
		e.exec(t, `insert into workflow_node_run (id, run_id, node_id, status, generation_id, is_reused)
			values ($1, $2, $3, $4, nullif($5::bigint, 0), $6)`, e.next(), runID, n.node, n.status, n.gen, n.reused)
	}
	e.consume(t, "generation", gOK, 20)
	e.consume(t, "generation", gBad, 15)
	e.refund(t, "generation", gBad)
	st := e.multiStep(t, s, tr.ID, "run_workflow", 60, agentapp.PaidResult{Credits: -1, RefType: "workflow_run",
		RefID: runID})
	e.exec(t, `update agent_session set state = 'waiting_task' where id = $1`, s.ID)

	sc := e.refScanner()
	if stats := e.tick(t, sc); stats.Woken != 0 {
		t.Fatalf("运行还在推进（节点 d 未派发），不应唤醒：%+v", stats)
	}
	e.exec(t, `update workflow_node_run set status = 'blocked', error_message = '上游没有产出' where run_id = $1 and node_id = 'd'`, runID)
	e.exec(t, `update workflow_run set status = 'partial_success' where id = $1`, runID)
	if stats := e.tick(t, sc); stats.Woken != 1 {
		t.Fatalf("运行终态后应唤醒：%+v", stats)
	}
	step, _ := e.r.GetStep(ctx, st.ID)
	if step.Status != repo.StepSuccess {
		t.Errorf("部分成功的运行，步骤应 success（缺口在说明里），实际 %s", step.Status)
	}
	e.tick(t, sc)
	entry, _ := e.r.BudgetByStep(ctx, st.ID)
	if entry.SettledAt == nil || entry.Quoted != 35 || entry.Refunded != 15 {
		t.Fatalf("应按账本结算：实扣 35（不含复用）、退回 15，实际 %+v", entry)
	}
	if len(entry.RefIDs) != 2 {
		t.Errorf("结算时应写回本次运行的子任务（不含复用）：%v", entry.RefIDs)
	}
}

// 同一步骤重入（租约过期重放）：条目已挂着引用 → 回那一次，不再执行 run。
func TestPaidCall_同一步骤重入回已有引用不再执行(t *testing.T) {
	r, pool, next := setup(t)
	s := paidSession(t, r, pool, next, 100)
	b := agentapp.NewBudget(r, next, nil)
	rc := paidRC(s, next(), 1)
	var runs int
	run := func(string) (agentapp.PaidResult, error) {
		runs++
		return agentapp.PaidResult{Credits: 30, RefType: "workflow_run", RefID: 4242}, nil
	}
	first, _, err := b.PaidCall(context.Background(), rc, 30, run)
	if err != nil {
		t.Fatal(err)
	}
	again, susp, err := b.PaidCall(context.Background(), rc, 30, run)
	if err != nil || susp != nil {
		t.Fatalf("err=%v susp=%v", err, susp)
	}
	if runs != 1 || !again.Replayed || again.RefID != first.RefID || again.RefType != "workflow_run" {
		t.Fatalf("重入应回已有运行且不再执行：runs=%d again=%+v", runs, again)
	}
	if n := occupied(t, pool, s.ID); n != 30 {
		t.Errorf("重入不应再占一份额度，占用 %d", n)
	}
}

// 多段工具并发：已有一条部分退回的条目（占 10），额度 30，8 个各报 10 → 恰好 2 个进 run。
func TestPaidCall_多段工具并发不超额度(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	s := paidSession(t, r, pool, next, 30)
	b := agentapp.NewBudget(r, next, nil)
	// 一条组执行：报 20，结算后退回 10 → 只占 10。
	prev := paidRC(s, next(), 1)
	if _, _, err := b.PaidCall(ctx, prev, 20, func(string) (agentapp.PaidResult, error) {
		return agentapp.PaidResult{Credits: 20, RefType: "script_group", RefID: 7, RefIDs: []int64{1, 2}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	e, err := r.BudgetByStep(ctx, prev.StepID)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := r.SettleEntry(ctx, e.ID, 20, 10, nil); err != nil || !ok {
		t.Fatalf("SettleEntry ok=%v err=%v", ok, err)
	}

	var (
		wg       sync.WaitGroup
		ran      atomic.Int32
		suspends atomic.Int32
	)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, susp, err := b.PaidCallTool(ctx, paidRC(s, next(), 1), "render", 10,
				func(string) (agentapp.PaidResult, error) {
					ran.Add(1)
					return agentapp.PaidResult{Credits: 10, RefType: "render_job", RefID: next()}, nil
				})
			if err != nil {
				t.Error(err)
			}
			if susp != nil {
				suspends.Add(1)
			}
		}()
	}
	wg.Wait()
	if ran.Load() != 2 || suspends.Load() != 6 {
		t.Fatalf("应恰好 2 次进 run、6 次挂起追加额度：ran=%d suspends=%d", ran.Load(), suspends.Load())
	}
	tx, err := r.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	used, err := r.UsedInTx(ctx, tx, s.ID)
	_ = tx.Rollback(ctx)
	if err != nil || used != 30 {
		t.Fatalf("占用应恰好用尽 30，实际 %d（%v）", used, err)
	}
	var cards int
	if err := pool.QueryRow(ctx, `select count(*)::int from agent_card where session_id = $1 and kind = 'budget'`,
		s.ID).Scan(&cards); err != nil || cards != 6 {
		t.Errorf("应写 6 张追加额度卡，实际 %d（%v）", cards, err)
	}
	// 追加额度卡带工具名。
	var payload json.RawMessage
	if err := pool.QueryRow(ctx, `select payload from agent_card where session_id = $1 and kind = 'budget' limit 1`,
		s.ID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var p agentapp.BudgetCardPayload
	if err := json.Unmarshal(payload, &p); err != nil || p.Tool != "render" {
		t.Errorf("追加额度卡应写明工具：%s", payload)
	}
}

// 规划态发给模型的请求体不含任何多段工具（注册了也不下发）；模型凭幻觉调用时二次校验拒绝且不扣费。
func TestPlanningTurn_请求体不含多段付费工具(t *testing.T) {
	e := newPlanEnv(t, 100)
	ctx := context.Background()
	for _, tool := range agenttools.MultiTools(agenttools.MultiDeps{}) {
		if err := e.reg.Register(tool); err != nil {
			t.Fatal(err)
		}
	}
	s, err := e.r.Create(ctx, e.pool, repo.Session{ID: e.next(), WorkspaceID: ws, ProjectID: project,
		PayerAccountID: payer, CreatedByUserID: user, State: domain.StatePlanning,
		Prefs: json.RawMessage(`{"reasoning":{"model":"m-agent"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	tr := e.userTurn(t, s.ID, "做一条一分钟的西游短片")
	chat := &planChat{outs: []agentapp.ChatOutput{
		planCall("c1", "run_workflow", `{}`),
		planCall("c2", "render", `{"timelineId":"1"}`),
	}}
	if out, err := e.runner(chat).RunTurn(ctx, s.ID, tr.ID); err != nil || out != agentapp.OutcomeDone {
		t.Fatalf("outcome=%v err=%v", out, err)
	}
	multi := map[string]bool{}
	for _, tool := range agenttools.MultiTools(agenttools.MultiDeps{}) {
		multi[string(tool.Name())] = true
	}
	for i, names := range chat.tools {
		for _, n := range names {
			if multi[n] {
				t.Fatalf("第 %d 次请求下发了多段工具 %s：%v", i+1, n, names)
			}
		}
	}
	assertNoPaidTools(t, chat)
	var failed int
	if err := e.pool.QueryRow(ctx, `select count(*)::int from agent_step where session_id = $1
		and tool in ('run_workflow','render') and status = 'failed'`, s.ID).Scan(&failed); err != nil || failed != 2 {
		t.Fatalf("幻觉调用应被二次校验拒绝（步骤 failed），实际 %d（%v）", failed, err)
	}
	var entries int
	if err := e.pool.QueryRow(ctx, `select count(*)::int from agent_budget_entry where session_id = $1`,
		s.ID).Scan(&entries); err != nil || entries != 0 {
		t.Fatalf("规划态不应产生任何额度条目，实际 %d（%v）", entries, err)
	}
	// 同一份注册表在执行态会下发它们（证明上面不是因为根本没注册）。
	names := map[string]bool{}
	for _, d := range e.reg.Schemas(domain.AllowedTools(domain.StateExecuting, nil)) {
		names[d.Name] = true
	}
	for n := range multi {
		if !names[n] {
			t.Errorf("执行态应下发 %s", n)
		}
	}
}
