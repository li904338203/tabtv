package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateCanvas_放行合法文档(t *testing.T) {
	cases := map[string]string{
		"空画布":           `{"schemaVersion":1,"nodes":[],"edges":[]}`,
		"缺 nodes/edges": `{"schemaVersion":1}`,
		"空对象":           `{}`,
		"带未知字段":         `{"未来字段":{"a":1},"nodes":[{"id":"n1","type":"image","怪字段":true}],"edges":[]}`,
		"order 有间隔": `{"nodes":[{"id":"a","type":"image"},{"id":"b","type":"image"},{"id":"v","type":"video"}],
		              "edges":[{"id":"e1","source":"a","target":"v","order":1},
		                       {"id":"e2","source":"b","target":"v","order":3}]}`,
		"不同目标可同号": `{"nodes":[{"id":"a","type":"image"},{"id":"v1","type":"video"},{"id":"v2","type":"video"}],
		              "edges":[{"id":"e1","source":"a","target":"v1","order":1},
		                       {"id":"e2","source":"a","target":"v2","order":1}]}`,
	}
	for name, doc := range cases {
		if err := ValidateCanvas(json.RawMessage(doc)); err != nil {
			t.Errorf("%s 应通过，实际 %v", name, err)
		}
	}
}

// 拦住的都是"存进去以后打不开"的情况。
func TestValidateCanvas_拦截结构性损坏(t *testing.T) {
	cases := []struct{ name, doc, want string }{
		{"不是对象", `[1,2]`, "JSON 对象"},
		{"nodes 不是数组", `{"nodes":{}}`, "必须是数组"},
		{"节点缺 id", `{"nodes":[{"type":"image"}]}`, "缺少 id"},
		{"节点缺 type", `{"nodes":[{"id":"n1"}]}`, "缺少 type"},
		// 重复 id 会让选中/删除/连线全部指向随机一个。
		{"节点 id 重复", `{"nodes":[{"id":"n1","type":"image"},{"id":"n1","type":"video"}]}`, "重复"},
		// 悬空边：渲染时找不到端点，下次打开直接失败。
		{"边指向不存在的节点", `{"nodes":[{"id":"a","type":"image"}],
			"edges":[{"id":"e1","source":"a","target":"ghost","order":1}]}`, "不存在的节点"},
		{"自环", `{"nodes":[{"id":"a","type":"image"}],
			"edges":[{"id":"e1","source":"a","target":"a","order":1}]}`, "自环"},
		{"边缺 order", `{"nodes":[{"id":"a","type":"image"},{"id":"v","type":"video"}],
			"edges":[{"id":"e1","source":"a","target":"v"}]}`, "order"},
		{"order 从 0 起", `{"nodes":[{"id":"a","type":"image"},{"id":"v","type":"video"}],
			"edges":[{"id":"e1","source":"a","target":"v","order":0}]}`, "必须从 1 起"},
		// 同一目标 order 重复 → 首尾帧无从排序。
		{"同目标 order 重复", `{"nodes":[{"id":"a","type":"image"},{"id":"b","type":"image"},{"id":"v","type":"video"}],
			"edges":[{"id":"e1","source":"a","target":"v","order":1},
			         {"id":"e2","source":"b","target":"v","order":1}]}`, "重复"},
		{"schemaVersion 是字符串", `{"schemaVersion":"1"}`, "必须是数字"},
	}
	for _, c := range cases {
		err := ValidateCanvas(json.RawMessage(c.doc))
		if err == nil {
			t.Errorf("%s 应被拒", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s 的错误应说明「%s」，实际 %q", c.name, c.want, err.Error())
		}
	}
}

func TestValidateCanvas_容量上限(t *testing.T) {
	nodes := make([]string, MaxNodes+1)
	for i := range nodes {
		nodes[i] = `{"id":"n` + string(rune('a'+i%26)) + string(rune('0'+i/26%10)) +
			string(rune('0'+i/260)) + `","type":"image"}`
	}
	doc := `{"nodes":[` + strings.Join(nodes, ",") + `],"edges":[]}`
	if err := ValidateCanvas(json.RawMessage(doc)); err == nil {
		t.Error("超过节点上限应被拒")
	}

	big := `{"nodes":[],"edges":[],"blob":"` + strings.Repeat("x", MaxCanvasBytes) + `"}`
	err := ValidateCanvas(json.RawMessage(big))
	if err == nil || !strings.Contains(err.Error(), "字节") {
		t.Errorf("超过体积上限应被拒，实际 %v", err)
	}
}

// 校验**不得改写**文档：调用前后原始字节必须一模一样。
// 这条看起来多余，但它防的是"顺手在校验里规范化一下"——
// 那会把后端没定义的字段吃掉，存一次丢一批且不可逆。
func TestValidateCanvas_不改写输入(t *testing.T) {
	doc := `{"未来字段":{"a":[1,2]},"nodes":[{"id":"n1","type":"image","x":1}],"edges":[]}`
	raw := json.RawMessage(doc)
	if err := ValidateCanvas(raw); err != nil {
		t.Fatal(err)
	}
	if string(raw) != doc {
		t.Fatalf("校验改写了输入：\n原 %s\n后 %s", doc, string(raw))
	}
}
