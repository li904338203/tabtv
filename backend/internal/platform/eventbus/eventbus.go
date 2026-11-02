// Package eventbus 是进程内的实时事件总线（TT-T-06，dev/01 TTD-3/TTD-8）。
//
// 来源系统里它是跨进程的 Redis 频道：Worker 发、API 订阅后转给 WebSocket hub。tabtv 是单进程，
// 发布方直接把「topic + 事件 JSON」交给一个出口函数 —— 桌面应用里出口是 Wails 的 EventsEmit，
// 前端按 topic 自己过滤（只有一个窗口，没有「投给谁」的问题）。
//
// ⚠️ 事件仍然是**尽力而为**的对账信号（GO-26）：出口还没接上（窗口没起来）时直接丢，
// 不排队、不重放 —— 前端断开重连后要用 HTTP 对账，不能依赖「必然收到」。
package eventbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
)

// Emitter 是事件的出口：topic 已校验，payload 是合法 JSON。
type Emitter func(topic string, payload []byte)

// Local 是进程内总线。零值可用（没有出口时丢弃）。
type Local struct {
	emit atomic.Pointer[Emitter]
}

// NewLocal 构造一个总线。
func NewLocal() *Local { return &Local{} }

// SetEmitter 接上出口（窗口就绪后由宿主调用；可替换）。
func (l *Local) SetEmitter(e Emitter) { l.emit.Store(&e) }

// Publish 校验后交给出口。签名与来源系统的 Publisher 相同，各域的 EventPublisher 端口原样满足。
//
// ⚠️ 非法 topic / 非法 JSON 在这里就拒：坏 JSON 进了前端会让整条事件通道的解析出错，
// 而那时已经分不清是谁发的。
func (l *Local) Publish(_ context.Context, topic string, payload []byte) error {
	if err := validTopic(topic); err != nil {
		return err
	}
	if len(payload) == 0 || !json.Valid(payload) {
		return errors.New("eventbus: payload 不是合法 JSON")
	}
	if p := l.emit.Load(); p != nil && *p != nil {
		(*p)(topic, payload)
	}
	return nil
}

// Envelope 是交给前端的一条事件：{topic, payload}，payload 原样。
type Envelope struct {
	Topic   string          `json:"topic"`
	Payload json.RawMessage `json:"payload"`
}

// validTopic 只认 `kind:id`：kind 是小写字母，id 是正整数。
//
// ⚠️ id 必须是规范的十进制（无符号、无前导零）：前端按 topic 字符串精确匹配，
// `workspace:+5` 或 `workspace:05` 投出去谁都收不到，而这不报错。
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
