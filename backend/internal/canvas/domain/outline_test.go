package domain

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestOutline_上游ID按order排序(t *testing.T) {
	raw := json.RawMessage(`{"nodes":[
		{"id":"a","type":"image","title":"首帧"},
		{"id":"b","type":"image","title":"尾帧"},
		{"id":"c","type":"text"},
		{"id":"v","type":"video","title":"成片","params":{"mode":"first_last_frame"},"result":{"status":"success"},"agent":{"sessionId":"1"}}
	],"edges":[
		{"id":"e1","source":"b","target":"v","order":2},
		{"id":"e2","source":"c","target":"v","order":3},
		{"id":"e3","source":"a","target":"v","order":1},
		{"id":"e4","source":"ghost","target":"","order":1}
	]}`)
	nodes, edges := Outline(raw)
	if edges != 4 || len(nodes) != 4 {
		t.Fatalf("nodes=%d edges=%d", len(nodes), edges)
	}
	v := nodes[3]
	if !slices.Equal(v.Upstream, []string{"a", "b", "c"}) {
		t.Errorf("上游必须按 order 排：%v", v.Upstream)
	}
	if v.Status != "success" || !v.ByAgent || v.Title != "成片" || v.Type != "video" {
		t.Errorf("字段取错：%+v", v)
	}
	if string(v.Params) != `{"mode":"first_last_frame"}` {
		t.Errorf("params=%s", v.Params)
	}
	if nodes[0].Upstream != nil || nodes[0].Params != nil {
		t.Errorf("无入边无 params 的节点应为空：%+v", nodes[0])
	}
}

func TestOutline_坏数据返回空(t *testing.T) {
	for _, raw := range []string{``, `[]`, `{"nodes":"x"}`} {
		if n, e := Outline(json.RawMessage(raw)); n != nil || e != 0 {
			t.Errorf("%q → %v %d", raw, n, e)
		}
	}
}
