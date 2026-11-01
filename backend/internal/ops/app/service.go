// Package app 编排任务运维用例：死信处置、受控重试、人工补偿（P13-03）。
//
// ⚠️ 这一域**自己不碰任务表**。重试与补偿都通过端口交给拥有那张表的域
// （生成域、时间线域）执行 —— 在这里直接 `update generation set status=...`
// 的话，就会出现第二条改任务状态的路径，而它必然与 Worker 那条不一致
// （退款、组子项回写、整图推进这三件事在这里一件都看不见）。
//
// ⚠️ 也**没有任何一条路能绕过账本**：退款一律由各域的 `Refunder` 按原消费
// 批次回补，后台不存在"直接把积分加回去"的入口。
package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/li904338203/tabtv/backend/internal/ops/domain"
	"github.com/li904338203/tabtv/backend/internal/ops/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// TaskFacts 是补偿判定所需的最小事实，由各域给出。
type TaskFacts struct {
	Finished   bool
	DeadlineAt *time.Time
	Credits    int
}

// TaskOps 由拥有任务表的域实现（生成域、时间线域）。
//
// ⚠️ 只有两个方法，而且都**不接受"要不要退款"这类参数**：退多少、按哪个
// 批次退、退不退得成，全由实现方按自己的账务规则决定。让调用方报金额的话，
// 后台就有了一条能指定退款数额的路径 —— 那正是"绕过账本"。
type TaskOps interface {
	// Describe 取补偿判定事实。
	Describe(ctx context.Context, id int64) (TaskFacts, error)
	// Compensate 判失败并退款，返回**本次是否真的改了状态**。
	//
	// ⚠️ 返回 false 不是错误，是"别人先动了"（任务刚好跑完、另一个运维
	// 先点了）。调用方必须据此回冲突，**不能当成功** —— 当成功的话运维
	// 以为自己处置了，而那条任务其实是成功结束的。
	Compensate(ctx context.Context, id int64, reason string) (bool, error)
}

// Retrier 由生成域实现：代用户发起一次重试。
type Retrier interface {
	RetryForOps(ctx context.Context, id int64, idempotencyKey string) (
		newID int64, credits int, err error)
}

// Republisher 把一条死信原样投回原队列。
type Republisher interface {
	Publish(ctx context.Context, routingKey string, payload []byte) error
}

type Service struct {
	repo     *repo.Repo
	tasks    map[domain.TaskKind]TaskOps
	retrier  Retrier
	republic Republisher
	audit    *audit.Logger
	log      *slog.Logger
}

func New(r *repo.Repo) *Service {
	return &Service{repo: r, tasks: map[domain.TaskKind]TaskOps{}, log: slog.Default()}
}

func (s *Service) SetAudit(a *audit.Logger)              { s.audit = a }
func (s *Service) SetLogger(l *slog.Logger)              { s.log = l }
func (s *Service) SetRetrier(r Retrier)                  { s.retrier = r }
func (s *Service) SetRepublisher(p Republisher)          { s.republic = p }
func (s *Service) Register(k domain.TaskKind, o TaskOps) { s.tasks[k] = o }

func (s *Service) writeAudit(ctx context.Context, e audit.Entry) {
	if s.audit != nil {
		s.audit.Write(ctx, e)
	}
}

// ---------------------------------------------------------------- 死信落库

// RecordDead 由 Worker 的排空器调用。
//
// ⚠️ 尽力从消息体里认出「这是哪个任务」，认不出来**也要落库**：
// `AckDead` 的第一种情形就是"消息解不开"，而那一条恰恰最需要人看
// —— 因为库里没有任何别的地方记着它。
func (s *Service) RecordDead(ctx context.Context, queue, routingKey, reason string,
	body []byte) error {
	aggType, aggID := domain.SniffAggregate(body)
	d, created, err := s.repo.Record(ctx, repo.DeadLetter{
		Queue: queue, RoutingKey: routingKey, Payload: string(body),
		AggregateType: aggType, AggregateID: aggID, Reason: reason,
	})
	if err != nil {
		return err
	}
	if !created {
		// 重复投递是 at-least-once 的**正常结局**，不是故障 ——
		// 按告警记的话死信告警会被重复消息淹没。
		s.log.Debug("死信重复投递，已有待处理记录", "queue", queue)
		return nil
	}
	// 真的新增一条是要有人看的：这是唯一会主动提醒运维的地方。
	s.log.Warn("收到死信，已落库待人工处置",
		"deadLetterId", d.ID, "queue", queue, "aggregateType", aggType,
		"aggregateId", aggID, "reason", reason)
	return nil
}

