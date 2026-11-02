// Package memqueue 是进程内消息队列（TT-T-07，dev/01 TTD-9），替代来源系统的 RabbitMQ。
//
// 语义照抄来源系统的消费约定：
//   - 每条队列绑定一个 topic，Publish 按 topic 路由；
//   - 回调返回 Done / Requeue / Discard / Dead：Requeue **带退避**重投（来源是立即重投，
//     进程内照搬会在「并发已满」时空转 CPU，dev/01 修订块 R-1），Dead 交给死信落库函数；
//   - 每条队列 N 个消费协程（N = 原来的消费者数 / prefetch），回调同步执行。
//
// ⚠️ 持久化不在这里：消息先写进 PG 的 outbox 再由投递器 Publish 进来，进程内只有投递。
// 进程异常退出时队列里没处理完的消息会丢，由启动时重放 outbox（desktop 装配）兜底 ——
// 四类消费者都是幂等的，重放已完成的任务只会得到 Duplicate。
package memqueue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Action 是一次消费的结局。
type Action int

const (
	// Done 处理完成。
	Done Action = iota
	// Requeue 稍后再试（并发上限、可重试失败），带退避。
	Requeue
	// Discard 正常丢弃：重复消息、任务已不存在。
	Discard
	// Dead 交给死信：消息解不开，或提交结果不确定需要人核对。
	Dead
)

// Handler 处理一条消息。
type Handler func(ctx context.Context, body []byte) Action

// DeadSink 记录一条死信（落 dead_letter 表）。失败只告警。
type DeadSink func(ctx context.Context, queue, topic, reason string, body []byte) error

// ErrNoQueue 表示没有队列绑定这个 topic。
var ErrNoQueue = errors.New("memqueue: 没有队列绑定这个 topic")

type message struct {
	body    []byte
	attempt int
}

type queue struct {
	name    string
	topic   string
	workers int
	handler Handler

	mu      sync.Mutex
	items   []message
	signal  chan struct{}
	pending sync.WaitGroup // 已投递未结束的消息（含退避中的），测试与优雅退出用
}

// Broker 持有全部队列。
type Broker struct {
	mu     sync.RWMutex
	byTop  map[string]*queue
	dead   DeadSink
	log    *slog.Logger
	maxGap time.Duration
	base   time.Duration
}

// New 构造一个 Broker。
func New(log *slog.Logger, dead DeadSink) *Broker {
	if log == nil {
		log = slog.Default()
	}
	return &Broker{byTop: map[string]*queue{}, dead: dead, log: log, base: time.Second, maxGap: 30 * time.Second}
}

// SetBackoff 调整重投退避（测试用）。
func (b *Broker) SetBackoff(base, max time.Duration) { b.base, b.maxGap = base, max }

// Register 声明一条队列。必须在 Start 之前调用。
func (b *Broker) Register(name, topic string, workers int, h Handler) {
	if workers < 1 {
		workers = 1
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.byTop[topic] = &queue{name: name, topic: topic, workers: workers, handler: h, signal: make(chan struct{}, 1)}
}

// Publish 把一条消息投给绑定了 topic 的队列。满足 generation/app.Publisher。
func (b *Broker) Publish(_ context.Context, topic string, payload []byte) error {
	b.mu.RLock()
	q := b.byTop[topic]
	b.mu.RUnlock()
	if q == nil {
		return fmt.Errorf("%w: %s", ErrNoQueue, topic)
	}
	body := append([]byte(nil), payload...)
	q.pending.Add(1)
	q.push(message{body: body})
	return nil
}

func (q *queue) push(m message) {
	q.mu.Lock()
	q.items = append(q.items, m)
	q.mu.Unlock()
	select {
	case q.signal <- struct{}{}:
	default:
	}
}

func (q *queue) pop() (message, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return message{}, false
	}
	m := q.items[0]
	q.items = q.items[1:]
	return m, true
}

// Start 启动全部消费协程，ctx 取消时停止。
func (b *Broker) Start(ctx context.Context) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, q := range b.byTop {
		for i := 0; i < q.workers; i++ {
			go b.consume(ctx, q)
		}
	}
}

func (b *Broker) consume(ctx context.Context, q *queue) {
	for {
		m, ok := q.pop()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-q.signal:
				continue
			case <-time.After(time.Second):
				continue
			}
		}
		b.handle(ctx, q, m)
		// 唤醒同队列的其他协程（一次 signal 只能叫醒一个，后面可能还有积压）。
		select {
		case q.signal <- struct{}{}:
		default:
		}
	}
}

func (b *Broker) handle(ctx context.Context, q *queue, m message) {
	action := b.safeCall(ctx, q, m)
	switch action {
	case Requeue:
		if ctx.Err() != nil {
			q.pending.Done()
			return
		}
		m.attempt++
		delay := b.base << min(m.attempt-1, 5)
		if delay > b.maxGap {
			delay = b.maxGap
		}
		time.AfterFunc(delay, func() { q.push(m) })
	case Dead:
		if b.dead != nil {
			if err := b.dead(context.WithoutCancel(ctx), q.name, q.topic, "handler", m.body); err != nil {
				b.log.Error("死信落库失败", "queue", q.name, "error", err)
			}
		}
		q.pending.Done()
	default:
		q.pending.Done()
	}
}

// safeCall 兜住回调里的 panic：一条坏消息不能让整条队列的消费协程退出。按 Dead 处理。
func (b *Broker) safeCall(ctx context.Context, q *queue, m message) (a Action) {
	defer func() {
		if r := recover(); r != nil {
			b.log.Error("消费回调 panic，消息转入死信", "queue", q.name, "panic", r)
			a = Dead
		}
	}()
	return q.handler(ctx, m.body)
}

// Wait 等到所有已投递的消息都有了最终结局（Done / Discard / Dead），或 ctx 到期。测试用。
func (b *Broker) Wait(ctx context.Context) error {
	b.mu.RLock()
	qs := make([]*queue, 0, len(b.byTop))
	for _, q := range b.byTop {
		qs = append(qs, q)
	}
	b.mu.RUnlock()
	done := make(chan struct{})
	go func() {
		for _, q := range qs {
			q.pending.Wait()
		}
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Depth 返回某 topic 队列里等待中的条数（环境自检用）。
func (b *Broker) Depth(topic string) int {
	b.mu.RLock()
	q := b.byTop[topic]
	b.mu.RUnlock()
	if q == nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}
