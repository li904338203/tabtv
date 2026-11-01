package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
)

// 扫描循环（Worker，每 2s，AGT-25 / dev/01 §4.3）。
//
// 会话「继续跑」不依赖任何进程内状态：挂起即落库退出（T-4），之后的每一次推进都由这里
// 从库里捞出来 —— 所以它**同时就是重启恢复**（同整图运行的 Tick，CLAUDE.md §5.8）。
// 生成终态的即时唤醒钩子只是优化，可以后补；它失败只能少一次优化，不能少一次唤醒。
//
// 一次 Tick 依次做：
//
//	C 超时轮：running 超阈值且租约已过期 → **先退款、再 CAS 判 failed** → 会话离开运行态
//	D 卡片过期：pending 卡片超过 CardExpire → expired，会话回 idle（不扣费、不取消已完成的生成）
//	Q 排队轮：会话不在运行态、没人持租约，却还有 pending 轮 → 按「直接发消息」的规则重新路由并入队
//	B 租约过期：运行态而租约为空 / 过期 → 用**原 turnID** 重写 outbox；没有未结束的轮则让会话离开运行态
//	A 唤醒：waiting_task 且所等任务全部终态 → 回写步骤终态 + 一个事务里 CAS 迁移 + 建 system 轮 + 入队
//	E 额度归还：条目 charged 而对应生成已失败 / 取消且生成域已退款 → refunded，步骤补终态；
//	  挂着非生成对象的条目（AGT-50：组 / 整图运行 / 渲染 / 同步文本调用）按账本结算，
//	  部分退回记进 refunded 列（scanner_ref.go）
//
// ⚠️ C 排在 B 前面：同时命中两者的轮以 C 为准（C 处理完它已是终态，B 就看不到了）。
// ⚠️ 多实例同时扫描的安全**不靠 Redis 锁**（那只是省重复劳动）：每一步都在锁住会话行的
// 事务里复核条件后再做，状态推进一律 CAS。

// TaskOutput 是一个生成产物的摘要（步骤展示用）。
type TaskOutput struct {
	Idx     int
	Type    string
	MediaID int64
	Text    string
}

// TaskResult 是扫描循环看到的一个生成任务。
type TaskResult struct {
	ID int64
	// Found=false：任务不存在或不属于这个工作区（按失败处理，不会永远等它）。
	Found        bool
	Status       string
	Terminal     bool
	ErrorCode    string
	ErrorMessage string
	// Outputs 成功时的全部产物（多产出全部列出，不只取第一个 —— GO-40）。
	Outputs []TaskOutput
	// Refunded：生成域的钱已经了结 —— 账本上有退款流水，或者本来就没扣过。
	// ⚠️ 只认账本，不认 credits_cost（那是报价，退款后不会变）。
	Refunded bool
}

// TaskStatuses 批量查生成任务（避免逐个 Get 的 N+1）。实现：wiring.AgentTaskStatuses。
//
// ⚠️ 单列一个端口而不是给 Generator 加方法：Generator 的适配器由付费工具（AGT-24）维护，
// 往接口上加方法会让它在补上实现之前编译不过。
type TaskStatuses interface {
	Statuses(ctx context.Context, ws int64, ids []int64) (map[int64]TaskResult, error)
}

// ScannerConfig 是扫描参数。
type ScannerConfig struct {
	// CardExpire 卡片过期时间（SEEAITV_AGENT_CARD_EXPIRE，默认 168h）。
	CardExpire time.Duration
	// MaxCalls 一轮的调用上限（SEEAITV_AGENT_TURN_MAX_CALLS），用于推导 StaleTurnAfter。
	MaxCalls int
	// StaleTurnAfter running 多久且租约过期才判死；0 = StaleTurnAfterFor(MaxCalls)。
	StaleTurnAfter time.Duration
	// RequeueQuiet 会话在这段时间内写过 outbox / 更新过就不重投（见 repo/scan.go 文件头）。
	RequeueQuiet time.Duration
	// Limit 每类扫描每次最多处理多少条（默认 100），按 last_active_at 升序。
	Limit int
}

const (
	// perCallBudget 一次推理调用的正常上限：默认空闲超时 60s（dev/01 §7.2）。
	perCallBudget = 60 * time.Second
	// staleMargin 工具耗时与写库的余量（同 script 的 staleCallAfter = 请求超时 + 5min）。
	staleMargin = 5 * time.Minute
	// staleFloor 阈值下限：设计文档的 20 分钟。
	staleFloor = 20 * time.Minute
	// defaultRequeueQuiet 重投节流：一条新轮从 API 写 outbox 到 Worker 取到通常 < 几秒。
	defaultRequeueQuiet = 20 * time.Second
	// ScanInterval Worker 调用 Tick 的间隔。
	ScanInterval = 2 * time.Second
	// ErrCodeTurnStale 被扫描循环判死的轮（agent_turn.error_code）。
	ErrCodeTurnStale = "turn_stale"
	// ErrCodeSessionCancelled 排队轮所在的会话已取消。
	ErrCodeSessionCancelled = "session_cancelled"
	// ErrCodeSuperseded 排队的系统轮被用户卡片取代（会话已挂起等用户）。
	ErrCodeSuperseded = "superseded"
)

