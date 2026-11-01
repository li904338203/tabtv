// Package tools 是画布 Agent 的工具注册表与工具实现（dev/01 §5）。
//
// ⚠️ 依赖方向是 `tools → app`：Tool 接口与 RunContext / ToolResult / Suspension 定义在
// agent/app（Runner 只认 app.ToolSet 端口），这里用类型别名引出，工具实现直接写
// `tools.Tool` 即可。反过来让 app import tools 会成环（工具实现要用 app 的端口）。
package tools

import (
	"fmt"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
)

// 类型别名：工具实现与 Runner 用的是同一组类型。
type (
	Tool       = app.Tool
	RunContext = app.RunContext
	ToolResult = app.ToolResult
	Suspension = app.Suspension
	ToolDef    = app.ToolDef
)

// Registry 按名字登记工具。注册在启动时完成，之后只读（无锁）。
type Registry struct {
	byName map[domain.ToolName]Tool
}

// NewRegistry 建注册表。
func NewRegistry(ts ...Tool) (*Registry, error) {
	r := &Registry{byName: map[domain.ToolName]Tool{}}
	for _, t := range ts {
		if err := r.Register(t); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Register 登记一个工具。
//
// ⚠️ 只收 domain 规格表里有的名字：规格表是白名单与付费 / 画布写属性的唯一定义，
// 注册一个表外的工具等于绕过 AllowedTools —— 它永远不会被下发，却会让人以为能用。
func (r *Registry) Register(t Tool) error {
	name := t.Name()
	if _, ok := domain.SpecOf(name); !ok {
		return fmt.Errorf("工具 %s 不在 domain 规格表里", name)
	}
	if _, dup := r.byName[name]; dup {
		return fmt.Errorf("工具 %s 重复注册", name)
	}
	r.byName[name] = t
	return nil
}

// Lookup 按名查。
func (r *Registry) Lookup(name domain.ToolName) (Tool, bool) {
	if r == nil {
		return nil, false
	}
	t, ok := r.byName[name]
	return t, ok
}

// Schemas 按 names 的顺序（即 AllowedTools 的顺序）输出已注册工具的定义；
// 规格表允许但还没实现的工具跳过 —— 不下发一个调了必失败的工具。
func (r *Registry) Schemas(names []domain.ToolName) []ToolDef {
	out := make([]ToolDef, 0, len(names))
	for _, n := range names {
		t, ok := r.Lookup(n)
		if !ok {
			continue
		}
		out = append(out, ToolDef{Name: string(n), Description: t.Description(), Parameters: t.Schema()})
	}
	return out
}

var _ app.ToolSet = (*Registry)(nil)
