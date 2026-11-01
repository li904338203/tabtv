//go:build integration

// Package testsupport 给集成测试提供**每个包共用一个** PostgreSQL 容器。
//
// ⚠️ 起因：原先每个测试函数各起一个容器。P05-01 时集成测试涨到 25 个以上，
// colima 被压垮，出现 60 秒等不到 "database system is ready"、
// 以及连接被拒的失败 —— 全是基础设施问题，与被测逻辑无关，
// 但它会让每次 `make test-it` 随机变红，久而久之没人再相信这套测试。
//
// Go 对每个包起一个独立的测试进程，因此"每进程一个容器"等于"每包一个"：
// 容器数从测试函数数降到包数（6 个左右），且包之间仍然隔离。
//
// 隔离靠**每个用例前清表**而不是重建容器：truncate 是毫秒级的。
package testsupport

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/li904338203/tabtv/backend/internal/platform/migrate"
	"github.com/li904338203/tabtv/backend/migrations"
)

var (
	once sync.Once
	pool *pgxpool.Pool
	dsn  string
	err  error
)

// Pool 返回本进程共用的连接池，首次调用时起容器并跑迁移。
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
	ctx := context.Background()
	var c *tcpostgres.PostgresContainer
	c, err = tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("seeaitv"), tcpostgres.WithUsername("seeaitv"),
		tcpostgres.WithPassword("seeaitv"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				// ⚠️ 超时给到 180 秒：机器上并行跑着好几个包的容器时，
				// 90 秒会在负载高峰偶发超时，而这与被测逻辑无关。
				WithOccurrence(2).WithStartupTimeout(180*time.Second)))
	if err != nil {
		return
	}
	dsn, err = c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return
	}

	var runner *migrate.Runner
	runner, err = migrate.Open(dsn, migrations.FS, migrations.Dir, nil)
	if err != nil {
		return
	}
	if err = runner.Up(ctx); err != nil {
		return
	}
	_ = runner.Close()

	pool, err = pgxpool.New(ctx, dsn)
	// 容器**不主动销毁**：进程退出时 testcontainers 的 ryuk 或 docker 会回收。
	// 在这里注册 cleanup 反而会在第一个用例结束时就把它关掉。
}

// Fresh 清空全部业务表，给当前用例一张干净的库。
//
// ⚠️ 不重建容器：truncate 是毫秒级的，而重建容器是十几秒。
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

// ---------------------------------------------------------------- Redis

var (
	redisOnce sync.Once
	redisAddr string
	redisErr  error
)

// RedisAddr 返回本进程共用的 Redis 地址。
//
// ⚠️ 与 Postgres 同一个理由：每个用例各起一个容器时，
// P05-04 实测出现过「Redis 客户端连到某个 HTTP 服务」的报错
// （`can't parse map reply: HTTP/1.1 400 Bad Request`）——
// 容器的映射端口在高并发下撞上了别的服务。
func RedisAddr(t *testing.T) string {
	t.Helper()
	redisOnce.Do(startRedis)
	if redisErr != nil {
		t.Fatalf("启动测试 Redis 失败: %v", redisErr)
	}
	return redisAddr
}

func startRedis() {
	ctx := context.Background()
	var c testcontainers.Container
	c, redisErr = testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "redis:8-alpine",
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForLog("Ready to accept connections").WithStartupTimeout(120 * time.Second),
		},
		Started: true,
	})
	if redisErr != nil {
		return
	}
	host, err := c.Host(ctx)
	if err != nil {
		redisErr = err
		return
	}
	port, err := c.MappedPort(ctx, "6379/tcp")
	if err != nil {
		redisErr = err
		return
	}
	redisAddr = host + ":" + port.Port()
}

// FlushRedis 清空当前 Redis，给用例一张干净的表。
func FlushRedis(t *testing.T, flush func(addr string) error) string {
	t.Helper()
	addr := RedisAddr(t)
	if err := flush(addr); err != nil {
		t.Fatalf("清空 Redis 失败: %v", err)
	}
	return addr
}
