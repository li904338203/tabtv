// Package bootstrap 按配置建立各依赖，并登记探活与关闭。
//
// api 与 worker 共用同一套领域包与依赖装配，只是启动集合不同（08 §2）：
// api 不启动 Provider consumer，worker 不暴露用户写接口。
package bootstrap

import (
	"context"
	"log/slog"

	"github.com/li904338203/tabtv/backend/internal/platform/cache"
	"github.com/li904338203/tabtv/backend/internal/platform/config"
	"github.com/li904338203/tabtv/backend/internal/platform/db"
	"github.com/li904338203/tabtv/backend/internal/platform/health"
	"github.com/li904338203/tabtv/backend/internal/platform/mq"
	"github.com/li904338203/tabtv/backend/internal/platform/objstore"
)

// Deps 持有已建立的依赖。字段可能为 nil —— 本地开发允许只起一部分依赖，
// 缺失的那项会在 readyz 里显示为未配置，而不是让进程起不来。
type Deps struct {
	DB       *db.Pool
	Cache    *cache.Client
	MQ       *mq.Conn
	ObjStore *objstore.Client

	Health *health.Registry
	closer *health.Closer
}

// Want 声明本进程需要哪些依赖。
type Want struct {
	DB       bool
	Cache    bool
	MQ       bool
	ObjStore bool
}

// Open 按需建立依赖。任一**已声明需要**的依赖建立失败即返回错误并回滚已建立的部分 ——
// 半开状态最难排查：进程起来了，但一半功能静默不可用。
func Open(ctx context.Context, cfg config.Config, want Want, logger *slog.Logger) (*Deps, error) {
	d := &Deps{
		Health: health.NewRegistry(cfg.HealthCheckTimeout),
		closer: &health.Closer{},
	}

	fail := func(err error) (*Deps, error) {
		d.Close(logger)
		return nil, err
	}

	if want.DB {
		pool, err := db.Open(ctx, cfg.Postgres)
		if err != nil {
			return fail(err)
		}
		d.DB = pool
		d.Health.Register(pool)
		d.closer.Add(pool.Close)
		logger.Info("PostgreSQL 已连接", "maxConns", cfg.Postgres.MaxConns)
	}

	if want.Cache {
		cl, err := cache.Open(ctx, cfg.Redis)
		if err != nil {
			return fail(err)
		}
		d.Cache = cl
		d.Health.Register(cl)
		d.closer.Add(cl.Close)
		logger.Info("Redis 已连接", "namespace", cfg.Redis.Namespace)
	}

	if want.MQ {
		conn, err := mq.Open(cfg.Queue)
		if err != nil {
			return fail(err)
		}
		d.MQ = conn
		d.Health.Register(conn)
		d.closer.Add(conn.Close)
		logger.Info("RabbitMQ 已连接", "exchangePrefix", cfg.Queue.ExchangePrefix)
	}

	if want.ObjStore {
		// local 存储的实现待 P04-04；当前仅 s3 可用。
		if cfg.Storage.Type != "s3" {
			logger.Warn("对象存储为 local，跳过连接（仅单机开发可用，production 已被 config 拒绝）")
		} else {
			oc, err := objstore.Open(ctx, cfg.Storage)
			if err != nil {
				return fail(err)
			}
			d.ObjStore = oc
			d.Health.Register(oc)
			logger.Info("对象存储已连接", "bucket", cfg.Storage.Bucket)
		}
	}

	return d, nil
}

// Close 倒序释放全部资源，不因单个失败而中断。
func (d *Deps) Close(logger *slog.Logger) {
	if d == nil || d.closer == nil {
		return
	}
	for _, err := range d.closer.Close() {
		logger.Error("关闭依赖失败", "error", err)
	}
}

// Ready 供 readyz 使用。
func (d *Deps) Ready(ctx context.Context) health.Result { return d.Health.Check(ctx) }
