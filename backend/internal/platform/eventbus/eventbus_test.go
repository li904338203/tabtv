package eventbus

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/redis/go-redis/v9"
)

type recordedPub struct {
	channel string
	message []byte
}

type fakeRedis struct{ pubs []recordedPub }

func (f *fakeRedis) Publish(_ context.Context, channel string, message any) *redis.IntCmd {
	f.pubs = append(f.pubs, recordedPub{channel: channel, message: message.([]byte)})
	return redis.NewIntResult(1, nil)
}

func nsKey(parts ...string) string {
	k := "seeaitv"
	for _, p := range parts {
		k += ":" + p
	}
	return k
}

type fakeHub struct {
	topics   []string
	payloads []string
}

func (h *fakeHub) Publish(topic string, msg []byte) int {
	h.topics = append(h.topics, topic)
	h.payloads = append(h.payloads, string(msg))
	return 1
}

func TestPublish_信封带topic且payload原样(t *testing.T) {
	f := &fakeRedis{}
	p := NewPublisher(f, nsKey)
	payload := `{"type":"generation.update","b":[1, 2],"a":"<x>&y"}`
	if err := p.Publish(context.Background(), "workspace:42", []byte(payload)); err != nil {
		t.Fatal(err)
	}
	if len(f.pubs) != 1 {
		t.Fatalf("应发出 1 条，实际 %d", len(f.pubs))
	}
	if f.pubs[0].channel != "seeaitv:events:bus" {
		t.Errorf("频道应带命名空间前缀，实际 %q", f.pubs[0].channel)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(f.pubs[0].message, &env); err != nil {
		t.Fatal(err)
	}
	if len(env) != 2 || string(env["topic"]) != `"workspace:42"` {
		t.Errorf("信封应只有 topic/payload 两个键，实际 %s", f.pubs[0].message)
	}
	// payload 必须逐字节原样（不重排键、不压缩空白、不转义 <>&）：浏览器收到的就是这一段。
	if string(env["payload"]) != payload {
		t.Errorf("payload 被改写：%s", env["payload"])
	}
}

func TestPublish_topic格式非法被拒(t *testing.T) {
	for _, topic := range []string{
		"", "workspace", "workspace:", ":42", "workspace:0", "workspace:05", "workspace:+5",
		"workspace:-1", "workspace:abc", "Workspace:1", "work space:1", "workspace:1:2",
		"workspace:99999999999999999999",
	} {
		f := &fakeRedis{}
		if err := NewPublisher(f, nsKey).Publish(context.Background(), topic, []byte(`{}`)); err == nil {
			t.Errorf("topic %q 应被拒", topic)
		}
		if len(f.pubs) != 0 {
			t.Errorf("topic %q 被拒后不应进入 Redis", topic)
		}
	}
	for _, topic := range []string{"workspace:42", "project:1", "agent:9007199254740993"} {
		f := &fakeRedis{}
		if err := NewPublisher(f, nsKey).Publish(context.Background(), topic, []byte(`{}`)); err != nil {
			t.Errorf("topic %q 应放行: %v", topic, err)
		}
	}
}

func TestPublish_payload不是JSON被拒(t *testing.T) {
	for _, payload := range []string{"", "{", "not json", `{"a":1}x`} {
		f := &fakeRedis{}
		if err := NewPublisher(f, nsKey).Publish(context.Background(), "project:1", []byte(payload)); err == nil {
			t.Errorf("payload %q 应被拒", payload)
		}
		if len(f.pubs) != 0 {
			t.Errorf("payload %q 被拒后不应进入 Redis", payload)
		}
	}
}

func TestDecode_坏信封丢弃不panic(t *testing.T) {
	for _, raw := range []string{
		"", "null", "[]", "{", `"str"`,
		`{"payload":{}}`,          // 缺 topic
		`{"topic":"workspace:1"}`, // 缺 payload
		`{"topic":"workspace:1","payload":null}`,
		`{"topic":"bad","payload":{}}`,     // topic 非法
		`{"topic":123,"payload":{}}`,       // topic 类型错
		`{"workspaceId":"1","payload":{}}`, // 旧信封误投到新频道
	} {
		if _, _, err := decode(raw); err == nil {
			t.Errorf("坏信封 %q 应报错", raw)
		}
	}
	topic, payload, err := decode(`{"topic":"project:7","payload":{"type":"x"}}`)
	if err != nil || topic != "project:7" || string(payload) != `{"type":"x"}` {
		t.Errorf("合法信封解析错误: %q %s %v", topic, payload, err)
	}
}

func TestBridge_按信封topic投递(t *testing.T) {
	h := &fakeHub{}
	b := NewBridge(nil, nsKey, h, nil)
	b.dispatch(`{"topic":"workspace:42","payload":{"type":"generation.update"}}`)
	b.dispatch(`garbage`)
	b.dispatch(`{"topic":"project:7","payload":{"type":"agent.delta"}}`)
	if len(h.topics) != 2 {
		t.Fatalf("应投递 2 条（坏的丢弃），实际 %d: %v", len(h.topics), h.topics)
	}
	if h.topics[0] != "workspace:42" || h.payloads[0] != `{"type":"generation.update"}` {
		t.Errorf("第 1 条投递错误: %s %s", h.topics[0], h.payloads[0])
	}
	if h.topics[1] != "project:7" || h.payloads[1] != `{"type":"agent.delta"}` {
		t.Errorf("第 2 条投递错误: %s %s", h.topics[1], h.payloads[1])
	}
}

// 往返：Publish 发出去的消息，dispatch 能按原 topic 投出原 payload。
func TestPublish_与Bridge往返(t *testing.T) {
	f := &fakeRedis{}
	payload := `{"type":"generation.update","workspaceId":"42"}`
	if err := NewPublisher(f, nsKey).Publish(context.Background(), "workspace:42", []byte(payload)); err != nil {
		t.Fatal(err)
	}
	h := &fakeHub{}
	NewBridge(nil, nsKey, h, nil).dispatch(string(f.pubs[0].message))
	if len(h.topics) != 1 || h.topics[0] != "workspace:42" || h.payloads[0] != payload {
		t.Errorf("往返不一致: %v %v", h.topics, h.payloads)
	}
}
