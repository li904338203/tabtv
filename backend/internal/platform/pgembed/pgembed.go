// Package pgembed 管理随包附带的 PostgreSQL 进程（dev/02 TTD-20/22）。
//
// ⚠️ 不用 fergusstrange/embedded-postgres 管理进程：它在判定数据目录「无效」（例如 PG_VERSION
// 对不上）时会 RemoveAll 整个数据目录再重新 initdb —— 对一个存着用户全部项目的桌面应用，
// 这等于一次静默清库。这里的规则只有一条：**任何情况都不删数据**。
// 目录里没有 PG_VERSION 才 initdb；有但版本不对就报错，交给错误页说明。
//
// 二进制来自 zonky 预编译包（与那个库用的是同一份），只含 initdb / pg_ctl / postgres，
// 没有 pg_dump —— 迁移前备份因此是停库后整目录复制（backup.go）。
package pgembed

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Major 是支持的 PostgreSQL 大版本。数据目录的 PG_VERSION 必须与它一致。
const Major = "17"

const (
	user     = "tabtv"
	database = "tabtv"
)

// Options 描述一个内嵌集群。
type Options struct {
	Home    string // PG 二进制根目录（其下 bin/ lib/ share/）
	Data    string // 集群目录
	State   string // tabtv.json：端口、密码、集群版本
	LogFile string // postgres 日志
}

// State 落在数据目录的 tabtv.json 里。
type State struct {
	Port     int    `json:"port"`
	Password string `json:"password"`
	Major    string `json:"pgMajor"`
}

// Server 是一个已启动的集群。
type Server struct {
	opts  Options
	state State
}

// DSN 返回连接串（只连本机）。
func (s *Server) DSN() string {
	return fmt.Sprintf("postgres://%s:%s@127.0.0.1:%d/%s?sslmode=disable", user, s.state.Password, s.state.Port, database)
}

// Port 返回监听端口。
func (s *Server) Port() int { return s.state.Port }

// Options 返回启动参数（备份时要停库再起库）。
func (s *Server) Options() Options { return s.opts }

func (o Options) bin(name string) string {
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(o.Home, "bin", name)
}

// Start 初始化（必要时）并启动集群，确保库 tabtv 存在。
func Start(ctx context.Context, o Options) (*Server, error) {
	if _, err := os.Stat(o.bin("pg_ctl")); err != nil {
		return nil, fmt.Errorf("找不到 PostgreSQL 程序 %s：%w", o.bin("pg_ctl"), err)
	}
	st, err := loadState(o.State)
	if err != nil {
		return nil, err
	}
	fresh, err := needsInit(o.Data)
	if err != nil {
		return nil, err
	}
	if fresh {
		if st.Password == "" {
			st.Password = randomHex(24)
		}
		if err := initdb(ctx, o, st.Password); err != nil {
			return nil, err
		}
		st.Major = Major
	} else if st.Password == "" {
		return nil, fmt.Errorf("集群 %s 已存在但 %s 里没有它的密码；数据未被改动，请从备份恢复 tabtv.json", o.Data, o.State)
	}
	if err := checkMajor(o.Data); err != nil {
		return nil, err
	}
	if err := writeConf(o.Data); err != nil {
		return nil, err
	}

	// 上次异常退出时 postgres 可能还活着（应用被强杀、postgres 没跟着退）：先停掉它再起，
	// 否则新端口起不来，而旧进程占着数据目录。
	if running(ctx, o) {
		_ = stop(ctx, o, "fast")
	}

	st.Port = choosePort(st.Port)
	if err := saveState(o.State, st); err != nil {
		return nil, err
	}
	if err := pgctlStart(ctx, o, st.Port); err != nil {
		return nil, err
	}
	s := &Server{opts: o, state: st}
	if err := s.ensureDatabase(ctx); err != nil {
		_ = s.Stop(context.Background())
		return nil, err
	}
	return s, nil
}

// Stop 优雅停库：fast 限时 10 秒，超时 immediate（下次启动由 PG 自身做崩溃恢复）。
func (s *Server) Stop(ctx context.Context) error {
	if err := stop(ctx, s.opts, "fast"); err != nil {
		return stop(ctx, s.opts, "immediate")
	}
	return nil
}

func needsInit(data string) (bool, error) {
	if _, err := os.Stat(filepath.Join(data, "PG_VERSION")); err == nil {
		return false, nil
	}
	entries, err := os.ReadDir(data)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if len(entries) > 0 {
		// 有东西却没有 PG_VERSION：可能是复制到一半的目录。绝不在上面 initdb。
		return false, fmt.Errorf("数据库目录 %s 不为空却不是一个完整的 PostgreSQL 集群（缺 PG_VERSION）；未做任何改动", data)
	}
	return true, nil
}

func checkMajor(data string) error {
	b, err := os.ReadFile(filepath.Join(data, "PG_VERSION"))
	if err != nil {
		return fmt.Errorf("读取 PG_VERSION 失败：%w", err)
	}
	if got := strings.TrimSpace(string(b)); got != Major {
		return fmt.Errorf("数据库集群版本是 PostgreSQL %s，本版 tabtv 只支持 %s；数据未被改动", got, Major)
	}
	return nil
}

