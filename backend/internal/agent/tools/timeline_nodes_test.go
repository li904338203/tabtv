package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
)

// 实测营销会话：三段视频用 generate 分别出好之后要出成片，原先 timeline_assemble 只认脚本，
// 模型只好用 canvas_apply 画一个空的「时间线」节点、再拿 generate 去「渲染」，全部失败。
// SCT-41（SCD-39）之后，按节点拼 = 建（或复用）智能剪辑节点 → 按顺序连进来 → 连线即入轨铺好，用户在画布上看得到它。

type fakeNodes map[string]app.NodeGeneration

func (f fakeNodes) LatestByNode(context.Context, int64, int64) (map[string]app.NodeGeneration, error) {
	return f, nil
}

func nodeTool(nodes fakeNodes) (TimelineAssemble, *scCanvas, *fakeSmartcuts, *fakeTimelines) {
	c := &scCanvas{}
	for id := range nodes {
		c.nodes = append(c.nodes, scNode(id, "video", nil))
	}
	c.nodes = append(c.nodes, scNode("d", "video", nil))
	s, tl := &fakeSmartcuts{}, &fakeTimelines{}
	return TimelineAssemble{MultiDeps{Canvas: c, Smartcuts: s, Timelines: tl, Nodes: nodes}}, c, s, tl
}

func TestTimelineAssemble_nodeIds路建节点并连线(t *testing.T) {
	tool, c, s, tl := nodeTool(fakeNodes{
		"a": {GenerationID: 1, Status: "success", OutputType: "video", OutputMediaID: 11},
		"b": {GenerationID: 2, Status: "success", OutputType: "video", OutputMediaID: 22},
	})
	res, err := tool.Run(context.Background(), multiRC(1), json.RawMessage(`{"nodeIds":["b","a"],"aspectRatio":"16:9"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.creates) != 1 || len(s.aspects) != 1 || s.aspects[0] != "8007199254740001:16:9" {
		t.Fatalf("新建智能剪辑节点与时间线，先定画幅：creates=%v aspects=%v", s.creates, s.aspects)
	}
	edges := c.opsOf(len(c.applied) - 1)
	if strings.Index(edges, `"source":"b"`) < 0 || strings.Index(edges, `"source":"b"`) > strings.Index(edges, `"source":"a"`) {
		t.Fatalf("应按给的顺序连进来：%s", edges)
	}
	if len(s.syncs) != 1 || tl.assembled != 0 {
		t.Fatal("按连线即入轨铺（不走按分镜装配）")
	}
	c0 := contentMap(t, res)
	if c0["assembled"] != true || c0["timelineId"] != "8007199254740001" || c0["nodeId"] != "a_new1" ||
		c0["appended"] != float64(2) || !strings.Contains(c0["next"].(string), "render") {
		t.Fatalf("结果要给出 timelineId、nodeId 并提示用 render：%v", c0)
	}
}

func TestTimelineAssemble_给timelineId时复用节点(t *testing.T) {
	tool, c, s, _ := nodeTool(fakeNodes{
		"a": {Status: "success", OutputType: "video", OutputMediaID: 11},
		"b": {Status: "success", OutputType: "video", OutputMediaID: 22},
	})
	c.nodes = append(c.nodes, scNode("s9", "smartcut", map[string]any{"timelineId": "77"}))
	c.edges = append(c.edges, map[string]any{"source": "a", "target": "s9", "order": 1})
	s.owners = map[int64]string{77: "s9"}
	s.arranged = true
	res, err := tool.Run(context.Background(), multiRC(3), json.RawMessage(`{"nodeIds":["a","b"],"timelineId":"77"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.creates) != 0 || len(c.applied) != 1 || strings.Contains(c.opsOf(0), `"source":"a"`) {
		t.Fatalf("复用 s9、只连 b：%d %v", len(c.applied), s.creates)
	}
	m := contentMap(t, res)
	if m["nodeId"] != "s9" || m["skipped"] != float64(1) || !strings.Contains(m["message"].(string), "已经剪过") {
		t.Fatalf("剪过的时间线：新连的只进素材条，说清楚：%v", m)
	}
}

func TestTimelineAssemble_有节点还没出片时默认不拼(t *testing.T) {
	nodes := fakeNodes{
		"a": {Status: "success", OutputType: "video", OutputMediaID: 11},
		"b": {Status: "failed", ErrorCode: "provider_rejected"},
		"c": {Status: "success", OutputType: "image", OutputMediaID: 33},
	}
	tool, cv, s, _ := nodeTool(nodes)
	res, err := tool.Run(context.Background(), multiRC(1), json.RawMessage(`{"nodeIds":["a","b","c","d"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cv.applied) != 0 || len(s.creates) != 0 {
		t.Fatal("有缺口时默认不拼，也不建节点（少一段的成片看起来是完整的）")
	}
	msg := contentMap(t, res)["message"].(string)
	for _, want := range []string{"第 2 段（节点 b）：最近一次生成失败了（provider_rejected）",
		"第 3 段（节点 c）：产出不是视频", "第 4 段（节点 d）：还没有生成过"} {
		if !strings.Contains(msg, want) {
			t.Errorf("缺口要逐段说清，缺 %q：%s", want, msg)
		}
	}
	// 接受缺口：只连已出片的，缺口仍然说出来。
	res, err = tool.Run(context.Background(), multiRC(2), json.RawMessage(`{"nodeIds":["a","b"],"allowGaps":true}`))
	if err != nil {
		t.Fatal(err)
	}
	edges := cv.opsOf(len(cv.applied) - 1)
	if !strings.Contains(edges, `"source":"a"`) || strings.Contains(edges, `"source":"b"`) {
		t.Fatalf("allowGaps=true 只连已出片的：%s", edges)
	}
	if !strings.Contains(string(res.Content), "第 2 段") {
		t.Fatalf("缺口仍要说出来：%s", res.Content)
	}
}

func TestTimelineAssemble_脚本与节点只能给一个(t *testing.T) {
	tool, _, _, _ := nodeTool(fakeNodes{})
	if _, err := tool.Run(context.Background(), multiRC(1), json.RawMessage(`{"scriptId":"9","nodeIds":["a"]}`)); err == nil {
		t.Fatal("两种用法不能混用")
	}
	if _, err := tool.Run(context.Background(), multiRC(1), json.RawMessage(`{"nodeIds":["a","a"]}`)); err == nil {
		t.Fatal("重复的节点要拒")
	}
}
