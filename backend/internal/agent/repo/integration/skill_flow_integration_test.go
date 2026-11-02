//go:build integration

// 首批技能（AGT-30）的真库测试：技能来自真 seed（seed.Apply），会话 / 卡片 / 画布 / 文档 /
// 生成受理 / 账本 / 扫描唤醒都用真库与真服务；推理模型、成员资格与报价是假实现。
//
// 要证明的是：seed 出来的技能真的能被 load_skill 加载、渲染进系统提示词、按白名单收窄工具
// （且只能收窄），并且按 video_single 的指引能把单段流程从规划走到 idle。
package integration

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/stdlib"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	agenttools "github.com/li904338203/tabtv/backend/internal/agent/tools"
	billingapp "github.com/li904338203/tabtv/backend/internal/billing/app"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	genrepo "github.com/li904338203/tabtv/backend/internal/generation/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/seed"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

// flowChat 按顺序回放预设输出，记下每次请求的工具名与系统提示词。
type flowChat struct {
	mu     sync.Mutex
	outs   []agentapp.ChatOutput
	tools  [][]string
	system []string
}

func (c *flowChat) Chat(_ context.Context, in agentapp.ChatInput, _ func(string)) (agentapp.ChatOutput, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var names []string
	for _, t := range in.Tools {
		names = append(names, t.Name)
	}
	c.tools = append(c.tools, names)
	c.system = append(c.system, in.System)
	if len(c.outs) == 0 {
		return agentapp.ChatOutput{Text: "好了"}, nil
	}
	o := c.outs[0]
	c.outs = c.outs[1:]
	return o, nil
}

// flowPricer：每个计划项固定 paidPrice（与生成报价一致，批准额度恰好够一条）。
type flowPricer struct{}

func (flowPricer) PriceItem(_ context.Context, it domain.PlanItem, _ int64) (agentapp.PricedItem, error) {
	it.Credits = paidPrice
	return agentapp.PricedItem{Item: it, Unit: "per_time", Available: true}, nil
}

// flowPlans 是批准时的重新报价（与 flowPricer 同价，不触发「价格已更新」）。
type flowPlans struct{}

func (flowPlans) QuotePlan(_ context.Context, items []domain.PlanItem, _ int64) ([]domain.PlanItem, error) {
	out := make([]domain.PlanItem, len(items))
	for i, it := range items {
		it.Credits = paidPrice
		out[i] = it
	}
	return out, nil
}

type flowEnv struct {
	*paidEnv
	svc   *agentapp.Service
	actor agentapp.SessionActor
}

func applySeed(t *testing.T, e *paidEnv) {
	t.Helper()
	db := stdlib.OpenDBFromPool(e.pool)
	defer db.Close()
	if err := seed.Apply(context.Background(), db, e.next); err != nil {
		t.Fatal(err)
	}
}

