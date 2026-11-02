// Package desktop 是桌面应用的启动编排（dev/01 §4）：数据目录 → 内嵌数据库 → 迁移 → 装配。
//
// 放在 backend 里而不是根目录的 app 包：Go 的 internal 规则不允许 backend 之外的包
// 引用 backend/internal/...，而编排要用到其中几乎所有东西。根目录只负责 Wails 窗口与绑定。
package desktop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/li904338203/tabtv/backend/internal/platform/datadir"
	"github.com/li904338203/tabtv/backend/internal/platform/db"
	"github.com/li904338203/tabtv/backend/internal/platform/eventbus"
	"github.com/li904338203/tabtv/backend/internal/platform/migrate"
	"github.com/li904338203/tabtv/backend/internal/platform/pgembed"
	"github.com/li904338203/tabtv/backend/internal/platform/resources"
	"github.com/li904338203/tabtv/backend/internal/platform/settings"
	"github.com/li904338203/tabtv/backend/migrations"
)

// Stage 标出启动失败发生在哪一步，错误页据此给出处置建议。
type Stage string

const (
	StageDataDir  Stage = "datadir"
	StageDatabase Stage = "database"
	StageBackup   Stage = "backup"
	StageMigrate  Stage = "migrate"
	StageAssemble Stage = "assemble"
)

// Error 是带阶段与位置信息的启动错误。
type Error struct {
	Stage   Stage
	DataDir string
	LogDir  string
	Err     error
}

func (e *Error) Error() string { return fmt.Sprintf("%s：%v", stageText(e.Stage), e.Err) }
func (e *Error) Unwrap() error { return e.Err }

func stageText(s Stage) string {
	switch s {
	case StageDataDir:
		return "准备数据目录失败"
	case StageDatabase:
		return "启动内嵌数据库失败"
	case StageBackup:
		return "迁移前备份数据库失败（为保护数据，未执行迁移）"
	case StageMigrate:
		return "升级数据库结构失败"
	case StageAssemble:
		return "初始化应用失败"
	default:
		return "启动失败"
	}
}

// Runtime 是一次成功启动的结果。
type Runtime struct {
	Dirs    datadir.Dirs
	DB      *pgembed.Server
	Handler http.Handler
	Timings map[string]time.Duration
	// Bus 是事件总线：宿主窗口就绪后用 SetEmitter 接上出口（Wails EventsEmit）。
	Bus *eventbus.Local
	// Builtin 是本次启动安装内置营销内容的结果（TT-T-24）。
	Builtin BuiltinReport

	pool      *db.Pool
	settings  *settings.Store
	bg        *background
	cancel    context.CancelFunc
	closeOnce sync.Once
}

// Close 停后台工作（限时 10 秒，没跑完的任务留在 running，下次启动由恢复扫描接管）、关连接池、停库。
func (r *Runtime) Close(ctx context.Context) {
	r.closeOnce.Do(func() {
		if r.cancel != nil {
			r.cancel()
		}
		if r.bg != nil {
			r.bg.Wait(10 * time.Second)
		}
		if r.pool != nil {
			_ = r.pool.Close()
		}
		if r.DB != nil {
			_ = r.DB.Stop(ctx)
		}
	})
}

// Dirs 尽早解析数据目录（启动失败时错误页也要显示它）。
func Dirs() (datadir.Dirs, error) { return datadir.Resolve() }

