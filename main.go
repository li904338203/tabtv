// tabtv 桌面应用入口（dev/01 TTD-1、§4）。
package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/li904338203/tabtv/app"
	"github.com/li904338203/tabtv/backend/desktop"
)

// 前端产物：Nuxt `ssr:false` + `nuxi generate` 输出到 frontend/.output/public。
//
//go:embed all:frontend/.output/public
var embedded embed.FS

func main() {
	assets, err := fs.Sub(embedded, "frontend/.output/public")
	if err != nil {
		log.Fatal(err)
	}

	dirs, _ := desktop.Dirs() // 失败也要拿到默认位置给错误页显示；真正的错误由 Boot 报
	logger, closeLog := desktop.OpenLog(dirs)
	defer closeLog()

	var (
		mu  sync.Mutex
		cur *desktop.Runtime
	)
	shutdown := func() {
		mu.Lock()
		defer mu.Unlock()
		if cur != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			cur.Close(ctx)
			cancel()
			cur = nil
		}
	}
	boot := func(ctx context.Context) (http.Handler, error) {
		shutdown() // 重试时先收掉上一次的半成品
		rt, err := desktop.Boot(ctx, logger)
		if err != nil {
			logger.Error("启动失败", "err", err)
			return nil, err
		}
		// 实时事件：经 Wails 事件通道交给前端（替代来源系统的 /ws/events，dev/01 TTD-3）。
		rt.OnEvent(func(topic string, payload []byte) {
			wruntime.EventsEmit(ctx, app.EventName, app.Event{Topic: topic, Payload: payload})
		})
		mu.Lock()
		cur = rt
		mu.Unlock()
		return rt.Handler, nil
	}
	a := app.New(boot, dirs.Root, dirs.Logs)
	ctxOf := app.ContextOf(a)
	runtimeNow := func() *desktop.Runtime {
		mu.Lock()
		defer mu.Unlock()
		return cur
	}
	guard := &app.CloseGuard{
		Active: func(ctx context.Context) (int, error) {
			rt := runtimeNow()
			if rt == nil {
				return 0, nil
			}
			w, err := rt.ActiveWork(ctx)
			return w.Total(), err
		},
		AlwaysMinimize: func() bool {
			rt := runtimeNow()
			return rt != nil && rt.CloseAlwaysMinimize()
		},
		SetAlwaysMinimize: func(ctx context.Context, on bool) {
			if rt := runtimeNow(); rt != nil {
				if err := rt.SetCloseAlwaysMinimize(ctx, on); err != nil {
					logger.Warn("保存「总是最小化」失败", "err", err)
				}
			}
		},
		Ask:      askClose,
		Minimize: wruntime.WindowMinimise,
	}
	// 自动更新（TT-T-31）：只访问 GitHub Releases；「自动检查」关掉时不发任何请求。
	bi := desktop.BuildInfo()
	app.ConfigureUpdate(a, app.UpdateConfig{
		Current: bi.Version, Commit: bi.Commit, BuildTime: bi.BuildTime,
		Quit: func() {
			guard.ForceQuit() // 安装器 / 新版本要接手：不再询问「还有任务在跑」
			if ctx := ctxOf(); ctx != nil {
				wruntime.Quit(ctx)
			}
		},
		Emit: func(info app.UpdateInfo) {
			if ctx := ctxOf(); ctx != nil {
				wruntime.EventsEmit(ctx, app.UpdateEventName, info)
			}
		},
	})
	go app.RunUpdateLoop(context.Background(), a, func() bool {
		rt := runtimeNow()
		return rt != nil && rt.UpdateAutoCheck()
	})

	// 系统关机 / 注销 / kill：走同一条退出路径，不询问（TT-T-30）。
	// ⚠️ 不接的话 Go 默认直接结束进程，OnShutdown 不会被调用，内嵌 PG 被强杀，下次启动要做崩溃恢复。
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		guard.ForceQuit()
		if ctx := ctxOf(); ctx != nil {
			wruntime.Quit(ctx)
			return
		}
		shutdown()
		os.Exit(0)
	}()

	err = wails.Run(&options.App{
		Title:     "tabtv",
		Width:     1440,
		Height:    900,
		MinWidth:  1024,
		MinHeight: 680,
		AssetServer: &assetserver.Options{
			Assets:     assets,
			Handler:    app.SPAFallback(assets),
			Middleware: chain(app.SaveMiddleware(app.ContextOf(a)), app.APIMiddleware(app.WithProbe(a.API()))),
		},
		OnStartup:     a.Startup,
		OnBeforeClose: guard.BeforeClose,
		OnShutdown:    func(context.Context) { shutdown() },
		// 只运行一个实例：两个进程抢同一个数据目录与 PG 集群会损坏数据。
		// 第二次打开应用时把已有窗口唤起（也是「最小化后台继续」之后找回窗口的入口）。
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId: "com.tabtv.app",
			OnSecondInstanceLaunch: func(options.SecondInstanceData) {
				if ctx := ctxOf(); ctx != nil {
					wruntime.WindowUnminimise(ctx)
					wruntime.WindowShow(ctx)
				}
			},
		},
		Bind: []interface{}{a},
	})
	if err != nil {
		log.Fatal(err)
	}
}

// askClose 在有任务进行时询问关闭方式。
// ⚠️ Wails v2 的问题对话框只有 macOS 支持自定义按钮；Windows / Linux 固定为「是 / 否」，文案按此改写。
func askClose(ctx context.Context, active int) app.CloseChoice {
	msg := fmt.Sprintf("还有 %d 项任务正在进行。", active)
	if runtime.GOOS == "darwin" {
		const keep, always, quit, cancel = "最小化，任务继续", "总是最小化", "仍然退出", "取消"
		got, err := wruntime.MessageDialog(ctx, wruntime.MessageDialogOptions{
			Type: wruntime.QuestionDialog, Title: "关闭 tabtv",
			Message:       msg + "最小化后任务会在后台跑完；退出的话进行中的任务会在下次启动时继续。",
			Buttons:       []string{keep, always, quit, cancel},
			DefaultButton: keep, CancelButton: cancel,
		})
		switch {
		case err != nil:
			return app.CloseCancel
		case got == keep:
			return app.CloseMinimize
		case got == always:
			return app.CloseMinimizeAlways
		case got == quit:
			return app.CloseQuit
		}
		return app.CloseCancel
	}
	got, err := wruntime.MessageDialog(ctx, wruntime.MessageDialogOptions{
		Type: wruntime.QuestionDialog, Title: "关闭 tabtv",
		Message:       msg + "\n\n是否最小化窗口、让任务在后台跑完？\n选「否」将退出，进行中的任务会在下次启动时继续。",
		DefaultButton: "Yes",
	})
	switch {
	case err != nil:
		return app.CloseCancel
	case got == "Yes":
		return app.CloseMinimize
	case got == "No":
		return app.CloseQuit
	}
	return app.CloseCancel // 用 Esc / 关闭按钮关掉对话框：当作没关
}

// chain 依次套上中间件（第一个最外层）。
func chain(ms ...func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		for i := len(ms) - 1; i >= 0; i-- {
			next = ms[i](next)
		}
		return next
	}
}
