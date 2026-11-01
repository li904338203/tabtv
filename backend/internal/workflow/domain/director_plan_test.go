package domain

import (
	"strings"
	"testing"
)

// 整图里的 3D 导演台（DRT-06，DRD-19）：导演台与发布出来的截图节点是**静态素材**——产出就是那张图，
// skip、0 积分、对下游算满足；导演台还没有截图时 blocked，下游跟着 blocked。

func directorNode(id string, mediaID int64) Node {
	return Node{ID: id, Kind: KindDirector, StaticMediaID: mediaID, StaticKind: "capture"}
}

func TestBuildPlan_导演台有截图为skip且不阻塞下游(t *testing.T) {
	g := Graph{
		Nodes: []Node{directorNode("dir", 9001), n("img", KindImage, "img2img")},
		Edges: []Edge{e("1", "dir", "img", 1)},
	}
	p := BuildPlan(PlanInput{Graph: g, Quotes: quotes("img")})
	dir := actionOf(p, "dir")
	if dir.Action != ActionSkip || dir.Credits != 0 || dir.StaticMediaID != 9001 {
		t.Fatalf("有截图的导演台应 skip、0 积分并带上媒体，得到 %+v", dir)
	}
	if !strings.Contains(dir.Reason, "导演台截图") {
		t.Errorf("原因要说清是导演台截图：%q", dir.Reason)
	}
	if got := actionOf(p, "img"); got.Action != ActionRun {
		t.Fatalf("下游不该被导演台阻塞，得到 %s（%s）", got.Action, got.Reason)
	}
	if p.TotalCredits != 5 || p.SkipCount != 1 || p.RunCount != 1 {
		t.Errorf("只有下游扣费：%+v", p)
	}
}

func TestBuildPlan_导演台无截图blocked(t *testing.T) {
	g := Graph{
		Nodes: []Node{directorNode("dir", 0), n("img", KindImage, "img2img")},
		Edges: []Edge{e("1", "dir", "img", 1)},
	}
	p := BuildPlan(PlanInput{Graph: g, Quotes: quotes("img")})
	dir := actionOf(p, "dir")
	if dir.Action != ActionBlocked || dir.Reason != "导演台还没有截图" || len(dir.Blockers) != 1 {
		t.Fatalf("没有截图的导演台应 blocked「导演台还没有截图」，得到 %+v", dir)
	}
	img := actionOf(p, "img")
	if img.Action != ActionBlocked || !strings.Contains(img.Reason, "dir") {
		t.Fatalf("下游应 blocked 并点名这个导演台，得到 %s（%s）", img.Action, img.Reason)
	}
}

func TestBuildPlan_发布节点为静态素材(t *testing.T) {
	g := Graph{
		Nodes: []Node{
			{ID: "pub", Kind: KindVideo, StaticMediaID: 7002, StaticKind: "preview"},
			n("vid", KindVideo, "ref2video"),
		},
		Edges: []Edge{e("1", "pub", "vid", 1)},
	}
	p := BuildPlan(PlanInput{Graph: g, Quotes: quotes("vid")})
	pub := actionOf(p, "pub")
	if pub.Action != ActionSkip || pub.StaticMediaID != 7002 || !strings.Contains(pub.Reason, "预演视频") {
		t.Fatalf("发布的预演视频应 skip 并说明是预演视频，得到 %+v", pub)
	}
	if got := actionOf(p, "vid").Action; got != ActionRun {
		t.Fatalf("下游应 run，得到 %s", got)
	}
}

// ⚠️ 发布节点是普通的图片节点，用户可能给它配了模型：照普通节点处理的话会被重新生成一次，
// 用模型的图顶掉截图，而钱是白花的。
func TestBuildPlan_发布节点配了模型也不重跑(t *testing.T) {
	pub := n("pub", KindImage, "text2img")
	pub.StaticMediaID, pub.StaticKind = 7001, "capture"
	p := BuildPlan(PlanInput{Graph: Graph{Nodes: []Node{pub}}, Quotes: quotes("pub"), Force: true})
	got := actionOf(p, "pub")
	if got.Action != ActionSkip || got.Credits != 0 {
		t.Fatalf("配了模型的发布节点也只能 skip（连强制重跑都不行），得到 %+v", got)
	}
	if p.Executable() {
		t.Error("一个要跑的节点都没有时不可执行")
	}
}

// 静态素材上游不算「上游没产出」——即使处置顺序将来被调整。
func TestFailedUpstreams_静态素材不算失败(t *testing.T) {
	idx := map[string]Node{"dir": directorNode("dir", 1)}
	if got := failedUpstreams([]string{"dir"}, map[string]Action{"dir": ActionBlocked}, idx); len(got) != 0 {
		t.Fatalf("静态素材上游不能被算成失败，得到 %v", got)
	}
}
