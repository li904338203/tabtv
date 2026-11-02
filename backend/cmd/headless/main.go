// Command headless 用与桌面应用完全相同的启动编排（desktop.Boot）起后端，但不开窗口，
// 而是把 /api/v1 挂在本机端口上 —— 供开发调试（Nuxt 开发服务器代理过来）、浏览器端到端验证与 UI 对照（TT-AC-05）。
//
// ⚠️ 只监听 127.0.0.1，且只用于开发与测试：它没有桌面版的进程内同源保护，本机其他程序都能访问。
//
//	go run ./cmd/headless -addr 127.0.0.1:8180            # 数据目录同桌面版（或用 TABTV_DATA_DIR 指定）
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/li904338203/tabtv/backend/desktop"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8180", "监听地址（只允许本机地址）")
	flag.Parse()
	host, _, err := net.SplitHostPort(*addr)
	if err != nil || (host != "127.0.0.1" && host != "localhost" && host != "::1") {
		slog.Error("只允许监听本机地址", "addr", *addr)
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rt, err := desktop.Boot(ctx, log)
	if err != nil {
		log.Error("启动失败", "err", err)
		os.Exit(1)
	}
	defer rt.Close(context.Background())
	srv := &http.Server{Addr: *addr, Handler: rt.Handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("监听失败", "err", err)
			stop()
		}
	}()
	log.Info("headless 已就绪", "addr", *addr, "dataDir", rt.Dirs.Root, "timings", rt.Timings)
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
}
