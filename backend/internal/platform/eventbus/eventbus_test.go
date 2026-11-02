package eventbus

import (
	"context"
	"testing"
)

func TestLocal_校验后交给出口(t *testing.T) {
	l := NewLocal()
	// 没有出口时丢弃且不报错（窗口还没起来）。
	if err := l.Publish(context.Background(), "workspace:5", []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	var got []string
	l.SetEmitter(func(topic string, payload []byte) { got = append(got, topic+" "+string(payload)) })
	if err := l.Publish(context.Background(), "workspace:5", []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != `workspace:5 {"a":1}` {
		t.Fatalf("出口收到 %v", got)
	}
}

func TestLocal_拒绝非法topic与payload(t *testing.T) {
	l := NewLocal()
	l.SetEmitter(func(string, []byte) { t.Fatal("非法事件不应投出") })
	for _, topic := range []string{"workspace", "Workspace:1", "workspace:05", "workspace:+5", "workspace:x", ":1"} {
		if err := l.Publish(context.Background(), topic, []byte(`{}`)); err == nil {
			t.Errorf("%q 应被拒", topic)
		}
	}
	for _, p := range []string{"", "{", "nul"} {
		if err := l.Publish(context.Background(), "project:1", []byte(p)); err == nil {
			t.Errorf("payload %q 应被拒", p)
		}
	}
}
