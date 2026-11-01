package canvasgen

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
)

// ---------------------------------------------------------------- 假端口（记录调用序）

type fakePorts struct {
	calls    []string
	applied  []ApplyInput
	quoted   QuoteInput
	accepted AcceptInput
	nodes    []CanvasNode
	edges    []CanvasEdge
	failLink bool
	noNodeID bool
}

func (f *fakePorts) Read(context.Context, int64, int64) (CanvasView, error) {
	f.calls = append(f.calls, "read")
	return CanvasView{Nodes: f.nodes, Edges: f.edges}, nil
}

func (f *fakePorts) Apply(_ context.Context, in ApplyInput) (ApplyResult, error) {
	f.applied = append(f.applied, in)
	if strings.Contains(string(in.Ops), "link_generation") {
		f.calls = append(f.calls, "link")
		if f.failLink {
			return ApplyResult{}, errors.New("画布写入失败")
		}
		return ApplyResult{}, nil
	}
	f.calls = append(f.calls, "apply")
	if f.noNodeID {
		return ApplyResult{}, nil
	}
	return ApplyResult{NodeIDs: []string{"a_new"}}, nil
}

func (f *fakePorts) QuoteGeneration(_ context.Context, in QuoteInput) (Quote, error) {
	f.calls = append(f.calls, "quote")
	f.quoted = in
	return Quote{Credits: 30}, nil
}

func (f *fakePorts) Accept(_ context.Context, in AcceptInput) (AcceptResult, error) {
	f.calls = append(f.calls, "accept")
	f.accepted = in
	return AcceptResult{GenerationID: 9007199254740993, Credits: 30, Status: "queued"}, nil
}

type fakeParams map[string][]string

func (p fakeParams) NodeParamCodes(_ context.Context, nodeType string) ([]string, error) {
	return p[nodeType], nil
}

// fakeDefaultParams 同时给出参数编码与默认值（wiring.AgentNodeParams 的形状）。
type fakeDefaultParams struct {
	fakeParams
	defaults map[string]map[string]string
}

func (p fakeDefaultParams) NodeParamDefaults(_ context.Context, nodeType string) (map[string]string, error) {
	return p.defaults[nodeType], nil
}

func newOrch(f *fakePorts, log *bytes.Buffer) Orchestrator {
	return Orchestrator{Canvas: f, Quoter: f, Accepter: f,
		Params: fakeParams{"video": {"video_mode", "prompt", "model", "duration", "resolution", "aspect_ratio"}},
		Logger: slog.New(slog.NewTextHandler(log, nil))}
}

var testCaller = Caller{WorkspaceID: 1, ProjectID: 2, UserID: 5, PayerAccountID: 7, MemberLevelID: 8,
	Origin: Origin{Kind: OriginAgent, SessionID: 3}, BatchID: "agent:3:4:1",
	LogAttrs: []any{"session", int64(3), "step", int64(4)}}

func opsOf(t *testing.T, in ApplyInput) []map[string]any {
	t.Helper()
	var ops []map[string]any
	if err := json.Unmarshal(in.Ops, &ops); err != nil {
		t.Fatal(err)
	}
	return ops
}

