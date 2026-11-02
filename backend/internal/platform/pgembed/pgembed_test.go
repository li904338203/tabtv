package pgembed

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/li904338203/tabtv/backend/internal/platform/resources"
)

func optsFor(t *testing.T) Options {
	t.Helper()
	home, err := resources.Postgres()
	if err != nil {
		t.Skipf("没有 PG 二进制，跳过：%v", err)
	}
	dir := t.TempDir()
	return Options{
		Home:    home,
		Data:    filepath.Join(dir, "pg"),
		State:   filepath.Join(dir, "tabtv.json"),
		LogFile: filepath.Join(dir, "logs", "postgres.log"),
	}
}

func query(t *testing.T, dsn, sql string) string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var s string
	if err := conn.QueryRow(ctx, sql).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStart_首次初始化_再次启动沿用数据(t *testing.T) {
	o := optsFor(t)
	ctx := context.Background()

	t0 := time.Now()
	s, err := Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("首次启动（含 initdb）耗时 %v", time.Since(t0))
	if got := query(t, s.DSN(), "show listen_addresses"); got != "127.0.0.1" {
		t.Fatalf("listen_addresses = %q，必须只听本机", got)
	}
	conn, _ := pgx.Connect(ctx, s.DSN())
	if _, err := conn.Exec(ctx, "create table keep_me(id int); insert into keep_me values (42)"); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}

	st, _ := loadState(o.State)
	if len(st.Password) < 32 || st.Major != Major {
		t.Fatalf("tabtv.json 不完整：%+v", st)
	}

	t1 := time.Now()
	s2, err := Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("再次启动耗时 %v", time.Since(t1))
	defer s2.Stop(ctx)
	if got := query(t, s2.DSN(), "select id::text from keep_me"); got != "42" {
		t.Fatalf("再次启动后数据不在：%q", got)
	}
}

func TestStart_端口被占时换端口并回写(t *testing.T) {
	o := optsFor(t)
	ctx := context.Background()
	s, err := Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	old := s.Port()
	_ = s.Stop(ctx)

	l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(old))
	if err != nil {
		t.Skipf("占不住旧端口：%v", err)
	}
	defer l.Close()

	s2, err := Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Stop(ctx)
	if s2.Port() == old {
		t.Fatal("端口被占时应换端口")
	}
	st, _ := loadState(o.State)
	if st.Port != s2.Port() {
		t.Fatalf("新端口没有回写：%d vs %d", st.Port, s2.Port())
	}
}

func TestStart_强杀后残留进程被接管(t *testing.T) {
	o := optsFor(t)
	ctx := context.Background()
	if _, err := Start(ctx, o); err != nil {
		t.Fatal(err)
	}
	// 模拟应用被强杀：不调用 Stop，postgres 还活着。再次 Start 必须能起来。
	s2, err := Start(ctx, o)
	if err != nil {
		t.Fatalf("残留进程时再次启动失败：%v", err)
	}
	_ = s2.Stop(ctx)
}

func TestStart_不完整目录不初始化也不删除(t *testing.T) {
	o := optsFor(t)
	if err := os.MkdirAll(o.Data, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(o.Data, "half-copied.txt")
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Start(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "缺 PG_VERSION") {
		t.Fatalf("期望拒绝，得到 %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("目录里的文件被删了")
	}
}

func TestStart_大版本不符时报错且不动数据(t *testing.T) {
	o := optsFor(t)
	if err := os.MkdirAll(o.Data, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(o.Data, "PG_VERSION"), []byte("16\n"), 0o600)
	_ = saveState(o.State, State{Password: "x", Major: "16"})
	_, err := Start(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "PostgreSQL 16") {
		t.Fatalf("期望版本错误，得到 %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(o.Data, "PG_VERSION")); string(b) != "16\n" {
		t.Fatal("PG_VERSION 被改动")
	}
}

func TestBackupCluster_复制集群且只留三份(t *testing.T) {
	o := optsFor(t)
	ctx := context.Background()
	s, err := Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	backups := filepath.Join(filepath.Dir(o.Data), "backups")
	_ = os.MkdirAll(backups, 0o755)
	for _, old := range []string{"pg-20200101T000000Z-v1", "pg-20200102T000000Z-v1", "pg-20200103T000000Z-v1"} {
		_ = os.MkdirAll(filepath.Join(backups, old), 0o755)
	}
	ns, dest, err := BackupCluster(ctx, s, backups, 81)
	if err != nil {
		t.Fatal(err)
	}
	defer ns.Stop(ctx)
	if _, err := os.Stat(filepath.Join(dest, "PG_VERSION")); err != nil {
		t.Fatal("备份里没有集群")
	}
	entries, _ := os.ReadDir(backups)
	if len(entries) != Keep {
		t.Fatalf("应只留 %d 份，现在 %d 份", Keep, len(entries))
	}
	if _, err := os.Stat(filepath.Join(backups, "pg-20200101T000000Z-v1")); err == nil {
		t.Fatal("最旧的一份应被清掉")
	}
	if got := query(t, ns.DSN(), "select 'ok'"); got != "ok" {
		t.Fatal("备份后库没有重新起来")
	}
}
