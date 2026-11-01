package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestValidateDocPath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"规划", "/plan.md", true},
		{"分镜表", "/shots.md", true},
		{"项目记忆", "/.memory.md", true},
		{"笔记", "/notes/角色设定.md", true},
		{"笔记英文", "/notes/doc-1.md", true},
		{"空", "", false},
		{"不以斜杠开头", "plan.md", false},
		{"大小写不同的固定名", "/Plan.md", false},
		{"根下任意名", "/other.md", false},
		{"笔记嵌套两层", "/notes/a/b.md", false},
		{"笔记不是md", "/notes/a.txt", false},
		{"笔记只有后缀", "/notes/.md", false},
		{"点点", "/notes/../plan.md", false},
		{"点", "/./plan.md", false},
		{"空段", "//plan.md", false},
		{"尾部空段", "/notes/", false},
		{"反斜杠", "/notes/a\\b.md", false},
		{"控制字符", "/notes/a\nb.md", false},
		{"超长", "/notes/" + strings.Repeat("长", 200) + ".md", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ValidateDocPath(c.in)
			if c.ok {
				if err != nil || got != c.in {
					t.Fatalf("应通过：got=%q err=%v", got, err)
				}
				return
			}
			if !errors.Is(err, ErrDocPathInvalid) {
				t.Fatalf("应拒绝 %q，err=%v", c.in, err)
			}
		})
	}
}

func TestValidateDocContent_上限按字节(t *testing.T) {
	if err := ValidateDocContent(strings.Repeat("a", MaxDocBytes)); err != nil {
		t.Fatalf("恰好到上限应通过: %v", err)
	}
	if err := ValidateDocContent(strings.Repeat("a", MaxDocBytes+1)); !errors.Is(err, ErrDocTooLarge) {
		t.Fatalf("超上限应拒绝: %v", err)
	}
}

func TestRebindDocIDs_只改文档节点且保留未知字段(t *testing.T) {
	raw := json.RawMessage(`{"未来字段":1,"nodes":[
		{"id":"d1","type":"document","params":{"docId":"100"},"agent":{"x":1}},
		{"id":"d2","type":"document","params":{"docId":"999"}},
		{"id":"t1","type":"text","params":{"docId":"100"}}
	],"edges":[]}`)
	out, n, err := RebindDocIDs(raw, map[int64]int64{100: 200})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应改写 1 个节点，实际 %d", n)
	}
	var tree map[string]any
	_ = json.Unmarshal(out, &tree)
	nodes := tree["nodes"].([]any)
	param := func(i int) any { return nodes[i].(map[string]any)["params"].(map[string]any)["docId"] }
	if param(0) != "200" {
		t.Errorf("文档节点没重绑: %v", param(0))
	}
	if param(1) != "999" {
		t.Errorf("不在映射里的 docId 应原样保留: %v", param(1))
	}
	if param(2) != "100" {
		t.Errorf("非文档节点不该被改: %v", param(2))
	}
	if _, ok := tree["未来字段"]; !ok {
		t.Error("未知顶层字段丢了")
	}
	if _, ok := nodes[0].(map[string]any)["agent"]; !ok {
		t.Error("节点上的未知字段丢了")
	}
}
