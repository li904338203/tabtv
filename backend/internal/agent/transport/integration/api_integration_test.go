//go:build integration

// 画布 Agent API（AGT-26）的真库 + httptest 测试。
//
// ⚠️ 走真实的路由与权限闸（collaboration 的 RequireWorkspace + GuardByMethod）：
// 「只读成员写接口 403」必须由路由层给出，绕开路由直接调 Service 的测试把闸整条删掉照样绿。
// 账本用真库（TurnLedger 要与 credit_transaction 逐笔一致）；报价、任务取消、媒体 / 资产查询用假实现。
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	agenthttp "github.com/li904338203/tabtv/backend/internal/agent/transport"
	billingapp "github.com/li904338203/tabtv/backend/internal/billing/app"
	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	canvasapp "github.com/li904338203/tabtv/backend/internal/canvas/app"
	canvasrepo "github.com/li904338203/tabtv/backend/internal/canvas/repo"
	collabapp "github.com/li904338203/tabtv/backend/internal/collaboration/app"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	collabrepo "github.com/li904338203/tabtv/backend/internal/collaboration/repo"
	collabhttp "github.com/li904338203/tabtv/backend/internal/collaboration/transport"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

// ---------------------------------------------------------------- 假端口

type fakePlans struct {
	mu    sync.Mutex
	price int // 每个计划项的新报价（0 = 原样返回卡片上的价）
}

func (f *fakePlans) QuotePlan(_ context.Context, items []domain.PlanItem, _ int64) ([]domain.PlanItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]domain.PlanItem, len(items))
	for i, it := range items {
		if f.price > 0 {
			it.Credits = f.price
		}
		out[i] = it
	}
	return out, nil
}

type fakeRefs struct{ media map[int64]string }

func (f fakeRefs) MediaKind(_ context.Context, _ int64, id int64) (string, error) {
	if k, ok := f.media[id]; ok {
		return k, nil
	}
	return "", httpx.Errf(errcode.NotFound, "素材不存在")
}

func (fakeRefs) Asset(context.Context, int64, int64) (string, string, error) {
	return "", "", httpx.Errf(errcode.NotFound, "资产不存在")
}

type fakeTasks struct {
	mu        sync.Mutex
	status    map[int64]string
	cancelled []int64
}

func (f *fakeTasks) Status(_ context.Context, _ int64, gid int64) (agentapp.TaskStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.status[gid]
	return agentapp.TaskStatus{Status: st, Terminal: st == "success" || st == "failed"}, nil
}

func (f *fakeTasks) Cancel(_ context.Context, _ int64, gid int64, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, gid)
	f.status[gid] = "failed"
	return nil
}

type fakeEvents struct {
	mu     sync.Mutex
	topics []string
	types  []string
}

func (f *fakeEvents) Publish(_ context.Context, topic string, payload any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := json.Marshal(payload)
	var m struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(b, &m)
	f.topics = append(f.topics, topic)
	f.types = append(f.types, m.Type)
	return nil
}

// ---------------------------------------------------------------- 环境

type userKey struct{}

type env struct {
	t       *testing.T
	pool    *pgxpool.Pool
	next    func() int64
	repo    *repo.Repo
	canvas  *canvasapp.Service
	ledger  *billingrepo.Repo
	plans   *fakePlans
	tasks   *fakeTasks
	events  *fakeEvents
	chat    *fakeChat
	srv     *httptest.Server
	ws      int64
	payer   int64
	owner   int64
	mate    int64
	viewer  int64
	project int64
	svc     *agentapp.Service
}

func mkUser(t *testing.T, pool *pgxpool.Pool, next func() int64, nickname, phone string) int64 {
	t.Helper()
	uid := next()
	if _, err := pool.Exec(context.Background(), `
		insert into app_user (id, phone, nickname, password_hash) values ($1,$2,$3,'x')`,
		uid, phone, nickname); err != nil {
		t.Fatal(err)
	}
	return uid
}

func newEnv(t *testing.T, enabled bool) *env {
	t.Helper()
	return newEnvWith(t, enabled, nil)
}

