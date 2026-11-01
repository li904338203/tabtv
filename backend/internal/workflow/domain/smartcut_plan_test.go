package domain

import (
	"strings"
	"testing"
)

// 整图里的智能剪辑（docs/agent/smartcut SCT-09，SC-AC-48）：导出过且没过期 → skip、0 积分、下游拿成片；
// 没导出 → blocked「智能剪辑还没有导出成片」；导出后又改过 → blocked「剪辑已改，成片未更新」；两种 blocked 下游都跟着 blocked。

func smartcutNode(id string, mediaID int64, stale bool) Node {
	return Node{ID: id, Kind: KindSmartcut, StaticMediaID: mediaID, StaticKind: "smartcut", StaticStale: stale}
}

func TestBuildPlan_智能剪辑已导出为skip(t *testing.T) {
	g := Graph{
		Nodes: []Node{smartcutNode("sc", 8001, false), n("vid", KindVideo, "video2video")},
		Edges: []Edge{e("1", "sc", "vid", 1)},
	}
	p := BuildPlan(PlanInput{Graph: g, Quotes: quotes("vid")})
	sc := actionOf(p, "sc")
	if sc.Action != ActionSkip || sc.Credits != 0 || sc.StaticMediaID != 8001 || sc.Reason != "智能剪辑已导出，直接使用成片" {
		t.Fatalf("导出过的智能剪辑应 skip、0 积分并带上成片，得到 %+v", sc)
	}
	if got := actionOf(p, "vid"); got.Action != ActionRun {
		t.Fatalf("下游应 run，得到 %s（%s）", got.Action, got.Reason)
	}
}

func TestBuildPlan_智能剪辑没导出或已过期都blocked(t *testing.T) {
	for _, c := range []struct {
		name   string
		node   Node
		reason string
	}{
		{"没导出", smartcutNode("sc", 0, false), "智能剪辑还没有导出成片"},
		{"导出后又改过", smartcutNode("sc", 0, true), "剪辑已改，成片未更新"},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := Graph{Nodes: []Node{c.node, n("vid", KindVideo, "video2video")}, Edges: []Edge{e("1", "sc", "vid", 1)}}
			p := BuildPlan(PlanInput{Graph: g, Quotes: quotes("vid")})
			sc := actionOf(p, "sc")
			if sc.Action != ActionBlocked || sc.Reason != c.reason || len(sc.Blockers) != 1 {
				t.Fatalf("应 blocked「%s」，得到 %+v", c.reason, sc)
			}
			vid := actionOf(p, "vid")
			if vid.Action != ActionBlocked || !strings.Contains(vid.Reason, "sc") {
				t.Fatalf("下游应 blocked 并点名智能剪辑，得到 %s（%s）", vid.Action, vid.Reason)
			}
			if p.TotalCredits != 0 {
				t.Errorf("一个都跑不了，不该报价：%d", p.TotalCredits)
			}
		})
	}
}

// SC-AC-28（后端部分）：成片发布到画布的视频节点是静态素材，配了模型也不重跑。
func TestBuildPlan_智能剪辑发布节点为静态素材(t *testing.T) {
	pub := n("pub", KindVideo, "text2video")
	pub.StaticMediaID, pub.StaticKind = 8002, "smartcut_output"
	g := Graph{Nodes: []Node{pub, n("txt", KindText, "video2prompt")}, Edges: []Edge{e("1", "pub", "txt", 1)}}
	p := BuildPlan(PlanInput{Graph: g, Quotes: quotes("pub", "txt"), Force: true})
	got := actionOf(p, "pub")
	if got.Action != ActionSkip || got.StaticMediaID != 8002 || got.Reason != "静态素材：智能剪辑成片" {
		t.Fatalf("发布节点应 skip 并说明是智能剪辑成片，得到 %+v", got)
	}
}
