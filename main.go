// tabtv 桌面应用入口（dev/01 TTD-1、§4）。
package main

import (
	"embed"
	"io/fs"
	"log"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"

	"github.com/li904338203/tabtv/app"
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

	// TT-T-04 起这里接入内嵌数据库与后端装配；TT-T-03 阶段后端未就绪，接口一律 503。
	a := app.New(nil, "", "")

	err = wails.Run(&options.App{
		Title:     "tabtv",
		Width:     1440,
		Height:    900,
		MinWidth:  1024,
		MinHeight: 680,
		AssetServer: &assetserver.Options{
			Assets:     assets,
			Handler:    app.SPAFallback(assets),
			Middleware: app.APIMiddleware(app.WithProbe(a.API())),
		},
		OnStartup: a.Startup,
		Bind:      []interface{}{a},
	})
	if err != nil {
		log.Fatal(err)
	}
}
