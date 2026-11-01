// Package migrate 封装 goose 迁移（ADR-0001 TECH-04）。
//
// 迁移文件以 embed.FS 打进二进制：交付物只有一个文件，不依赖目录挂载，
// 这是新环境独立上线（FULL-09）的前提。
package migrate

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io"

	_ "github.com/jackc/pgx/v5/stdlib" // goose 需要 database/sql 驱动
	"github.com/pressly/goose/v3"
)

// FS 由 migrations 包注入，避免本包与文件布局耦合。
type FS struct {
	embed.FS
	Dir string
}

type Runner struct {
	db  *sql.DB
	dir string
}

// Open 建立迁移用连接。
// 刻意与业务用的 pgxpool 分开：迁移是短时、单连接、需要 DDL 锁的操作，
// 和业务连接池混用会让迁移期间的连接耗尽影响在线请求。
func Open(dsn string, fsys embed.FS, dir string, logOut io.Writer) (*Runner, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开迁移连接失败: %w", err)
	}
	goose.SetBaseFS(fsys)
	goose.SetTableName("schema_migration")
	if logOut != nil {
		goose.SetLogger(log{logOut})
	}
	if err := goose.SetDialect("postgres"); err != nil {
		db.Close()
		return nil, err
	}
	return &Runner{db: db, dir: dir}, nil
}

func (r *Runner) Close() error { return r.db.Close() }

func (r *Runner) Up(ctx context.Context) error {
	return goose.UpContext(ctx, r.db, r.dir)
}

// UpTo 迁移到指定版本为止。供数据迁移（补登、回填）的集成测试用：
// 先停在它之前一版造数据，再单独跑这一条验它的口径 —— 只用 Up 的话，
// 测试会随下一条迁移的加入而失去"停在哪一版"的控制。
func (r *Runner) UpTo(ctx context.Context, version int64) error {
	return goose.UpToContext(ctx, r.db, r.dir, version)
}

// Down 只回滚一步。刻意不提供 down-to-zero：
// 误执行会清空整库，而回滚的正确做法是恢复备份（07 §9）。
func (r *Runner) Down(ctx context.Context) error {
	return goose.DownContext(ctx, r.db, r.dir)
}

func (r *Runner) Status(ctx context.Context) error {
	return goose.StatusContext(ctx, r.db, r.dir)
}

func (r *Runner) Version(ctx context.Context) (int64, error) {
	return goose.GetDBVersionContext(ctx, r.db)
}

// DB 暴露给 seed / bootstrap 复用同一连接。
func (r *Runner) DB() *sql.DB { return r.db }

type log struct{ w io.Writer }

func (l log) Fatalf(format string, v ...any) { fmt.Fprintf(l.w, format, v...) }
func (l log) Printf(format string, v ...any) { fmt.Fprintf(l.w, format, v...) }