// ---------------------------------------------------------------- 死信处置

func (s *Service) ListDead(ctx context.Context, f repo.DeadFilter) ([]repo.DeadLetter, int, error) {
	list, err := s.repo.ListDead(ctx, f)
	if err != nil {
		return nil, 0, httpx.Wrap(errcode.Internal, err)
	}
	pending, err := s.repo.CountDeadPending(ctx)
	if err != nil {
		return nil, 0, httpx.Wrap(errcode.Internal, err)
	}
	return list, pending, nil
}

// ReplayDead 把一条死信投回原队列。
//
// ⚠️ **先 CAS 标记、再投递**，顺序不能反。
//
// 反过来（先投递再标记）的话，标记失败时它仍显示 pending —— 运维会再点
// 一次，于是同一条消息被执行两遍。而死信里有一类是「提交结果不确定，
// 可能已在上游计费」，那一类重投两次就是**让用户被计两次费**。
//
// 代价是：投递失败时这条记录已经是 `replayed` 了。所以那种情况**不回滚
// 状态**，而是把失败原因**追加**到备注里并明确报错 —— 退回 pending 的话
// 另一个运维会再投一次，而第一次可能其实已经投出去了。
func (s *Service) ReplayDead(ctx context.Context, adminID, id int64, note string) (
	repo.DeadLetter, error) {
	cur, err := s.repo.GetDead(ctx, id)
	if err != nil {
		return repo.DeadLetter{}, httpx.Err(errcode.NotFound)
	}
	if err := domain.CanReplay(cur.Status); err != nil {
		return repo.DeadLetter{}, httpx.Errf(errcode.InvalidStateTransition, err.Error())
	}
	if s.republic == nil {
		return repo.DeadLetter{}, httpx.Errf(errcode.Internal, "消息投递未接入")
	}

	out, err := s.repo.MarkDead(ctx, id, domain.DeadReplayed, adminID, note, true)
	if errors.Is(err, repo.ErrNotFound) {
		// CAS 落空：另一个运维先动了。
		return repo.DeadLetter{}, httpx.Errf(errcode.Conflict,
			"这条死信刚被其他人处置过，请刷新后确认")
	}
	if err != nil {
		return repo.DeadLetter{}, httpx.Wrap(errcode.Internal, err)
	}

	s.writeAudit(ctx, audit.Entry{
		ActorType: audit.ActorAdmin, ActorID: adminID,
		Action: "dead_letter.replay", ObjectType: "dead_letter", ObjectID: id,
		Before: map[string]any{"status": string(cur.Status)},
		After:  map[string]any{"status": string(out.Status), "queue": cur.Queue},
		Reason: note,
	})

	// ⚠️ 用**原始 routing key**，不按队列名猜一个：猜错的话消息会落到别的
	// 消费者手上、解不开、再进一次死信 —— 看起来像"重投没生效"。
	if err := s.republic.Publish(ctx, cur.RoutingKey, []byte(cur.Payload)); err != nil {
		if e := s.repo.AppendDeadNote(ctx, id, "重投时投递失败："+err.Error()); e != nil {
			s.log.Warn("追加死信备注失败", "deadLetterId", id, "error", e)
		}
		return repo.DeadLetter{}, httpx.Errf(errcode.DependencyUnavailable,
			"这条死信已标记为已重投，但投递到队列失败了（原因已记在备注里）；"+
				"请先确认队列是否可用，不要重复点击")
	}
	return out, nil
}

// DiscardDead 丢弃一条死信。理由必填。
func (s *Service) DiscardDead(ctx context.Context, adminID, id int64, note string) (
	repo.DeadLetter, error) {
	cur, err := s.repo.GetDead(ctx, id)
	if err != nil {
		return repo.DeadLetter{}, httpx.Err(errcode.NotFound)
	}
	if err := domain.CanDiscard(cur.Status, note); err != nil {
		code := errcode.InvalidStateTransition
		if errors.Is(err, domain.ErrNoteRequired) {
			code = errcode.InvalidArgument
		}
		return repo.DeadLetter{}, httpx.Errf(code, err.Error())
	}
	out, err := s.repo.MarkDead(ctx, id, domain.DeadDiscarded, adminID, note, false)
	if errors.Is(err, repo.ErrNotFound) {
		return repo.DeadLetter{}, httpx.Errf(errcode.Conflict,
			"这条死信刚被其他人处置过，请刷新后确认")
	}
	if err != nil {
		return repo.DeadLetter{}, httpx.Wrap(errcode.Internal, err)
	}
	s.writeAudit(ctx, audit.Entry{
		ActorType: audit.ActorAdmin, ActorID: adminID,
		Action: "dead_letter.discard", ObjectType: "dead_letter", ObjectID: id,
		Before: map[string]any{"status": string(cur.Status)},
		After:  map[string]any{"status": string(out.Status), "queue": cur.Queue},
		Reason: note,
	})
	return out, nil
}

