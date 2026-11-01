package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"github.com/li904338203/tabtv/backend/internal/platform/usererr"
	"time"

	"github.com/li904338203/tabtv/backend/internal/generation/domain"
	"github.com/li904338203/tabtv/backend/internal/generation/repo"
)

// Provider 是上游适配器。协议实现在 P06；这里只依赖最小能力。
type Provider interface {
	// Submit 提交任务。⚠️ 实现必须能区分「上游明确拒绝」与
	// 「不知道上游收到没有」（网络超时、连接中断）—— 后者返回 SubmitUncertain，
	// 决定重发与否的规则不在适配器里（见 domain.ResolveUncertain）。
	Submit(ctx context.Context, job Job) (SubmitResult, error)
	// Query 查询异步任务。
	Query(ctx context.Context, job Job, providerTaskID string) (QueryResult, error)
	// CanQueryByIdempotencyKey 上游是否支持按我们的幂等键反查。
	// 这决定了「提交不确定窗口」能不能自愈。
	CanQueryByIdempotencyKey() bool
}

// Job 是交给适配器的不可变工作项。
//
// ⚠️ 适配器**自己不查用户数据**：它拿到的是已完成归属校验、
// 输入顺序确定、参数快照不可变的工作项（08 §6）。
type Job struct {
	GenerationID   int64
	IdempotencyKey string
	OperationCode  string
	ModelCode      string
	ResultType     string
	Params         json.RawMessage
	Inputs         json.RawMessage
}

type SubmitResult struct {
	Outcome        domain.SubmitOutcome
	ProviderTaskID string
	// Outputs 仅同步协议（SubmitCompleted）使用：一次调用就拿到了产物。
	Outputs []ProviderOutput
	// Raw 是上游原始响应，落库供排查。
	Raw json.RawMessage
	// HTTPStatus 供重试分类；0 表示不是 HTTP 错误。
	HTTPStatus int
	Message    string
}

type QueryResult struct {
	Done       bool
	Success    bool
	Progress   int
	Outputs    []ProviderOutput
	Raw        json.RawMessage
	HTTPStatus int
	Message    string
}

// Refunder 由账本域实现：任务失败时退款。
type Refunder interface {
	Refund(ctx context.Context, payerID int64, refType, refID, reason string) error
}

// Executor 执行一个生成任务。
//
// ⚠️ 消费**必须幂等**：MQ 是 at-least-once，同一条消息会重复到达。
// 幂等靠 `ClaimForRun` 的 CAS —— 第二次领取影响 0 行，直接 ack 丢弃，
// 不靠"先查状态再执行"（查和改之间别的 Worker 就领走了）。
type Executor struct {
	repo     *repo.Repo
	provider Provider
	refunder Refunder
	log      *slog.Logger
	// MaxRunningPerPayer 跨实例的并发上限。0 表示不限。
	MaxRunningPerPayer int
	// PollInterval 异步任务的轮询间隔。
	PollInterval time.Duration

	archiver    MediaArchiver
	notifier    Notifier
	assetBinder AssetBinder
	groupBinder GroupItemBinder
	runAdvancer RunAdvancer
	smartcut    SmartcutBinder
	events      EventPublisher
	works       WorkRegistrar
	nextID      func() int64
}

func NewExecutor(r *repo.Repo, p Provider, log *slog.Logger) *Executor {
	return &Executor{
		repo: r, provider: p, log: log, nextID: func() int64 { return 0 },
		MaxRunningPerPayer: 3,
		PollInterval:       3 * time.Second,
	}
}

func (e *Executor) SetRefunder(r Refunder)  { e.refunder = r }
func (e *Executor) SetIDGen(f func() int64) { e.nextID = f }

// Outcome 告诉调用方这条消息该怎么 ack。
type Outcome string

