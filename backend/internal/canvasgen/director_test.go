package canvasgen

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
)

// fakeDirectorScenes 按（节点）幂等建场景；owners 是本项目里已有的场景 → 绑定的节点。
type fakeDirectorScenes struct {
	owners  map[int64]string
	created []string // "节点<-源场景"
	failOn  string   // 对这个节点建场景时报错
	next    int64
}

func (f *fakeDirectorScenes) Create(_ context.Context, _ Caller, nodeID string, source int64) (int64, error) {
	if nodeID == f.failOn {
		return 0, errors.New("场景服务挂了")
	}
	for id, owner := range f.owners {
		if owner == nodeID {
			return id, nil
		}
	}
	f.next++
	id := 9007199254740000 + f.next
	if f.owners == nil {
		f.owners = map[int64]string{}
	}
	f.owners[id] = nodeID
	f.created = append(f.created, nodeID+"<-"+itoa(source))
	return id, nil
}

func (f *fakeDirectorScenes) SceneNode(_ context.Context, _ Caller, sceneID int64) (string, error) {
	if owner, ok := f.owners[sceneID]; ok {
		return owner, nil
	}
	return "", ErrDirectorSceneNotFound
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

var directorCaller = Caller{WorkspaceID: 1, ProjectID: 2, BatchID: "agent:3:4:1",
	Origin: Origin{Kind: OriginAgent, SessionID: 3}}

// 建节点 → 按新节点建场景 → 独立批次写回 sceneId（字符串）。标题缺省「3D 导演台」，参数只有 sceneId。
func TestCreateDirectorNode_建节点建场景写回(t *testing.T) {
	f := &fakePorts{}
	scenes := &fakeDirectorScenes{}
	nodeID, sceneID, err := CreateDirectorNode(context.Background(), Orchestrator{Canvas: f}, directorCaller,
		DirectorNodeInput{Position: &Position{X: 10, Y: 20}}, scenes)
	if err != nil || nodeID != "a_new" || sceneID != 9007199254740001 {
		t.Fatalf("node=%s scene=%d err=%v", nodeID, sceneID, err)
	}
	if len(scenes.created) != 1 || scenes.created[0] != "a_new<-0" {
		t.Fatalf("场景要按新节点建、不分叉：%v", scenes.created)
	}
	if len(f.applied) != 2 {
		t.Fatalf("两批：建节点、写回：%d", len(f.applied))
	}
	add, back := string(f.applied[0].Ops), string(f.applied[1].Ops)
	for _, want := range []string{`"type":"director"`, `"title":"3D 导演台"`, `"params":{}`, `"position":{"x":10,"y":20}`} {
		if !strings.Contains(add, want) {
			t.Errorf("建节点缺 %s：%s", want, add)
		}
	}
	if f.applied[1].BatchID != "agent:3:4:1:scene" || !strings.Contains(back, `"update_node"`) ||
		!strings.Contains(back, `"sceneId":"9007199254740001"`) {
		t.Fatalf("写回应是独立批次的 update_node、sceneId 为字符串：%s %s", f.applied[1].BatchID, back)
	}
}

// 场景建失败：节点保留（ObjectError 带节点 ID），不写回 —— 前端打开导演台时按节点补建（DRD-43）。
func TestCreateDirectorNode_场景失败节点保留(t *testing.T) {
	f := &fakePorts{}
	nodeID, sceneID, err := CreateDirectorNode(context.Background(), Orchestrator{Canvas: f}, directorCaller,
		DirectorNodeInput{Title: "  "}, &fakeDirectorScenes{failOn: "a_new"})
	var oe *ObjectError
	if !errors.As(err, &oe) || oe.NodeID != "a_new" || nodeID != "a_new" || sceneID != 0 {
		t.Fatalf("应返回 ObjectError 并保留节点：node=%s scene=%d err=%v", nodeID, sceneID, err)
	}
	if len(f.applied) != 1 {
		t.Fatalf("场景没建好就不写回：%d 批", len(f.applied))
	}
	if !strings.Contains(string(f.applied[0].Ops), `"title":"3D 导演台"`) {
		t.Fatalf("空白标题用默认标题：%s", f.applied[0].Ops)
	}
}

// 节点上没有 sceneId：按节点补建并写回。
func TestDirectorSceneOfNode_没有场景先补建并写回(t *testing.T) {
	f := &fakePorts{nodes: []CanvasNode{{ID: "d1", Type: "director", Params: map[string]any{}}}}
	scenes := &fakeDirectorScenes{}
	id, repair, err := DirectorSceneOfNode(context.Background(), Orchestrator{Canvas: f}, directorCaller, "d1", scenes)
	if err != nil || id == 0 || repair != DirectorRepairCreated {
		t.Fatalf("id=%d repair=%q err=%v", id, repair, err)
	}
	if len(scenes.created) != 1 || scenes.created[0] != "d1<-0" {
		t.Fatalf("应按节点新建：%v", scenes.created)
	}
	if len(f.applied) != 1 || f.applied[0].BatchID != "agent:3:4:1:scene" ||
		!strings.Contains(string(f.applied[0].Ops), `"sceneId":"`+itoa(id)+`"`) {
		t.Fatalf("应写回新场景：%+v", f.applied)
	}
}

// sceneId 就是这个节点自己的场景：直接用，不建、不写。
func TestDirectorSceneOfNode_自己的场景直接用(t *testing.T) {
	f := &fakePorts{nodes: []CanvasNode{{ID: "d1", Type: "director",
		Params: map[string]any{"sceneId": "77"}}}}
	scenes := &fakeDirectorScenes{owners: map[int64]string{77: "d1"}}
	id, repair, err := DirectorSceneOfNode(context.Background(), Orchestrator{Canvas: f}, directorCaller, "d1", scenes)
	if err != nil || id != 77 || repair != "" || len(scenes.created) != 0 || len(f.applied) != 0 {
		t.Fatalf("id=%d repair=%q created=%v applied=%d err=%v", id, repair, scenes.created, len(f.applied), err)
	}
}

// 复制出来还没打开过的节点带着原节点的 sceneId：分叉一份给它，**不能**写进原节点的场景（DR-AC-42）。
func TestDirectorSceneOfNode_共用别人的场景时分叉(t *testing.T) {
	f := &fakePorts{nodes: []CanvasNode{{ID: "copy", Type: "director", Params: map[string]any{"sceneId": "77"}}}}
	scenes := &fakeDirectorScenes{owners: map[int64]string{77: "orig"}}
	id, repair, err := DirectorSceneOfNode(context.Background(), Orchestrator{Canvas: f}, directorCaller, "copy", scenes)
	if err != nil || id == 77 || repair != DirectorRepairForked {
		t.Fatalf("id=%d repair=%q err=%v", id, repair, err)
	}
	if len(scenes.created) != 1 || scenes.created[0] != "copy<-77" {
		t.Fatalf("应以原场景为源分叉：%v", scenes.created)
	}
	if len(f.applied) != 1 || !strings.Contains(string(f.applied[0].Ops), `"sceneId":"`+itoa(id)+`"`) {
		t.Fatalf("应写回分叉出来的场景：%+v", f.applied)
	}
}

// sceneId 指向的场景不在本项目（别的项目粘贴来的）：新建空场景，不拿它当分叉源。
func TestDirectorSceneOfNode_场景不在本项目时新建(t *testing.T) {
	f := &fakePorts{nodes: []CanvasNode{{ID: "d1", Type: "director", Params: map[string]any{"sceneId": "88"}}}}
	scenes := &fakeDirectorScenes{}
	id, repair, err := DirectorSceneOfNode(context.Background(), Orchestrator{Canvas: f}, directorCaller, "d1", scenes)
	if err != nil || id == 0 || id == 88 || repair != DirectorRepairMissing {
		t.Fatalf("id=%d repair=%q err=%v", id, repair, err)
	}
	if len(scenes.created) != 1 || scenes.created[0] != "d1<-0" {
		t.Fatalf("不在本项目的场景不能当分叉源：%v", scenes.created)
	}
}

// 节点不存在 / 不是导演台：报错，不建任何东西。
func TestDirectorSceneOfNode_节点不对时报错(t *testing.T) {
	f := &fakePorts{nodes: []CanvasNode{{ID: "img", Type: "image"}}}
	scenes := &fakeDirectorScenes{}
	o := Orchestrator{Canvas: f}
	if _, _, err := DirectorSceneOfNode(context.Background(), o, directorCaller, "nope", scenes); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("节点不存在应 ErrNodeNotFound：%v", err)
	}
	var nd *NotDirectorError
	if _, _, err := DirectorSceneOfNode(context.Background(), o, directorCaller, "img", scenes); !errors.As(err, &nd) ||
		nd.Type != "image" {
		t.Fatalf("不是导演台应 NotDirectorError：%v", err)
	}
	if len(scenes.created) != 0 || len(f.applied) != 0 {
		t.Fatalf("不应建场景或写画布：%v %d", scenes.created, len(f.applied))
	}
}

