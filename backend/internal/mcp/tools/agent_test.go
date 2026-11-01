package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 委托站内 Agent（MCPT-17）的单测：假会话端口 + 真确认服务（假 quote 表）+ 与 transport 相同的四步。
// 发起人校验、状态机、站内重新报价这些由站内 Agent 判的规则在集成测试里用真库与真 agent 服务验
// （tools/integration/agent_integration_test.go）。

// ---------------------------------------------------------------- 假端口

type fakeSubmit struct {
	cardID int64
	body   string
}

// fakeAgent：会话 21（项目 11，发起人 1），卡片按 kind 放进 pending；priceTo>0 时批准规划回 409 price_changed
// 并把卡片合计写成 priceTo（同站内 priceChanged）。
type fakeAgent struct {
	mu        sync.Mutex
	sess      app.AgentSession
	submits   []fakeSubmit
	starts    []app.AgentStartInput
	sends     []app.AgentSendInput
	priceTo   int
	estErr    error
	states    []string // get 依次返回的状态（等待用）
	gets      int
	cardAfter int // 第几次 get 起出现 pending 卡片（0 = 一直按 sess）
	later     []app.AgentCard
}

func newFakeAgent() *fakeAgent {
	return &fakeAgent{sess: app.AgentSession{ID: 21, ProjectID: 11, Title: "三打白骨精", State: "awaiting_approval",
		CreatedByUserID: 1, InitiatorName: "负责人", Approved: 100, Used: 60, Refunded: 5,
		Messages: []app.AgentMessage{{ID: 1, Role: "user", Content: "做一个三打白骨精"},
			{ID: 2, Role: "assistant", Content: "好的，先规划"}}}}
}

func planCard(id int64, total int, blocked bool) app.AgentCard {
	item := map[string]any{"kind": "video", "modelCode": "kling", "operationCode": "text2video", "label": "镜头 1",
		"quantity": 5, "unit": "second", "credits": total}
	if blocked {
		item["blockedCode"], item["blockedReason"] = "42204", "需要会员"
	}
	b, _ := json.Marshal(map[string]any{"title": "三打白骨精", "items": []any{item}, "total": total, "balance": 100,
		"canApprove": !blocked})
	return app.AgentCard{ID: id, Kind: domain.AgentCardPlanConfirm, Status: "pending", Payload: b}
}

func simpleCard(id int64, kind string, payload any) app.AgentCard {
	b, _ := json.Marshal(payload)
	return app.AgentCard{ID: id, Kind: kind, Status: "pending", Payload: b}
}

func (f *fakeAgent) addCard(c app.AgentCard) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sess.PendingCards = append(f.sess.PendingCards, c)
}

func (f *fakeAgent) Start(_ context.Context, _ app.Principal, in app.AgentStartInput) (app.AgentWrite, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, in)
	return app.AgentWrite{SessionID: 21, ProjectID: 11, State: "planning", TurnID: 7000, Replayed: len(f.starts) > 1}, nil
}

func (f *fakeAgent) Send(_ context.Context, _ app.Principal, sid int64, in app.AgentSendInput) (app.AgentWrite, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends = append(f.sends, in)
	return app.AgentWrite{SessionID: sid, ProjectID: 11, State: "executing", TurnID: 7002, Queued: true}, nil
}

func (f *fakeAgent) Get(context.Context, app.Principal, int64) (app.AgentSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	s := f.sess
	if len(f.states) > 0 {
		i := f.gets - 1
		if i >= len(f.states) {
			i = len(f.states) - 1
		}
		s.State = f.states[i]
	}
	if f.cardAfter > 0 && f.gets >= f.cardAfter {
		s.PendingCards = append(append([]app.AgentCard{}, s.PendingCards...), f.later...)
	}
	return s, nil
}

