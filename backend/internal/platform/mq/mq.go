// Package mq 提供 RabbitMQ 连接与拓扑声明（ADR-0001 TECH-05）。
//
// 投递一致性靠 PostgreSQL outbox，不靠 after-commit 回调：
// 提交成功但消息没发出去必须是**可恢复**状态（FLOW-02）。
package mq

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/li904338203/tabtv/backend/internal/platform/config"
)

type Conn struct {
	mu     sync.Mutex
	conn   *amqp.Connection
	url    string
	prefix string
}

// Open 建立连接。拓扑声明在 P05 随消费者一起落地。
func Open(cfg config.Queue) (*Conn, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("SEEAITV_QUEUE_URL 未配置")
	}
	conn, err := amqp.Dial(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("RabbitMQ 连接失败: %w", err)
	}
	return &Conn{conn: conn, url: cfg.URL, prefix: cfg.ExchangePrefix}, nil
}

// Exchange 按前缀构造 exchange 名。
// ⚠️ 基线前缀 seeaitv，**禁止与旧系统的 aitv.* 撞名**（INIT-07）：
// 撞名会让新 Worker 消费到旧系统的消息，或反过来。
func (c *Conn) Exchange(name string) string { return c.prefix + "." + name }

// Channel 开一个新 channel；**连接已断时自动重连**。
//
// ⚠️ channel **不是并发安全的**，每个消费者/发布者各开各的，不要共用。
//
// ⚠️ 自动重连不是锦上添花（P14-03 的恢复演练实测出来的）：原先这里直接
// `c.conn.Channel()`，于是 broker 一重启（升级、崩溃、主备切换），
// 进程里所有 channel 全部失效且**再也不会恢复** —— 而进程还活着、
// `/healthz` 照样 200。表现是：
//   - Worker 一条消息都不消费，任务**永远停在 queued**，而积分已经扣了；
//   - API 的 outbox 投递器发不出去，日志里刷「投递失败，稍后重试」到放弃。
//
// 两者都**没有任何对外错误**，只有日志里几行 ERROR —— 而进程是"健康"的，
// 编排系统不会重启它。演练里 broker 重启后消息全部存活，**消费者却没回来**。
func (c *Conn) Channel() (*amqp.Channel, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil && !c.conn.IsClosed() {
		if ch, err := c.conn.Channel(); err == nil {
			return ch, nil
		}
		// 连接看着是开的但开不出 channel：当作已断，重连一次。
		_ = c.conn.Close()
	}
	if c.url == "" {
		return nil, fmt.Errorf("RabbitMQ 连接已关闭且无法重连（未记录连接串）")
	}
	conn, err := amqp.Dial(c.url)
	if err != nil {
		return nil, fmt.Errorf("RabbitMQ 重连失败: %w", err)
	}
	c.conn = conn
	return c.conn.Channel()
}

func (c *Conn) Name() string { return "rabbitmq" }

func (c *Conn) Check(_ context.Context) error {
	if c.conn == nil || c.conn.IsClosed() {
		return fmt.Errorf("RabbitMQ 连接已关闭")
	}
	// 开一个 channel 才能真正确认连接可用：IsClosed 只反映本地状态，
	// 网络断开后本地可能尚未感知。
	ch, err := c.conn.Channel()
	if err != nil {
		return fmt.Errorf("RabbitMQ 无法开启 channel: %w", err)
	}
	return ch.Close()
}

func (c *Conn) Close() error {
	if c.conn == nil || c.conn.IsClosed() {
		return nil
	}
	return c.conn.Close()
}

// Publisher 是 outbox 投递用的最小发布接口。
//
// ⚠️ 每次 Publish 复用同一个 channel 并加锁：amqp091 的 Channel **不是并发安全**的，
// 多个 goroutine 同时用会话协议错乱，表现为随机的 "unexpected frame" 与连接被服务端关闭。
type Publisher struct {
	conn *Conn
	mu   sync.Mutex
	ch   *amqp.Channel
}

func NewPublisher(c *Conn) *Publisher { return &Publisher{conn: c} }