// 查场景出了别的错（不是「不在本项目」）：原样返回，不能悄悄新建一份空场景顶替 —— 那会把复制来的内容丢掉。
func TestDirectorSceneOfNode_查场景失败不新建(t *testing.T) {
	f := &fakePorts{nodes: []CanvasNode{{ID: "d1", Type: "director", Params: map[string]any{"sceneId": "77"}}}}
	boom := errors.New("数据库抖了一下")
	scenes := &errScenes{err: boom}
	if _, _, err := DirectorSceneOfNode(context.Background(), Orchestrator{Canvas: f}, directorCaller, "d1", scenes); !errors.Is(err, boom) {
		t.Fatalf("应原样返回：%v", err)
	}
	if scenes.creates != 0 || len(f.applied) != 0 {
		t.Fatal("不应建场景或写画布")
	}
}

type errScenes struct {
	err     error
	creates int
}

func (e *errScenes) Create(context.Context, Caller, string, int64) (int64, error) {
	e.creates++
	return 1, nil
}

func (e *errScenes) SceneNode(context.Context, Caller, int64) (string, error) { return "", e.err }

// sceneId 的几种形态：字符串雪花号、JS 安全整数范围内的数字；其余一律当没有。
func TestDirectorSceneIDOf_只认正整数(t *testing.T) {
	cases := []struct {
		v    any
		want int64
	}{
		{"9007199254740993", 9007199254740993}, {" 12 ", 12}, {float64(42), 42},
		{"", 0}, {"abc", 0}, {"-3", 0}, {float64(1.5), 0}, {float64(1 << 60), 0}, {nil, 0}, {true, 0},
	}
	for _, c := range cases {
		if got := DirectorSceneIDOf(map[string]any{"sceneId": c.v}); got != c.want {
			t.Errorf("%v → %d，期望 %d", c.v, got, c.want)
		}
	}
}

// 写回批次 `{batch}:scene` 不超过 canvas_op_log.batch_id 的 64 字符（Agent 与 MCP 两种批次的上限）。
func TestDirectorWriteBackBatchID_长度不超过画布批次列(t *testing.T) {
	for _, batch := range []string{
		"agent:9223372036854775807:9223372036854775807:99",     // Agent：会话 + 步骤 + attempt
		"mcp:9223372036854775807:o:" + strings.Repeat("f", 24), // MCP：domain.ObjectBatchID 的上限
	} {
		if got := WriteBackBatchID(batch, directorWriteBack); len(got) > 64 {
			t.Errorf("canvas_op_log.batch_id 是 varchar(64)，派生后 %d 字符：%s", len(got), got)
		}
	}
}
