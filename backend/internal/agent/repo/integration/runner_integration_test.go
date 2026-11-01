//go:build integration

// 轮次运行器（AGT-22）的真库测试：账本用真库（billing/repo + wiring.AgentLedger），
// 推理模型与成员资格用假实现。
//
// ⚠️ 扣费 / 退款「各只一次」的正确性由账本唯一索引 + FinishTurn 的 CAS 共同保证，
// 只有真库能证明 —— 内存假账本测的是「我以为它会怎样」。
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
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

type itChat struct {
	mu    sync.Mutex
	calls int
	text  string
	err   error
}

func (c *itChat) Chat(_ context.Context, _ agentapp.ChatInput, onDelta func(string)) (agentapp.ChatOutput, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.err != nil {
		return agentapp.ChatOutput{}, c.err
	}
	onDelta(c.text)
	return agentapp.ChatOutput{Text: c.text}, nil
}

type itQuoter struct{ credits int }

func (q itQuoter) QuoteGeneration(context.Context, agentapp.QuoteInput) (agentapp.Quote, error) {
	return agentapp.Quote{Credits: q.credits}, nil
}
func (q itQuoter) QuoteTurn(context.Context, string, int64) (agentapp.Quote, error) {
	return agentapp.Quote{Credits: q.credits}, nil
}

type itMembers struct{ member bool }

func (m itMembers) Role(context.Context, int64, int64) (collab.Role, bool, error) {
	return collab.RoleEditor, m.member, nil
}

type runnerEnv struct {
	r      *repo.Repo
	pool   *pgxpool.Pool
	next   func() int64
	bill   *billingrepo.Repo
	ledger wiring.AgentLedger
	chat   *itChat
}

func newRunnerEnv(t *testing.T, balance int) *runnerEnv {
	t.Helper()
	r, pool, next := setup(t)
	bill := billingrepo.New(pool, next)
	if balance > 0 {
		if _, err := bill.Grant(context.Background(), billingrepo.GrantInput{
			PayerID: payer, Source: "purchase", Amount: balance,
			SourceRefID: fmt.Sprintf("it-%d", next()),
		}); err != nil {
			t.Fatal(err)
		}
	}
	return &runnerEnv{r: r, pool: pool, next: next, bill: bill,
		ledger: wiring.AgentLedger{Repo: bill, Compensate: 30 * 24 * time.Hour},
		chat:   &itChat{text: "好的，我来规划。"}}
}

