package canvasgen

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeSmartcutTimelines 按（节点）幂等建时间线；owners 是本项目里已有的时间线 → 绑定的节点。
type fakeSmartcutTimelines struct {
	owners  map[int64]string
	created []string // "节点<-源时间线"
	failOn  string
	lookErr error
	next    int64
}

func (f *fakeSmartcutTimelines) Create(_ context.Context, _ Caller, nodeID string, source int64) (int64, error) {
	if nodeID == f.failOn {
		return 0, errors.New("时间线服务挂了")
	}
	for id, owner := range f.owners {
		if owner == nodeID {
			return id, nil
		}
	}
	f.next++
	id := 8007199254740000 + f.next
	if f.owners == nil {
		f.owners = map[int64]string{}
	}
	f.owners[id] = nodeID
	f.created = append(f.created, nodeID+"<-"+itoa(source))
	return id, nil
}

func (f *fakeSmartcutTimelines) TimelineNode(_ context.Context, _ Caller, timelineID int64) (string, error) {
	if f.lookErr != nil {
		return "", f.lookErr
	}
	if owner, ok := f.owners[timelineID]; ok {
		return owner, nil
	}
	return "", ErrSmartcutTimelineNotFound
}

// 建节点 → 按新节点建时间线 → 独立批次写回 timelineId（字符串）。标题缺省「智能剪辑」，参数只有 timelineId。
func TestCreateSmartcutNode_建节点建时间线写回(t *testing.T) {
	f := &fakePorts{}
	tl := &fakeSmartcutTimelines{}
	nodeID, timelineID, err := CreateSmartcutNode(context.Background(), Orchestrator{Canvas: f}, directorCaller,
		SmartcutNodeInput{Position: &Position{X: 1, Y: 2}}, tl)
	if err != nil || nodeID != "a_new" || timelineID != 8007199254740001 {
		t.Fatalf("node=%s timeline=%d err=%v", nodeID, timelineID, err)
	}
	if len(tl.created) != 1 || tl.created[0] != "a_new<-0" || len(f.applied) != 2 {
		t.Fatalf("按新节点建、不分叉；两批：%v %d", tl.created, len(f.applied))
	}
	add, back := string(f.applied[0].Ops), string(f.applied[1].Ops)
	if !strings.Contains(add, `"type":"smartcut"`) || !strings.Contains(add, `"title":"智能剪辑"`) {
		t.Errorf("建节点：%s", add)
	}
	if f.applied[1].BatchID != "agent:3:4:1:timeline" || !strings.Contains(back, `"timelineId":"8007199254740001"`) {
		t.Fatalf("写回应是独立批次、timelineId 为字符串：%s %s", f.applied[1].BatchID, back)
	}
}

func TestCreateSmartcutNode_时间线失败节点保留(t *testing.T) {
	f := &fakePorts{}
	nodeID, timelineID, err := CreateSmartcutNode(context.Background(), Orchestrator{Canvas: f}, directorCaller,
		SmartcutNodeInput{}, &fakeSmartcutTimelines{failOn: "a_new"})
	var oe *ObjectError
	if !errors.As(err, &oe) || oe.NodeID != "a_new" || nodeID != "a_new" || timelineID != 0 || len(f.applied) != 1 {
		t.Fatalf("应返回 ObjectError 并保留节点、不写回：node=%s %d %v %d", nodeID, timelineID, err, len(f.applied))
	}
}

// 四种结果：已绑定（直接用）、指向别人（分叉）、缺失（新建）、不在本项目（新建）。
func TestSmartcutTimelineOfNode_四种结果(t *testing.T) {
	cases := []struct {
		name        string
		params      map[string]any
		owners      map[int64]string
		wantRepair  string
		wantCreated string // 空 = 不建
		wantSame    int64
	}{
		{"已绑定直接用", map[string]any{"timelineId": "77"}, map[int64]string{77: "s1"}, "", "", 77},
		{"指向别的节点时分叉", map[string]any{"timelineId": "77"}, map[int64]string{77: "orig"}, SmartcutRepairForked, "s1<-77", 0},
		{"没有时间线就新建", map[string]any{}, nil, SmartcutRepairCreated, "s1<-0", 0},
		{"不在本项目就新建", map[string]any{"timelineId": "88"}, nil, SmartcutRepairMissing, "s1<-0", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakePorts{nodes: []CanvasNode{{ID: "s1", Type: "smartcut", Params: c.params}}}
			tl := &fakeSmartcutTimelines{owners: c.owners}
			id, repair, err := SmartcutTimelineOfNode(context.Background(), Orchestrator{Canvas: f}, directorCaller, "s1", tl)
			if err != nil || repair != c.wantRepair {
				t.Fatalf("id=%d repair=%q err=%v", id, repair, err)
			}
			if c.wantCreated == "" {
				if id != c.wantSame || len(tl.created) != 0 || len(f.applied) != 0 {
					t.Fatalf("自己的时间线直接用、不建不写：id=%d %v %d", id, tl.created, len(f.applied))
				}
				return
			}
			if len(tl.created) != 1 || tl.created[0] != c.wantCreated {
				t.Fatalf("建法不对：%v", tl.created)
			}
			if len(f.applied) != 1 || !strings.Contains(string(f.applied[0].Ops), `"timelineId":"`+itoa(id)+`"`) {
				t.Fatalf("应写回新的时间线：%+v", f.applied)
			}
		})
	}
}

