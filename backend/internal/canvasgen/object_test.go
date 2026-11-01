package canvasgen

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// 建节点 → 建对象 → 写回：节点先落盘、对象按真实节点 ID 建、写回用独立批次 `{batch}:{suffix}`。
func TestCreateObjectNode_节点先落盘再建对象并用独立批次写回(t *testing.T) {
	f := &fakePorts{}
	o := Orchestrator{Canvas: f}
	c := Caller{WorkspaceID: 1, ProjectID: 2, BatchID: "mcp:5:k:abc",
		Origin: Origin{Kind: OriginMCP, GrantID: 5}}
	var gotNode string
	id, err := o.CreateObjectNode(context.Background(), c, ObjectNode{NodeType: "script", Title: "脚本",
		Params: map[string]any{"sourceType": "script"}}, "script",
		func(_ context.Context, nodeID string) (map[string]any, error) {
			gotNode = nodeID
			return ScriptNodeParams(ScriptFacts{ID: 9007199254740993, Status: "draft"}), nil
		})
	if err != nil || id != "a_new" || gotNode != "a_new" {
		t.Fatalf("id=%s gotNode=%s err=%v", id, gotNode, err)
	}
	if len(f.applied) != 2 || f.applied[0].BatchID != "mcp:5:k:abc" || f.applied[1].BatchID != "mcp:5:k:abc:script" {
		t.Fatalf("两批：%+v", f.applied)
	}
	if f.applied[0].Origin.Kind != OriginMCP || f.applied[1].Origin.GrantID != 5 {
		t.Fatalf("来源要透传：%+v", f.applied)
	}
	if !strings.Contains(string(f.applied[1].Ops), `"scriptId":"9007199254740993"`) ||
		!strings.Contains(string(f.applied[1].Ops), `"update_node"`) {
		t.Fatalf("写回应是 update_node 且 ID 为字符串：%s", f.applied[1].Ops)
	}
}

// 建对象失败：节点已建好（返回节点 ID + ObjectError，业务码可 Unwrap），不写回。
func TestCreateObjectNode_建对象失败保留节点且不写回(t *testing.T) {
	f := &fakePorts{}
	o := Orchestrator{Canvas: f}
	boom := errors.New("脚本服务挂了")
	id, err := o.CreateObjectNode(context.Background(), Caller{BatchID: "b"}, ObjectNode{NodeType: "script"}, "script",
		func(context.Context, string) (map[string]any, error) { return nil, boom })
	var oe *ObjectError
	if !errors.As(err, &oe) || oe.NodeID != "a_new" || id != "a_new" || !errors.Is(err, boom) {
		t.Fatalf("err=%v id=%s", err, id)
	}
	if len(f.applied) != 1 {
		t.Fatalf("不应写回：%d", len(f.applied))
	}
}

// 画布没返回节点 ID：ErrNoNodeID，不建对象。
func TestCreateObjectNode_画布未返回节点ID不建对象(t *testing.T) {
	f := &fakePorts{noNodeID: true}
	called := false
	_, err := Orchestrator{Canvas: f}.CreateObjectNode(context.Background(), Caller{}, ObjectNode{NodeType: "group"}, "group",
		func(context.Context, string) (map[string]any, error) { called = true; return nil, nil })
	if !errors.Is(err, ErrNoNodeID) || called {
		t.Fatalf("err=%v called=%v", err, called)
	}
}

// 组节点参数：子项带 itemId（字符串）、视频组 outputType=video、成功项 progress=100。
func TestGroupNodeParams_子项带itemId(t *testing.T) {
	p := GroupNodeParams(GroupFacts{ID: 10, ProjectID: 2, ScriptID: 9, GroupType: "video_generator", Status: "draft",
		Items: []GroupItemFacts{{ID: 7, ShotID: 3, ShotNo: 1, Status: "success", DurationSec: 5}}}, "kling")
	b, _ := json.Marshal(p)
	for _, want := range []string{`"itemId":"7"`, `"shotId":"3"`, `"outputType":"video"`, `"progress":100`,
		`"modelCode":"kling"`, `"groupId":"10"`, `"duration":5`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("缺 %s：%s", want, b)
		}
	}
}
