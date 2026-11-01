//go:build integration

// 规划阶段免费工具（AGT-23）的真库测试：一整轮 read_canvas → ask_user，用户回答后
// load_skill → write_doc(/plan.md) → submit_plan。
//
// 会话 / 卡片 / 技能 / 画布 / 文档 / 账本余额 / 节点生成查询都用真库与真服务；
// 推理模型、成员资格与逐项报价是假实现（报价映射与 QuoteWithFacts 的一致性由单测与
// wiring.PlanQuoteRequest 钉住）。要证明的是：规划态每一次请求都没有付费与画布写工具、
// 卡片与会话状态真的落库、文档节点真的建在画布上、技能白名单真的收窄了下一次请求。
package integration

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	agenttools "github.com/li904338203/tabtv/backend/internal/agent/tools"
	billingapp "github.com/li904338203/tabtv/backend/internal/billing/app"
	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	canvasapp "github.com/li904338203/tabtv/backend/internal/canvas/app"
	canvasrepo "github.com/li904338203/tabtv/backend/internal/canvas/repo"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	genrepo "github.com/li904338203/tabtv/backend/internal/generation/repo"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

// planChat 按顺序回放预设输出，记下每次请求下发的工具名。
type planChat struct {
	mu      sync.Mutex
	outs    []agentapp.ChatOutput
	tools   [][]string
	systems []string
}

func (c *planChat) Chat(_ context.Context, in agentapp.ChatInput, _ func(string)) (agentapp.ChatOutput, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var names []string
	for _, t := range in.Tools {
		names = append(names, t.Name)
	}
	c.tools = append(c.tools, names)
	c.systems = append(c.systems, in.System)
	if len(c.outs) == 0 {
		return agentapp.ChatOutput{Text: "好了"}, nil
	}
	o := c.outs[0]
	c.outs = c.outs[1:]
	return o, nil
}

func planCall(id, name, args string) agentapp.ChatOutput {
	return agentapp.ChatOutput{ToolCalls: []agentapp.ToolCall{{ID: id, Name: name, Arguments: json.RawMessage(args)}}}
}

// planPricer：每秒 / 每张 4 积分。
type planPricer struct{}

func (planPricer) PriceItem(_ context.Context, it domain.PlanItem, _ int64) (agentapp.PricedItem, error) {
	it.Credits = 4 * it.Quantity
	return agentapp.PricedItem{Item: it, Unit: "per_second", Available: true}, nil
}

type planTurnQuoter struct{}

func (planTurnQuoter) QuoteGeneration(context.Context, agentapp.QuoteInput) (agentapp.Quote, error) {
	return agentapp.Quote{}, nil
}
func (planTurnQuoter) QuoteTurn(context.Context, string, int64) (agentapp.Quote, error) {
	return agentapp.Quote{}, nil
}

type planMembers struct{}

func (planMembers) Role(context.Context, int64, int64) (collab.Role, bool, error) {
	return collab.RoleEditor, true, nil
}

type planAssets struct{}

func (planAssets) ListAssets(context.Context, int64, agentapp.AssetQuery) (agentapp.AssetPage, error) {
	return agentapp.AssetPage{}, nil
}

type planEnv struct {
	r      *repo.Repo
	pool   *pgxpool.Pool
	next   func() int64
	canvas *canvasapp.Service
	reg    *agenttools.Registry
}

