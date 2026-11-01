//go:build integration

// Agent 数据层（AGT-21）的真库测试。
//
// ⚠️ 必须用真库、真并发：租约、seq 连续、卡片提交、额度预留的正确性全由 SQL 决定 ——
// 顺序调用永远先撞上状态检查，那种测试把 CAS 条件整条删掉照样绿（CLAUDE.md §5.7）。
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	genrepo "github.com/li904338203/tabtv/backend/internal/generation/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
)

const (
	ws      = int64(9101)
	project = int64(9201)
	payer   = int64(9301)
	user    = int64(9401)
)

func setup(t *testing.T) (*repo.Repo, *pgxpool.Pool, func() int64) {
	t.Helper()
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	return repo.New(pool), pool, gen.Next
}

func mustSession(t *testing.T, r *repo.Repo, pool *pgxpool.Pool, next func() int64) repo.Session {
	t.Helper()
	s, err := r.Create(context.Background(), pool, repo.Session{
		ID: next(), WorkspaceID: ws, ProjectID: project, PayerAccountID: payer, CreatedByUserID: user,
		Title: "测试会话",
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mustTurn(t *testing.T, r *repo.Repo, pool *pgxpool.Pool, next func() int64, sessionID int64) repo.Turn {
	t.Helper()
	tr, err := r.InsertTurn(context.Background(), pool, repo.Turn{
		ID: next(), SessionID: sessionID, Trigger: domain.TriggerUser})
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// ---------------------------------------------------------------- 会话

func TestSession_幂等键与保留未知字段(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	in := repo.Session{ID: next(), WorkspaceID: ws, ProjectID: project, PayerAccountID: payer,
		CreatedByUserID: user, CreateIdempotencyKey: "k-1",
		Prefs: json.RawMessage(`{"video":{"model":"m"},"future":{"x":1}}`)}
	s, err := r.Create(ctx, pool, in)
	if err != nil {
		t.Fatal(err)
	}
	if s.State != domain.StateIdle || s.ApprovedBudget != 0 {
		t.Errorf("默认值不对：%+v", s)
	}
	var prefs map[string]any
	_ = json.Unmarshal(s.Prefs, &prefs)
	if _, ok := prefs["future"]; !ok {
		t.Error("未知字段应原样保留")
	}
	in.ID = next()
	if _, err := r.Create(ctx, pool, in); !errors.Is(err, repo.ErrDuplicate) {
		t.Fatalf("同键第二次应 ErrDuplicate，得到 %v", err)
	}
	got, err := r.GetByCreateKey(ctx, ws, "k-1")
	if err != nil || got.ID != s.ID {
		t.Errorf("按幂等键应取回原会话：%v %+v", err, got)
	}
	// 不带键的不互相撞。
	for i := 0; i < 2; i++ {
		mustSession(t, r, pool, next)
	}
	// 越权：别的工作区取不到。
	if _, err := r.Get(ctx, ws+1, s.ID); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("跨工作区应 ErrNotFound，得到 %v", err)
	}
	list, err := r.ListByProject(ctx, ws, project, 10)
	if err != nil || len(list) != 3 {
		t.Errorf("项目下应有 3 个会话：%v %d", err, len(list))
	}
}

func TestClaimLease_并发只有一个成功(t *testing.T) {
	r, pool, next := setup(t)
	s := mustSession(t, r, pool, next)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := r.ClaimLease(context.Background(), s.ID, "runner-"+string(rune('a'+i)), 90*time.Second)
			if err != nil {
				t.Error(err)
			}
			if ok {
				wins.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("8 个并发 ClaimLease 应恰好 1 个成功，得到 %d", wins.Load())
	}
}

func TestClaimLease_租约过期可被抢占(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	s := mustSession(t, r, pool, next)
	if ok, _ := r.ClaimLease(ctx, s.ID, "A", 90*time.Second); !ok {
		t.Fatal("首次应取得")
	}
	if ok, _ := r.ClaimLease(ctx, s.ID, "B", 90*time.Second); ok {
		t.Fatal("租约有效期内别人不应取得")
	}
	// 模拟 A 崩溃：把租约拨到过去（不用 sleep 卡时间，§2.19）。
	if _, err := pool.Exec(ctx, `update agent_session set runner_lease_until = now() - interval '1 second'
		where id = $1`, s.ID); err != nil {
		t.Fatal(err)
	}
	if ok, _ := r.ClaimLease(ctx, s.ID, "B", 90*time.Second); !ok {
		t.Fatal("过期后应可被抢占")
	}
	// A 醒来续租失败（持有者已换），释放也不生效。
	if ok, _ := r.RenewLease(ctx, s.ID, "A", 90*time.Second); ok {
		t.Error("租约被抢走后原持有者不能续")
	}
	if ok, _ := r.ReleaseLease(ctx, s.ID, "A"); ok {
		t.Error("原持有者不能释放别人的租约")
	}
	if ok, _ := r.RenewLease(ctx, s.ID, "B", 90*time.Second); !ok {
		t.Error("当前持有者应能续")
	}
	if ok, _ := r.ReleaseLease(ctx, s.ID, "B"); !ok {
		t.Error("当前持有者应能释放")
	}
	if ok, _ := r.ClaimLease(ctx, s.ID, "C", 90*time.Second); !ok {
		t.Error("释放后应可再取")
	}
}

func TestTransitionState_CAS防止重复迁移(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	s := mustSession(t, r, pool, next)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := r.TransitionState(ctx, pool, s.ID, domain.StateIdle, domain.StatePlanning)
			if err != nil {
				t.Error(err)
			}
			if ok {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("同一迁移应只成功一次，得到 %d", wins.Load())
	}
	got, _ := r.Get(ctx, ws, s.ID)
	if got.State != domain.StatePlanning {
		t.Errorf("状态应为 planning，得到 %s", got.State)
	}
}

// 04 文档 P-06 / P-18：停止请求只针对当时在跑的那一轮。会话重新进入「有轮在跑」时清掉，
// 否则详情的 stopRequested 恒为 true（前端停止键永久不可用），而排队的下一轮一开始就被停掉。
func TestTransitionState_进入运行态清掉上一次停止请求(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	s := mustSession(t, r, pool, next)
	stopOf := func() *time.Time {
		got, err := r.Get(ctx, ws, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.StopRequestedAt
	}
	move := func(from, to domain.State) {
		t.Helper()
		if ok, err := r.TransitionState(ctx, pool, s.ID, from, to); err != nil || !ok {
			t.Fatalf("%s → %s 迁移失败：%v %v", from, to, ok, err)
		}
	}
	move(domain.StateIdle, domain.StatePlanning)
	if ok, _ := r.RequestStop(ctx, s.ID); !ok {
		t.Fatal("应能记停止请求")
	}
	move(domain.StatePlanning, domain.StateExecuting) // 运行中迁移：停止仍针对这一轮
	if stopOf() == nil {
		t.Fatal("运行中迁移不该清掉停止请求")
	}
	move(domain.StateExecuting, domain.StateIdle) // 这一轮结束
	if stopOf() == nil {
		t.Fatal("离开运行态时不动停止请求")
	}
	move(domain.StateIdle, domain.StatePlanning) // 新的一轮开始
	if stopOf() != nil {
		t.Fatal("进入运行态时应清掉上一次的停止请求")
	}
	// waiting_task → executing（任务结束唤醒 / 追问）同样是新的一轮开始。
	move(domain.StatePlanning, domain.StateExecuting)
	move(domain.StateExecuting, domain.StateWaitingTask)
	if ok, _ := r.RequestStop(ctx, s.ID); !ok {
		t.Fatal("应能记停止请求")
	}
	move(domain.StateWaitingTask, domain.StateExecuting)
	if stopOf() != nil {
		t.Fatal("从 waiting_task 进入执行时应清掉停止请求")
	}
}

func TestSession_额度停止与杂项更新(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	s := mustSession(t, r, pool, next)

	tx, err := r.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.LockForUpdate(ctx, tx, s.ID); err != nil {
		t.Fatal(err)
	}
	v := 3
	if err := r.SetApprovedBudget(ctx, tx, s.ID, 60, &v); err != nil {
		t.Fatal(err)
	}
	total, err := r.AddApprovedBudget(ctx, tx, s.ID, 20)
	if err != nil || total != 80 {
		t.Fatalf("追加后额度应为 80：%v %d", err, total)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, _ := r.RequestStop(ctx, s.ID); !ok {
		t.Error("应能记停止请求")
	}
	if err := r.UpdateTodo(ctx, s.ID, json.RawMessage(`[{"t":"写规划","done":true}]`)); err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateSkills(ctx, s.ID, json.RawMessage(`["canvas"]`)); err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateSummary(ctx, s.ID, "早先聊了选题"); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Get(ctx, ws, s.ID)
	if got.ApprovedBudget != 80 || got.PlanDocVersion == nil || *got.PlanDocVersion != 3 ||
		got.StopRequestedAt == nil || got.Summary != "早先聊了选题" {
		t.Errorf("更新未生效：%+v", got)
	}
	if err := r.ClearStop(ctx, pool, s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.TransitionState(ctx, pool, s.ID, domain.StateIdle, domain.StateCancelled); err != nil {
		t.Fatal(err)
	}
	if ok, _ := r.RequestStop(ctx, s.ID); ok {
		t.Error("已取消的会话不应再记停止请求")
	}
	if err := r.UpdateTodo(ctx, next(), nil); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("不存在的会话应 ErrNotFound，得到 %v", err)
	}
}

// ---------------------------------------------------------------- 消息

func TestAppendMessage_并发seq连续(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	s := mustSession(t, r, pool, next)
	const n = 20
	ids := make([]int64, n)
	for i := range ids {
		ids[i] = next()
	}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tx, err := r.Begin(ctx)
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := r.Append(ctx, tx, repo.Message{ID: ids[i], SessionID: s.ID,
				Role: repo.RoleUser, Content: "hi"}); err != nil {
				t.Error(err)
				return
			}
			if err := tx.Commit(ctx); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	msgs, err := r.ListRecent(ctx, s.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != n {
		t.Fatalf("应有 %d 条消息，得到 %d", n, len(msgs))
	}
	for i, m := range msgs {
		if m.Seq != i+1 {
			t.Fatalf("seq 应为 1..%d 连续，第 %d 条是 %d", n, i, m.Seq)
		}
	}
}

func TestListPage_按seq游标翻页不漏不重(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	s := mustSession(t, r, pool, next)
	turn := mustTurn(t, r, pool, next, s.ID)
	const n = 23
	for i := 0; i < n; i++ {
		tx, _ := r.Begin(ctx)
		m := repo.Message{ID: next(), SessionID: s.ID, TurnID: turn.ID, Role: repo.RoleAssistant}
		if i%5 == 0 {
			m.ToolCalls = json.RawMessage(`[{"id":"c1","name":"read_canvas"}]`)
		}
		if _, err := r.Append(ctx, tx, m); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var seen []int
	cursor := 0
	for page := 0; page < 10; page++ {
		msgs, nextCursor, err := r.ListPage(ctx, s.ID, cursor, 5)
		if err != nil {
			t.Fatal(err)
		}
		for i := 1; i < len(msgs); i++ {
			if msgs[i].Seq <= msgs[i-1].Seq {
				t.Fatal("页内应按 seq 升序")
			}
		}
		for _, m := range msgs {
			seen = append(seen, m.Seq)
		}
		if nextCursor == 0 {
			break
		}
		cursor = nextCursor
	}
	sort.Ints(seen)
	if len(seen) != n {
		t.Fatalf("翻完应恰好 %d 条，得到 %d", n, len(seen))
	}
	for i, v := range seen {
		if v != i+1 {
			t.Fatalf("漏或重：%v", seen)
		}
	}
	first, _, _ := r.ListPage(ctx, s.ID, 0, 5)
	if len(first) != 5 || first[len(first)-1].Seq != n {
		t.Error("第一页应是最新的一页")
	}
	for _, m := range first {
		// 第 i 条（0 起）i%5==0 时带了工具调用，即 seq%5==1；其余落库为 null。
		if (m.Seq%5 == 1) != (m.ToolCalls != nil) {
			t.Errorf("seq %d 的 tool_calls 存取不对：%s", m.Seq, m.ToolCalls)
		}
	}
}

// ---------------------------------------------------------------- 轮次

func TestTurn_幂等键(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	s := mustSession(t, r, pool, next)
	in := repo.Turn{ID: next(), SessionID: s.ID, Trigger: domain.TriggerUser, IdempotencyKey: "m-1"}
	first, err := r.InsertTurn(ctx, pool, in)
	if err != nil {
		t.Fatal(err)
	}
	in.ID = next()
	if _, err := r.InsertTurn(ctx, pool, in); !errors.Is(err, repo.ErrDuplicate) {
		t.Fatalf("同键第二次应 ErrDuplicate，得到 %v", err)
	}
	got, err := r.GetTurnByIdempotencyKey(ctx, s.ID, "m-1")
	if err != nil || got.ID != first.ID {
		t.Errorf("应取回原轮次：%v", err)
	}
	// 系统轮不带键，不互相撞。
	for i := 0; i < 2; i++ {
		if _, err := r.InsertTurn(ctx, pool, repo.Turn{ID: next(), SessionID: s.ID,
			Trigger: domain.TriggerSystem}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTurnFinish_终态不被覆盖(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	s := mustSession(t, r, pool, next)
	tr := mustTurn(t, r, pool, next, s.ID)
	if ok, _ := r.SetTurnQuote(ctx, tr.ID, "chat-1", 3); !ok {
		t.Fatal("pending 时应能记报价")
	}
	if ok, _ := r.MarkTurnRunning(ctx, tr.ID); !ok {
		t.Fatal("pending → running 应成功")
	}
	if ok, _ := r.MarkTurnRunning(ctx, tr.ID); ok {
		t.Fatal("重复 MarkRunning 不应成功")
	}
	if ok, _ := r.SetTurnQuote(ctx, tr.ID, "chat-2", 9); ok {
		t.Error("running 后不应再改报价")
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st := domain.TurnSuccess
			if i%2 == 1 {
				st = domain.TurnFailed
			}
			ok, err := r.FinishTurn(ctx, tr.ID, st, 2, 1, "", "")
			if err != nil {
				t.Error(err)
			}
			if ok {
				wins.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("结算应只成功一次，得到 %d", wins.Load())
	}
	got, _ := r.GetTurn(ctx, tr.ID)
	if !got.Status.IsTerminal() || got.FinishedAt == nil || got.CreditsCost != 3 || got.ModelCode != "chat-1" {
		t.Errorf("轮次结果不对：%+v", got)
	}
	if _, err := r.FinishTurn(ctx, tr.ID, domain.TurnRunning, 0, 0, "", ""); !errors.Is(err, repo.ErrNotTerminal) {
		t.Error("非终态应拒绝")
	}
}

func TestStaleRunning_只取超时的running轮(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()

	mk := func(leaseValid bool, startedAgo string, status string) int64 {
		s := mustSession(t, r, pool, next)
		tr := mustTurn(t, r, pool, next, s.ID)
		if _, err := pool.Exec(ctx, `update agent_turn set status = $2, started_at = now() - $3::interval
			where id = $1`, tr.ID, status, startedAgo); err != nil {
			t.Fatal(err)
		}
		if leaseValid {
			if ok, _ := r.ClaimLease(ctx, s.ID, "w", time.Minute); !ok {
				t.Fatal("取租约失败")
			}
		}
		return tr.ID
	}
	stale := mk(false, "30 minutes", "running")
	_ = mk(true, "30 minutes", "running")  // 租约有效：Worker 还活着，不能动
	_ = mk(false, "1 minute", "running")   // 还没超时
	_ = mk(false, "30 minutes", "success") // 已终态
	got, err := r.StaleRunning(ctx, 20*time.Minute, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != stale {
		t.Fatalf("只应取到超时且租约过期的那一轮，得到 %+v", got)
	}
}

// ---------------------------------------------------------------- 步骤

func TestStep_等待与结束(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	s := mustSession(t, r, pool, next)
	tr := mustTurn(t, r, pool, next, s.ID)
	st, err := r.InsertStep(ctx, pool, repo.Step{ID: next(), SessionID: s.ID, TurnID: tr.ID,
		Tool: string(domain.ToolGenerate), Args: json.RawMessage(`{"prompt":"x","extra":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if st.Attempt != 1 || st.Status != repo.StepRunning {
		t.Errorf("默认值不对：%+v", st)
	}
	genID := next()
	if ok, _ := r.SetStepWaiting(ctx, pool, st.ID, genID); !ok {
		t.Fatal("running → waiting 应成功")
	}
	w, _ := r.WaitingBySession(ctx, s.ID)
	if len(w) != 1 || w[0].GenerationID != genID {
		t.Errorf("应有一个等待步骤：%+v", w)
	}
	by, _ := r.StepsByGeneration(ctx, genID)
	if len(by) != 1 || by[0].ID != st.ID {
		t.Errorf("按任务反查不对：%+v", by)
	}
	if ok, _ := r.FinishStep(ctx, pool, st.ID, repo.StepSuccess, json.RawMessage(`{"n":1}`), ""); !ok {
		t.Fatal("waiting → success 应成功")
	}
	if ok, _ := r.FinishStep(ctx, pool, st.ID, repo.StepFailed, nil, "晚到的失败"); ok {
		t.Error("终态不应被覆盖")
	}
	got, _ := r.GetStep(ctx, st.ID)
	if got.Status != repo.StepSuccess || string(got.Display) != `{"n": 1}` || got.FinishedAt == nil {
		t.Errorf("步骤结果不对：%+v %s", got, got.Display)
	}
	if w, _ := r.WaitingBySession(ctx, s.ID); len(w) != 0 {
		t.Error("结束后不应再在等待列表里")
	}
}

// ---------------------------------------------------------------- 卡片

func TestCardSubmit_CAS只成功一次(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	s := mustSession(t, r, pool, next)
	c, err := r.InsertCard(ctx, pool, repo.Card{ID: next(), SessionID: s.ID, StepID: next(),
		Kind: domain.CardChoice, Payload: json.RawMessage(`{"title":"t","options":[],"resumeState":"planning"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := r.SubmitCard(ctx, pool, c.ID, json.RawMessage(`{"selected":["a"]}`), user+int64(i))
			if err != nil {
				t.Error(err)
			}
			if ok {
				wins.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("并发提交应恰好 1 次成功，得到 %d", wins.Load())
	}
	got, _ := r.GetCard(ctx, s.ID, c.ID)
	if got.Status != domain.CardSubmitted || got.SubmittedAt == nil || got.SubmittedBy == 0 {
		t.Errorf("卡片状态不对：%+v", got)
	}
	if ok, _ := r.VoidCard(ctx, pool, c.ID); ok {
		t.Error("已提交的卡片不能作废")
	}
	if _, err := r.GetCard(ctx, s.ID+1, c.ID); !errors.Is(err, repo.ErrNotFound) {
		t.Error("别的会话取不到这张卡")
	}
}

func TestCardExpire_只处理pending(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	s := mustSession(t, r, pool, next)
	mk := func() int64 {
		c, err := r.InsertCard(ctx, pool, repo.Card{ID: next(), SessionID: s.ID, StepID: next(),
			Kind: domain.CardPlanConfirm, Payload: json.RawMessage(`{"items":[]}`)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `update agent_card set created_at = now() - interval '8 days'
			where id = $1`, c.ID); err != nil {
			t.Fatal(err)
		}
		return c.ID
	}
	old := mk()
	submitted := mk()
	voided := mk()
	if ok, _ := r.SubmitCard(ctx, pool, submitted, json.RawMessage(`{"decision":"approve"}`), user); !ok {
		t.Fatal("提交失败")
	}
	if ok, _ := r.VoidCard(ctx, pool, voided); !ok {
		t.Fatal("作废失败")
	}
	fresh, _ := r.InsertCard(ctx, pool, repo.Card{ID: next(), SessionID: s.ID, StepID: next(),
		Kind: domain.CardBudget, Payload: json.RawMessage(`{}`)})

	got, err := r.ExpireOlderThan(ctx, 7*24*time.Hour, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != old || got[0].Status != domain.CardExpired {
		t.Fatalf("只应过期那张旧的 pending 卡，得到 %+v", got)
	}
	pending, _ := r.PendingBySession(ctx, s.ID)
	if len(pending) != 1 || pending[0].ID != fresh.ID {
		t.Errorf("新卡应仍是 pending：%+v", pending)
	}
	for id, want := range map[int64]domain.CardStatus{submitted: domain.CardSubmitted, voided: domain.CardVoided} {
		c, _ := r.GetCard(ctx, s.ID, id)
		if c.Status != want {
			t.Errorf("卡 %d 应保持 %s，得到 %s", id, want, c.Status)
		}
	}
}

// ---------------------------------------------------------------- 额度

func reserve(t *testing.T, r *repo.Repo, sessionID, entryID, stepID int64, quoted int) error {
	t.Helper()
	ctx := context.Background()
	tx, err := r.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := r.InsertReserved(ctx, tx, repo.BudgetEntry{ID: entryID, SessionID: sessionID,
		StepID: stepID, Quoted: quoted}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func TestBudgetEntry_同一步骤唯一(t *testing.T) {
	r, pool, next := setup(t)
	s := mustSession(t, r, pool, next)
	step := next()
	if err := reserve(t, r, s.ID, next(), step, 10); err != nil {
		t.Fatal(err)
	}
	if err := reserve(t, r, s.ID, next(), step, 10); !errors.Is(err, repo.ErrDuplicate) {
		t.Fatalf("同一步骤第二行应撞 uk_agent_budget_step，得到 %v", err)
	}
}

func TestBudgetUsed_只计reserved与charged(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	s := mustSession(t, r, pool, next)
	ids := map[string]int64{}
	for name, q := range map[string]int{"reserved": 5, "charged": 7, "released": 11, "refunded": 13} {
		ids[name] = next()
		if err := reserve(t, r, s.ID, ids[name], next(), q); err != nil {
			t.Fatal(err)
		}
	}
	gen := next()
	if ok, _ := r.MarkCharged(ctx, ids["charged"], gen); !ok {
		t.Fatal("reserved → charged 应成功")
	}
	if ok, _ := r.MarkReleased(ctx, ids["released"]); !ok {
		t.Fatal("reserved → released 应成功")
	}
	if ok, _ := r.MarkRefunded(ctx, ids["refunded"]); ok {
		t.Fatal("reserved 不能直接 refunded")
	}
	if ok, _ := r.MarkCharged(ctx, ids["refunded"], next()); !ok {
		t.Fatal("reserved → charged 应成功")
	}
	if ok, _ := r.MarkRefunded(ctx, ids["refunded"]); !ok {
		t.Fatal("charged → refunded 应成功")
	}
	if ok, _ := r.MarkReleased(ctx, ids["charged"]); ok {
		t.Error("charged 不能 released")
	}
	tx, _ := r.Begin(ctx)
	defer func() { _ = tx.Rollback(ctx) }()
	used, err := r.UsedInTx(ctx, tx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if used != 12 {
		t.Errorf("已用额度应为 5+7=12，得到 %d", used)
	}
	all, _ := r.BudgetBySession(ctx, s.ID)
	for _, e := range all {
		if e.ID == ids["charged"] && e.GenerationID != gen {
			t.Error("charged 行应挂上生成任务 ID")
		}
	}
}

// 额度并发：8 个付费工具同时「锁会话 → 查已用 → CheckBudget → 预留」，总预留不超额度（AG-08）。
func TestBudget_并发预留不超额度(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	s := mustSession(t, r, pool, next)
	tx, _ := r.Begin(ctx)
	if _, err := r.LockForUpdate(ctx, tx, s.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.SetApprovedBudget(ctx, tx, s.ID, 30, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var ok atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		entryID, stepID := next(), next()
		wg.Add(1)
		go func() {
			defer wg.Done()
			tx, err := r.Begin(ctx)
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = tx.Rollback(ctx) }()
			sess, err := r.LockForUpdate(ctx, tx, s.ID)
			if err != nil {
				t.Error(err)
				return
			}
			used, err := r.UsedInTx(ctx, tx, s.ID)
			if err != nil {
				t.Error(err)
				return
			}
			d, err := domain.CheckBudget(sess.ApprovedBudget, used, 10)
			if err != nil || !d.Allow {
				return
			}
			if _, err := r.InsertReserved(ctx, tx, repo.BudgetEntry{ID: entryID, SessionID: s.ID,
				StepID: stepID, Quoted: 10}); err != nil {
				t.Error(err)
				return
			}
			if err := tx.Commit(ctx); err == nil {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 3 {
		t.Fatalf("额度 30、每次 10，应恰好预留成功 3 次，得到 %d", ok.Load())
	}
}

// ---------------------------------------------------------------- 技能与偏好

func TestSkillAndPref_读写(t *testing.T) {
	r, pool, next := setup(t)
	_ = pool
	ctx := context.Background()
	sk := repo.Skill{ID: next(), Code: "canvas", Name: "画布技能", Category: "general", Body: "说明 {{x}}",
		Tools: json.RawMessage(`["read_canvas","generate"]`), Sort: 2, IsEnabled: true}
	id1, err := r.UpsertSkill(ctx, sk)
	if err != nil {
		t.Fatal(err)
	}
	// 内容不变：幂等，版本不动。
	sk.ID = next()
	id2, err := r.UpsertSkill(ctx, sk)
	if err != nil || id2 != id1 {
		t.Fatalf("重复 upsert 应返回原 ID：%v %d %d", err, id1, id2)
	}
	got, _ := r.GetSkillByCode(ctx, "canvas")
	if got.Version != 1 {
		t.Errorf("内容未变版本应保持 1，得到 %d", got.Version)
	}
	sk.Body = "新说明"
	if _, err := r.UpsertSkill(ctx, sk); err != nil {
		t.Fatal(err)
	}
	got, _ = r.GetSkillByCode(ctx, "canvas")
	if got.Version != 2 || got.Body != "新说明" {
		t.Errorf("内容变化后版本应 +1：%+v", got)
	}
	if _, err := r.UpsertSkill(ctx, repo.Skill{ID: next(), Code: "off", Name: "停用", Category: "c",
		Body: "b", IsEnabled: false}); err != nil {
		t.Fatal(err)
	}
	list, _ := r.ListEnabledSkills(ctx)
	if len(list) != 1 || list[0].Code != "canvas" {
		t.Errorf("只应列出启用的技能：%+v", list)
	}
	if err := r.UpsertSkillReference(ctx, repo.SkillReference{ID: next(), SkillID: id1,
		Name: "单段流程工作指引", Body: "步骤……"}); err != nil {
		t.Fatal(err)
	}
	ref, err := r.SkillReferenceByName(ctx, id1, "单段流程工作指引")
	if err != nil || ref.Body != "步骤……" {
		t.Errorf("附属文档读取不对：%v %+v", err, ref)
	}
	if _, err := r.SkillReferenceByName(ctx, id1, "不存在"); !errors.Is(err, repo.ErrNotFound) {
		t.Error("不存在的附属文档应 ErrNotFound")
	}

	p, err := r.GetUserPref(ctx, user)
	if err != nil || string(p) != `{}` {
		t.Errorf("没设过偏好应返回 {}：%v %s", err, p)
	}
	if err := r.UpsertUserPref(ctx, user, json.RawMessage(`{"video":{"model":"a"},"future":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := r.UpsertUserPref(ctx, user, json.RawMessage(`{"video":{"model":"b"},"future":1}`)); err != nil {
		t.Fatal(err)
	}
	p, _ = r.GetUserPref(ctx, user)
	var m map[string]any
	_ = json.Unmarshal(p, &m)
	if m["future"] == nil || m["video"].(map[string]any)["model"] != "b" {
		t.Errorf("偏好应覆盖写入且保留未知字段：%s", p)
	}
}

// ---------------------------------------------------------------- outbox

type recordingPub struct {
	mu     sync.Mutex
	topics []string
	bodies [][]byte
}

func (p *recordingPub) Publish(_ context.Context, topic string, payload []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.topics = append(p.topics, topic)
	p.bodies = append(p.bodies, payload)
	return nil
}

func TestEnqueueTurn_事务回滚不留消息(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	s := mustSession(t, r, pool, next)
	tx, _ := r.Begin(ctx)
	if err := r.EnqueueTurnInTx(ctx, tx, next(), s.ID, next()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `select count(*) from outbox_message`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("回滚后不应留下 outbox 行，得到 %d", n)
	}
}

func TestEnqueueTurn_投递器可认领(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	s := mustSession(t, r, pool, next)
	turnID := next()
	tx, _ := r.Begin(ctx)
	if err := r.EnqueueTurnInTx(ctx, tx, next(), s.ID, turnID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var aggType string
	if err := pool.QueryRow(ctx, `select aggregate_type from outbox_message`).Scan(&aggType); err != nil {
		t.Fatal(err)
	}
	if aggType != "agent" {
		t.Errorf("aggregate_type 应为 agent，得到 %s", aggType)
	}
	pub := &recordingPub{}
	relay := genapp.NewRelay(genrepo.New(pool), pub, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if sent := relay.Once(ctx); sent != 1 {
		t.Fatalf("投递器应认领并发出 1 条，得到 %d", sent)
	}
	if pub.topics[0] != domain.TopicTurnRequested {
		t.Errorf("topic 应为 %s，得到 %s", domain.TopicTurnRequested, pub.topics[0])
	}
	var payload domain.TurnRequestedPayload
	if err := json.Unmarshal(pub.bodies[0], &payload); err != nil {
		t.Fatal(err)
	}
	if payload.SessionID != strconv.FormatInt(s.ID, 10) || payload.TurnID != strconv.FormatInt(turnID, 10) || payload.SchemaVersion != 1 {
		t.Errorf("消息体不对：%s", pub.bodies[0])
	}
	if sent := relay.Once(ctx); sent != 0 {
		t.Error("已投递的不应再投")
	}
}

// 视频规格三问按固定题目 id 从已回答的问答卡里取（会话 107419662995685376）：只取本会话已提交的问答卡，新的在前。
func TestAnsweredQuestionCards_只取已提交的问答卡且新的在前(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	s := mustSession(t, r, pool, next)
	other := mustSession(t, r, pool, next)
	payload := json.RawMessage(`{"title":"t","questions":[{"id":"aspect_ratio","text":"画幅","type":"text"}],"resumeState":"planning"}`)
	insert := func(sessionID int64, kind domain.CardKind) repo.Card {
		t.Helper()
		c, err := r.InsertCard(ctx, pool, repo.Card{ID: next(), SessionID: sessionID, StepID: next(), Kind: kind, Payload: payload})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	submit := func(c repo.Card, text string) {
		t.Helper()
		sub := json.RawMessage(`{"answers":[{"questionId":"aspect_ratio","text":"` + text + `"}]}`)
		if ok, err := r.SubmitCard(ctx, pool, c.ID, sub, user); err != nil || !ok {
			t.Fatalf("提交失败：%v %v", ok, err)
		}
	}
	first, second := insert(s.ID, domain.CardQuestions), insert(s.ID, domain.CardQuestions)
	submit(first, "16:9")
	submit(second, "9:16")
	insert(s.ID, domain.CardQuestions) // 待回答：不算
	choice := insert(s.ID, domain.CardChoice)
	if ok, err := r.SubmitCard(ctx, pool, choice.ID, json.RawMessage(`{"selected":["a"]}`), user); err != nil || !ok {
		t.Fatal(err)
	}
	submit(insert(other.ID, domain.CardQuestions), "1:1") // 别的会话：不算

	got, err := r.AnsweredQuestionCards(ctx, s.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != second.ID || got[1].ID != first.ID {
		t.Fatalf("应只有本会话两张已提交的问答卡、新的在前：%+v", got)
	}
	if !strings.Contains(string(got[0].Submission), "9:16") {
		t.Fatalf("要带上提交内容：%s", got[0].Submission)
	}
	if one, _ := r.AnsweredQuestionCards(ctx, s.ID, 1); len(one) != 1 || one[0].ID != second.ID {
		t.Fatalf("limit 截断时保留最新的：%+v", one)
	}
}
