// Package updater 检查、下载并应用新版本（TT-T-31，dev/04 TTD-56、R-7）。
//
// 只访问一个地址：GitHub Releases API（TT-N-02 允许的两个出网目标之一），请求里只带 User-Agent 版本号，
// 不带任何使用数据，也不在应用里存放任何 token。
package updater

import (
	"strconv"
	"strings"
)

// Version 是解析后的语义化版本。
type Version struct {
	Major, Minor, Patch int
	Pre                 string // 预发布标识（-rc1 之类）；空 = 正式版
}

// ParseVersion 解析 `v1.2.3` / `1.2.3-rc1`；`dev`、带 `-dirty` 的本地构建与无法解析的一律返回 false ——
// 它们不参与比较，否则一个开发构建会被提示「升级」到某个正式版而覆盖掉开发者手上的程序。
func ParseVersion(s string) (Version, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if s == "" || s == "dev" || strings.Contains(s, "dirty") {
		return Version{}, false
	}
	if i := strings.IndexByte(s, '+'); i >= 0 { // 构建元数据不参与比较
		s = s[:i]
	}
	var v Version
	core := s
	if i := strings.IndexByte(s, '-'); i >= 0 {
		core, v.Pre = s[:i], s[i+1:]
		if v.Pre == "" {
			return Version{}, false
		}
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return Version{}, false
	}
	nums := [3]int{}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return Version{}, false
		}
		nums[i] = n
	}
	v.Major, v.Minor, v.Patch = nums[0], nums[1], nums[2]
	return v, true
}

// Compare 返回 -1 / 0 / 1。同一核心版本下，预发布版低于正式版（0.2.0-rc1 < 0.2.0）；
// 预发布标识按点分段比较：数字段按数值、其余按字典序（rc2 < rc10 也成立：字母前缀相同则比尾随数字）。
func Compare(a, b Version) int {
	for _, d := range [][2]int{{a.Major, b.Major}, {a.Minor, b.Minor}, {a.Patch, b.Patch}} {
		if d[0] != d[1] {
			return sign(d[0] - d[1])
		}
	}
	switch {
	case a.Pre == b.Pre:
		return 0
	case a.Pre == "":
		return 1
	case b.Pre == "":
		return -1
	}
	as, bs := strings.Split(a.Pre, "."), strings.Split(b.Pre, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if c := comparePreIdent(as[i], bs[i]); c != 0 {
			return c
		}
	}
	return sign(len(as) - len(bs))
}

func comparePreIdent(a, b string) int {
	an, aerr := strconv.Atoi(a)
	bn, berr := strconv.Atoi(b)
	if aerr == nil && berr == nil {
		return sign(an - bn)
	}
	// rc2 vs rc10：拆成「字母前缀 + 数字尾巴」比，免得按字典序得出 rc10 < rc2。
	ap, at := splitTail(a)
	bp, bt := splitTail(b)
	if ap == bp && at >= 0 && bt >= 0 {
		return sign(at - bt)
	}
	return strings.Compare(a, b)
}

func splitTail(s string) (string, int) {
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	if i == len(s) {
		return s, -1
	}
	n, _ := strconv.Atoi(s[i:])
	return s[:i], n
}

func sign(n int) int {
	switch {
	case n > 0:
		return 1
	case n < 0:
		return -1
	}
	return 0
}

// Newer 报告 latest 是否比 current 新；任一方解析不了都返回 false。
func Newer(current, latest string) bool {
	c, ok1 := ParseVersion(current)
	l, ok2 := ParseVersion(latest)
	return ok1 && ok2 && Compare(l, c) > 0
}