func (f *fakeAgent) SubmitCard(_ context.Context, p app.Principal, sid, cid int64, sub []byte) (app.AgentWrite, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p.UserID != f.sess.CreatedByUserID {
		return app.AgentWrite{}, httpx.Err(errcode.AgentNotInitiator)
	}
	for i, c := range f.sess.PendingCards {
		if c.ID != cid {
			continue
		}
		if c.Kind == domain.AgentCardPlanConfirm && strings.Contains(string(sub), `"approve"`) && f.priceTo > 0 {
			var raw map[string]any
			_ = json.Unmarshal(c.Payload, &raw)
			prev := raw["total"]
			raw["total"] = f.priceTo
			f.sess.PendingCards[i].Payload, _ = json.Marshal(raw)
			return app.AgentWrite{}, httpx.Errf(errcode.Conflict, "价格已更新").WithDetails(map[string]any{
				"reason": "price_changed", "total": f.priceTo, "previousTotal": prev})
		}
		f.submits = append(f.submits, fakeSubmit{cardID: cid, body: string(sub)})
		f.sess.PendingCards = append(f.sess.PendingCards[:i:i], f.sess.PendingCards[i+1:]...)
		turn := int64(7001)
		if strings.Contains(string(sub), `"reject"`) {
			turn = 0
		}
		return app.AgentWrite{SessionID: sid, ProjectID: 11, State: "executing", TurnID: turn, CardID: cid,
			CardStatus: "submitted"}, nil
	}
	return app.AgentWrite{}, httpx.Err(errcode.AgentCardClosed)
}

func (f *fakeAgent) EstimateTurn(context.Context, app.Principal, int64) (app.AgentTurnEstimate, error) {
	if f.estErr != nil {
		return app.AgentTurnEstimate{}, f.estErr
	}
	return app.AgentTurnEstimate{ModelCode: "claude", Credits: 2}, nil
}

func (f *fakeAgent) submitCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.submits)
}

// agentWithCards 是覆盖率用例里的会话：五种卡片各一张（结构化与 outputSchema 一起验）。
func agentWithCards() *fakeAgent {
	f := newFakeAgent()
	f.addCard(planCard(31, 30, false))
	f.addCard(simpleCard(32, "budget", map[string]any{"reason": "多一镜", "requested": 40}))
	f.addCard(simpleCard(33, "choice", map[string]any{"title": "方向", "options": []map[string]any{{"id": "a", "label": "悲壮"}}}))
	f.addCard(simpleCard(34, "questions", map[string]any{"questions": []map[string]any{
		{"id": "q1", "text": "多长", "type": "single", "options": []map[string]any{{"id": "s", "label": "短"}}}}}))
	f.addCard(simpleCard(35, "delete_confirm", map[string]any{"nodeIds": []string{"n1"}}))
	return f
}

// agentDeps：假会话端口 + 真确认服务（假 quote 表），余额 balance。
func agentDeps(balance int) (Deps, *fakeAgent, *fakeQuoteStore) {
	d, _, store := genDeps(balance)
	ag := newFakeAgent()
	d.Agent, d.WebBase = ag, "https://app.example.com"
	return d, ag, store
}

func agentErr(t *testing.T, reg *app.Registry, ctx context.Context, args string) error {
	t.Helper()
	if ctx == nil {
		ctx = ctxWithPrincipal()
	}
	return callErr(t, reg, ctx, "agent", args)
}

const chatURL = "https://app.example.com/video/chat/11?session=21"

// ---------------------------------------------------------------- 用例