const (
	// OutcomeDone 已处理（成功或明确失败）：ack。
	OutcomeDone Outcome = "done"
	// OutcomeDuplicate 重复消息或任务已是终态：ack 丢弃。
	OutcomeDuplicate Outcome = "duplicate"
	// OutcomeRequeue 暂时不能处理（并发达上限）：nack 重回队列。
	// ⚠️ 不是失败 —— 判失败的话，用户批量提交十个任务会有七个直接失败。
	OutcomeRequeue Outcome = "requeue"
	// OutcomeRetry 可重试失败：nack 重回队列，由退避控制节奏。
	OutcomeRetry Outcome = "retry"
	// OutcomeDead 已判失败并退款，但这条消息要留档供人工核对。
	//
	// ⚠️ 只用于「提交结果不确定」：请求可能已经到了上游、可能已在那边计费，
	// 而我们没有任何凭据去认领它。库里的 generation 行只能说"失败了"，
	// 说不出"去上游查一下有没有一个孤儿任务"—— 那正是死信队列要承载的信息。
	// 普通的重试耗尽**不用这个**：那条 generation 已经是 failed、有错误码、
	// 已退款，运维按状态就能捞到，再复制一份进 MQ 只会让两份记录早晚不一致。
	OutcomeDead Outcome = "dead"
)

// Execute 处理一条任务消息。
func (e *Executor) Execute(ctx context.Context, generationID int64) (Outcome, error) {
	g, err := e.repo.ClaimForRun(ctx, generationID, e.MaxRunningPerPayer)
	switch {
	case errors.Is(err, repo.ErrNotClaimable):
		// 已被别的 Worker 领走、已取消、已完成 —— 重复消息的正常结局。
		return OutcomeDuplicate, nil
	case errors.Is(err, repo.ErrNotFound):
		// 任务不在了（彻底删除）。消息丢弃，不算错误。
		return OutcomeDuplicate, nil
	case errors.Is(err, repo.ErrConcurrencyLimit):
		return OutcomeRequeue, nil
	case err != nil:
		return OutcomeRetry, err
	}

	// ⚠️ 死线用**存下来的那个**判，不按当前配置重算。
	if domain.ExpiredBy(g.DeadlineAt, time.Now()) {
		e.fail(ctx, g, domain.ErrorCodeTimeout, "任务已超过死线", nil)
		return OutcomeDone, nil
	}

	job := Job{
		GenerationID: g.ID, IdempotencyKey: g.IdempotencyKey,
		OperationCode: opCodeOf(g), ModelCode: modelCodeOf(g),
		ResultType: g.ResultType, Params: g.ParamsSnapshot, Inputs: g.InputsSnapshot,
	}

	res, err := e.provider.Submit(ctx, job)
	if err != nil && res.Outcome == "" {
		// 适配器没给出结论：按不确定处理，**不当作没发生过**。
		res.Outcome = domain.SubmitUncertain
		res.Message = err.Error()
	}

	switch res.Outcome {
	case domain.SubmitCompleted:
		// ⚠️ 同步协议也必须走归档：直接 Finish 会跳过产物落库与通知，
		// 用户看到"成功"却在产物列表里什么都没有。
		ok, err := e.Complete(ctx, g, res.Outputs, res.Raw)
		if err != nil {
			return OutcomeRetry, err
		}
		if !ok {
			return OutcomeDuplicate, nil
		}
		return OutcomeDone, nil

	case domain.SubmitAccepted:
		// ⚠️ 先落 task ID 再轮询：崩在落库之前就是不确定窗口，
		// 恢复扫描会按 `attempt>0 且 provider_task_id is null` 捞出来专门处理。
		if err := e.repo.RecordProviderTask(ctx, g.ID, res.ProviderTaskID); err != nil {
			return OutcomeRetry, err
		}
		return e.poll(ctx, g, job, res.ProviderTaskID)

	case domain.SubmitRejected:
		class := domain.ClassifyHTTP(res.HTTPStatus)
		if class == domain.RetryTransient && g.Attempt < domain.MaxAttempts {
			if _, err := e.repo.ReleaseToQueue(ctx, g.ID); err != nil {
				return OutcomeRetry, err
			}
			return OutcomeRetry, nil
		}
		e.fail(ctx, g, "provider_rejected", res.Message, res.Raw)
		return OutcomeDone, nil

	default: // SubmitUncertain
		retry, code := domain.ResolveUncertain(e.provider.CanQueryByIdempotencyKey())
		if retry {
			// 支持反查：放回队列，下一轮先查再决定是否重提。
			if _, err := e.repo.ReleaseToQueue(ctx, g.ID); err != nil {
				return OutcomeRetry, err
			}
			return OutcomeRetry, nil
		}
		// ⚠️ 不支持反查时**绝不重发** —— 重发可能在上游创建第二个任务、
		// 让用户被上游计费两次。判失败并退款，错误码单开一个，
		// 让运维能捞出这批"可能已在上游消费"的任务人工核对。
		e.log.Warn("提交结果不确定，已判失败待人工核对",
			"generationId", g.ID, "message", res.Message)
		e.fail(ctx, g, code, "提交结果不确定，已退款，请重新发起", res.Raw)
		return OutcomeDead, nil
	}
}

