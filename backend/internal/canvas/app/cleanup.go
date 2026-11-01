package app

import (
	"context"
	"log/slog"
	"time"
)

// Cleaner 定期清空超保留期的回收站项目。
//
// 放在 Worker 进程而不是 API：这是后台作业，和请求路径无关，
// 塞进 API 会让一次删几万行的锁竞争直接打到用户的保存请求上。
type Cleaner struct {
	svc      *Service
	log      *slog.Logger
	lock     Locker
	interval time.Duration
	batch    int
	// sweeper 清理孤儿时间线（智能剪辑节点被删后留下的，SCT-07），保留期与回收站同口径（SC-PARAM-22）。
	sweeper TimelineSweeper
}

// SetTimelineSweeper 接上孤儿时间线清理。为 nil 时不清（旧行为）。
func (c *Cleaner) SetTimelineSweeper(s TimelineSweeper) { c.sweeper = s }

// Locker 是跨实例互斥。Worker 可以水平扩容，多个实例同时清理会互相撞锁、
// 把同一批行重复扫一遍。
//
// ⚠️ 这把锁只用来**避免重复劳动**，不承担正确性 ——
// 删除本身带 `status='deleted' and deleted_at < ?` 条件，重复执行也不会删错行（GO-11）。
type Locker interface {
	// TryLock 拿到锁返回 true；拿不到说明别的实例正在跑，本次跳过即可。
	TryLock(ctx context.Context, key string, ttl time.Duration) (bool, error)
}

func NewCleaner(svc *Service, log *slog.Logger, lock Locker) *Cleaner {
	return &Cleaner{
		svc: svc, log: log, lock: lock,
		// 每小时扫一次而不是每天：间隔越密，单次要删的行越少，
		// 长事务持锁堵住写入的窗口也越短。
		interval: time.Hour,
		batch:    500,
	}
}

func (c *Cleaner) SetInterval(d time.Duration) { c.interval = d }

// Run 阻塞运行直到 ctx 取消。
func (c *Cleaner) Run(ctx context.Context) {
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

// Once 执行一轮清理，返回删除的项目数。
func (c *Cleaner) Once(ctx context.Context) int {
	if c.lock != nil {
		ok, err := c.lock.TryLock(ctx, "cleanup:project-trash", c.interval)
		if err != nil {
			// 锁服务不可用不该阻止清理：最坏是多个实例各删一批，结果一样。
			c.log.Warn("回收站清理取锁失败，仍继续执行", "error", err)
		} else if !ok {
			return 0
		}
	}

	before := time.Now().Add(-c.svc.Retention())
	total, released, kept := 0, 0, 0
	for {
		ids, media, err := c.svc.repo.PurgeExpired(ctx, before, c.batch, c.svc.purgeHook())
		if err != nil {
			c.log.Error("回收站清理失败", "error", err)
			return total
		}
		total += len(ids)
		// 每批提交之后就释放这一批的导演台文件（DRD-30），失败只告警（见 releaseDirectorMedia）。
		r, k := c.svc.releaseDirectorMedia(ctx, media)
		released, kept = released+r, kept+k
		if len(ids) < c.batch {
			break
		}
		// 批间让出：连续大批删除会把 WAL 和锁一直占着。
		select {
		case <-ctx.Done():
			return total
		case <-time.After(time.Second):
		}
	}
	if total > 0 {
		// ⚠️ 只记条数不记项目名：清理日志不该成为已删除内容的副本。
		c.log.Info("回收站清理完成", "purged", total,
			"retentionDays", int(c.svc.Retention().Hours()/24),
			"directorMediaReleased", released, "directorMediaStillUsed", kept)
	}
	if n := c.sweepTimelines(ctx, before); n > 0 {
		c.log.Info("孤儿时间线清理完成", "deleted", n)
	}
	return total
}

// sweepTimelines 清理孤儿时间线：智能剪辑节点被删超过保留期的、所属项目已不存在的（SC-F-06）。
// 失败只告警：它们只是占地方，不保护素材（引用检查器不认它们，SCT-10）。
func (c *Cleaner) sweepTimelines(ctx context.Context, before time.Time) int {
	if c.sweeper == nil {
		return 0
	}
	total := 0
	for {
		n, err := c.sweeper.SweepOrphans(ctx, before, c.batch)
		if err != nil {
			c.log.Warn("孤儿时间线清理失败", "error", err)
			return total
		}
		total += n
		if n < c.batch {
			return total
		}
		select {
		case <-ctx.Done():
			return total
		case <-time.After(time.Second):
		}
	}
}
