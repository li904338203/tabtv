package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
)

// 付费工具的统一外壳（AGT-24，dev/01 §5.2）。
//
//	tx: 锁会话行 → 已用 → CheckBudget(批准额度, 已用, 本次报价)
//	    超出 → 写追加额度卡 → commit → 返回挂起（over_budget）
//	    放行 → 写 reserved 条目 → commit
//	事务外 run(幂等键)              // 例如 Generator.Accept：受理自己有事务与 outbox
//	失败 → released（额度归还）；成功 → charged + generation_id
//
// ⚠️ 检查与预留必须在**同一个**锁住会话行的事务里（AG-08）：拆成「先查再插」两步的话，
// 并发的付费工具都会看到「够」然后一起超额。
// ⚠️ run 不在事务里：Provider 调用、MQ 发布、受理的扣费事务都不能被会话行锁挂着。
// ⚠️ 额度条目**不是账本**：它只回答「还剩多少授权」；「花了多少」只从 credit_transaction
// 取（AGT-26 TurnLedger）。agent 不另扣生成费 —— 受理在自己的事务里扣。
// ⚠️ charged → refunded（任务失败退款后额度归还）由扫描循环做（AGT-25），本外壳不碰：
// 它要等生成域真的退了款才能归还，而那发生在本轮早已结束之后。契约是 charged 条目
// 一定挂着业务引用（generation_id，或 AGT-50 的 ref_type / ref_id / ref_ids）。
// 部分退回（组部分成功）由扫描循环按账本结算进 refunded 列，见 scanner_ref.go。

// 付费工具相关的端口错误（由 wiring 适配器从各域错误翻译过来，工具按它分流）。
var (
	// ErrNodeNotOwned 画布节点不是本会话创建、或用户之后改过（canvas ErrNotOwned 的翻译）。
	// 工具据此让模型在旁边新建，而不是重试。
	ErrNodeNotOwned = errors.New("该节点由用户创建或已被用户修改")
	// ErrTaskFinished 任务已经结束（成功 / 失败 / 已取消），无法再取消。
	ErrTaskFinished = errors.New("任务已经结束")
	// ErrNotExecuting 会话不在执行态：付费调用一律拒（规划态的幻觉调用在这里兜底第二道）。
	ErrNotExecuting = errors.New("会话不在执行态，不能发起付费调用")
)

// InsufficientCreditsError 受理时余额不足。Balance<0 表示没取到余额。
// errors.Is(err, ErrInsufficientCredits) 成立。
type InsufficientCreditsError struct {
	Balance int
	Cause   error
}

func (e *InsufficientCreditsError) Error() string {
	if e.Cause != nil {
		return "积分不足: " + e.Cause.Error()
	}
	return "积分不足"
}

func (e *InsufficientCreditsError) Is(target error) bool { return target == ErrInsufficientCredits }

// PaidResult 是付费调用的结果。GenerationID 为关联的生成任务（条目 charged 时挂上）。
type PaidResult struct {
	GenerationID int64
	// Credits 受理实扣的积分；<0 表示不知道（条目保留报价，结算时按账本校正）。
	Credits int
	// RefType / RefID / RefIDs 关联的业务对象（AGT-50，00047）：分镜调用、组、整图运行、渲染任务……
	// 只给 GenerationID 时按 generation 记。扫描循环据它查终态、按账本结算与部分退回。
	RefType string
	RefID   int64
	RefIDs  []int64
	// Replayed：这一步此前已经受理成功（条目已 charged 并挂着引用），本次**没有再调 run**，
	// 结果是那一次的引用。⚠️ 这是没有自身幂等键的能力（整图运行、渲染）防重复的落点：
	// 租约过期重放同一步骤时不会再建第二个运行 / 第二个渲染任务。
	Replayed bool
}

// Budget 是付费调用外壳的实现（锁会话行 + 额度条目）。
type Budget struct {
	repo   *repo.Repo
	nextID func() int64
	log    *slog.Logger
	events Events
}

// SetEvents 接上事件总线：发出追加额度卡后推 agent.card（只是对账信号，失败只记日志）。
func (b *Budget) SetEvents(e Events) { b.events = e }

// NewBudget 建外壳。logger 可为 nil。
func NewBudget(r *repo.Repo, nextID func() int64, logger *slog.Logger) *Budget {
	return &Budget{repo: r, nextID: nextID, log: logger}
}

// BudgetCardPayload 是追加额度卡的完整内容：在 domain.BudgetPayload（批准时读 Requested）
// 之上补本次需要、差额与恢复状态，前端据此说清「这一步要多少、还差多少」。
type BudgetCardPayload struct {
	domain.BudgetPayload
	// Needed 本次调用的报价。
	Needed int `json:"needed"`
	// Shortfall = 已用 + 本次 − 额度（与 Requested 相同：批准即刚好够这一步）。
	Shortfall int `json:"shortfall"`
	// Tool 发起这次付费调用的工具。
	Tool string `json:"tool,omitempty"`
	// ResumeState 批准后回到的状态（恒为 executing）。
	ResumeState domain.State `json:"resumeState"`
}