// 分流表：每种卡 × 每种回答的去向。⚠️ approve 的那两格（规划确认 / 追加额度）必须走确认服务 ——
// 站内 SubmitCard 只认「发起人本人」，MCP 凭据就是本人，不在这里拦模型就能自己批准。
func TestAgentSubmit_分流表(t *testing.T) {
	cases := []struct {
		name, args string
		card       app.AgentCard
		wantStatus string // 结果 status（直接提交的是会话状态 executing）
		submitted  bool
		quotes     int
	}{
		{"规划 approve 要确认", `{"decision":"approve"}`, planCard(31, 30, false), "needs_confirmation", false, 1},
		{"追加额度 approve 要确认", `{"decision":"approve"}`, simpleCard(31, "budget",
			map[string]any{"reason": "多一镜", "requested": 40}), "needs_confirmation", false, 1},
		{"规划 reject 直接提交", `{"decision":"reject"}`, planCard(31, 30, false), "executing", true, 0},
		{"规划 adjust 直接提交", `{"decision":"adjust","comment":"少一镜"}`, planCard(31, 30, false), "executing", true, 0},
		{"追加额度 reject 直接提交", `{"decision":"reject"}`, simpleCard(31, "budget",
			map[string]any{"requested": 40}), "executing", true, 0},
		{"选择卡直接提交", `{"selected":["a"]}`, simpleCard(31, "choice", map[string]any{"title": "方向",
			"options": []map[string]any{{"id": "a", "label": "悲壮"}}}), "executing", true, 0},
		{"问答卡直接提交", `{"answers":[{"questionId":"q1","text":"十秒"}]}`, simpleCard(31, "questions",
			map[string]any{"questions": []map[string]any{{"id": "q1", "text": "多长", "type": "text"}}}), "executing", true, 0},
		{"删除确认只给链接", `{"decision":"approve"}`, simpleCard(31, "delete_confirm",
			map[string]any{"nodeIds": []string{"n1"}}), "open_in_seeaitv", false, 0},
		{"规划有不可用项不签发", `{"decision":"approve"}`, planCard(31, 30, true), "cannot_approve", false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, ag, store := agentDeps(100)
			ag.addCard(c.card)
			reg := NewRegistry(d)
			args := `{"method":"submit_card","sessionId":"21","cardId":"31",` + strings.TrimPrefix(c.args, "{")
			m := callOut(t, reg, "agent", args)
			if m["status"] != c.wantStatus || (ag.submitCount() == 1) != c.submitted || len(store.rows) != c.quotes {
				t.Fatalf("status=%v 提交=%d quote=%d：%v", m["status"], ag.submitCount(), len(store.rows), m)
			}
			if m["chatUrl"] != chatURL {
				t.Errorf("要带会话链接：%v", m["chatUrl"])
			}
			if c.wantStatus == "needs_confirmation" {
				nc := m["needsConfirmation"].(map[string]any)
				if store.rows[0].Tool != domain.QuoteToolAgentCard || !strings.Contains(m["message"].(string), "chatUrl") ||
					nc["credits"] == nil {
					t.Fatalf("%v", m)
				}
			}
		})
	}
}

// 规划批准：报价 = 卡片当前合计；基线确认后提交一次，同一确认重试 3 次不再提交（重放）。
func TestAgentApprove_基线确认后只提交一次(t *testing.T) {
	d, ag, store := agentDeps(100)
	ag.addCard(planCard(31, 30, false))
	reg := NewRegistry(d)
	args := `{"method":"submit_card","sessionId":"21","cardId":"31","decision":"approve"}`
	m := callOut(t, reg, "agent", args)
	if store.rows[0].Credits != 30 || ag.submitCount() != 0 {
		t.Fatalf("报价应取卡片合计、未确认不提交：%+v", store.rows[0])
	}
	ca := confirmed(args, m)
	for i := 0; i < 3; i++ {
		m = callOut(t, reg, "agent", ca)
		if (m["replayed"] == true) != (i > 0) || m["credits"] != float64(30) {
			t.Fatalf("第 %d 次：%v", i+1, m)
		}
	}
	if ag.submitCount() != 1 || ag.submits[0].body != `{"decision":"approve"}` {
		t.Fatalf("应只提交一次 approve：%+v", ag.submits)
	}
	if fee, _ := m["turnFee"].(map[string]any); fee != nil {
		t.Fatalf("重放不新建一轮：%v", fee)
	}
	// 换一张卡用旧确认：指纹不符。
	ag.addCard(planCard(32, 30, false))
	err := agentErr(t, reg, nil, strings.Replace(ca, `"cardId":"31"`, `"cardId":"32"`, 1))
	if bizCodeOf(err) != errcode.McpQuoteInvalid || ag.submitCount() != 1 {
		t.Fatalf("换卡片应无效：%v", err)
	}
}

