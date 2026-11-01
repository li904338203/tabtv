//go:build integration

// 生意增长顾问的链路验证（MKT-T-26，MKD-11、MK-14）：顾问**没有自己的代码路径**，
// 它只是一条营销场景技能 —— 纯对话按轮扣 agent_chat，过程中要出图走规划确认卡、按图片单位另扣。
//
// 技能经管理端服务（SkillAdmin，含营销场景的槽位 / 口播校验）自建，会话 / 卡片 / 画布 / 文档 /
// 生成受理 / 账本全部真库；推理模型、成员资格、报价是假实现（对话轮 advisorTurnPrice、生成 paidPrice）。
//
// ⚠️ 两类扣费要**分别成行**：按轮扣的是 ref_type=agent_turn（ref_id = 轮次），出图扣的是
// ref_type=generation（ref_id = 任务）。合成一行的话「这一轮聊天花了多少」与「这张图花了多少」
// 就再也分不开了，而用量审计（CD-60）按的正是这两列。
package integration

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	agenttools "github.com/li904338203/tabtv/backend/internal/agent/tools"
	billingapp "github.com/li904338203/tabtv/backend/internal/billing/app"
	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	genrepo "github.com/li904338203/tabtv/backend/internal/generation/repo"
	mktdomain "github.com/li904338203/tabtv/backend/internal/marketing/domain"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

// advisorTurnPrice 是假 agent_chat 报价（与生成价 paidPrice 不同，两类流水一眼分得开）。
const advisorTurnPrice = 3

// advisorCode 与 03 选项建议稿 S-13 一致；内容（正文、预填）属 PARAM-12，这里是测试数据。
const advisorCode = "mk_growth_advisor"

// advisorSlots：商品**可选**（03 S-13「商品可、文字必」）。⚠️ 不能写成 `[]` —— 空声明等于
// 「用场景默认声明」，而默认里商品是必填的（wiring.AgentScene.ValidateSlotFill），纯咨询会被挡住。
const advisorSlots = `[{"key":"product","label":"商品","accept":["image"],"required":false,"maxCount":20,` +
	`"fields":[{"key":"name","label":"商品名称"},{"key":"sellingPoints","label":"卖点"}]},` +
	`{"key":"character","label":"角色","accept":["image","character_asset"],"required":false,"maxCount":2}]`

// advisorTools 是顾问的白名单：只读 + 文档 + ask_user + quote + 规划确认（submit_plan，免费）+ 执行态生成。
// submit_plan 不能省：营销会话里付费 generate 必须对应已批准的计划项（MKT-T-22），没有它就出不了确认卡。
var advisorTools = []string{"read_canvas", "list_assets", "read_doc", "write_doc", "ask_user", "quote",
	"list_marketing_options", "view_media", "submit_plan", "generate", "load_skill", "read_skill_ref"}

// advisorQuoter：对话轮按 agent_chat 报 advisorTurnPrice，生成报 paidPrice。
type advisorQuoter struct{}

func (advisorQuoter) QuoteGeneration(context.Context, agentapp.QuoteInput) (agentapp.Quote, error) {
	return agentapp.Quote{Credits: paidPrice}, nil
}
func (advisorQuoter) QuoteTurn(context.Context, string, int64) (agentapp.Quote, error) {
	return agentapp.Quote{Credits: advisorTurnPrice}, nil
}

// advisorScene 是营销会话的场景段与营销输入：场景段用**代码兜底**（catalog 的 promptFallback，
// 与线上 DB 行渲染失败时回落的是同一份），营销输入用 marketing/domain.SlotSummary（全仓唯一实现）。
type advisorScene struct{}

func (advisorScene) RenderScenePrompt(_ context.Context, code, _ string, vars map[string]string) (string, error) {
	return catalogapp.New(nil, nil).RenderFallbackPrompt(code, vars)
}
func (advisorScene) FallbackScenePrompt(code string, vars map[string]string) (string, error) {
	return catalogapp.New(nil, nil).RenderFallbackPrompt(code, vars)
}
func (advisorScene) MaxVariants() int { return 4 }
func (advisorScene) MarketingSummary(_ context.Context, _ int64, in agentapp.SceneInput, visible int) (string, error) {
	return mktdomain.SlotSummary(mktdomain.SummaryInput{ProductName: in.Product.Name,
		SellingPoints: in.Product.SellingPoints, ProductImageCount: len(in.Products), VisibleImageCount: visible,
		MaxVariants: 4}), nil
}