func newPlanEnv(t *testing.T, balance int) *planEnv {
	t.Helper()
	ctx := context.Background()
	r, pool, next := setup(t)
	bill := billingrepo.New(pool, next)
	if _, err := bill.Grant(ctx, billingrepo.GrantInput{PayerID: payer, Source: "purchase", Amount: balance,
		SourceRefID: "plan-" + strconv.FormatInt(next(), 10)}); err != nil {
		t.Fatal(err)
	}
	crepo := canvasrepo.New(pool)
	if err := crepo.Insert(ctx, canvasrepo.Project{ID: project, WorkspaceID: ws, CreatedBy: user, Name: "规划项目",
		CanvasData: json.RawMessage(`{"schemaVersion":1,"viewport":{"x":0,"y":0,"zoom":1},"nodes":[` +
			`{"id":"n_user1","type":"image","title":"用户的图","position":{"x":0,"y":0},"size":{"w":330,"h":180},` +
			`"params":{"prompt":"不该出现在摘要里的提示词全文"}}],"edges":[]}`)}); err != nil {
		t.Fatal(err)
	}
	canvas := canvasapp.New(crepo, next)
	tools := agenttools.PlanningTools(agenttools.PlanningDeps{
		Store: r, Canvas: wiring.AgentCanvas{Svc: canvas},
		Generations: wiring.AgentNodeGenerations{Repo: genrepo.New(pool)},
		Assets:      planAssets{}, Pricer: planPricer{},
		Balances:  wiring.AgentAccounts{Billing: billingapp.New(bill)},
		Documents: wiring.AgentDocuments{Svc: canvas}, Skills: wiring.AgentSkillRenderer{},
		NextID: next, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	reg, err := agenttools.NewRegistry(tools...)
	if err != nil {
		t.Fatal(err)
	}
	return &planEnv{r: r, pool: pool, next: next, canvas: canvas, reg: reg}
}

func (e *planEnv) runner(chat agentapp.ChatModel) *agentapp.Runner {
	return agentapp.NewRunner(agentapp.RunnerDeps{
		Store: agentapp.NewRepoStore(e.r), Chat: chat, Quoter: planTurnQuoter{},
		Ledger: wiring.AgentLedger{Repo: billingrepo.New(e.pool, e.next)}, Members: planMembers{},
		Canvas: wiring.AgentCanvas{Svc: e.canvas}, Documents: wiring.AgentDocuments{Svc: e.canvas},
		Tools: e.reg, SkillRender: wiring.AgentSkillRenderer{}, NextID: e.next,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, agentapp.RunnerConfig{MaxCalls: 8, ContextChars: 60000})
}

func (e *planEnv) userTurn(t *testing.T, sessionID int64, text string) repo.Turn {
	t.Helper()
	ctx := context.Background()
	tx, err := e.r.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.Append(ctx, tx, repo.Message{ID: e.next(), SessionID: sessionID, Role: repo.RoleUser,
		Content: text}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tr, err := e.r.InsertTurn(ctx, e.pool, repo.Turn{ID: e.next(), SessionID: sessionID, Trigger: domain.TriggerUser})
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// assertNoPaidTools 断言每一次请求都没有付费与画布写工具（AG-05）。
func assertNoPaidTools(t *testing.T, chat *planChat) {
	t.Helper()
	if len(chat.tools) == 0 {
		t.Fatal("模型一次都没被调用")
	}
	for i, names := range chat.tools {
		if len(names) == 0 {
			t.Fatalf("第 %d 次请求没有任何工具（规划工具没注册上？）", i+1)
		}
		for _, n := range names {
			spec, ok := domain.SpecOf(domain.ToolName(n))
			if !ok || spec.Paid || spec.CanvasWrite {
				t.Fatalf("第 %d 次请求下发了付费 / 画布写工具 %s：%v", i+1, n, names)
			}
		}
	}
}

func TestPlanningTurn_请求体不含付费工具(t *testing.T) {
	e := newPlanEnv(t, 100)
	ctx := context.Background()
	s, err := e.r.Create(ctx, e.pool, repo.Session{ID: e.next(), WorkspaceID: ws, ProjectID: project,
		PayerAccountID: payer, CreatedByUserID: user, State: domain.StatePlanning,
		// 画幅 / 清晰度 / 时长已在偏好里选定（design/01 修订块 R-1：「智能」时带视频的 submit_plan 要求先确认）。
		Prefs: json.RawMessage(`{"reasoning":{"model":"m-agent"},"video":{"ratio":"9:16","resolution":"720p","duration":15}}`)})
	if err != nil {
		t.Fatal(err)
	}
	tr := e.userTurn(t, s.ID, "做一条三打白骨精的短片")
	chat := &planChat{outs: []agentapp.ChatOutput{
		planCall("c1", "read_canvas", `{}`),
		planCall("c2", "ask_user", `{"kind":"choice","title":"选方向","options":[{"label":"悟空孤影"},{"label":"三打燃战"}]}`),
	}}
	if out, err := e.runner(chat).RunTurn(ctx, s.ID, tr.ID); err != nil || out != agentapp.OutcomeDone {
		t.Fatalf("outcome=%v err=%v", out, err)
	}
	assertNoPaidTools(t, chat)
	if len(chat.tools) != 2 {
		t.Fatalf("ask_user 挂起后本轮应结束（2 次调用），实际 %d", len(chat.tools))
	}
	got, _ := e.r.GetByID(ctx, s.ID)
	if got.State != domain.StateAwaitingInput {
		t.Fatalf("应进 awaiting_input，实际 %s", got.State)
	}
	cards, _ := e.r.PendingBySession(ctx, s.ID)
	if len(cards) != 1 || cards[0].Kind != domain.CardChoice {
		t.Fatalf("应有一张待处理选择卡: %+v", cards)
	}
	if resume, err := domain.ResumeStateOf(cards[0].Domain()); err != nil || resume != domain.StatePlanning {
		t.Fatalf("resumeState 应为 planning: %v %v", resume, err)
	}
	// read_canvas 的工具结果里没有 params 全文。
	msgs, _ := e.r.ListRecent(ctx, s.ID, 20)
	for _, m := range msgs {
		if m.Role == repo.RoleTool && m.ToolCallID == "c1" {
			if strings.Contains(m.Content, "不该出现在摘要里") || !strings.Contains(m.Content, `"nodeCount":1`) {
				t.Fatalf("read_canvas 结果不对: %s", m.Content)
			}
		}
	}
}

func TestPlanningTurn_提交规划后会话awaiting_approval(t *testing.T) {
	e := newPlanEnv(t, 50)
	ctx := context.Background()
	if _, err := e.r.UpsertSkill(ctx, repo.Skill{ID: e.next(), Code: "story", Name: "故事选题", IsEnabled: true,
		Body:  "阶段 {{state}}：先写 plan.md 再提交规划",
		Tools: json.RawMessage(`["read_canvas","write_doc","submit_plan","load_skill","generate"]`)}); err != nil {
		t.Fatal(err)
	}
	s, err := e.r.Create(ctx, e.pool, repo.Session{ID: e.next(), WorkspaceID: ws, ProjectID: project,
		PayerAccountID: payer, CreatedByUserID: user, State: domain.StatePlanning,
		// 画幅 / 清晰度 / 时长已在偏好里选定（design/01 修订块 R-1：「智能」时带视频的 submit_plan 要求先确认）。
		Prefs: json.RawMessage(`{"reasoning":{"model":"m-agent"},"video":{"ratio":"9:16","resolution":"720p","duration":15}}`)})
	if err != nil {
		t.Fatal(err)
	}
	// 第一轮：问用户。
	tr := e.userTurn(t, s.ID, "做一条短片")
	chat := &planChat{outs: []agentapp.ChatOutput{
		planCall("c1", "ask_user", `{"kind":"choice","title":"选方向","options":[{"label":"A"},{"label":"B"}]}`),
	}}
	if _, err := e.runner(chat).RunTurn(ctx, s.ID, tr.ID); err != nil {
		t.Fatal(err)
	}
	// 用户提交卡片（模拟 API 的 SubmitCard：卡片 submitted + 回到 planning + 新一轮）。
	cards, _ := e.r.PendingBySession(ctx, s.ID)
	if len(cards) != 1 {
		t.Fatalf("应有一张选择卡: %+v", cards)
	}
	if ok, err := e.r.SubmitCard(ctx, e.pool, cards[0].ID, json.RawMessage(`{"selected":["1"]}`), user); err != nil || !ok {
		t.Fatalf("提交卡片失败: %v", err)
	}
	if ok, err := e.r.TransitionState(ctx, e.pool, s.ID, domain.StateAwaitingInput, domain.StatePlanning); err != nil || !ok {
		t.Fatalf("回到 planning 失败: %v", err)
	}
	tr2 := e.userTurn(t, s.ID, "选 A")

	// 第二轮：加载技能 → 写规划文档 → 提交规划（模型在计划项里塞了 credits:1）。
	chat2 := &planChat{outs: []agentapp.ChatOutput{
		planCall("d1", "load_skill", `{"code":"story"}`),
		planCall("d2", "write_doc", `{"path":"/plan.md","content":"# 三打白骨精\n- 15 秒视频"}`),
		planCall("d3", "submit_plan", `{"title":"三打白骨精","items":[{"type":"video","modelCode":"seedance","durationSec":15,"title":"主片","credits":1}]}`),
	}}
	if out, err := e.runner(chat2).RunTurn(ctx, s.ID, tr2.ID); err != nil || out != agentapp.OutcomeDone {
		t.Fatalf("outcome=%v err=%v", out, err)
	}
	assertNoPaidTools(t, chat2)
	if len(chat2.tools) != 3 {
		t.Fatalf("submit_plan 挂起后本轮应结束（3 次调用），实际 %d: %v", len(chat2.tools), chat2.tools)
	}
	// load_skill 之后的请求被技能白名单收窄：只剩 read_canvas / write_doc / submit_plan / load_skill
	// （白名单里的 generate 在规划态照样没有 —— 只能收窄不能放宽）。
	want := []string{"read_canvas", "write_doc", "submit_plan", "load_skill"}
	for i := 1; i < len(chat2.tools); i++ {
		if len(chat2.tools[i]) != len(want) {
			t.Fatalf("第 %d 次请求应被技能收窄为 %v，实际 %v", i+1, want, chat2.tools[i])
		}
		for j := range want {
			if chat2.tools[i][j] != want[j] {
				t.Fatalf("第 %d 次请求应被技能收窄为 %v，实际 %v", i+1, want, chat2.tools[i])
			}
		}
	}
	if len(chat2.tools[0]) <= len(want) {
		t.Fatalf("加载技能前不该收窄: %v", chat2.tools[0])
	}

	got, _ := e.r.GetByID(ctx, s.ID)
	if got.State != domain.StateAwaitingApproval {
		t.Fatalf("应进 awaiting_approval，实际 %s", got.State)
	}
	if string(got.SkillCodes) != `["story"]` {
		t.Fatalf("skill_codes 应含 story: %s", got.SkillCodes)
	}
	pending, _ := e.r.PendingBySession(ctx, s.ID)
	if len(pending) != 1 || pending[0].Kind != domain.CardPlanConfirm {
		t.Fatalf("应有一张待处理规划卡: %+v", pending)
	}
	var p domain.PlanConfirmPayload
	if err := json.Unmarshal(pending[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	// 15 秒 × 4 = 60，余额 50 → 差 10、不可批准；模型给的 credits:1 被忽略。
	if p.Total != 60 || p.Balance != 50 || p.Shortfall != 10 || p.CanApprove || p.Items[0].Credits != 60 {
		t.Fatalf("卡片金额 / 余额 / 差额不对: %+v", p)
	}
	if p.DocPath != "/plan.md" || p.PlanDocVersion != 1 {
		t.Fatalf("应记规划文档版本: %+v", p)
	}
	// 画布上多了一个 document 节点，只存 docId，agent.sessionId 正确。
	var raw json.RawMessage
	if err := e.pool.QueryRow(ctx, `select canvas_data from project where id = $1`, project).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Nodes []struct {
			Type   string         `json:"type"`
			Params map[string]any `json:"params"`
			Agent  map[string]any `json:"agent"`
		} `json:"nodes"`
	}
	_ = json.Unmarshal(raw, &doc)
	var docNodes int
	var docID string
	if err := e.pool.QueryRow(ctx, `select id::text from project_document where project_id = $1 and path = '/plan.md'`,
		project).Scan(&docID); err != nil {
		t.Fatal(err)
	}
	for _, n := range doc.Nodes {
		if n.Type != "document" {
			continue
		}
		docNodes++
		if n.Params["docId"] != docID || len(n.Params) != 1 || n.Agent["sessionId"] != strconv.FormatInt(s.ID, 10) {
			t.Fatalf("文档节点不对: %+v（docId=%s）", n, docID)
		}
	}
	if docNodes != 1 {
		t.Fatalf("应有一个文档节点，实际 %d: %s", docNodes, raw)
	}

	// 再提交一次规划：上一张 pending 的规划卡被作废。
	if ok, err := e.r.TransitionState(ctx, e.pool, s.ID, domain.StateAwaitingApproval, domain.StatePlanning); err != nil || !ok {
		t.Fatalf("回规划失败: %v", err)
	}
	tr3 := e.userTurn(t, s.ID, "改成 10 秒")
	chat3 := &planChat{outs: []agentapp.ChatOutput{
		planCall("e1", "submit_plan", `{"items":[{"type":"video","modelCode":"seedance","durationSec":10}]}`),
	}}
	if _, err := e.runner(chat3).RunTurn(ctx, s.ID, tr3.ID); err != nil {
		t.Fatal(err)
	}
	var voided, pend int
	if err := e.pool.QueryRow(ctx, `select count(*) filter (where status='voided'), count(*) filter (where status='pending')
		from agent_card where session_id = $1 and kind = 'plan_confirm'`, s.ID).Scan(&voided, &pend); err != nil {
		t.Fatal(err)
	}
	if voided != 1 || pend != 1 {
		t.Fatalf("旧规划卡应 voided、新卡 pending：voided=%d pending=%d", voided, pend)
	}
}

// design/01 修订块 R-1（会话 107419662995685376）：画幅 / 清晰度 / 时长是「智能」时，带视频的 submit_plan 被退回；
// 按固定题目 id 问过之后，回答从真库的问答卡里读出来，写进提示词、规划照常提交。
func TestPlanningTurn_智能规格先确认再提交(t *testing.T) {
	e := newPlanEnv(t, 100)
	ctx := context.Background()
	s, err := e.r.Create(ctx, e.pool, repo.Session{ID: e.next(), WorkspaceID: ws, ProjectID: project,
		PayerAccountID: payer, CreatedByUserID: user, State: domain.StatePlanning,
		Prefs: json.RawMessage(`{"reasoning":{"model":"m-agent"},"video":{"model":"seedance"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	plan := `{"title":"短片","items":[{"type":"video","modelCode":"seedance","durationSec":15,"title":"主片"}]}`
	// 第一轮：没问就提交 → 退回；再按固定题目 id 发问答卡。
	tr := e.userTurn(t, s.ID, "做一条短片")
	chat := &planChat{outs: []agentapp.ChatOutput{
		planCall("c1", "submit_plan", plan),
		planCall("c2", "ask_user", `{"kind":"questions","title":"确认规格","questions":[`+
			`{"id":"aspect_ratio","text":"画幅？","type":"single","options":[{"label":"9:16 竖屏（推荐）"},{"label":"16:9 横屏"}]},`+
			`{"id":"resolution","text":"清晰度？","type":"single","options":[{"label":"720p（推荐）"},{"label":"1080p"}]},`+
			`{"id":"duration","text":"时长（秒）？","type":"text"}]}`),
	}}
	if _, err := e.runner(chat).RunTurn(ctx, s.ID, tr.ID); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(chat.systems[0], "画幅、清晰度、时长：用户没有指定") {
		t.Fatalf("第一轮提示词应要求先确认：\n%s", chat.systems[0])
	}
	cards, _ := e.r.PendingBySession(ctx, s.ID)
	if len(cards) != 1 || cards[0].Kind != domain.CardQuestions {
		t.Fatalf("没确认时不发规划卡、只有问答卡：%+v", cards)
	}
	sub := `{"answers":[{"questionId":"aspect_ratio","selected":["1"]},{"questionId":"resolution","selected":["1"]},` +
		`{"questionId":"duration","text":"15 秒"}]}`
	if ok, err := e.r.SubmitCard(ctx, e.pool, cards[0].ID, json.RawMessage(sub), user); err != nil || !ok {
		t.Fatalf("提交问答卡失败: %v", err)
	}
	if ok, err := e.r.TransitionState(ctx, e.pool, s.ID, domain.StateAwaitingInput, domain.StatePlanning); err != nil || !ok {
		t.Fatalf("回到 planning 失败: %v", err)
	}
	// 第二轮：确认过的规格进提示词，规划照常提交。
	tr2 := e.userTurn(t, s.ID, "我的回答：…")
	chat2 := &planChat{outs: []agentapp.ChatOutput{planCall("d1", "submit_plan", plan)}}
	if _, err := e.runner(chat2).RunTurn(ctx, s.ID, tr2.ID); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"- 画幅：9:16", "- 视频清晰度：720p", "- 视频时长：15 秒"} {
		if !strings.Contains(chat2.systems[0], want) {
			t.Fatalf("第二轮提示词缺 %q：\n%s", want, chat2.systems[0])
		}
	}
	if got, _ := e.r.GetByID(ctx, s.ID); got.State != domain.StateAwaitingApproval {
		t.Fatalf("确认过之后应能提交规划，实际 %s", got.State)
	}
}
