package app

import (
	"io/fs"
	"net/http"
	"strings"
)

// APIPrefix 是交给后端路由的路径前缀（dev/01 TTD-2）。
// ⚠️ 精确按「/api/」匹配：只写 "/api" 的话 /apix 这类前端路由也会被吞进后端。
const APIPrefix = "/api/"

// APIMiddleware 把 /api/ 前缀的请求交给 api，其余交回 Wails 的静态资源处理。
//
// 用 Middleware 而不是 AssetServer.Handler：后者只在静态资源找不到时才被调用，
// 一旦前端产物里恰好有同名文件，接口请求就会被当成静态文件吐回去。
func APIMiddleware(api http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, APIPrefix) {
				api.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// SPAFallback 在静态资源里找不到文件时返回 index.html，由前端路由接手
// （/video/projects 这类路径在产物里没有对应文件）。
//
// 带扩展名的路径（.js、.png…）找不到就是真的 404：回 index.html 的话，
// 浏览器会把一段 HTML 当成脚本执行，报出一条毫不相干的语法错误。
func SPAFallback(assets fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		last := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if strings.Contains(last, ".") {
			http.NotFound(w, r)
			return
		}
		b, err := fs.ReadFile(assets, "index.html")
		if err != nil {
			http.Error(w, "前端产物缺少 index.html", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(b)
	})
}

// NotReadyAPI 是后端还没装配好时的占位：所有接口一律 503，
// 前端据此显示启动中或错误页，而不是把 404 当成「没有这个功能」。
func NotReadyAPI() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"code":50310,"message":"后端尚未就绪"}`))
	})
}