// StaleTurnAfterFor 从配置推导「一轮跑多久算死」：调用上限 × 单次调用上限 + 余量，不低于 20 分钟。
//
// ⚠️ 阈值必须大于一轮的正常上限，否则会把一个还在跑的轮判死并退款、随后它又写出产出。
// 默认 20 次 × 60s = 20 分钟**已经与设计文档的阈值相等**（还没算工具耗时），所以不能写死 20 分钟。
// 判死**同时**要求会话租约已过期（repo.StaleRunning）：租约还在续期就证明有人在跑，一律不动 ——
// 这才是主保证，阈值只是第二道。
func StaleTurnAfterFor(maxCalls int) time.Duration {
	if maxCalls <= 0 {
		maxCalls = 20
	}
	d := time.Duration(maxCalls)*perCallBudget + staleMargin
	if d < staleFloor {
		d = staleFloor
	}
	return d
}

// ScannerDeps 是扫描循环的依赖。Repo / Tasks / Ledger / NextID 必须给；Events 为 nil 时不推事件。
type ScannerDeps struct {
	Repo  *repo.Repo
	Tasks TaskStatuses
	// Refs 按业务引用查终态（AGT-50 多段工具）。nil 时挂着非生成对象的等待步骤不会被唤醒、
	// 条目不会被结算（只在没装多段工具的测试里为 nil）。
	Refs   RefStatuses
	Ledger Ledger
	Events Events
	NextID func() int64
	Logger *slog.Logger
	Now    func() time.Time
}

// Scanner 是扫描循环。无状态：多实例同时跑安全。
type Scanner struct {
	d   ScannerDeps
	cfg ScannerConfig
}