// newEnvWith 同 newEnv，可在装服务前改端口（营销会话的集成测试要换真实的引用解析与场景端口）。
func newEnvWith(t *testing.T, enabled bool, mutate func(e *env, p *agentapp.ServicePorts)) *env {
	t.Helper()
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	next := gen.Next
	ctx := context.Background()
	e := &env{t: t, pool: pool, next: next, repo: repo.New(pool),
		plans: &fakePlans{}, tasks: &fakeTasks{status: map[int64]string{}}, events: &fakeEvents{},
		chat: &fakeChat{out: "优化后的提示词"}}

	e.owner = mkUser(t, pool, next, "发起人", "13900000001")
	e.mate = mkUser(t, pool, next, "同事", "13900000002")
	e.viewer = mkUser(t, pool, next, "只读", "13900000003")
	cr := collabrepo.New(pool)
	w, err := cr.CreateTeam(ctx, [3]int64{next(), next(), next()}, "制作组", e.owner)
	if err != nil {
		t.Fatal(err)
	}
	e.ws, e.payer = w.ID, w.PayerAccountID
	for _, m := range []struct {
		uid  int64
		role string
	}{{e.mate, "editor"}, {e.viewer, "viewer"}} {
		if _, err := pool.Exec(ctx, `insert into workspace_member (id, workspace_id, user_id, role)
			values ($1,$2,$3,$4)`, next(), e.ws, m.uid, m.role); err != nil {
			t.Fatal(err)
		}
	}

	e.canvas = canvasapp.New(canvasrepo.New(pool), next)
	e.project = next()
	if err := canvasrepo.New(pool).Insert(ctx, canvasrepo.Project{
		ID: e.project, WorkspaceID: e.ws, CreatedBy: e.owner, Name: "测试项目",
		CanvasData: json.RawMessage(`{"nodes":[{"id":"n_1","type":"image","title":"角色图"}],"edges":[]}`),
	}); err != nil {
		t.Fatal(err)
	}

	e.ledger = billingrepo.New(pool, next)
	billing := billingapp.New(e.ledger)
	ports := agentapp.ServicePorts{
		Projects: wiring.AgentAPIProjects{Svc: e.canvas},
		Refs:     fakeRefs{media: map[int64]string{777: "image"}},
		Plans:    e.plans,
		Accounts: wiring.AgentAccounts{Billing: billing},
		Tasks:    e.tasks,
		Events:   e.events,
		// 一键优化（AGT-43）：模型与报价用假实现，账本用真库（consume + refund 要逐笔核对）。
		Optimize: &agentapp.OptimizePorts{
			Chat: e.chat, Quoter: fakeTurnQuoter{credits: 36},
			Ledger: wiring.AgentLedger{Repo: e.ledger}, Reasoning: fakeReasoning{code: "m-reason"},
		},
	}
	if mutate != nil {
		mutate(e, &ports)
	}
	svc := agentapp.NewService(e.repo, next, ports, nil)
	e.svc = svc

	userIDs := func(ctx context.Context) (int64, bool) {
		v, ok := ctx.Value(userKey{}).(int64)
		return v, ok
	}
	collabH := collabhttp.NewHandler(collabapp.New(cr, next), userIDs)
	requireUser := func(n http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			uid, err := strconv.ParseInt(r.Header.Get("X-Test-User"), 10, 64)
			if err != nil {
				httpx.Fail(w, r, httpx.Err(errcode.Unauthenticated))
				return
			}
			n.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, uid)))
		})
	}
	wsGuard := func(n http.Handler) http.Handler {
		return collabH.RequireWorkspace(collabhttp.GuardByMethod(n))
	}
	h := agenthttp.NewHandler(svc, func(r *http.Request) int64 {
		v, _ := userIDs(r.Context())
		return v
	}, enabled)
	r := chi.NewRouter()
	r.Use(httpx.TraceID)
	h.Mount(r, requireUser, wsGuard)
	e.srv = httptest.NewServer(r)
	t.Cleanup(e.srv.Close)
	return e
}

type resp struct {
	status int
	body   map[string]any
}

func (r resp) code() int {
	c, _ := r.body["code"].(float64)
	return int(c)
}

func (e *env) do(method, path string, user int64, key string, body any) resp {
	e.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-User", strconv.FormatInt(user, 10))
	req.Header.Set("X-Workspace-Id", strconv.FormatInt(e.ws, 10))
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	out := resp{status: res.StatusCode, body: map[string]any{}}
	_ = json.NewDecoder(res.Body).Decode(&out.body)
	return out
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) grant(amount int) {
	e.t.Helper()
	if _, err := e.ledger.Grant(context.Background(), billingrepo.GrantInput{
		PayerID: e.payer, Source: "admin_adjust", Amount: amount, Reason: "测试",
	}); err != nil {
		e.t.Fatal(err)
	}
}

// createSession 由发起人建一个会话，返回会话 ID。
func (e *env) createSession(key string) int64 {
	e.t.Helper()
	r := e.do("POST", "/agent/sessions", e.owner, key, map[string]any{
		"projectId": strconv.FormatInt(e.project, 10),
		"message":   map[string]any{"text": "三打白骨精，悟空视角"},
	})
	if r.status != http.StatusCreated && r.status != http.StatusOK {
		e.t.Fatalf("建会话失败：%d %v", r.status, r.body)
	}
	sid, _ := strconv.ParseInt(r.body["sessionId"].(string), 10, 64)
	return sid
}

// suspendWith 把会话挂起在一张卡片上（模拟 Worker 发卡），返回卡片 ID。
func (e *env) suspendWith(sid int64, kind domain.CardKind, payload any) int64 {
	e.t.Helper()
	ctx := context.Background()
	sess, err := e.repo.GetByID(ctx, sid)
	if err != nil {
		e.t.Fatal(err)
	}
	turn, err := e.repo.InsertTurn(ctx, e.pool, repo.Turn{ID: e.next(), SessionID: sid, Trigger: domain.TriggerSystem})
	if err != nil {
		e.t.Fatal(err)
	}
	step, err := e.repo.InsertStep(ctx, e.pool, repo.Step{ID: e.next(), SessionID: sid, TurnID: turn.ID, Tool: "ask_user"})
	if err != nil {
		e.t.Fatal(err)
	}
	raw, _ := json.Marshal(payload)
	card, err := e.repo.InsertCard(ctx, e.pool, repo.Card{ID: e.next(), SessionID: sid, StepID: step.ID,
		Kind: kind, Payload: raw})
	if err != nil {
		e.t.Fatal(err)
	}
	to, _ := domain.SuspendStateFor(kind)
	if _, err := e.repo.TransitionState(ctx, e.pool, sid, sess.State, to); err != nil {
		e.t.Fatal(err)
	}
	return card.ID
}

