// Package ffsem 是进程级的 ffmpeg 并发信号量（docs/agent/smartcut SCD-30、dev/01 §10）。
//
// 渲染与预览素材扫描器都要跑 ffmpeg，而一路 ffmpeg 就能吃满几个核。两边各自限流的话，
// 合起来照样超：一次导出撞上一段长视频的预览转码，Worker 把机器啃满，查询接口跟着从毫秒劣化到几秒
// （旧系统的 HttpClient 泄漏就是这么把库挤垮的，教训是同一类）。
//
// ⚠️ 只依赖标准库：渲染在 timeline 域、扫描在 media 域，让一方依赖另一方是反的，
// 各抄一份的话两边早晚不一致（CLAUDE.md §3 的下沉判据）。
//
// ⚠️ 进程级、不跨实例：管的是「这台机器同时跑几个 ffmpeg」。跨实例的上限各有各的
// （渲染读库里的 running 数，扫描靠 skip locked 一次领一行）。
package ffsem

import (
	"context"
	"sync"
)

// Sem 是一个计数信号量。nil 的 *Sem 表示不限（测试与不跑 ffmpeg 的进程用）。
type Sem struct{ slots chan struct{} }

// New 建一个容量为 n 的信号量；n < 1 按 1 处理（0 会让所有 ffmpeg 永远等下去，而那不报错）。
func New(n int) *Sem {
	if n < 1 {
		n = 1
	}
	return &Sem{slots: make(chan struct{}, n)}
}

// Acquire 拿一个名额；ctx 先结束就放弃并返回 ctx.Err()（被取消的导出不该还排在队里等 CPU）。
//
// 返回的 release 多调无害：调用方常在 defer 里放一次、在提前完成的路径上再放一次，
// 放两次的话就凭空多出一个名额 —— 那正是信号量要防的事。
func (s *Sem) Acquire(ctx context.Context) (release func(), err error) {
	if s == nil {
		return func() {}, nil
	}
	select {
	case s.slots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-s.slots }) }, nil
	case <-ctx.Done():
		return func() {}, ctx.Err()
	}
}

// Cap 是容量；nil 为 0（不限）。
func (s *Sem) Cap() int {
	if s == nil {
		return 0
	}
	return cap(s.slots)
}

// InUse 是当前被占用的名额数（日志与测试用）。
func (s *Sem) InUse() int {
	if s == nil {
		return 0
	}
	return len(s.slots)
}
