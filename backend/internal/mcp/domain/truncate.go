package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// 截断时写进结果顶层的两个字段（MCP-I-07）。注册表会把它们加进每个工具的 outputSchema
// （可选属性），否则截断后的结果过不了 outputSchema 校验 —— 那是协议错误，客户端什么都拿不到。
const (
	FieldTruncated      = "truncated"
	FieldTruncationHint = "truncationHint"
)

// CharsPerToken 是「字符数 ÷ 系数 ≈ token 数」的估算系数。
//
// 这是**近似**：英文 JSON 约 4 字符/token，中文约 1~1.5 字符/token，结果里两者混排，取 3。
// 估得偏小（实际 token 更多）的代价是模型上下文被多占一些；没有精确分词器可用，
// 而为截断引入某一家模型的分词器会把结果绑在那家模型上。
const CharsPerToken = 3

// hintReserve 给截断说明本身预留的字符数（说明要写进结果，也占预算）。
const hintReserve = 400

// EstimateTokens 估算一段 JSON 的 token 数（按 Unicode 码点，不按字节）。
func EstimateTokens(b []byte) int {
	n := utf8.RuneCount(b)
	return (n + CharsPerToken - 1) / CharsPerToken
}

// Truncate 把 v 序列化成 JSON；估算超过 maxTokens 时**从数组尾部丢元素**，直到放得下。
//
//   - 结果始终是合法 JSON（在解析后的结构上删元素再序列化，不在字节上切）。
//   - 截断时在顶层对象写入 `truncated:true` 与 `truncationHint`（哪个数组保留了几项、
//     怎么精确读取）。只说"已截断"的话，模型不知道该翻页还是该换参数，会原样再调一次。
//   - 每轮挑**序列化后最大**的那个数组收缩（二分出能放下的最多项数）；收到 0 仍放不下
//     就换下一个。没有数组可删时原样返回、不标 truncated —— 那是工具的 DTO 设计问题，
//     不能靠截断层切字符串掩盖（切出来的半个 ID 比没有更糟）。
//   - maxTokens <= 0 视为不限。
//
// 序列化失败返回 (nil, false)，调用方按内部错误处理。
func Truncate(v any, maxTokens int) (json.RawMessage, bool) {
	raw, err := marshal(v)
	if err != nil {
		return nil, false
	}
	if maxTokens <= 0 || EstimateTokens(raw) <= maxTokens {
		return raw, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // 大整数不经 float64，避免精度丢失
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return raw, false
	}
	budget := maxTokens*CharsPerToken - hintReserve
	if budget < 0 {
		budget = 0
	}
	fits := func() bool {
		b, err := marshal(doc)
		return err == nil && utf8.RuneCount(b) <= budget
	}

	notes := map[string]*cut{}
	var order []string
	for !fits() {
		var arrs []arrayRef
		if top, ok := doc.([]any); ok { // 顶层就是数组（注册表要求出参是对象，这里只是兜底）
			arrs = append(arrs, arrayRef{path: "$",
				get: func() []any { v, _ := doc.([]any); return v },
				set: func(v []any) { doc = v }})
			for i, el := range top {
				arrs = append(arrs, collectArrays(el, fmt.Sprintf("$[%d]", i))...)
			}
		} else {
			arrs = collectArrays(doc, "")
		}
		var best *arrayRef
		bestSize := -1
		for i := range arrs {
			a := &arrs[i]
			if len(a.get()) == 0 {
				continue
			}
			b, _ := marshal(a.get())
			if s := utf8.RuneCount(b); s > bestSize {
				best, bestSize = a, s
			}
		}
		if best == nil {
			break // 没有可删的数组了
		}
		full := best.get()
		n := cut{path: best.path, total: len(full)}
		if prev, ok := notes[best.path]; ok {
			n.total = prev.total
		}
		// 二分：保留前 k 项时能放下的最大 k。
		lo, hi := 0, len(full)-1
		k := 0
		for lo <= hi {
			mid := (lo + hi) / 2
			best.set(full[:mid])
			if fits() {
				k, lo = mid, mid+1
			} else {
				hi = mid - 1
			}
		}
		best.set(full[:k])
		n.kept = k
		if _, ok := notes[best.path]; !ok {
			order = append(order, best.path)
		}
		notes[best.path] = &n
	}
	if len(notes) == 0 {
		return raw, false
	}

	cuts := make([]cut, 0, len(order))
	for _, p := range order {
		cuts = append(cuts, *notes[p])
	}
	sort.SliceStable(cuts, func(i, j int) bool { return cuts[i].path < cuts[j].path })
	if obj, ok := doc.(map[string]any); ok {
		obj[FieldTruncated] = true
		obj[FieldTruncationHint] = hint(cuts)
	}
	out, err := marshal(doc)
	if err != nil {
		return raw, false
	}
	return out, true
}

type cut struct {
	path        string
	kept, total int
}

func hint(cuts []cut) string {
	parts := make([]string, 0, len(cuts))
	for _, c := range cuts {
		parts = append(parts, fmt.Sprintf("%s 只保留前 %d/%d 项", c.path, c.kept, c.total))
	}
	return "结果过长已截断：" + strings.Join(parts, "；") +
		"。请缩小范围精确读取（如传 cursor 翻页、用 nodeIds / ID 只取需要的项），不要原样重试。"
}

type arrayRef struct {
	path string
	get  func() []any
	set  func([]any)
}

// collectArrays 收集 node 内部所有数组（含嵌套）及其改写入口，路径形如 `nodes`、`nodes[2].items`。
// node 本身若是数组不在此登记（顶层数组由调用方登记，路径 `$`）。
func collectArrays(node any, path string) []arrayRef {
	var out []arrayRef
	visit := func(p string, v any, get func() []any, set func([]any)) {
		if arr, ok := v.([]any); ok {
			out = append(out, arrayRef{path: p, get: get, set: set})
			for i, el := range arr {
				out = append(out, collectArrays(el, fmt.Sprintf("%s[%d]", p, i))...)
			}
			return
		}
		out = append(out, collectArrays(v, p)...)
	}
	switch t := node.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys) // 遍历顺序固定 → 同一输入截断结果稳定
		for _, k := range keys {
			p := k
			if path != "" {
				p = path + "." + k
			}
			key := k
			visit(p, t[k], func() []any { v, _ := t[key].([]any); return v },
				func(v []any) { t[key] = v })
		}
	case []any:
		for i := range t {
			idx := i
			visit(fmt.Sprintf("%s[%d]", path, i), t[i], func() []any { v, _ := t[idx].([]any); return v },
				func(v []any) { t[idx] = v })
		}
	}
	return out
}

// marshal 与 json.Marshal 相同，但**不转义 HTML 字符**（`<`、`>`、`&` 原样输出）：
// 转义成 \u003c 会让字符数虚高，也让模型读到一串转义码。
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
