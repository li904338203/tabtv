package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// 画布 JSON 的服务端校验。
//
// ⚠️ **只校验不改写**。校验通过后落库的是**原始字节**，而不是"解析再序列化"的结果 ——
// 后者会把后端没定义的字段吃掉（GO-35），画布 JSON 会随前端演进长出新字段，
// 存一次丢一批是不可逆的。
//
// ⚠️ 校验只拦**结构性损坏**，不做"顺手修复"：
// 服务端悄悄改过的文档与客户端手上的那份就不一致了，下次保存的 diff 会莫名其妙。
// 拦住的都是"存进去以后打不开"的情况，其余一律放行。

const (
	// MaxNodes / MaxCanvasBytes 是**设计建议值**，不是用户已批准的容量政策。
	// 依据：09 §性能目标要求 100+ 节点画布可用，留 5 倍余量。
	MaxNodes       = 500
	MaxEdges       = 2000
	MaxCanvasBytes = 4 << 20 // 4MB
)

var ErrCanvasInvalid = errors.New("画布数据非法")

type canvasError struct{ msg string }

func (e canvasError) Error() string { return e.msg }
func (e canvasError) Is(target error) bool {
	return target == ErrCanvasInvalid
}

func invalid(format string, args ...any) error {
	return canvasError{msg: fmt.Sprintf(format, args...)}
}

// ValidateCanvas 校验画布 JSON。raw 为空按空画布处理。
func ValidateCanvas(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	if len(raw) > MaxCanvasBytes {
		return invalid("画布数据 %d 字节超过上限 %d 字节", len(raw), MaxCanvasBytes)
	}
	var tree map[string]any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return invalid("画布数据必须是 JSON 对象")
	}

	// schemaVersion 缺失可以接受（老文档），但出现了就必须是数字 ——
	// 写成字符串会让将来按版本分支的迁移逻辑全部走错。
	if v, ok := tree["schemaVersion"]; ok {
		if _, isNum := v.(float64); !isNum {
			return invalid("schemaVersion 必须是数字")
		}
	}

	nodes, err := asArray(tree, "nodes")
	if err != nil {
		return err
	}
	edges, err := asArray(tree, "edges")
	if err != nil {
		return err
	}
	if len(nodes) > MaxNodes {
		return invalid("节点数 %d 超过上限 %d", len(nodes), MaxNodes)
	}
	if len(edges) > MaxEdges {
		return invalid("连线数 %d 超过上限 %d", len(edges), MaxEdges)
	}

	ids := make(map[string]struct{}, len(nodes))
	for i, raw := range nodes {
		n, ok := raw.(map[string]any)
		if !ok {
			return invalid("nodes[%d] 必须是对象", i)
		}
		id, _ := n["id"].(string)
		if id == "" {
			return invalid("nodes[%d] 缺少 id", i)
		}
		if _, dup := ids[id]; dup {
			// 重复 id 会让"选中/删除/连线"全部指向随机一个，是必炸的。
			return invalid("节点 id 重复: %s", id)
		}
		ids[id] = struct{}{}
		if t, _ := n["type"].(string); t == "" {
			return invalid("节点 %s 缺少 type", id)
		}
	}

	// 同一目标的入边 order 不得重复：order 决定首尾帧的首/尾与参考图锚定顺序，
	// 重复就无从排序。⚠️ 允许有**间隔**（1、3）——间隔不影响排序，
	// 为它报错只会让一张本来能用的画布存不进去。
	seenOrder := map[string]map[int]string{}
	for i, raw := range edges {
		e, ok := raw.(map[string]any)
		if !ok {
			return invalid("edges[%d] 必须是对象", i)
		}
		id, _ := e["id"].(string)
		if id == "" {
			id = "edges[" + strconv.Itoa(i) + "]"
		}
		src, _ := e["source"].(string)
		dst, _ := e["target"].(string)
		if src == "" || dst == "" {
			return invalid("连线 %s 缺少 source 或 target", id)
		}
		// 悬空边是"存进去以后打不开"的典型：渲染时找不到端点。
		if _, ok := ids[src]; !ok {
			return invalid("连线 %s 的 source 指向不存在的节点 %s", id, src)
		}
		if _, ok := ids[dst]; !ok {
			return invalid("连线 %s 的 target 指向不存在的节点 %s", id, dst)
		}
		if src == dst {
			return invalid("连线 %s 是自环", id)
		}

		ord, ok := e["order"].(float64)
		if !ok {
			return invalid("连线 %s 缺少 order（它决定首尾帧与参考图顺序，不能省）", id)
		}
		o := int(ord)
		if o < 1 {
			return invalid("连线 %s 的 order 必须从 1 起，实际 %d", id, o)
		}
		if seenOrder[dst] == nil {
			seenOrder[dst] = map[int]string{}
		}
		if prev, dup := seenOrder[dst][o]; dup {
			return invalid("节点 %s 的入边 order=%d 重复（%s 与 %s）", dst, o, prev, id)
		}
		seenOrder[dst][o] = id
	}
	return nil
}

func asArray(tree map[string]any, key string) ([]any, error) {
	v, ok := tree[key]
	if !ok || v == nil {
		return nil, nil // 缺失按空处理：空画布不该被拒
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, invalid("%s 必须是数组", key)
	}
	return arr, nil
}