// Publish 以 topic 作为 routing key 发到约定的 exchange。
//
// ⚠️ 消息设为**持久化**（DeliveryMode 2）：非持久化消息在 broker 重启后消失，
// 而它对应的任务已经扣过费、已在库里排队 —— 用户会看到一个永远排队的任务。
func (p *Publisher) Publish(ctx context.Context, topic string, payload []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ch == nil || p.ch.IsClosed() {
		ch, err := p.conn.Channel()
		if err != nil {
			return err
		}
		exchange := p.conn.Exchange("tasks")
		if err := ch.ExchangeDeclare(exchange, "topic", true, false, false, false, nil); err != nil {
			_ = ch.Close()
			return err
		}
		p.ch = ch
	}
	return p.ch.PublishWithContext(ctx, p.conn.Exchange("tasks"), topic, false, false,
		amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			Timestamp:    time.Now(),
			Body:         payload,
		})
}

// Consumer 订阅一个主题并逐条消费。
//
// ⚠️ **手动 ack**（autoAck=false）：自动 ack 意味着消息一投递就算处理完了，
// Worker 崩在执行途中那条任务就永远没人接手（GO-16）。
type Consumer struct {
	conn  *Conn
	queue string
	topic string
	log   *slog.Logger
}

func NewConsumer(c *Conn, queue, topic string) *Consumer {
	return &Consumer{conn: c, queue: queue, topic: topic}
}

// Ack 结果：如何处置这条消息。
type AckAction int

const (
	// AckDone 处理完成，确认。
	AckDone AckAction = iota
	// AckRequeue 放回队列稍后再试（并发上限、可重试失败）。
	AckRequeue
	// AckDiscard 正常丢弃：重复消息、任务已不存在。
	//
	// ⚠️ 实现上是 **ack 而不是 nack**。队列挂了死信交换机之后，
	// nack(requeue=false) 会把消息路由进死信队列 —— 而重复消息是
	// at-least-once 投递的**正常结局**，不是故障。让它进死信，
	// 死信队列会被重复消息淹没，真正需要人看的那几条反而找不到了。
	AckDiscard
	// AckDead 进死信队列：这条消息我们处理不了，且**库里的状态不足以
	// 让运维知道该做什么**。目前只有两种情形用它：
	//
	//   - 消息解不开（连是哪个任务都不知道，丢了就是静默的消息丢失）；
	//   - 提交结果不确定（可能已在上游计费，必须有人去对账）。
	//
	// ⚠️ 「重试耗尽判失败」**不进死信**：那条 generation 已经是 failed、
	// 有错误码、已退款，运维在管理端按状态就能捞到。再复制一份进 MQ
	// 只是把同一件事记两遍，而两份记录早晚会不一致。
	AckDead
)