// advisorPlanScene：顾问会话没有商品图与选项 —— 选图上限查不到（不选图），不钉任何选项。
type advisorPlanScene struct{}

func (advisorPlanScene) ImageLimit(context.Context, domain.PlanItem) (int, bool, error) {
	return 0, false, nil
}
func (advisorPlanScene) PlanPicks(context.Context, agentapp.PlanPickQuery, time.Time) ([]agentapp.PlanPickFact, error) {
	return nil, nil
}
func (advisorPlanScene) TalkNotice([]string) string { return "" }

type advisorEnv struct {
	*paidEnv
	svc   *agentapp.Service
	actor agentapp.SessionActor
}

func newAdvisorEnv(t *testing.T) *advisorEnv {
	t.Helper()
	e := newPaidEnv(t, map[int64]int{payer: 1000})
	ac := wiring.AgentCanvas{Svc: e.canvas}
	planning := agenttools.PlanningTools(agenttools.PlanningDeps{
		Store: e.r, Canvas: ac, Generations: wiring.AgentNodeGenerations{Repo: genrepo.New(e.pool)},
		Assets: planAssets{}, Pricer: flowPricer{},
		Balances:  wiring.AgentAccounts{Billing: billingapp.New(e.bill)},
		Documents: wiring.AgentDocuments{Svc: e.canvas}, Skills: wiring.AgentSkillRenderer{},
		Scene: advisorPlanScene{}, NextID: e.next, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	// 营销会话的 generate 要接计划来源（真 agent_session / agent_card）。
	paid := []agentapp.Tool{
		agenttools.CanvasApply{Canvas: ac},
		agenttools.Generate{Canvas: ac, Quoter: paidQuoter{paidPrice}, Generator: e.cgen, Budget: e.budget,
			Plans: wiring.AgentPlanSource{Repo: e.r}},
		agenttools.CancelTask{Generator: e.cgen, Steps: e.r},
	}
	reg, err := agenttools.NewRegistry(append(planning, paid...)...)
	if err != nil {
		t.Fatal(err)
	}
	e.reg = reg
	svc := agentapp.NewService(e.r, e.next, agentapp.ServicePorts{
		Projects: wiring.AgentAPIProjects{Svc: e.canvas}, Plans: flowPlans{},
		Accounts: wiring.AgentAccounts{Billing: billingapp.New(e.bill)}, Tasks: e.cgen,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	createAdvisorSkill(t, e)
	return &advisorEnv{paidEnv: e, svc: svc, actor: agentapp.SessionActor{WorkspaceID: ws, PayerAccountID: payer,
		UserID: user, Role: collab.RoleEditor}}
}

// createAdvisorSkill 经管理端服务建顾问技能（真实的营销槽位 / 口播校验）并启用。
func createAdvisorSkill(t *testing.T, e *paidEnv) {
	t.Helper()
	ctx := context.Background()
	a := agentapp.NewSkillAdmin(e.r, itSkillModels{}, wiring.AgentSkillRenderer{}, e.next)
	a.SetMarketingChecks(wiring.AgentSkillCopy{}, wiring.AgentSkillSlots{})
	pre := "我想提升这个商品的销量，请帮我分析目标人群、核心卖点和适合的短视频内容方向。"
	saved, err := a.Create(ctx, 1, agentapp.SkillInput{
		Code: advisorCode, Name: "生意增长顾问", Category: "营销洞察",
		Description: "聊聊你的商品和生意，帮你梳理卖点、人群、内容方向与投放思路。",
		Body: "当前阶段 {{state}}。先问清商品、价格带、主要渠道、目前的问题；结论写进 /notes/增长建议.md。" +
			"没有外部数据源：不编造行业 / 竞品 / 平台数据，涉及时写明「需你核实」；不承诺 ROI 或销量。" +
			"用户要出图 / 出片时先 submit_plan，确认后再生成。",
		Steps: []domain.SkillStep{{No: 1, Name: "问诊"}, {No: 2, Name: "分析与建议"},
			{No: 3, Name: "落地（可选）", Checkpoint: true}},
		Tools: advisorTools, CoverMediaID: e.next(),
		Scenes: []string{"marketing"}, InputSlots: json.RawMessage(advisorSlots), PrefillText: &pre,
	})
	if err != nil {
		t.Fatalf("经管理端建顾问技能失败：%v", err)
	}
	if _, err := a.SetEnabled(ctx, 1, saved.Skill.ID, true); err != nil {
		t.Fatalf("启用顾问技能失败：%v", err)
	}
}

// seedAdvisor 建一个**没有商品图**的顾问会话（与 POST /agent/sessions 营销分支落库的形状一致；
// 「无商品图能不能建」由 transport 层 TestAdvisor_纯咨询无商品图可建会话 钉住）+ 首条用户消息 + 轮次。
func (e *advisorEnv) seedAdvisor(t *testing.T, text string) (repo.Session, int64) {
	t.Helper()
	ctx := context.Background()
	si := `{"products":[],"product":{"name":"","sellingPoints":""},"characters":[],"sourceVideo":null,` +
		`"picks":{"creative":null,"hook":null,"style":null},"pins":[]}`
	s, err := e.r.Create(ctx, e.pool, repo.Session{ID: e.next(), WorkspaceID: ws, ProjectID: project,
		PayerAccountID: payer, CreatedByUserID: user, State: domain.StatePlanning,
		SkillCodes: json.RawMessage(`["` + advisorCode + `"]`),
		Prefs:      json.RawMessage(`{"reasoning":{"model":"m-agent"}}`), Scene: domain.SceneMarketing,
		SceneInput: json.RawMessage(si)})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := e.r.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.Append(ctx, tx, repo.Message{ID: e.next(), SessionID: s.ID, Role: repo.RoleUser,
		Content: text}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tr, err := e.r.InsertTurn(ctx, e.pool, repo.Turn{ID: e.next(), SessionID: s.ID, Trigger: domain.TriggerUser})
	if err != nil {
		t.Fatal(err)
	}
	return s, tr.ID
}

func (e *advisorEnv) runner(chat agentapp.ChatModel) *agentapp.Runner {
	return agentapp.NewRunner(agentapp.RunnerDeps{
		Store: agentapp.NewRepoStore(e.r), Chat: chat, Quoter: advisorQuoter{},
		Ledger:  wiring.AgentLedger{Repo: e.bill, Compensate: 30 * 24 * time.Hour},
		Members: paidMembers{collab.RoleEditor, true}, Canvas: wiring.AgentCanvas{Svc: e.canvas},
		Documents: wiring.AgentDocuments{Svc: e.canvas}, Tools: e.reg, Scene: advisorScene{},
		SkillRender: wiring.AgentSkillRenderer{}, NextID: e.next,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, agentapp.RunnerConfig{MaxCalls: 8, ContextChars: 60000})
}

func (e *advisorEnv) run(t *testing.T, sessionID, turnID int64, outs ...agentapp.ChatOutput) *flowChat {
	t.Helper()
	chat := &flowChat{outs: outs}
	if out, err := e.runner(chat).RunTurn(context.Background(), sessionID, turnID); err != nil ||
		out != agentapp.OutcomeDone {
		t.Fatalf("outcome=%v err=%v", out, err)
	}
	return chat
}

func (e *advisorEnv) post(t *testing.T, sessionID int64, key, text string) int64 {
	t.Helper()
	w, err := e.svc.PostMessage(context.Background(), e.actor, sessionID,
		agentapp.PostMessageInput{IdempotencyKey: key, Text: text})
	if err != nil || w.TurnID == 0 {
		t.Fatalf("发消息失败：%+v %v", w, err)
	}
	return w.TurnID
}

type ledgerRow struct {
	Type    string
	RefType string
	RefID   string
	Change  int
}

// consumes 列出付费主体的全部非发放流水（按时间序）。
func (e *advisorEnv) consumes(t *testing.T) []ledgerRow {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `select type, coalesce(ref_type,''), coalesce(ref_id,''), change
		from credit_transaction where payer_account_id = $1 and type <> 'grant' order by created_at, id`, payer)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []ledgerRow
	for rows.Next() {
		var r ledgerRow
		if err := rows.Scan(&r.Type, &r.RefType, &r.RefID, &r.Change); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// assertAdvisorPlanning：规划态请求里没有付费 / 画布写工具（AG-05），且只在白名单之内。
func assertAdvisorPlanning(t *testing.T, chat *flowChat) {
	t.Helper()
	assertPlanningOnly(t, chat)
	wl := toolSet(advisorTools)
	for i, names := range chat.tools {
		for _, n := range names {
			if !wl[n] {
				t.Fatalf("第 %d 次请求出现白名单外的工具 %s：%v", i+1, n, names)
			}
		}
	}
}

func TestAdvisor_三轮纯对话只按轮扣费(t *testing.T) {
	e := newAdvisorEnv(t)
	ctx := context.Background()
	s, t1 := e.seedAdvisor(t, "我卖手工皂，月销 200 单，想提升销量，帮我分析下人群和内容方向。")

	// 第 1 轮：问诊（纯文本，不发卡 —— 发卡会挂起会话，那是另一条已覆盖的路径）。
	chat := e.run(t, s.ID, t1, agentapp.ChatOutput{Text: "先问几个问题：价格带、主要渠道、目前卡在哪一步？"})
	assertAdvisorPlanning(t, chat)
	sys := chat.system[0]
	// MKT-F-83：场景段（代码兜底）与技能正文里的「不编造」写法真的进了系统提示词；无商品时营销输入如实说「无」。
	for _, must := range []string{domain.SceneHeading(domain.SceneMarketing), "不编造数据", "需你核实",
		"不承诺 ROI 或销量", "## 已加载技能：生意增长顾问", "商品：未填写", "商品图：无"} {
		if !strings.Contains(sys, must) {
			t.Fatalf("系统提示词缺少「%s」", must)
		}
	}

	// 第 2 轮：分析。
	t2 := e.post(t, s.ID, "adv-msg-0002", "客单价 39，主要在抖音，播放量还行但转化低。")
	assertAdvisorPlanning(t, e.run(t, s.ID, t2,
		agentapp.ChatOutput{Text: "转化低多半是承接段与卖点的问题，建议先测开头之后的 3~8 秒。"}))

	// 第 3 轮：把建议写进项目文档（文档工具免费）再总结。
	t3 := e.post(t, s.ID, "adv-msg-0003", "好，帮我整理成文档。")
	assertAdvisorPlanning(t, e.run(t, s.ID, t3,
		call("c1", "write_doc", `{"path":"/notes/增长建议.md","content":"# 增长建议\n\n- 人群切片：…\n- 测试假设：…（行业数据需你核实）"}`),
		agentapp.ChatOutput{Text: "已写进 增长建议.md。"}))

	// MKT-AC-30：账本只有 3 条按轮扣费，单价 = agent_chat 报价；没有别的收费流水、没有生成任务。
	rows := e.consumes(t)
	if len(rows) != 3 {
		t.Fatalf("应恰好 3 条流水：%+v", rows)
	}
	turns := map[string]bool{}
	for _, r := range rows {
		if r.Type != "consume" || r.RefType != agentapp.RefTypeTurn || r.Change != -advisorTurnPrice {
			t.Fatalf("流水应全是按轮扣费（agent_turn，-%d）：%+v", advisorTurnPrice, r)
		}
		turns[r.RefID] = true
	}
	for _, id := range []int64{t1, t2, t3} {
		if !turns[strconv.FormatInt(id, 10)] {
			t.Fatalf("轮次 %d 没有对应的扣费流水：%+v", id, rows)
		}
	}
	if n := e.count(t, `select count(*)::int from generation`); n != 0 {
		t.Fatalf("纯对话不应产生生成任务：%d", n)
	}
	if n := e.count(t, `select count(*)::int from agent_card where session_id = $1`, s.ID); n != 0 {
		t.Fatalf("纯对话不应发确认卡：%d", n)
	}
	var cost int
	if err := e.pool.QueryRow(ctx, `select coalesce(sum(credits_cost),0)::int from agent_turn where session_id = $1`,
		s.ID).Scan(&cost); err != nil || cost != 3*advisorTurnPrice {
		t.Fatalf("轮次记的费用应与账本一致：%d %v", cost, err)
	}
	if n := e.count(t, `select count(*)::int from project_document where project_id = $1 and path = '/notes/增长建议.md'`,
		project); n != 1 {
		t.Fatalf("增长建议文档应已落库：%d", n)
	}
}

func TestAdvisor_出图先确认后扣费且分行(t *testing.T) {
	e := newAdvisorEnv(t)
	ctx := context.Background()
	s, t1 := e.seedAdvisor(t, "帮我想想手工皂的内容方向。")
	assertAdvisorPlanning(t, e.run(t, s.ID, t1, agentapp.ChatOutput{Text: "可以从「成分安心」切入。"}))

	// 第 2 轮：用户要一张示意图 → 规划（免费）→ 确认卡带图片报价，会话挂起。
	t2 := e.post(t, s.ID, "adv-img-0002", "出一张示意图看看效果。")
	chat := e.run(t, s.ID, t2,
		call("c1", "submit_plan", `{"title":"示意图","items":[{"type":"image","key":"sketch",`+
			`"modelCode":"dev_image","count":1,"title":"成分安心示意图"}]}`))
	assertAdvisorPlanning(t, chat)
	cards, err := e.r.PendingBySession(ctx, s.ID)
	if err != nil || len(cards) != 1 || cards[0].Kind != domain.CardPlanConfirm {
		t.Fatalf("应恰好 1 张待确认的规划卡：%+v %v", cards, err)
	}
	var p domain.PlanConfirmPayload
	if err := json.Unmarshal(cards[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Items) != 1 || p.Items[0].Kind != "image" || p.Items[0].Key != "sketch" ||
		p.Items[0].Credits != paidPrice || p.Total != paidPrice || !p.CanApprove {
		t.Fatalf("确认卡应带这张图的报价：%+v", p)
	}
	// 确认前：只有两条按轮扣费，没有任何生成扣费、没有生成任务。
	before := e.consumes(t)
	for _, r := range before {
		if r.RefType != agentapp.RefTypeTurn {
			t.Fatalf("确认前不应有按轮以外的扣费：%+v", before)
		}
	}
	if len(before) != 2 || e.count(t, `select count(*)::int from generation`) != 0 {
		t.Fatalf("确认前应只有 2 条按轮扣费、0 个生成任务：%+v", before)
	}

	// 用户确认 → 执行轮按计划项生成。
	res, err := e.svc.SubmitCard(ctx, e.actor, s.ID, cards[0].ID, json.RawMessage(`{"decision":"approve"}`))
	if err != nil || res.TurnID == 0 {
		t.Fatalf("确认失败：%+v %v", res, err)
	}
	e.run(t, s.ID, res.TurnID,
		call("c2", "generate", `{"planItemKey":"sketch","prompt":"手工皂摆在木桌上，旁边是橄榄与薰衣草","wait":false}`),
		agentapp.ChatOutput{Text: "示意图已提交生成。"})

	gens := e.generations(t)
	if len(gens) != 1 || gens[0].Credits != paidPrice || gens[0].Payer != payer {
		t.Fatalf("应受理 1 张图、按图片单价记费：%+v", gens)
	}
	// MKT-AC-31：确认后按图片计价单位扣费，与按轮扣费分别成行。
	rows := e.consumes(t)
	var turnRows, genRows []ledgerRow
	for _, r := range rows {
		switch {
		case r.Type == "consume" && r.RefType == agentapp.RefTypeTurn && r.Change == -advisorTurnPrice:
			turnRows = append(turnRows, r)
		case r.Type == "consume" && r.RefType == "generation" && r.Change == -paidPrice:
			genRows = append(genRows, r)
		default:
			t.Fatalf("出现意料之外的流水：%+v", r)
		}
	}
	// 按轮：问答轮、规划轮、确认后的执行轮（批准建的是用户轮，照常按轮计费 —— 现有 Agent 规则，顾问不另设）。
	userTurns := e.count(t, `select count(*)::int from agent_turn where session_id = $1 and trigger = 'user'`, s.ID)
	if userTurns != 3 || len(turnRows) != userTurns || len(genRows) != 1 ||
		genRows[0].RefID != strconv.FormatInt(gens[0].ID, 10) {
		t.Fatalf("应 %d 条按轮扣费（每个用户轮一条）+ 1 条生成扣费且各自成行：%+v", userTurns, rows)
	}
	// 计价单位冻结在受理快照里（per_image × 张数），与按轮的 agent_chat 是两个计价口径。
	var unit string
	if err := e.pool.QueryRow(ctx, `select params_snapshot->'_snapshot'->>'priceUnit' from generation where id = $1`,
		gens[0].ID).Scan(&unit); err != nil || unit != "per_image" {
		t.Errorf("生成任务的计价单位应为 per_image：%q %v", unit, err)
	}
}