func (e *env) session(sid int64) repo.Session {
	e.t.Helper()
	s, err := e.repo.GetByID(context.Background(), sid)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *env) card(sid, cid int64) repo.Card {
	e.t.Helper()
	c, err := e.repo.GetCard(context.Background(), sid, cid)
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

func path(f string, a ...any) string { return fmt.Sprintf(f, a...) }

func planPayload(total int, docVersion int) domain.PlanConfirmPayload {
	return domain.PlanConfirmPayload{
		Title: "三打白骨精", DocPath: "/plan.md", PlanDocVersion: docVersion,
		Items: []domain.PlanItem{
			{Kind: "video", ModelCode: "m1", Label: "镜头 1", Quantity: 1, Unit: "time", Credits: total / 2},
			{Kind: "video", ModelCode: "m1", Label: "镜头 2", Quantity: 1, Unit: "time", Credits: total - total/2},
		},
		Total: total, Balance: 0,
	}
}

func approve() map[string]any {
	return map[string]any{"submission": map[string]any{"decision": "approve"}}
}

// ---------------------------------------------------------------- 权限

func TestAgentAPI_只读成员写接口403(t *testing.T) {
	e := newEnv(t, true)
	sid := e.createSession("k-owner-0001")
	cid := e.suspendWith(sid, domain.CardChoice, domain.ChoicePayload{Title: "选方向",
		Options: []domain.Option{{ID: "a", Label: "悲壮"}}, ResumeState: domain.StatePlanning})

	for _, c := range []struct {
		name, method, url string
		body              any
	}{
		{"建会话", "POST", "/agent/sessions", map[string]any{"projectId": strconv.FormatInt(e.project, 10),
			"message": map[string]any{"text": "hi"}}},
		{"发消息", "POST", path("/agent/sessions/%d/messages", sid), map[string]any{"text": "hi"}},
		{"提交卡片", "POST", path("/agent/sessions/%d/cards/%d/submit", sid, cid),
			map[string]any{"submission": map[string]any{"selected": []string{"a"}}}},
		{"停止", "POST", path("/agent/sessions/%d/stop", sid), nil},
	} {
		r := e.do(c.method, c.url, e.viewer, "k-viewer-0001", c.body)
		if r.status != http.StatusForbidden || r.code() != int(errcode.InsufficientRole) {
			t.Errorf("%s：只读成员应 403/%d，得到 %d %v", c.name, errcode.InsufficientRole, r.status, r.body)
		}
	}
	for _, url := range []string{
		path("/projects/%d/agent/sessions", e.project), path("/agent/sessions/%d", sid),
		path("/agent/sessions/%d/messages", sid), "/agent/skills",
	} {
		if r := e.do("GET", url, e.viewer, "", nil); r.status != http.StatusOK {
			t.Errorf("GET %s 只读成员应 200，得到 %d %v", url, r.status, r.body)
		}
	}
	if n := e.count(`select count(*) from agent_session`); n != 1 {
		t.Errorf("只读成员不应建出会话，会话数 %d", n)
	}
	if c := e.card(sid, cid); c.Status != domain.CardPending {
		t.Errorf("只读成员的提交不应生效，卡片状态 %s", c.Status)
	}
}

func TestAgentAPI_非发起人提交卡片403(t *testing.T) {
	e := newEnv(t, true)
	sid := e.createSession("k-owner-0002")
	cid := e.suspendWith(sid, domain.CardChoice, domain.ChoicePayload{Title: "选方向",
		Options: []domain.Option{{ID: "a", Label: "悲壮"}}, ResumeState: domain.StatePlanning})

	r := e.do("POST", path("/agent/sessions/%d/cards/%d/submit", sid, cid), e.mate, "",
		map[string]any{"submission": map[string]any{"selected": []string{"a"}}})
	if r.status != http.StatusForbidden || r.code() != int(errcode.AgentNotInitiator) {
		t.Fatalf("非发起人提交卡片应 403/40306，得到 %d %v", r.status, r.body)
	}
	r = e.do("POST", path("/agent/sessions/%d/messages", sid), e.mate, "k-mate-00001", map[string]any{"text": "我也说一句"})
	if r.status != http.StatusForbidden || r.code() != int(errcode.AgentNotInitiator) {
		t.Fatalf("非发起人发消息应 403/40306，得到 %d %v", r.status, r.body)
	}
	if c := e.card(sid, cid); c.Status != domain.CardPending {
		t.Errorf("卡片不应被提交：%s", c.Status)
	}
}

func TestAgentAPI_他人会话可查看(t *testing.T) {
	e := newEnv(t, true)
	sid := e.createSession("k-owner-0003")
	r := e.do("GET", path("/agent/sessions/%d", sid), e.mate, "", nil)
	if r.status != http.StatusOK {
		t.Fatalf("同工作区成员应能查看：%d %v", r.status, r.body)
	}
	s := r.body["session"].(map[string]any)
	if s["isInitiator"] != false || s["initiatorName"] != "发起人" {
		t.Errorf("应标明不是发起人并带发起人名字：%v", s)
	}
	msgs := r.body["messages"].(map[string]any)["items"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["content"] != "三打白骨精，悟空视角" {
		t.Errorf("详情应带首条消息：%v", msgs)
	}
	list := e.do("GET", path("/projects/%d/agent/sessions", e.project), e.mate, "", nil)
	if items := list.body["items"].([]any); len(items) != 1 {
		t.Errorf("项目会话列表应含他人发起的会话：%v", list.body)
	}
}

func TestAgentAPI_总开关关闭404(t *testing.T) {
	e := newEnv(t, false)
	for _, c := range []struct{ method, url string }{
		{"GET", "/agent/preferences"}, {"GET", "/agent/skills"}, {"POST", "/agent/sessions"},
		{"GET", path("/projects/%d/agent/sessions", e.project)},
	} {
		r := e.do(c.method, c.url, e.owner, "k-off-000001", map[string]any{})
		if r.status != http.StatusNotFound || r.code() != int(errcode.AgentDisabled) {
			t.Errorf("%s %s 应 404/40402，得到 %d %v", c.method, c.url, r.status, r.body)
		}
	}
}

// ---------------------------------------------------------------- 建会话 / 发消息

func TestCreateSession_幂等键重复只建一个(t *testing.T) {
	e := newEnv(t, true)
	body := map[string]any{"projectId": strconv.FormatInt(e.project, 10), "message": map[string]any{"text": "hi"}}

	if r := e.do("POST", "/agent/sessions", e.owner, "", body); r.status != http.StatusBadRequest ||
		r.code() != int(errcode.MissingIdempotencyKey) {
		t.Fatalf("缺幂等键应 400/40002，得到 %d %v", r.status, r.body)
	}

	// 并发 6 个同键请求 + 再串行一次：只建 1 个会话、1 个轮、1 条 outbox。
	var wg sync.WaitGroup
	ids := make([]string, 6)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := e.do("POST", "/agent/sessions", e.owner, "k-same-key-01", body)
			if s, ok := r.body["sessionId"].(string); ok {
				ids[i] = s
			}
		}(i)
	}
	wg.Wait()
	r := e.do("POST", "/agent/sessions", e.owner, "k-same-key-01", body)
	if r.status != http.StatusOK || r.body["replayed"] != true {
		t.Fatalf("重放应 200 + replayed，得到 %d %v", r.status, r.body)
	}
	for _, s := range ids {
		if s != r.body["sessionId"] {
			t.Errorf("同键请求应拿到同一个会话：%v vs %v", ids, r.body["sessionId"])
		}
	}
	if n := e.count(`select count(*) from agent_session`); n != 1 {
		t.Errorf("会话数应为 1，得到 %d", n)
	}
	if n := e.count(`select count(*) from agent_turn`); n != 1 {
		t.Errorf("轮次数应为 1，得到 %d", n)
	}
	if n := e.count(`select count(*) from outbox_message where aggregate_type = 'agent'`); n != 1 {
		t.Errorf("outbox 应为 1，得到 %d", n)
	}
	if n := e.count(`select count(*) from agent_message`); n != 1 {
		t.Errorf("消息数应为 1，得到 %d", n)
	}

	// 顺带建项目：同一个键只建一个项目。
	cp := map[string]any{"createProject": map[string]any{"name": "新故事"}, "message": map[string]any{"text": "写个故事"}}
	a := e.do("POST", "/agent/sessions", e.owner, "k-create-proj", cp)
	b := e.do("POST", "/agent/sessions", e.owner, "k-create-proj", cp)
	if a.status != http.StatusCreated || b.status != http.StatusOK || a.body["projectId"] != b.body["projectId"] {
		t.Fatalf("顺带建项目应幂等：%v / %v", a.body, b.body)
	}
	if n := e.count(`select count(*) from project`); n != 2 {
		t.Errorf("应只多建 1 个项目，项目数 %d", n)
	}
}