// poll 轮询异步任务直到完成或到死线。
func (e *Executor) poll(ctx context.Context, g repo.Generation, job Job, taskID string) (Outcome, error) {
	ticker := time.NewTicker(e.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// 进程退出：任务留在 running，恢复扫描会接手。
			return OutcomeRetry, ctx.Err()
		case <-ticker.C:
		}

		if domain.ExpiredBy(g.DeadlineAt, time.Now()) {
			e.fail(ctx, g, domain.ErrorCodeTimeout, "等待上游结果超过死线", nil)
			return OutcomeDone, nil
		}

		q, err := e.provider.Query(ctx, job, taskID)
		if err != nil {
			// ⚠️ 轮询失败**不增加 attempt**（GO-16）：attempt 记的是
			// "我们向上游提交了几次"，把查询失败算进去会让任务提前耗尽重试次数。
			e.log.Warn("轮询失败，稍后重试", "generationId", g.ID, "error", err)
			continue
		}
		if !q.Done {
			_ = e.repo.Heartbeat(ctx, g.ID, q.Progress)
			continue
		}
		if !q.Success {
			e.fail(ctx, g, "provider_failed", q.Message, q.Raw)
			return OutcomeDone, nil
		}
		ok, err := e.Complete(ctx, g, q.Outputs, q.Raw)
		if err != nil {
			return OutcomeRetry, err
		}
		if !ok {
			// 任务已经是终态了（被取消、被恢复扫描判超时）——
			// **不要退款**：那笔退款可能已经发生过。
			return OutcomeDuplicate, nil
		}
		return OutcomeDone, nil
	}
}

// fail 写终态并退款。⚠️ 只有**真的改动了状态**才退款。
func (e *Executor) fail(ctx context.Context, g repo.Generation, code, msg string, raw json.RawMessage) {
	// ⚠️ 上游 / Provider 层的原话不给用户看（2026-10-31 用户要求）：翻成中文文案后，任务状态、退款流水原因、
	// 组子项、整图运行、智能剪辑、站内通知拿到的都是这句；原话只进 error_detail 与日志。
	detail := ""
	if code == "provider_rejected" || code == "provider_failed" {
		detail, msg = msg, usererr.FromUpstream(msg)
		e.log.Warn("生成失败（上游原话）", "generationId", g.ID, "code", code, "detail", detail)
	}
	ok, err := e.repo.Finish(ctx, repo.FinishInput{
		ID: g.ID, Status: domain.StatusFailed, ErrorCode: code, ErrorMessage: msg, ErrorDetail: detail, RawResult: raw,
	})
	if err != nil {
		e.log.Error("写失败状态出错", "generationId", g.ID, "error", err)
		return
	}
	if !ok {
		// 已是终态：晚到的失败不得覆盖，也不得触发第二次退款。
		return
	}
	e.refund(ctx, g, msg)
	// 资产图失败也要回写：停在 generating 的话用户既看不到图也不敢重试。
	e.bindAsset(ctx, g, 0, domain.StatusFailed)
	e.bindGroupItem(ctx, g, domain.StatusFailed, msg)
	e.advanceRun(ctx, g, domain.StatusFailed, code, msg)
	bindSmartcut(ctx, e.smartcut, e.log, g, domain.StatusFailed, nil, msg)
	e.afterTerminal(ctx, g, domain.StatusFailed, nil, code, msg)
}

