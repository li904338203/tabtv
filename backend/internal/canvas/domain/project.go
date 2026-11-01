// Package domain 是项目/画布域的纯规则层：命名校验、复制策略、回收站保留期。
//
// 这一层不碰数据库、不碰 HTTP —— 复制策略是本次最容易出错的一块，
// 必须能用表驱动测试逐条钉住（08 §10）。
package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrNameEmpty      = errors.New("名称不能为空")
	ErrNameTooLong    = errors.New("名称过长")
	ErrFolderLimit    = errors.New("文件夹数量已达上限")
	ErrNotInTrash     = errors.New("项目不在回收站中")
	ErrAlreadyDeleted = errors.New("项目已在回收站中")
)

const (
	MaxProjectNameLen = 100
	MaxFolderNameLen  = 64
	// MaxFolders 单层上限。文件夹刻意不做树：
	// 单层免掉 parent_id 的防循环校验，而上限防止「文件夹当标签用」把选择器撑爆。
	MaxFolders = 100
)

// ValidateProjectName 校验项目名。
//
// ⚠️ 按**字符数**不按字节数：`len()` 下一个中文占 3 字节，
// 100 字节只能存 33 个中文字，中文用户会莫名其妙被拒。
func ValidateProjectName(s string) (string, error) {
	return validateName(s, MaxProjectNameLen)
}

func ValidateFolderName(s string) (string, error) {
	return validateName(s, MaxFolderNameLen)
}

func validateName(s string, max int) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ErrNameEmpty
	}
	if n := utf8.RuneCountInString(s); n > max {
		return "", fmt.Errorf("%w：%d 字，上限 %d 字", ErrNameTooLong, n, max)
	}
	return s, nil
}

// CopyName 生成副本名，并保证不超长。
//
// ⚠️ 后缀是**加在截断之后**的：先拼后缀再截断会把「副本」两个字截掉，
// 于是复制出来的项目和原项目同名，列表里根本分不出哪个是哪个。
func CopyName(origin string) string {
	const suffix = " 副本"
	room := MaxProjectNameLen - utf8.RuneCountInString(suffix)
	r := []rune(origin)
	if len(r) > room {
		r = r[:room]
	}
	return string(r) + suffix
}

// ---------------------------------------------------------------- 复制策略

// CopyStats 记录一次复制改写了什么，用于日志与测试断言。
type CopyStats struct {
	Nodes            int
	Edges            int
	UnboundResults   int // 解绑的产物快照数
	PreservedPreview int // 解绑后仍保留预览地址的数量
}

// CopyCanvasData 生成副本的画布数据。
//
// 两条规则，缺一不可：
//
//  1. **未知字段原样保留**（GO-35）。画布 JSON 会随前端演进长出新字段，
//     后端按 struct 反序列化再序列化会把没定义的键悄悄吃掉 —— 复制一次丢一批。
//     因此这里走 `map[string]any` 的通用树，只改该改的那几个键。
//
//  2. **节点产物必须解绑**（GAP-16）。`nodes[].result.id` 是**原项目**的
//     generationId。输入解析要求「同项目」，所以副本里的它能预览却不能作新任务输入 ——
//     留着它等于埋一个「用户点了生成才报错」的坑，而错误信息还指不到原因。
//     改写成 `sourceGenerationId`：预览字段（outputUrl/outputType）全部保留，
//     来源可追溯，同时任何把它当输入的代码路径都会明确地找不到 id。
//
// ⚠️ 不复制 generation 行与账本流水（CV-03）：副本不该凭空拥有一份消费记录。
func CopyCanvasData(raw json.RawMessage, rebind OutputRebinder) (json.RawMessage, CopyStats, error) {
	var stats CopyStats
	if len(raw) == 0 {
		return json.RawMessage(`{}`), stats, nil
	}
	var tree map[string]any
	if err := json.Unmarshal(raw, &tree); err != nil {
		// 画布数据存不进去就不该能存进来；真出现了要让复制失败而不是产出半个副本。
		return nil, stats, fmt.Errorf("画布数据不是合法 JSON 对象: %w", err)
	}

	nodes, _ := tree["nodes"].([]any)
	stats.Nodes = len(nodes)
	for _, n := range nodes {
		node, ok := n.(map[string]any)
		if !ok {
			continue
		}
		result, ok := node["result"].(map[string]any)
		if !ok {
			continue
		}
		id, ok := result["id"]
		if !ok || id == nil {
			continue
		}
		delete(result, "id")
		result["sourceGenerationId"] = id
		stats.UnboundResults++
		if s, _ := result["outputUrl"].(string); s != "" {
			stats.PreservedPreview++
		}
		// 留给 P05：把原产物映射成副本可用的媒体引用。
		// 没有实现时就只是解绑 —— 宁可让用户重新生成，也不给一个点了才失败的按钮。
		if rebind != nil {
			if mediaID, ok := rebind.Rebind(id); ok {
				result["mediaId"] = mediaID
			}
		}
	}
	if edges, ok := tree["edges"].([]any); ok {
		stats.Edges = len(edges)
	}

	out, err := json.Marshal(tree)
	if err != nil {
		return nil, stats, err
	}
	return out, stats, nil
}

