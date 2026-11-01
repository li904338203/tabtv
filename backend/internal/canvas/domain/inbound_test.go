package domain

import (
	"encoding/json"
	"testing"
)

// 入边：按 order 升序、相同按源节点 ID；跳过源是文档 / 画框的与悬空边；每个上游带上它自己第一条入边的操作码。
// 与前端 smartcutInboundSources 同一条规则的完整用例在 api/fixtures/smartcut/inputs/（wiring 的 fixture 测试）。
func TestInboundSources(t *testing.T) {
	raw := json.RawMessage(`{
	  "nodes": [
	    {"id":"sc","type":"smartcut"},
	    {"id":"b","type":"audio","title":"配音","params":{"x":1},"result":{"id":"77","status":"success","mediaId":"9","outputUrl":"/api/v1/media/9","error":3}},
	    {"id":"a","type":"video"},
	    {"id":"t","type":"text"},
	    {"id":"d","type":"document"},
	    {"id":"f","type":"frame"}
	  ],
	  "edges": [
	    {"source":"b","target":"sc","order":2},
	    {"source":"a","target":"sc","order":2},
	    {"source":"t","target":"sc"},
	    {"source":"d","target":"sc","order":1},
	    {"source":"f","target":"sc","order":1},
	    {"source":"ghost","target":"sc","order":1},
	    {"source":"d","target":"b","order":1,"operation":"text2music"},
	    {"source":"t","target":"b","order":2,"operation":"text2audio"}
	  ]
	}`)
	got, err := InboundSources(raw, "sc")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range got {
		ids = append(ids, s.NodeID)
	}
	if len(got) != 3 || ids[0] != "t" || ids[1] != "a" || ids[2] != "b" {
		t.Fatalf("缺 order 的按 0 排最前，相同按 ID：%v", ids)
	}
	b := got[2]
	if b.Order != 2 || b.Type != "audio" || b.Title != "配音" || b.Params["x"] != float64(1) ||
		b.Result.ID != "77" || b.Result.MediaID != "9" || b.Result.Error != "" {
		t.Fatalf("上游字段（result 只认字符串）：%+v", b)
	}
	// b 的入边里文档那条被跳过，第一条是 t → b 的 text2audio。
	if b.FirstInboundOperation != "text2audio" || got[0].FirstInboundOperation != "" {
		t.Fatalf("上游自己的第一条入边：%q %q", b.FirstInboundOperation, got[0].FirstInboundOperation)
	}
	if none, err := InboundSources(raw, "nope"); err != nil || len(none) != 0 {
		t.Fatalf("节点不在画布上时为空：%v %v", none, err)
	}
	if _, err := InboundSources(json.RawMessage(`{"nodes":{}`), "sc"); err == nil {
		t.Fatal("坏 JSON 要报错，不能静默变空")
	}
	if empty, err := InboundSources(nil, "sc"); err != nil || empty != nil {
		t.Fatal("空画布为空")
	}
}
