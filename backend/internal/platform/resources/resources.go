// Package resources 定位随包附带的资源（PG 二进制、ffmpeg、字体）。
//
// 一处定位、三处使用：各自去找的话，打包后的目录结构一改，
// 总有一处找错地方，而症状分别是「数据库起不来」「渲染失败」「字幕没字」—— 看起来毫不相干。
//
// 查找顺序：
//  1. 环境变量 TABTV_RESOURCES（开发与测试时显式指定）；
//  2. 可执行文件旁：<exe 目录>/resources，macOS 的 .app 里是 Contents/Resources；
//  3. 开发态：从当前目录往上找仓库里的 build/third_party（由 build/fetch-third-party.sh 下载）。
package resources

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

// ErrNotFound 表示找不到某项资源。
var ErrNotFound = errors.New("找不到随包资源")

// Platform 返回资源目录里用的平台名（GOOS-GOARCH）。
func Platform() string { return runtime.GOOS + "-" + runtime.GOARCH }

// Root 返回打包后的资源根目录（含 pg/、bin/、fonts/）；开发态返回空串。
func Root() string {
	if v := os.Getenv("TABTV_RESOURCES"); v != "" {
		return v
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	exe, _ = filepath.EvalSymlinks(exe)
	dir := filepath.Dir(exe)
	for _, c := range []string{
		filepath.Join(dir, "resources"),
		filepath.Join(dir, "..", "Resources"), // macOS: tabtv.app/Contents/MacOS/tabtv → Contents/Resources
	} {
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c
		}
	}
	return ""
}

// devThirdParty 从当前目录往上找仓库里的 build/third_party/<name>/<平台>。
func devThirdParty(name string) string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	for dir := wd; ; {
		c := filepath.Join(dir, "build", "third_party", name, Platform())
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// ThirdParty 返回开发态下载的第三方组件目录（build/third_party/<name>/<平台>）；找不到返回空串。
func ThirdParty(name string) string { return devThirdParty(name) }

// Postgres 返回 PG 二进制根目录（其下有 bin/、lib/、share/）。
func Postgres() (string, error) {
	if v := os.Getenv("TABTV_PG_HOME"); v != "" {
		return v, nil
	}
	if root := Root(); root != "" {
		if c := filepath.Join(root, "pg"); exists(filepath.Join(c, "bin")) {
			return c, nil
		}
	}
	if c := devThirdParty("postgres"); c != "" {
		return c, nil
	}
	return "", errors.Join(ErrNotFound, errors.New("PostgreSQL 二进制（开发时先运行 build/fetch-third-party.sh postgres）"))
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
