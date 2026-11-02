package app

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
)

// StartupError 是启动失败时交给错误页的详情（TT-F-02）。
type StartupError struct {
	Reason  string `json:"reason"`
	Detail  string `json:"detail"`
	DataDir string `json:"dataDir"`
	LogDir  string `json:"logDir"`
}

// Booter 执行一次完整启动（数据库、迁移、装配），成功返回后端路由。
// 重试按钮会再次调用它，所以实现必须可重入：上一次留下的半成品要自己清理。
type Booter func(ctx context.Context) (http.Handler, error)

// App 是暴露给前端的 Wails 绑定对象。
type App struct {
	ctx     context.Context
	boot    Booter
	dataDir string
	logDir  string

	mu      sync.Mutex
	failure *StartupError
	api     atomic.Pointer[http.Handler]
	update  *updateState
}

// New 创建 App。boot 为 nil 时只提供静态页面（T03 的骨架阶段）。
func New(boot Booter, dataDir, logDir string) *App {
	a := &App{boot: boot, dataDir: dataDir, logDir: logDir}
	h := NotReadyAPI()
	a.api.Store(&h)
	return a
}

// ContextOf 返回 Wails 的应用 context（窗口创建后才有）。
// ⚠️ 写成包级函数而不是 App 的方法：App 的导出方法都会被 Wails 绑定给前端，context 无法序列化。
func ContextOf(a *App) func() context.Context {
	return func() context.Context { return a.ctx }
}

// API 返回始终指向「当前」后端路由的 handler：重试成功后无需重建 AssetServer。
func (a *App) API() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		(*a.api.Load()).ServeHTTP(w, r)
	})
}

// Startup 由 Wails 在窗口创建后调用。
func (a *App) Startup(ctx context.Context) {
	a.ctx = ctx
	a.runBoot()
}

func (a *App) runBoot() {
	if a.boot == nil {
		return
	}
	h, err := a.boot(a.ctx)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.failure = &StartupError{Reason: "启动失败", Detail: err.Error(), DataDir: a.dataDir, LogDir: a.logDir}
		return
	}
	a.failure = nil
	a.api.Store(&h)
}

// GetStartupError 供前端错误页读取；为 nil 表示启动正常。
func (a *App) GetStartupError() *StartupError {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.failure
}

// RetryStartup 重新执行一次启动，返回新的错误（nil 表示成功）。
func (a *App) RetryStartup() *StartupError {
	a.runBoot()
	return a.GetStartupError()
}
