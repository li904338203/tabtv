package app

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/workflow/domain"
	"github.com/li904338203/tabtv/backend/internal/workflow/repo"
)

// 整图里的智能剪辑（SCT-09）：成片从端口取（不读 params）；过期的不当静态素材；发布节点读 smartcutSource；
// 下游指纹按成片媒体算（换了成片判不可复用）。

const smartcutCanvas = `{"nodes":[
	{"id":"sc","type":"smartcut","params":{"timelineId":"11","outputMediaId":"999"}},
	{"id":"vid","type":"video","params":{"model":"m","mode":"video2video"}},
	{"id":"pub","type":"video","params":{"model":"m","smartcutSource":{"mediaId":"8102","timelineId":"11"}}}
],"edges":[{"id":"e1","source":"sc","target":"vid","order":1}]}`

func parseSmartcut(t *testing.T, outputs map[string]SmartcutOutput) map[string]domain.Node {
	t.Helper()
	ops := &fakeOps{}
	s := New()
	s.SetOperations(ops)
	g, _, err := s.parseGraphWith(context.Background(), json.RawMessage(smartcutCanvas), outputs)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]domain.Node{}
	for _, n := range g.Nodes {
		out[n.ID] = n
	}
	for _, k := range ops.asked {
		if k == "smartcut" {
			t.Error("不该为智能剪辑判定操作码")
		}
	}
	return out
}

func TestParseGraph_智能剪辑的成片从端口取(t *testing.T) {
	fresh := parseSmartcut(t, map[string]SmartcutOutput{"sc": {MediaID: 8101, RenderVersion: 5, CurrentVersion: 5}})
	if n := fresh["sc"]; n.StaticMediaID != 8101 || n.StaticKind != "smartcut" || n.StaticStale || n.Operation != "" {
		t.Fatalf("导出过且没过期：静态素材取端口给的成片（不读 params 里的 999），得到 %+v", n)
	}
	stale := parseSmartcut(t, map[string]SmartcutOutput{"sc": {MediaID: 8101, RenderVersion: 4, CurrentVersion: 5}})
	if n := stale["sc"]; n.StaticMediaID != 0 || !n.StaticStale {
		t.Fatalf("导出后又改过：不当静态素材并标过期，得到 %+v", n)
	}
	none := parseSmartcut(t, nil)
	if n := none["sc"]; n.StaticMediaID != 0 || n.StaticStale {
		t.Fatalf("没导出过：既不是静态素材也不算过期，得到 %+v", n)
	}
	if n := fresh["pub"]; n.StaticMediaID != 8102 || n.StaticKind != "smartcut_output" {
		t.Fatalf("发布节点读 smartcutSource.mediaId，得到 %+v", n)
	}
	// 下游指纹按成片媒体算：换了成片（同一节点），下游的指纹必须变（不能误判可复用）。
	other := parseSmartcut(t, map[string]SmartcutOutput{"sc": {MediaID: 8199, RenderVersion: 6, CurrentVersion: 6}})
	if fresh["vid"].Fingerprint == other["vid"].Fingerprint {
		t.Fatal("换了成片，下游的复用指纹应当变化")
	}
	if fresh["vid"].Fingerprint != parseSmartcut(t, map[string]SmartcutOutput{"sc": {MediaID: 8101, RenderVersion: 9, CurrentVersion: 9}})["vid"].Fingerprint {
		t.Fatal("同一份成片（只是版本号不同）下游指纹不该变")
	}
}

type fakeCanvasSource struct{ raw json.RawMessage }

func (f fakeCanvasSource) CanvasOf(context.Context, int64, int64) (json.RawMessage, error) {
	return f.raw, nil
}

type fakeSmartcutOutputs map[string]SmartcutOutput

func (f fakeSmartcutOutputs) Outputs(context.Context, int64, int64) (map[string]SmartcutOutput, error) {
	return f, nil
}

type fixedQuoter struct{}

func (fixedQuoter) QuoteNode(context.Context, QuoteAsk) (domain.Quote, error) {
	return domain.Quote{Credits: 7, EstimatedSeconds: 30}, nil
}

// SC-AC-48 的整条路：计划里智能剪辑 skip（0 积分）、下游 run；派发时下游的输入改写成 reference，指向成片媒体。
// 导出后又改过时，智能剪辑与下游都 blocked，整张图一分钱都不扣。
func TestPlan_智能剪辑成片交给下游(t *testing.T) {
	s := New()
	s.SetOperations(&fakeOps{})
	s.SetQuoter(fixedQuoter{})
	s.SetCanvas(fakeCanvasSource{json.RawMessage(smartcutCanvas)})
	s.SetSmartcutOutputs(fakeSmartcutOutputs{"sc": {MediaID: 8101, RenderVersion: 5, CurrentVersion: 5}})
	p, err := s.Plan(context.Background(), PlanInput{WorkspaceID: 1, ProjectID: 2, TargetNodeIDs: []string{"vid"}})
	if err != nil {
		t.Fatal(err)
	}
	var sc, vid domain.PlanNode
	for _, n := range p.Nodes {
		switch n.ID {
		case "sc":
			sc = n
		case "vid":
			vid = n
		}
	}
	if sc.Action != domain.ActionSkip || sc.Credits != 0 || sc.StaticMediaID != 8101 || vid.Action != domain.ActionRun {
		t.Fatalf("智能剪辑 skip、下游 run：%+v / %+v", sc, vid)
	}
	snap, _ := json.Marshal(p)
	s.SetRuns(fakeRuns{})
	refs := s.resolveInputs(context.Background(), repo.Run{ID: 1, GraphSnapshot: snap}, "vid", vid.Inputs)
	var in []map[string]any
	if err := json.Unmarshal(refs, &in); err != nil || len(in) != 1 || in[0]["kind"] != "reference" ||
		in[0]["referenceId"] != "8101" || in[0]["sourceNodeId"] != "sc" {
		t.Fatalf("下游的输入应改写成 reference → 成片 8101：%s %v", refs, err)
	}

	s.SetSmartcutOutputs(fakeSmartcutOutputs{"sc": {MediaID: 8101, RenderVersion: 4, CurrentVersion: 5}})
	p, err = s.Plan(context.Background(), PlanInput{WorkspaceID: 1, ProjectID: 2, TargetNodeIDs: []string{"vid"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range p.Nodes {
		if n.Action != domain.ActionBlocked {
			t.Fatalf("成片过期时整条支线 blocked：%s %s（%s）", n.ID, n.Action, n.Reason)
		}
	}
	if p.TotalCredits != 0 {
		t.Fatalf("一分钱都不扣：%d", p.TotalCredits)
	}
}
