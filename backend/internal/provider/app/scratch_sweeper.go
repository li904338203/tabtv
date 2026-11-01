package app

import (
	"context"
	"log/slog"
	"time"
)

// ScratchLister 是清理作业要的：按前缀列出旧对象、逐个删掉（objstore.Client 实现）。
type ScratchLister interface {
	ListOlderThan(ctx context.Context, prefix string, before time.Time, limit int) ([]string, error)
	Delete(ctx context.Context, key string) error
}

// ScratchSweeper 兜底清理没走到终态的截取片段（cut.go）。
//
// 正常路径上片段用完即删（同步提交返回就删、异步查询到终态再删）；走不到那一步的有：Worker 中途崩了、
// 任务被取消或撞了死线（执行器不会再查询它）、删除本身失败。这些片段没有任何引用，按时间删就行。
//
// ⚠️ 年龄取 24 小时，远大于任何任务的死线：删早了的话，一个还在上游排队的异步任务来取素材时只拿到 404，
// 而那时钱已经扣了。公网中转存储那一份放在中转前缀下，由存储侧的过期规则清理，这里只扫主存储。
type ScratchSweeper struct {
	store    ScratchLister
	log      *slog.Logger
	Interval time.Duration
	MaxAge   time.Duration
	now      func() time.Time
}

func NewScratchSweeper(store ScratchLister, log *slog.Logger) *ScratchSweeper {
	if log == nil {
		log = slog.Default()
	}
	return &ScratchSweeper{store: store, log: log, Interval: time.Hour, MaxAge: 24 * time.Hour, now: time.Now}
}

// Run 每隔 Interval 扫一次，ctx 结束即退出。启动时先扫一次：Worker 重启往往正是有残留的时候。
func (s *ScratchSweeper) Run(ctx context.Context) {
	t := time.NewTicker(s.Interval)
	defer t.Stop()
	for {
		if n, err := s.SweepOnce(ctx); err != nil {
			s.log.Warn("清理截取片段失败", "error", err, "deleted", n)
		} else if n > 0 {
			s.log.Info("清理了过期的截取片段", "deleted", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// SweepOnce 删掉 ScratchPrefix 下超过 MaxAge 的对象，返回删了几个。分批列、逐个删：一次列几十万个对象
// 会长时间占着存储的连接，而这件事一点都不急。
func (s *ScratchSweeper) SweepOnce(ctx context.Context) (int, error) {
	const batch = 500
	before := s.now().Add(-s.MaxAge)
	deleted := 0
	for {
		keys, err := s.store.ListOlderThan(ctx, ScratchPrefix, before, batch)
		if err != nil {
			return deleted, err
		}
		for _, k := range keys {
			if err := s.store.Delete(ctx, k); err != nil {
				return deleted, err
			}
			deleted++
		}
		if len(keys) < batch {
			return deleted, nil
		}
	}
}
