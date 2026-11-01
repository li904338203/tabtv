package app

import (
	"bytes"
	"encoding/json"

	"github.com/google/jsonschema-go/jsonschema"
)

// SnapshotTool 是工具快照 api/mcp/tools.json 的一项（开发 02 §4.2、MT-8）。
// 字段顺序即输出顺序；schema 的属性顺序取结构体字段顺序（jsonschema-go 的 PropertyOrder），
// 所以同一份代码重复生成**逐字节一致**，`make gen-check` 才能拿它比对漂移。
type SnapshotTool struct {
	Name         string             `json:"name"`
	Toolset      string             `json:"toolset"`
	Description  string             `json:"description"`
	Methods      []SnapshotMethod   `json:"methods"`
	Annotations  Annotations        `json:"annotations"`
	InputSchema  *jsonschema.Schema `json:"inputSchema"`
	OutputSchema *jsonschema.Schema `json:"outputSchema"`
}

// SnapshotMethod 是一个 method 的权限与计费属性。Name 为空表示工具没有 method 参数。
// ReadOnly = 在 /mcp/readonly 上可见（不写、不花钱、只要 view，MCP-F-14）：
// 绊线测试与前端直接读它，不必各自再推一遍规则。
type SnapshotMethod struct {
	Name        string `json:"name"`
	Action      string `json:"action"`
	Scope       string `json:"scope"`
	ReadOnly    bool   `json:"readOnly"`
	Paid        bool   `json:"paid"`
	Write       bool   `json:"write"`
	Destructive bool   `json:"destructive"`
}

// Snapshot 把注册表导出成快照 JSON（按工具名排序、两空格缩进、结尾换行）。
// 工具为空时输出 `[]`，不是 `null`。
func Snapshot(r *Registry) ([]byte, error) {
	out := []SnapshotTool{}
	for _, t := range r.List() {
		st := SnapshotTool{Name: t.Name, Toolset: t.Toolset, Description: t.Description,
			Annotations: t.Annotations(), InputSchema: t.InputSchema(), OutputSchema: t.OutputSchema()}
		for _, m := range t.Methods {
			scope, _ := scopeOf(m)
			st.Methods = append(st.Methods, SnapshotMethod{Name: m.Name, Action: string(m.Action), Scope: scope,
				ReadOnly: m.ReadOnly(), Paid: m.Paid, Write: m.Write, Destructive: m.Destructive})
		}
		out = append(out, st)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