// 第一次批准的结果里如实给出这一轮的对话费（批准会让站内 Agent 跑一轮）。
func TestAgentApprove_结果带轮次费(t *testing.T) {
	d, ag, _ := agentDeps(100)
	ag.addCard(simpleCard(31, "budget", map[string]any{"requested": 40}))
	reg := NewRegistry(d)
	args := `{"method":"submit_card","sessionId":"21","cardId":"31","decision":"approve"}`
	m := callOut(t, reg, "agent", confirmed(args, callOut(t, reg, "agent", args)))
	fee, _ := m["turnFee"].(map[string]any)
	if m["credits"] != float64(40) || fee == nil || fee["credits"] != float64(2) || ag.submitCount() != 1 {
		t.Fatalf("%v", m)
	}
}

// 站内批准时重新报价变了：本次确认作废（重放说清失败），签发新报价并回 40911，不按新价批准。
func TestAgentApprove_价格变化返回新报价(t *testing.T) {
	d, ag, store := agentDeps(100)
	ag.addCard(planCard(31, 30, false))
	ag.priceTo = 36
	reg := NewRegistry(d)
	args := `{"method":"submit_card","sessionId":"21","cardId":"31","decision":"approve"}`
	ca := confirmed(args, callOut(t, reg, "agent", args))
	err := agentErr(t, reg, nil, ca)
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.McpQuotePriceChanged {
		t.Fatalf("应回价格变化：%v", err)
	}
	q, _ := be.Details["quote"].(map[string]any)
	if q == nil || q["credits"] != 36 || be.Details["oldCredits"] != 30 || len(store.rows) != 2 || ag.submitCount() != 0 {
		t.Fatalf("新报价 36、旧 30、没有批准：%v（quote %d 张）", be.Details, len(store.rows))
	}
	// 旧确认再交：重放成失败，不再提交。
	if err := agentErr(t, reg, nil, ca); bizCodeOf(err) != errcode.Conflict || ag.submitCount() != 0 {
		t.Fatalf("旧确认不能再用：%v", err)
	}
	// 新报价确认后（站内价格已一致）提交一次。
	ag.priceTo = 0
	m := callOut(t, reg, "agent", withConfirm(args, q["quoteId"].(string), 36))
	if m["credits"] != float64(36) || ag.submitCount() != 1 {
		t.Fatalf("%v", m)
	}
}

func TestAgentApprove_余额不足不签发(t *testing.T) {
	d, ag, store := agentDeps(10)
	ag.addCard(planCard(31, 30, false))
	err := agentErr(t, NewRegistry(d), nil, `{"method":"submit_card","sessionId":"21","cardId":"31","decision":"approve"}`)
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.InsufficientCredits || be.Details["shortfall"] != 20 ||
		len(store.rows) != 0 || ag.submitCount() != 0 {
		t.Fatalf("%v", err)
	}
}

// 页面确认：客户端回显正确的 quoteId + 金额也不批准（同意只认审批页上的 approved_at）。
func TestAgentApprove_页面确认回显不算(t *testing.T) {
	d, ag, store := agentDeps(100)
	ag.addCard(planCard(31, 30, false))
	reg := NewRegistry(d)
	p, _ := app.PrincipalFrom(ctxWithPrincipal())
	p.RequirePageConfirm = true
	ctx := app.WithPrincipal(context.Background(), p)
	args := `{"method":"submit_card","sessionId":"21","cardId":"31","decision":"approve"}`
	tool, _ := reg.Lookup("agent")
	c, _ := tool.Decode(json.RawMessage(args))
	out, err := tool.Handler(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out.Data)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	nc := m["needsConfirmation"].(map[string]any)
	if !strings.HasSuffix(nc["approvalUrl"].(string), "/mcp/approve/"+nc["quoteId"].(string)) {
		t.Fatalf("应给审批链接：%v", nc)
	}
	err = agentErr(t, reg, ctx, withConfirm(args, nc["quoteId"].(string), 30))
	if bizCodeOf(err) != errcode.McpApprovalPending || ag.submitCount() != 0 || store.rows[0].UsedAt != nil {
		t.Fatalf("未在页面批准不得提交：%v", err)
	}
}