// Boot 执行一次完整启动。失败返回 *Error；已启动的数据库会被停掉，重试可以从头再来。
func Boot(ctx context.Context, log *slog.Logger) (*Runtime, error) {
	timings := map[string]time.Duration{}
	t0 := time.Now()

	dirs, err := datadir.Resolve()
	if err != nil {
		return nil, &Error{Stage: StageDataDir, DataDir: dirs.Root, LogDir: dirs.Logs, Err: err}
	}
	fail := func(stage Stage, err error) error {
		return &Error{Stage: stage, DataDir: dirs.Root, LogDir: dirs.Logs, Err: err}
	}

	home, err := resources.Postgres()
	if err != nil {
		return nil, fail(StageDatabase, err)
	}
	opts := pgembed.Options{
		Home:    home,
		Data:    dirs.PG,
		State:   dirs.Config,
		LogFile: filepath.Join(dirs.Logs, "postgres.log"),
	}
	srv, err := pgembed.Start(ctx, opts)
	if err != nil {
		return nil, fail(StageDatabase, err)
	}
	timings["database"] = time.Since(t0)

	t1 := time.Now()
	srv, err = migrateWithBackup(ctx, srv, dirs, log)
	if err != nil {
		if srv != nil {
			_ = srv.Stop(context.Background())
		}
		return nil, err.(*stageErr).wrap(dirs)
	}
	timings["migrate"] = time.Since(t1)

	t2 := time.Now()
	rt := &Runtime{Dirs: dirs, DB: srv, Timings: timings}
	if err := rt.start(ctx, log); err != nil {
		_ = srv.Stop(context.Background())
		return nil, fail(StageAssemble, err)
	}
	timings["assemble"] = time.Since(t2)
	log.Info("启动完成", "dataDir", dirs.Root, "database", timings["database"], "migrate", timings["migrate"],
		"assemble", timings["assemble"])
	return rt, nil
}

type stageErr struct {
	stage Stage
	err   error
}

func (e *stageErr) Error() string { return e.err.Error() }
func (e *stageErr) wrap(d datadir.Dirs) error {
	return &Error{Stage: e.stage, DataDir: d.Root, LogDir: d.Logs, Err: e.err}
}

// migrateWithBackup：库落后则先备份（空库除外）再迁移；超前只告警放行（来源系统的版本闸规则）。
func migrateWithBackup(ctx context.Context, srv *pgembed.Server, dirs datadir.Dirs, log *slog.Logger) (*pgembed.Server, error) {
	expected, err := migrate.ExpectedVersion(migrations.FS, migrations.Dir)
	if err != nil {
		return srv, &stageErr{StageMigrate, err}
	}
	applied, err := appliedVersion(ctx, srv)
	if err != nil {
		return srv, &stageErr{StageMigrate, err}
	}
	switch {
	case applied > expected:
		log.Warn("数据库结构比本版应用新（可能刚回退了应用版本），照常启动", "applied", applied, "expected", expected)
		return srv, nil
	case applied == expected:
		return srv, nil
	}
	if applied > 0 {
		ns, dest, err := pgembed.BackupCluster(ctx, srv, dirs.Backups, applied)
		if err != nil {
			return ns, &stageErr{StageBackup, err}
		}
		srv = ns
		log.Info("迁移前已备份数据库", "backup", dest, "from", applied, "to", expected)
	}
	runner, err := migrate.Open(srv.DSN(), migrations.FS, migrations.Dir, io.Discard)
	if err != nil {
		return srv, &stageErr{StageMigrate, err}
	}
	defer runner.Close()
	if err := runner.Up(ctx); err != nil {
		return srv, &stageErr{StageMigrate, err}
	}
	return srv, nil
}

func appliedVersion(ctx context.Context, srv *pgembed.Server) (int64, error) {
	runner, err := migrate.Open(srv.DSN(), migrations.FS, migrations.Dir, io.Discard)
	if err != nil {
		return 0, err
	}
	defer runner.Close()
	// goose 在版本表不存在时会建表并返回 0（空库）。
	return runner.Version(ctx)
}

// AsError 把任意错误还原成 *Error（取阶段与目录）。
func AsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// OpenLog 打开数据目录里的应用日志（按天一个文件）。失败时退回标准错误输出。
func OpenLog(dirs datadir.Dirs) (*slog.Logger, func()) {
	if err := os.MkdirAll(dirs.Logs, 0o755); err != nil {
		return slog.New(slog.NewTextHandler(os.Stderr, nil)), func() {}
	}
	name := filepath.Join(dirs.Logs, "tabtv-"+time.Now().Format("2006-01-02")+".log")
	f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return slog.New(slog.NewTextHandler(os.Stderr, nil)), func() {}
	}
	return slog.New(slog.NewJSONHandler(io.MultiWriter(f, os.Stderr), nil)), func() { _ = f.Close() }
}

// OnEvent 接上实时事件的出口（TTD-3）：宿主把它转成 Wails 的 EventsEmit。
// 每条事件是 {topic, payload}，payload 是原样的事件 JSON；前端按 topic 自己过滤。
func (r *Runtime) OnEvent(f func(topic string, payload []byte)) {
	if r.Bus != nil {
		r.Bus.SetEmitter(f)
	}
}