func TestSmartcutTimelineOfNode_节点不对或查询失败(t *testing.T) {
	f := &fakePorts{nodes: []CanvasNode{{ID: "img", Type: "image"},
		{ID: "s1", Type: "smartcut", Params: map[string]any{"timelineId": "77"}}}}
	o := Orchestrator{Canvas: f}
	tl := &fakeSmartcutTimelines{}
	if _, _, err := SmartcutTimelineOfNode(context.Background(), o, directorCaller, "nope", tl); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("节点不存在：%v", err)
	}
	var ns *NotSmartcutError
	if _, _, err := SmartcutTimelineOfNode(context.Background(), o, directorCaller, "img", tl); !errors.As(err, &ns) {
		t.Fatalf("不是智能剪辑：%v", err)
	}
	// 查时间线出了别的错：原样返回，不能悄悄新建一条空的顶替 —— 那会把复制来的剪辑丢掉。
	tl.lookErr = errors.New("库挂了")
	if _, _, err := SmartcutTimelineOfNode(context.Background(), o, directorCaller, "s1", tl); err == nil ||
		len(tl.created) != 0 || len(f.applied) != 0 {
		t.Fatalf("查询失败不新建：%v %v %d", err, tl.created, len(f.applied))
	}
}

// ---------------------------------------------------------------- 替用户把素材连进来（SCT-41）

func smartcutCanvas() *fakePorts {
	return &fakePorts{nodes: []CanvasNode{{ID: "s1", Type: "smartcut"}, {ID: "v1", Type: "video"},
		{ID: "i1", Type: "image"}, {ID: "a1", Type: "audio"}, {ID: "d1", Type: "director"}, {ID: "sc", Type: "script"}},
		edges: []CanvasEdge{{Source: "v1", Target: "s1", Order: 1}, {Source: "i1", Target: "x", Order: 1}}}
}

// 按给的顺序连、已经连着的跳过（重试不多连一条）；新连的不给 order（画布按现有入边续上）；独立批次。
func TestConnectSmartcutInputs_按顺序连并跳过已连的(t *testing.T) {
	f := smartcutCanvas()
	got, err := ConnectSmartcutInputs(context.Background(), Orchestrator{Canvas: f}, directorCaller, "s1",
		[]string{"a1", "v1", "i1", "sc"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Added, ",") != "a1,i1,sc" || strings.Join(got.Existing, ",") != "v1" {
		t.Fatalf("新连 a1、i1、sc，v1 本来就连着：%+v", got)
	}
	if len(f.applied) != 1 || f.applied[0].BatchID != "agent:3:4:1:inputs" {
		t.Fatalf("一批写完、批次带 :inputs：%+v", f.applied)
	}
	ops := string(f.applied[0].Ops)
	a, i, sc := strings.Index(ops, `"source":"a1"`), strings.Index(ops, `"source":"i1"`), strings.Index(ops, `"source":"sc"`)
	if a < 0 || i < a || sc < i || strings.Contains(ops, `"order"`) || strings.Count(ops, `"target":"s1"`) != 3 {
		t.Fatalf("按给的顺序连进 s1、不带 order：%s", ops)
	}
	// 再来一次（模型重试）：全都连着了，不再写。
	f.edges = append(f.edges, CanvasEdge{Source: "a1", Target: "s1"}, CanvasEdge{Source: "i1", Target: "s1"},
		CanvasEdge{Source: "sc", Target: "s1"})
	got, err = ConnectSmartcutInputs(context.Background(), Orchestrator{Canvas: f}, directorCaller, "s1",
		[]string{"a1", "v1", "i1", "sc"})
	if err != nil || len(got.Added) != 0 || len(got.Existing) != 4 || len(f.applied) != 1 {
		t.Fatalf("全都连着时不写：%+v %v %d", got, err, len(f.applied))
	}
}

// 任何一个不合适就一条都不连，并说清是哪一个、为什么。
func TestConnectSmartcutInputs_不合适就一条都不连(t *testing.T) {
	cases := []struct {
		name   string
		inputs []string
		want   string
	}{
		{"不在画布上", []string{"v1", "nope"}, "画布上没有节点 nope"},
		{"类型不能连", []string{"d1"}, "节点 d1 是 director，不能连进智能剪辑"},
		{"重复", []string{"i1", "i1"}, "重复"},
		{"连给自己", []string{"s1"}, "不能把智能剪辑连给它自己"},
		{"空 ID", []string{" "}, "为空"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := smartcutCanvas()
			_, err := ConnectSmartcutInputs(context.Background(), Orchestrator{Canvas: f}, directorCaller, "s1", c.inputs)
			var ie *SmartcutInputError
			if !errors.As(err, &ie) || !strings.Contains(err.Error(), c.want) || len(f.applied) != 0 {
				t.Fatalf("应拒绝且不写：%v %d", err, len(f.applied))
			}
		})
	}
	tooMany := make([]string, MaxSmartcutInputs+1)
	if err := CheckSmartcutInputs(CanvasView{}, "s1", tooMany); err == nil || !strings.Contains(err.Error(), "最多") {
		t.Fatalf("超过上限：%v", err)
	}
}

