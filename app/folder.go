package app

import (
	"fmt"
	"os/exec"
	"runtime"
)

// OpenDataDir 在系统文件管理器里打开数据目录（设置 → 常规、启动错误页）。
func (a *App) OpenDataDir() error { return openFolder(a.dataDir) }

// OpenLogDir 在系统文件管理器里打开日志目录（TT-T-12，错误页与环境自检用）。
func (a *App) OpenLogDir() error { return openFolder(a.logDir) }

func openFolder(path string) error {
	if path == "" {
		return fmt.Errorf("目录未确定")
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		cmd = exec.Command("explorer", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	return cmd.Start()
}
