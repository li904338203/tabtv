package ws

import (
	"encoding/json"
	"sync"
	"testing"
)

func drain(c *Conn) []string {
	var out []string
	for {
		select {
		case msg := <-c.Recv():
			out = append(out, string(msg))
		default:
			return out
		}
	}
}

func TestParseTopic(t *testing.T) {
	ok := []struct {
		in   string
		kind string
		id   int64
	}{
		{"project:123", "project", 123},
		{"workspace:9", "workspace", 9},
		{"generation:456", "generation", 456},
		{"user:1", "user", 1},
	}
	for _, c := range ok {
		got, err := ParseTopic(c.in)
		if err != nil {
			t.Errorf("ParseTopic(%q) 意外失败: %v", c.in, err)
			continue
		}
		if got.Kind != c.kind || got.ID != c.id {
			t.Errorf("ParseTopic(%q) = %+v", c.in, got)
		}
	}
	// 形态非法一律拒绝，不做猜测 —— 猜测会让越权检查有机会被绕过。
	for _, bad := range []string{"", "project", "project:", ":123", "unknown:1",
		"project:abc", "project:-1", "project:0", "project:1:2"} {
		if _, err := ParseTopic(bad); err == nil {
			t.Errorf("非法 topic %q 应被拒绝", bad)
		}
	}
}

func TestHub_按topic投递(t *testing.T) {
	h := NewHub()
	a := h.Add(1)
	b := h.Add(2)
	a.subscribe("project:100")
	b.subscribe("project:200")

	if n := h.Publish("project:100", []byte(`{"x":1}`)); n != 1 {
		t.Fatalf("期望投递给 1 个连接，实际 %d", n)
	}
	if len(drain(a)) != 1 {
		t.Error("订阅者 a 应收到消息")
	}
	if len(drain(b)) != 0 {
		t.Error("未订阅该 topic 的 b 不应收到消息")
	}
}

// 撤销用户时必须**先推 subscription.revoked 再断开**。
// 反过来客户端只看到一次断线，会按断线重连处理并立刻重试，
// 而不知道自己已被移除。
func TestHub_撤销用户先通知再断开(t *testing.T) {
	h := NewHub()
	c := h.Add(42)
	c.subscribe("project:1")
	c.subscribe("workspace:2")

	h.RevokeUser(42, "banned")

	msgs := drain(c)
	if len(msgs) != 1 {
		t.Fatalf("期望收到 1 条撤销通知，实际 %d 条", len(msgs))
	}
	var f revokedFrame
	if err := json.Unmarshal([]byte(msgs[0]), &f); err != nil {
		t.Fatal(err)
	}
	if f.Type != "subscription.revoked" {
		t.Errorf("帧类型 = %q", f.Type)
	}
	if f.Reason != "banned" {
		t.Errorf("reason = %q", f.Reason)
	}
	if len(f.Topics) != 2 {
		t.Errorf("应列出全部被撤销的 topic，实际 %d 个", len(f.Topics))
	}
	if h.Stats().Connections != 0 {
		t.Error("撤销后连接应已移除")
	}
}

// 撤销单个 topic 时连接本身保留：用户可能还订阅着其他合法 topic。
func TestHub_撤销topic保留连接(t *testing.T) {
	h := NewHub()
	c := h.Add(7)
	c.subscribe("project:1")
	c.subscribe("project:2")

	h.RevokeTopic("project:1", "share_revoked")

	if c.hasTopic("project:1") {
		t.Error("被撤销的 topic 应已移除")
	}
	if !c.hasTopic("project:2") {
		t.Error("其他 topic 不应受影响")
	}
	if h.Stats().Connections != 1 {
		t.Error("撤销单个 topic 不应断开连接")
	}
	if h.Publish("project:1", []byte("x")) != 0 {
		t.Error("撤销后不应再向该 topic 投递")
	}
}

// 慢客户端不能拖住广播：缓冲满即丢弃并计数，
// 客户端重连后用 HTTP 对账（事件不是事实来源，GO-39）。
func TestHub_慢客户端不阻塞广播(t *testing.T) {
	h := NewHub()
	slow := h.Add(1)
	fast := h.Add(2)
	slow.subscribe("t")
	fast.subscribe("t")

	// 塞满 slow 的缓冲。
	for i := 0; i < sendBuffer+10; i++ {
		h.Publish("t", []byte("m"))
	}
	// fast 的缓冲同样会满，但关键是 Publish 没有阻塞住。
	if h.Stats().Dropped == 0 {
		t.Error("缓冲满时应记录丢弃计数")
	}
	_ = fast
}

// 重复 Remove 不能 panic：defer 与显式撤销可能同时发生。
func TestHub_重复移除幂等(t *testing.T) {
	h := NewHub()
	c := h.Add(1)
	h.Remove(c)
	h.Remove(c) // 不应 panic（close 已关闭的 channel 会 panic）
	if h.Stats().Connections != 0 {
		t.Error("连接应已移除")
	}
}

func TestHub_并发安全(t *testing.T) {
	h := NewHub()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := h.Add(int64(i % 5))
			c.subscribe("shared")
			h.Publish("shared", []byte("m"))
			h.RevokeUser(int64(i%5), "test")
			h.Remove(c)
		}(i)
	}
	wg.Wait()
	if h.Stats().Connections != 0 {
		t.Errorf("全部移除后应为 0，实际 %d", h.Stats().Connections)
	}
}

// ⚠️ 移除一个成员时**只能撤销那个人的订阅**（P12-03、NEW-18）。
//
// 用 RevokeTopic 的话，"移除某人"会变成"全团队掉线" —— 其余成员什么都没做错，
// 界面上只会看到"订阅已撤销"然后再也收不到生成进度。
// 这个错误**只在多人同时在线时才暴露**，单人测试永远看不到。
func TestRevokeUserTopic_只影响那个人(t *testing.T) {
	h := NewHub()
	const topic = "workspace:42"

	leaving := h.Add(7)
	staying := h.Add(8)
	sameUserOtherTab := h.Add(7)
	for _, c := range []*Conn{leaving, staying, sameUserOtherTab} {
		c.subscribe(topic)
	}

	h.RevokeUserTopic(7, topic, "member_removed")

	if leaving.hasTopic(topic) {
		t.Error("被移除的人应失去订阅")
	}
	// ⚠️ 同一个人的其他标签页也要一起断 —— 留着的话他换个窗口照样收事件。
	if sameUserOtherTab.hasTopic(topic) {
		t.Error("同一个人的其他连接也应失去订阅")
	}
	if !staying.hasTopic(topic) {
		t.Error("其余成员不该被牵连")
	}
	// 事件仍然投递得到留下的人。
	if n := h.Publish(topic, []byte("x")); n != 1 {
		t.Errorf("应还有 1 个订阅者收到，实际 %d", n)
	}
	if h.Stats().Connections != 3 {
		t.Error("撤销订阅不应断开连接")
	}
}