func initdb(ctx context.Context, o Options, password string) error {
	if err := os.MkdirAll(o.Data, 0o700); err != nil {
		return err
	}
	pw, err := os.CreateTemp("", "tabtv-pw-*")
	if err != nil {
		return err
	}
	defer os.Remove(pw.Name())
	if _, err := pw.WriteString(password + "\n"); err != nil {
		pw.Close()
		return err
	}
	pw.Close()
	cmd := command(ctx, o.bin("initdb"),
		"-D", o.Data, "-U", user, "--pwfile="+pw.Name(),
		"-E", "UTF8", "--locale=C", "-A", "scram-sha-256")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("初始化数据库失败：%w\n%s", err, tail(out))
	}
	return nil
}

// writeConf 每次启动重写 tabtv.conf 并确保被 postgresql.conf 引入（TTD-22）。
// 写成文件而不是 pg_ctl -o 参数：-o 在 Windows 上经 cmd 解释，空字符串引号不会被剥掉。
func writeConf(data string) error {
	conf := strings.Join([]string{
		"# 由 tabtv 每次启动重写，请勿手改",
		"listen_addresses = '127.0.0.1'",
		"unix_socket_directories = ''",
		"max_connections = 40",
		"shared_buffers = 128MB",
		"fsync = on",
		"timezone = 'UTC'",
		"log_timezone = 'UTC'",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(data, "tabtv.conf"), []byte(conf), 0o600); err != nil {
		return err
	}
	main := filepath.Join(data, "postgresql.conf")
	b, err := os.ReadFile(main)
	if err != nil {
		return err
	}
	if strings.Contains(string(b), "include_if_exists = 'tabtv.conf'") {
		return nil
	}
	f, err := os.OpenFile(main, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString("\ninclude_if_exists = 'tabtv.conf'\n")
	return err
}

func pgctlStart(ctx context.Context, o Options, port int) error {
	if err := os.MkdirAll(filepath.Dir(o.LogFile), 0o755); err != nil {
		return err
	}
	cmd := command(ctx, o.bin("pg_ctl"), "start", "-D", o.Data, "-w", "-t", "60",
		"-l", o.LogFile, "-o", "-p "+strconv.Itoa(port))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("启动数据库失败：%w\n%s\n（详见 %s）", err, tail(out), o.LogFile)
	}
	return nil
}

func running(ctx context.Context, o Options) bool {
	if _, err := os.Stat(filepath.Join(o.Data, "postmaster.pid")); err != nil {
		return false
	}
	return command(ctx, o.bin("pg_ctl"), "status", "-D", o.Data).Run() == nil
}

func stop(ctx context.Context, o Options, mode string) error {
	cmd := command(ctx, o.bin("pg_ctl"), "stop", "-D", o.Data, "-m", mode, "-w", "-t", "10")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("停止数据库失败（%s）：%w\n%s", mode, err, tail(out))
	}
	return nil
}

func (s *Server) ensureDatabase(ctx context.Context) error {
	dsn := fmt.Sprintf("postgres://%s:%s@127.0.0.1:%d/postgres?sslmode=disable", user, s.state.Password, s.state.Port)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("连接数据库失败：%w", err)
	}
	defer conn.Close(ctx)
	var exists bool
	if err := conn.QueryRow(ctx, "select exists(select 1 from pg_database where datname = $1)", database).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err = conn.Exec(ctx, "create database "+database+" encoding 'UTF8' template template0")
	return err
}

// choosePort 优先沿用上次的端口；被占了就换一个空闲端口（端口写回 tabtv.json）。
func choosePort(prev int) int {
	if prev > 0 && portFree(prev) {
		return prev
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 54329
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func portFree(port int) bool {
	l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

func loadState(path string) (State, error) {
	var st State
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("读取 %s 失败：%w", path, err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("%s 不是合法的 JSON：%w", path, err)
	}
	return st, nil
}

func saveState(path string, st State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	hideWindow(cmd)
	return cmd
}

func tail(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 2000 {
		s = "…" + s[len(s)-2000:]
	}
	return s
}

// WaitReady 轮询直到能连上（给备份后重启用）。
func (s *Server) WaitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := pgx.Connect(ctx, s.DSN())
		if err == nil {
			conn.Close(ctx)
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// Attach 连接一个已经在运行的集群（不启动、不停止）。共享测试集群用：
// 多个测试进程同时起来时，后来者直接用先到者起好的那个，而不是把它停掉重起。
func Attach(ctx context.Context, o Options) (*Server, error) {
	st, err := loadState(o.State)
	if err != nil {
		return nil, err
	}
	if st.Port == 0 || st.Password == "" {
		return nil, errors.New("集群尚未初始化")
	}
	s := &Server{opts: o, state: st}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	conn, err := pgx.Connect(cctx, s.DSN())
	if err != nil {
		return nil, err
	}
	conn.Close(ctx)
	return s, nil
}

// AdminDSN 返回连到 postgres 库的连接串（建库、删库用）。
func (s *Server) AdminDSN() string {
	return fmt.Sprintf("postgres://%s:%s@127.0.0.1:%d/postgres?sslmode=disable", user, s.state.Password, s.state.Port)
}

// DSNFor 返回连到指定库的连接串。
func (s *Server) DSNFor(db string) string {
	return fmt.Sprintf("postgres://%s:%s@127.0.0.1:%d/%s?sslmode=disable", user, s.state.Password, s.state.Port, db)
}
