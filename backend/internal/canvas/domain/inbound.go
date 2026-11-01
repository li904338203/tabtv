package domain

import (
	"encoding/json"
	"fmt"
	"sort"
)

// InboundSource 是一个节点的一条入边的上游（docs/agent/smartcut SCT-25，SCD-22）：
// 智能剪辑按它解析素材条，前端 `utils/smartcutInputs.ts` 的 `smartcutInboundSources` 是同一条规则，
// 两边由 api/fixtures/smartcut/inputs/ 绊住。
type InboundSource struct {
	// Order 是入边的 order（缺省为 0）。
	Order  int
	NodeID string
	Type   string
	Title  string
	// Params 是上游节点 params 的原样对象（没有时为 nil）。
	Params map[string]any
	Result InboundResult
	// FirstInboundOperation 是这个上游节点**自己**的第一条入边（同样的排序与跳过规则）的操作码；没有为空。
	// 音频的默认用途看它：文字转语音来的默认人声（SC-F-11）。
	FirstInboundOperation string
}

// InboundResult 是上游节点 result 里与素材有关的几项（UI 快照，不是 generation 的事实表）。
type InboundResult struct {
	ID                 string
	SourceGenerationID string
	Status             string
	MediaID            string
	OutputURL          string
	OutputType         string
	OutputText         string
	Error              string
}

// skippedSourceTypes 文档与画框不是素材：连线即使存在也跳过（不占编号）。
var skippedSourceTypes = map[string]bool{"document": true, "frame": true}

// InboundSources 按 order 升序（相同按源节点 ID）给出 nodeID 的全部上游。
// 跳过源是文档 / 画框的连线与悬空边（源节点不存在）。节点不在画布上时返回空（还没保存，或已删除）。
//
// ⚠️ 解析失败返回错误，不静默变空：素材条一空，「连线即入轨」会把它当成「全部断开」，
// 用到这些素材的片段全被标成来源已断开。
func InboundSources(raw json.RawMessage, nodeID string) ([]InboundSource, error) {
	var doc struct {
		Nodes []map[string]any `json:"nodes"`
		Edges []map[string]any `json:"edges"`
	}
	if len(raw) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("画布数据解析失败：%w", err)
	}
	nodes := make(map[string]map[string]any, len(doc.Nodes))
	for _, n := range doc.Nodes {
		if id := outlineStr(n["id"]); id != "" {
			nodes[id] = n
		}
	}
	var out []InboundSource
	for _, in := range inboundOf(doc.Edges, nodes, nodeID) {
		n := nodes[in.src]
		s := InboundSource{Order: in.order, NodeID: in.src, Type: outlineStr(n["type"]), Title: outlineStr(n["title"])}
		if p, ok := n["params"].(map[string]any); ok {
			s.Params = p
		}
		if r, ok := n["result"].(map[string]any); ok {
			// 只认字符串（与前端一致）：数字形式的 ID 在画布里本来就不合法，不能悄悄格式化成一个能用的 ID。
			str := func(k string) string { v, _ := r[k].(string); return v }
			s.Result = InboundResult{ID: str("id"), SourceGenerationID: str("sourceGenerationId"), Status: str("status"),
				MediaID: str("mediaId"), OutputURL: str("outputUrl"), OutputType: str("outputType"),
				OutputText: str("outputText"), Error: str("error")}
		}
		if first := inboundOf(doc.Edges, nodes, in.src); len(first) > 0 {
			s.FirstInboundOperation = first[0].operation
		}
		out = append(out, s)
	}
	return out, nil
}

type inboundEdge struct {
	src       string
	order     int
	operation string
}

// inboundOf 指向 target 的入边：跳过悬空边与源是文档 / 画框的，按 order 升序、相同按源节点 ID。
func inboundOf(edges []map[string]any, nodes map[string]map[string]any, target string) []inboundEdge {
	var out []inboundEdge
	for _, e := range edges {
		src, _ := e["source"].(string)
		if dst, _ := e["target"].(string); dst != target || src == "" {
			continue
		}
		n, ok := nodes[src]
		if !ok || skippedSourceTypes[outlineStr(n["type"])] {
			continue
		}
		o, _ := e["order"].(float64)
		op, _ := e["operation"].(string)
		out = append(out, inboundEdge{src: src, order: int(o), operation: op})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].order != out[j].order {
			return out[i].order < out[j].order
		}
		return out[i].src < out[j].src
	})
	return out
}
