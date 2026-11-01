// Package health 聚合各依赖的探活结果。
//
// 区分两类端点（容器编排的语义不同，混用会导致重启风暴）：
//   - liveness  /healthz  进程还活着吗？**不查依赖** —— 数据库挂了重启进程没用，
//     反而会在依赖恢复前不断重启，把问题放大。
//   - readiness /readyz   能接流量吗？**逐个探活依赖**，任一不通即不就绪。
package health

import (
	"context"
	"sync"
	"time"
)

// Checker 是一个具名依赖的探活函数。实现必须尊重 ctx 超时。
type Checker interface {
	Name() string
	Check(ctx context.Context) error
}

// CheckerFunc 让普通函数满足 Checker。
type CheckerFunc struct {
	CheckerName string
	Fn          func(ctx context.Context) error
}

func (c CheckerFunc) Name() string                    { return c.CheckerName }
func (c CheckerFunc) Check(ctx context.Context) error { return c.Fn(ctx) }

type Registry struct {
	mu       sync.RWMutex
	checkers []Checker
	// timeout 是单个依赖的探活上限。探活本身卡住比依赖挂掉更糟：
	// readyz 迟迟不返回，编排系统会判定超时并重启一个其实健康的进程。
	timeout time.Duration
}

func NewRegistry(timeout time.Duration) *Registry {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return &Registry{timeout: timeout}
}

func (r *Registry) Register(c Checker) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checkers = append(r.checkers, c)
}

type Result struct {
	Status string            `json:"status"` // ok | degraded
	Checks map[string]Detail `json:"checks"`
}

type Detail struct {
	Status     string `json:"status"` // ok | error
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"durationMs"`
}

// Check 并发探活全部依赖，任一失败则整体 degraded。
// 并发是必要的：串行探活四个依赖，最坏耗时是四倍超时。
func (r *Registry) Check(ctx context.Context) Result {
	r.mu.RLock()
	checkers := make([]Checker, len(r.checkers))
	copy(checkers, r.checkers)
	timeout := r.timeout
	r.mu.RUnlock()

	res := Result{Status: "ok", Checks: make(map[string]Detail, len(checkers))}
	if len(checkers) == 0 {
		return res
	}

	type outcome struct {
		name   string
		detail Detail
	}
	ch := make(chan outcome, len(checkers))
	var wg sync.WaitGroup
	for _, c := range checkers {
		wg.Add(1)
		go func(c Checker) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			start := time.Now()
			err := c.Check(cctx)
			d := Detail{Status: "ok", DurationMS: time.Since(start).Milliseconds()}
			if err != nil {
				d.Status = "error"
				d.Error = err.Error()
			}
			ch <- outcome{c.Name(), d}
		}(c)
	}
	wg.Wait()
	close(ch)

	for o := range ch {
		if o.detail.Status != "ok" {
			res.Status = "degraded"
		}
		res.Checks[o.name] = o.detail
	}
	return res
}

// Closer 供进程退出时统一释放资源。倒序关闭：
// 后建立的依赖通常被先建立的依赖使用，先关后者会让前者在收尾时失败。
type Closer struct {
	mu    sync.Mutex
	items []func() error
}

func (c *Closer) Add(fn func() error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = append(c.items, fn)
}

func (c *Closer) Close() []error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var errs []error
	for i := len(c.items) - 1; i >= 0; i-- {
		if err := c.items[i](); err != nil {
			errs = append(errs, err)
		}
	}
	c.items = nil
	return errs
}