func newFlowEnv(t *testing.T) *flowEnv {
	t.Helper()
	e := newPaidEnv(t, map[int64]int{payer: 1000})
	applySeed(t, e)
	planning := agenttools.PlanningTools(agenttools.PlanningDeps{
		Store: e.r, Canvas: wiring.AgentCanvas{Svc: e.canvas},
		Generations: wiring.AgentNodeGenerations{Repo: genrepo.New(e.pool)},
		Assets:      planAssets{}, Pricer: flowPricer{},
		Balances:  wiring.AgentAccounts{Billing: billingapp.New(e.bill)},
		Documents: wiring.AgentDocuments{Svc: e.canvas}, Skills: wiring.AgentSkillRenderer{},
		NextID: e.next, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	reg, err := agenttools.NewRegistry(append(planning, e.tools...)...)
	if err != nil {
		t.Fatal(err)
	}
	e.reg = reg
	svc := agentapp.NewService(e.r, e.next, agentapp.ServicePorts{
		Projects: wiring.AgentAPIProjects{Svc: e.canvas},
		Plans:    flowPlans{},
		Accounts: wiring.AgentAccounts{Billing: billingapp.New(e.bill)},
		Tasks:    e.cgen,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return &flowEnv{paidEnv: e, svc: svc, actor: agentapp.SessionActor{WorkspaceID: ws, PayerAccountID: payer,
		UserID: user, Role: collab.RoleEditor}}
}

func (e *flowEnv) runner(chat agentapp.ChatModel) *agentapp.Runner {
	return agentapp.NewRunner(agentapp.RunnerDeps{
		Store: agentapp.NewRepoStore(e.r), Chat: chat, Quoter: paidQuoter{paidPrice},
		Ledger:  wiring.AgentLedger{Repo: e.bill, Compensate: 30 * 24 * time.Hour},
		Members: paidMembers{collab.RoleEditor, true}, Canvas: wiring.AgentCanvas{Svc: e.canvas},
		Documents: wiring.AgentDocuments{Svc: e.canvas}, Tools: e.reg,
		SkillRender: wiring.AgentSkillRenderer{}, NextID: e.next,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, agentapp.RunnerConfig{MaxCalls: 8, ContextChars: 60000})
}

func (e *flowEnv) run(t *testing.T, sessionID, turnID int64, outs ...agentapp.ChatOutput) *flowChat {
	t.Helper()
	chat := &flowChat{outs: outs}
	if out, err := e.runner(chat).RunTurn(context.Background(), sessionID, turnID); err != nil ||
		out != agentapp.OutcomeDone {
		t.Fatalf("outcome=%v err=%v", out, err)
	}
	return chat
}

func (e *flowEnv) state(t *testing.T, sessionID int64) domain.State {
	t.Helper()
	s, err := e.r.GetByID(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	return s.State
}

func (e *flowEnv) pendingCard(t *testing.T, sessionID int64) repo.Card {
	t.Helper()
	cards, err := e.r.PendingBySession(context.Background(), sessionID)
	if err != nil || len(cards) != 1 {
		t.Fatalf("应恰好 1 张待提交卡片：%v err=%v", cards, err)
	}
	return cards[0]
}

func (e *flowEnv) submit(t *testing.T, sessionID, cardID int64, submission string) int64 {
	t.Helper()
	res, err := e.svc.SubmitCard(context.Background(), e.actor, sessionID, cardID, json.RawMessage(submission))
	if err != nil {
		t.Fatalf("提交卡片失败：%v", err)
	}
	if res.TurnID == 0 {
		t.Fatalf("提交卡片应建新一轮：%+v", res)
	}
	return res.TurnID
}

type flowNode struct {
	ID      string         `json:"id"`
	Type    string         `json:"type"`
	Title   string         `json:"title"`
	Members []string       `json:"members"`
	Agent   map[string]any `json:"agent"`
}

func (e *flowEnv) nodes(t *testing.T) []flowNode {
	t.Helper()
	var raw json.RawMessage
	if err := e.pool.QueryRow(context.Background(), `select canvas_data from project where id = $1`,
		project).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Nodes []flowNode `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Nodes
}

func byType(nodes []flowNode, typ string) []flowNode {
	var out []flowNode
	for _, n := range nodes {
		if n.Type == typ {
			out = append(out, n)
		}
	}
	return out
}

func toolSet(names []string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

// ---------------------------------------------------------------- seed

func TestSeed_重复执行不覆盖已改内容(t *testing.T) {
	e := newPaidEnv(t, nil)
	ctx := context.Background()
	applySeed(t, e)
	// ⚠️ 不写死条数：技能库 2026-11-01 从 3 条扩到 13 条时这里没跟着改，一直是红的（README §5 B-17）。
	if n, want := e.count(t, `select count(*)::int from agent_skill`), seed.AgentSkillCount(); n != want {
		t.Fatalf("agent_skill 应 %d 行，实际 %d", want, n)
	}
	refs := e.count(t, `select count(*)::int from agent_skill_reference`)
	if refs < 2 {
		t.Fatalf("agent_skill_reference 应 ≥ 2 行，实际 %d", refs)
	}
	if n := e.count(t, `select count(*)::int from agent_skill where body not like '【待运营定稿】%'`); n != 0 {
		t.Fatalf("%d 个技能正文首行未标注待运营定稿", n)
	}

	// 运营改了正文与一份附属文档、停用了一个技能。
	// 用户在设置页改过（tabtv：内置版本规则据 is_user_modified 判断，TT-T-24）。
	if _, err := e.pool.Exec(ctx, `update agent_skill set body = '运营定稿的正文', is_enabled = false,
		is_user_modified = true, version = version + 1 where code = 'video_single'`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `update agent_skill_reference set body = '运营定稿的指引'
		where name = '单段流程工作指引'`); err != nil {
		t.Fatal(err)
	}

	applySeed(t, e)
	if n, want := e.count(t, `select count(*)::int from agent_skill`), seed.AgentSkillCount(); n != want {
		t.Fatalf("重复 seed 后 agent_skill 应仍 %d 行，实际 %d", want, n)
	}
	if n := e.count(t, `select count(*)::int from agent_skill_reference`); n != refs {
		t.Fatalf("重复 seed 后附属文档行数应不变：%d → %d", refs, n)
	}
	sk, err := e.r.GetSkillByCode(ctx, "video_single")
	if err != nil {
		t.Fatal(err)
	}
	if sk.Body != "运营定稿的正文" || sk.IsEnabled || sk.Version != 2 {
		t.Fatalf("seed 不应覆盖运营改过的技能：body=%q enabled=%v version=%d", sk.Body, sk.IsEnabled, sk.Version)
	}
	ref, err := e.r.SkillReferenceByName(ctx, sk.ID, "单段流程工作指引")
	if err != nil || ref.Body != "运营定稿的指引" {
		t.Fatalf("seed 不应覆盖运营改过的附属文档：%q err=%v", ref.Body, err)
	}
}

// ---------------------------------------------------------------- 白名单

func TestSkillFlow_选题技能收窄执行态工具(t *testing.T) {
	e := newFlowEnv(t)
	s, tr := e.seed(t, domain.StateExecuting, 100, payer)
	chat := e.run(t, s.ID, tr.ID, call("c1", "load_skill", `{"code":"story_topic"}`))
	if len(chat.tools) != 2 {
		t.Fatalf("应调用模型 2 次，实际 %d", len(chat.tools))
	}
	if got := toolSet(chat.tools[0]); !got["generate"] || !got["canvas_apply"] {
		t.Fatalf("加载前执行态应有付费与画布写工具：%v", chat.tools[0])
	}
	sk, err := e.r.GetSkillByCode(context.Background(), "story_topic")
	if err != nil {
		t.Fatal(err)
	}
	var wl []domain.ToolName
	var raw []string
	_ = json.Unmarshal(sk.Tools, &raw)
	for _, n := range raw {
		wl = append(wl, domain.ToolName(n))
	}
	want := domain.AllowedTools(domain.StateExecuting, wl)
	got := chat.tools[1]
	if len(got) != len(want) {
		t.Fatalf("加载后应收窄为白名单 %v，实际 %v", want, got)
	}
	for i := range want {
		if string(want[i]) != got[i] {
			t.Fatalf("加载后应收窄为白名单 %v，实际 %v", want, got)
		}
	}
	if gs := toolSet(got); gs["generate"] || gs["canvas_apply"] || gs["cancel_task"] {
		t.Fatalf("故事选题不应带付费 / 画布写工具：%v", got)
	}
	if !strings.Contains(chat.system[1], "## 已加载技能：故事选题") ||
		!strings.Contains(chat.system[1], "当前会话阶段：executing") {
		t.Fatalf("技能正文应渲染进系统提示词")
	}
}

func TestSkillFlow_白名单不能放宽规划态(t *testing.T) {
	e := newFlowEnv(t)
	ctx := context.Background()
	// 运营误把付费工具写进白名单：规划态照样拿不到。
	if _, err := e.pool.Exec(ctx, `update agent_skill set tools = '["generate","canvas_apply","read_canvas","submit_plan"]'
		where code = 'story_topic'`); err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"video_single", "story_topic"} {
		s, tr := e.seed(t, domain.StatePlanning, 0, payer)
		chat := e.run(t, s.ID, tr.ID, call("c1", "load_skill", `{"code":"`+code+`"}`))
		if len(chat.tools) != 2 {
			t.Fatalf("%s：应调用模型 2 次，实际 %d", code, len(chat.tools))
		}
		for i, names := range chat.tools {
			for _, n := range names {
				spec, ok := domain.SpecOf(domain.ToolName(n))
				if !ok || spec.Paid || spec.CanvasWrite {
					t.Fatalf("%s：第 %d 次规划态请求出现 %s：%v", code, i+1, n, names)
				}
			}
		}
		if code == "video_single" && len(chat.tools[1]) != len(domain.AllowedTools(domain.StatePlanning, nil)) {
			t.Fatalf("video_single 不收窄：%v", chat.tools[1])
		}
		if code == "story_topic" {
			if gs := toolSet(chat.tools[1]); len(gs) != 2 || !gs["read_canvas"] || !gs["submit_plan"] {
				t.Fatalf("规划态应只剩白名单与状态机的交集 read_canvas、submit_plan：%v", chat.tools[1])
			}
		}
	}
}

// ---------------------------------------------------------------- 单段流程

func TestSkillFlow_单段流程走完回idle(t *testing.T) {
	e := newFlowEnv(t)
	ctx := context.Background()
	s, tr := e.seed(t, domain.StatePlanning, 0, payer)

	// 第 1 轮（规划）：加载技能 → 读指引 → 发选择卡。
	chat := e.run(t, s.ID, tr.ID,
		call("c1", "load_skill", `{"code":"video_single"}`),
		call("c2", "read_skill_ref", `{"code":"video_single","name":"单段流程工作指引"}`),
		call("c3", "ask_user", `{"kind":"choice","title":"选一个方向","options":[`+
			`{"id":"a","label":"悟空孤影"},{"id":"b","label":"三打燃战"}]}`),
	)
	assertPlanningOnly(t, chat)
	if !strings.Contains(chat.system[1], "## 已加载技能：视频生成（单段）") ||
		!strings.Contains(chat.system[1], "当前会话阶段：planning") || strings.Contains(chat.system[1], "{{") {
		t.Fatal("video_single 正文应渲染后进系统提示词")
	}
	if got := e.state(t, s.ID); got != domain.StateAwaitingInput {
		t.Fatalf("发卡后应 awaiting_input，实际 %s", got)
	}

	// 用户选方向 → 第 2 轮（规划）：写 /plan.md → 提交规划。
	t2 := e.submit(t, s.ID, e.pendingCard(t, s.ID).ID, `{"selected":["a"]}`)
	chat = e.run(t, s.ID, t2,
		call("c4", "write_doc", `{"path":"/plan.md","content":"# 悟空孤影\n\n- 时长 10 秒，竖屏\n- 阶段：山道 → 识破 → 举棒"}`),
		call("c5", "submit_plan", `{"title":"悟空孤影","items":[{"type":"video","modelCode":"dev_video","durationSec":10,"title":"成片"}]}`),
	)
	assertPlanningOnly(t, chat)
	if got := e.state(t, s.ID); got != domain.StateAwaitingApproval {
		t.Fatalf("提交规划后应 awaiting_approval，实际 %s", got)
	}

	// 用户批准 → 第 3 轮（执行）：生成一个视频节点。
	t3 := e.submit(t, s.ID, e.pendingCard(t, s.ID).ID, `{"decision":"approve"}`)
	if got := e.state(t, s.ID); got != domain.StateExecuting {
		t.Fatalf("批准后应 executing，实际 %s", got)
	}
	chat = e.run(t, s.ID, t3,
		call("c6", "generate", `{"nodeType":"video","title":"悟空孤影","modelCode":"dev_video",`+
			`"operationCode":"text2video","prompt":"山道上，悟空识破白骨精，举棒","durationSec":10}`),
	)
	if gs := toolSet(chat.tools[0]); !gs["generate"] || !gs["canvas_apply"] {
		t.Fatalf("执行态 video_single 不收窄：%v", chat.tools[0])
	}
	if got := e.state(t, s.ID); got != domain.StateWaitingTask {
		t.Fatalf("提交生成后应 waiting_task，实际 %s", got)
	}
	gens := e.generations(t)
	if len(gens) != 1 {
		t.Fatalf("应恰好 1 个生成任务：%+v", gens)
	}

	// 任务成功（假上游立即终态）→ 扫描唤醒。
	if _, err := e.pool.Exec(ctx, `update generation set status = 'success' where id = $1`, gens[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `insert into generation_output (id, generation_id, idx, media_object_id, output_type)
		values ($1, $2, 0, $3, 'video')`, e.next(), gens[0].ID, e.next()); err != nil {
		t.Fatal(err)
	}
	sc := agentapp.NewScanner(agentapp.ScannerDeps{
		Repo: e.r, Tasks: wiring.AgentTaskStatuses{Repo: genrepo.New(e.pool)},
		Ledger: wiring.AgentLedger{Repo: e.bill, Compensate: 30 * 24 * time.Hour}, NextID: e.next,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, agentapp.ScannerConfig{RequeueQuiet: 2 * time.Second})
	if st, err := sc.Tick(ctx, 0); err != nil || st.Woken != 1 {
		t.Fatalf("应唤醒 1 个会话：%+v err=%v", st, err)
	}
	var t4 int64
	if err := e.pool.QueryRow(ctx, `select id from agent_turn where session_id = $1 and trigger = 'system'
		and status = 'pending' order by created_at desc limit 1`, s.ID).Scan(&t4); err != nil {
		t.Fatal(err)
	}

	// 第 4 轮（执行）：画框把规划文档与视频框在一起 → 总结。
	docs, videos := byType(e.nodes(t), "document"), byType(e.nodes(t), "video")
	if len(docs) != 1 || len(videos) != 1 {
		t.Fatalf("画布上应有 1 个文档节点与 1 个视频节点：docs=%v videos=%v", docs, videos)
	}
	frameArgs, _ := json.Marshal(map[string]any{"ops": []map[string]any{{"kind": "add_frame",
		"frame": map[string]any{"title": "悟空孤影", "members": []string{docs[0].ID, videos[0].ID}}}}})
	e.run(t, s.ID, t4,
		call("c7", "canvas_apply", string(frameArgs)),
		agentapp.ChatOutput{Text: "已按以下计划生成，请查看效果：悟空孤影（10 秒）。"},
	)
	if got := e.state(t, s.ID); got != domain.StateIdle {
		t.Fatalf("总结后应回 idle，实际 %s", got)
	}

	nodes := e.nodes(t)
	frames := byType(nodes, "frame")
	if len(byType(nodes, "video")) != 1 || len(byType(nodes, "document")) != 1 || len(frames) != 1 {
		t.Fatalf("画布上应 1 视频 + 1 文档 + 1 画框：%+v", nodes)
	}
	members := toolSet(frames[0].Members)
	if len(members) != 2 || !members[docs[0].ID] || !members[videos[0].ID] {
		t.Fatalf("画框成员应为文档与视频：%v", frames[0].Members)
	}
	if frames[0].Title != "悟空孤影" {
		t.Errorf("画框标题应为规划标题：%q", frames[0].Title)
	}
	// 只扣了一次生成费（对话轮报价为 0）。
	if n := e.count(t, `select count(*)::int from credit_transaction where payer_account_id = $1 and type = 'consume'`,
		payer); n != 1 {
		t.Errorf("应只有 1 笔生成消费，实际 %d", n)
	}
}

func assertPlanningOnly(t *testing.T, chat *flowChat) {
	t.Helper()
	for i, names := range chat.tools {
		for _, n := range names {
			spec, ok := domain.SpecOf(domain.ToolName(n))
			if !ok || spec.Paid || spec.CanvasWrite {
				t.Fatalf("第 %d 次规划态请求出现 %s：%v", i+1, n, names)
			}
		}
	}
}
