// Package testcluster 给集成测试提供一个**跨进程共享**的内嵌 PostgreSQL 集群（TT-T-04）。
//
// 来源系统每个测试包起一个 docker 容器；tabtv 不依赖 docker：
//   - 集群放在固定目录（TABTV_TEST_PG_DIR，缺省系统临时目录下的 tabtv-test-pg），
//     第一个进程起它，其余进程直接连（目录锁串行化，避免两个进程同时 initdb）；
//   - 迁移只跑一次，结果存成模板库 tpl_v<迁移版本>；每个测试进程从模板 `create database` 一份自己的库，
//     毫秒级、互相隔离；迁移有新版本时模板名随之变化，旧模板自然作废；
//   - 集群跑完不停（下次复用，省掉 initdb）。`make test-clean` 删掉测试库并停集群。
package testcluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/li904338203/tabtv/backend/internal/platform/migrate"
	"github.com/li904338203/tabtv/backend/internal/platform/pgembed"
	"github.com/li904338203/tabtv/backend/internal/platform/resources"
	"github.com/li904338203/tabtv/backend/migrations"
)

// Dir 返回共享测试集群所在目录。
func Dir() string {
	if v := os.Getenv("TABTV_TEST_PG_DIR"); v != "" {
		return v
	}
	return filepath.Join(os.TempDir(), "tabtv-test-pg")
}

// Options 返回共享测试集群的启动参数。
func Options() (pgembed.Options, error) {
	home, err := resources.Postgres()
	if err != nil {
		return pgembed.Options{}, err
	}
	dir := Dir()
	return pgembed.Options{
		Home:    home,
		Data:    filepath.Join(dir, "pg"),
		State:   filepath.Join(dir, "tabtv.json"),
		LogFile: filepath.Join(dir, "postgres.log"),
		// 多个测试包并行（make test-it 是 -p 4），每个包一个 32 连接的池（testsupport），再加模板与管理连接。
		MaxConnections: 200,
	}, nil
}

// Ensure 确保共享集群在运行（必要时启动）。
func Ensure(ctx context.Context) (*pgembed.Server, error) {
	o, err := Options()
	if err != nil {
		return nil, err
	}
	if s, err := pgembed.Attach(ctx, o); err == nil {
		return s, nil
	}
	unlock, err := lock(ctx, Dir())
	if err != nil {
		return nil, err
	}
	defer unlock()
	if s, err := pgembed.Attach(ctx, o); err == nil {
		return s, nil
	}
	return pgembed.Start(ctx, o)
}

// NewDatabase 从迁移好的模板建一个本进程专用的库，返回连接串。
func NewDatabase(ctx context.Context) (string, error) {
	s, err := Ensure(ctx)
	if err != nil {
		return "", err
	}
	tpl, err := ensureTemplate(ctx, s)
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("t_%d_%s", os.Getpid(), randomHex(4))
	conn, err := pgx.Connect(ctx, s.AdminDSN())
	if err != nil {
		return "", err
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, fmt.Sprintf(`create database %q template %q`, name, tpl)); err != nil {
		return "", fmt.Errorf("从模板建测试库失败：%w", err)
	}
	return s.DSNFor(name), nil
}

// EmptyDatabase 建一个空库（迁移测试要从零开始跑）。
func EmptyDatabase(ctx context.Context) (string, error) {
	s, err := Ensure(ctx)
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("t_%d_%s", os.Getpid(), randomHex(4))
	conn, err := pgx.Connect(ctx, s.AdminDSN())
	if err != nil {
		return "", err
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, fmt.Sprintf(`create database %q template template0 encoding 'UTF8'`, name)); err != nil {
		return "", err
	}
	return s.DSNFor(name), nil
}

func ensureTemplate(ctx context.Context, s *pgembed.Server) (string, error) {
	want, err := migrate.ExpectedVersion(migrations.FS, migrations.Dir)
	if err != nil {
		return "", err
	}
	tpl := fmt.Sprintf("tpl_v%d", want)
	exists := func() (bool, error) {
		conn, err := pgx.Connect(ctx, s.AdminDSN())
		if err != nil {
			return false, err
		}
		defer conn.Close(ctx)
		var ok bool
		err = conn.QueryRow(ctx, "select exists(select 1 from pg_database where datname=$1)", tpl).Scan(&ok)
		return ok, err
	}
	if ok, err := exists(); err != nil || ok {
		return tpl, err
	}
	unlock, err := lock(ctx, Dir())
	if err != nil {
		return "", err
	}
	defer unlock()
	if ok, err := exists(); err != nil || ok {
		return tpl, err
	}
	// 先迁移到一个临时名字，成功后再改名 —— 迁移失败时不会留下一个半成品模板被别的进程拿去用。
	build := tpl + "_building"
	conn, err := pgx.Connect(ctx, s.AdminDSN())
	if err != nil {
		return "", err
	}
	defer conn.Close(ctx)
	_, _ = conn.Exec(ctx, fmt.Sprintf(`drop database if exists %q`, build))
	if _, err := conn.Exec(ctx, fmt.Sprintf(`create database %q template template0 encoding 'UTF8'`, build)); err != nil {
		return "", err
	}
	runner, err := migrate.Open(s.DSNFor(build), migrations.FS, migrations.Dir, nil)
	if err != nil {
		return "", err
	}
	upErr := runner.Up(ctx)
	_ = runner.Close()
	if upErr != nil {
		return "", fmt.Errorf("迁移测试模板失败：%w", upErr)
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf(`alter database %q rename to %q`, build, tpl)); err != nil {
		return "", err
	}
	return tpl, nil
}

// Clean 删除全部测试库（t_*），保留模板。
func Clean(ctx context.Context) (int, error) {
	o, err := Options()
	if err != nil {
		return 0, err
	}
	s, err := pgembed.Attach(ctx, o)
	if err != nil {
		return 0, nil // 集群没在跑，没什么可清
	}
	conn, err := pgx.Connect(ctx, s.AdminDSN())
	if err != nil {
		return 0, err
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, "select datname from pg_database where datname like 't\\_%'")
	if err != nil {
		return 0, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return 0, err
		}
		names = append(names, n)
	}
	rows.Close()
	for _, n := range names {
		if _, err := conn.Exec(ctx, fmt.Sprintf(`drop database if exists %q with (force)`, n)); err != nil {
			return 0, err
		}
	}
	return len(names), nil
}

// Stop 停止共享集群。
func Stop(ctx context.Context) error {
	o, err := Options()
	if err != nil {
		return err
	}
	s, err := pgembed.Attach(ctx, o)
	if err != nil {
		return nil
	}
	return s.Stop(ctx)
}

// lock 用「建目录」做跨进程锁（Windows 与 Unix 行为一致）；超过 3 分钟的锁视为残留。
func lock(ctx context.Context, dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, ".lock")
	deadline := time.Now().Add(5 * time.Minute)
	for {
		err := os.Mkdir(path, 0o755)
		if err == nil {
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if st, serr := os.Stat(path); serr == nil && time.Since(st.ModTime()) > 3*time.Minute {
			_ = os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("等待测试集群锁超时：%s", path)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
