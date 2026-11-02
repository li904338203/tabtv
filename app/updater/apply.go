package updater

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// Mode 是本机能怎么更新。
type Mode string

const (
	// ModeAuto：下载、校验、替换、重启都由应用完成（Windows 安装版、Linux AppImage）。
	ModeAuto Mode = "auto"
	// ModeManual：只提示并打开下载页（macOS 未签名应用不能自己替换自己；Linux .deb 没有权限改系统目录）。
	ModeManual Mode = "manual"
)

// Target 描述本机的更新方式。
type Target struct {
	Mode Mode
	// Dir 是下载目录；AppImage 下载到它自己所在的目录，保证最后的 rename 是同一文件系统内的原子替换。
	Dir string
	// AppImage 是正在运行的 AppImage 路径（只 Linux）。
	AppImage string
}

// Detect 判断本机的更新方式。
func Detect(goos string, getenv func(string) string) Target {
	switch goos {
	case "windows":
		return Target{Mode: ModeAuto, Dir: filepath.Join(os.TempDir(), "tabtv-update")}
	case "linux":
		if p := getenv("APPIMAGE"); p != "" {
			return Target{Mode: ModeAuto, Dir: filepath.Dir(p), AppImage: p}
		}
	}
	return Target{Mode: ModeManual}
}

// Current 是本机的 Detect。
func Current() Target { return Detect(runtime.GOOS, os.Getenv) }

// Apply 安装已下载并校验过的更新包，返回之后由调用方走正常退出路径（TT-T-30：停后台、停库）。
//
//   - Windows：启动 NSIS 安装器（/S 静默）。安装器在本进程退出后覆盖程序并启动新版本（发布流水线负责安装脚本的等待与重启，dev/04 R-7）。
//   - Linux AppImage：chmod +x → 原子 rename 覆盖正在运行的文件（Linux 上替换运行中的文件是安全的，旧进程继续用旧 inode）
//     → 延迟 2 秒再拉起新版本：立刻拉起的话新进程撞上旧进程还持有的单实例锁，只会唤起旧窗口然后自己退出。
func (t Target) Apply(pkg string) error {
	switch {
	case t.Mode != ModeAuto:
		return fmt.Errorf("本机不支持自动安装，请从下载页手动更新")
	case runtime.GOOS == "windows":
		return exec.Command(pkg, "/S").Start()
	case t.AppImage != "":
		if err := os.Chmod(pkg, 0o755); err != nil {
			return err
		}
		if err := os.Rename(pkg, t.AppImage); err != nil {
			return fmt.Errorf("替换程序失败：%w", err)
		}
		cmd := exec.Command("/bin/sh", "-c", `sleep 2; exec "$0"`, t.AppImage)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
		return cmd.Start()
	}
	return fmt.Errorf("本机不支持自动安装")
}