func TestConnectSmartcutInputs_目标不是智能剪辑(t *testing.T) {
	f := smartcutCanvas()
	o := Orchestrator{Canvas: f}
	if _, err := ConnectSmartcutInputs(context.Background(), o, directorCaller, "nope", []string{"v1"}); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("目标不存在：%v", err)
	}
	var ns *NotSmartcutError
	if _, err := ConnectSmartcutInputs(context.Background(), o, directorCaller, "v1", []string{"i1"}); !errors.As(err, &ns) {
		t.Fatalf("目标不是智能剪辑：%v", err)
	}
	if len(f.applied) != 0 {
		t.Fatal("目标不对时不写")
	}
}

// 只读取时间线：自己的才给；没有、指向别人的、不在本项目的都是 ok=false，而且不建、不写（只读凭据也要能调）。
func TestSmartcutBoundTimeline_只读不补建(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]any
		owners map[int64]string
		wantOK bool
	}{
		{"绑定在自己身上", map[string]any{"timelineId": "77"}, map[int64]string{77: "s1"}, true},
		{"指向别的节点", map[string]any{"timelineId": "77"}, map[int64]string{77: "orig"}, false},
		{"没有时间线", map[string]any{}, nil, false},
		{"不在本项目", map[string]any{"timelineId": "88"}, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakePorts{nodes: []CanvasNode{{ID: "s1", Type: "smartcut", Params: c.params}}}
			tl := &fakeSmartcutTimelines{owners: c.owners}
			id, ok, err := SmartcutBoundTimeline(context.Background(), Orchestrator{Canvas: f}, directorCaller, "s1", tl)
			if err != nil || ok != c.wantOK || (ok && id != 77) || len(tl.created) != 0 || len(f.applied) != 0 {
				t.Fatalf("id=%d ok=%v err=%v created=%v applied=%d", id, ok, err, tl.created, len(f.applied))
			}
		})
	}
	tl := &fakeSmartcutTimelines{lookErr: errors.New("库挂了")}
	f := &fakePorts{nodes: []CanvasNode{{ID: "s1", Type: "smartcut", Params: map[string]any{"timelineId": "77"}}}}
	if _, _, err := SmartcutBoundTimeline(context.Background(), Orchestrator{Canvas: f}, directorCaller, "s1", tl); err == nil {
		t.Fatal("查询失败原样返回")
	}
}

func TestEdgesOf_解析连线(t *testing.T) {
	got := EdgesOf([]byte(`{"nodes":[],"edges":[{"id":"e1","source":"a","target":"b","order":2},{"source":"","target":"b"}]}`))
	if len(got) != 1 || got[0] != (CanvasEdge{Source: "a", Target: "b", Order: 2}) {
		t.Fatalf("%+v", got)
	}
	if EdgesOf([]byte(`not json`)) != nil || EdgesOf(nil) != nil {
		t.Fatal("解析不了返回 nil")
	}
}
