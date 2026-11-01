//go:build integration

// 批量变体（MKT-T-27、MKD-10）：停后续、部分成功、退款恰好一次、重试换幂等键、并发不超扣、
// 批准兜底与画框排列 —— 真库 + 真受理 + 真账本 + 真画布 + 真 Runner 写 agent_step。
//
// ⚠️ 「停后续」「部分成功」都按 agent_step 的 plan_item_key + error_code 现算，这里断言的正是
// 「Runner 真写下的码」能被「真读出来的步骤」判对 —— 两头各自单测都绿，接不上的话照样坏。
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

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	billingapp "github.com/li904338203/tabtv/backend/internal/billing/app"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

// batchItems 是 n 个变体的营销条目（v1..vn，各用 #1 一张商品图）。
func batchItems(n int) []domain.PlanItem {
	out := make([]domain.PlanItem, 0, n)
	for i := 1; i <= n; i++ {
		it := videoItem(fmt.Sprintf("v%d", i), []string{"101"}, []int{1})
		it.Variant, it.VariantLabel = i, fmt.Sprintf("Hook：反问开场 · 风格 %d", i)
		out = append(out, it)
	}
	return out
}

func batchService(e *paidEnv) *agentapp.Service {
	return agentapp.NewService(e.r, e.next, agentapp.ServicePorts{
		Accounts: wiring.AgentAccounts{Billing: billingapp.New(e.bill)}, MaxVariants: 4,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

var batchActor = agentapp.SessionActor{WorkspaceID: ws, PayerAccountID: payer, UserID: user, Role: collab.RoleEditor}

// planSteps 读本会话的 generate 步骤：键 → 按开始时间的步骤列表。
func planSteps(t *testing.T, e *paidEnv, sessionID int64) map[string][]repo.Step {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `select id from agent_step
		where session_id = $1 and tool = 'generate' order by started_at, id`, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	out := map[string][]repo.Step{}
	for _, id := range ids {
		st, err := e.r.GetStep(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		out[st.PlanItemKey] = append(out[st.PlanItemKey], st)
	}
	return out
}

// finishSteps 模拟扫描循环：按生成任务的终态结束等待中的步骤。
func finishSteps(t *testing.T, e *paidEnv, steps map[string][]repo.Step, outcome map[string]string) {
	t.Helper()
	for key, status := range outcome {
		ss := steps[key]
		st := ss[len(ss)-1]
		if ok, err := e.r.FinishStep(context.Background(), e.pool, st.ID, status, nil, ""); err != nil || !ok {
			t.Fatalf("结束步骤 %s：%v %v", key, ok, err)
		}
	}
}

func generateCalls(keys ...string) []agentapp.ChatOutput {
	out := make([]agentapp.ChatOutput, 0, len(keys))
	for i, k := range keys {
		out = append(out, call(fmt.Sprintf("c%d", i), "generate", `{"planItemKey":"`+k+`","wait":false}`))
	}
	return out
}

func TestBatch_中途余额不足停后续(t *testing.T) {
	e, _ := newMarketingEnv(t, 15) // 只够一个变体（paidPrice=10）
	ctx := context.Background()
	s, tr := seedMarketing(t, e)
	planCard(t, e, s, batchItems(3), "approve")
	if _, err := marketingRunner(e, &scriptedChat{outs: generateCalls("v1", "v2", "v3")}).RunTurn(ctx, s.ID, tr.ID); err != nil {
		t.Fatal(err)
	}
	steps := planSteps(t, e, s.ID)
	got := func(k string) string {
		st := steps[k][len(steps[k])-1]
		return st.Status + "|" + st.ErrorCode
	}
	if got("v1") != "waiting|" || got("v2") != "failed|INSUFFICIENT_CREDITS" ||
		got("v3") != "failed|"+domain.ToolErrPlanStopped {
		t.Fatalf("v1 受理、v2 余额不足、v3 停掉：%s / %s / %s", got("v1"), got("v2"), got("v3"))
	}
	// 第 3 个不受理、不扣费；已成功的不受影响。
	if n := e.cgen.accepts.Load(); n != 2 {
		t.Errorf("只有 v1 与 v2 走到受理（v3 被停）：%d", n)
	}
	if gens := e.generations(t); len(gens) != 1 {
		t.Errorf("只有 v1 一个任务：%d", len(gens))
	}
	if bal, _ := e.bill.Balance(ctx, payer); bal != 5 {
		t.Errorf("只扣了 v1 的 10：余额 %d", bal)
	}
	// 回给模型的是结构化结果，说清谁停的。
	msgs, _ := e.r.ListRecent(ctx, s.ID, 50)
	stopped := 0
	for _, m := range msgs {
		var c map[string]any
		if m.Role == repo.RoleTool && json.Unmarshal([]byte(m.Content), &c) == nil &&
			c["error"] == domain.ToolErrPlanStopped && c["stoppedBy"] == "v2" {
			stopped++
		}
	}
	if stopped != 1 {
		t.Errorf("v3 的工具结果应是 plan_stopped（stoppedBy=v2）：%d", stopped)
	}

	// 会话详情：v1 成功后为部分成功，v3 是 stopped。
	finishSteps(t, e, steps, map[string]string{"v1": repo.StepSuccess})
	d, err := batchService(e).Get(ctx, batchActor, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	p := d.PlanProgress
	if p == nil || p.Status != domain.PlanProgressPartialSuccess || p.Succeeded != 1 || p.Failed != 1 || p.Stopped != 1 {
		t.Fatalf("部分成功（1 成 1 败 1 停）：%+v", p)
	}
	if p.Items[1].ErrorCode != domain.StepErrInsufficientCredits || p.Items[2].State != domain.PlanItemStopped {
		t.Fatalf("逐条：%+v", p.Items)
	}
}

func TestBatch_单个失败退款恰好一次(t *testing.T) {
	e, _ := newMarketingEnv(t, 100)
	ctx := context.Background()
	s, tr := seedMarketing(t, e)
	card := planCard(t, e, s, batchItems(3), "approve")
	if _, err := marketingRunner(e, &scriptedChat{outs: generateCalls("v1", "v2", "v3")}).RunTurn(ctx, s.ID, tr.ID); err != nil {
		t.Fatal(err)
	}
	steps := planSteps(t, e, s.ID)
	gen2 := steps["v2"][0].GenerationID
	if gen2 == 0 || len(e.generations(t)) != 3 {
		t.Fatalf("三个变体各一个任务：%+v", steps)
	}
	// 上游失败（同一条失败被处理两次：重复消息 / 运维重复点）→ 退款恰好一次。
	for i := 0; i < 2; i++ {
		changed, err := e.gen.CompensateFail(ctx, gen2, "上游失败")
		if err != nil || changed != (i == 0) {
			t.Fatalf("第 %d 次判失败：changed=%v err=%v", i+1, changed, err)
		}
	}
	if n := e.count(t, `select count(*) from credit_transaction where type = 'refund' and ref_type = 'generation'
		and ref_id = $1`, strconv.FormatInt(gen2, 10)); n != 1 {
		t.Fatalf("退款恰好一次：%d", n)
	}
	if bal, _ := e.bill.Balance(ctx, payer); bal != 80 {
		t.Errorf("扣 30 退 10：余额 %d", bal)
	}
	finishSteps(t, e, steps, map[string]string{"v1": repo.StepSuccess, "v2": repo.StepFailed, "v3": repo.StepSuccess})

	svc := batchService(e)
	d, err := svc.Get(ctx, batchActor, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p := d.PlanProgress; p == nil || p.CardID != card.ID || p.Status != domain.PlanProgressPartialSuccess ||
		p.Succeeded != 2 || p.Failed != 1 || p.Total != 3 {
		t.Fatalf("部分成功（2/3）：%+v", d.PlanProgress)
	}
	// 对账：生成 / 退回两行与账本逐笔一致（退回单列，不从生成里减）。
	l, err := svc.TurnLedgerOf(ctx, batchActor, s.ID, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	var consume, refund int
	for _, en := range l.Entries {
		switch en.Type {
		case "consume":
			consume -= en.Change
		case "refund":
			refund += en.Change
		}
	}
	if l.Generation != 30 || l.Refunded != 10 || consume != 30 || refund != 10 || len(l.Entries) != 4 {
		t.Fatalf("对账：生成 %d 退回 %d，流水 consume %d refund %d 共 %d 条", l.Generation, l.Refunded, consume, refund,
			len(l.Entries))
	}

	// 画框：三个变体节点在同一个画框里、按变体序从左到右，标题 = 变体说明。
	assertBatchFrame(t, e, s.ID, card.ID, 3)

	// 失败的变体重试 = 新步骤、新幂等键（不命中上一次失败的记录）。
	if _, err := e.pool.Exec(ctx, `update agent_session set state = 'executing' where id = $1`, s.ID); err != nil {
		t.Fatal(err)
	}
	tr2, err := e.r.InsertTurn(ctx, e.pool, repo.Turn{ID: e.next(), SessionID: s.ID, Trigger: domain.TriggerUser})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := marketingRunner(e, &scriptedChat{outs: generateCalls("v2")}).RunTurn(ctx, s.ID, tr2.ID); err != nil {
		t.Fatal(err)
	}
	retry := planSteps(t, e, s.ID)["v2"]
	if len(retry) != 2 || retry[1].ID == retry[0].ID || retry[1].GenerationID == 0 || retry[1].GenerationID == gen2 {
		t.Fatalf("重试应是新步骤、新任务：%+v", retry)
	}
	var keys []string
	rows, _ := e.pool.Query(ctx, `select idempotency_key from generation where id = any($1)`,
		[]int64{gen2, retry[1].GenerationID})
	for rows.Next() {
		var k string
		_ = rows.Scan(&k)
		keys = append(keys, k)
	}
	rows.Close()
	if len(keys) != 2 || keys[0] == keys[1] {
		t.Fatalf("重试换幂等键：%v", keys)
	}
	if d, _ := svc.Get(ctx, batchActor, s.ID); d.PlanProgress.Items[1].State != domain.PlanItemSubmitted ||
		d.PlanProgress.Items[1].Attempts != 2 || d.PlanProgress.Status != domain.PlanProgressRunning {
		t.Fatalf("重试中：%+v", d.PlanProgress)
	}
}

// assertBatchFrame：计划画框恰好一个、n 个成员、成员按变体序从左到右、标题 = 变体说明。
func assertBatchFrame(t *testing.T, e *paidEnv, sessionID, cardID int64, n int) {
	t.Helper()
	var raw json.RawMessage
	if err := e.pool.QueryRow(context.Background(), `select canvas_data from project where id = $1`, project).
		Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Nodes []struct {
			ID       string             `json:"id"`
			Type     string             `json:"type"`
			Title    string             `json:"title"`
			Position map[string]float64 `json:"position"`
			Members  []string           `json:"members"`
			Agent    map[string]any     `json:"agent"`
		} `json:"nodes"`
	}
	_ = json.Unmarshal(raw, &doc)
	byID := map[string]int{}
	frames := 0
	var members []string
	for i, nd := range doc.Nodes {
		byID[nd.ID] = i
		if nd.Type == "frame" && nd.Agent["planCardId"] == strconv.FormatInt(cardID, 10) &&
			nd.Agent["sessionId"] == strconv.FormatInt(sessionID, 10) {
			frames++
			members = nd.Members
		}
	}
	if frames != 1 || len(members) != n {
		t.Fatalf("应恰好一个计划画框、%d 个成员：frames=%d members=%v", n, frames, members)
	}
	lastX := -1e18
	for i, m := range members {
		nd := doc.Nodes[byID[m]]
		if want := fmt.Sprintf("Hook：反问开场 · 风格 %d", i+1); nd.Title != want {
			t.Errorf("第 %d 个成员标题 %q，期望 %q", i+1, nd.Title, want)
		}
		if nd.Position["x"] <= lastX {
			t.Errorf("变体应从左到右：%v", nd.Position)
		}
		lastX = nd.Position["x"]
	}
}

func TestBatch_并发PaidCall不超额(t *testing.T) {
	e, _ := newMarketingEnv(t, 30) // 只够 3 次
	ctx := context.Background()
	s, _ := seedMarketing(t, e)
	planCard(t, e, s, batchItems(4), "approve")
	rcs := make([]agentapp.RunContext, 8)
	for i := range rcs {
		rcs[i] = e.step(t, s, "generate")
		rcs[i].Scene = domain.SceneMarketing
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var ok, insufficient, other int
	for i, rc := range rcs {
		wg.Add(1)
		go func(i int, rc agentapp.RunContext) {
			defer wg.Done()
			_, err := e.tool(domain.ToolGenerate).Run(ctx, rc, json.RawMessage(
				fmt.Sprintf(`{"planItemKey":"v%d","wait":false}`, i%4+1)))
			mu.Lock()
			defer mu.Unlock()
			var se *agentapp.StepError
			switch {
			case err == nil:
				ok++
			case errors.As(err, &se) && se.ErrorCode == domain.StepErrInsufficientCredits:
				insufficient++
			default:
				other++
				t.Errorf("意外错误：%v", err)
			}
		}(i, rc)
	}
	wg.Wait()
	if ok != 3 || insufficient != 5 || other != 0 {
		t.Fatalf("恰好 3 次成功、5 次余额不足：ok=%d insufficient=%d other=%d", ok, insufficient, other)
	}
	if bal, _ := e.bill.Balance(ctx, payer); bal != 0 {
		t.Fatalf("余额应恰好扣到 0、绝不为负：%d", bal)
	}
	if n := len(e.generations(t)); n != 3 {
		t.Fatalf("恰好 3 个任务：%d", n)
	}
}

func TestApprove_超上限兜底拒(t *testing.T) {
	e, _ := newMarketingEnv(t, 100)
	ctx := context.Background()
	s, _ := seedMarketing(t, e)
	if _, err := e.pool.Exec(ctx, `update agent_session set state = 'awaiting_approval' where id = $1`, s.ID); err != nil {
		t.Fatal(err)
	}
	svc := batchService(e)
	// 绕过 Agent 写进来的 5 个变体：批准被拒（40001），卡片仍待确认、额度不变。
	c5 := planCard(t, e, s, batchItems(5), "")
	_, err := svc.SubmitCard(ctx, batchActor, s.ID, c5.ID, json.RawMessage(`{"decision":"approve"}`))
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.InvalidArgument || be.Details["reason"] != domain.ToolErrTooManyVariants ||
		be.Details["max"] != 4 || be.Details["variants"] != 5 {
		t.Fatalf("应 400/40001 too_many_variants：%v", err)
	}
	if n := e.count(t, `select count(*) from agent_card where id = $1 and status = 'pending'`, c5.ID); n != 1 {
		t.Fatal("被拒的卡片仍待确认")
	}
	if n := e.count(t, `select approved_budget from agent_session where id = $1`, s.ID); n != 500 {
		t.Fatalf("额度不变：%d", n)
	}
	// 4 个变体过了上限这一关（之后的报价端口本用例没接，回的是另一类错误）。
	c4 := planCard(t, e, s, batchItems(4), "")
	_, err = svc.SubmitCard(ctx, batchActor, s.ID, c4.ID, json.RawMessage(`{"decision":"approve"}`))
	if errors.As(err, &be) && be.Details["reason"] == domain.ToolErrTooManyVariants {
		t.Fatalf("4 个变体不应被上限拦：%v", err)
	}
}
