package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
)

// ---------------------------------------------------------------- 内存版 RunnerStore

type fakeStore struct {
	mu       sync.Mutex
	sessions map[int64]*repo.Session
	turns    map[int64]*repo.Turn
	msgs     []repo.Message
	steps    map[int64]*repo.Step
	skills   map[string]repo.Skill
	// onGetSession 每次 GetSession 时回调（测试用来在某个时刻设置停止请求）。
	onGetSession func(s *repo.Session)
	// answered 已回答的问答卡（最新的在前；视频规格三问从这里取回答）。
	answered []repo.Card
}

func newFakeStore() *fakeStore {
	return &fakeStore{sessions: map[int64]*repo.Session{}, turns: map[int64]*repo.Turn{},
		steps: map[int64]*repo.Step{}, skills: map[string]repo.Skill{}}
}

func (f *fakeStore) AnsweredQuestionCards(context.Context, int64, int) ([]repo.Card, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]repo.Card(nil), f.answered...), nil
}

func (f *fakeStore) GetSession(_ context.Context, id int64) (repo.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[id]
	if !ok {
		return repo.Session{}, repo.ErrNotFound
	}
	if f.onGetSession != nil {
		f.onGetSession(s)
	}
	return *s, nil
}

func (f *fakeStore) GetTurn(_ context.Context, id int64) (repo.Turn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.turns[id]
	if !ok {
		return repo.Turn{}, repo.ErrNotFound
	}
	return *t, nil
}

func (f *fakeStore) ClaimLease(_ context.Context, id int64, runner string, ttl time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sessions[id]
	if s.RunnerLeaseUntil != nil && s.RunnerLeaseUntil.After(time.Now()) {
		return false, nil
	}
	u := time.Now().Add(ttl)
	s.RunnerID, s.RunnerLeaseUntil = runner, &u
	return true, nil
}

func (f *fakeStore) RenewLease(_ context.Context, id int64, runner string, ttl time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sessions[id]
	if s.RunnerID != runner || s.RunnerLeaseUntil == nil {
		return false, nil
	}
	u := time.Now().Add(ttl)
	s.RunnerLeaseUntil = &u
	return true, nil
}

func (f *fakeStore) ReleaseLease(_ context.Context, id int64, runner string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sessions[id]
	if s.RunnerID != runner {
		return false, nil
	}
	s.RunnerID, s.RunnerLeaseUntil = "", nil
	return true, nil
}

func (f *fakeStore) SetTurnQuote(_ context.Context, id int64, model string, credits int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.turns[id]
	if t.Status != domain.TurnPending {
		return false, nil
	}
	t.ModelCode, t.CreditsCost = model, credits
	return true, nil
}

func (f *fakeStore) MarkTurnRunning(_ context.Context, id int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.turns[id]
	if t.Status != domain.TurnPending {
		return false, nil
	}
	t.Status = domain.TurnRunning
	return true, nil
}

func (f *fakeStore) FinishTurn(_ context.Context, id int64, status domain.TurnStatus, calls, valid int,
	code, msg string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.turns[id]
	if t.Status.IsTerminal() {
		return false, nil
	}
	t.Status, t.CallCount, t.ValidOutputs, t.ErrorCode, t.ErrorMessage = status, calls, valid, code, msg
	return true, nil
}

func (f *fakeStore) TransitionState(_ context.Context, id int64, from, to domain.State) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sessions[id]
	if s.State != from {
		return false, nil
	}
	s.State = to
	// 同 repo.TransitionState：进入「有轮在跑」时清掉上一次的停止请求。
	if domain.HasActiveTurn(to) && !domain.HasActiveTurn(from) {
		s.StopRequestedAt = nil
	}
	return true, nil
}

func (f *fakeStore) AppendMessage(_ context.Context, m repo.Message) (repo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m.Seq = len(f.msgs) + 1
	if m.Refs == nil {
		m.Refs = json.RawMessage(`[]`)
	}
	f.msgs = append(f.msgs, m)
	return m, nil
}

