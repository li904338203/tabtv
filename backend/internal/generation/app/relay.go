package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/li904338203/tabtv/backend/internal/generation/repo"
)

// Publisher 把消息发到 MQ。由平台的 MQ 客户端实现。
type Publisher interface {
	Publish(ctx context.Context, topic string, payload []byte) error
}

// Relay 是 outbox 投递器：把已提交的 outbox 消息发到 MQ。
//
// ⚠️ 它保证的是 **at-least-once**，不是 exactly-once：
// "标记已发"与"MQ 收到"之间永远有一个窗口，崩在那里就会重发。
// 因此**消费端必须幂等**（先查状态再执行）—— 这是 Worker 的责任，
// 不是投递器能解决的问题（GO-13、FLOW-02）。
type Relay struct {
	repo       *repo.Repo
	pub        Publisher
	log        *slog.Logger
	interval   time.Duration
	batch      int
	maxAttempt int
}

func NewRelay(r *repo.Repo, pub Publisher, log *slog.Logger) *Relay {
	return &Relay{
		repo: r, pub: pub, log: log,
		// 间隔要短：这是任务从"受理成功"到"进入队列"的延迟。
		interval: 2 * time.Second,
		batch:    100,
		// 超过这个次数仍失败就只告警不再刷屏；消息**不删**，
		// 留着等人处理 —— 删了就等于这个任务永远不会被执行。
		maxAttempt: 10,
	}
}

func (r *Relay) SetInterval(d time.Duration) { r.interval = d }

func (r *Relay) Run(ctx context.Context) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	r.Once(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Once(ctx)
		}
	}
}

// Once 投递一批，返回成功条数。
func (r *Relay) Once(ctx context.Context) int {
	msgs, err := r.repo.ClaimOutbox(ctx, r.batch)
	if err != nil {
		r.log.Error("认领待投递消息失败", "error", err)
		return 0
	}
	sent := 0
	for _, m := range msgs {
		if err := r.pub.Publish(ctx, m.Topic, m.Payload); err != nil {
			if m.Attempt < r.maxAttempt {
				r.log.Warn("投递失败，稍后重试",
					"outboxId", m.ID, "attempt", m.Attempt, "error", err)
			} else if m.Attempt == r.maxAttempt {
				// 只在越过阈值那一次告警，避免日志刷屏淹没别的问题。
				r.log.Error("投递持续失败，消息保留待人工处理",
					"outboxId", m.ID, "aggregateId", m.Aggregate, "error", err)
			}
			_ = r.repo.MarkOutboxFailed(ctx, m.ID, err.Error())
			continue
		}
		// ⚠️ 先发后标：反过来的话，标记成功但发送失败的消息会永远不再投递，
		// 而那个任务已经扣过费、已经在库里排队。宁可重发也不能不发。
		if err := r.repo.MarkOutboxPublished(ctx, m.ID); err != nil {
			r.log.Error("标记已投递失败（消息可能被重发，消费端需幂等）",
				"outboxId", m.ID, "error", err)
			continue
		}
		sent++
	}
	return sent
}
