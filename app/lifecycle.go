package app

import (
	"context"
	"sync/atomic"
)

// 关窗口不等于杀任务（TT-T-30，TT-F-03）。
//
// 窗口、调度器、数据库都在同一个进程里：Wails 默认关窗即退出，running 的生成会被打断，
// 「提交结果不确定」的那一类下次启动只能判失败 —— 能不中断就不该中断。
//
// ⚠️ v1 不做托盘（dev/04 R-5）：Wails v2 没有托盘，第三方 systray 要与 Wails 抢 macOS 主线程。
// 「后台继续」= **最小化窗口**：窗口留在程序坞 / 任务栏里，找得回来；再次打开应用也会把它唤起（单实例）。
// 用「隐藏」的话 Windows / Linux 上没有任何入口能把它找回来，用户只能去任务管理器里杀进程。

// CloseChoice 是关窗口时用户的选择。
type CloseChoice int

const (
	CloseCancel         CloseChoice = iota // 不关
	CloseMinimize                          // 最小化，任务继续
	CloseMinimizeAlways                    // 最小化，并且以后不再询问
	CloseQuit                              // 仍然退出
)

// CloseGuard 决定一次关窗口请求要不要拦下。各个依赖注入进来，规则可单测。
type CloseGuard struct {
	// Active 返回还没结束的工作数；出错时（例如数据库没起来）按 0 处理 —— 那时也没有任务能跑。
	Active func(ctx context.Context) (int, error)
	// AlwaysMinimize / SetAlwaysMinimize 读写「不再询问，总是最小化」。
	AlwaysMinimize    func() bool
	SetAlwaysMinimize func(ctx context.Context, on bool)
	// Ask 弹原生对话框。
	Ask func(ctx context.Context, active int) CloseChoice
	// Minimize 最小化窗口。
	Minimize func(ctx context.Context)

	quitting atomic.Bool
}

// ForceQuit 标记「这次退出不要再问」（系统关机、收到 SIGTERM、第二次确认后）。
func (g *CloseGuard) ForceQuit() { g.quitting.Store(true) }

// BeforeClose 是 Wails 的 OnBeforeClose：返回 true 表示拦下这次关闭。
func (g *CloseGuard) BeforeClose(ctx context.Context) bool {
	if g.quitting.Load() || g.Active == nil {
		return false
	}
	n, err := g.Active(ctx)
	if err != nil || n == 0 {
		return false
	}
	if g.AlwaysMinimize != nil && g.AlwaysMinimize() {
		g.minimize(ctx)
		return true
	}
	choice := CloseQuit
	if g.Ask != nil {
		choice = g.Ask(ctx, n)
	}
	switch choice {
	case CloseMinimizeAlways:
		if g.SetAlwaysMinimize != nil {
			g.SetAlwaysMinimize(ctx, true)
		}
		g.minimize(ctx)
		return true
	case CloseMinimize:
		g.minimize(ctx)
		return true
	case CloseCancel:
		return true
	default:
		g.quitting.Store(true)
		return false
	}
}

func (g *CloseGuard) minimize(ctx context.Context) {
	if g.Minimize != nil {
		g.Minimize(ctx)
	}
}