// PaidCall 在额度内执行一次付费调用（dev/01 §5.2）。
//
// 返回：
//   - 超额度：(零值, 挂起, nil) —— 已写追加额度卡，调用方把挂起原样交给 Runner；run 没有被调用；
//   - run 失败：(零值, nil, run 的错误) —— 条目已 released；
//   - 成功：(run 的结果, nil, nil) —— 条目已 charged。
func (b *Budget) PaidCall(ctx context.Context, rc RunContext, quote int,
	run func(idemKey string) (PaidResult, error)) (PaidResult, *Suspension, error) {
	return b.paidCall(ctx, rc, quote, "", run)
}

// PaidCallTool 同 PaidCall，额外把工具名写进追加额度卡。
func (b *Budget) PaidCallTool(ctx context.Context, rc RunContext, tool string, quote int,
	run func(idemKey string) (PaidResult, error)) (PaidResult, *Suspension, error) {
	return b.paidCall(ctx, rc, quote, tool, run)
}

func (b *Budget) paidCall(ctx context.Context, rc RunContext, quote int, tool string,
	run func(idemKey string) (PaidResult, error)) (PaidResult, *Suspension, error) {
	if quote < 0 {
		return PaidResult{}, nil, fmt.Errorf("报价非法：%d", quote)
	}
	if rc.SessionID == 0 || rc.StepID == 0 {
		return PaidResult{}, nil, errors.New("付费调用缺少会话或步骤")
	}
	attempt := rc.Attempt
	if attempt <= 0 {
		attempt = 1
	}

	entry, susp, replay, err := b.reserve(ctx, rc, quote, tool)
	if err != nil || susp != nil {
		return PaidResult{}, susp, err
	}
	if replay != nil {
		return *replay, nil, nil
	}

	res, runErr := run(domain.PaidIdempotencyKey(rc.SessionID, rc.StepID, attempt))
	// ⚠️ 落条目用脱离取消的 ctx：run 返回之后本轮可能已被停止 / 租约丢失，
	// 而受理成功的那笔要挂上 generation_id，失败的那笔要归还 —— 都不能因为 ctx 取消而丢。
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if runErr != nil {
		if ok, err := b.repo.MarkReleased(sctx, entry.ID); err != nil || !ok {
			b.warn(ctx, "付费调用失败后释放额度条目失败", entry.ID, err)
		}
		return PaidResult{}, nil, runErr
	}
	if ok, err := b.repo.MarkChargedRef(sctx, entry.ID, res.Credits, chargeRef(res)); err != nil || !ok {
		// 受理已经成功（钱已扣）：条目停在 reserved 只会让额度显得更紧，不会超额 —— 告警即可。
		b.warn(ctx, "付费调用成功后标记额度条目失败", entry.ID, err)
	}
	return res, nil, nil
}

// chargeRef 把 run 的结果换成条目上的引用：没给 RefType 而给了 GenerationID 的按 generation 记。
func chargeRef(res PaidResult) repo.ChargeRef {
	ref := repo.ChargeRef{GenerationID: res.GenerationID, Type: res.RefType, ID: res.RefID, IDs: res.RefIDs}
	if ref.Type == "" && res.GenerationID != 0 {
		ref.Type, ref.ID = repo.RefGeneration, res.GenerationID
	}
	return ref
}

