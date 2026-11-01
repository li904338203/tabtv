// Package buildinfo 暴露构建期注入的版本信息。
//
// 三个二进制共用，由 -ldflags 注入。健康检查会回显这些值：
// 排障时第一个要确认的就是"线上跑的到底是哪个版本"。
package buildinfo

import "runtime"

// 由 -X 注入，默认值用于 go run 等未注入的场景。
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"buildTime"`
	GoVersion string `json:"goVersion"`
}

func Get() Info {
	return Info{
		Version:   Version,
		Commit:    Commit,
		BuildTime: BuildTime,
		GoVersion: runtime.Version(),
	}
}