func TestCreateSession_引用归属校验(t *testing.T) {
	e := newEnv(t, true)
	mk := func(key string, refs ...map[string]any) resp {
		return e.do("POST", "/agent/sessions", e.owner, key, map[string]any{
			"projectId": strconv.FormatInt(e.project, 10),
			"message":   map[string]any{"text": "看这个", "refs": refs},
		})
	}
	for _, c := range []struct {
		name string
		ref  map[string]any
	}{
		{"别的工作区 / 不存在的素材", map[string]any{"kind": "media", "id": "888"}},
		{"画布上没有的节点", map[string]any{"kind": "node", "id": "n_404"}},
		{"带地址", map[string]any{"kind": "media", "id": "777", "url": "https://evil.example/a.png"}},
		{"未知类型", map[string]any{"kind": "file", "id": "1"}},
	} {
		r := mk("k-ref-"+c.name, c.ref)
		if r.status != http.StatusBadRequest {
			t.Errorf("%s 应 400，得到 %d %v", c.name, r.status, r.body)
		}
	}
	if n := e.count(`select count(*) from agent_session`); n != 0 {
		t.Fatalf("引用不合法时不应建会话，会话数 %d", n)
	}
	r := mk("k-ref-ok-0001", map[string]any{"kind": "node", "id": "n_1"}, map[string]any{"kind": "media", "id": "777"})
	if r.status != http.StatusCreated {
		t.Fatalf("合法引用应建成：%d %v", r.status, r.body)
	}
	var refs []agentapp.ResolvedRef
	if err := e.pool.QueryRow(context.Background(), `select refs from agent_message limit 1`).Scan(&refs); err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs[0].NodeType != "image" || refs[0].Label != "角色图" || refs[1].MediaKind != "image" {
		t.Errorf("存库的应是服务端解析后的摘要：%+v", refs)
	}
}

func TestPostMessage_运行中排队为下一轮(t *testing.T) {
	e := newEnv(t, true)
	sid := e.createSession("k-owner-0004")
	if st := e.session(sid).State; st != domain.StatePlanning {
		t.Fatalf("首条消息后应在 planning，得到 %s", st)
	}
	r := e.do("POST", path("/agent/sessions/%d/messages", sid), e.owner, "k-msg-000001", map[string]any{"text": "再加一点"})
	if r.status != http.StatusCreated || r.body["queued"] != true || r.body["state"] != "planning" {
		t.Fatalf("运行中发消息应排队为下一轮：%d %v", r.status, r.body)
	}
	if n := e.count(`select count(*) from agent_turn where session_id = $1`, sid); n != 2 {
		t.Errorf("应建第二轮，轮次数 %d", n)
	}
	// 同键重发：不建第三轮。
	r = e.do("POST", path("/agent/sessions/%d/messages", sid), e.owner, "k-msg-000001", map[string]any{"text": "再加一点"})
	if r.status != http.StatusOK || r.body["replayed"] != true {
		t.Fatalf("同键重发应重放：%d %v", r.status, r.body)
	}
	if n := e.count(`select count(*) from agent_turn where session_id = $1`, sid); n != 2 {
		t.Errorf("同键重发不应建新轮，轮次数 %d", n)
	}
	if n := e.count(`select count(*) from outbox_message where aggregate_type = 'agent'`); n != 2 {
		t.Errorf("outbox 应为 2，得到 %d", n)
	}
}