func (e *Executor) refund(ctx context.Context, g repo.Generation, reason string) {
	if e.refunder == nil || g.CreditsCost <= 0 {
		return
	}
	if err := e.refunder.Refund(ctx, g.PayerAccountID, "generation", itoa(g.ID), reason); err != nil {
		// ⚠️ 退款失败只告警不重试整个任务：任务状态已经落定，
		// 重跑会把一个已失败的任务又变回 running。退款的一次性由账本的唯一索引兜底，
		// 这里失败的话由对账作业捞出来补。
		e.log.Error("退款失败，需对账补偿",
			"generationId", g.ID, "payerAccountId", g.PayerAccountID, "error", err)
	}
}

// RecoverStale 恢复扫描：处理超时、不确定窗口与孤儿排队任务。
func (e *Executor) RecoverStale(ctx context.Context, orphanAfter time.Duration, limit int) int {
	list, err := e.repo.FindStale(ctx, orphanAfter, limit)
	if err != nil {
		// ⚠️ RecoverStale 失败时要看得见：它是超时任务的**唯一**兜底，
		// 静默失效的后果是一批任务永远停在 running、永远不退款，
		// 而监控上什么都不红。日志里带上是哪一步失败的。
		e.log.Error("恢复扫描失败，超时任务将不会被处理", "error", err)
		return 0
	}
	handled := 0
	for _, s := range list {
		switch s.Kind {
		case repo.StaleExpired:
			e.fail(ctx, s.Generation, domain.ErrorCodeTimeout, "任务超过死线未完成", nil)
			handled++
		case repo.StaleUncertain:
			retry, code := domain.ResolveUncertain(e.provider != nil &&
				e.provider.CanQueryByIdempotencyKey())
			if retry {
				if ok, _ := e.repo.ReleaseToQueue(ctx, s.ID); ok {
					handled++
				}
				continue
			}
			e.fail(ctx, s.Generation, code, "提交结果不确定，已退款，请重新发起", nil)
			handled++
		case repo.StaleOrphanQueued:
			// 受理成功却一直没被领取：投递丢了或 Worker 全挂过。
			// ⚠️ 这里**不改状态**，只告警 —— 任务本身没问题，
			// 需要的是重新投递（由 outbox 补投或人工触发），改成失败反而丢掉了它。
			e.log.Warn("任务长时间未被领取，可能投递丢失",
				"generationId", s.ID, "createdAt", s.CreatedAt)
		}
	}
	return handled
}

// RecoverRefunds 给「扣了费、失败了、却没有退款」的任务补退款（CD-82），返回补了几笔。
//
// ⚠️ 失败路径上的退款失败只告警（见 refund），注释里说的「对账作业」此前并不存在 ——
// 这就是它。退款仍走同一个 Refunder（按原消费批次回补、过期批次发补偿），
// 一次性由账本唯一索引兜底：与失败路径的退款撞上时那边已经退了，这里拿到的是「已退过」。
func (e *Executor) RecoverRefunds(ctx context.Context, settleAfter time.Duration, limit int) int {
	if e.refunder == nil {
		return 0
	}
	list, err := e.repo.FindUnrefundedFailures(ctx, settleAfter, limit)
	if err != nil {
		e.log.Error("补退款扫描失败", "error", err)
		return 0
	}
	done := 0
	for _, g := range list {
		if err := e.refunder.Refund(ctx, g.PayerAccountID, "generation", itoa(g.ID),
			"失败任务补退款："+g.ErrorMessage); err != nil {
			e.log.Error("补退款失败，下一轮再试", "generationId", g.ID, "error", err)
			continue
		}
		e.log.Info("已为失败任务补退款", "generationId", g.ID, "credits", g.CreditsCost)
		done++
	}
	return done
}

func opCodeOf(g repo.Generation) string    { return snapshotString(g.ParamsSnapshot, "operationCode") }
func modelCodeOf(g repo.Generation) string { return snapshotString(g.ParamsSnapshot, "modelCode") }

// snapshotString 从参数快照的 `_snapshot` 段里取值。
//
// ⚠️ 从**快照**取而不是回查配置表：后台此后改模型绑定都不得改变
// 这次任务的解释（06 §3.1）。
func snapshotString(params json.RawMessage, key string) string {
	var tree struct {
		Snapshot map[string]any `json:"_snapshot"`
	}
	if err := json.Unmarshal(params, &tree); err != nil {
		return ""
	}
	if v, ok := tree.Snapshot[key].(string); ok {
		return v
	}
	return ""
}
