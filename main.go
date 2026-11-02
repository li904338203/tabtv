// tabtv 桌面应用入口（dev/01 TTD-1、§4）。
package main

import (
	"context"
	"embed"
	"io/fs"
	"log"
	"net/http"
	"sync"
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
		OnStartup:  a.Startup,
		OnShutdown: func(context.Context) { shutdown() },
		Bind:       []interface{}{a},
	})
	if err != nil {
		log.Fatal(err)
	}
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
