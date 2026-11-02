//go:build integration

// Package testsupport 给集成测试提供**每个测试进程一个**独立的 PostgreSQL 库。
//
// tabtv 不依赖 docker（TT-T-04）：所有测试进程共用一个内嵌 PG 集群（pgembed/testcluster），
// 每个进程从迁移好的模板库复制一份自己的库 —— 包之间仍然隔离，建库是毫秒级的。
// 隔离靠**每个用例前清表**（Fresh）。
package testsupport

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/platform/pgembed/testcluster"
)

var (
	once sync.Once
	pool *pgxpool.Pool
	dsn  string
	err  error
)

// Pool 返回本进程共用的连接池，首次调用时建库（模板里已迁移好）。
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	once.Do(start)
	if err != nil {
		t.Fatalf("启动测试数据库失败: %v", err)
	}
	return pool
}

// DSN 返回连接串，给需要自己开连接的测试（如迁移测试）用。
func DSN(t *testing.T) string {
	t.Helper()
	once.Do(start)
	if err != nil {
		t.Fatalf("启动测试数据库失败: %v", err)
	}
	return dsn
}

func start() {
	dsn, err = testcluster.NewDatabase(context.Background())
	if err != nil {
		return
	}
	// ⚠️ 连接数写死，不跟 CPU 数走：pgxpool 缺省是 max(4, CPU 数)，在 4 核的 CI 机器上只有 4 条。
	// 真并发用例（8 个协程抢同一行 / 同一把锁）在那里会把池子占满 —— 预热池子的测试自己就卡死，
	// 而在核数多的开发机上一切正常（TT-T-29 CI 抓出）。
	cfg, perr := pgxpool.ParseConfig(dsn)
	if perr != nil {
		err = perr
		return
	}
	cfg.MaxConns = 32
	pool, err = pgxpool.NewWithConfig(context.Background(), cfg)
}

// Fresh 清空全部业务表，给当前用例一张干净的库。
//
// ⚠️ 不重建库：truncate 是毫秒级的。
// `schema_migration` 必须保留 —— 清掉它等于让迁移状态与实际结构脱节。
func Fresh(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p := Pool(t)
	ctx := context.Background()
	rows, qerr := p.Query(ctx, `
		select tablename from pg_tables
		where schemaname = 'public' and tablename <> 'schema_migration'`)
	if qerr != nil {
		t.Fatalf("列出表失败: %v", qerr)
	}
	var tables []string
	for rows.Next() {
		var name string
		if serr := rows.Scan(&name); serr != nil {
			rows.Close()
			t.Fatalf("读取表名失败: %v", serr)
		}
		tables = append(tables, `"`+name+`"`)
	}
	rows.Close()
	if rerr := rows.Err(); rerr != nil {
		t.Fatalf("列出表失败: %v", rerr)
	}
	if len(tables) == 0 {
		return p
	}
	// 一条 TRUNCATE 带上全部表：分开执行会被外键顺序卡住（虽然本项目是逻辑外键，
	// 但一次性执行本来也更快）。
	sql := "truncate table " + join(tables, ", ") + " restart identity cascade"
	if _, eerr := p.Exec(ctx, sql); eerr != nil {
		t.Fatalf("清表失败: %v", eerr)
	}
	return p
}

func join(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}
