package domain

import "math"

// Ratio 是统计里的比率（错误率、确认通过率）：分母为 0 时回 **-1**（「还没有数据」），不是 0 ——
// 0% 是一个真实的结论（「一次都没错过」），拿它表示没有数据会让一个从没人用过的客户端
// 看起来比一个偶尔出错的客户端更健康（同 observability 成功率的口径，CD-64）。
// 结果保留四位小数（前端展示到 0.01%）。
func Ratio(num, den int64) float64 {
	if den <= 0 {
		return -1
	}
	return math.Round(float64(num)/float64(den)*10000) / 10000
}

// 客户端分布里一项的种类。
const (
	ClientKindNamed   = "client"
	ClientKindUnknown = "unknown" // 调用记录里没有客户端名
	ClientKindOther   = "other"   // 排名之外的合并项
)

// ClientShare 是客户端分布的一项。
type ClientShare struct {
	Name   string
	Kind   string
	Calls  int64
	Errors int64
}

// TopClients 把按调用数倒序的分组截成前 top 项，其余并进一个「其他」。
//
// ⚠️ **各项之和恒等于输入之和**：直接截断的话，各客户端之和对不上总调用数，而那个差额
// 没有任何人解释得了。未知客户端（空名）**单列不丢**，且不占排名名额（它不是一个客户端，
// 被挤进「其他」的话「有多少调用说不清来源」这个信号就没了）。
func TopClients(in []ClientShare, top int) []ClientShare {
	out := []ClientShare{}
	var unknown, other *ClientShare
	named := 0
	for _, c := range in {
		switch {
		case c.Name == "":
			if unknown == nil {
				unknown = &ClientShare{Kind: ClientKindUnknown}
			}
			unknown.Calls += c.Calls
			unknown.Errors += c.Errors
		case named < top:
			c.Kind = ClientKindNamed
			out = append(out, c)
			named++
		default:
			if other == nil {
				other = &ClientShare{Kind: ClientKindOther}
			}
			other.Calls += c.Calls
			other.Errors += c.Errors
		}
	}
	if other != nil {
		out = append(out, *other)
	}
	if unknown != nil {
		out = append(out, *unknown)
	}
	return out
}
