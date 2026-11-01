//go:build integration

// 付费与画布写工具（AGT-24：canvas_apply / generate / cancel_task）的真库测试。
//
// 画布、生成受理、账本、额度条目全部用真库与真服务（canvas/app、generation/app、
// billing/repo、wiring 适配器）；只有推理模型、成员资格与报价是假实现 ——
// 要证明的是「先写画布后受理」「额度条目与账本」「退款只一次」，这些都由 SQL 决定。
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	agenttools "github.com/li904338203/tabtv/backend/internal/agent/tools"
	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	canvasapp "github.com/li904338203/tabtv/backend/internal/canvas/app"
	canvasrepo "github.com/li904338203/tabtv/backend/internal/canvas/repo"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	genrepo "github.com/li904338203/tabtv/backend/internal/generation/repo"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

const (
	paidPrice = 10
	teamPayer = int64(9302)
)

// paidGenQuoter 是受理侧的固定报价（目录域隔离在外，这里测的是编排与事务边界）。
type paidGenQuoter struct{ credits int }

func (q paidGenQuoter) QuoteForAccept(context.Context, genapp.QuoteAsk) (genapp.QuoteAnswer, error) {
	return genapp.QuoteAnswer{Credits: q.credits, EstimatedSeconds: 30, ResultType: "image", ModelID: 1,
		OperationID: 2, ProviderCode: "dev", APIProtocol: "dev_fake", PriceUnit: "per_image",
		PriceCredits: q.credits}, nil
}

// nodeProbe 是受理侧的输入解析：**在受理那一刻读库**，记下画布里有没有这个节点。
type nodeProbe struct {
	pool *pgxpool.Pool
	mu   sync.Mutex
	seen []map[string]any // 每次受理时读到的节点（nil = 没找到）
}

func (p *nodeProbe) ResolveForAccept(ctx context.Context, ws, projectID int64, nodeID, _ string,
	_ json.RawMessage) (genapp.ResolvedInputs, error) {
	var raw json.RawMessage
	if err := p.pool.QueryRow(ctx, `select canvas_data from project where id = $1 and workspace_id = $2`,
		projectID, ws).Scan(&raw); err != nil {
		return genapp.ResolvedInputs{}, err
	}
	var doc struct {
		Nodes []map[string]any `json:"nodes"`
	}
	_ = json.Unmarshal(raw, &doc)
	var found map[string]any
	for _, n := range doc.Nodes {
		if n["id"] == nodeID {
			found = n
		}
	}
	p.mu.Lock()
	p.seen = append(p.seen, found)
	p.mu.Unlock()
	return genapp.ResolvedInputs{Inputs: json.RawMessage(`[]`), RoleCounts: map[string]int{}}, nil
}

// countingGen 数受理次数（「余额不足不重试」）。
type countingGen struct {
	agentapp.Generator
	accepts atomic.Int32
}

func (c *countingGen) Accept(ctx context.Context, in agentapp.GenerateInput) (agentapp.GenerateResult, error) {
	c.accepts.Add(1)
	return c.Generator.Accept(ctx, in)
}

// paidQuoter：对话轮 0 积分（让账本里只有生成费），生成报价固定。
type paidQuoter struct{ gen int }

func (q paidQuoter) QuoteGeneration(context.Context, agentapp.QuoteInput) (agentapp.Quote, error) {
	return agentapp.Quote{Credits: q.gen}, nil
}
func (q paidQuoter) QuoteTurn(context.Context, string, int64) (agentapp.Quote, error) {
	return agentapp.Quote{}, nil
}

type paidMembers struct {
	role   collab.Role
	member bool
}

func (m paidMembers) Role(context.Context, int64, int64) (collab.Role, bool, error) {
	return m.role, m.member, nil
}

// scriptedChat 按顺序回放预设的输出；之后一律回一句话结束本轮。记下每次下发的工具名。
type scriptedChat struct {
	mu    sync.Mutex
	outs  []agentapp.ChatOutput
	tools [][]string
}

