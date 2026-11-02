package protocol

import (
	"strings"

	"github.com/li904338203/tabtv/backend/internal/platform/pgtext"
)

// bodySnippet 取响应原文的前 500 个字符，写进错误信息。
//
// ⚠️ 上游可以回任意字节（被网关拦截的二进制错误页）：PG 的 text 列拒收 0x00，
// 不清洗的话失败信息落库整条回滚，执行器判可重试、一直重试到撞死线。
func bodySnippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	if s == "" {
		return "（空响应体）"
	}
	return pgtext.Fit(s, 500)
}