// ---------------------------------------------------------------- 卡片

func TestSubmitCard_重复提交幂等(t *testing.T) {
	e := newEnv(t, true)
	sid := e.createSession("k-owner-0005")
	cid := e.suspendWith(sid, domain.CardChoice, domain.ChoicePayload{Title: "选方向",
		Options:     []domain.Option{{ID: "a", Label: "方向 1 悲壮离别"}, {ID: "b", Label: "方向 2"}},
		ResumeState: domain.StatePlanning})
	turnsBefore := e.count(`select count(*) from agent_turn where session_id = $1`, sid)

	body := map[string]any{"submission": map[string]any{"selected": []string{"a"}}}
	r := e.do("POST", path("/agent/sessions/%d/cards/%d/submit", sid, cid), e.owner, "", body)
	if r.status != http.StatusOK || r.body["state"] != "planning" || r.body["cardStatus"] != "submitted" {
		t.Fatalf("首次提交应成功并回到发卡前状态：%d %v", r.status, r.body)
	}
	var content string
	if err := e.pool.QueryRow(context.Background(),
		`select content from agent_message where card_id = $1`, cid).Scan(&content); err != nil {
		t.Fatal(err)
	}
	if content != "我选择了：方向 1 悲壮离别" {
		t.Errorf("提交应写成具体内容的用户消息，得到 %q", content)
	}
	r = e.do("POST", path("/agent/sessions/%d/cards/%d/submit", sid, cid), e.owner, "", body)
	if r.status != http.StatusConflict || r.code() != int(errcode.AgentCardClosed) {
		t.Fatalf("重复提交应 409/40908，得到 %d %v", r.status, r.body)
	}
	if n := e.count(`select count(*) from agent_turn where session_id = $1`, sid); n != turnsBefore+1 {
		t.Errorf("重复提交不应新建轮次：%d → %d", turnsBefore, n)
	}
}

// 04 文档 P-08：会话停在规划确认时，点一张更早的、还挂着的选择卡。原先把挂起卡写死成追加额度卡，
// 于是直接进了执行态（规划卡也没作废），绕过了「未批准不得执行」。现在与直接发消息同一个去向。
func TestSubmitCard_过时的选择卡不能绕过规划确认(t *testing.T) {
	e := newEnv(t, true)
	e.grant(1000)
	sid := e.createSession("k-owner-0105")
	stale := e.suspendWith(sid, domain.CardChoice, domain.ChoicePayload{Title: "选方向",
		Options: []domain.Option{{ID: "a", Label: "方向 1"}, {ID: "b", Label: "方向 2"}}, ResumeState: domain.StatePlanning})
	// 用户没点这张卡、直接说了话，Agent 随后提交了规划：会话停在规划确认，选择卡还挂着。
	plan := e.suspendWith(sid, domain.CardPlanConfirm, planPayload(60, 0))

	r := e.do("POST", path("/agent/sessions/%d/cards/%d/submit", sid, stale), e.owner, "",
		map[string]any{"submission": map[string]any{"selected": []string{"a"}}})
	if r.status != http.StatusOK {
		t.Fatalf("提交过时的卡片应按一条消息处理：%d %v", r.status, r.body)
	}
	s := e.session(sid)
	if s.State != domain.StatePlanning || s.ApprovedBudget != 0 {
		t.Fatalf("应回到规划（同直接发消息），不能进执行：%+v", s)
	}
	if c := e.card(sid, plan); c.Status != domain.CardVoided {
		t.Fatalf("被跳过的规划卡应作废：%s", c.Status)
	}
	if c := e.card(sid, stale); c.Status != domain.CardSubmitted {
		t.Fatalf("过时的卡片本身记为已提交：%s", c.Status)
	}
}

func TestApprove_额度等于报价合计(t *testing.T) {
	e := newEnv(t, true)
	e.grant(1000)
	sid := e.createSession("k-owner-0006")
	cid := e.suspendWith(sid, domain.CardPlanConfirm, planPayload(60, 0))
	r := e.do("POST", path("/agent/sessions/%d/cards/%d/submit", sid, cid), e.owner, "", approve())
	if r.status != http.StatusOK || r.body["state"] != "executing" {
		t.Fatalf("批准应成功并进入执行：%d %v", r.status, r.body)
	}
	if s := e.session(sid); s.ApprovedBudget != 60 {
		t.Errorf("approved_budget 应 == 卡片合计 60，得到 %d", s.ApprovedBudget)
	}
	if r.body["approvedBudget"].(float64) != 60 {
		t.Errorf("响应里的额度应为 60：%v", r.body)
	}
}

