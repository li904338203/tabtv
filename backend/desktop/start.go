package desktop

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/li904338203/tabtv/backend/internal/platform/bootstrap"
	"github.com/li904338203/tabtv/backend/internal/platform/config"
	"github.com/li904338203/tabtv/backend/internal/platform/db"
	"github.com/li904338203/tabtv/backend/internal/platform/eventbus"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/objstore"
	"github.com/li904338203/tabtv/backend/internal/platform/settings"
)

// desktopConfig 是桌面应用的配置：来源系统的默认值 + 桌面差异（TT-T-12 再把可调项接到设置表）。
//
// ⚠️ 用 LoadRaw 不用 Load：Load 会按服务端规则校验（必须配 JWT 密钥、Redis、MQ 地址……），
// 那些在桌面版里根本不存在。环境变量（SEEAITV_*）仍可覆盖，供开发与测试用。
func desktopConfig(dsn, mediaDir string) config.Config {
	cfg := config.LoadRaw()
	cfg.Postgres = config.Postgres{DSN: dsn, MaxConns: 20, MinConns: 2, MaxConnLifetime: time.Hour}
	cfg.Storage.Type = "local"
	cfg.Storage.Bucket = objstore.LocalBucket
	cfg.Dev.FakePayment = false
	if cfg.LogRetention <= 0 {
		cfg.LogRetention = 30 * 24 * time.Hour
	}
	return cfg
}

// start 建依赖、写内置配置与本机身份、装配，并启动后台工作。
func (r *Runtime) start(ctx context.Context, log *slog.Logger) error {
	cfg := desktopConfig(r.DB.DSN(), r.Dirs.Media)
	pool, err := db.Open(ctx, cfg.Postgres)
	if err != nil {
		return fmt.Errorf("连接数据库失败：%w", err)
	}
	r.pool = pool
	store, err := objstore.NewLocal(r.Dirs.Media, cfg.Storage.SignTTL)
	if err != nil {
		return err
	}
	st, err := settings.Open(ctx, pool.Pool)
	if err != nil {
		return fmt.Errorf("读取设置失败：%w", err)
	}
	r.settings = st
	applySettings(&cfg, st)
	r.Bus = eventbus.NewLocal()
	deps := &bootstrap.Deps{DB: pool, Bus: r.Bus, ObjStore: store}

	gen, err := id.New(0)
	if err != nil {
		return err
	}
	ident, tokens, err := seedAndIdentity(ctx, r.DB.DSN(), pool.Pool, gen.Next)
	if err != nil {
		return err
	}
	if err := freePricing(ctx, pool.Pool, gen.Next); err != nil {
		return fmt.Errorf("写入价格配置失败：%w", err)
	}
	a, err := assemble(ctx, cfg, deps, ident, tokenService{tokens}, gen, log, st, r.Dirs)
	if err != nil {
		return err
	}
	if _, err := installProviderTemplates(ctx, pool.Pool, gen.Next, ident.AdminID, log); err != nil {
		return fmt.Errorf("写入厂商模板失败：%w", err)
	}
	if r.Builtin, err = installBuiltinContent(ctx, pool.Pool, store, gen.Next, ident.AdminID, log); err != nil {
		return fmt.Errorf("安装内置营销内容失败：%w", err)
	}
	bgCtx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.bg = a.bg
	if err := a.bg.Start(bgCtx); err != nil {
		cancel()
		return err
	}
	r.Handler = a.handler
	return nil
}
