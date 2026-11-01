package wiring

import (
	"encoding/json"
	"reflect"
	"testing"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	canvasdomain "github.com/li904338203/tabtv/backend/internal/canvas/domain"
)

// canvas/domain.Outline 是第二个解析 canvasData 的摘要函数（MCPT-04，开发 01 §12 R10）。
// 它与 Agent 的 SummarizeCanvas 在共有的五个字段上必须逐字一致 —— 否则 Agent 与外部客户端
// 看到的「节点状态」会分叉，而两边各自都"没错"。
func TestOutline_与SummarizeCanvas节点字段一致(t *testing.T) {
	// Agent 摘要多了字段而这里没跟上的话，对照就漏了一项 —— 先钉住字段数。
	if n := reflect.TypeOf(agentapp.CanvasNodeSummary{}).NumField(); n != 5 {
		t.Fatalf("CanvasNodeSummary 现在有 %d 个字段：请把新字段加进 canvas/domain.Outline 与本对照", n)
	}
	cases := []string{
		`{"nodes":[{"id":"a","type":"text","title":"开场"},{"id":"b","type":"image","result":{"status":"running"},"agent":{"sessionId":"9"}}],"edges":[{"source":"a","target":"b","order":1}]}`,
		`{"nodes":[{"id":7,"type":"video","title":3.5,"result":{"status":"success"}},{"id":"x","type":"audio","agent":"不是对象"}],"edges":[]}`,
		`{"nodes":[{"id":"n1","type":"image","result":"坏"}]}`,
		`{"nodes":[]}`,
		`不是 JSON`,
		``,
	}
	for _, c := range cases {
		raw := json.RawMessage(c)
		want, wantEdges := agentapp.SummarizeCanvas(raw)
		got, gotEdges := canvasdomain.Outline(raw)
		if wantEdges != gotEdges || len(want) != len(got) {
			t.Fatalf("%s：条数不一致 agent=(%d,%d) outline=(%d,%d)", c, len(want), wantEdges, len(got), gotEdges)
		}
		for i := range want {
			w, g := want[i], got[i]
			if w.ID != g.ID || w.Type != g.Type || w.Title != g.Title || w.Status != g.Status || w.ByAgent != g.ByAgent {
				t.Errorf("%s 第 %d 个节点不一致：agent=%+v outline=%+v", c, i, w, g)
			}
		}
	}
}
