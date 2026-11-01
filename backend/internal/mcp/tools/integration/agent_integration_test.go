//go:build integration

// 对外 MCP 委托站内 Agent（MCPT-17）的真库测试：真实的 agent 服务（wiring.NewAgentService：建会话、发消息、
// 提交卡片、规划确认的站内重新报价、余额）+ 真实的确认服务（mcp_quote）+ 真实的目录报价与账本。
//
// ⚠️ 不跑 Worker、不触发任何上游：站内 Agent 发卡由测试按 Worker 的写法落库（轮 → 步骤 → 卡片 → 挂起），
// 推理模型只配了价、从不调用。
// ⚠️ 「不带确认批准不了」「同一确认只批准一次」「价格变了不按新价批准」都要真库与真 agent 服务才证明得了：
// 假端口的测试把站内 SubmitCard 的重新报价整条删掉照样绿。
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
	agentdomain "github.com/li904338203/tabtv/backend/internal/agent/domain"
	agentrepo "github.com/li904338203/tabtv/backend/internal/agent/repo"
	billingapp "github.com/li904338203/tabtv/backend/internal/billing/app"
	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	collabapp "github.com/li904338203/tabtv/backend/internal/collaboration/app"
	collabrepo "github.com/li904338203/tabtv/backend/internal/collaboration/repo"
	contentapp "github.com/li904338203/tabtv/backend/internal/content/app"
	contentrepo "github.com/li904338203/tabtv/backend/internal/content/repo"
	identityapp "github.com/li904338203/tabtv/backend/internal/identity/app"
	identityrepo "github.com/li904338203/tabtv/backend/internal/identity/repo"
	mcpapp "github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	mcprepo "github.com/li904338203/tabtv/backend/internal/mcp/repo"
	"github.com/li904338203/tabtv/backend/internal/mcp/tools"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/platform/bootstrap"
	"github.com/li904338203/tabtv/backend/internal/platform/config"
	"github.com/li904338203/tabtv/backend/internal/platform/db"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

type agentEnv struct {
	*env
	agentRepo *agentrepo.Repo
	agentSvc  *agentapp.Service
	in        wiring.MCPReadInput
}

