//go:build integration

// 付费调用外壳（AGT-24，app/budget.go PaidCall）的真库测试。
//
// ⚠️ 额度检查与预留在同一个锁住会话行的事务里 —— 只有真库多 goroutine 抢同一个会话
// 才能证明它不超额：顺序调用永远先看到「已用」再插入，把 for update 删掉照样绿。
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
)

// paidSession 建一个执行态、批准额度为 approved 的会话。
func paidSession(t *testing.T, r *repo.Repo, pool *pgxpool.Pool, next func() int64, approved int) repo.Session {
	t.Helper()
	s, err := r.Create(context.Background(), pool, repo.Session{ID: next(), WorkspaceID: ws, ProjectID: project,
		PayerAccountID: payer, CreatedByUserID: user, State: domain.StateExecuting, ApprovedBudget: approved})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func paidRC(s repo.Session, stepID int64, attempt int) agentapp.RunContext {
	return agentapp.RunContext{WorkspaceID: s.WorkspaceID, ProjectID: s.ProjectID, SessionID: s.ID,
		StepID: stepID, Attempt: attempt, UserID: s.CreatedByUserID, PayerAccountID: s.PayerAccountID,
		State: domain.StateExecuting}
}

// occupied 是 reserved + charged 的报价合计（额度判定口径）。
func occupied(t *testing.T, pool *pgxpool.Pool, sessionID int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `select coalesce(sum(quoted),0)::int from agent_budget_entry
		where session_id = $1 and status in ('reserved','charged')`, sessionID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPaidCall_8并发不超额度(t *testing.T) {
	r, pool, next := setup(t)
	s := paidSession(t, r, pool, next, 30)
	b := agentapp.NewBudget(r, next, nil)

	var (
		wg       sync.WaitGroup
		ran      atomic.Int32
		suspends atomic.Int32
		errs     atomic.Int32
	)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, susp, err := b.PaidCall(context.Background(), paidRC(s, next(), 1), 10,
				func(string) (agentapp.PaidResult, error) {
					ran.Add(1)
					return agentapp.PaidResult{GenerationID: next(), Credits: 10}, nil
				})
			switch {
			case err != nil:
				errs.Add(1)
				t.Errorf("不该报错：%v", err)
			case susp != nil:
				if susp.Event != domain.EvOverBudget || susp.CardID == 0 {
					t.Errorf("挂起应为追加额度且带卡片：%+v", susp)
				}
				suspends.Add(1)
			}
		}()
	}
	wg.Wait()
	if ran.Load() != 3 || suspends.Load() != 5 || errs.Load() != 0 {
		t.Fatalf("应恰好 3 次进入 run、5 次挂起：run=%d suspend=%d err=%d", ran.Load(), suspends.Load(), errs.Load())
	}
	if got := occupied(t, pool, s.ID); got > 30 || got != 30 {
		t.Fatalf("占用应为 30 且不超额度，实际 %d", got)
	}
	var cards int
	_ = pool.QueryRow(context.Background(), `select count(*) from agent_card where session_id = $1 and kind = 'budget'
		and status = 'pending'`, s.ID).Scan(&cards)
	if cards != 5 {
		t.Errorf("每次挂起写一张追加额度卡，应 5 张，实际 %d", cards)
	}
}

func TestPaidCall_受理失败释放额度(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	s := paidSession(t, r, pool, next, 10)
	b := agentapp.NewBudget(r, next, nil)

	step1 := next()
	boom := errors.New("上游拒绝")
	_, susp, err := b.PaidCall(ctx, paidRC(s, step1, 1), 10, func(string) (agentapp.PaidResult, error) {
		return agentapp.PaidResult{}, boom
	})
	if !errors.Is(err, boom) || susp != nil {
		t.Fatalf("run 的错误应原样返回：susp=%v err=%v", susp, err)
	}
	e, err := r.BudgetByStep(ctx, step1)
	if err != nil || e.Status != repo.BudgetReleased {
		t.Fatalf("失败后条目应 released：%+v %v", e, err)
	}
	if occupied(t, pool, s.ID) != 0 {
		t.Fatal("released 不应再占额度")
	}
	// 释放的额度能被随后的调用用回来。
	ran := false
	_, susp, err = b.PaidCall(ctx, paidRC(s, next(), 1), 10, func(string) (agentapp.PaidResult, error) {
		ran = true
		return agentapp.PaidResult{GenerationID: 77, Credits: 10}, nil
	})
	if err != nil || susp != nil || !ran {
		t.Fatalf("释放后应能用回这部分额度：ran=%v susp=%v err=%v", ran, susp, err)
	}
}