// 非发起人：不签发报价、不提交（站内 requireInitiator 才是权威判定，这里提前挡一道）。
func TestAgentSubmit_非发起人拒绝(t *testing.T) {
	d, ag, store := agentDeps(100)
	ag.addCard(planCard(31, 30, false))
	ag.sess.CreatedByUserID = 99
	for _, args := range []string{`{"decision":"approve"}`, `{"decision":"reject"}`} {
		err := agentErr(t, NewRegistry(d), nil, `{"method":"submit_card","sessionId":"21","cardId":"31",`+
			strings.TrimPrefix(args, "{"))
		if bizCodeOf(err) != errcode.AgentNotInitiator || len(store.rows) != 0 || ag.submitCount() != 0 {
			t.Fatalf("%s：%v", args, err)
		}
	}
}

// 卡片不在待处理列表：没有确认凭证时直接说已关闭，不签发、不提交。
func TestAgentSubmit_卡片已关闭(t *testing.T) {
	d, ag, store := agentDeps(100)
	err := agentErr(t, NewRegistry(d), nil, `{"method":"submit_card","sessionId":"21","cardId":"31","decision":"approve"}`)
	if bizCodeOf(err) != errcode.AgentCardClosed || len(store.rows) != 0 || ag.submitCount() != 0 {
		t.Fatalf("%v", err)
	}
}

// start / send：如实给出这一轮的对话费；报不出价时不编数字。
func TestAgentStart_带轮次费且报不出价不编数字(t *testing.T) {
	d, ag, _ := agentDeps(100)
	reg := NewRegistry(d)
	m := callOut(t, reg, "agent", `{"method":"start","projectId":"11","text":"三打白骨精","skillCode":"video_single","idempotencyKey":"k1"}`)
	fee := m["turnFee"].(map[string]any)
	if m["sessionId"] != "21" || m["chatUrl"] != chatURL || fee["known"] != true || fee["credits"] != float64(2) ||
		!strings.Contains(fee["note"].(string), "2 积分") || ag.starts[0].SkillCode != "video_single" {
		t.Fatalf("%v", m)
	}
	// 同一幂等键重放：没有新的一轮，不报轮次费。
	m = callOut(t, reg, "agent", `{"method":"start","projectId":"11","text":"三打白骨精","idempotencyKey":"k1"}`)
	if m["replayed"] != true || m["turnFee"] != nil {
		t.Fatalf("%v", m)
	}
	ag.estErr = httpx.Errf(errcode.ModelUnavailable, "还没有可用的推理模型")
	m = callOut(t, reg, "agent", `{"method":"send","sessionId":"21","text":"再来一版","idempotencyKey":"k2"}`)
	fee = m["turnFee"].(map[string]any)
	if fee["known"] != false || fee["credits"] != nil || m["queued"] != true {
		t.Fatalf("报不出价时不给数字：%v", m)
	}
	for _, bad := range []string{`{"method":"start","projectId":"11","text":"x"}`,
		`{"method":"start","text":"x","idempotencyKey":"k"}`,
		`{"method":"start","projectId":"11","createProjectName":"新","text":"x","idempotencyKey":"k"}`,
		`{"method":"send","sessionId":"21","text":"x"}`} {
		if err := agentErr(t, reg, nil, bad); bizCodeOf(err) != errcode.InvalidArgument {
			t.Errorf("%s 应参数错误：%v", bad, err)
		}
	}
	m = callOut(t, reg, "agent", `{"method":"start","createProjectName":"新片","text":"x","idempotencyKey":"k3"}`)
	if n := ag.starts[len(ag.starts)-1].CreateProjectName; n == nil || *n != "新片" {
		t.Fatalf("createProjectName 透传：%v", m)
	}
}

