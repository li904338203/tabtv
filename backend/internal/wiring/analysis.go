package wiring

import (
	"context"

	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
)

// 预览素材扫描器的叫醒（docs/agent/smartcut SCT-16，SCD-30）：排进分析之后叫醒扫描器，不用等 5 秒的兜底扫描。
//
// 单进程（TT-T-06）：叫醒就是往一个容量 1 的通道里非阻塞地塞一个信号 —— 扫描器一次会把能领的全领完，
// 连发十次和一次的效果一样。⚠️ 只是加速：信号丢了也只是退回兜底间隔。

// AnalysisWake 是进程内的叫醒通道。
type AnalysisWake struct{ ch chan struct{} }

// NewAnalysisWake 构造叫醒通道。
func NewAnalysisWake() *AnalysisWake { return &AnalysisWake{ch: make(chan struct{}, 1)} }

// Wake 实现 mediaapp.AnalysisWaker。
func (w *AnalysisWake) Wake(context.Context) error {
	select {
	case w.ch <- struct{}{}:
	default:
	}
	return nil
}

// Wakeups 给扫描器的信号通道。
func (w *AnalysisWake) Wakeups() <-chan struct{} { return w.ch }

var _ mediaapp.AnalysisWaker = (*AnalysisWake)(nil)