func TestApprove_余额不足拒绝(t *testing.T) {
	e := newEnv(t, true)
	e.grant(10)
	sid := e.createSession("k-owner-0007")
	cid := e.suspendWith(sid, domain.CardPlanConfirm, planPayload(60, 0))
	r := e.do("POST", path("/agent/sessions/%d/cards/%d/submit", sid, cid), e.owner, "", approve())
	if r.status != http.StatusUnprocessableEntity || r.code() != int(errcode.InsufficientCredits) {
		t.Fatalf("余额不足应 422/42201，得到 %d %v", r.status, r.body)
	}
	d := r.body["details"].(map[string]any)
	if d["shortfall"].(float64) != 50 || d["required"].(float64) != 60 || d["balance"].(float64) != 10 {
		t.Errorf("应带差额：%v", d)
	}
	if c := e.card(sid, cid); c.Status != domain.CardPending {
		t.Errorf("余额不足时卡片应保持待处理：%s", c.Status)
	}
	if s := e.session(sid); s.ApprovedBudget != 0 || s.State != domain.StateAwaitingApproval {
		t.Errorf("余额不足不应写额度或迁移状态：%+v", s)
	}
}

func TestApprove_价格变动返回新报价(t *testing.T) {
	e := newEnv(t, true)
	e.grant(1000)
	sid := e.createSession("k-owner-0008")
	cid := e.suspendWith(sid, domain.CardPlanConfirm, planPayload(60, 0))
	e.plans.price = 40 // 两项各 40 → 合计 80

	r := e.do("POST", path("/agent/sessions/%d/cards/%d/submit", sid, cid), e.owner, "", approve())
	if r.status != http.StatusConflict || r.code() != int(errcode.Conflict) {
		t.Fatalf("价格变动应 409/40901，得到 %d %v", r.status, r.body)
	}
	d := r.body["details"].(map[string]any)
	if d["reason"] != "price_changed" || d["total"].(float64) != 80 || d["previousTotal"].(float64) != 60 {
		t.Errorf("details 应带新报价：%v", d)
	}
	var p domain.PlanConfirmPayload
	_ = json.Unmarshal(e.card(sid, cid).Payload, &p)
	if p.Total != 80 || e.card(sid, cid).Status != domain.CardPending {
		t.Errorf("新报价应写回卡片且卡片仍待处理：%+v", p)
	}
	if s := e.session(sid); s.ApprovedBudget != 0 {
		t.Errorf("价格变动时不应按新价格批准：%d", s.ApprovedBudget)
	}
	// 用户看过新价后再确认：按 80 批准。
	r = e.do("POST", path("/agent/sessions/%d/cards/%d/submit", sid, cid), e.owner, "", approve())
	if r.status != http.StatusOK || e.session(sid).ApprovedBudget != 80 {
		t.Fatalf("再次确认应按新报价批准：%d %v", r.status, r.body)
	}
}

