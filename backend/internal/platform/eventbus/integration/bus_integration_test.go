//go:build integration

// 事件总线的真 Redis 往返测试（AGT-02）。
//
// ⚠️ 必须用真 Redis：要守的是「发布方的频道名 == 订阅方的频道名（含命名空间前缀）」
// 与「payload 经 Redis 一圈后逐字节不变」—— 用假实现测的只是两边各自按自己想的调用。
//
// ⚠️ 用 testsupport 共用的那个 Redis 容器，而不是 testcontainers 的 redis 模块：
// 本包里每个用例各起一个容器会把 colima（2GB）压垮，且多拉一个镜像。
package integration

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	"github.com/li904338203/tabtv/backend/internal/platform/cache"
	"github.com/li904338203/tabtv/backend/internal/platform/config"
	"github.com/li904338203/tabtv/backend/internal/platform/eventbus"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
)

type recordHub struct {
	mu   sync.Mutex
	msgs map[string][]string
}

func (h *recordHub) Publish(topic string, msg []byte) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.msgs == nil {
		h.msgs = map[string][]string{}
	}
	h.msgs[topic] = append(h.msgs[topic], string(msg))
	return 1
}

func (h *recordHub) get(topic string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.msgs[topic]...)
}

// startBridge 起一条桥，并等到 Redis 确认有订阅者之后才返回
// （pub/sub 不缓存：订阅建立之前发的消息直接丢，不等的话测试时有时无地红）。
func startBridge(t *testing.T) (*cache.Client, *recordHub) {
	t.Helper()
	c, err := cache.Open(context.Background(), config.Redis{
		Addr:      testsupport.RedisAddr(t),
		Namespace: "it-" + t.Name(),
	})
	if err != nil {
		t.Fatalf("连接测试 Redis 失败: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	hub := &recordHub{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go eventbus.NewBridge(c.Client, c.Key, hub, nil).Run(ctx)

	channel := c.Key(eventbus.Channel)
	deadline := time.Now().Add(10 * time.Second)
	for {
		n, err := c.PubSubNumSub(ctx, channel).Result()
		if err == nil && n[channel] > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("等不到总线订阅建立: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return c, hub
}

func waitFor(t *testing.T, hub *recordHub, topic string) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if got := hub.get(topic); len(got) > 0 {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("hub 在 %s 上等不到消息", topic)
	return nil
}

func TestBus_真Redis发布后Bridge投递到hub(t *testing.T) {
	c, hub := startBridge(t)
	payload := `{"type":"agent.delta","text":"你好 \"世界\"","n":[1, 2]}`
	pub := eventbus.NewPublisher(c.Client, c.Key)
	if err := pub.Publish(context.Background(), "project:7", []byte(payload)); err != nil {
		t.Fatal(err)
	}
	got := waitFor(t, hub, "project:7")
	if len(got) != 1 || got[0] != payload {
		t.Fatalf("payload 经 Redis 一圈后被改写或重复: %q", got)
	}
}

// 生成事件改走总线之后，浏览器在 workspace:{id} 上收到的仍是裸 payload。
func TestBus_生成事件经总线投到workspace(t *testing.T) {
	c, hub := startBridge(t)
	payload := json.RawMessage(`{"type":"generation.update","generationId":"9001","workspaceId":"42"}`)
	if err := genapp.NewRedisEventPublisher(c.Client, c.Key).PublishState(
		context.Background(), 42, payload); err != nil {
		t.Fatal(err)
	}
	got := waitFor(t, hub, "workspace:42")
	if len(got) != 1 || got[0] != string(payload) {
		t.Fatalf("生成事件投递不一致: %q", got)
	}
}