// 走完整条链：Prepare → EnsureNode → Quote → Accept → Link。
func run(t *testing.T, o Orchestrator, c Caller, req Request) (string, AcceptResult) {
	t.Helper()
	ctx := context.Background()
	p := o.Prepare(ctx, c, req)
	nodeID, err := o.EnsureNode(ctx, c, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.Quote(ctx, c, p, nodeID); err != nil {
		t.Fatal(err)
	}
	r, err := o.Accept(ctx, c, p, nodeID, "idem")
	if err != nil {
		t.Fatal(err)
	}
	o.Link(ctx, c, nodeID, r.GenerationID)
	return nodeID, r
}

// ---------------------------------------------------------------- 用例

func TestOrchestrator_顺序为建节点后报价再受理(t *testing.T) {
	f := &fakePorts{}
	o := newOrch(f, &bytes.Buffer{})
	req := Request{NodeType: "video", OperationCode: "text2video", ModelCode: "sd", Prompt: " 海边 ",
		Params:      BuildNodeParams("video", "sd", "text2video", "海边", map[string]any{"aspectRatio": "9:16"}),
		DurationSec: 15, Inputs: json.RawMessage(`[{"kind":"text","order":1,"text":"x"}]`)}
	nodeID, _ := run(t, o, testCaller, req)
	if got := strings.Join(f.calls, ","); got != "apply,quote,accept,link" {
		t.Fatalf("调用序应为 建节点 → 报价 → 受理 → 挂任务，实际 %s", got)
	}
	if nodeID != "a_new" || f.quoted.NodeID != "a_new" || f.accepted.NodeID != "a_new" {
		t.Errorf("报价与受理都要指向已落盘的节点：%s %s %s", nodeID, f.quoted.NodeID, f.accepted.NodeID)
	}
	// 报价与受理用同一份归一后的参数与时长；文本去空白。
	if string(f.quoted.Params) != string(f.accepted.Params) || f.quoted.DurationSec != 15 || f.accepted.DurationSec != 15 {
		t.Errorf("报价与受理参数不一致：%s / %s", f.quoted.Params, f.accepted.Params)
	}
	if !strings.Contains(string(f.accepted.Params), `"aspect_ratio":"9:16"`) ||
		!strings.Contains(string(f.accepted.Params), `"duration":"15"`) {
		t.Errorf("参数应为画布编码：%s", f.accepted.Params)
	}
	if f.quoted.Text != "海边" || f.accepted.Text != "海边" || f.accepted.IdempotencyKey != "idem" {
		t.Errorf("文本 / 幂等键：%+v", f.accepted)
	}
	if f.accepted.PayerAccountID != 7 || f.accepted.MemberLevelID != 8 || f.quoted.MemberLevelID != 8 ||
		string(f.quoted.DeclaredInputs) != string(req.Inputs) || string(f.accepted.Inputs) != string(req.Inputs) {
		t.Errorf("调用方字段与输入要原样带到报价与受理：%+v %+v", f.quoted, f.accepted)
	}
	if f.applied[0].BatchID != "agent:3:4:1" || f.applied[0].Origin.SessionID != 3 {
		t.Errorf("建节点用调用方的批次与来源：%+v", f.applied[0])
	}
}

func TestOrchestrator_Link用独立批次且失败只告警(t *testing.T) {
	var log bytes.Buffer
	f := &fakePorts{failLink: true}
	o := newOrch(f, &log)
	run(t, o, testCaller, Request{NodeType: "video", OperationCode: "text2video", ModelCode: "sd",
		Params: map[string]any{}})
	link := f.applied[len(f.applied)-1]
	if link.BatchID != "agent:3:4:1:link" || link.BatchID == f.applied[0].BatchID {
		t.Errorf("挂任务那一批必须用独立的批次 ID（否则命中幂等什么都没写）：%s", link.BatchID)
	}
	ops := opsOf(t, link)
	if len(ops) != 1 || ops[0]["kind"] != "link_generation" || ops[0]["nodeId"] != "a_new" {
		t.Fatalf("应发一条 link_generation：%v", ops)
	}
	if p, _ := ops[0]["patch"].(map[string]any); p["generationId"] != "9007199254740993" {
		t.Errorf("generationId 应为字符串且不丢精度：%v", ops[0]["patch"])
	}
	out := log.String()
	for _, want := range []string{"挂到画布节点失败", "session=3", "step=4", "node=a_new", "generation=9007199254740993"} {
		if !strings.Contains(out, want) {
			t.Errorf("告警应含 %q：%s", want, out)
		}
	}
	// generationID 为 0 不发。
	f2 := &fakePorts{}
	newOrch(f2, &bytes.Buffer{}).Link(context.Background(), testCaller, "a_new", 0)
	if len(f2.applied) != 0 {
		t.Error("没有任务时不应发 link_generation")
	}
}

func TestOrchestrator_给nodeId时按节点类型归一并在patch里删别名键(t *testing.T) {
	f := &fakePorts{nodes: []CanvasNode{{ID: "a_v", Type: "video"}}}
	o := newOrch(f, &bytes.Buffer{})
	ctx := context.Background()
	p := o.Prepare(ctx, testCaller, Request{NodeID: "a_v", OperationCode: "img2video", ModelCode: "sd",
		Params: map[string]any{"model": "sd", "aspectRatio": "9:16"}, DurationSec: 10, Title: "改名"})
	if p.ResolvedNodeType != "video" || p.DurationSec != 10 {
		t.Fatalf("应读出节点类型并归一：%+v", p)
	}
	if len(p.Dropped) != 1 || p.Dropped[0] != "aspectRatio" {
		t.Errorf("别名键应记进 Dropped：%v", p.Dropped)
	}
	nodeID, err := o.EnsureNode(ctx, testCaller, p)
	if err != nil || nodeID != "a_v" {
		t.Fatalf("改已有节点返回原 ID：%s %v", nodeID, err)
	}
	up := opsOf(t, f.applied[0])[0]
	if up["kind"] != "update_node" || up["nodeId"] != "a_v" {
		t.Fatalf("应更新已有节点：%v", up)
	}
	patch, _ := up["patch"].(map[string]any)
	params, _ := patch["params"].(map[string]any)
	if params["aspect_ratio"] != "9:16" || params["duration"] != "10" || params["mode"] != "img2video" {
		t.Errorf("已有视频节点也要归一（含 mode）：%v", params)
	}
	if v, ok := params["aspectRatio"]; !ok || v != nil {
		t.Errorf("别名键应以 null 删除（merge patch）：%v", params)
	}
	if patch["title"] != "改名" {
		t.Errorf("给了标题要一并改：%v", patch)
	}
}

// 新建节点按画布配置补默认值（与参数面板同一份）；已有节点不补 —— 那上面的值是用户设的，补了会在 patch 里盖掉。
func TestOrchestrator_新建节点补默认值已有节点不补(t *testing.T) {
	f := &fakePorts{nodes: []CanvasNode{{ID: "a_v", Type: "video"}}}
	o := newOrch(f, &bytes.Buffer{})
	o.Params = fakeDefaultParams{
		fakeParams: fakeParams{"video": {"video_mode", "prompt", "model", "duration", "resolution", "aspect_ratio"}},
		defaults: map[string]map[string]string{"video": {"video_mode": "text2video", "duration": "5",
			"resolution": "720p", "aspect_ratio": "16:9"}},
	}
	ctx := context.Background()
	p := o.Prepare(ctx, testCaller, Request{NodeType: "video", OperationCode: "ref2video", ModelCode: "sd",
		Params: map[string]any{"model": "sd", "aspectRatio": "9:16"}, DurationSec: 15})
	var got map[string]any
	_ = json.Unmarshal(p.AcceptParams, &got)
	want := map[string]any{"model": "sd", "mode": "ref2video", "aspect_ratio": "9:16", "duration": "15",
		"resolution": "720p"}
	if !reflect.DeepEqual(got, want) || p.DurationSec != 15 {
		t.Fatalf("新建节点：只补缺的（清晰度），模型给的画幅（别名）、时长、操作都不被默认值顶掉：%v", got)
	}
	// 什么都没给：画幅、清晰度、时长都按配置默认补上，受理时长同步成 5。
	p = o.Prepare(ctx, testCaller, Request{NodeType: "video", OperationCode: "text2video", ModelCode: "sd",
		Params: map[string]any{"model": "sd"}})
	_ = json.Unmarshal(p.AcceptParams, &got)
	if got["aspect_ratio"] != "16:9" || got["resolution"] != "720p" || got["duration"] != "5" || p.DurationSec != 5 {
		t.Fatalf("都没给时按配置默认补齐：%v dur=%d", got, p.DurationSec)
	}
	// 已有节点：不补。
	p = o.Prepare(ctx, testCaller, Request{NodeID: "a_v", OperationCode: "ref2video", ModelCode: "sd",
		Params: map[string]any{"model": "sd"}})
	if _, ok := p.Params["aspect_ratio"]; ok {
		t.Fatalf("已有节点不补默认值：%v", p.Params)
	}
}

// Agent 在自己的节点上重跑（追加额度后带同一个 nodeId 再调一次）而没再写画幅 / 时长：
// 受理参数以节点上当前的为底，merge patch 仍只写这次给的。
func TestOrchestrator_已有节点上生成以节点参数打底(t *testing.T) {
	f := &fakePorts{nodes: []CanvasNode{{ID: "a_v", Type: "video", Params: map[string]any{
		"model": "sd", "mode": "ref2video", "prompt": "沙发", "aspect_ratio": "9:16", "duration": "15",
		"resolution": "720p"}}}}
	o := newOrch(f, &bytes.Buffer{})
	run(t, o, testCaller, Request{NodeID: "a_v", OperationCode: "ref2video", ModelCode: "sd",
		Params: map[string]any{"model": "sd"}})
	var acc map[string]any
	_ = json.Unmarshal(f.accepted.Params, &acc)
	if acc["aspect_ratio"] != "9:16" || acc["resolution"] != "720p" || acc["prompt"] != "沙发" || acc["duration"] != "15" {
		t.Fatalf("受理参数应以节点当前参数为底：%v", acc)
	}
	if f.accepted.DurationSec != 15 || f.quoted.DurationSec != 15 || string(f.quoted.Params) != string(f.accepted.Params) {
		t.Fatalf("时长取节点上的，报价与受理一致：accept=%d quote=%d", f.accepted.DurationSec, f.quoted.DurationSec)
	}
	patch, _ := opsOf(t, f.applied[0])[0]["patch"].(map[string]any)
	params, _ := patch["params"].(map[string]any)
	if _, ok := params["aspect_ratio"]; ok {
		t.Fatalf("merge patch 只写这次给的，不把节点上的再写一遍：%v", params)
	}
}

func TestOrchestrator_StampNode只作用于add_node(t *testing.T) {
	stamped := 0
	c := testCaller
	c.StampNode = func(op map[string]any) {
		stamped++
		n, _ := op["node"].(map[string]any)
		n["agent"] = map[string]any{"stepId": "4"}
	}
	ctx := context.Background()

	f := &fakePorts{}
	o := newOrch(f, &bytes.Buffer{})
	p := o.Prepare(ctx, c, Request{NodeType: "image", OperationCode: "text2img", ModelCode: "m",
		Params: map[string]any{}, Position: &Position{X: 10, Y: 20}})
	if _, err := o.EnsureNode(ctx, c, p); err != nil {
		t.Fatal(err)
	}
	add := opsOf(t, f.applied[0])[0]
	node, _ := add["node"].(map[string]any)
	if add["kind"] != "add_node" || stamped != 1 {
		t.Fatalf("add_node 应调用一次 StampNode：%v stamped=%d", add, stamped)
	}
	if meta, _ := node["agent"].(map[string]any); meta["stepId"] != "4" || node["title"] != "image" {
		t.Errorf("新节点应带调用方元数据、标题缺省为类型：%v", node)
	}
	if pos, _ := add["position"].(map[string]any); pos["x"] != 10.0 || pos["y"] != 20.0 {
		t.Errorf("位置：%v", add["position"])
	}

	f2 := &fakePorts{nodes: []CanvasNode{{ID: "a_v", Type: "image"}}}
	o2 := newOrch(f2, &bytes.Buffer{})
	p2 := o2.Prepare(ctx, c, Request{NodeID: "a_v", OperationCode: "text2img", ModelCode: "m",
		Params: map[string]any{}})
	if _, err := o2.EnsureNode(ctx, c, p2); err != nil {
		t.Fatal(err)
	}
	if stamped != 1 {
		t.Errorf("update_node 不应调用 StampNode：stamped=%d", stamped)
	}

	// 未给 StampNode 也能建。
	f3 := &fakePorts{}
	o3 := newOrch(f3, &bytes.Buffer{})
	c3 := testCaller
	p3 := o3.Prepare(ctx, c3, Request{NodeType: "text", OperationCode: "text", ModelCode: "m", Params: map[string]any{}})
	if _, err := o3.EnsureNode(ctx, c3, p3); err != nil {
		t.Fatal(err)
	}
}

func TestOrchestrator_画布未返回节点ID时报错(t *testing.T) {
	f := &fakePorts{noNodeID: true}
	o := newOrch(f, &bytes.Buffer{})
	ctx := context.Background()
	p := o.Prepare(ctx, testCaller, Request{NodeType: "image", OperationCode: "text2img", ModelCode: "m",
		Params: map[string]any{}})
	if _, err := o.EnsureNode(ctx, testCaller, p); !errors.Is(err, ErrNoNodeID) ||
		err.Error() != "画布没有返回新建节点的 ID" {
		t.Fatalf("应返回 ErrNoNodeID（文案不变）：%v", err)
	}
}

func TestLinkBatchID_长度不超过画布批次列(t *testing.T) {
	// agent:{session}:{step}:{attempt} 三个雪花号 + attempt 两位 + :link 的上限。
	got := LinkBatchID("agent:9223372036854775807:9223372036854775807:99")
	if len(got) > 64 {
		t.Errorf("canvas_op_log.batch_id 是 varchar(64)，派生后 %d 字符：%s", len(got), got)
	}
}
