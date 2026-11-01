package app

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/workflow/domain"
	"github.com/li904338203/tabtv/backend/internal/workflow/repo"
)

// 整图里的 3D 导演台（DRT-06）：解析出静态素材、按媒体算下游指纹、派发时改写成 reference 输入。

const directorCanvas = `{"nodes":[
	{"id":"dir","type":"director","params":{"sceneId":"11","outputMediaId":"%s"}},
	{"id":"pub","type":"image","params":{"model":"m","directorSource":{"kind":"preview","sceneId":"11","mediaId":"8002"}}},
	{"id":"img","type":"image","params":{"model":"m"}},
	{"id":"bad","type":"director","params":{"outputMediaId":"abc"}}
],"edges":[
	{"id":"e1","source":"dir","target":"img","order":1},
	{"id":"e2","source":"pub","target":"img","order":2}
]}`

func parseDirector(t *testing.T, outputMediaID string) (map[string]domain.Node, *fakeOps) {
	t.Helper()
	ops := &fakeOps{}
	s := New()
	s.SetOperations(ops)
	raw := json.RawMessage(fmt.Sprintf(directorCanvas, outputMediaID))
	g, _, err := s.parseGraph(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]domain.Node{}
	for _, n := range g.Nodes {
		out[n.ID] = n
	}
	return out, ops
}

func TestParseGraph_读取导演台与发布节点的媒体(t *testing.T) {
	nodes, ops := parseDirector(t, "8001")
	if n := nodes["dir"]; n.StaticMediaID != 8001 || n.StaticKind != "capture" || n.Operation != "" || n.ModelCode != "" {
		t.Fatalf("导演台：媒体取 outputMediaId，不判操作码也不读模型，得到 %+v", n)
	}
	if n := nodes["pub"]; n.StaticMediaID != 8002 || n.StaticKind != "preview" {
		t.Fatalf("发布节点：媒体取 directorSource.mediaId，种类取 directorSource.kind，得到 %+v", n)
	}
	if n := nodes["bad"]; n.StaticMediaID != 0 {
		t.Fatalf("转不成正整数的媒体 ID 当成没有截图，得到 %d", n.StaticMediaID)
	}
	if n := nodes["img"]; n.StaticMediaID != 0 {
		t.Fatalf("普通图片节点不是静态素材，得到 %d", n.StaticMediaID)
	}
	for _, k := range ops.asked {
		if k == "director" {
			t.Error("不该为导演台判定操作码")
		}
	}
	// 声明的输入仍指向上游节点 ID（派发时再改写形态），不是 media:xx。
	var decl []map[string]any
	if err := json.Unmarshal(nodes["img"].Inputs, &decl); err != nil || len(decl) != 2 {
		t.Fatalf("下游应有两条声明输入：%s %v", nodes["img"].Inputs, err)
	}
	if decl[0]["sourceNodeId"] != "dir" || decl[1]["sourceNodeId"] != "pub" {
		t.Fatalf("声明输入的 sourceNodeId 应是上游节点 ID，得到 %v", decl)
	}
}

func TestParseGraph_换截图下游指纹变化(t *testing.T) {
	before, _ := parseDirector(t, "8001")
	after, _ := parseDirector(t, "8009")
	same, _ := parseDirector(t, "8001")
	if before["img"].Fingerprint == after["img"].Fingerprint {
		t.Fatal("换了导演台截图，下游的复用指纹必须变（否则会复用旧截图出的图）")
	}
	if before["img"].Fingerprint != same["img"].Fingerprint {
		t.Fatal("截图没变，下游指纹应稳定")
	}
}

func TestRoleOf_导演台算图片(t *testing.T) {
	cases := map[domain.NodeKind]string{
		domain.KindDirector: "image", domain.KindImage: "image", domain.KindVideo: "video",
		domain.KindAudio: "audio", domain.KindText: "text", domain.KindScript: "text",
	}
	for k, want := range cases {
		if got := roleOf(k); got != want {
			t.Errorf("roleOf(%s) = %s，期望 %s", k, got, want)
		}
	}
}

// fakeRuns 只实现派发要用的 ListNodes；其余方法被调到就 panic（嵌入 nil 接口）。
type fakeRuns struct {
	Runs
	nodes []repo.NodeRun
}

func (f fakeRuns) ListNodes(context.Context, int64) ([]repo.NodeRun, error) { return f.nodes, nil }

func TestResolveInputs_静态素材改写为reference(t *testing.T) {
	s := New()
	s.SetRuns(fakeRuns{nodes: []repo.NodeRun{{NodeID: "gen", GenerationID: 555}}})
	snap, _ := json.Marshal(domain.Plan{Nodes: []domain.PlanNode{
		{ID: "dir", Kind: domain.KindDirector, Action: domain.ActionSkip, StaticMediaID: 8001},
		{ID: "gen", Kind: domain.KindImage, Action: domain.ActionRun},
	}})
	run := repo.Run{ID: 1, GraphSnapshot: snap}
	out := s.resolveInputs(context.Background(), run, "img", json.RawMessage(`[
		{"order":1,"sourceNodeId":"dir","kind":"generation"},
		{"order":2,"sourceNodeId":"gen","kind":"generation"}]`))
	var refs []map[string]any
	if err := json.Unmarshal(out, &refs); err != nil || len(refs) != 2 {
		t.Fatalf("应有两条输入：%s %v", out, err)
	}
	if refs[0]["kind"] != "reference" || refs[0]["referenceId"] != "8001" ||
		refs[0]["sourceNodeId"] != "dir" || refs[0]["order"] != float64(1) {
		t.Fatalf("导演台输入应改写为 {kind:reference, referenceId, sourceNodeId, order}，得到 %v", refs[0])
	}
	if _, has := refs[0]["generationId"]; has {
		t.Error("静态素材没有任务，不该带 generationId")
	}
	if refs[1]["generationId"] != "555" {
		t.Errorf("普通上游照旧换成 generationId，得到 %v", refs[1])
	}
}

// 快照里的媒体不随画布变化：运行中途重新截图不影响这一次运行。
func TestPlan_快照带上静态素材的媒体(t *testing.T) {
	ops := &fakeOps{}
	s := New()
	s.SetOperations(ops)
	g, _, err := s.parseGraph(context.Background(), json.RawMessage(fmt.Sprintf(directorCanvas, "8001")))
	if err != nil {
		t.Fatal(err)
	}
	p := domain.BuildPlan(domain.PlanInput{Graph: g, Targets: []string{"img"}})
	raw, _ := json.Marshal(p)
	var back domain.Plan
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	for _, pn := range back.Nodes {
		if pn.ID == "dir" && pn.StaticMediaID != 8001 {
			t.Fatalf("快照里导演台应带媒体 8001，得到 %d", pn.StaticMediaID)
		}
	}
}
