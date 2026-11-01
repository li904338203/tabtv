// Package domain 是智能剪辑的纯函数（docs/agent/smartcut dev/01 §2.1）：不碰库、不发网络（purity_test 钉住）。
package domain

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// NodeType 智能剪辑节点的类型码（与 seed 的 node_type、前端新建节点一致）。
const NodeType = "smartcut"

// MaxNodeIDChars 画布节点 ID 的长度上限（timeline.canvas_node_id 是 varchar(64)）。
const MaxNodeIDChars = 64

// InboundNodeTypes 能连进智能剪辑的节点类型（与 seed 的六条入边逐一对应，seed 的测试钉住）：
// Agent / MCP 替用户连线时按它判（SCT-41）—— 画布写入本身不按连线规则拦，连了一条不允许的线，
// 用户在画布上看到一条打不开的边。
var InboundNodeTypes = []string{"video", "image", "audio", "text", "script", "group"}

// AcceptsInbound 这种节点能不能连进智能剪辑。
func AcceptsInbound(nodeType string) bool {
	for _, t := range InboundNodeTypes {
		if t == nodeType {
			return true
		}
	}
	return false
}

// NodeParams 写回智能剪辑节点的参数：只有 timelineId（字符串，防 JS 精度丢失）。
//
// ⚠️ 成片、封面、版本、过期标记都不写进节点（SCK-13）：导出在 Worker 里异步完成，
// 写进节点的值会被画布的自动保存覆盖。它们由摘要接口给出。
func NodeParams(timelineID int64) map[string]any {
	return map[string]any{"timelineId": strconv.FormatInt(timelineID, 10)}
}

// TimelineIDOf 读节点参数里的 timelineId；缺失或不是正整数时返回 0（按「没有时间线」处理）。
// 前端写的是字符串；数字形式只在 JS 安全整数范围内认（超出的已经丢了精度，认了就是指向别人的时间线）。
func TimelineIDOf(params map[string]any) int64 {
	switch v := params["timelineId"].(type) {
	case string:
		id, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err == nil && id > 0 {
			return id
		}
	case float64:
		if v > 0 && v <= 1<<53 && v == math.Trunc(v) {
			return int64(v)
		}
	}
	return 0
}

// RebindTimelineIDs 复制项目时把画布里智能剪辑节点的 params.timelineId 换成副本的时间线（SC-F-170）。
// ids 是「源时间线 → 副本时间线」；不在表里的原样不动（打开时按节点自愈）。返回改写后的画布与改了几处。
//
// ⚠️ 不认识的字段原样保留（GO-35）：只改 timelineId 这一个键。
func RebindTimelineIDs(canvas json.RawMessage, ids map[int64]int64) (json.RawMessage, int, error) {
	if len(canvas) == 0 || len(ids) == 0 {
		return canvas, 0, nil
	}
	var tree map[string]any
	if err := json.Unmarshal(canvas, &tree); err != nil {
		return nil, 0, fmt.Errorf("画布数据不是合法 JSON 对象: %w", err)
	}
	nodes, _ := tree["nodes"].([]any)
	n := 0
	for _, item := range nodes {
		node, ok := item.(map[string]any)
		if !ok || node["type"] != NodeType {
			continue
		}
		params, ok := node["params"].(map[string]any)
		if !ok {
			continue
		}
		if nw, ok := ids[TimelineIDOf(params)]; ok {
			params["timelineId"] = strconv.FormatInt(nw, 10)
			n++
		}
	}
	if n == 0 {
		return canvas, 0, nil
	}
	out, err := json.Marshal(tree)
	if err != nil {
		return nil, 0, err
	}
	return out, n, nil
}

// RenderStale 「剪辑已改，成片未更新」（SC-F-154）：最近一次成功导出时的版本比当前版本旧。
// 没导出过（successVersion == 0）不算过期 —— 那是「还没有成片」，节点卡与整图各有说法。
func RenderStale(successVersion, currentVersion int) bool {
	return successVersion > 0 && successVersion < currentVersion
}
