package app

import (
	"context"
	"log/slog"
	"time"
)

// Locker 是跨实例互斥（与画布回收站清理同一套语义）。
//
// ⚠️ 这把锁只用来**避免重复劳动**，不承担正确性：
// 过期语句本身带 `status='pending' and expires_at <= now()`，
// 多个实例同时跑也只会把同一批行标成同一个状态。
type Locker interface {
	TryLock(ctx context.Context, key string, ttl time.Duration) (bool, error)
}

// InvitationCleaner 把过期的待处理邀请落实成 expired。
//
// ⚠️ 它不是"整理数据"，而是 uk_invite_pending 能用下去的前提：
// 唯一索引的条件是 `status = 'pending'`。过期的行若永远停在 pending，
// 同一个人就**再也邀请不了第二次** —— 而用户看到的错误会是「名称已存在」，
// 指向一个完全无关的方向。
//
// 对外行为不依赖这个作业：读取路径都带 `expires_at > now()`，
// 过期邀请在到点那一刻就已经用不了了（与提示词 ready/stale、
// 导出包过期、会员权益按 expire_at 判定是同一条规则 —— 对外行为算出来，
// 作业只负责把状态写下来）。
type InvitationCleaner struct {
	svc      *Service
	log      *slog.Logger
	lock     Locker
	interval time.Duration
	batch    int
}

func NewInvitationCleaner(svc *Service, log *slog.Logger, lock Locker) *InvitationCleaner {
	return &InvitationCleaner{
		svc: svc, log: log, lock: lock,
		// 邀请有效期 7 天，过期这件事本身不急；每小时一次已经远远够用，
		// 而间隔越密单次要改的行越少，长事务持锁的窗口也越短。
		interval: time.Hour,
		batch:    500,
	}
}

func (c *InvitationCleaner) SetInterval(d time.Duration) { c.interval = d }

func (c *InvitationCleaner) Run(ctx context.Context) {
	t := time.NewTicker(c.interval)
	defer t.Stop()
	// 启动即跑一次：进程重启频繁时，只在整点跑会让清理永远轮不上。
	c.Once(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.Once(ctx)
		}
	}
}

// Once 跑一轮，返回标记的条数。
func (c *InvitationCleaner) Once(ctx context.Context) int {
	if c.lock != nil {
		ok, err := c.lock.TryLock(ctx, "cleanup:workspace-invitation", c.interval)
		if err != nil {
			// 锁服务不可用不该阻止清理：最坏是多个实例各标一批，结果一样。
			c.log.Warn("邀请过期清理取锁失败，仍继续执行", "error", err)
		} else if !ok {
			return 0
		}
	}
	total := 0
	for {
		n, err := c.svc.repo.ExpireInvitations(ctx, c.batch)
		if err != nil {
			c.log.Error("邀请过期清理失败", "error", err)
			return total
		}
		total += n
		if n < c.batch {
			break
		}
		select {
		case <-ctx.Done():
			return total
		case <-time.After(time.Second):
		}
	}
	if total > 0 {
		c.log.Info("邀请过期清理完成", "expired", total)
	}
	return total
}