// NewScanner 建扫描循环。
func NewScanner(d ScannerDeps, cfg ScannerConfig) *Scanner {
	if cfg.CardExpire <= 0 {
		cfg.CardExpire = 168 * time.Hour
	}
	if cfg.StaleTurnAfter <= 0 {
		cfg.StaleTurnAfter = StaleTurnAfterFor(cfg.MaxCalls)
	}
	if cfg.RequeueQuiet <= 0 {
		cfg.RequeueQuiet = defaultRequeueQuiet
	}
	if cfg.Limit <= 0 {
		cfg.Limit = 100
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Scanner{d: d, cfg: cfg}
}

// Config 返回生效的配置（日志与测试用）。
func (s *Scanner) Config() ScannerConfig { return s.cfg }

// Stats 是一次 Tick 的处理计数。
type Stats struct {
	StaleFailed    int // C：判死并退款的轮
	CardsExpired   int // D：过期的卡片
	QueuedResumed  int // Q：重新路由并入队的排队轮
	QueuedDropped  int // Q：作废的排队轮（会话已取消 / 系统轮被卡片取代）
	Requeued       int // B：用原 turnID 重新入队
	Orphaned       int // B：运行态却没有未结束的轮，已让会话离开运行态
	Woken          int // A：唤醒的会话
	StepsFinished  int // A / E：回写终态的步骤
	BudgetRefunded int // E：归还的额度条目
	BudgetSettled  int // E：按账本结算的非生成对象条目（含部分退回，AGT-50）
	TurnRefunded   int // R：补退成功的对话费（结算时退款失败的轮，04 文档 P-17）
}

// Total 是这次处理的总条数（为 0 时不打日志）。
func (st Stats) Total() int {
	return st.StaleFailed + st.CardsExpired + st.QueuedResumed + st.QueuedDropped + st.Requeued +
		st.Orphaned + st.Woken + st.StepsFinished + st.BudgetRefunded + st.BudgetSettled + st.TurnRefunded
}

// Tick 扫一遍。limit<=0 用配置值。返回的 error 汇总了各类扫描的失败（一类失败不影响其余）。
func (s *Scanner) Tick(ctx context.Context, limit int) (Stats, error) {
	if limit <= 0 {
		limit = s.cfg.Limit
	}
	var st Stats
	errs := []error{
		s.scanStale(ctx, limit, &st),
		s.scanCards(ctx, limit, &st),
		s.scanQueued(ctx, limit, &st),
		s.scanLeaseExpired(ctx, limit, &st),
		s.scanWaiting(ctx, limit, &st),
		s.scanBudget(ctx, limit, &st),
		s.scanSettle(ctx, limit, &st),
		s.scanTurnRefunds(ctx, limit, &st),
	}
	return st, errors.Join(errs...)
}

// turnRefundSettleAfter 补退款前给 Runner 自己的结算留的时间。
const turnRefundSettleAfter = time.Minute

// scanTurnRefunds 是 R：补退「失败 / 取消却没退成」的对话费（04 文档 P-17）。
//
// 账本的退款按 (ref_type, ref_id) 只成功一次：与 Runner 或另一个实例撞上时只有一方生效。
// 找不到原消费流水（当时没扣成）不是错误，跳过。
func (s *Scanner) scanTurnRefunds(ctx context.Context, limit int, st *Stats) error {
	cands, err := s.d.Repo.UnrefundedTurns(ctx, turnRefundSettleAfter, limit)
	if err != nil {
		return fmt.Errorf("扫描待补退的对话费: %w", err)
	}
	var errs []error
	for _, c := range cands {
		err := s.d.Ledger.Refund(ctx, c.PayerAccountID, RefTypeTurn, idStr(c.TurnID), "补退：轮次失败后退款没有成功")
		switch {
		case err == nil:
			st.TurnRefunded++
			s.d.Logger.Info("已补退 Agent 对话费", "turnId", c.TurnID, "sessionId", c.SessionID, "credits", c.Credits)
		case errors.Is(err, ErrLedgerNotFound):
		default:
			errs = append(errs, fmt.Errorf("补退轮次 %d: %w", c.TurnID, err))
		}
	}
	return errors.Join(errs...)
}

// ---------------------------------------------------------------- C 超时轮

func (s *Scanner) scanStale(ctx context.Context, limit int, st *Stats) error {
	turns, err := s.d.Repo.StaleRunning(ctx, s.cfg.StaleTurnAfter, limit)
	if err != nil {
		return fmt.Errorf("扫描超时轮: %w", err)
	}
	var errs []error
	for _, t := range turns {
		if err := s.failStale(ctx, t, st); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// failStale 判死一轮：**先退款、再 CAS 判失败**（同 script.RecoverStaleCalls）。
//
// ⚠️ 顺序反过来的话：判完失败而退款没成，下一轮扫描就再也捞不到它（它已不是 running），
// 钱就永远扣着。退款是幂等的（账本唯一索引，重复视为成功），所以先退、退成了再判，
// 中间崩了下次照样捞到、照样退一次（第二次是幂等命中）。
func (s *Scanner) failStale(ctx context.Context, t repo.Turn, st *Stats) error {
	sess, err := s.d.Repo.GetByID(ctx, t.SessionID)
	if err != nil {
		return fmt.Errorf("超时轮 %d 取会话: %w", t.ID, err)
	}
	if t.CreditsCost > 0 {
		if err := s.d.Ledger.Refund(ctx, sess.PayerAccountID, RefTypeTurn, idStr(t.ID),
			"Agent 轮次超时未完成，退回对话费"); err != nil {
			// 不判失败：留到下一次扫描再退（判了就再也捞不到了）。
			return fmt.Errorf("超时轮 %d 退款失败（下次扫描重试）: %w", t.ID, err)
		}
	}
	changed, err := s.d.Repo.FinishTurn(ctx, t.ID, domain.TurnFailed, t.CallCount, t.ValidOutputs,
		ErrCodeTurnStale, fmt.Sprintf("运行超过 %s 且执行者已失联，本轮按失败结束并退款", s.cfg.StaleTurnAfter))
	if err != nil {
		return fmt.Errorf("超时轮 %d 判失败: %w", t.ID, err)
	}
	if !changed {
		return nil // 别的实例 / 迟到的 Runner 已经结束了它
	}
	st.StaleFailed++
	s.d.Logger.Warn("Agent 轮次超时判死并退款", "sessionId", sess.ID, "turnId", t.ID,
		"credits", t.CreditsCost, "startedAt", t.StartedAt)

	tx, err := s.d.Repo.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	locked, err := s.d.Repo.LockForUpdate(ctx, tx, sess.ID)
	if err != nil {
		return err
	}
	now, err := s.d.Repo.DBNow(ctx, tx)
	if err != nil {
		return err
	}
	if !repo.LeaseFree(locked, now) || (locked.State != domain.StatePlanning && locked.State != domain.StateExecuting) {
		return tx.Commit(ctx)
	}
	to, moved, err := s.leaveRunning(ctx, tx, locked)
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if moved {
		locked.State = to
		s.publishState(ctx, locked, t.ID, ErrCodeTurnStale)
	}
	return nil
}

// leaveRunning 让运行态（planning / executing）却没人在跑的会话离开运行态：
// 执行态且还有等待中的步骤 → waiting_task（交给 A 唤醒），否则 → idle。
func (s *Scanner) leaveRunning(ctx context.Context, tx pgx.Tx, sess repo.Session) (domain.State, bool, error) {
	ev := domain.EvTurnEnd
	if sess.State == domain.StateExecuting {
		waiting, err := s.d.Repo.WaitingStepsInTx(ctx, tx, sess.ID)
		if err != nil {
			return sess.State, false, err
		}
		if len(waiting) > 0 {
			ev = domain.EvWaitTask
		}
	}
	to, ok := domain.CanTransition(sess.State, ev)
	if !ok {
		return sess.State, false, nil
	}
	moved, err := s.d.Repo.TransitionState(ctx, tx, sess.ID, sess.State, to)
	return to, moved, err
}

// ---------------------------------------------------------------- D 卡片过期

func (s *Scanner) scanCards(ctx context.Context, limit int, st *Stats) error {
	cards, err := s.d.Repo.ExpireOlderThan(ctx, s.cfg.CardExpire, limit)
	if err != nil {
		return fmt.Errorf("扫描过期卡片: %w", err)
	}
	st.CardsExpired += len(cards)
	seen := map[int64]bool{}
	var errs []error
	for _, c := range cards {
		if seen[c.SessionID] {
			continue
		}
		seen[c.SessionID] = true
		sess, moved, err := s.idleAfterExpiry(ctx, c.SessionID)
		if err != nil {
			errs = append(errs, fmt.Errorf("卡片过期后回 idle（会话 %d）: %w", c.SessionID, err))
			continue
		}
		for _, cc := range cards {
			if cc.SessionID == c.SessionID {
				s.publish(ctx, sess.ProjectID, &AgentCardEvent{Type: EventAgentCard,
					SessionID: idStr(cc.SessionID), CardID: idStr(cc.ID), Kind: string(cc.Kind),
					Status: string(domain.CardExpired)})
			}
		}
		if moved {
			s.publishState(ctx, sess, 0, "card_expired")
		}
	}
	return errors.Join(errs...)
}

// idleAfterExpiry 卡片过期后把挂起的会话 CAS 回 idle。
//
// ⚠️ 只动挂起态（awaiting_input / awaiting_approval），且要求没有别的待处理卡片：
// 用户在卡片过期前已经直接发消息让会话跑起来了的，不能把它打回 idle。
// ⚠️ 不扣费、不退任何已完成的生成：卡片过期只是「不再等这个回答」。
func (s *Scanner) idleAfterExpiry(ctx context.Context, sessionID int64) (repo.Session, bool, error) {
	tx, err := s.d.Repo.Begin(ctx)
	if err != nil {
		return repo.Session{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	locked, err := s.d.Repo.LockForUpdate(ctx, tx, sessionID)
	if err != nil {
		return repo.Session{}, false, err
	}
	if !domain.IsSuspendedForUser(locked.State) {
		return locked, false, tx.Commit(ctx)
	}
	pending, err := s.d.Repo.HasPendingCards(ctx, tx, sessionID)
	if err != nil {
		return locked, false, err
	}
	if pending {
		return locked, false, tx.Commit(ctx)
	}
	to, ok := domain.CanTransition(locked.State, domain.EvCardExpired)
	if !ok {
		return locked, false, tx.Commit(ctx)
	}
	moved, err := s.d.Repo.TransitionState(ctx, tx, sessionID, locked.State, to)
	if err != nil {
		return locked, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return locked, false, err
	}
	if moved {
		locked.State = to
	}
	return locked, moved, nil
}

// ---------------------------------------------------------------- Q 排队轮

func (s *Scanner) scanQueued(ctx context.Context, limit int, st *Stats) error {
	sessions, err := s.d.Repo.QueuedTurnSessions(ctx, s.cfg.RequeueQuiet, limit)
	if err != nil {
		return fmt.Errorf("扫描排队轮: %w", err)
	}
	var errs []error
	for _, sess := range sessions {
		if err := s.resumeQueued(ctx, sess.ID, st); err != nil {
			errs = append(errs, fmt.Errorf("排队轮（会话 %d）: %w", sess.ID, err))
		}
	}
	return errors.Join(errs...)
}

// resumeQueued 处理一个不在运行态、却还有 pending 轮的会话（AGT-22 的已知缺口）。
//
// 这一轮是用户在上一轮运行中发的消息：当时会话在 planning / executing，PostMessage 不迁移
// 状态、只排队；而那条队列消息在 Worker 持有租约时到达被当作 Busy 丢弃，或在上一轮结束、
// 会话已离开运行态之后才被取到（Runner 不再跑它，见 runner.go）。
// 处理方式就是**把它当成用户此刻直接发的一条消息**：按 domain.MessageEvent 选迁移
// （与 PostMessage 同一条规则，含挂起时作废规划确认卡），再用**原 turnID** 入队。
func (s *Scanner) resumeQueued(ctx context.Context, sessionID int64, st *Stats) error {
	tx, err := s.d.Repo.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	locked, err := s.d.Repo.LockForUpdate(ctx, tx, sessionID)
	if err != nil {
		return err
	}
	now, err := s.d.Repo.DBNow(ctx, tx)
	if err != nil {
		return err
	}
	if domain.IsRunning(locked.State) && locked.State != domain.StateWaitingTask {
		return tx.Commit(ctx) // 已经跑起来了：交给 Runner / B
	}
	if !repo.LeaseFree(locked, now) {
		return tx.Commit(ctx)
	}
	if recent, err := s.d.Repo.RecentOutbox(ctx, tx, sessionID, s.cfg.RequeueQuiet); err != nil || recent {
		return errors.Join(err, tx.Commit(ctx))
	}
	turn, err := s.d.Repo.UnfinishedTurn(ctx, tx, sessionID, false)
	if errors.Is(err, repo.ErrNotFound) {
		return tx.Commit(ctx)
	}
	if err != nil {
		return err
	}

	// 作废：会话已取消，或系统轮遇上了等用户的卡片（用户的回答会带来新的一轮）。
	if locked.State == domain.StateCancelled ||
		(turn.Trigger == domain.TriggerSystem && domain.IsSuspendedForUser(locked.State)) {
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		return s.dropQueued(ctx, locked, turn, st)
	}

	pending, err := s.d.Repo.PendingBySession(ctx, sessionID)
	if err != nil {
		return err
	}
	used, err := s.d.Repo.UsedInTx(ctx, tx, sessionID)
	if err != nil {
		return err
	}
	mc := domain.MessageContext{PlanApproved: locked.ApprovedBudget > 0 && used < locked.ApprovedBudget}
	card, hasCard := suspendCard(locked.State, pending)
	if hasCard {
		mc.PendingCard = card.Kind
		if locked.State == domain.StateAwaitingInput {
			if mc.ResumeState, err = domain.ResumeStateOf(card.Domain()); err != nil {
				mc.ResumeState = domain.StatePlanning // 同 PostMessage：卡片损坏保守回规划
			}
		}
	} else if locked.State == domain.StateAwaitingInput {
		mc.ResumeState = domain.StatePlanning
	}
	route, err := domain.MessageEvent(locked.State, mc)
	if err != nil {
		return err
	}
	if route.Transition {
		moved, err := s.d.Repo.TransitionState(ctx, tx, sessionID, locked.State, route.To)
		if err != nil {
			return err
		}
		if !moved {
			return tx.Commit(ctx)
		}
		locked.State = route.To
	}
	voided := false
	if route.VoidCard && hasCard {
		if voided, err = s.d.Repo.VoidCard(ctx, tx, card.ID); err != nil {
			return err
		}
		if err := s.d.Repo.ClearPlanVersion(ctx, tx, sessionID); err != nil {
			return err
		}
	}
	if err := s.d.Repo.EnqueueTurnInTx(ctx, tx, s.d.NextID(), sessionID, turn.ID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	st.QueuedResumed++
	s.d.Logger.Info("排队轮已重新入队", "sessionId", sessionID, "turnId", turn.ID, "state", locked.State)
	if voided {
		s.publish(ctx, locked.ProjectID, &AgentCardEvent{Type: EventAgentCard, SessionID: idStr(sessionID),
			CardID: idStr(card.ID), Kind: string(card.Kind), Status: string(domain.CardVoided)})
	}
	s.publishState(ctx, locked, turn.ID, "")
	return nil
}

// dropQueued 作废一条不会再跑的排队轮。
//
// ⚠️ pending 轮也可能已经扣过费：Runner 的顺序是「记报价 → 扣费 → 标 running」，
// 崩在扣费之后、标 running 之前就是一个扣了钱的 pending 轮。所以有报价就**先尝试退款**：
// 没扣过时账本找不到原流水会报错，那是预期内的，不阻断作废；真扣过的就在这里退掉。
func (s *Scanner) dropQueued(ctx context.Context, sess repo.Session, t repo.Turn, st *Stats) error {
	code := ErrCodeSuperseded
	if sess.State == domain.StateCancelled {
		code = ErrCodeSessionCancelled
	}
	if t.CreditsCost > 0 {
		if err := s.d.Ledger.Refund(ctx, sess.PayerAccountID, RefTypeTurn, idStr(t.ID),
			"Agent 排队轮作废"); err != nil {
			s.d.Logger.Debug("作废排队轮时尝试退款未成（多半是本就没扣过）", "turnId", t.ID, "error", err)
		}
	}
	changed, err := s.d.Repo.FinishTurn(ctx, t.ID, domain.TurnCancelled, 0, 0, code, "")
	if err != nil {
		return err
	}
	if changed {
		st.QueuedDropped++
	}
	return nil
}

// ---------------------------------------------------------------- B 租约过期

func (s *Scanner) scanLeaseExpired(ctx context.Context, limit int, st *Stats) error {
	sessions, err := s.d.Repo.LeaseFreeRunning(ctx, s.cfg.RequeueQuiet, limit)
	if err != nil {
		return fmt.Errorf("扫描租约过期的会话: %w", err)
	}
	var errs []error
	for _, sess := range sessions {
		if err := s.requeue(ctx, sess.ID, st); err != nil {
			errs = append(errs, fmt.Errorf("租约过期重入队（会话 %d）: %w", sess.ID, err))
		}
	}
	return errors.Join(errs...)
}

// requeue 运行态但没人持租约：用**原 turnID** 重写 outbox（不新建轮、不重复计费 ——
// Runner 对 running 的轮不再扣费，对 pending 的轮扣费撞账本唯一索引视为已扣）。
// 没有任何未结束的轮（Runner 崩在落终态之后、迁移会话之前）→ 让会话离开运行态。
func (s *Scanner) requeue(ctx context.Context, sessionID int64, st *Stats) error {
	tx, err := s.d.Repo.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	locked, err := s.d.Repo.LockForUpdate(ctx, tx, sessionID)
	if err != nil {
		return err
	}
	now, err := s.d.Repo.DBNow(ctx, tx)
	if err != nil {
		return err
	}
	if (locked.State != domain.StatePlanning && locked.State != domain.StateExecuting) || !repo.LeaseFree(locked, now) {
		return tx.Commit(ctx)
	}
	if recent, err := s.d.Repo.RecentOutbox(ctx, tx, sessionID, s.cfg.RequeueQuiet); err != nil || recent {
		return errors.Join(err, tx.Commit(ctx))
	}
	turn, err := s.d.Repo.UnfinishedTurn(ctx, tx, sessionID, true)
	if errors.Is(err, repo.ErrNotFound) {
		to, moved, err := s.leaveRunning(ctx, tx, locked)
		if err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		if moved {
			st.Orphaned++
			locked.State = to
			s.d.Logger.Warn("运行态会话没有未结束的轮，已离开运行态", "sessionId", sessionID, "to", to)
			s.publishState(ctx, locked, 0, "")
		}
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.d.Repo.EnqueueTurnInTx(ctx, tx, s.d.NextID(), sessionID, turn.ID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	st.Requeued++
	s.d.Logger.Info("租约过期，已用原 turnID 重新入队", "sessionId", sessionID, "turnId", turn.ID,
		"turnStatus", turn.Status)
	return nil
}

// ---------------------------------------------------------------- A 唤醒

func (s *Scanner) scanWaiting(ctx context.Context, limit int, st *Stats) error {
	sessions, err := s.d.Repo.WaitingTaskSessions(ctx, limit)
	if err != nil {
		return fmt.Errorf("扫描 waiting_task 会话: %w", err)
	}
	var errs []error
	for _, sess := range sessions {
		if err := s.wake(ctx, sess, st); err != nil {
			errs = append(errs, fmt.Errorf("唤醒（会话 %d）: %w", sess.ID, err))
		}
	}
	return errors.Join(errs...)
}

// wake 所等任务全部终态 → 一个事务里：回写步骤终态 + CAS waiting_task→executing +
// 建 system 轮（不计费）+ 入队。CAS 失败即放弃（别的实例已唤醒）。
//
// ⚠️ 只有 1 个终态时不唤醒：等的是「全部」，部分完成就续跑的话模型会对着半批产出做总结。
func (s *Scanner) wake(ctx context.Context, sess repo.Session, st *Stats) error {
	steps, err := s.d.Repo.WaitingBySession(ctx, sess.ID)
	if err != nil {
		return err
	}
	results, err := s.statuses(ctx, sess.WorkspaceID, stepGenerationIDs(steps))
	if err != nil {
		return err
	}
	refs, err := s.refWaits(ctx, sess, steps)
	if err != nil {
		return err
	}
	for _, step := range steps {
		if r := results[step.GenerationID]; step.GenerationID != 0 && !r.Terminal {
			return nil // 还有没结束的
		}
		if w, ok := refs[step.ID]; step.GenerationID == 0 && (!ok || !w.terminal()) {
			return nil // 等的组 / 运行 / 渲染还没结束
		}
	}

	tx, err := s.d.Repo.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	locked, err := s.d.Repo.LockForUpdate(ctx, tx, sess.ID)
	if err != nil {
		return err
	}
	now, err := s.d.Repo.DBNow(ctx, tx)
	if err != nil {
		return err
	}
	if locked.State != domain.StateWaitingTask || !repo.LeaseFree(locked, now) {
		return tx.Commit(ctx) // 别的实例已唤醒 / 用户追问已让它跑起来
	}
	if _, err := s.d.Repo.UnfinishedTurn(ctx, tx, sess.ID, true); err == nil {
		return tx.Commit(ctx) // 有未结束的轮：交给 B / Q
	} else if !errors.Is(err, repo.ErrNotFound) {
		return err
	}
	// 锁内再读一遍等待步骤：候选查询之后可能多出新的等待步骤（那时它的任务我们还没查过）。
	inTx, err := s.d.Repo.WaitingStepsInTx(ctx, tx, sess.ID)
	if err != nil {
		return err
	}
	for _, step := range inTx {
		if _, ok := results[step.GenerationID]; step.GenerationID != 0 && !ok {
			return tx.Commit(ctx) // 下一次扫描再看
		}
		if _, ok := refs[step.ID]; step.GenerationID == 0 && !ok {
			return tx.Commit(ctx)
		}
	}
	to, ok := domain.CanTransition(locked.State, domain.EvTasksDone)
	if !ok {
		return tx.Commit(ctx)
	}
	moved, err := s.d.Repo.TransitionState(ctx, tx, sess.ID, locked.State, to)
	if err != nil {
		return err
	}
	if !moved {
		return tx.Commit(ctx)
	}

	// 步骤终态回写（唤醒之前，与迁移同一个事务）。
	type finished struct {
		step    repo.Step
		status  string
		display json.RawMessage
	}
	var done []finished
	for _, step := range inTx {
		status, display, errMsg := stepOutcome(step, results[step.GenerationID])
		if step.GenerationID == 0 {
			status, display, errMsg = refStepOutcome(step, refs[step.ID])
		}
		changed, err := s.d.Repo.FinishStep(ctx, tx, step.ID, status, display, errMsg)
		if err != nil {
			return err
		}
		if changed {
			done = append(done, finished{step, status, display})
		}
	}

	turn, err := s.d.Repo.InsertTurn(ctx, tx, repo.Turn{ID: s.d.NextID(), SessionID: sess.ID,
		Trigger: domain.TriggerSystem, IdempotencyKey: wakeKey(inTx)})
	if err != nil {
		return err
	}
	if _, err := s.d.Repo.Append(ctx, tx, repo.Message{ID: s.d.NextID(), SessionID: sess.ID, TurnID: turn.ID,
		Role: repo.RoleSystemEvent, Content: wakeNoteWith(inTx, results, refs)}); err != nil {
		return err
	}
	if err := s.d.Repo.EnqueueTurnInTx(ctx, tx, s.d.NextID(), sess.ID, turn.ID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	st.Woken++
	st.StepsFinished += len(done)
	s.d.Logger.Info("所等任务全部终态，会话已唤醒", "sessionId", sess.ID, "turnId", turn.ID,
		"steps", len(inTx))
	for _, f := range done {
		s.publish(ctx, sess.ProjectID, &AgentStepEvent{Type: EventAgentStep, SessionID: idStr(sess.ID),
			TurnID: idStr(f.step.TurnID), StepID: idStr(f.step.ID), Tool: f.step.Tool, Status: f.status,
			Display: f.display})
	}
	locked.State = to
	s.publishState(ctx, locked, turn.ID, "tasks_done")
	return nil
}

// wakeKey 是唤醒轮的幂等键：同一批等待步骤只唤醒一次（uk_agent_turn_idem 兜底）。
// 没有等待步骤（数据不一致的 waiting_task）时不带键。
func wakeKey(steps []repo.Step) string {
	if len(steps) == 0 {
		return ""
	}
	return fmt.Sprintf("wake:%d:%d:%d", steps[0].ID, steps[len(steps)-1].ID, len(steps))
}

// wakeNote 是唤醒时写的一条系统事件（system_event）：所等任务各自的结局。
// ⚠️ 必须是 system_event 而不是 system_note：唤醒轮的模型要靠它知道成败与产出。
func wakeNote(steps []repo.Step, results map[int64]TaskResult) string {
	return wakeNoteWith(steps, results, nil)
}

// wakeNoteWith 同 wakeNote，另外说清等的组 / 整图运行 / 渲染各自的结局（AGT-50）。
func wakeNoteWith(steps []repo.Step, results map[int64]TaskResult, refs map[int64]refWait) string {
	if len(steps) == 0 {
		return "所等的任务已全部结束，继续执行。"
	}
	var ok, failed, cancelled int
	var lines []string
	for _, step := range steps {
		if step.GenerationID == 0 {
			w := refs[step.ID]
			status, _, errMsg := refStepOutcome(step, w)
			line := fmt.Sprintf("- %s（%s）：", step.Tool, w.label())
			switch status {
			case repo.StepSuccess:
				ok++
				line += nonEmpty(w.res.Summary, "成功")
			case repo.StepCancelled:
				cancelled++
				line += "已取消"
			default:
				failed++
				line += "失败：" + errMsg
			}
			lines = append(lines, line)
			continue
		}
		status, _, errMsg := stepOutcome(step, results[step.GenerationID])
		line := fmt.Sprintf("- %s（任务 %d）：", step.Tool, step.GenerationID)
		switch status {
		case repo.StepSuccess:
			ok++
			line += fmt.Sprintf("成功，产出 %d 个", len(results[step.GenerationID].Outputs))
		case repo.StepCancelled:
			cancelled++
			line += "已取消"
		default:
			failed++
			line += "失败：" + errMsg
		}
		lines = append(lines, line)
	}
	return fmt.Sprintf("所等的 %d 个任务已全部结束：成功 %d、失败 %d、取消 %d。\n%s",
		len(steps), ok, failed, cancelled, strings.Join(lines, "\n"))
}

// stepOutcome 把生成任务的终态换成步骤终态与展示内容。
func stepOutcome(step repo.Step, r TaskResult) (status string, display json.RawMessage, errMsg string) {
	result := map[string]any{"generationId": idStr(step.GenerationID)}
	switch {
	case step.GenerationID == 0:
		status = repo.StepSuccess
	case !r.Found:
		status, errMsg = repo.StepFailed, "任务不存在或已被清理"
	case r.Status == "success":
		status = repo.StepSuccess
		outs := make([]map[string]any, 0, len(r.Outputs))
		for _, o := range r.Outputs {
			item := map[string]any{"idx": o.Idx, "type": o.Type}
			if o.MediaID != 0 {
				item["mediaId"] = idStr(o.MediaID)
			}
			if o.Text != "" {
				item["text"] = truncRunes(o.Text, maxOutputTextRunes)
			}
			outs = append(outs, item)
		}
		result["outputs"] = outs
	case r.Status == "cancelled":
		status, errMsg = repo.StepCancelled, nonEmpty(r.ErrorMessage, "任务已取消")
	default:
		status, errMsg = repo.StepFailed, nonEmpty(r.ErrorMessage, nonEmpty(r.ErrorCode, "任务失败"))
	}
	result["status"] = status
	if errMsg != "" {
		result["error"] = errMsg
	}
	return status, mergeDisplay(step.Display, result), errMsg
}

const maxOutputTextRunes = 200

// mergeDisplay 把任务结局并进步骤原有的展示内容（键 result），保留原有字段（GO-35）。
func mergeDisplay(prev json.RawMessage, result map[string]any) json.RawMessage {
	obj := map[string]json.RawMessage{}
	if len(prev) > 0 && json.Unmarshal(prev, &obj) != nil {
		obj = map[string]json.RawMessage{"previous": prev}
	}
	b, err := json.Marshal(result)
	if err != nil {
		return nil
	}
	obj["result"] = b
	out, err := json.Marshal(obj)
	if err != nil {
		return nil
	}
	return out
}

func nonEmpty(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	if utf8.RuneCountInString(s) > 500 {
		return truncRunes(s, 500)
	}
	return s
}

func stepGenerationIDs(steps []repo.Step) []int64 {
	var ids []int64
	for _, s := range steps {
		if s.GenerationID != 0 {
			ids = append(ids, s.GenerationID)
		}
	}
	return ids
}

// statuses 批量查任务；查不到的补一条 Found=false 的终态结果（不会永远等一个不存在的任务）。
func (s *Scanner) statuses(ctx context.Context, ws int64, ids []int64) (map[int64]TaskResult, error) {
	out := map[int64]TaskResult{}
	if len(ids) == 0 {
		return out, nil
	}
	got, err := s.d.Tasks.Statuses(ctx, ws, ids)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		r, ok := got[id]
		if !ok || !r.Found {
			r = TaskResult{ID: id, Found: false, Status: "failed", Terminal: true, Refunded: true}
		}
		out[id] = r
	}
	return out, nil
}

// ---------------------------------------------------------------- E 额度归还

func (s *Scanner) scanBudget(ctx context.Context, limit int, st *Stats) error {
	cands, err := s.d.Repo.ChargedRefundCandidates(ctx, limit)
	if err != nil {
		return fmt.Errorf("扫描待归还额度: %w", err)
	}
	byWS := map[int64][]repo.RefundCandidate{}
	var order []int64
	for _, c := range cands {
		if _, ok := byWS[c.WorkspaceID]; !ok {
			order = append(order, c.WorkspaceID)
		}
		byWS[c.WorkspaceID] = append(byWS[c.WorkspaceID], c)
	}
	var errs []error
	for _, ws := range order {
		group := byWS[ws]
		ids := make([]int64, 0, len(group))
		for _, c := range group {
			ids = append(ids, c.GenerationID)
		}
		results, err := s.statuses(ctx, ws, ids)
		if err != nil {
			errs = append(errs, fmt.Errorf("查任务状态（工作区 %d）: %w", ws, err))
			continue
		}
		for _, c := range group {
			if err := s.settleEntry(ctx, c, results[c.GenerationID], st); err != nil {
				errs = append(errs, fmt.Errorf("额度条目 %d: %w", c.EntryID, err))
			}
		}
	}
	return errors.Join(errs...)
}

// settleEntry 处理一条 charged 的额度条目：
//   - 生成失败 / 取消且生成域已退款 → 条目 refunded（额度归还），步骤补 failed / cancelled；
//   - 生成成功而步骤还停在 waiting（会话已不在 waiting_task，A 不会替它回写）→ 步骤补 success；
//   - 还没结束，或失败了但退款还没到账 → 不动（生成域的补退款作业会补上，之后再归还）。
//
// ⚠️ 额度条目不是账本：真正的退款由生成域按原批次完成（只退一次），这里只是让
// 「还剩多少授权」跟上 —— 账还没退就先归还额度的话，Agent 能在钱没回来时再花一次。
func (s *Scanner) settleEntry(ctx context.Context, c repo.RefundCandidate, r TaskResult, st *Stats) error {
	if !r.Terminal {
		return nil
	}
	step := repo.Step{ID: c.StepID, TurnID: c.TurnID, Tool: c.Tool, GenerationID: c.GenerationID}
	if c.StepStatus == repo.StepWaiting {
		full, err := s.d.Repo.GetStep(ctx, c.StepID)
		if err != nil {
			return err
		}
		step = full
	}
	status, display, errMsg := stepOutcome(step, r)
	if status == repo.StepFailed || status == repo.StepCancelled {
		if !r.Refunded {
			return nil
		}
		ok, err := s.d.Repo.MarkRefunded(ctx, c.EntryID)
		if err != nil {
			return err
		}
		if ok {
			st.BudgetRefunded++
		}
	}
	if c.StepStatus != repo.StepWaiting {
		return nil
	}
	changed, err := s.d.Repo.FinishStep(ctx, s.d.Repo.Pool(), c.StepID, status, display, errMsg)
	if err != nil {
		return err
	}
	if changed {
		st.StepsFinished++
		s.publish(ctx, c.ProjectID, &AgentStepEvent{Type: EventAgentStep, SessionID: idStr(c.SessionID),
			TurnID: idStr(c.TurnID), StepID: idStr(c.StepID), Tool: c.Tool, Status: status, Display: display})
	}
	return nil
}

// ---------------------------------------------------------------- 事件

func (s *Scanner) publishState(ctx context.Context, sess repo.Session, turnID int64, reason string) {
	ev := &AgentStateEvent{Type: EventAgentState, SessionID: idStr(sess.ID), State: string(sess.State),
		ApprovedBudget: sess.ApprovedBudget, Reason: reason}
	if turnID != 0 {
		ev.TurnID = idStr(turnID)
	}
	s.publish(ctx, sess.ProjectID, ev)
}

// publish 发事件（只是对账信号，失败只记 debug）。
func (s *Scanner) publish(ctx context.Context, projectID int64, ev any) {
	if s.d.Events == nil {
		return
	}
	now := nowStr(s.d.Now())
	switch e := ev.(type) {
	case *AgentStateEvent:
		e.OccurredAt = now
	case *AgentStepEvent:
		e.OccurredAt = now
	case *AgentCardEvent:
		e.OccurredAt = now
	}
	if err := s.d.Events.Publish(ctx, ProjectTopic(projectID), ev); err != nil {
		s.d.Logger.Debug("agent 事件发布失败（只是对账信号）", "error", err)
	}
}
