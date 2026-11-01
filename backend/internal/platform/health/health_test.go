package health

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func ok(name string) Checker {
	return CheckerFunc{name, func(context.Context) error { return nil }}
}

func bad(name string, err error) Checker {
	return CheckerFunc{name, func(context.Context) error { return err }}
}

// 任一依赖不通，整体必须 degraded —— readyz 据此返回 503 让负载均衡摘掉本实例。
func TestCheck_单个失败即整体降级(t *testing.T) {
	r := NewRegistry(time.Second)
	r.Register(ok("postgres"))
	r.Register(bad("redis", errors.New("connection refused")))
	r.Register(ok("rabbitmq"))

	res := r.Check(context.Background())
	if res.Status != "degraded" {
		t.Fatalf("期望 degraded，实际 %q", res.Status)
	}
	if res.Checks["redis"].Status != "error" {
		t.Errorf("redis 应为 error")
	}
	// 必须指名具体是哪个依赖挂了，否则排障时只能挨个试。
	if res.Checks["redis"].Error == "" {
		t.Errorf("失败项必须带 error 信息")
	}
	if res.Checks["postgres"].Status != "ok" {
		t.Errorf("健康项不应被连累")
	}
}

func TestCheck_全部健康(t *testing.T) {
	r := NewRegistry(time.Second)
	r.Register(ok("postgres"))
	r.Register(ok("redis"))
	res := r.Check(context.Background())
	if res.Status != "ok" {
		t.Fatalf("期望 ok，实际 %q", res.Status)
	}
	if len(res.Checks) != 2 {
		t.Fatalf("期望 2 项检查，实际 %d", len(res.Checks))
	}
}

// 探活必须并发。串行探活 N 个依赖时最坏耗时是 N 倍超时，
// readyz 会先被编排系统判超时，把一个其实健康的进程重启掉。
func TestCheck_并发执行(t *testing.T) {
	const n = 4
	const delay = 120 * time.Millisecond

	r := NewRegistry(time.Second)
	var inflight, peak int32
	for i := 0; i < n; i++ {
		r.Register(CheckerFunc{fmt.Sprintf("dep%d", i), func(context.Context) error {
			cur := atomic.AddInt32(&inflight, 1)
			for {
				old := atomic.LoadInt32(&peak)
				if cur <= old || atomic.CompareAndSwapInt32(&peak, old, cur) {
					break
				}
			}
			time.Sleep(delay)
			atomic.AddInt32(&inflight, -1)
			return nil
		}})
	}

	start := time.Now()
	r.Check(context.Background())
	elapsed := time.Since(start)

	if got := atomic.LoadInt32(&peak); got < n {
		t.Errorf("期望 %d 个探活并发执行，实测峰值并发仅 %d", n, got)
	}
	// 并发执行总耗时应接近单个延迟，而不是累加。
	if elapsed > delay*time.Duration(n)/2 {
		t.Errorf("耗时 %v 接近串行，探活未并发", elapsed)
	}
}

// 卡住的依赖必须被超时切断，不能拖垮整个 readyz。
func TestCheck_单项超时不拖垮整体(t *testing.T) {
	r := NewRegistry(80 * time.Millisecond)
	r.Register(ok("fast"))
	r.Register(CheckerFunc{"slow", func(ctx context.Context) error {
		select {
		case <-time.After(5 * time.Second):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}})

	start := time.Now()
	res := r.Check(context.Background())
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("超时未生效，耗时 %v", elapsed)
	}
	if res.Status != "degraded" {
		t.Errorf("卡住的依赖应判为不健康")
	}
	if res.Checks["fast"].Status != "ok" {
		t.Errorf("快依赖不应被慢依赖连累")
	}
}

func TestCheck_无依赖时为ok(t *testing.T) {
	res := NewRegistry(time.Second).Check(context.Background())
	if res.Status != "ok" || len(res.Checks) != 0 {
		t.Fatalf("空注册表应为 ok 且无检查项，实际 %+v", res)
	}
}

// 倒序关闭：后建立的依赖通常被先建立的使用，先关后者会让前者收尾失败。
func TestCloser_倒序关闭(t *testing.T) {
	var order []int
	c := &Closer{}
	for i := 1; i <= 3; i++ {
		c.Add(func() error { order = append(order, i); return nil })
	}
	if errs := c.Close(); len(errs) != 0 {
		t.Fatalf("不应有错误: %v", errs)
	}
	want := []int{3, 2, 1}
	for i, v := range want {
		if order[i] != v {
			t.Fatalf("关闭顺序应为 %v，实际 %v", want, order)
		}
	}
}

// 单个关闭失败不能中断其余资源的释放，否则会泄漏连接。
func TestCloser_失败不中断其余(t *testing.T) {
	var closed int
	c := &Closer{}
	c.Add(func() error { closed++; return nil })
	c.Add(func() error { return errors.New("关闭失败") })
	c.Add(func() error { closed++; return nil })

	errs := c.Close()
	if len(errs) != 1 {
		t.Fatalf("期望收集到 1 个错误，实际 %d", len(errs))
	}
	if closed != 2 {
		t.Fatalf("其余 2 项仍应被关闭，实际 %d", closed)
	}
}

// 重复调用 Close 不应重复释放（defer 与显式关闭可能同时发生）。
func TestCloser_幂等(t *testing.T) {
	var n int
	c := &Closer{}
	c.Add(func() error { n++; return nil })
	c.Close()
	c.Close()
	if n != 1 {
		t.Fatalf("重复 Close 应只释放一次，实际 %d 次", n)
	}
}
