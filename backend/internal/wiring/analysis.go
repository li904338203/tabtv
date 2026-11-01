package wiring

import (
	"context"
	"log/slog"

	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	"github.com/li904338203/tabtv/backend/internal/platform/cache"
)

// 预览素材扫描器的叫醒（docs/agent/smartcut SCT-16，SCD-30）：API 排进分析之后在 Redis 发一条，
// Worker 的扫描器订阅着、收到就去领，不用等 5 秒的兜底扫描。
//
// ⚠️ 只是加速：pub/sub 不在线就丢，Redis 挂了也只是退回兜底间隔（GO-11）。消息内容没有意义，
// 扫描器收到后自己去库里领 —— 不能让「收到了哪条消息」决定做什么，那样丢一条就漏一份。

// analysisWakeChannel 叫醒频道（带命名空间前缀，INIT-07）。
func analysisWakeChannel(c *cache.Client) string { return c.Key("media", "analysis", "wake") }

// AnalysisWaker 把 Redis 发布接成 mediaapp.AnalysisWaker；c 为 nil 时返回 nil（只靠兜底扫描）。
func AnalysisWaker(c *cache.Client) mediaapp.AnalysisWaker {
	if c == nil {
		return nil
	}
	return analysisWaker{c}
}

type analysisWaker struct{ c *cache.Client }

func (w analysisWaker) Wake(ctx context.Context) error {
	return w.c.Publish(ctx, analysisWakeChannel(w.c), "1").Err()
}

// AnalysisWakeups 订阅叫醒频道，转成扫描器要的信号。c 为 nil 时返回 nil 通道（永远不触发，扫描器只靠兜底）。
//
// 缓冲 1、满了就丢：扫描器一次会把能领的全领完，连发十条和一条的效果一样。
func AnalysisWakeups(ctx context.Context, c *cache.Client, log *slog.Logger) <-chan struct{} {
	if c == nil {
		return nil
	}
	out := make(chan struct{}, 1)
	ps := c.Subscribe(ctx, analysisWakeChannel(c))
	go func() {
		defer func() { _ = ps.Close() }()
		msgs := ps.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-msgs:
				if !ok {
					log.Warn("预览素材叫醒频道已关闭，扫描器只靠兜底间隔")
					return
				}
				select {
				case out <- struct{}{}:
				default:
				}
			}
		}
	}()
	return out
}