func (f *fakeStore) RecentMessages(_ context.Context, sessionID int64, limit int) ([]repo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []repo.Message
	for _, m := range f.msgs {
		if m.SessionID == sessionID {
			out = append(out, m)
		}
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

func (f *fakeStore) InsertStep(_ context.Context, s repo.Step) (repo.Step, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s.Status = repo.StepRunning
	f.steps[s.ID] = &s
	return s, nil
}

func (f *fakeStore) FinishStep(_ context.Context, id int64, status string, display json.RawMessage,
	errMsg string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.steps[id]
	if s == nil || (s.Status != repo.StepRunning && s.Status != repo.StepWaiting) {
		return false, nil
	}
	s.Status, s.ErrorMessage = status, errMsg
	if display != nil {
		s.Display = display
	}
	return true, nil
}

func (f *fakeStore) SetStepWaiting(_ context.Context, id, gen int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.steps[id]
	if s == nil || s.Status != repo.StepRunning {
		return false, nil
	}
	s.Status, s.GenerationID = repo.StepWaiting, gen
	return true, nil
}

func (f *fakeStore) TagStep(_ context.Context, id int64, planItemKey, errorCode string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.steps[id]; s != nil {
		if planItemKey != "" {
			s.PlanItemKey = planItemKey
		}
		if errorCode != "" {
			s.ErrorCode = errorCode
		}
	}
	return nil
}

func (f *fakeStore) HasWaitingSteps(_ context.Context, sessionID int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.steps {
		if s.SessionID == sessionID && s.Status == repo.StepWaiting {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeStore) UpdateSummary(_ context.Context, id int64, summary string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[id].Summary = summary
	return nil
}

func (f *fakeStore) SkillByCode(_ context.Context, code string) (repo.Skill, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.skills[code]
	if !ok {
		return repo.Skill{}, repo.ErrNotFound
	}
	return s, nil
}

func (f *fakeStore) stepsByStatus(status string) []repo.Step {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []repo.Step
	for _, s := range f.steps {
		if s.Status == status {
			out = append(out, *s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (f *fakeStore) messagesByRole(role string) []repo.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []repo.Message
	for _, m := range f.msgs {
		if m.Role == role {
			out = append(out, m)
		}
	}
	return out
}

// ---------------------------------------------------------------- 假推理模型

// fakeChat 按脚本依次回复；每次调用都记下收到的 ChatInput（断言请求入参）。
type fakeChat struct {
	mu      sync.Mutex
	inputs  []ChatInput
	replies []fakeReply
	// fallback 脚本用完之后的回复（缺省：一段纯文本）。
	fallback *fakeReply
}

type fakeReply struct {
	deltas []string
	out    ChatOutput
	err    error
	// beforeReturn 在返回前回调（测试用来模拟流式中途发生的事）。
	beforeReturn func()
	// waitCtx 为真时：吐完 delta 后阻塞到 ctx 取消（模拟被停止中断的长流）。
	waitCtx bool
}

func (c *fakeChat) Chat(ctx context.Context, in ChatInput, onDelta func(string)) (ChatOutput, error) {
	c.mu.Lock()
	c.inputs = append(c.inputs, in)
	var r fakeReply
	switch {
	case len(c.replies) > 0:
		r, c.replies = c.replies[0], c.replies[1:]
	case c.fallback != nil:
		r = *c.fallback
	default:
		r = fakeReply{out: ChatOutput{Text: "好的"}}
	}
	c.mu.Unlock()
	for _, d := range r.deltas {
		onDelta(d)
	}
	if r.beforeReturn != nil {
		r.beforeReturn()
	}
	if r.waitCtx {
		select {
		case <-ctx.Done():
			return ChatOutput{}, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return r.out, r.err
}

func (c *fakeChat) calls() []ChatInput {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ChatInput(nil), c.inputs...)
}

// ---------------------------------------------------------------- 假账本与其他端口

type fakeLedger struct {
	mu       sync.Mutex
	balance  int
	consumed map[string]int
	refunded map[string]int
}

func newFakeLedger(balance int) *fakeLedger {
	return &fakeLedger{balance: balance, consumed: map[string]int{}, refunded: map[string]int{}}
}

func (l *fakeLedger) Consume(_ context.Context, _, _ int64, amount int, refType, refID, _ string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := refType + ":" + refID
	if _, ok := l.consumed[k]; ok {
		return ErrLedgerDuplicate
	}
	if l.balance < amount {
		return ErrInsufficientCredits
	}
	l.balance -= amount
	l.consumed[k] = amount
	return nil
}

func (l *fakeLedger) Refund(_ context.Context, _ int64, refType, refID, _ string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := refType + ":" + refID
	amt, ok := l.consumed[k]
	if !ok {
		return errors.New("找不到原消费流水")
	}
	if _, done := l.refunded[k]; done {
		return nil
	}
	l.refunded[k] = amt
	l.balance += amt
	return nil
}

type fakeQuoter struct{ credits int }

func (q fakeQuoter) QuoteGeneration(context.Context, QuoteInput) (Quote, error) {
	return Quote{Credits: q.credits}, nil
}

func (q fakeQuoter) QuoteTurn(context.Context, string, int64) (Quote, error) {
	return Quote{Credits: q.credits}, nil
}

type fakeMembers struct {
	mu     sync.Mutex
	role   collab.Role
	member bool
}

func (m *fakeMembers) Role(context.Context, int64, int64) (collab.Role, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.role, m.member, nil
}

type fakeEvents struct {
	mu     sync.Mutex
	events []any
}

func (e *fakeEvents) Publish(_ context.Context, _ string, payload any) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, payload)
	return nil
}

// fakeTool 是一个可配置的工具。
type fakeTool struct {
	name   domain.ToolName
	result ToolResult
	err    error
	runs   int
}

func (t *fakeTool) Name() domain.ToolName   { return t.name }
func (t *fakeTool) Description() string     { return "测试工具 " + string(t.name) }
func (t *fakeTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *fakeTool) Paid() bool              { s, _ := domain.SpecOf(t.name); return s.Paid }
func (t *fakeTool) Action() collab.Action {
	s, _ := domain.SpecOf(t.name)
	return collab.Action(s.Action)
}
func (t *fakeTool) Run(context.Context, RunContext, json.RawMessage) (ToolResult, error) {
	t.runs++
	return t.result, t.err
}

// fakeToolSet 实现 ToolSet（app 包不能 import tools，否则成环）。
type fakeToolSet map[domain.ToolName]Tool

func (s fakeToolSet) Lookup(n domain.ToolName) (Tool, bool) { t, ok := s[n]; return t, ok }
func (s fakeToolSet) Schemas(names []domain.ToolName) []ToolDef {
	var out []ToolDef
	for _, n := range names {
		if t, ok := s[n]; ok {
			out = append(out, ToolDef{Name: string(n), Description: t.Description(), Parameters: t.Schema()})
		}
	}
	return out
}

// allTools 注册第一期全部工具（都返回成功）。
func allTools() fakeToolSet {
	set := fakeToolSet{}
	for _, s := range domain.Tools() {
		set[s.Name] = &fakeTool{name: s.Name, result: ToolResult{Content: json.RawMessage(`{"ok":true}`)}}
	}
	return set
}

// ---------------------------------------------------------------- 组装

type harness struct {
	store   *fakeStore
	chat    *fakeChat
	ledger  *fakeLedger
	members *fakeMembers
	events  *fakeEvents
	tools   fakeToolSet
	runner  *Runner
	nextID  int64
}

const (
	hSession = int64(100)
	hTurn    = int64(200)
)

func newHarness(state domain.State, trigger domain.TurnTrigger, credits, balance int) *harness {
	h := &harness{store: newFakeStore(), chat: &fakeChat{}, ledger: newFakeLedger(balance),
		members: &fakeMembers{role: collab.RoleEditor, member: true}, events: &fakeEvents{},
		tools: allTools(), nextID: 1000}
	created := time.Now().Add(-time.Second)
	h.store.sessions[hSession] = &repo.Session{ID: hSession, WorkspaceID: 1, ProjectID: 2,
		PayerAccountID: 3, CreatedByUserID: 4, State: state, Prefs: json.RawMessage(`{"reasoning":{"model":"m1"}}`),
		SkillCodes: json.RawMessage(`[]`)}
	h.store.turns[hTurn] = &repo.Turn{ID: hTurn, SessionID: hSession, Trigger: trigger,
		Status: domain.TurnPending, CreatedAt: created}
	h.store.msgs = append(h.store.msgs, repo.Message{ID: 1, SessionID: hSession, Seq: 1, Role: repo.RoleUser,
		Content: "帮我做一个关于猫的短片", Refs: json.RawMessage(`[]`)})
	h.runner = NewRunner(RunnerDeps{
		Store: h.store, Chat: h.chat, Quoter: fakeQuoter{credits: credits}, Ledger: h.ledger,
		Members: h.members, Events: h.events, Tools: h.tools,
		NextID: func() int64 { h.nextID++; return h.nextID },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, RunnerConfig{MaxCalls: 5, ContextChars: 60000})
	return h
}

func (h *harness) run() (Outcome, error) {
	return h.runner.RunTurn(context.Background(), hSession, hTurn)
}

func (h *harness) session() repo.Session {
	s, _ := h.store.GetSession(context.Background(), hSession)
	return s
}
func (h *harness) turn() repo.Turn { t, _ := h.store.GetTurn(context.Background(), hTurn); return t }

func toolCall(id, name, args string) ToolCall {
	tc := ToolCall{ID: id, Name: name}
	if json.Valid([]byte(args)) {
		tc.Arguments = json.RawMessage(args)
	} else {
		tc.RawArguments = args
	}
	return tc
}