func TestApprove_规划文档已改卡片作废(t *testing.T) {
	e := newEnv(t, true)
	e.grant(1000)
	ctx := context.Background()
	doc, err := e.canvas.CreateDocument(ctx, e.ws, e.project, canvasapp.DocumentActor{UserID: e.owner},
		collab.RoleOwner, canvasapp.CreateDocumentInput{Path: "/plan.md", Content: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	sid := e.createSession("k-owner-0009")
	cid := e.suspendWith(sid, domain.CardPlanConfirm, planPayload(60, doc.Version))
	if _, err := e.canvas.UpdateDocument(ctx, e.ws, e.project, doc.ID, canvasapp.DocumentActor{UserID: e.owner},
		collab.RoleOwner, doc.Version, "v2：改了规划"); err != nil {
		t.Fatal(err)
	}
	turnsBefore := e.count(`select count(*) from agent_turn where session_id = $1`, sid)

	r := e.do("POST", path("/agent/sessions/%d/cards/%d/submit", sid, cid), e.owner, "", approve())
	if r.status != http.StatusConflict || r.code() != int(errcode.AgentCardClosed) {
		t.Fatalf("规划文档已改应 409/40908，得到 %d %v", r.status, r.body)
	}
	if d := r.body["details"].(map[string]any); d["reason"] != "plan_doc_changed" {
		t.Errorf("details.reason 应为 plan_doc_changed：%v", d)
	}
	if c := e.card(sid, cid); c.Status != domain.CardVoided {
		t.Errorf("卡片应作废：%s", c.Status)
	}
	s := e.session(sid)
	if s.State != domain.StatePlanning || s.ApprovedBudget != 0 {
		t.Errorf("应回到规划且不写额度：%+v", s)
	}
	if n := e.count(`select count(*) from agent_turn where session_id = $1 and trigger = 'system'`, sid); n < 2 {
		t.Errorf("应建一个系统轮（不计费）让 Agent 重新规划")
	}
	if n := e.count(`select count(*) from agent_turn where session_id = $1`, sid); n != turnsBefore+1 {
		t.Errorf("应恰好新建一轮：%d → %d", turnsBefore, n)
	}
}

func TestAdjust_旧规划作废回规划态(t *testing.T) {
	e := newEnv(t, true)
	sid := e.createSession("k-owner-0010")
	cid := e.suspendWith(sid, domain.CardPlanConfirm, planPayload(60, 0))
	r := e.do("POST", path("/agent/sessions/%d/cards/%d/submit", sid, cid), e.owner, "",
		map[string]any{"submission": map[string]any{"decision": "adjust", "comment": "换成水墨风"}})
	if r.status != http.StatusOK || r.body["state"] != "planning" || r.body["cardStatus"] != "voided" {
		t.Fatalf("调整应作废卡片并回到规划：%d %v", r.status, r.body)
	}
	if c := e.card(sid, cid); c.Status != domain.CardVoided || len(c.Submission) == 0 {
		t.Errorf("卡片应作废且留下修改意见：%+v", c)
	}
	if r.body["turnId"] == nil {
		t.Error("调整应建一轮让 Agent 重新规划")
	}
	// 作废的卡片不能再批准。
	r = e.do("POST", path("/agent/sessions/%d/cards/%d/submit", sid, cid), e.owner, "", approve())
	if r.code() != int(errcode.AgentCardClosed) {
		t.Errorf("作废的卡片再提交应 40908：%v", r.body)
	}
}

func TestBudgetCard_拒绝后回idle不再扣费(t *testing.T) {
	e := newEnv(t, true)
	e.grant(1000)
	sid := e.createSession("k-owner-0011")
	cid := e.suspendWith(sid, domain.CardBudget, domain.BudgetPayload{Reason: "重试第 3 镜", Requested: 20,
		Approved: 60, Used: 60})
	turnsBefore := e.count(`select count(*) from agent_turn where session_id = $1`, sid)
	r := e.do("POST", path("/agent/sessions/%d/cards/%d/submit", sid, cid), e.owner, "",
		map[string]any{"submission": map[string]any{"decision": "reject"}})
	if r.status != http.StatusOK || r.body["state"] != "idle" || r.body["turnId"] != nil {
		t.Fatalf("拒绝追加应回 idle 且不建轮：%d %v", r.status, r.body)
	}
	if n := e.count(`select count(*) from agent_turn where session_id = $1`, sid); n != turnsBefore {
		t.Errorf("拒绝后不应新建轮次（不再扣费）：%d → %d", turnsBefore, n)
	}
	if s := e.session(sid); s.ApprovedBudget != 0 {
		t.Errorf("拒绝不应追加额度：%d", s.ApprovedBudget)
	}

	// 另一张追加额度卡：批准 → 额度 += 追加额。
	cid2 := e.suspendWith(sid, domain.CardBudget, domain.BudgetPayload{Reason: "再来", Requested: 25})
	r = e.do("POST", path("/agent/sessions/%d/cards/%d/submit", sid, cid2), e.owner, "", approve())
	if r.status != http.StatusOK || r.body["state"] != "executing" || e.session(sid).ApprovedBudget != 25 {
		t.Fatalf("批准追加应 += 追加额并回执行：%d %v", r.status, r.body)
	}
}

// ---------------------------------------------------------------- 停止 / 取消

func (e *env) waitingStep(sid int64, gid int64) {
	e.t.Helper()
	ctx := context.Background()
	turn, err := e.repo.LatestTurn(ctx, sid)
	if err != nil {
		e.t.Fatal(err)
	}
	st, err := e.repo.InsertStep(ctx, e.pool, repo.Step{ID: e.next(), SessionID: sid, TurnID: turn.ID,
		Tool: "generate", CreditsQuoted: 30})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.repo.SetStepWaiting(ctx, e.pool, st.ID, gid); err != nil {
		e.t.Fatal(err)
	}
}

func TestStop_写停止标记并列出未终态任务(t *testing.T) {
	e := newEnv(t, true)
	sid := e.createSession("k-owner-0012")
	e.waitingStep(sid, 5001)
	e.waitingStep(sid, 5002)
	e.tasks.status[5001] = "running"
	e.tasks.status[5002] = "success"

	r := e.do("POST", path("/agent/sessions/%d/stop", sid), e.owner, "", nil)
	if r.status != http.StatusOK || r.body["stopRequested"] != true {
		t.Fatalf("停止应成功：%d %v", r.status, r.body)
	}
	tasks := r.body["tasks"].([]any)
	if len(tasks) != 1 || tasks[0].(map[string]any)["generationId"] != "5001" {
		t.Errorf("只应列出未终态的任务：%v", tasks)
	}
	if e.session(sid).StopRequestedAt == nil {
		t.Error("应写 stop_requested_at")
	}
	if len(e.tasks.cancelled) != 0 {
		t.Error("停止不应自动取消已提交的任务")
	}
}

func TestCancel_后续消息被拒(t *testing.T) {
	e := newEnv(t, true)
	sid := e.createSession("k-owner-0013")
	e.waitingStep(sid, 6001)
	e.tasks.status[6001] = "running"
	cid := e.suspendWith(sid, domain.CardChoice, domain.ChoicePayload{Title: "选",
		Options: []domain.Option{{ID: "a", Label: "A"}}, ResumeState: domain.StatePlanning})

	r := e.do("POST", path("/agent/sessions/%d/cancel", sid), e.owner, "", map[string]any{"cancelTasks": true})
	if r.status != http.StatusOK || r.body["state"] != "cancelled" {
		t.Fatalf("取消应成功：%d %v", r.status, r.body)
	}
	if got := r.body["cancelledTasks"].([]any); len(got) != 1 || got[0] != "6001" {
		t.Errorf("应取消未终态任务：%v", got)
	}
	if c := e.card(sid, cid); c.Status != domain.CardVoided {
		t.Errorf("待处理卡片应作废：%s", c.Status)
	}
	r = e.do("POST", path("/agent/sessions/%d/messages", sid), e.owner, "k-after-cancel", map[string]any{"text": "还在吗"})
	if r.status != http.StatusConflict || r.code() != int(errcode.AgentSessionCancelled) {
		t.Fatalf("取消后发消息应 409/40909，得到 %d %v", r.status, r.body)
	}
	// 重复取消：返回当前结果，不报错。
	if r := e.do("POST", path("/agent/sessions/%d/cancel", sid), e.owner, "", nil); r.status != http.StatusOK {
		t.Errorf("重复取消应 200：%d %v", r.status, r.body)
	}
}

// ---------------------------------------------------------------- 账本

func TestTurnLedger_三项与账本一致(t *testing.T) {
	e := newEnv(t, true)
	e.grant(1000)
	ctx := context.Background()
	sid := e.createSession("k-owner-0014")
	turn, err := e.repo.LatestTurn(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	e.waitingStep(sid, 7001)
	e.waitingStep(sid, 7002)
	consume := func(refType, refID string, amount int) {
		if _, err := e.ledger.Consume(ctx, billingrepo.ConsumeInput{PayerID: e.payer, Amount: amount,
			RefType: refType, RefID: refID, UserID: e.owner, Reason: "测试"}); err != nil {
			t.Fatal(err)
		}
	}
	consume("agent_turn", strconv.FormatInt(turn.ID, 10), 4)
	consume("generation", "7001", 30)
	consume("generation", "7002", 20)
	consume("generation", "9999", 50) // 不属于本轮
	if _, err := e.ledger.Refund(ctx, billingrepo.RefundInput{PayerID: e.payer, RefType: "generation",
		RefID: "7002", Reason: "失败退款"}); err != nil {
		t.Fatal(err)
	}

	r := e.do("GET", path("/agent/sessions/%d/turns/%d/ledger", sid, turn.ID), e.mate, "", nil)
	if r.status != http.StatusOK {
		t.Fatalf("取账本失败：%d %v", r.status, r.body)
	}
	if r.body["chat"].(float64) != 4 || r.body["generation"].(float64) != 50 || r.body["refunded"].(float64) != 20 {
		t.Errorf("对话 / 生成 / 退回应为 4 / 50 / 20（退回单列不从花费里减）：%v", r.body)
	}
	entries := r.body["entries"].([]any)
	want := e.count(`select count(*) from credit_transaction where payer_account_id = $1 and
		((ref_type = 'agent_turn' and ref_id = $2) or (ref_type = 'generation' and ref_id in ('7001','7002')))`,
		e.payer, strconv.FormatInt(turn.ID, 10))
	if len(entries) != want || want != 4 {
		t.Errorf("逐笔流水应与账本一致：接口 %d 条，账本 %d 条", len(entries), want)
	}
	// 别的会话的轮次：404。
	if r := e.do("GET", path("/agent/sessions/%d/turns/%d/ledger", sid, 123), e.owner, "", nil); r.status != http.StatusNotFound {
		t.Errorf("不属于本会话的轮次应 404：%d", r.status)
	}
}

// ---------------------------------------------------------------- 偏好与事件

func TestPreferences_原样保存只要登录(t *testing.T) {
	e := newEnv(t, true)
	body := map[string]any{"prefs": map[string]any{"video": map[string]any{"model": "seedance"}, "future": 1}}
	if r := e.do("PUT", "/agent/preferences", e.viewer, "", body); r.status != http.StatusOK {
		t.Fatalf("保存偏好失败：%d %v", r.status, r.body)
	}
	r := e.do("GET", "/agent/preferences", e.viewer, "", nil)
	p := r.body["prefs"].(map[string]any)
	if p["future"].(float64) != 1 {
		t.Errorf("偏好应原样保存（含未知字段）：%v", p)
	}
	if len(e.events.types) != 0 {
		t.Errorf("偏好不发事件：%v", e.events.types)
	}
}

func TestEvents_写接口发到项目topic(t *testing.T) {
	e := newEnv(t, true)
	e.createSession("k-owner-0015")
	topic := "project:" + strconv.FormatInt(e.project, 10)
	if len(e.events.topics) == 0 || e.events.topics[0] != topic || e.events.types[0] != agentapp.EventAgentState {
		t.Errorf("建会话后应发 agent.state 到 %s：%v %v", topic, e.events.topics, e.events.types)
	}
}

// 04 文档 P-22：发消息带技能是追加（原先覆盖整组），且只认适用于会话场景的技能。
func TestPostMessage_带技能追加且只认本场景的技能(t *testing.T) {
	e := newEnv(t, true)
	ctx := context.Background()
	for _, sk := range []repo.Skill{
		{ID: e.next(), Code: "gen_a", Name: "通用 A", IsEnabled: true},
		{ID: e.next(), Code: "gen_b", Name: "通用 B", IsEnabled: true},
		{ID: e.next(), Code: "mk_only", Name: "营销专用", IsEnabled: true, Scenes: json.RawMessage(`["marketing"]`)},
	} {
		if _, err := e.repo.InsertSkill(ctx, sk); err != nil {
			t.Fatal(err)
		}
	}
	sid := e.createSession("k-owner-0110")
	r := e.do("POST", path("/agent/sessions/%d/messages", sid), e.owner, "k-msg-000110",
		map[string]any{"text": "用 A", "skillCode": "gen_a"})
	if r.status != http.StatusCreated {
		t.Fatalf("发消息失败：%d %v", r.status, r.body)
	}
	r = e.do("POST", path("/agent/sessions/%d/messages", sid), e.owner, "k-msg-000111",
		map[string]any{"text": "再用 B", "skillCode": "gen_b"})
	if r.status != http.StatusCreated {
		t.Fatalf("发消息失败：%d %v", r.status, r.body)
	}
	if got := string(e.session(sid).SkillCodes); got != `["gen_a", "gen_b"]` && got != `["gen_a","gen_b"]` {
		t.Fatalf("第二次带技能应追加，不覆盖第一次的：%s", got)
	}
	r = e.do("POST", path("/agent/sessions/%d/messages", sid), e.owner, "k-msg-000112",
		map[string]any{"text": "用营销技能", "skillCode": "mk_only"})
	if r.status != http.StatusBadRequest || r.code() != int(errcode.InvalidArgument) {
		t.Fatalf("通用会话不能用仅营销场景的技能：%d %v", r.status, r.body)
	}
}