// get：结构化卡片 + 不可信文本；waitSeconds 在运行中每秒查一次，出现待处理卡片即返回。
func TestAgentGet_等到卡片即返回(t *testing.T) {
	d, ag, _ := agentDeps(100)
	ag.sess.State = "planning"
	ag.states = []string{"planning", "planning", "planning", "awaiting_approval"}
	ag.cardAfter, ag.later = 4, []app.AgentCard{planCard(31, 30, false)}
	reg := NewRegistry(d)
	m := callOut(t, reg, "agent", `{"method":"get","sessionId":"21","waitSeconds":30}`)
	s := m["session"].(map[string]any)
	cards := s["pendingCards"].([]any)
	if m["status"] != "awaiting_approval" || len(cards) != 1 || m["waitedSec"] != float64(2) || ag.gets != 4 {
		t.Fatalf("应等到卡片出现（第 4 次查询）：%v gets=%d", m, ag.gets)
	}
	c := cards[0].(map[string]any)
	if c["chatUrl"] != chatURL || c["needsUserConfirmation"] != true || c["plan"] == nil {
		t.Fatalf("%v", c)
	}
	msgs := s["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["untrusted"].(map[string]any)["content"] != "做一个三打白骨精" {
		t.Fatalf("消息正文放在 untrusted：%v", msgs)
	}
	if s["untrusted"].(map[string]any)["title"] != "三打白骨精" {
		t.Fatalf("%v", s)
	}
	// 不在运行中：不等，查一次就返回。
	d2, ag2, _ := agentDeps(100)
	ag2.sess.State = "idle"
	m = callOut(t, NewRegistry(d2), "agent", `{"method":"get","sessionId":"21","waitSeconds":30}`)
	if ag2.gets != 1 || m["waitedSec"] != nil {
		t.Fatalf("空闲时不等：%v", m)
	}
	// 一直在跑：到点返回当前状态（不是错误），等待受上限约束。
	d3, ag3, _ := agentDeps(100)
	ag3.sess.State = "executing"
	d3.WaitMax = 5_000_000_000
	m = callOut(t, NewRegistry(d3), "agent", `{"method":"get","sessionId":"21","waitSeconds":60}`)
	if m["status"] != "executing" || m["waitedSec"] != float64(5) || !strings.Contains(m["message"].(string), "仍在运行") {
		t.Fatalf("%v", m)
	}
}

// 声明如实：start / send / submit_card 会触发计费的一轮或批准花钱 → Paid + Write + generate；get 只读；工具集默认关闭。
func TestAgentTools_声明如实(t *testing.T) {
	reg := NewRegistry(Deps{})
	tool, ok := reg.Lookup("agent")
	if !ok || tool.Toolset != domain.ToolsetAgent || containsStr(domain.DefaultToolsets(), tool.Toolset) {
		t.Fatalf("agent 工具应在默认关闭的 agent 工具集：%+v", tool.Toolset)
	}
	want := map[string][4]any{
		"start": {true, true, false, collab.ActionGenerate}, "send": {true, true, false, collab.ActionGenerate},
		"get": {false, false, false, collab.ActionView}, "submit_card": {true, true, false, collab.ActionGenerate},
	}
	for _, m := range tool.Methods {
		if got := [4]any{m.Write, m.Paid, m.Destructive, m.Action}; got != want[m.Name] {
			t.Errorf("agent.%s 声明 %v，应为 %v", m.Name, got, want[m.Name])
		}
		delete(want, m.Name)
	}
	if len(want) > 0 {
		t.Errorf("缺 method：%v", want)
	}
	for k, p := range tool.InputSchema().Properties {
		if strings.HasPrefix(strings.ToLower(k), "confirm") && schemaIsBool(p) {
			t.Errorf("agent.%s 是布尔确认参数", k)
		}
	}
	if _, err := tool.Decode(json.RawMessage(`{"method":"submit_card","confirm":true}`)); err == nil {
		t.Error("应拒绝 confirm:true")
	}
	for _, s := range []string{"对话费", "不逐轮确认", "用户本人确认", "seeAitv 页面"} {
		if !strings.Contains(tool.Description, s) {
			t.Errorf("描述要如实说明「%s」", s)
		}
	}
}

// 站内 Agent 关闭时 agent 工具不注册（列表、快照片段里都没有）。
func TestAgentTools_站内Agent关闭时不注册(t *testing.T) {
	if _, ok := NewRegistry(Deps{AgentDisabled: true}).Lookup("agent"); ok {
		t.Fatal("站内 Agent 关闭时不应注册 agent 工具")
	}
	for _, ts := range app.ToolsetAvailability(NewRegistry(Deps{AgentDisabled: true}).List()) {
		if ts.Name == domain.ToolsetAgent && ts.Available {
			t.Fatal("配置片段里 agent 工具集应标为不可用")
		}
	}
}