// OutputRebinder 把原项目的 generationId 映射成副本可直接使用的媒体 ID。
//
// 这是留给生成域（P05）的接缝：产物的归属主体是**工作区级的 media_object**，
// 而不是项目级的 generation，因此重绑定在架构上是成立的 ——
// 只是它要读 generation_output，那是生成域的表，本域不能直接查（08 §1 依赖方向）。
type OutputRebinder interface {
	Rebind(generationID any) (string, bool)
}

// ---------------------------------------------------------------- 回收站

// RetentionDeadline 返回该项目被彻底清理的时刻。
//
// ⚠️ 一律从 `deletedAt` 起算，**不从 `updatedAt`** —— 恢复、改名、移动文件夹
// 都会刷 updatedAt，用它算的话「删了又恢复又删」的项目每次重获整个保留期，永远清不掉。
func RetentionDeadline(deletedAt time.Time, retention time.Duration) time.Time {
	return deletedAt.Add(retention)
}

// DaysLeft 返回回收站剩余天数，供前端显示「还剩 N 天」。
//
// ⚠️ **向上取整**。向下取整会让刚删除的项目显示「剩余 29 天」——
// 保留期明明是 30 天，用户看到的第一眼就少了一天，因为 now 总是比 deletedAt
// 晚那么几毫秒。同理不足一天显示 1 天：显示 0 天会让人以为已经没救了，其实还能恢复。
func DaysLeft(deletedAt, now time.Time, retention time.Duration) int {
	left := RetentionDeadline(deletedAt, retention).Sub(now)
	if left <= 0 {
		return 0
	}
	const day = 24 * time.Hour
	return int((left + day - 1) / day)
}

// ---------------------------------------------------------------- 套用模板

// CanvasIsEmpty 判断画布上是不是**一个节点、一条连线都没有**。
//
// 套用模板只允许空画布（老系统同一条规则）：往一张画了东西的画布上套模板，
// 要么覆盖掉用户的节点（不可逆），要么把两份节点叠在一起（ID 可能撞、连线乱）——
// 两种都不是用户点「角色三视图」时想要的。
//
// ⚠️ 解析不了的画布**不算空**：宁可拒绝套用，也不要把一份读不懂的内容当成空白覆盖掉。
func CanvasIsEmpty(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var tree struct {
		Nodes json.RawMessage `json:"nodes"`
		Edges json.RawMessage `json:"edges"`
	}
	if err := json.Unmarshal(raw, &tree); err != nil {
		return false
	}
	return emptyArray(tree.Nodes) && emptyArray(tree.Edges)
}

func emptyArray(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return true
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return false // 不是数组：读不懂，按「不空」处理
	}
	return len(items) == 0
}
