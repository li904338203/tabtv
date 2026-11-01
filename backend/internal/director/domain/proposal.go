package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// 提案应用（DRD-26）：网页路径（回提案给前端预览）与 Agent / MCP 路径（服务端写入）共用这一份。
// 前端或另一条路径再实现一份「怎么把提案合进场景」的话，两条路的结果迟早不一样。
//
// ⚠️ 只替换可替换对象（source=ai 且未锁定，DR-F-105）：用户摆的、锁定的对象一个字节都不动。
// ⚠️ 场景按 map 树合并，未知字段原样保留（GO-35）；入参的场景不被修改。

// Changes 是变更清单（对象 ID），前端据此在预览里给新增 / 修改 / 删除的对象标色（DR-F-102）。
type Changes struct {
	Added   []string `json:"added"`
	Updated []string `json:"updated"`
	Removed []string `json:"removed"`
}

// Proposal 是 AI 搭景的提案：完整的建议场景 + 变更清单 + 说明（dev/02 §4.2 DirectorBuildResult.proposal）。
type Proposal struct {
	Scene    json.RawMessage `json:"scene"`
	Changes  Changes         `json:"changes"`
	Defaults []string        `json:"defaults"`
}

// ApplyProposal 把求解结果合进场景，返回建议场景正文与变更清单。
//
// 合并之后的正文过一遍 Validate：对象数、正文大小超上限时返回错误（包着 *ValidationError 或
// ErrSceneTooLarge，调用方按原错误映射）。Resolved 里指向不可替换对象的修改与删除一律跳过 ——
// 正常情况下 Solve 已经把它们滤掉并写了说明，这里是第二道闸（场景在两次调用之间可能变了）。
func ApplyProposal(scene Scene, r Resolved) (json.RawMessage, Changes, error) {
	tree := deepCopyMap(scene.Tree())
	list, _ := tree["objects"].([]any)

	exists := map[string]bool{}
	replaceable := map[string]bool{}
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			o := Object{raw: m}
			exists[o.ID()] = true
			replaceable[o.ID()] = Replaceable(o)
		}
	}
	removed := map[string]bool{}
	for _, id := range r.Removed {
		if replaceable[id] {
			removed[id] = true
		}
	}
	updates := map[string]map[string]any{}
	for _, u := range r.Updated {
		if id := str(u["id"]); replaceable[id] && !removed[id] {
			updates[id] = u
		}
	}

	changes := Changes{Added: []string{}, Updated: []string{}, Removed: []string{}}
	out := make([]any, 0, len(list)+len(r.Added))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			out = append(out, item)
			continue
		}
		id := Object{raw: m}.ID()
		switch u, isUpdate := updates[id]; {
		case removed[id]:
			changes.Removed = append(changes.Removed, id)
		case isUpdate:
			out = append(out, deepCopyMap(u))
			changes.Updated = append(changes.Updated, id)
		default:
			out = append(out, item)
		}
	}
	for _, a := range r.Added {
		id := str(a["id"])
		if exists[id] {
			return nil, Changes{}, fmt.Errorf("新对象 ID %q 与场景里已有的对象重复", id)
		}
		exists[id] = true
		out = append(out, deepCopyMap(a))
		changes.Added = append(changes.Added, id)
	}
	tree["objects"] = out
	if r.OutputCamera != nil {
		if *r.OutputCamera == "" {
			tree["outputCameraId"] = nil
		} else {
			tree["outputCameraId"] = *r.OutputCamera
		}
	}

	raw, err := json.Marshal(tree)
	if err != nil {
		return nil, Changes{}, fmt.Errorf("建议场景序列化失败：%w", err)
	}
	if err := Validate(raw); err != nil {
		return nil, Changes{}, fmt.Errorf("搭景方案合进场景后不合法：%w", err)
	}
	return raw, changes, nil
}

// Propose 是解析之后的全部步骤：求解 → 应用 → 汇总说明。网页路径与 Agent / MCP 路径都调它。
//
// 说明的顺序：模型自己写的 notes → 解析时丢弃了什么 → 求解时用了哪些默认值；重复的只留一条。
func Propose(scene Scene, d Draft, dropped []string, opt SolveOptions) (Proposal, error) {
	r, err := Solve(scene, d, opt)
	if err != nil {
		return Proposal{}, err
	}
	raw, changes, err := ApplyProposal(scene, r)
	if err != nil {
		return Proposal{}, err
	}
	defaults := []string{}
	seen := map[string]bool{}
	for _, group := range [][]string{d.Notes, dropped, r.Defaults} {
		for _, n := range group {
			if n != "" && !seen[n] {
				seen[n] = true
				defaults = append(defaults, n)
			}
		}
	}
	return Proposal{Scene: raw, Changes: changes, Defaults: defaults}, nil
}

// deepCopyMap 深拷贝 map 树（只含 json.Unmarshal 产出的类型与本包构造的 map / 切片）。
func deepCopyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	out, _ := copyAny(m).(map[string]any)
	return out
}

func copyAny(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, item := range x {
			out[k] = copyAny(item)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = copyAny(item)
		}
		return out
	}
	return v
}

// jsonTree 把本包构造的 map（可能含 int 等 Go 类型）规范成 json.Unmarshal 的形态（数字一律 float64），
// 之后读回来的数字类型才一致。
func jsonTree(m map[string]any) map[string]any {
	raw, err := json.Marshal(m)
	if err != nil {
		return m
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return m
	}
	return out
}

// jsonEqual 比较两棵 map 树序列化之后是否逐字相同（Go 序列化 map 时按键排序）。
func jsonEqual(a, b map[string]any) bool {
	ra, err1 := json.Marshal(a)
	rb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(ra, rb)
}
