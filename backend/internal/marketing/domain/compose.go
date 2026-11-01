package domain

import (
	"sort"
	"strings"
)

// Compose 拼营销段：**创意 → Hook → 风格 → 场景约束段**（MKT-F-44、dev/01 §4.1）。
//
//   - 顺序按类型排，**不按入参顺序**：调用方从会话钉版本、从 params 解析出来的顺序
//     都不可靠，顺序错了模型会先照着风格词画（同图片预设的拼接原则，CD-45）。
//     同类型多条时保持入参相对顺序（稳定排序）。
//   - 段间 `\n`；空段（片段被清空）跳过，**不留空行**；每段去掉两端空白。
//   - constraint 是场景约束段（商品保真等，MKT-F-47）：只要调用方决定调用 Compose
//     （也就是 `params.marketing` 存在），它就**总是追加**，不随选了几样而变 ——
//     它不是选项的附属。全空返回空串。
//
// 「画面描述在最前」由生成域保证（`generation/domain.OrderAppendix`），不在这里。
func Compose(snaps []Snapshot, constraint string) string {
	sorted := make([]Snapshot, len(snaps))
	copy(sorted, snaps)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Kind.order() < sorted[j].Kind.order() })

	parts := make([]string, 0, len(sorted)+1)
	for _, s := range sorted {
		if p := strings.TrimSpace(s.Prompt); p != "" {
			parts = append(parts, p)
		}
	}
	if c := strings.TrimSpace(constraint); c != "" {
		parts = append(parts, c)
	}
	return strings.Join(parts, "\n")
}