// newAgentEnv：真实装配 + 主体带全部 scope 与全部工具集（agent 工具集默认关闭）。
func newAgentEnv(t *testing.T) *agentEnv {
	t.Helper()
	e := newEnv(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	core, err := wiring.BuildCore(wiring.CoreInput{Cfg: config.Config{},
		Deps: &bootstrap.Deps{DB: &db.Pool{Pool: e.pool}}, NextID: e.next, Logger: logger,
		Audit: audit.New(e.pool, e.next, logger), Media: e.media, Catalog: e.catalog})
	if err != nil {
		t.Fatal(err)
	}
	billing := billingapp.New(billingrepo.New(e.pool, e.next))
	ar := agentrepo.New(e.pool)
	// 与 cmd/api 同一个构造函数；Chat 只为装上「一键优化」那组端口（报价 / 默认推理模型），本测试从不调用模型。
	agentSvc := wiring.NewAgentService(wiring.AgentAPIInput{Repo: ar, NextID: e.next, Canvas: core.Canvas,
		Media: e.media, Script: core.Script, Catalog: e.catalog, Billing: billing, Gen: core.Gen, Logger: logger,
		Chat: core.TextProvider, Ledger: core.Billing})
	in := wiring.MCPReadInput{
		Identity: identityapp.New(identityrepo.New(e.pool), nil, nil, nil, e.next),
		Collab:   collabapp.New(collabrepo.New(e.pool), e.next), Billing: billing, Canvas: core.Canvas,
		GenRepo: core.GenRepo, Gen: core.Gen, Catalog: e.catalog, Content: contentapp.New(contentrepo.New(e.pool, e.next)),
		Workflow: core.Workflow, Media: e.media, MediaURLTTL: time.Hour,
		Store: mcprepo.New(e.pool), NextID: e.next, QuoteTTL: 10 * time.Minute,
		Ledger: core.Billing, StateKey: mcpapp.DeriveStateKey("integration"), ApprovalBase: "https://app.example.com",
		Agent: agentSvc, AgentEnabled: true,
	}
	deps := wiring.NewMCPToolDeps(in)
	if deps.Agent == nil || deps.AgentDisabled {
		t.Fatal("agent 端口没有接上")
	}
	e.reg = tools.NewRegistry(deps)
	k, err := e.keys.CreateKey(context.Background(), e.owner, mcpapp.CreateKeyInput{WorkspaceID: e.ws,
		ClientName: "Claude Code", Scopes: domain.Scopes(), Toolsets: domain.Toolsets()})
	if err != nil {
		t.Fatal(err)
	}
	if e.p, err = e.resolver.Resolve(context.Background(), k.Plaintext); err != nil {
		t.Fatal(err)
	}
	ae := &agentEnv{env: e, agentRepo: ar, agentSvc: agentSvc, in: in}
	// 图片模型（规划计划项）7 积分 / 张；推理模型 agent_chat 3 积分 / 轮。
	e.seedPriced("img-m", "text2img", "image", "per_image", 7, "text")
	ae.seedReasoning("chat-m", 3)
	e.credit(100)
	return ae
}

// seedReasoning 配一个推理模型：绑 agent_chat 并定价（与 Agent 每轮报价同一条路）。
func (e *agentEnv) seedReasoning(model string, credits int) {
	e.t.Helper()
	exec(e.t, e.pool, `insert into api_protocol (code, name, is_async) values ('openai_chat','OpenAI Chat',false)
		on conflict (code) do nothing`)
	pid := e.next()
	exec(e.t, e.pool, `insert into provider_config (id, code, name, adapter_type, base_url, auth_type, status)
		values ($1,$2,$2,'http','http://127.0.0.1:1/v1','none','enabled')`, pid, "p-"+model)
	mid := e.next()
	exec(e.t, e.pool, `insert into model (id, code, name, provider_id, domain, upstream_model_code, api_protocol,
		submit_path, est_seconds, status, sort, is_agent_capable)
		values ($1,$2,$2,$3,'text','up','openai_chat','/chat',20,'enabled',0,true)`, mid, model, pid)
	exec(e.t, e.pool, `insert into operation (id, code, name, domain, in_node_types, out_node_type, status)
		values ($1,'agent_chat','Agent 对话','text','text','text','enabled') on conflict (code) do nothing`, e.next())
	exec(e.t, e.pool, `insert into model_operation (id, model_id, operation_id, is_enabled)
		select $1, $2, o.id, true from operation o where o.code = 'agent_chat'`, e.next(), mid)
	exec(e.t, e.pool, `insert into pricing (id, operation_id, model_id, credits, unit, status)
		select $1, o.id, $2, $3, 'per_time', 'enabled' from operation o where o.code = 'agent_chat'`, e.next(), mid, credits)
	// 推理操作沿用同一模型在画布里的输入能力（agent_chat ← text2text，catalog/domain）。
	exec(e.t, e.pool, `insert into operation (id, code, name, domain, in_node_types, out_node_type, status)
		values ($1,'text2text','文生文','text','text','text','enabled') on conflict (code) do nothing`, e.next())
	exec(e.t, e.pool, `insert into model_input_capability (id, model_id, operation_id, input_role, min_count, max_count)
		select $1, $2, o.id, 'text', 0, 4 from operation o where o.code = 'text2text'`, e.next(), mid)
}

func (e *agentEnv) need(args string) map[string]any {
	e.t.Helper()
	m, _, err := e.call(e.p, "agent", args)
	if err != nil {
		e.t.Fatalf("agent %s：%v", args, err)
	}
	return m
}

func (e *agentEnv) callErr(p mcpapp.Principal, args string) error {
	e.t.Helper()
	_, _, err := e.call(p, "agent", args)
	return err
}

// start 经 MCP 开会话（真实 CreateSession），返回会话 ID。
func (e *agentEnv) start(pid int64, key string) int64 {
	e.t.Helper()
	m := e.need(`{"method":"start","projectId":"` + itoa(pid) + `","text":"做一张三打白骨精的封面","idempotencyKey":"` + key + `"}`)
	sid, _ := strconv.ParseInt(m["sessionId"].(string), 10, 64)
	return sid
}

// planCard 按 Worker 的写法发一张规划确认卡：逐项真实报价（AgentPlanQuoter，与站内 submit_plan 同一条路），
// 再把会话挂起在 awaiting_approval。返回卡片 ID 与合计。
func (e *agentEnv) planCard(sid int64) (int64, int) {
	e.t.Helper()
	ctx := context.Background()
	items, err := wiring.AgentPlanQuoter{Catalog: e.catalog}.QuotePlan(ctx, []agentdomain.PlanItem{
		{Kind: "image", ModelCode: "img-m", OperationCode: "text2img", Label: "封面", Quantity: 2, Unit: "image"}}, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	total := 0
	for _, it := range items {
		total += it.Credits
	}
	return e.suspend(sid, agentdomain.CardPlanConfirm, agentdomain.PlanConfirmPayload{Title: "封面", Items: items,
		Total: total, Balance: 100, CanApprove: true}), total
}

func (e *agentEnv) suspend(sid int64, kind agentdomain.CardKind, payload any) int64 {
	e.t.Helper()
	ctx := context.Background()
	sess, err := e.agentRepo.GetByID(ctx, sid)
	if err != nil {
		e.t.Fatal(err)
	}
	turn, err := e.agentRepo.InsertTurn(ctx, e.pool, agentrepo.Turn{ID: e.next(), SessionID: sid,
		Trigger: agentdomain.TriggerSystem})
	if err != nil {
		e.t.Fatal(err)
	}
	step, err := e.agentRepo.InsertStep(ctx, e.pool, agentrepo.Step{ID: e.next(), SessionID: sid, TurnID: turn.ID,
		Tool: "submit_plan"})
	if err != nil {
		e.t.Fatal(err)
	}
	raw, _ := json.Marshal(payload)
	card, err := e.agentRepo.InsertCard(ctx, e.pool, agentrepo.Card{ID: e.next(), SessionID: sid, StepID: step.ID,
		Kind: kind, Payload: raw})
	if err != nil {
		e.t.Fatal(err)
	}
	to, _ := agentdomain.SuspendStateFor(kind)
	if ok, err := e.agentRepo.TransitionState(ctx, e.pool, sid, sess.State, to); err != nil || !ok {
		e.t.Fatalf("挂起失败：%v %v", ok, err)
	}
	return card.ID
}

func (e *agentEnv) cardStatus(sid, cid int64) agentdomain.CardStatus {
	e.t.Helper()
	c, err := e.agentRepo.GetCard(context.Background(), sid, cid)
	if err != nil {
		e.t.Fatal(err)
	}
	return c.Status
}

func (e *agentEnv) approved(sid int64) int {
	return e.count(`select approved_budget from agent_session where id=$1`, sid)
}

func approveArgs(sid, cid int64) string {
	return `{"method":"submit_card","sessionId":"` + itoa(sid) + `","cardId":"` + itoa(cid) + `","decision":"approve"}`
}

// start：会话归凭据用户本人、带会话链接与这一轮的对话费（agent_chat 的站内报价）；同一幂等键重试不多建。
func TestMcpAgent_StartOwnedByUser_TurnFee(t *testing.T) {
	e := newAgentEnv(t)
	pid := e.project(e.ws, "片子", genCanvas, time.Now())
	key := `{"method":"start","projectId":"` + itoa(pid) + `","text":"做一张封面","skillCode":"","idempotencyKey":"mcp-s1"}`
	m := e.need(key)
	sid := mustInt(t, m["sessionId"])
	fee := m["turnFee"].(map[string]any)
	if fee["known"] != true || fee["credits"] != float64(3) || fee["modelCode"] != "chat-m" {
		t.Fatalf("轮次费取站内报价：%v", m)
	}
	if m["chatUrl"] != "https://app.example.com/video/chat/"+itoa(pid)+"?session="+itoa(sid) {
		t.Fatalf("%v", m["chatUrl"])
	}
	if n := e.count(`select count(*) from agent_session where id=$1 and created_by_user_id=$2 and workspace_id=$3
		and payer_account_id=$4`, sid, e.owner, e.ws, e.payer); n != 1 {
		t.Fatal("会话应归凭据用户本人、记在团队付费主体上")
	}
	if m2 := e.need(key); m2["replayed"] != true || m2["sessionId"] != m["sessionId"] || m2["turnFee"] != nil {
		t.Fatalf("同一幂等键重放：%v", m2)
	}
	if n := e.count(`select count(*) from agent_turn where session_id=$1`, sid); n != 1 {
		t.Fatalf("只建一轮：%d", n)
	}
	// API 侧只写库与 outbox，不扣费（这一轮由 Worker 跑时才扣 agent_chat）。
	if n := e.count(`select count(*) from credit_transaction`); n != 0 {
		t.Fatalf("受理不扣费：%d", n)
	}
	// get：会话快照、最近消息（不可信）、预算。
	g := e.need(`{"method":"get","sessionId":"` + itoa(sid) + `"}`)
	s := g["session"].(map[string]any)
	msgs := s["messages"].([]any)
	if s["createdByUserId"] != itoa(e.owner) || len(msgs) != 1 ||
		msgs[0].(map[string]any)["untrusted"].(map[string]any)["content"] != "做一张封面" {
		t.Fatalf("%v", g)
	}
}

// fakeChat 是进程内推理模型：只回一句话、不调工具（不发网络）。
type fakeChat struct{}

func (fakeChat) Chat(context.Context, agentapp.ChatInput, func(string)) (agentapp.ChatOutput, error) {
	return agentapp.ChatOutput{Text: "好的，我先看看画布再规划。", FinishReason: "stop"}, nil
}

// 账本核对：经 MCP start 的这一轮由站内运行器（与 Worker 同一个 Runner、真实报价与账本）扣对话费，
// 金额 == start 结果里 turnFee 报的数；MCP 自己不另收（M-4）。消息重投再跑一次不重复扣。
func TestMcpAgent_TurnCharged_EqualsTurnFee(t *testing.T) {
	e := newAgentEnv(t)
	pid := e.project(e.ws, "片子", genCanvas, time.Now())
	m := e.need(`{"method":"start","projectId":"` + itoa(pid) + `","text":"做一张封面","idempotencyKey":"mcp-r1"}`)
	sid, turnID := mustInt(t, m["sessionId"]), mustInt(t, m["turnId"])
	fee := int(m["turnFee"].(map[string]any)["credits"].(float64))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runner := agentapp.NewRunner(agentapp.RunnerDeps{
		Store: agentapp.NewRepoStore(e.agentRepo), Chat: fakeChat{},
		Quoter: wiring.AgentQuoter{Catalog: e.catalog}, Ledger: wiring.AgentLedger{Repo: billingrepo.New(e.pool, e.next)},
		Members: wiring.AgentMembers{Repo: collabrepo.New(e.pool)}, Reasoning: wiring.AgentVideoModels{Catalog: e.catalog},
		NextID: e.next, Logger: logger,
	}, agentapp.RunnerConfig{MaxCalls: 3, ContextChars: 60000})
	for i := 0; i < 2; i++ {
		if _, err := runner.RunTurn(context.Background(), sid, turnID); err != nil {
			t.Fatalf("第 %d 次跑这一轮：%v", i+1, err)
		}
	}
	var n, spent int
	if err := e.pool.QueryRow(context.Background(), `select count(*), coalesce(sum(-change),0) from credit_transaction
		where payer_account_id=$1 and type='consume' and ref_type='agent_turn' and ref_id=$2`, e.payer, itoa(turnID)).
		Scan(&n, &spent); err != nil {
		t.Fatal(err)
	}
	if n != 1 || spent != fee || fee != 3 {
		t.Fatalf("这一轮应扣一次对话费 %d：%d 笔共 %d", fee, n, spent)
	}
	if c := e.count(`select count(*) from credit_transaction where payer_account_id=$1`, e.payer); c != 1 {
		t.Fatalf("账本里只有这一轮的对话费，没有 MCP 自己的收费：%d 笔", c)
	}
}

func TestMcpAgent_ApproveWithoutConfirm_NoCharge(t *testing.T) {
	e := newAgentEnv(t)
	pid := e.project(e.ws, "片子", genCanvas, time.Now())
	sid := e.start(pid, "mcp-a1")
	cid, total := e.planCard(sid)
	m := e.need(approveArgs(sid, cid))
	nc, _ := m["needsConfirmation"].(map[string]any)
	if m["status"] != "needs_confirmation" || nc == nil || int(nc["credits"].(float64)) != total || total != 14 {
		t.Fatalf("不带确认只签发报价：%v", m)
	}
	card := m["card"].(map[string]any)
	if card["needsUserConfirmation"] != true || card["plan"].(map[string]any)["total"] != float64(14) {
		t.Fatalf("结果带结构化卡片：%v", card)
	}
	if e.cardStatus(sid, cid) != agentdomain.CardPending || e.approved(sid) != 0 {
		t.Fatal("未确认不得批准")
	}
	if n := e.count(`select count(*) from credit_transaction`); n != 0 {
		t.Fatalf("未确认不扣费：%d", n)
	}
	if n := e.count(`select count(*) from agent_turn where session_id=$1 and idempotency_key like 'card:%'`, sid); n != 0 {
		t.Fatal("未确认不得建批准那一轮")
	}
	// 金额不符 / 不认识的 quote：无效，不批准。
	bad := strings.TrimSuffix(approveArgs(sid, cid), "}") + `,"confirmQuoteId":"` + nc["quoteId"].(string) + `","confirmCredits":1}`
	if err := e.callErr(e.p, bad); bizCode(err) != errcode.McpQuoteInvalid || e.cardStatus(sid, cid) != agentdomain.CardPending {
		t.Fatalf("金额不符应无效：%v", err)
	}
}

func TestMcpAgent_ApproveBaseline_ChargedOnce(t *testing.T) {
	e := newAgentEnv(t)
	pid := e.project(e.ws, "片子", genCanvas, time.Now())
	sid := e.start(pid, "mcp-a2")
	cid, total := e.planCard(sid)
	m := e.need(approveArgs(sid, cid))
	ca := strings.TrimSuffix(approveArgs(sid, cid), "}") + confirmArgs(m["needsConfirmation"].(map[string]any)) + "}"
	for i := 0; i < 3; i++ {
		r := e.need(ca)
		if (r["replayed"] == true) != (i > 0) || r["credits"] != float64(total) {
			t.Fatalf("第 %d 次：%v", i+1, r)
		}
		if i == 0 {
			fee, _ := r["turnFee"].(map[string]any)
			if fee == nil || fee["credits"] != float64(3) || r["status"] != "executing" {
				t.Fatalf("批准后站内 Agent 跑一轮（对话费 3）：%v", r)
			}
		}
	}
	if e.cardStatus(sid, cid) != agentdomain.CardSubmitted || e.approved(sid) != total {
		t.Fatalf("批准一次：额度应 = 合计 %d，得到 %d", total, e.approved(sid))
	}
	if n := e.count(`select count(*) from agent_turn where session_id=$1 and idempotency_key = $2`, sid,
		"card:"+itoa(cid)); n != 1 {
		t.Fatalf("批准那一轮只建一次：%d", n)
	}
	if n := e.count(`select count(*) from mcp_quote where tool='agent.submit_card' and used_at is not null`); n != 1 {
		t.Fatalf("一张确认、用一次：%d", n)
	}
	// 批准是授权不是扣费：账本此刻一条流水都没有（生成费由站内 Agent 按计划逐项受理时扣）。
	if n := e.count(`select count(*) from credit_transaction`); n != 0 {
		t.Fatalf("批准不扣费：%d", n)
	}
}

func TestMcpAgent_ApprovePageMode_EchoIgnored(t *testing.T) {
	e := newAgentEnv(t)
	if _, err := e.keys.SetRequirePageConfirm(context.Background(), e.owner, e.p.GrantID, true); err != nil {
		t.Fatal(err)
	}
	e.p.RequirePageConfirm = true
	pid := e.project(e.ws, "片子", genCanvas, time.Now())
	sid := e.start(pid, "mcp-a3")
	cid, _ := e.planCard(sid)
	m := e.need(approveArgs(sid, cid))
	nc := m["needsConfirmation"].(map[string]any)
	if !strings.HasSuffix(nc["approvalUrl"].(string), "/mcp/approve/"+nc["quoteId"].(string)) {
		t.Fatalf("应给审批链接：%v", nc)
	}
	ca := strings.TrimSuffix(approveArgs(sid, cid), "}") + confirmArgs(nc) + "}"
	for i := 0; i < 2; i++ {
		if err := e.callErr(e.p, ca); bizCode(err) != errcode.McpApprovalPending {
			t.Fatalf("回显金额不算同意：%v", err)
		}
	}
	if e.cardStatus(sid, cid) != agentdomain.CardPending || e.approved(sid) != 0 {
		t.Fatal("未在页面批准不得批准")
	}
	ap := mcpapp.NewApprovals(mcprepo.New(e.pool),
		wiring.MCPApprovalWorkspaces{Svc: collabapp.New(collabrepo.New(e.pool), e.next)},
		wiring.MCPPayerBalances{Svc: billingapp.New(billingrepo.New(e.pool, e.next))},
		audit.New(e.pool, e.next, slog.New(slog.NewTextHandler(io.Discard, nil))))
	if _, err := ap.Approve(context.Background(), e.owner, quoteIDOf(t, nc)); err != nil {
		t.Fatal(err)
	}
	if r := e.need(ca); r["credits"] != float64(14) || e.cardStatus(sid, cid) != agentdomain.CardSubmitted {
		t.Fatalf("页面批准后才生效：%v", r)
	}
}

func TestMcpAgent_PriceChanged_NewQuote(t *testing.T) {
	e := newAgentEnv(t)
	pid := e.project(e.ws, "片子", genCanvas, time.Now())
	sid := e.start(pid, "mcp-a4")
	cid, _ := e.planCard(sid)
	m := e.need(approveArgs(sid, cid))
	ca := strings.TrimSuffix(approveArgs(sid, cid), "}") + confirmArgs(m["needsConfirmation"].(map[string]any)) + "}"
	// 发卡之后运营改了价：7 → 9（站内批准时重新报价合计 18）。
	exec(t, e.pool, `update pricing set credits = 9 where model_id = (select id from model where code='img-m')`)
	err := e.callErr(e.p, ca)
	be := bizOf(err)
	if be == nil || be.Code != errcode.McpQuotePriceChanged {
		t.Fatalf("应回价格变化：%v", err)
	}
	q, _ := be.Details["quote"].(map[string]any)
	if q == nil || q["credits"] != 18 || be.Details["oldCredits"] != 14 {
		t.Fatalf("新合计 18：%v", be.Details)
	}
	if e.cardStatus(sid, cid) != agentdomain.CardPending || e.approved(sid) != 0 {
		t.Fatal("价格变了不得按新价批准")
	}
	var cardTotal int
	if err := e.pool.QueryRow(context.Background(), `select (payload->>'total')::int from agent_card where id=$1`, cid).
		Scan(&cardTotal); err != nil || cardTotal != 18 {
		t.Fatalf("站内把新报价写回了卡片：%d %v", cardTotal, err)
	}
	// 旧确认再交：不再提交（重放说清之前失败了）。
	if err := e.callErr(e.p, ca); bizCode(err) != errcode.Conflict || e.approved(sid) != 0 {
		t.Fatalf("旧确认不能再用：%v", err)
	}
	// 新报价确认：按 18 批准一次。
	na := strings.TrimSuffix(approveArgs(sid, cid), "}") + `,"confirmQuoteId":"` + q["quoteId"].(string) + `","confirmCredits":18}`
	if r := e.need(na); r["credits"] != float64(18) || e.approved(sid) != 18 {
		t.Fatalf("新报价确认后批准：%v 额度 %d", r, e.approved(sid))
	}
}

func TestMcpAgent_NotInitiator_Rejected(t *testing.T) {
	e := newAgentEnv(t)
	pid := e.project(e.ws, "片子", genCanvas, time.Now())
	sid := e.start(pid, "mcp-a5")
	cid, _ := e.planCard(sid)
	mate := mkUser(t, e.pool, e.next, "同事", "13900000041")
	exec(t, e.pool, `insert into workspace_member (id, workspace_id, user_id, role) values ($1,$2,$3,'editor')`,
		e.next(), e.ws, mate)
	k, err := e.keys.CreateKey(context.Background(), mate, mcpapp.CreateKeyInput{WorkspaceID: e.ws,
		ClientName: "Cursor", Scopes: domain.Scopes(), Toolsets: domain.Toolsets()})
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.resolver.Resolve(context.Background(), k.Plaintext)
	if err != nil {
		t.Fatal(err)
	}
	// 同工作区成员看得到会话（站内规则），但不能回答卡片、不能继续对话。
	if _, _, err := e.call(p, "agent", `{"method":"get","sessionId":"`+itoa(sid)+`"}`); err != nil {
		t.Fatalf("成员可以看：%v", err)
	}
	if err := e.callErr(p, approveArgs(sid, cid)); bizCode(err) != errcode.AgentNotInitiator {
		t.Fatalf("非发起人批准：%v", err)
	}
	if err := e.callErr(p, `{"method":"send","sessionId":"`+itoa(sid)+`","text":"我也说","idempotencyKey":"m1"}`); bizCode(err) !=
		errcode.AgentNotInitiator {
		t.Fatalf("非发起人发消息：%v", err)
	}
	if n := e.count(`select count(*) from mcp_quote`); n != 0 || e.cardStatus(sid, cid) != agentdomain.CardPending {
		t.Fatalf("非发起人不签发报价、不提交：quote %d", n)
	}
}

func TestMcpAgent_GetWait_ReturnsOnCard(t *testing.T) {
	e := newAgentEnv(t)
	pid := e.project(e.ws, "片子", genCanvas, time.Now())
	sid := e.start(pid, "mcp-a6")
	done := make(chan int64, 1)
	go func() {
		time.Sleep(1500 * time.Millisecond)
		cid, _ := e.planCard(sid)
		done <- cid
	}()
	begin := time.Now()
	m := e.need(`{"method":"get","sessionId":"` + itoa(sid) + `","waitSeconds":20}`)
	cid := <-done
	cards := m["session"].(map[string]any)["pendingCards"].([]any)
	if m["status"] != "awaiting_approval" || len(cards) != 1 || cards[0].(map[string]any)["cardId"] != itoa(cid) {
		t.Fatalf("应等到卡片出现：%v", m)
	}
	if el := time.Since(begin); el > 10*time.Second || el < time.Second {
		t.Fatalf("出现卡片即返回：等了 %v", el)
	}
	// 选择卡：不花钱，直接提交（会让站内 Agent 再跑一轮，结果带对话费）。
	sid2 := e.start(pid, "mcp-a7")
	cid2 := e.suspend(sid2, agentdomain.CardChoice, agentdomain.ChoicePayload{Title: "风格",
		Options: []agentdomain.Option{{ID: "a", Label: "水墨"}}, ResumeState: agentdomain.StatePlanning})
	r := e.need(`{"method":"submit_card","sessionId":"` + itoa(sid2) + `","cardId":"` + itoa(cid2) + `","selected":["a"]}`)
	if e.cardStatus(sid2, cid2) != agentdomain.CardSubmitted || r["turnFee"] == nil || r["status"] != "planning" {
		t.Fatalf("选择卡直接提交：%v", r)
	}
	// 删除确认卡：不经 MCP 提交，只给链接。
	cid3 := e.suspend(sid2, agentdomain.CardDeleteConfirm, agentdomain.DeleteConfirmPayload{Reason: "重复",
		NodeIDs: []string{"n_user1"}, ResumeState: agentdomain.StatePlanning})
	r = e.need(`{"method":"submit_card","sessionId":"` + itoa(sid2) + `","cardId":"` + itoa(cid3) + `","decision":"approve"}`)
	if r["status"] != "open_in_seeaitv" || e.cardStatus(sid2, cid3) != agentdomain.CardPending {
		t.Fatalf("删除确认只给链接：%v", r)
	}
}

func TestMcpAgent_DisabledHidesToolset(t *testing.T) {
	e := newAgentEnv(t)
	in := e.in
	in.AgentEnabled = false
	d := wiring.NewMCPToolDeps(in)
	if !d.AgentDisabled || d.Agent != nil {
		t.Fatal("站内 Agent 关闭时不接端口")
	}
	if _, ok := tools.NewRegistry(d).Lookup("agent"); ok {
		t.Fatal("站内 Agent 关闭时 agent 工具不可见")
	}
	for _, ts := range mcpapp.ToolsetAvailability(tools.NewRegistry(d).List()) {
		if ts.Name == domain.ToolsetAgent && ts.Available {
			t.Fatal("配置片段里 agent 工具集应不可用")
		}
	}
}
