package app

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
)

// fakeOps 按目标类型给一个固定操作码，记下被问过哪些类型。
type fakeOps struct{ asked []string }

func (f *fakeOps) ResolveOperation(_ context.Context, targetType, _ string, _ []string) (string, error) {
	f.asked = append(f.asked, targetType)
	return map[string]string{"image": "text2img", "video": "img2video", "text": "text2text"}[targetType], nil
}

func parse(t *testing.T, canvas string) ([]string, []string, *fakeOps) {
	t.Helper()
	ops := &fakeOps{}
	s := New()
	s.SetOperations(ops)
	g, _, err := s.parseGraph(context.Background(), json.RawMessage(canvas))
	if err != nil {
		t.Fatal(err)
	}
	nodes := make([]string, 0, len(g.Nodes))
	for _, n := range g.Nodes {
		nodes = append(nodes, n.ID)
	}
	edges := make([]string, 0, len(g.Edges))
	for _, e := range g.Edges {
		edges = append(edges, e.ID)
	}
	sort.Strings(nodes)
	sort.Strings(edges)
	return nodes, edges, ops
}

func TestParseGraph_跳过文档与画框节点及其连线(t *testing.T) {
	nodes, edges, ops := parse(t, `{"nodes":[
		{"id":"img","type":"image","params":{"modelCode":"m"}},
		{"id":"vid","type":"video","params":{"modelCode":"v"}},
		{"id":"doc","type":"document","params":{"docId":"1"}},
		{"id":"frm","type":"frame","params":{}}
	],"edges":[
		{"id":"e1","source":"img","target":"vid","order":1},
		{"id":"e2","source":"doc","target":"vid","order":2},
		{"id":"e3","source":"img","target":"frm","order":1}
	]}`)
	if got := joinIDs(nodes); got != "img,vid" {
		t.Errorf("节点应只剩 img,vid，实际 %s", got)
	}
	if got := joinIDs(edges); got != "e1" {
		t.Errorf("触及文档/画框的连线应被丢弃，实际 %s", got)
	}
	for _, k := range ops.asked {
		if k == "document" || k == "frame" {
			t.Errorf("不该为 %s 节点判定操作码", k)
		}
	}
}

func TestParseGraph_普通节点不受影响(t *testing.T) {
	nodes, edges, _ := parse(t, `{"nodes":[
		{"id":"txt","type":"text","params":{}},
		{"id":"sty","type":"style","params":{"materialCode":"s"}},
		{"id":"img","type":"image","params":{"modelCode":"m"}}
	],"edges":[
		{"id":"e1","source":"txt","target":"img","order":1},
		{"id":"e2","source":"sty","target":"img","order":2}
	]}`)
	if got := joinIDs(nodes); got != "img,sty,txt" {
		t.Errorf("节点不该少，实际 %s", got)
	}
	if got := joinIDs(edges); got != "e1,e2" {
		t.Errorf("连线不该少，实际 %s", got)
	}
}

func joinIDs(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += ","
		}
		out += v
	}
	return out
}

// 用户端与 MCP 写的模型键是 `model`，组节点与旧联调画布写 `modelCode`：两种都要认，
// 否则真实画布跑整图时每个生成节点的模型都是空串（MCPT-11E 第 218 步）。
func TestParseGraph_模型键兼容model与modelCode(t *testing.T) {
	ops := &fakeOps{}
	s := New()
	s.SetOperations(ops)
	g, _, err := s.parseGraph(context.Background(), json.RawMessage(`{"nodes":[
		{"id":"a","type":"video","params":{"model":"m1"}},
		{"id":"b","type":"image","params":{"modelCode":"m2"}},
		{"id":"c","type":"image","params":{"model":"m3","modelCode":"old"}},
		{"id":"d","type":"image","params":{}},
		{"id":"t","type":"text","params":{"model":"auto-picked"}},
		{"id":"t2","type":"text","params":{"modelCode":"m4"}}
	],"edges":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	// 文本节点只认显式 modelCode：面板自动选上的 model 不能把手打的文本变成要扣费的任务。
	want := map[string]string{"a": "m1", "b": "m2", "c": "m3", "d": "", "t": "", "t2": "m4"}
	for _, n := range g.Nodes {
		if n.ModelCode != want[n.ID] {
			t.Errorf("节点 %s 的模型应为 %q，实际 %q", n.ID, want[n.ID], n.ModelCode)
		}
	}
}
