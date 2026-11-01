package domain

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// NodeTypeDirector 是画布上导演台节点的类型码。
const NodeTypeDirector = "director"

// RebindSceneIDs 把副本画布里导演台节点的 `params.sceneId` 改写成副本自己的场景 ID（DRD-30）。
//
// 形状照 `canvas/domain.RebindDocIDs`：
//   - ⚠️ 只改 `type=="director"` 的节点：别的节点 params 里恰好有个 sceneId 键，不该被顺手改掉；
//   - ⚠️ 不在映射里的 sceneId **原样保留**（源项目里指向别处场景的节点）：场景查询带 project_id，
//     它在副本里只会显示「场景不在本项目」，打开时由前端自愈出一份新场景（DRD-43），不会读到源项目的内容；
//   - 走通用 map 树，未知字段原样保留（GO-35）。
//
// 返回改写了几个节点；一个都没改时原样返回入参（字节不变）。
func RebindSceneIDs(canvas json.RawMessage, ids map[int64]int64) (json.RawMessage, int, error) {
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
		if !ok || node["type"] != NodeTypeDirector {
			continue
		}
		params, ok := node["params"].(map[string]any)
		if !ok {
			continue
		}
		old, ok := sceneIDOf(params["sceneId"])
		if !ok {
			continue
		}
		if nw, ok := ids[old]; ok {
			// 对外 ID 一律 string（防 JS 精度丢失）。
			params["sceneId"] = strconv.FormatInt(nw, 10)
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

func sceneIDOf(v any) (int64, bool) {
	switch x := v.(type) {
	case string:
		id, err := strconv.ParseInt(x, 10, 64)
		return id, err == nil && id > 0
	case float64:
		// float64 在 2^53 以上会丢精度，雪花号恰好在那个范围 —— 这种写法本来就是坏的，
		// 只为不崩；转不回来的就当不在映射里（同 canvas/domain.docIDOf）。
		return int64(x), x > 0
	}
	return 0, false
}