// Consume 阻塞消费直到 ctx 取消。handler 返回如何 ack。
func (c *Consumer) consumeOnce(ctx context.Context, prefetch int,
	handler func(context.Context, []byte) AckAction) error {
	ch, err := c.conn.Channel()
	if err != nil {
		return err
	}
	defer ch.Close()

	exchange := c.conn.Exchange("tasks")
	if err := ch.ExchangeDeclare(exchange, "topic", true, false, false, false, nil); err != nil {
		return err
	}
	// 死信拓扑：主队列挂 x-dead-letter-exchange，nack(requeue=false) 的消息
	// 自动路由到死信队列，带上原始 routing key。
	//
	// ⚠️ 必须**先声明死信侧**再声明主队列：主队列的参数指向死信交换机，
	// 交换机不存在时 broker 会把消息静默丢掉 —— 死信队列存在的意义就没了。
	deadExchange := c.conn.Exchange("dead")
	if err := ch.ExchangeDeclare(deadExchange, "topic", true, false, false, false, nil); err != nil {
		return err
	}
	deadQueue, err := ch.QueueDeclare(c.conn.prefix+"."+c.queue+".dead", true, false, false, false, nil)
	if err != nil {
		return err
	}
	// ⚠️ 死信队列按**本队列的 topic** 绑定，不是 `#`。
	//
	// 死信路由保留**原始 routing key**，而每条主队列各自声明一个 `.dead`
	// 队列。三个队列都用 `#` 绑在同一个死信交换机上的话，**一条死信会被
	// 复制进全部三个死信队列** —— 排空进库之后看起来像三次独立的故障，
	// 而其中两条属于根本不认识这种消息的队列。
	//
	// 在没有人消费死信队列之前这是无害的（P13-03 之前正是如此），
	// 所以它一直没有暴露。
	if err := ch.QueueBind(deadQueue.Name, c.topic, deadExchange, false, nil); err != nil {
		return err
	}
	// 解掉历史上那条 `#`：绑定是累加的，只加新的话旧的仍然生效 ——
	// 表现为"在干净的 broker 上是对的、在你本机是错的"。
	// 从未绑过时 broker 不报错，所以这一行可以无条件执行。
	if err := ch.QueueUnbind(deadQueue.Name, "#", deadExchange, nil); err != nil {
		return err
	}

	q, err := ch.QueueDeclare(c.conn.prefix+"."+c.queue, true, false, false, false,
		amqp.Table{"x-dead-letter-exchange": deadExchange})
	if err != nil {
		// ⚠️ 队列参数改动会让已存在的队列声明失败（PRECONDITION_FAILED）。
		// 报出来而不是忽略：忽略的结果是死信配置没生效，而一切看起来正常。
		return fmt.Errorf("声明队列 %s 失败（已存在的队列参数不同时需要先删除旧队列）: %w",
			c.conn.prefix+"."+c.queue, err)
	}
	if err := ch.QueueBind(q.Name, c.topic, exchange, false, nil); err != nil {
		return err
	}
	// ⚠️ prefetch=1 对长耗时任务是必须的：一次预取多条会让一个 Worker
	// 把整批任务锁在自己手里，而它可能正卡在某个上游调用上。
	if prefetch <= 0 {
		prefetch = 1
	}
	if err := ch.Qos(prefetch, 0, false); err != nil {
		return err
	}

	deliveries, err := ch.Consume(q.Name, "", false, false, false, false, nil)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case d, ok := <-deliveries:
			if !ok {
				return nil
			}
			switch handler(ctx, d.Body) {
			case AckRequeue:
				_ = d.Nack(false, true)
			case AckDead:
				// nack(requeue=false) → 由 x-dead-letter-exchange 路由进死信队列。
				_ = d.Nack(false, false)
			default:
				// AckDone 与 AckDiscard 都是 ack：前者处理完了，
				// 后者是"本来就不该再处理"，两者都不该进死信。
				_ = d.Ack(false)
			}
		}
	}
}

// DeadMessage 是一条死信。
type DeadMessage struct {
	// Queue 是产生它的**主队列**名（不含 prefix 与 `.dead` 后缀）。
	Queue string
	// RoutingKey 是原始 routing key。重投时要**原样用它** ——
	// 按队列名猜一个的话消息会落到别的消费者手上，解不开、再进一次死信，
	// 看起来像"重投没生效"。
	RoutingKey string
	Body       []byte
	// Reason 是 broker 给的死信原因（rejected / expired / maxlen）。
	Reason string
}

// DrainDead 排空死信队列：每条交给 handler 落库，成功即 ack。
//
// ⚠️ **落库失败必须 nack 回队列**，不能 ack。死信是"最需要人看的那几条"，
// 库写不进去时把它 ack 掉等于彻底销毁证据 —— 而那条消息在任何地方都不再存在。
//
// ⚠️ 这里**不重投、不执行**，只搬运。重投是运维在管理端做的一次显式决定：
// 自动重投的话，一条必然失败的消息会在"失败 → 死信 → 重投"之间无限循环，
// 而每一圈都可能在上游产生一次真实调用。
func (c *Consumer) drainDeadOnce(ctx context.Context,
	handler func(context.Context, DeadMessage) error) error {
	ch, err := c.conn.Channel()
	if err != nil {
		return err
	}
	defer ch.Close()

	name := c.conn.prefix + "." + c.queue + ".dead"
	// 只声明不改参数：主队列那边已经把死信拓扑建好了（Consume 里）。
	// 这里重复声明是为了让排空器能独立启动，而不依赖启动顺序。
	if _, err := ch.QueueDeclare(name, true, false, false, false, nil); err != nil {
		return err
	}
	// 排空器一次处理一条：它做的是"写一行库"，攒批没有收益，
	// 而 prefetch 大了之后进程退出会让一批消息回到队列重发。
	if err := ch.Qos(1, 0, false); err != nil {
		return err
	}
	deliveries, err := ch.Consume(name, "", false, false, false, false, nil)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case d, ok := <-deliveries:
			if !ok {
				return nil
			}
			msg := DeadMessage{
				Queue: c.queue, RoutingKey: d.RoutingKey, Body: d.Body,
				Reason: firstDeathReason(d.Headers),
			}
			if err := handler(ctx, msg); err != nil {
				// 回队列重试：宁可重复落一行（唯一索引会挡住），
				// 也不能把这条消息弄丢。
				_ = d.Nack(false, true)
				continue
			}
			_ = d.Ack(false)
		}
	}
}

