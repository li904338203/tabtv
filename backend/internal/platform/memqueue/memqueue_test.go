package memqueue

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBroker_按topic路由且每条只处理一次(t *testing.T) {
	b := New(nil, nil)
	var a, c atomic.Int32
	b.Register("a.jobs", "a.queued", 3, func(context.Context, []byte) Action { a.Add(1); return Done })
	b.Register("c.jobs", "c.queued", 1, func(context.Context, []byte) Action { c.Add(1); return Done })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.Start(ctx)
	for i := 0; i < 50; i++ {
		_ = b.Publish(ctx, "a.queued", []byte("x"))
	}
	_ = b.Publish(ctx, "c.queued", []byte("y"))
	wctx, wc := context.WithTimeout(ctx, 5*time.Second)
	defer wc()
	if err := b.Wait(wctx); err != nil {
		t.Fatal(err)
	}
	if a.Load() != 50 || c.Load() != 1 {
		t.Fatalf("a=%d c=%d", a.Load(), c.Load())
	}
	if err := b.Publish(ctx, "nobody", nil); err == nil {
		t.Fatal("没有队列的 topic 应报错（投递器据此重试，消息留在 outbox）")
	}
}

func TestBroker_Requeue带退避后重投(t *testing.T) {
	b := New(nil, nil)
	b.SetBackoff(10*time.Millisecond, 40*time.Millisecond)
	var n atomic.Int32
	b.Register("q", "t", 1, func(context.Context, []byte) Action {
		if n.Add(1) < 4 {
			return Requeue
		}
		return Done
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.Start(ctx)
	start := time.Now()
	_ = b.Publish(ctx, "t", []byte("x"))
	wctx, wc := context.WithTimeout(ctx, 5*time.Second)
	defer wc()
	if err := b.Wait(wctx); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 4 {
		t.Fatalf("应处理 4 次，实际 %d", n.Load())
	}
	// 退避 10+20+40ms：不是空转立即重投。
	if time.Since(start) < 60*time.Millisecond {
		t.Fatalf("重投没有退避：%v", time.Since(start))
	}
}

func TestBroker_Dead与panic都进死信且队列继续工作(t *testing.T) {
	var mu sync.Mutex
	var dead []string
	b := New(nil, func(_ context.Context, queue, topic, reason string, body []byte) error {
		mu.Lock()
		dead = append(dead, queue+"|"+string(body))
		mu.Unlock()
		return nil
	})
	var ok atomic.Int32
	b.Register("q", "t", 1, func(_ context.Context, body []byte) Action {
		switch string(body) {
		case "bad":
			return Dead
		case "boom":
			panic("坏消息")
		}
		ok.Add(1)
		return Done
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.Start(ctx)
	for _, m := range []string{"bad", "boom", "good", "good"} {
		_ = b.Publish(ctx, "t", []byte(m))
	}
	wctx, wc := context.WithTimeout(ctx, 5*time.Second)
	defer wc()
	if err := b.Wait(wctx); err != nil {
		t.Fatal(err)
	}
	if ok.Load() != 2 || len(dead) != 2 {
		t.Fatalf("ok=%d dead=%v", ok.Load(), dead)
	}
}

func TestBroker_并发上限等于消费协程数(t *testing.T) {
	b := New(nil, nil)
	var cur, peak atomic.Int32
	b.Register("q", "t", 2, func(context.Context, []byte) Action {
		v := cur.Add(1)
		for {
			p := peak.Load()
			if v <= p || peak.CompareAndSwap(p, v) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		cur.Add(-1)
		return Done
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.Start(ctx)
	for i := 0; i < 10; i++ {
		_ = b.Publish(ctx, "t", []byte("x"))
	}
	wctx, wc := context.WithTimeout(ctx, 5*time.Second)
	defer wc()
	if err := b.Wait(wctx); err != nil {
		t.Fatal(err)
	}
	if peak.Load() != 2 {
		t.Fatalf("同时在跑的应恰为 2，峰值 %d", peak.Load())
	}
}
