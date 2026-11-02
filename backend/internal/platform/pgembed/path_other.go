//go:build !windows

package pgembed

// shortPath 只在 Windows 上有意义（见 forPG）；其余平台原样返回。
func shortPath(p string) (string, error) { return p, nil }
