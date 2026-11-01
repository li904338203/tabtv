package domain

import (
	"encoding/json"
	"fmt"
	"sort"
)

// OutlineNode 是画布大纲里的一个节点（MCPT-04 canvas_read，开发 01 §12 R10）。
//
// ⚠️ ID / Type / Title / Status / ByAgent 五个字段的取法必须与 Agent 的
// `agent/app.SummarizeCanvas` **逐字一致**（对照测试在 wiring 包里钉住）：两个入口各解析一遍
// canvasData，取法一旦分叉，Agent 与外部客户端看到的「节点状态」就是两个答案，而两边都"没错"。
// 这里多出来的只有 Upstream 与 Params —— 前者是 MCP 大纲要的，后者只在精确读取时下发。
type OutlineNode struct {
	ID    string
	Type  string
	Title string
	// Status 取节点上的 result.status（UI 快照，不是 generation 的事实表）。
	Status  string
	ByAgent bool
	// Upstream 是入边的源节点 ID，**按 edge.order 升序**（order 相同按源 ID）。
	// ⚠️ 顺序有业务含义：首尾帧的首/尾、参考图的「图1/图2」都按它定（GO-35）。
	Upstream []string
	// Params 是节点 params 的原文（可能为空）。
	Params json.RawMessage
}

// Outline 解析画布大纲与连线条数。解析失败返回空：大纲只是读视图，一个坏字段不该让整张画布读不出来
// （与 SummarizeCanvas 相同的容错）。指向不存在节点的悬空边不计入任何节点的 Upstream，但计入条数。
func Outline(raw json.RawMessage) (nodes []OutlineNode, edges int) {
	var doc struct {
		Nodes []map[string]any `json:"nodes"`
		Edges []map[string]any `json:"edges"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &doc) != nil {
		return nil, 0
	}
	type in struct {
		src   string
		order float64
	}
	incoming := map[string][]in{}
	for _, e := range doc.Edges {
		src, _ := e["source"].(string)
		dst, _ := e["target"].(string)
		if src == "" || dst == "" {
			continue
		}
		o, _ := e["order"].(float64)
		incoming[dst] = append(incoming[dst], in{src: src, order: o})
	}
	for _, n := range doc.Nodes {
		s := OutlineNode{ID: outlineStr(n["id"]), Type: outlineStr(n["type"]), Title: outlineStr(n["title"])}
		if r, ok := n["result"].(map[string]any); ok {
			s.Status = outlineStr(r["status"])
		}
		if _, ok := n["agent"].(map[string]any); ok {
			s.ByAgent = true
		}
		if p, ok := n["params"]; ok && p != nil {
			s.Params, _ = json.Marshal(p)
		}
		ins := incoming[s.ID]
		sort.SliceStable(ins, func(i, j int) bool {
			if ins[i].order != ins[j].order {
				return ins[i].order < ins[j].order
			}
			return ins[i].src < ins[j].src
		})
		for _, x := range ins {
			s.Upstream = append(s.Upstream, x.src)
		}
		nodes = append(nodes, s)
	}
	return nodes, len(doc.Edges)
}

// outlineStr 与 agent/app 的 str 同一取法：字符串原样、数字格式化、其余为空。
func outlineStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case float64:
		return fmt.Sprintf("%v", x)
	}
	return ""
}
