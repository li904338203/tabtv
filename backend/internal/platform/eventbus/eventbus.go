// Package eventbus 是跨进程的实时事件总线：任意进程把「topic + 事件 JSON」发到
// Redis 频道 `events:bus`，API 进程订阅后按 topic 原样投给 WebSocket hub（AGT-02）。
//
// ⚠️ 叶子包：只依赖 go-redis 与标准库，**不 import internal/transport/ws** ——
// hub 以接口传入。反过来依赖的话，Worker 为了发一条事件就要把整个 WebSocket 层编进来。
//
// ⚠️ 只放「发布」与「桥接」两件事，不要往里加别的（项目 CLAUDE.md §3 的下沉判据）。
//
// ⚠️ pub/sub 是**尽力而为**的：订阅者不在线就丢。事件只是对账信号（GO-26），
// 不要因为有了总线就让任何前端逻辑依赖「必然收到」。
package eventbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
)

// Channel 是总线频道名（实际频道带命名空间前缀，由调用方的 key 函数加）。
const Channel = "events:bus"

// Envelope 是总线上的一条消息。Payload 原样投给订阅了 Topic 的 WebSocket 连接。
type Envelope struct {
	Topic   string          `json:"topic"`
	Payload json.RawMessage `json:"payload"`
}

// RedisPublisher 是 Publisher 需要的那一个 Redis 方法（*redis.Client 满足它；测试可替换）。
type RedisPublisher interface {
	Publish(ctx context.Context, channel string, message any) *redis.IntCmd
}

// Hub 是 Bridge 投递的目标（ws.Hub 满足它）。
type Hub interface {
	Publish(topic string, msg []byte) int
}

// Publisher 往总线发事件。
type Publisher struct {
	rdb RedisPublisher
	key func(parts ...string) string
}

// NewPublisher 构造发布方。key 与 cache.Client.Key 一致（带命名空间前缀，INIT-07）。
func NewPublisher(rdb RedisPublisher, key func(...string) string) *Publisher {
	return &Publisher{rdb: rdb, key: key}
}

// Publish 把一条事件发到总线。
//
// ⚠️ 非法 topic / 非法 JSON 在这里就拒，**不进 Redis**：坏 JSON 进了 WS 帧会让前端
// 整条连接的解析出错，而那时已经分不清是谁发的。这里只校验格式，不做授权 ——
// 授权发生在订阅时（ws 层的 topic 鉴权）。
func (p *Publisher) Publish(ctx context.Context, topic string, payload []byte) error {
	if err := validTopic(topic); err != nil {
		return err
	}
	if len(payload) == 0 || !json.Valid(payload) {
		return errors.New("eventbus: payload 不是合法 JSON")
	}
	return p.rdb.Publish(ctx, p.key(Channel), encode(topic, payload)).Err()
}

// encode 手拼信封而不走 json.Marshal：后者会把 RawMessage 压缩并转义 `<>&`，
// payload 就不再逐字节原样。topic 已校验为 `[a-z]+:[0-9]+`、payload 已校验为合法 JSON，
// 拼出来的必然是合法 JSON。
func encode(topic string, payload []byte) []byte {
	msg := make([]byte, 0, len(payload)+len(topic)+24)
	msg = append(msg, `{"topic":"`...)
	msg = append(msg, topic...)
	msg = append(msg, `","payload":`...)
	msg = append(msg, payload...)
	return append(msg, '}')
}

// validTopic 只认 `kind:id`：kind 是小写字母，id 是正整数（与 ws.ParseTopic 的形态一致，
// 但不在这里维护 kind 白名单 —— 那是订阅侧的事，新增 kind 不该要改两处）。
//
// ⚠️ id 必须是规范的十进制（无符号、无前导零）：hub 按 topic 字符串精确匹配，
// `workspace:+5` 或 `workspace:05` 能被 ParseInt 接受，却投不到订阅了 `workspace:5` 的连接 ——
// 发布成功、谁都收不到，而这不报错。
func validTopic(topic string) error {
	kind, id, ok := strings.Cut(topic, ":")
	if !ok || kind == "" || id == "" || id[0] == '0' {
		return fmt.Errorf("eventbus: topic 格式非法: %q", topic)
	}
	for i := 0; i < len(kind); i++ {
		if kind[i] < 'a' || kind[i] > 'z' {
			return fmt.Errorf("eventbus: topic 格式非法: %q", topic)
		}
	}
	for i := 0; i < len(id); i++ {
		if id[i] < '0' || id[i] > '9' {
			return fmt.Errorf("eventbus: topic 格式非法: %q", topic)
		}
	}
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		return fmt.Errorf("eventbus: topic id 越界: %q", topic)
	}
	return nil
}

// decode 解一条总线消息。坏信封（不是 JSON、缺 topic、topic 非法、缺 payload）一律报错，
// 由调用方告警丢弃 —— 不 panic，也不按猜出来的 topic 投递。
func decode(raw string) (topic string, payload []byte, err error) {
	var env Envelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return "", nil, fmt.Errorf("eventbus: 信封不是合法 JSON: %w", err)
	}
	if err := validTopic(env.Topic); err != nil {
		return "", nil, err
	}
	if len(env.Payload) == 0 || string(env.Payload) == "null" {
		return "", nil, errors.New("eventbus: 信封缺少 payload")
	}
	return env.Topic, env.Payload, nil
}

// Bridge 在 API 进程订阅总线，按信封 topic 投给 WebSocket hub。
type Bridge struct {
	rdb *redis.Client
	key func(parts ...string) string
	hub Hub
	log *slog.Logger
}

// NewBridge 构造桥。
func NewBridge(rdb *redis.Client, key func(...string) string, hub Hub, log *slog.Logger) *Bridge {
	return &Bridge{rdb: rdb, key: key, hub: hub, log: log}
}

// Run 阻塞订阅直到 ctx 取消。go-redis 的 PubSub 断线会自行重连。
func (b *Bridge) Run(ctx context.Context) {
	sub := b.rdb.Subscribe(ctx, b.key(Channel))
	defer sub.Close()
	ch := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case m, ok := <-ch:
			if !ok {
				return
			}
			b.dispatch(m.Payload)
		}
	}
}

// dispatch 处理一条原始消息。⚠️ 除格式校验外不做任何业务判断 —— 投给谁由 topic 决定，
// 谁能收到由订阅鉴权决定。
func (b *Bridge) dispatch(raw string) {
	topic, payload, err := decode(raw)
	if err != nil {
		if b.log != nil {
			b.log.Warn("总线事件格式非法，已丢弃", "error", err)
		}
		return
	}
	b.hub.Publish(topic, payload)
}
