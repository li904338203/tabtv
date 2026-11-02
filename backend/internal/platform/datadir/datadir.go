// Package datadir 解析 tabtv 的数据目录（dev/02 TTD-21）。
//
// 布局：
//
//	<数据目录>/pg/        PostgreSQL 集群
//	<数据目录>/media/     本地对象存储
//	<数据目录>/logs/      应用与数据库日志
//	<数据目录>/backups/   迁移前的数据库备份
//	<数据目录>/tabtv.json 数据目录内唯一的启动配置（端口、密码、集群版本）
//
// 数据目录可以被迁移到别处（设置页，TT-F-33）：默认位置下放一个 `location` 指针文件，
// 内容是新目录的绝对路径。指针指向的目录不存在时**报错而不是回落到默认位置**——
// 回落的话用户会看到一个空的应用，以为数据全丢了，而它们其实只是在一块没插上的移动硬盘上。
package datadir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// AppName 是数据目录名。
const AppName = "tabtv"

// Dirs 是解析后的数据目录各子路径。
type Dirs struct {
	Root    string
	PG      string
	Media   string
	Logs    string
	Backups string
	Config  string // tabtv.json
}

// Of 由根目录得出各子路径。
func Of(root string) Dirs {
	return Dirs{
		Root:    root,
		PG:      filepath.Join(root, "pg"),
		Media:   filepath.Join(root, "media"),
		Logs:    filepath.Join(root, "logs"),
		Backups: filepath.Join(root, "backups"),
		Config:  filepath.Join(root, "tabtv.json"),
	}
}

// DefaultRoot 返回系统惯例位置：
// macOS ~/Library/Application Support/tabtv；Windows %APPDATA%\tabtv；Linux $XDG_DATA_HOME/tabtv（缺省 ~/.local/share/tabtv）。
func DefaultRoot() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", AppName), nil
	case "windows":
		if v := os.Getenv("APPDATA"); v != "" {
			return filepath.Join(v, AppName), nil
		}
		dir, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, AppName), nil
	default:
		if v := os.Getenv("XDG_DATA_HOME"); v != "" {
			return filepath.Join(v, AppName), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".local", "share", AppName), nil
	}
}

// PointerFile 返回默认位置下的指针文件路径。
func PointerFile(defaultRoot string) string { return filepath.Join(defaultRoot, "location") }

// Resolve 解析当前数据目录：显式指定（TABTV_DATA_DIR）> 指针文件 > 默认位置。
// 返回的目录与子目录都已创建。
func Resolve() (Dirs, error) {
	if v := os.Getenv("TABTV_DATA_DIR"); v != "" {
		return ensure(v)
	}
	def, err := DefaultRoot()
	if err != nil {
		return Dirs{}, fmt.Errorf("取默认数据目录失败：%w", err)
	}
	root, err := follow(def)
	if err != nil {
		return Dirs{Root: def}, err
	}
	return ensure(root)
}

func follow(def string) (string, error) {
	b, err := os.ReadFile(PointerFile(def))
	if errors.Is(err, os.ErrNotExist) {
		return def, nil
	}
	if err != nil {
		return "", fmt.Errorf("读取数据目录指针失败：%w", err)
	}
	target := strings.TrimSpace(string(b))
	if target == "" {
		return def, nil
	}
	if st, err := os.Stat(target); err != nil || !st.IsDir() {
		return "", fmt.Errorf("数据目录 %s 不存在或不可访问（由 %s 指向）。如果它在外接磁盘上，请先接上磁盘", target, PointerFile(def))
	}
	return target, nil
}

func ensure(root string) (Dirs, error) {
	d := Of(root)
	for _, p := range []string{d.Root, d.PG, d.Media, d.Logs, d.Backups} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			return d, fmt.Errorf("创建数据目录 %s 失败：%w", p, err)
		}
	}
	return d, nil
}

// SetPointer 把数据目录指向 target（迁移数据目录时调用，TT-T-22）。
// target 等于默认位置时删除指针。
func SetPointer(target string) error {
	def, err := DefaultRoot()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(def, 0o755); err != nil {
		return err
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	if filepath.Clean(abs) == filepath.Clean(def) {
		err := os.Remove(PointerFile(def))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	tmp := PointerFile(def) + ".tmp"
	if err := os.WriteFile(tmp, []byte(abs+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, PointerFile(def))
}
