// Package db 提供 PostgreSQL 连接池（pgx/v5 原生，ADR-0001 TECH-03）。
//
// 不走 database/sql：本项目需要 jsonb 原样往返、数组、带状态条件的 CAS 更新
// 与 advisory lock，原生接口少一层类型转换。
package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/platform/config"
)

type Pool struct {
	*pgxpool.Pool
}

// Open 建立连接池并**立即探活**。
// 不做惰性连接：配置写错要在启动时就暴露，而不是等第一个用户请求才报错。
func Open(ctx context.Context, cfg config.Postgres) (*Pool, error) {
	if cfg.DSN == "" {
		return nil, fmt.Errorf("SEEAITV_PG_DSN 未配置")
	}
	pc, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("解析 PG DSN 失败: %w", err)
	}
	pc.MaxConns = cfg.MaxConns
	pc.MinConns = cfg.MinConns
	pc.MaxConnLifetime = cfg.MaxConnLifetime

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("创建 PG 连接池失败: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("PG 探活失败: %w", err)
	}
	return &Pool{pool}, nil
}

func (p *Pool) Name() string { return "postgres" }

func (p *Pool) Check(ctx context.Context) error { return p.Ping(ctx) }

func (p *Pool) Close() error {
	p.Pool.Close() // pgxpool.Close 无返回值，包一层以适配统一的 Closer
	return nil
}