func (c *scriptedChat) Chat(_ context.Context, in agentapp.ChatInput, _ func(string)) (agentapp.ChatOutput, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var names []string
	for _, t := range in.Tools {
		names = append(names, t.Name)
	}
	c.tools = append(c.tools, names)
	if len(c.outs) == 0 {
		return agentapp.ChatOutput{Text: "好了"}, nil
	}
	o := c.outs[0]
	c.outs = c.outs[1:]
	return o, nil
}

func call(id, name, args string) agentapp.ChatOutput {
	return agentapp.ChatOutput{ToolCalls: []agentapp.ToolCall{{ID: id, Name: name, Arguments: json.RawMessage(args)}}}
}

type paidEnv struct {
	r      *repo.Repo
	pool   *pgxpool.Pool
	next   func() int64
	bill   *billingrepo.Repo
	canvas *canvasapp.Service
	gen    *genapp.Service
	probe  *nodeProbe
	cgen   *countingGen
	budget *agentapp.Budget
	tools  []agentapp.Tool
	reg    *agenttools.Registry
}

func newPaidEnv(t *testing.T, payerBalances map[int64]int) *paidEnv {
	t.Helper()
	ctx := context.Background()
	r, pool, next := setup(t)
	bill := billingrepo.New(pool, next)
	for p, amount := range payerBalances {
		if _, err := bill.Grant(ctx, billingrepo.GrantInput{PayerID: p, Source: "purchase", Amount: amount,
			SourceRefID: "paid-" + strconv.FormatInt(next(), 10)}); err != nil {
			t.Fatal(err)
		}
	}
	crepo := canvasrepo.New(pool)
	if err := crepo.Insert(ctx, canvasrepo.Project{ID: project, WorkspaceID: ws, CreatedBy: user, Name: "Agent 项目",
		CanvasData: json.RawMessage(`{"schemaVersion":1,"viewport":{"x":0,"y":0,"zoom":1},"nodes":[` +
			`{"id":"n_user1","type":"image","title":"用户的图","position":{"x":0,"y":0},"size":{"w":330,"h":180},"params":{}}` +
			`],"edges":[]}`)}); err != nil {
		t.Fatal(err)
	}
	canvas := canvasapp.New(crepo, next)
	probe := &nodeProbe{pool: pool}
	gen := genapp.New(genrepo.New(pool), next)
	gen.SetCredits(bill)
	gen.SetQuoter(paidGenQuoter{paidPrice})
	gen.SetInputResolver(probe)
	ledger := wiring.AgentLedger{Repo: bill, Compensate: 30 * 24 * time.Hour}
	gen.SetRefunder(ledger)

	cgen := &countingGen{Generator: wiring.AgentGenerator{Gen: gen, Ledger: bill}}
	budget := agentapp.NewBudget(r, next, nil)
	ac := wiring.AgentCanvas{Svc: canvas}
	tools := []agentapp.Tool{
		agenttools.CanvasApply{Canvas: ac},
		agenttools.Generate{Canvas: ac, Quoter: paidQuoter{paidPrice}, Generator: cgen, Budget: budget},
		agenttools.CancelTask{Generator: cgen, Steps: r},
	}
	reg, err := agenttools.NewRegistry(tools...)
	if err != nil {
		t.Fatal(err)
	}
	return &paidEnv{r: r, pool: pool, next: next, bill: bill, canvas: canvas, gen: gen, probe: probe, cgen: cgen,
		budget: budget, tools: tools, reg: reg}
}

