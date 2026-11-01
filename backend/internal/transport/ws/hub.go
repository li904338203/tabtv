// Package ws 实现 /ws/events 的连接管理与事件分发。
//
// 协议见 api/events/README.md：
//   - token 走**首帧**不入 URL（URL 会进网关日志、浏览器历史与 Referer）；
//   - subscribe 的每个 topic **逐条鉴权**，不通过的放进 rejected，不让整次订阅失败；
//   - 成员移除 / 共享撤销 / 会话失效时主动推 subscription.revoked 并停止投递（CD-09）。
//
// ⚠️ 旧系统按业务开了 /ws/generation 与 /ws/writing 两条连接。
// 新系统只开一条，事件类型由 type 区分（CD-08）。
package ws

import (
	"sync"
	"sync/atomic"
)

// Conn 是 hub 视角下的一个连接。
type Conn struct {
	id     uint64
	userID int64
	send   chan []byte
	// ⚠️ 用 done 信号而不是 close(send) 表示关闭。
	// Go 里**不能关闭其他 goroutine 还可能发送的 channel** ——
	// push 与 close 之间必然存在窗口，会触发 send on closed channel。
	// race detector 在并发测试里抓到过这个问题。
	done      chan struct{}
	closeOnce sync.Once
	mu        sync.RWMutex
	topics    map[string]bool
}

// close 幂等地发出关闭信号。
func (c *Conn) close() { c.closeOnce.Do(func() { close(c.done) }) }

// Done 供写循环感知关闭。
func (c *Conn) Done() <-chan struct{} { return c.done }

// Recv 供写循环取待发消息。
func (c *Conn) Recv() <-chan []byte { return c.send }

func (c *Conn) UserID() int64 { return c.userID }

func (c *Conn) subscribe(topic string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.topics[topic] = true
}

func (c *Conn) unsubscribe(topic string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.topics, topic)
}

func (c *Conn) hasTopic(topic string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.topics[topic]
}

func (c *Conn) topicList() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(c.topics))
	for t := range c.topics {
		out = append(out, t)
	}
	return out
}

// push 非阻塞投递。
// ⚠️ 必须非阻塞：一个卡住的慢客户端不能拖住整个广播循环，
// 否则一个人的网络问题会让所有人收不到事件。
func (c *Conn) push(msg []byte) bool {
	select {
	case <-c.done:
		return false
	default:
	}
	select {
	case c.send <- msg:
		return true
	case <-c.done:
		return false
	default:
		// 发送缓冲满 = 客户端消费不过来。丢弃并计数，
		// 客户端重连后用 HTTP 对账即可（事件不是事实来源，GO-39）。
		return false
	}
}

type Hub struct {
	mu      sync.RWMutex
	conns   map[uint64]*Conn
	byUser  map[int64]map[uint64]*Conn
	nextID  atomic.Uint64
	dropped atomic.Uint64
}

func NewHub() *Hub {
	return &Hub{
		conns:  make(map[uint64]*Conn),
		byUser: make(map[int64]map[uint64]*Conn),
	}
}

const sendBuffer = 64

func (h *Hub) Add(userID int64) *Conn {
	c := &Conn{
		id:     h.nextID.Add(1),
		userID: userID,
		send:   make(chan []byte, sendBuffer),
		done:   make(chan struct{}),
		topics: make(map[string]bool),
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.conns[c.id] = c
	if h.byUser[userID] == nil {
		h.byUser[userID] = make(map[uint64]*Conn)
	}
	h.byUser[userID][c.id] = c
	return c
}

func (h *Hub) Remove(c *Conn) {
	h.mu.Lock()
	delete(h.conns, c.id)
	if m := h.byUser[c.userID]; m != nil {
		delete(m, c.id)
		if len(m) == 0 {
			delete(h.byUser, c.userID)
		}
	}
	h.mu.Unlock()
	// 只发信号，不关 send —— 关了会与并发的 push 竞态。
	c.close()
}

// Publish 向订阅了该 topic 的连接投递。
func (h *Hub) Publish(topic string, msg []byte) int {
	h.mu.RLock()
	targets := make([]*Conn, 0, len(h.conns))
	for _, c := range h.conns {
		if c.hasTopic(topic) {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()

	sent := 0
	for _, c := range targets {
		if c.push(msg) {
			sent++
		} else {
			h.dropped.Add(1)
		}
	}
	return sent
}

// RevokeUser 撤销某用户的全部连接：先推 subscription.revoked，再关连接。
//
// 顺序很重要——先关连接的话客户端只看到一次"断线"，
// 会按断线重连处理并立刻重试，而不知道自己已被移除。
func (h *Hub) RevokeUser(userID int64, reason string) {
	h.mu.RLock()
	conns := make([]*Conn, 0)
	for _, c := range h.byUser[userID] {
		conns = append(conns, c)
	}
	h.mu.RUnlock()

	for _, c := range conns {
		if topics := c.topicList(); len(topics) > 0 {
			if msg, err := encodeRevoked(topics, reason); err == nil {
				c.push(msg)
			}
		}
		h.Remove(c)
	}
}

// RevokeUserTopic 只撤销**某一个用户**在该 topic 上的订阅。
//
// ⚠️ 移除一个成员时必须用它，不能用 RevokeTopic（P12-03）：后者会把**所有**
// 订阅了这个 topic 的连接一起踢掉 —— 一次"移除某人"会让整个团队的实时事件
// 订阅同时断掉，而他们什么都没做错，界面上只会看到"订阅已撤销"然后再也收不到
// 生成进度。那是一个只有在多人同时在线时才会暴露的错误。
func (h *Hub) RevokeUserTopic(userID int64, topic, reason string) {
	h.mu.RLock()
	targets := make([]*Conn, 0)
	for _, c := range h.conns {
		if c.userID == userID && c.hasTopic(topic) {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()

	msg, err := encodeRevoked([]string{topic}, reason)
	for _, c := range targets {
		c.unsubscribe(topic)
		if err == nil {
			c.push(msg)
		}
	}
}

// RevokeTopic 撤销某个 topic 的**全部**订阅。
//
// ⚠️ 只在这个 topic 对**所有人**都失效时才用 —— 目前只有"工作区被归档"
// 一种情形。针对某个人的撤销一律走 RevokeUserTopic。
// 连接本身保留：用户可能还订阅着其他合法 topic。
func (h *Hub) RevokeTopic(topic, reason string) {
	h.mu.RLock()
	targets := make([]*Conn, 0)
	for _, c := range h.conns {
		if c.hasTopic(topic) {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()

	msg, err := encodeRevoked([]string{topic}, reason)
	for _, c := range targets {
		c.unsubscribe(topic)
		if err == nil {
			c.push(msg)
		}
	}
}

type Stats struct {
	Connections int
	Users       int
	Dropped     uint64
}

func (h *Hub) Stats() Stats {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return Stats{Connections: len(h.conns), Users: len(h.byUser), Dropped: h.dropped.Load()}
}