func TestPaidCall_超额度写追加卡并挂起(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	s := paidSession(t, r, pool, next, 10)
	b := agentapp.NewBudget(r, next, nil)
	if _, _, err := b.PaidCall(ctx, paidRC(s, next(), 1), 8, func(string) (agentapp.PaidResult, error) {
		return agentapp.PaidResult{GenerationID: 1, Credits: 8}, nil
	}); err != nil {
		t.Fatal(err)
	}

	step := next()
	ran := false
	_, susp, err := b.PaidCallTool(ctx, paidRC(s, step, 1), "generate", 5, func(string) (agentapp.PaidResult, error) {
		ran = true
		return agentapp.PaidResult{}, nil
	})
	if err != nil || susp == nil || ran {
		t.Fatalf("超额度应挂起且不调用 run：susp=%v err=%v ran=%v", susp, err, ran)
	}
	if susp.Event != domain.EvOverBudget {
		t.Errorf("挂起事件应为 over_budget：%s", susp.Event)
	}
	if _, err := r.BudgetByStep(ctx, step); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("超额度不应写额度条目：%v", err)
	}
	card, err := r.GetCard(ctx, s.ID, susp.CardID)
	if err != nil || card.Kind != domain.CardBudget || card.StepID != step || card.Status != domain.CardPending {
		t.Fatalf("应写一张 pending 的追加额度卡：%+v %v", card, err)
	}
	var p agentapp.BudgetCardPayload
	if err := json.Unmarshal(card.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Needed != 5 || p.Used != 8 || p.Approved != 10 || p.Shortfall != 3 || p.Requested != 3 ||
		p.ResumeState != domain.StateExecuting || p.Tool != "generate" || p.Reason == "" {
		t.Errorf("卡片内容不对：%+v", p)
	}
	// 批准语义（AGT-26）：approved += requested → 恰好够这一步。
	if dec, _ := domain.CheckBudget(p.Approved+p.Requested, p.Used, p.Needed); !dec.Allow {
		t.Error("按卡片批准后应恰好放行这一步")
	}
}

func TestPaidCall_额度恰好用尽放行(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	s := paidSession(t, r, pool, next, 20)
	b := agentapp.NewBudget(r, next, nil)
	for i := 0; i < 2; i++ {
		_, susp, err := b.PaidCall(ctx, paidRC(s, next(), 1), 10, func(string) (agentapp.PaidResult, error) {
			return agentapp.PaidResult{GenerationID: next(), Credits: 10}, nil
		})
		if err != nil || susp != nil {
			t.Fatalf("第 %d 次应放行：susp=%v err=%v", i+1, susp, err)
		}
	}
	if occupied(t, pool, s.ID) != 20 {
		t.Fatal("恰好用尽")
	}
	// 0 积分的调用在用尽时也放行（判断只有一处，不在调用方特判）。
	if _, susp, err := b.PaidCall(ctx, paidRC(s, next(), 1), 0, func(string) (agentapp.PaidResult, error) {
		return agentapp.PaidResult{}, nil
	}); err != nil || susp != nil {
		t.Fatalf("0 积分应放行：%v %v", susp, err)
	}
	// 再多 1 分就挂起。
	if _, susp, _ := b.PaidCall(ctx, paidRC(s, next(), 1), 1, func(string) (agentapp.PaidResult, error) {
		return agentapp.PaidResult{}, nil
	}); susp == nil {
		t.Fatal("超 1 分应挂起")
	}
}

func TestPaidCall_同一步骤重试换幂等键并复用已归还条目(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	s := paidSession(t, r, pool, next, 10)
	b := agentapp.NewBudget(r, next, nil)
	step := next()

	var keys []string
	_, _, err := b.PaidCall(ctx, paidRC(s, step, 1), 10, func(k string) (agentapp.PaidResult, error) {
		keys = append(keys, k)
		return agentapp.PaidResult{}, errors.New("失败")
	})
	if err == nil {
		t.Fatal("应失败")
	}
	_, _, err = b.PaidCall(ctx, paidRC(s, step, 2), 10, func(k string) (agentapp.PaidResult, error) {
		keys = append(keys, k)
		return agentapp.PaidResult{GenerationID: 99, Credits: 9}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] == keys[1] || keys[1] != domain.PaidIdempotencyKey(s.ID, step, 2) {
		t.Fatalf("幂等键应随 attempt 变：%v", keys)
	}
	e, _ := r.BudgetByStep(ctx, step)
	if e.Status != repo.BudgetCharged || e.GenerationID != 99 || e.Quoted != 9 {
		t.Fatalf("条目应 charged、挂上任务并按实扣校正：%+v", e)
	}
	// 已 charged 的步骤不能再占一份额度、也不再执行：回那一次的引用（AGT-50 起由拒绝改为 Replayed，
	// 整图运行 / 渲染这种没有自身幂等键的能力靠它防重复）。
	again, susp, err := b.PaidCall(ctx, paidRC(s, step, 3), 1, func(string) (agentapp.PaidResult, error) {
		t.Fatal("不该调用 run")
		return agentapp.PaidResult{}, nil
	})
	if err != nil || susp != nil || !again.Replayed || again.GenerationID != 99 {
		t.Fatalf("同一步骤已受理成功时应回那一次（不执行、不占额度）：%+v susp=%v err=%v", again, susp, err)
	}
	if n := occupied(t, pool, s.ID); n != 9 {
		t.Fatalf("重入不应再占额度，占用 %d", n)
	}
}

func TestPaidCall_非执行态拒绝(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	s := paidSession(t, r, pool, next, 100)
	if ok, err := r.TransitionState(ctx, pool, s.ID, domain.StateExecuting, domain.StateCancelled); err != nil || !ok {
		t.Fatal(ok, err)
	}
	_, _, err := agentapp.NewBudget(r, next, nil).PaidCall(ctx, paidRC(s, next(), 1), 1,
		func(string) (agentapp.PaidResult, error) {
			t.Fatal("不该调用 run")
			return agentapp.PaidResult{}, nil
		})
	if !errors.Is(err, agentapp.ErrNotExecuting) {
		t.Fatalf("会话已取消时应拒绝：%v", err)
	}
	if occupied(t, pool, s.ID) != 0 {
		t.Fatal("不应写额度条目")
	}
}