// ---------------------------------------------------------------- 补偿与重试

// CompensateInput 是一次人工补偿的请求。
type CompensateInput struct {
	Kind    domain.TaskKind
	TaskID  int64
	Reason  string
	Force   bool
	AdminID int64
}

// Compensate 人工把一条卡住的任务判失败并退款。
func (s *Service) Compensate(ctx context.Context, in CompensateInput, now time.Time) error {
	ops, ok := s.tasks[in.Kind]
	if !ok {
		return httpx.Errf(errcode.InvalidArgument, domain.ErrBadKind.Error())
	}
	facts, err := ops.Describe(ctx, in.TaskID)
	if err != nil {
		return err
	}
	if err := domain.CheckCompensate(domain.CompensateInput{
		Kind: in.Kind, Finished: facts.Finished, DeadlineAt: facts.DeadlineAt,
		Reason: in.Reason, Force: in.Force,
	}, now); err != nil {
		code := errcode.InvalidArgument
		if errors.Is(err, domain.ErrAlreadyFinished) {
			// ⚠️ "已经结束了"是**状态**问题不是参数问题：报成参数错误的话
			// 运维会去改自己的输入，而输入一个字都没错。
			code = errcode.InvalidStateTransition
		}
		return httpx.Errf(code, err.Error())
	}

	changed, err := ops.Compensate(ctx, in.TaskID, in.Reason)
	if err != nil {
		return err
	}
	if !changed {
		return httpx.Errf(errcode.Conflict,
			"这条任务刚刚已经结束了（可能自己跑完了，或另一个人先处置过），未做改动")
	}
	s.writeAudit(ctx, audit.Entry{
		ActorType: audit.ActorAdmin, ActorID: in.AdminID,
		Action: "task.compensate", ObjectType: string(in.Kind), ObjectID: in.TaskID,
		After: map[string]any{
			"status": "failed", "errorCode": "ops_compensated",
			"refundedCredits": facts.Credits, "force": in.Force,
		},
		Reason: in.Reason,
	})
	return nil
}

// RetryResult 是一次受控重试的结果。
type RetryResult struct {
	NewTaskID int64
	Credits   int
}

// Retry 由运维代发起一次重试。
//
// ⚠️ **重新扣费**，且扣的是**原任务付费主体**的账。后台没有"免费重试"：
// 那是一个商业决定（平台替用户吃掉上游成本），不能由一个运维按钮隐式做出。
// 余额不足时会失败 —— 那条错误要原样带给运维，他才知道该去让用户充值。
func (s *Service) Retry(ctx context.Context, adminID, taskID int64, reason string) (
	RetryResult, error) {
	if s.retrier == nil {
		return RetryResult{}, httpx.Errf(errcode.Internal, "重试未接入")
	}
	if reason == "" {
		// 与人工调账同一条：这里是管理员自己在操作，没有用户在等着，
		// 补一个默认理由等于让审计记录失去意义。
		return RetryResult{}, httpx.Errf(errcode.InvalidArgument, "受控重试必须填写原因")
	}
	// ⚠️ 幂等键带 `ops:` 前缀与任务 ID：同一条任务被连点两次只产生一个新任务。
	// ⚠️ 但它**不能只有任务 ID** —— 那样第二次真正需要的重试会命中第一次那条
	// 记录，运维永远点不动第二次（与脚本轮次、整图 attempt 同一个坑，已踩两次）。
	// 这里用时间戳到秒：连点防得住，隔一会儿再点是一次新的决定。
	key := "ops:retry:" + itoa(taskID) + ":" + itoa(time.Now().Unix())
	newID, credits, err := s.retrier.RetryForOps(ctx, taskID, key)
	if err != nil {
		return RetryResult{}, err
	}
	s.writeAudit(ctx, audit.Entry{
		ActorType: audit.ActorAdmin, ActorID: adminID,
		Action: "task.retry", ObjectType: "generation", ObjectID: taskID,
		After:  map[string]any{"newTaskId": itoa(newID), "credits": credits},
		Reason: reason,
	})
	return RetryResult{NewTaskID: newID, Credits: credits}, nil
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [24]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