// reserve 是外壳的事务部分：锁会话行、判定、预留或发追加额度卡。
// 同一步骤已经受理成功过（条目 charged 且挂着引用）时返回 replay，不再预留。
func (b *Budget) reserve(ctx context.Context, rc RunContext, quote int,
	tool string) (repo.BudgetEntry, *Suspension, *PaidResult, error) {
	tx, err := b.repo.Begin(ctx)
	if err != nil {
		return repo.BudgetEntry{}, nil, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	sess, err := b.repo.LockForUpdate(ctx, tx, rc.SessionID)
	if err != nil {
		return repo.BudgetEntry{}, nil, nil, fmt.Errorf("锁定会话失败: %w", err)
	}
	if sess.WorkspaceID != rc.WorkspaceID {
		return repo.BudgetEntry{}, nil, nil, errors.New("会话不属于该工作区")
	}
	// 以**库里的**状态为准，不信 RunContext 里那份（可能在本轮里已被用户取消）。
	if sess.State != domain.StateExecuting {
		return repo.BudgetEntry{}, nil, nil, fmt.Errorf("%w（当前 %s）", ErrNotExecuting, sess.State)
	}
	// 同一步骤重入（租约过期重放）：已受理的直接回那一次的引用（AGT-50）。
	// ⚠️ 在锁里查：会话行锁保证同一会话不会有两个调用同时走到这里。
	if prev, err := b.repo.BudgetByStepIn(ctx, tx, rc.StepID); err == nil {
		if prev.Status == repo.BudgetCharged && prev.RefType != "" {
			if err := tx.Commit(ctx); err != nil {
				return repo.BudgetEntry{}, nil, nil, err
			}
			return repo.BudgetEntry{}, nil, &PaidResult{GenerationID: prev.GenerationID, Credits: prev.Quoted,
				RefType: prev.RefType, RefID: prev.RefID, RefIDs: prev.RefIDs, Replayed: true}, nil
		}
	} else if !errors.Is(err, repo.ErrNotFound) {
		return repo.BudgetEntry{}, nil, nil, err
	}
	used, err := b.repo.UsedInTx(ctx, tx, sess.ID)
	if err != nil {
		return repo.BudgetEntry{}, nil, nil, err
	}
	dec, err := domain.CheckBudget(sess.ApprovedBudget, used, quote)
	if err != nil {
		return repo.BudgetEntry{}, nil, nil, err
	}

	if !dec.Allow {
		refunded, err := b.repo.RefundedInTx(ctx, tx, sess.ID)
		if err != nil {
			return repo.BudgetEntry{}, nil, nil, err
		}
		reason := fmt.Sprintf("这一步需要 ✦%d，已批准额度 ✦%d 中已占用 ✦%d，还差 ✦%d",
			quote, sess.ApprovedBudget, used, dec.Shortfall)
		payload, err := json.Marshal(BudgetCardPayload{
			BudgetPayload: domain.BudgetPayload{Reason: reason, Requested: dec.Shortfall,
				Approved: sess.ApprovedBudget, Used: used, Refunded: refunded},
			Needed: quote, Shortfall: dec.Shortfall, Tool: tool, ResumeState: domain.StateExecuting,
		})
		if err != nil {
			return repo.BudgetEntry{}, nil, nil, err
		}
		card, err := b.repo.InsertCard(ctx, tx, repo.Card{ID: b.nextID(), SessionID: sess.ID, StepID: rc.StepID,
			Kind: domain.CardBudget, Payload: payload})
		if err != nil {
			return repo.BudgetEntry{}, nil, nil, fmt.Errorf("写追加额度卡失败: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return repo.BudgetEntry{}, nil, nil, err
		}
		b.publishCard(ctx, sess, card.ID)
		return repo.BudgetEntry{}, &Suspension{Event: domain.EvOverBudget, CardID: card.ID, Reason: reason}, nil, nil
	}

	// 同一步骤的下一个 attempt：先复用上一次已归还（released / refunded）的那一行。
	// ⚠️ 先 update 再 insert，不反过来：insert 撞唯一索引会让 PG 把整个事务标成 aborted，
	// 之后的语句一律失败（会话行锁保证同一会话不会并发走到这里）。
	entry, err := b.repo.ReReserveInTx(ctx, tx, rc.StepID, quote)
	if errors.Is(err, repo.ErrDuplicate) {
		entry, err = b.repo.InsertReserved(ctx, tx, repo.BudgetEntry{ID: b.nextID(), SessionID: sess.ID,
			StepID: rc.StepID, Quoted: quote})
		if errors.Is(err, repo.ErrDuplicate) {
			// 上一次仍是 reserved / charged：同一步骤不能占两份额度。
			return repo.BudgetEntry{}, nil, nil, errors.New("该步骤已有进行中或已受理的付费调用")
		}
	}
	if err != nil {
		return repo.BudgetEntry{}, nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return repo.BudgetEntry{}, nil, nil, err
	}
	return entry, nil, nil, nil
}

func (b *Budget) publishCard(ctx context.Context, sess repo.Session, cardID int64) {
	if b.events == nil {
		return
	}
	ev := &AgentCardEvent{Type: EventAgentCard, OccurredAt: nowStr(time.Now()), SessionID: idStr(sess.ID),
		CardID: idStr(cardID), Kind: string(domain.CardBudget), Status: string(domain.CardPending)}
	if err := b.events.Publish(ctx, ProjectTopic(sess.ProjectID), ev); err != nil && b.log != nil {
		b.log.DebugContext(ctx, "agent.card 事件发布失败（只是对账信号）", "cardId", cardID, "error", err)
	}
}

func (b *Budget) warn(ctx context.Context, msg string, entryID int64, err error) {
	if b.log != nil {
		b.log.WarnContext(ctx, msg, "entryId", entryID, "error", err)
	}
}
