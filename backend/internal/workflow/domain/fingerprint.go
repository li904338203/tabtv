package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// 复用指纹（NEW-05）。
//
// ⚠️ **只能有这一个实现**。计划器算"现在的设置"的指纹，生成域算"上次产出用的"
// 指纹，两边必须逐字节一致。各写一份的结果是它们**永远对不上** ——
// 于是每次运行都把所有节点重跑一遍，用户每跑一次图就多花一次钱，
// 而系统没有任何报错，日志里也看不出异常。
//
// 这就是为什么生成域的适配器要调用这个函数，而不是"照着实现一遍"。

// Fingerprint 按「操作 + 模型 + 参数 + 有序输入」算指纹。
//
// ⚠️ `params` 要**规范化**再算：JSON 对象的键顺序不稳定，
// 前端两次发来的同一份参数可能是 `{"a":1,"b":2}` 与 `{"b":2,"a":1}` ——
// 直接对原始字节做哈希，同一份设置会算出两个指纹，复用永远不成立。
//
// ⚠️ 输入**按 order 排序**后参与：顺序即首尾帧与参考图的锚定顺序，
// 换了顺序就是另一次生成，不能复用上一次的图。
func Fingerprint(operation, modelCode string, params json.RawMessage,
	inputs []InputRef) string {
	var b strings.Builder
	b.WriteString("op=")
	b.WriteString(operation)
	b.WriteString("\nmodel=")
	b.WriteString(modelCode)
	b.WriteString("\nparams=")
	b.WriteString(canonicalJSON(params))
	b.WriteString("\ninputs=")

	sorted := append([]InputRef(nil), inputs...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Order != sorted[j].Order {
			return sorted[i].Order < sorted[j].Order
		}
		return sorted[i].Ref < sorted[j].Ref
	})
	for _, in := range sorted {
		b.WriteString(strconv.Itoa(in.Order))
		b.WriteByte(':')
		b.WriteString(in.Kind)
		b.WriteByte(':')
		b.WriteString(in.Ref)
		b.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}

// InputRef 是一条参与指纹的输入。
type InputRef struct {
	Order int
	Kind  string
	// Ref 是这条输入指向的东西：上游节点 ID、参考文件 ID、素材码或文本本身。
	Ref string
}

// canonicalJSON 把 JSON 规范化成键有序的紧凑形式。
//
// ⚠️ 解不开时**返回原文而不是空串**：返回空串会让两份不同的坏参数
// 算出同一个指纹，于是复用一张完全不相干的图。
func canonicalJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	out, err := json.Marshal(sortKeys(v))
	if err != nil {
		return string(raw)
	}
	return string(out)
}

// sortKeys 递归地把 map 换成有序的键值对序列。
//
// Go 的 encoding/json 本来就按键排序输出 map，但**嵌套在 any 里的
// 数字会变成 float64**，`1` 与 `1.0` 因此都序列化成 `1` —— 这正是我们要的
// （前端发 1 还是 1.0 不该算两种设置）。这里只需要保证结构递归走到底。
func sortKeys(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = sortKeys(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = sortKeys(val)
		}
		return out
	}
	return v
}