func (e *runnerEnv) runner(credits int, member bool) *agentapp.Runner {
	return agentapp.NewRunner(agentapp.RunnerDeps{
		Store: agentapp.NewRepoStore(e.r), Chat: e.chat, Quoter: itQuoter{credits},
		Ledger: e.ledger, Members: itMembers{member}, NextID: e.next,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, agentapp.RunnerConfig{MaxCalls: 3, ContextChars: 60000})
}

// seed 建一个会话（指定状态，带推理模型偏好）+ 一条用户消息 + 一个轮次。
func (e *runnerEnv) seed(t *testing.T, state domain.State, trigger domain.TurnTrigger) (repo.Session, repo.Turn) {
	t.Helper()
	ctx := context.Background()
	s, err := e.r.Create(ctx, e.pool, repo.Session{ID: e.next(), WorkspaceID: ws, ProjectID: project,
		PayerAccountID: payer, CreatedByUserID: user, State: state,
		Prefs: json.RawMessage(`{"reasoning":{"model":"m-agent"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := e.r.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.Append(ctx, tx, repo.Message{ID: e.next(), SessionID: s.ID, Role: repo.RoleUser,
		Content: "帮我做一张猫的海报"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tr, err := e.r.InsertTurn(ctx, e.pool, repo.Turn{ID: e.next(), SessionID: s.ID, Trigger: trigger})
	if err != nil {
		t.Fatal(err)
	}
	return s, tr
}

// ledgerCounts 数本轮在账本里的 consume / refund 流水条数与净变动。
func (e *runnerEnv) ledgerCounts(t *testing.T, turnID int64) (consume, refund, net int) {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `
		select type, count(*), coalesce(sum(change), 0) from credit_transaction
		where ref_type = 'agent_turn' and ref_id = $1 group by type`, strconv.FormatInt(turnID, 10))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var typ string
		var n, sum int
		if err := rows.Scan(&typ, &n, &sum); err != nil {
			t.Fatal(err)
		}
		switch typ {
		case "consume":
			consume = n
		case "refund":
			refund = n
		}
		net += sum
	}
	return consume, refund, net
}

func (e *runnerEnv) state(t *testing.T, sessionID, turnID int64) (repo.Session, repo.Turn) {
	t.Helper()
	s, err := e.r.GetByID(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := e.r.GetTurn(context.Background(), turnID)
	if err != nil {
		t.Fatal(err)
	}
	return s, tr
}

func TestRunTurn_扣费与账本一致(t *testing.T) {
	e := newRunnerEnv(t, 100)
	s, tr := e.seed(t, domain.StatePlanning, domain.TriggerUser)
	out, err := e.runner(7, true).RunTurn(context.Background(), s.ID, tr.ID)
	if err != nil || out != agentapp.OutcomeDone {
		t.Fatalf("outcome=%v err=%v", out, err)
	}
	consume, refund, net := e.ledgerCounts(t, tr.ID)
	if consume != 1 || refund != 0 || net != -7 {
		t.Fatalf("应恰好 1 笔 consume(-7)：consume=%d refund=%d net=%d", consume, refund, net)
	}
	s2, tr2 := e.state(t, s.ID, tr.ID)
	if tr2.Status != domain.TurnSuccess || tr2.CreditsCost != 7 || tr2.ModelCode != "m-agent" ||
		tr2.CallCount != 1 || tr2.ValidOutputs != 1 {
		t.Errorf("轮次不对：%+v", tr2)
	}
	if s2.State != domain.StateIdle || s2.RunnerLeaseUntil != nil {
		t.Errorf("会话应回 idle 并释放租约：%+v", s2)
	}
	msgs, _ := e.r.ListRecent(context.Background(), s.ID, 10)
	if len(msgs) != 2 || msgs[1].Role != repo.RoleAssistant || msgs[1].TurnID != tr.ID {
		t.Errorf("应写一条助手消息：%+v", msgs)
	}
}

func TestRunTurn_系统唤醒轮不扣费(t *testing.T) {
	e := newRunnerEnv(t, 100)
	s, tr := e.seed(t, domain.StateExecuting, domain.TriggerSystem)
	// 系统轮即使失败也不能调用退款（没有原消费流水，账本会报 ErrNotFound）。
	e.chat.err = errors.New("上游断开")
	if _, err := e.runner(7, true).RunTurn(context.Background(), s.ID, tr.ID); err == nil {
		t.Fatal("应带回错误")
	}
	consume, refund, _ := e.ledgerCounts(t, tr.ID)
	if consume != 0 || refund != 0 {
		t.Fatalf("系统轮 0 笔流水：consume=%d refund=%d", consume, refund)
	}
	if _, tr2 := e.state(t, s.ID, tr.ID); tr2.Status != domain.TurnFailed || tr2.CreditsCost != 0 {
		t.Errorf("轮次应 failed 且 0 积分：%+v", tr2)
	}
}

func TestRunTurn_无有效输出全额退款且只退一次(t *testing.T) {
	e := newRunnerEnv(t, 100)
	s, tr := e.seed(t, domain.StatePlanning, domain.TriggerUser)
	e.chat.err = errors.New("连接被拒绝")
	rn := e.runner(9, true)
	if _, err := rn.RunTurn(context.Background(), s.ID, tr.ID); err == nil {
		t.Fatal("应带回错误")
	}
	// 同一条消息重投 3 次。
	for i := 0; i < 3; i++ {
		if out, _ := rn.RunTurn(context.Background(), s.ID, tr.ID); out != agentapp.OutcomeDuplicate {
			t.Fatalf("第 %d 次重投应为 duplicate：%v", i+1, out)
		}
	}
	consume, refund, net := e.ledgerCounts(t, tr.ID)
	if consume != 1 || refund != 1 || net != 0 {
		t.Fatalf("消费与退款各 1 笔、净 0：consume=%d refund=%d net=%d", consume, refund, net)
	}
	if e.chat.calls != 1 {
		t.Errorf("重投不得再调模型：%d", e.chat.calls)
	}
	s2, tr2 := e.state(t, s.ID, tr.ID)
	if tr2.Status != domain.TurnFailed || s2.State != domain.StateFailed {
		t.Errorf("轮次与会话应 failed：%s %s", tr2.Status, s2.State)
	}
}

func TestRunTurn_余额不足轮失败会话回idle(t *testing.T) {
	e := newRunnerEnv(t, 3)
	s, tr := e.seed(t, domain.StatePlanning, domain.TriggerUser)
	if _, err := e.runner(5, true).RunTurn(context.Background(), s.ID, tr.ID); err != nil {
		t.Fatal(err)
	}
	if e.chat.calls != 0 {
		t.Fatal("余额不足不得调用模型")
	}
	consume, refund, _ := e.ledgerCounts(t, tr.ID)
	if consume != 0 || refund != 0 {
		t.Fatalf("不应有流水：%d %d", consume, refund)
	}
	s2, tr2 := e.state(t, s.ID, tr.ID)
	if tr2.Status != domain.TurnFailed || tr2.ErrorCode != agentapp.ErrCodeInsufficientCredits {
		t.Errorf("轮次不对：%+v", tr2)
	}
	if s2.State != domain.StateIdle {
		t.Errorf("会话应回 idle：%s", s2.State)
	}
}

func TestRunTurn_同一轮重投只扣一次(t *testing.T) {
	e := newRunnerEnv(t, 100)
	s, tr := e.seed(t, domain.StatePlanning, domain.TriggerUser)
	// 模拟上一个 Worker 扣完费就崩了（还没来得及 MarkTurnRunning）。
	if err := e.ledger.Consume(context.Background(), payer, user, 4, agentapp.RefTypeTurn,
		strconv.FormatInt(tr.ID, 10), "Agent 对话"); err != nil {
		t.Fatal(err)
	}
	rn := e.runner(4, true)
	if out, err := rn.RunTurn(context.Background(), s.ID, tr.ID); err != nil || out != agentapp.OutcomeDone {
		t.Fatalf("重投应接着跑完：%v %v", out, err)
	}
	if out, _ := rn.RunTurn(context.Background(), s.ID, tr.ID); out != agentapp.OutcomeDuplicate {
		t.Fatalf("跑完后再投应为 duplicate：%v", out)
	}
	consume, refund, net := e.ledgerCounts(t, tr.ID)
	if consume != 1 || refund != 0 || net != -4 {
		t.Fatalf("只扣一次：consume=%d refund=%d net=%d", consume, refund, net)
	}
	if _, tr2 := e.state(t, s.ID, tr.ID); tr2.Status != domain.TurnSuccess {
		t.Errorf("轮次应 success：%+v", tr2)
	}
}

func TestRunTurn_租约被占时丢弃(t *testing.T) {
	e := newRunnerEnv(t, 100)
	s, tr := e.seed(t, domain.StatePlanning, domain.TriggerUser)
	if ok, err := e.r.ClaimLease(context.Background(), s.ID, "other-worker", time.Minute); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if out, _ := e.runner(4, true).RunTurn(context.Background(), s.ID, tr.ID); out != agentapp.OutcomeBusy {
		t.Fatalf("应为 busy：%v", out)
	}
	consume, _, _ := e.ledgerCounts(t, tr.ID)
	s2, tr2 := e.state(t, s.ID, tr.ID)
	if consume != 0 || e.chat.calls != 0 || tr2.Status != domain.TurnPending || s2.RunnerID != "other-worker" {
		t.Errorf("租约被占时不得做任何事：consume=%d calls=%d turn=%s runner=%s",
			consume, e.chat.calls, tr2.Status, s2.RunnerID)
	}
}

func TestRunTurn_发起人被移出会话取消(t *testing.T) {
	e := newRunnerEnv(t, 100)
	s, tr := e.seed(t, domain.StatePlanning, domain.TriggerUser)
	if _, err := e.runner(4, false).RunTurn(context.Background(), s.ID, tr.ID); err != nil {
		t.Fatal(err)
	}
	consume, _, _ := e.ledgerCounts(t, tr.ID)
	s2, tr2 := e.state(t, s.ID, tr.ID)
	if s2.State != domain.StateCancelled || tr2.Status != domain.TurnCancelled || consume != 0 {
		t.Errorf("会话应取消、轮次取消且不扣费：%s %s consume=%d", s2.State, tr2.Status, consume)
	}
}