// firstDeathReason 从 broker 加的 `x-death` 头里取死信原因。
//
// 取不到时回空串而不是报错：头的结构因 broker 版本而异，
// 而"原因不详"不该让一条死信落不了库。
func firstDeathReason(h amqp.Table) string {
	deaths, ok := h["x-death"].([]any)
	if !ok || len(deaths) == 0 {
		return ""
	}
	entry, ok := deaths[0].(amqp.Table)
	if !ok {
		return ""
	}
	reason, _ := entry["reason"].(string)
	return reason
}

// ---------------------------------------------------------------- 断线重连

// reconnectMin / reconnectMax 是重连退避的上下界。
//
// ⚠️ 上界要**有**：broker 长时间不可用时，每秒重连一次会在日志里刷几万行，
// 真正的故障信息淹没在里面。下界也要**有**：broker 刚重启完那几秒会拒连，
// 不退避的话会在它起来之前就把日志刷满。
const (
	reconnectMin = 1 * time.Second
	reconnectMax = 30 * time.Second
	// healthyRun 超过这个时长的一次消费视为"连上了"，下次断开从最小退避重来。
	// 不重置的话，一次偶发断开会让之后每次重连都等满 30 秒。
	healthyRun = time.Minute
)

// Consume 订阅并消费，**断线自动重连**直到 ctx 取消。
//
// ⚠️ 它现在只在 ctx 取消时返回 nil；原先它会在 channel 关闭时**直接返回**，
// 而调用方只是打一行「消费者退出」的日志 —— 于是 broker 重启之后
// Worker 静静地什么都不做，任务永远停在 queued 而积分已经扣了（P14-03 实测）。
//
// ⚠️ 重连后**整套拓扑重新声明**（consumeOnce 里那一整段）：broker 若是
// 从干净状态起来的（换了节点、数据目录被清），队列和绑定都不在了，
// 只重新 Consume 会直接报 NOT_FOUND。
func (c *Consumer) Consume(ctx context.Context, prefetch int,
	handler func(context.Context, []byte) AckAction) error {
	backoff := reconnectMin
	for {
		start := time.Now()
		err := c.consumeOnce(ctx, prefetch, handler)
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(start) >= healthyRun {
			backoff = reconnectMin
		}
		c.logger().Warn("消费者断开，准备重连",
			"queue", c.queue, "ranFor", time.Since(start).Round(time.Second).String(),
			"retryIn", backoff.String(), "error", errText(err))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > reconnectMax {
			backoff = reconnectMax
		}
	}
}

// DrainDead 排空死信队列，**断线自动重连**直到 ctx 取消。理由同 Consume。
func (c *Consumer) DrainDead(ctx context.Context,
	handler func(context.Context, DeadMessage) error) error {
	backoff := reconnectMin
	for {
		start := time.Now()
		err := c.drainDeadOnce(ctx, handler)
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(start) >= healthyRun {
			backoff = reconnectMin
		}
		c.logger().Warn("死信排空器断开，准备重连",
			"queue", c.queue, "retryIn", backoff.String(), "error", errText(err))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > reconnectMax {
			backoff = reconnectMax
		}
	}
}

// SetLogger 给消费者一个 logger。不设则用 slog 默认值 ——
// ⚠️ 重连必须看得见：一个"一直在重连"的 Worker 与一个"正常空闲"的 Worker
// 在外部看来完全一样（都是 healthz 200、都不消费）。
func (c *Consumer) SetLogger(l *slog.Logger) { c.log = l }

func (c *Consumer) logger() *slog.Logger {
	if c.log != nil {
		return c.log
	}
	return slog.Default()
}

func errText(err error) string {
	if err == nil {
		// channel 被服务端正常关闭时 consumeOnce 返回 nil —— 但那同样是断开，
		// 不能因为"没有 error"就当作什么都没发生。
		return "channel 已关闭"
	}
	return err.Error()
}
