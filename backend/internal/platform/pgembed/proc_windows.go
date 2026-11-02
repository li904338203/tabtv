//go:build windows

package pgembed

import (
	"os/exec"
	"syscall"
)

// hideWindow 不让 initdb / pg_ctl 在 Windows 上弹出控制台窗口。
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}
