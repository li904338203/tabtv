//go:build !probe

package app

import "net/http"

// WithProbe 在非探针构建里原样返回（探针见 probe.go，只在 -tags probe 下存在）。
func WithProbe(api http.Handler) http.Handler { return api }