func (e *paidEnv) runner(chat agentapp.ChatModel, m paidMembers) *agentapp.Runner {
	return agentapp.NewRunner(agentapp.RunnerDeps{
		Store: agentapp.NewRepoStore(e.r), Chat: chat, Quoter: paidQuoter{paidPrice},
		Ledger:  wiring.AgentLedger{Repo: e.bill, Compensate: 30 * 24 * time.Hour},
		Members: m, Canvas: wiring.AgentCanvas{Svc: e.canvas}, Tools: e.reg, NextID: e.next,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, agentapp.RunnerConfig{MaxCalls: 5, ContextChars: 60000})
}

// seed 建会话（状态、额度、付费主体）+ 一条用户消息 + 一个用户轮次。
func (e *paidEnv) seed(t *testing.T, state domain.State, approved int, payerID int64) (repo.Session, repo.Turn) {
	t.Helper()
	ctx := context.Background()
	s, err := e.r.Create(ctx, e.pool, repo.Session{ID: e.next(), WorkspaceID: ws, ProjectID: project,
		PayerAccountID: payerID, CreatedByUserID: user, State: state, ApprovedBudget: approved,
		// 画幅 / 清晰度 / 时长已在偏好里选定（design/01 修订块 R-1：「智能」时带视频的 submit_plan 要求先确认）。
		Prefs: json.RawMessage(`{"reasoning":{"model":"m-agent"},"video":{"ratio":"9:16","resolution":"720p","duration":10}}`)})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := e.r.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.Append(ctx, tx, repo.Message{ID: e.next(), SessionID: s.ID, Role: repo.RoleUser,
		Content: "生成一张猫"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tr, err := e.r.InsertTurn(ctx, e.pool, repo.Turn{ID: e.next(), SessionID: s.ID, Trigger: domain.TriggerUser})
	if err != nil {
		t.Fatal(err)
	}
	return s, tr
}

// step 插一条步骤（直接调工具时用；Runner 路径由 Runner 自己插）。
func (e *paidEnv) step(t *testing.T, s repo.Session, tool string) agentapp.RunContext {
	t.Helper()
	tr, err := e.r.InsertTurn(context.Background(), e.pool, repo.Turn{ID: e.next(), SessionID: s.ID,
		Trigger: domain.TriggerSystem})
	if err != nil {
		t.Fatal(err)
	}
	st, err := e.r.InsertStep(context.Background(), e.pool, repo.Step{ID: e.next(), SessionID: s.ID, TurnID: tr.ID,
		Tool: tool, Attempt: 1})
	if err != nil {
		t.Fatal(err)
	}
	return agentapp.RunContext{WorkspaceID: ws, ProjectID: project, SessionID: s.ID, TurnID: tr.ID, StepID: st.ID,
		Attempt: 1, UserID: user, Role: collab.RoleEditor, PayerAccountID: s.PayerAccountID, State: s.State}
}

func (e *paidEnv) tool(name domain.ToolName) agentapp.Tool {
	t, _ := e.reg.Lookup(name)
	return t
}

func (e *paidEnv) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

type genRow struct {
	ID      int64
	NodeID  string
	Payer   int64
	IdemKey string
	Status  string
	Credits int
	Project int64
	Creator int64
}

func (e *paidEnv) generations(t *testing.T) []genRow {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `select id, coalesce(node_id,''), payer_account_id,
		idempotency_key, status, credits_cost, project_id, created_by_user_id from generation order by created_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []genRow
	for rows.Next() {
		var g genRow
		if err := rows.Scan(&g.ID, &g.NodeID, &g.Payer, &g.IdemKey, &g.Status, &g.Credits, &g.Project,
			&g.Creator); err != nil {
			t.Fatal(err)
		}
		out = append(out, g)
	}
	return out
}

func content(t *testing.T, res agentapp.ToolResult) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(res.Content, &m); err != nil {
		t.Fatalf("工具结果不是对象：%s", res.Content)
	}
	return m
}

// ---------------------------------------------------------------- generate

func TestGenerate_先写画布后受理节点已落盘(t *testing.T) {
	e := newPaidEnv(t, map[int64]int{payer: 100})
	ctx := context.Background()
	s, tr := e.seed(t, domain.StateExecuting, 100, payer)
	chat := &scriptedChat{outs: []agentapp.ChatOutput{call("c1", "generate",
		`{"nodeType":"image","title":"橘猫","modelCode":"dev_image","operationCode":"text2img","prompt":"一只橘猫"}`)}}
	if _, err := e.runner(chat, paidMembers{collab.RoleEditor, true}).RunTurn(ctx, s.ID, tr.ID); err != nil {
		t.Fatal(err)
	}

	gens := e.generations(t)
	if len(gens) != 1 {
		t.Fatalf("应恰好受理 1 个任务：%+v", gens)
	}
	g := gens[0]
	if len(e.probe.seen) != 1 || e.probe.seen[0] == nil {
		t.Fatalf("受理时画布里必须已有该节点（先写画布后受理）：%v", e.probe.seen)
	}
	node := e.probe.seen[0]
	if node["id"] != g.NodeID || !strings.HasPrefix(g.NodeID, "a_") || node["type"] != "image" {
		t.Errorf("受理挂在 Agent 建的节点上：node=%v gen.node=%s", node, g.NodeID)
	}
	meta, _ := node["agent"].(map[string]any)
	steps, _ := e.r.StepsByGeneration(ctx, g.ID)
	if len(steps) != 1 {
		t.Fatalf("应有 1 个步骤挂着这个任务：%+v", steps)
	}
	if meta["sessionId"] != strconv.FormatInt(s.ID, 10) || meta["stepId"] != strconv.FormatInt(steps[0].ID, 10) {
		t.Errorf("节点应带 agent.sessionId / stepId：%v（step %d）", meta, steps[0].ID)
	}
	params, _ := node["params"].(map[string]any)
	if params["model"] != "dev_image" || params["prompt"] != "一只橘猫" {
		t.Errorf("节点参数应与前端同形：%v", params)
	}
	if steps[0].Status != repo.StepWaiting {
		t.Errorf("步骤应为 waiting：%s", steps[0].Status)
	}
	if g.IdemKey != domain.PaidIdempotencyKey(s.ID, steps[0].ID, 1) || g.Project != project || g.Creator != user {
		t.Errorf("任务归属 / 幂等键不对：%+v", g)
	}
	entry, err := e.r.BudgetByStep(ctx, steps[0].ID)
	if err != nil || entry.Status != repo.BudgetCharged || entry.GenerationID != g.ID || entry.Quoted != paidPrice {
		t.Errorf("额度条目应 charged 并挂上任务：%+v %v", entry, err)
	}
	if bal, _ := e.bill.Balance(ctx, payer); bal != 100-paidPrice {
		t.Errorf("生成费只由受理扣一次：余额 %d", bal)
	}
	s2, _ := e.r.GetByID(ctx, s.ID)
	if s2.State != domain.StateWaitingTask {
		t.Errorf("wait 默认 true，会话应进入 waiting_task：%s", s2.State)
	}

	// 受理之后任务挂到节点 result 上（link_generation，独立批次、写操作日志）——
	// 不挂的话画布无从知道节点上有任务，节点一直显示成空的。
	p, err := e.canvas.Get(ctx, ws, project, collab.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Nodes []map[string]any `json:"nodes"`
	}
	if err := json.Unmarshal(p.CanvasData, &doc); err != nil {
		t.Fatal(err)
	}
	var linked map[string]any
	for _, n := range doc.Nodes {
		if n["id"] == g.NodeID {
			linked, _ = n["result"].(map[string]any)
		}
	}
	if linked["id"] != strconv.FormatInt(g.ID, 10) || linked["status"] != "queued" {
		t.Errorf("节点 result 应挂上本次任务：%v", linked)
	}
	if n := e.count(t, `select count(*) from canvas_op_log where project_id = $1 and batch_id = $2
		and ops::text like '%link_generation%'`, project,
		fmt.Sprintf("agent:%d:%d:1:link", s.ID, steps[0].ID)); n != 1 {
		t.Errorf("link_generation 应写一条操作日志：%d", n)
	}
}

func TestGenerate_幂等键随attempt变(t *testing.T) {
	e := newPaidEnv(t, map[int64]int{payer: 100})
	ctx := context.Background()
	s, _ := e.seed(t, domain.StateExecuting, 100, payer)
	rc := e.step(t, s, "generate")
	gen := e.tool(domain.ToolGenerate)

	res, err := gen.Run(ctx, rc, json.RawMessage(
		`{"nodeType":"image","modelCode":"dev_image","operationCode":"text2img","prompt":"猫","wait":false}`))
	if err != nil {
		t.Fatal(err)
	}
	first := res.GenerationID
	nodeID, _ := content(t, res)["nodeId"].(string)
	// 任务失败、生成域退款，扫描循环（AGT-25）把条目标 refunded。这里直接模拟这两步。
	if _, err := e.pool.Exec(ctx, `update generation set status = 'failed' where id = $1`, first); err != nil {
		t.Fatal(err)
	}
	entry, _ := e.r.BudgetByStep(ctx, rc.StepID)
	if ok, err := e.r.MarkRefunded(ctx, entry.ID); err != nil || !ok {
		t.Fatal(ok, err)
	}

	// 同一步骤 attempt 2：新幂等键 → 新任务，不命中上一次 failed 的那条。
	rc.Attempt = 2
	res2, err := gen.Run(ctx, rc, json.RawMessage(
		`{"nodeId":"`+nodeID+`","modelCode":"dev_image","operationCode":"text2img","prompt":"猫","wait":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if res2.GenerationID == first || res2.GenerationID == 0 {
		t.Fatalf("attempt 2 应是新任务：%d vs %d", res2.GenerationID, first)
	}
	gens := e.generations(t)
	if len(gens) != 2 || gens[0].IdemKey == gens[1].IdemKey ||
		gens[1].IdemKey != domain.PaidIdempotencyKey(s.ID, rc.StepID, 2) || gens[1].Status != "queued" {
		t.Fatalf("两次受理的幂等键应不同：%+v", gens)
	}
	if gens[1].NodeID != nodeID {
		t.Errorf("重试挂在同一个节点上：%s vs %s", gens[1].NodeID, nodeID)
	}
	entry, _ = e.r.BudgetByStep(ctx, rc.StepID)
	if entry.Status != repo.BudgetCharged || entry.GenerationID != res2.GenerationID {
		t.Errorf("条目应重新 charged 到新任务：%+v", entry)
	}
	if res.Suspend != nil || res2.Suspend != nil {
		t.Error("wait=false 不挂起")
	}
}

func TestGenerate_余额不足释放额度不重试(t *testing.T) {
	e := newPaidEnv(t, map[int64]int{payer: 4})
	ctx := context.Background()
	s, _ := e.seed(t, domain.StateExecuting, 100, payer)
	rc := e.step(t, s, "generate")
	_, err := e.tool(domain.ToolGenerate).Run(ctx, rc, json.RawMessage(
		`{"nodeType":"image","modelCode":"dev_image","operationCode":"text2img","prompt":"猫"}`))
	if err == nil || !strings.Contains(err.Error(), "还差 6") || !strings.Contains(err.Error(), "不要重试") {
		t.Fatalf("应回给模型「余额不足，还差 6」：%v", err)
	}
	if n := e.cgen.accepts.Load(); n != 1 {
		t.Errorf("余额不足不重试：受理调用 %d 次", n)
	}
	entry, _ := e.r.BudgetByStep(ctx, rc.StepID)
	if entry.Status != repo.BudgetReleased {
		t.Errorf("条目应 released：%+v", entry)
	}
	if len(e.generations(t)) != 0 {
		t.Error("不应留下任务")
	}
	if bal, _ := e.bill.Balance(ctx, payer); bal != 4 {
		t.Errorf("余额不应变：%d", bal)
	}
}

func TestGenerate_团队消费记在团队账户(t *testing.T) {
	e := newPaidEnv(t, map[int64]int{payer: 100, teamPayer: 100})
	ctx := context.Background()
	s, _ := e.seed(t, domain.StateExecuting, 100, teamPayer)
	rc := e.step(t, s, "generate")
	res, err := e.tool(domain.ToolGenerate).Run(ctx, rc, json.RawMessage(
		`{"nodeType":"image","modelCode":"dev_image","operationCode":"text2img","prompt":"团队的猫"}`))
	if err != nil {
		t.Fatal(err)
	}
	g := e.generations(t)[0]
	if g.Payer != teamPayer || g.ID != res.GenerationID {
		t.Fatalf("任务的付费主体应为会话的 payer：%+v", g)
	}
	if n := e.count(t, `select count(*) from credit_transaction where ref_type='generation' and payer_account_id=$1`,
		payer); n != 0 {
		t.Errorf("个人账户一条流水都不该有：%d", n)
	}
	if n := e.count(t, `select count(*) from credit_transaction where ref_type='generation' and type='consume'
		and payer_account_id=$1 and ref_id=$2`, teamPayer, strconv.FormatInt(g.ID, 10)); n != 1 {
		t.Errorf("团队账户应恰好 1 笔消费：%d", n)
	}
	if bal, _ := e.bill.Balance(ctx, payer); bal != 100 {
		t.Errorf("个人余额不应变：%d", bal)
	}
}

func TestGenerate_超额度挂起且节点保留(t *testing.T) {
	e := newPaidEnv(t, map[int64]int{payer: 100})
	ctx := context.Background()
	s, _ := e.seed(t, domain.StateExecuting, 5, payer)
	rc := e.step(t, s, "generate")
	res, err := e.tool(domain.ToolGenerate).Run(ctx, rc, json.RawMessage(
		`{"nodeType":"image","modelCode":"dev_image","operationCode":"text2img","prompt":"猫"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Suspend == nil || res.Suspend.Event != domain.EvOverBudget || res.GenerationID != 0 {
		t.Fatalf("应挂起等追加额度：%+v", res)
	}
	c := content(t, res)
	if c["status"] != "over_budget" || c["nodeId"] == "" {
		t.Errorf("要告诉模型节点 ID，批准后用它重试：%v", c)
	}
	if e.cgen.accepts.Load() != 0 || len(e.generations(t)) != 0 {
		t.Error("超额度不应受理")
	}
}

// ---------------------------------------------------------------- canvas_apply

func TestCanvasApply_规划态二次校验拒绝(t *testing.T) {
	e := newPaidEnv(t, map[int64]int{payer: 100})
	ctx := context.Background()
	s, tr := e.seed(t, domain.StatePlanning, 100, payer)
	// 模型凭历史幻觉调用本次没下发的工具。
	chat := &scriptedChat{outs: []agentapp.ChatOutput{{ToolCalls: []agentapp.ToolCall{
		{ID: "c1", Name: "canvas_apply", Arguments: json.RawMessage(`{"ops":[{"kind":"add_node","node":{"type":"text"}}]}`)},
		{ID: "c2", Name: "generate", Arguments: json.RawMessage(
			`{"nodeType":"image","modelCode":"dev_image","operationCode":"text2img","prompt":"猫"}`)},
	}}}}
	if _, err := e.runner(chat, paidMembers{collab.RoleEditor, true}).RunTurn(ctx, s.ID, tr.ID); err != nil {
		t.Fatal(err)
	}
	for _, names := range chat.tools {
		for _, n := range names {
			if n == "canvas_apply" || n == "generate" || n == "cancel_task" {
				t.Fatalf("规划态不应下发 %s", n)
			}
		}
	}
	if n := e.count(t, `select count(*) from canvas_op_log`); n != 0 {
		t.Errorf("不应写画布：%d", n)
	}
	if n := e.count(t, `select count(*) from agent_budget_entry`); n != 0 {
		t.Errorf("额度表不应有新行：%d", n)
	}
	if n := e.count(t, `select count(*) from credit_transaction where type <> 'grant'`); n != 0 {
		t.Errorf("账本不应有新行：%d", n)
	}
	if n := e.count(t, `select count(*) from agent_step where session_id=$1 and status='failed'`, s.ID); n != 2 {
		t.Errorf("两次幻觉调用都应留下失败步骤：%d", n)
	}
}

func TestCanvasApply_不改用户节点改为提示新建(t *testing.T) {
	e := newPaidEnv(t, nil)
	ctx := context.Background()
	s, _ := e.seed(t, domain.StateExecuting, 0, payer)
	rc := e.step(t, s, "canvas_apply")
	ca := e.tool(domain.ToolCanvasApply)

	_, err := ca.Run(ctx, rc, json.RawMessage(`{"ops":[{"kind":"update_node","nodeId":"n_user1","patch":{"title":"改掉"}}]}`))
	if err == nil || err.Error() != agenttools.NotOwnedHint {
		t.Fatalf("改用户节点应回「请在旁边新建」：%v", err)
	}
	rc2 := e.step(t, s, "canvas_apply")
	res, err := ca.Run(ctx, rc2, json.RawMessage(`{"ops":[{"kind":"add_node","ref":"ref:a","node":{"type":"text","title":"旁边的",`+
		`"params":{"prompt":"x"}},"position":{"x":400,"y":0}},{"kind":"add_edge","edge":{"source":"n_user1","target":"ref:a","order":1}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	ids, _ := content(t, res)["nodeIds"].([]any)
	if len(ids) != 1 || res.Outputs != 1 {
		t.Fatalf("应返回新节点 ID：%s", res.Content)
	}
	p, err := e.canvas.Get(ctx, ws, project, collab.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Nodes []map[string]any `json:"nodes"`
	}
	_ = json.Unmarshal(p.CanvasData, &doc)
	if len(doc.Nodes) != 2 || doc.Nodes[0]["title"] != "用户的图" {
		t.Fatalf("用户节点不应被改：%+v", doc.Nodes)
	}
	meta, _ := doc.Nodes[1]["agent"].(map[string]any)
	if meta["sessionId"] != strconv.FormatInt(s.ID, 10) || meta["stepId"] != strconv.FormatInt(rc2.StepID, 10) {
		t.Errorf("新节点应带 agent 元数据：%v", meta)
	}
	// 同一批重放（同一步骤同一 attempt）只写一次。
	res2, err := ca.Run(ctx, rc2, json.RawMessage(`{"ops":[{"kind":"add_node","node":{"type":"text"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(res2.Content) != string(res.Content) ||
		e.count(t, `select count(*) from canvas_op_log where project_id=$1`, project) != 1 {
		t.Errorf("同一批次应幂等：%s vs %s", res2.Content, res.Content)
	}
}

// ---------------------------------------------------------------- cancel_task

// acceptViaTool 用 generate 受理一个任务并像 Runner 那样把步骤标 waiting。
func (e *paidEnv) acceptViaTool(t *testing.T, s repo.Session) (agentapp.RunContext, int64) {
	t.Helper()
	ctx := context.Background()
	rc := e.step(t, s, "generate")
	res, err := e.tool(domain.ToolGenerate).Run(ctx, rc, json.RawMessage(
		`{"nodeType":"image","modelCode":"dev_image","operationCode":"text2img","prompt":"猫","wait":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := e.r.SetStepWaiting(ctx, e.pool, rc.StepID, res.GenerationID); err != nil || !ok {
		t.Fatal(ok, err)
	}
	return rc, res.GenerationID
}

func TestCancelTask_只退一次(t *testing.T) {
	e := newPaidEnv(t, map[int64]int{payer: 100})
	ctx := context.Background()
	s, _ := e.seed(t, domain.StateExecuting, 100, payer)
	rc, genID := e.acceptViaTool(t, s)
	ct := e.tool(domain.ToolCancelTask)
	args := json.RawMessage(`{"generationId":"` + strconv.FormatInt(genID, 10) + `"}`)

	res, err := ct.Run(ctx, rc, args)
	if err != nil || content(t, res)["cancelled"] != true {
		t.Fatalf("第一次应取消成功：%s %v", res.Content, err)
	}
	res, err = ct.Run(ctx, rc, args)
	if err != nil || content(t, res)["cancelled"] != false {
		t.Fatalf("第二次应说「已经结束」而不是报错：%s %v", res.Content, err)
	}
	ref := strconv.FormatInt(genID, 10)
	if n := e.count(t, `select count(*) from credit_transaction where ref_type='generation' and ref_id=$1
		and type='refund'`, ref); n != 1 {
		t.Fatalf("生成域退款应恰好 1 笔：%d", n)
	}
	if bal, _ := e.bill.Balance(ctx, payer); bal != 100 {
		t.Errorf("退款后余额应回到 100：%d", bal)
	}
	if g := e.generations(t)[0]; g.Status != "cancelled" {
		t.Errorf("任务应 cancelled：%s", g.Status)
	}
	// 额度条目不由本工具改：charged → refunded 是扫描循环（AGT-25）的事。
	entry, _ := e.r.BudgetByStep(ctx, rc.StepID)
	if entry.Status != repo.BudgetCharged || entry.GenerationID != genID {
		t.Errorf("cancel_task 不碰额度条目：%+v", entry)
	}
}

func TestCancelTask_非本会话任务拒绝(t *testing.T) {
	e := newPaidEnv(t, map[int64]int{payer: 100})
	ctx := context.Background()
	other, _ := e.seed(t, domain.StateExecuting, 100, payer)
	_, genID := e.acceptViaTool(t, other)
	s, _ := e.seed(t, domain.StateExecuting, 100, payer)
	rc := e.step(t, s, "cancel_task")

	_, err := e.tool(domain.ToolCancelTask).Run(ctx, rc,
		json.RawMessage(`{"generationId":`+strconv.FormatInt(genID, 10)+`}`))
	if err == nil || !strings.Contains(err.Error(), "不是本会话") {
		t.Fatalf("应拒绝取消别的会话的任务：%v", err)
	}
	// 用户自己在画布上点的生成（没有任何步骤）同样拒绝。
	userGen, err := e.gen.Accept(ctx, genapp.AcceptInput{WorkspaceID: ws, PayerAccountID: payer, UserID: user,
		ProjectID: project, NodeID: "n_user1", OperationCode: "text2img", ModelCode: "dev_image",
		IdempotencyKey: "user-click-1", Count: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.tool(domain.ToolCancelTask).Run(ctx, rc, json.RawMessage(
		`{"generationId":"`+strconv.FormatInt(userGen.Generation.ID, 10)+`"}`)); err == nil {
		t.Fatal("应拒绝取消用户自己的任务")
	}
	for _, g := range e.generations(t) {
		if g.Status != "queued" {
			t.Errorf("任务不应被取消：%+v", g)
		}
	}
}

// ---------------------------------------------------------------- 权限

func TestPaidTool_只读成员被拒且不扣费(t *testing.T) {
	e := newPaidEnv(t, map[int64]int{payer: 100})
	ctx := context.Background()
	s, tr := e.seed(t, domain.StateExecuting, 100, payer)
	chat := &scriptedChat{outs: []agentapp.ChatOutput{call("c1", "generate",
		`{"nodeType":"image","modelCode":"dev_image","operationCode":"text2img","prompt":"猫"}`)}}
	if _, err := e.runner(chat, paidMembers{collab.RoleViewer, true}).RunTurn(ctx, s.ID, tr.ID); err != nil {
		t.Fatal(err)
	}
	if e.cgen.accepts.Load() != 0 || len(e.generations(t)) != 0 {
		t.Error("只读成员不应受理")
	}
	if n := e.count(t, `select count(*) from agent_budget_entry`) + e.count(t, `select count(*) from canvas_op_log`); n != 0 {
		t.Errorf("不应有额度条目与画布写入：%d", n)
	}
	if bal, _ := e.bill.Balance(ctx, payer); bal != 100 {
		t.Errorf("不应扣费：%d", bal)
	}
	var msg string
	_ = e.pool.QueryRow(ctx, `select coalesce(error_message,'') from agent_step where session_id=$1`, s.ID).Scan(&msg)
	if !strings.Contains(msg, "generate") {
		t.Errorf("步骤应说清缺哪项权限：%q", msg)
	}
}

func TestPaidTool_发起人移出会话取消(t *testing.T) {
	e := newPaidEnv(t, map[int64]int{payer: 100})
	ctx := context.Background()
	s, tr := e.seed(t, domain.StateExecuting, 100, payer)
	chat := &scriptedChat{outs: []agentapp.ChatOutput{call("c1", "generate",
		`{"nodeType":"image","modelCode":"dev_image","operationCode":"text2img","prompt":"猫"}`)}}
	if _, err := e.runner(chat, paidMembers{"", false}).RunTurn(ctx, s.ID, tr.ID); err != nil {
		t.Fatal(err)
	}
	s2, _ := e.r.GetByID(ctx, s.ID)
	if s2.State != domain.StateCancelled {
		t.Errorf("发起人被移出，会话应 cancelled：%s", s2.State)
	}
	if e.cgen.accepts.Load() != 0 || e.count(t, `select count(*) from agent_budget_entry`) != 0 {
		t.Error("不应受理也不应占额度")
	}
	if bal, _ := e.bill.Balance(ctx, payer); bal != 100 {
		t.Errorf("不应扣费：%d", bal)
	}
}
