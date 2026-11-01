package domain

import (
	"strings"
	"testing"
)

// 建图辅助：n 造节点，e 造边。
func n(id string, kind NodeKind, op string) Node {
	return Node{ID: id, Kind: kind, Operation: op, ModelCode: "m1"}
}

func done(id string, kind NodeKind, op, fp string) Node {
	return Node{ID: id, Kind: kind, Operation: op, ModelCode: "m1",
		HasSuccess: true, Fingerprint: fp, ResultFingerprint: fp}
}

func e(id, src, dst string, order int) Edge {
	return Edge{ID: id, Source: src, Target: dst, Order: order}
}

func quotes(ids ...string) map[string]Quote {
	m := map[string]Quote{}
	for _, id := range ids {
		m[id] = Quote{Credits: 5, EstimatedSeconds: 10}
	}
	return m
}

func actionOf(p Plan, id string) PlanNode {
	for _, x := range p.Nodes {
		if x.ID == id {
			return x
		}
	}
	return PlanNode{}
}

// ---------------------------------------------------------------- 环检测

// ⚠️ 只说"图里有环"在 100 个节点的画布上等于没说 —— 用户没有任何办法找到它。
func TestDetectCycles_报出环上的具体节点(t *testing.T) {
	g := Graph{
		Nodes: []Node{n("a", KindText, "text2text"), n("b", KindImage, "text2img"),
			n("c", KindVideo, "img2video")},
		Edges: []Edge{e("1", "a", "b", 1), e("2", "b", "c", 1), e("3", "c", "a", 1)},
	}
	cycles := DetectCycles(g)
	if len(cycles) != 1 {
		t.Fatalf("应找到 1 个环，实际 %d", len(cycles))
	}
	if len(cycles[0]) != 3 {
		t.Fatalf("环上应有 3 个节点: %v", cycles[0])
	}
	got := strings.Join(cycles[0], ",")
	for _, id := range []string{"a", "b", "c"} {
		if !strings.Contains(got, id) {
			t.Fatalf("环里应包含 %s: %v", id, cycles[0])
		}
	}
}

// ⚠️ 只报第一个的话用户修完再跑，又冒出来一个。
func TestDetectCycles_多个独立环一次全报(t *testing.T) {
	g := Graph{
		Nodes: []Node{
			n("a", KindText, "t"), n("b", KindText, "t"),
			n("x", KindText, "t"), n("y", KindText, "t"),
		},
		Edges: []Edge{
			e("1", "a", "b", 1), e("2", "b", "a", 1),
			e("3", "x", "y", 1), e("4", "y", "x", 1),
		},
	}
	if got := len(DetectCycles(g)); got != 2 {
		t.Fatalf("应找到 2 个环，实际 %d", got)
	}
}

func TestDetectCycles_无环时为空(t *testing.T) {
	g := Graph{
		Nodes: []Node{n("a", KindText, "t"), n("b", KindImage, "i")},
		Edges: []Edge{e("1", "a", "b", 1)},
	}
	if got := DetectCycles(g); len(got) != 0 {
		t.Fatalf("无环图不该报环: %v", got)
	}
}

// 菱形（a→b, a→c, b→d, c→d）不是环，常被朴素实现误判。
func TestDetectCycles_菱形不是环(t *testing.T) {
	g := Graph{
		Nodes: []Node{n("a", KindText, "t"), n("b", KindImage, "i"),
			n("c", KindImage, "i"), n("d", KindVideo, "v")},
		Edges: []Edge{e("1", "a", "b", 1), e("2", "a", "c", 1),
			e("3", "b", "d", 1), e("4", "c", "d", 2)},
	}
	if got := DetectCycles(g); len(got) != 0 {
		t.Fatalf("菱形不是环: %v", got)
	}
}

func TestValidate_一次报完所有问题(t *testing.T) {
	// ⚠️ 遇到第一个就返回的话，用户改一个跑一次，十个问题来回十趟。
	g := Graph{
		Nodes: []Node{n("a", KindText, "t"), n("b", KindText, "t")},
		Edges: []Edge{e("1", "a", "b", 1), e("2", "b", "a", 1),
			e("3", "a", "ghost", 1)},
	}
	issues := Validate(g, []string{"nowhere"})
	codes := map[IssueCode]int{}
	for _, i := range issues {
		codes[i.Code]++
	}
	if codes[IssueCycle] == 0 || codes[IssueDangling] == 0 || codes[IssueUnknownNode] == 0 {
		t.Fatalf("三类问题都应报出来: %+v", issues)
	}
}

// ---------------------------------------------------------------- 子图

// ⚠️ 取后代的话跑的是"X 之后的东西"，而 X 自己还没有产出。
func TestAncestors_取祖先闭包不是后代(t *testing.T) {
	g := Graph{
		Nodes: []Node{n("a", KindText, "t"), n("b", KindImage, "i"),
			n("c", KindVideo, "v"), n("d", KindVideo, "v")},
		Edges: []Edge{e("1", "a", "b", 1), e("2", "b", "c", 1), e("3", "c", "d", 1)},
	}
	got := Ancestors(g, []string{"c"})
	for _, want := range []string{"a", "b", "c"} {
		if !got[want] {
			t.Fatalf("%s 应在祖先闭包里: %v", want, got)
		}
	}
	if got["d"] {
		t.Fatal("d 是后代，不该进来 —— 跑它对产出 c 毫无帮助，却要扣钱")
	}
}

// ⚠️ 冻结的来源（智能剪辑、导演台、发布出来的静态素材）之上的节点不进子图：它们的产出不因这次运行而变，
// 拉进来只会多扣钱（SCT-42 实测：只跑智能剪辑的下游，整条素材链跟着重跑）。整图不受影响。
func TestAncestors_走到冻结的来源就停(t *testing.T) {
	pub := n("pub", KindVideo, "")
	pub.StaticMediaID = 9
	g := Graph{
		Nodes: []Node{n("a", KindVideo, "v"), n("sc", KindSmartcut, ""), n("c", KindVideo, "v"),
			n("i", KindImage, "i"), n("dir", KindDirector, ""), n("d", KindImage, "i"),
			n("x", KindText, "t"), pub, n("f", KindVideo, "v")},
		Edges: []Edge{e("1", "a", "sc", 1), e("2", "sc", "c", 1), e("3", "i", "dir", 1), e("4", "dir", "d", 1),
			e("5", "x", "pub", 1), e("6", "pub", "f", 1)},
	}
	for _, c := range []struct {
		target     string
		want, skip []string
	}{
		{"c", []string{"sc", "c"}, []string{"a"}},
		{"d", []string{"dir", "d"}, []string{"i"}},
		{"f", []string{"pub", "f"}, []string{"x"}},
		{"sc", []string{"sc"}, []string{"a"}},
	} {
		got := Ancestors(g, []string{c.target})
		for _, id := range c.want {
			if !got[id] {
				t.Fatalf("目标 %s：%s 应在子图里: %v", c.target, id, got)
			}
		}
		for _, id := range c.skip {
			if got[id] {
				t.Fatalf("目标 %s：%s 在冻结的来源之上，不该进子图: %v", c.target, id, got)
			}
		}
	}
	// 未导出的智能剪辑同样不往上走：整图不替用户导出，跑它的上游补不出成片。
	if got := Ancestors(Graph{Nodes: []Node{n("a", KindVideo, "v"), n("sc", KindSmartcut, "")},
		Edges: []Edge{e("1", "a", "sc", 1)}}, []string{"sc"}); got["a"] {
		t.Fatalf("没导出的智能剪辑也不该把上游拉进来: %v", got)
	}
	if got := Ancestors(g, nil); len(got) != len(g.Nodes) {
		t.Fatalf("整图应取全部节点: %v", got)
	}
}

func TestAncestors_空目标表示整图(t *testing.T) {
	g := Graph{Nodes: []Node{n("a", KindText, "t"), n("b", KindImage, "i")}}
	if got := Ancestors(g, nil); len(got) != 2 {
		t.Fatalf("空目标应取整图: %v", got)
	}
}

// ---------------------------------------------------------------- 分层

// ⚠️ 给线性序的话调度器只能一个一个跑，一张两条支线的图要跑两倍时间。
func TestTopoLevels_独立分支分在同一层(t *testing.T) {
	g := Graph{
		Nodes: []Node{n("a", KindText, "t"), n("b", KindImage, "i"),
			n("c", KindImage, "i"), n("d", KindVideo, "v")},
		Edges: []Edge{e("1", "a", "b", 1), e("2", "a", "c", 1),
			e("3", "b", "d", 1), e("4", "c", "d", 2)},
	}
	levels := TopoLevels(g, Ancestors(g, nil))
	if len(levels) != 3 {
		t.Fatalf("应分 3 层: %v", levels)
	}
	if len(levels[1]) != 2 {
		t.Fatalf("b 与 c 互不依赖，应同层并行: %v", levels)
	}
}

func TestTopoLevels_有环返回nil(t *testing.T) {
	g := Graph{
		Nodes: []Node{n("a", KindText, "t"), n("b", KindText, "t")},
		Edges: []Edge{e("1", "a", "b", 1), e("2", "b", "a", 1)},
	}
	if got := TopoLevels(g, Ancestors(g, nil)); got != nil {
		t.Fatalf("有环应返回 nil: %v", got)
	}
}

// ---------------------------------------------------------------- 计划

// ⚠️ 这是整个调度里最容易漏的一条：素材节点没有 operation、永远跑不出东西，
// 但它有出边。一视同仁地套"上游没成功就阻塞"，
// **任何挂了风格节点的画布都跑不起来**。
func TestBuildPlan_素材节点跳过且不阻塞下游(t *testing.T) {
	g := Graph{
		Nodes: []Node{
			{ID: "style1", Kind: KindStyle}, // 没有 operation
			n("txt", KindText, "text2text"),
			n("img", KindImage, "text2img"),
		},
		Edges: []Edge{e("1", "txt", "img", 1), e("2", "style1", "img", 2)},
	}
	p := BuildPlan(PlanInput{Graph: g, Quotes: quotes("txt", "img")})
	if len(p.Issues) != 0 {
		t.Fatalf("挂了风格节点不该是错误: %+v", p.Issues)
	}
	if got := actionOf(p, "style1").Action; got != ActionSkip {
		t.Fatalf("素材节点应 skip，实际 %s", got)
	}
	if got := actionOf(p, "img").Action; got != ActionRun {
		t.Fatalf("下游不该被素材节点阻塞，实际 %s（%s）",
			got, actionOf(p, "img").Reason)
	}
	if p.SkipCount != 1 || p.RunCount != 2 {
		t.Fatalf("计数不对: skip=%d run=%d", p.SkipCount, p.RunCount)
	}
}

// ⚠️ 只看有没有产出的话，用户改完提示词再点运行，拿回来的还是旧图 ——
// 而界面显示"已完成"。
func TestBuildPlan_指纹一致才复用(t *testing.T) {
	g := Graph{Nodes: []Node{done("img", KindImage, "text2img", "fp-1")}}
	p := BuildPlan(PlanInput{Graph: g, Quotes: quotes("img")})
	if got := actionOf(p, "img").Action; got != ActionReuse {
		t.Fatalf("指纹一致应复用，实际 %s", got)
	}
	if p.TotalCredits != 0 {
		t.Fatalf("复用不该扣费，实际 %d", p.TotalCredits)
	}

	// 改了参数 → 指纹变 → 必须重跑。
	g.Nodes[0].Fingerprint = "fp-2"
	p2 := BuildPlan(PlanInput{Graph: g, Quotes: quotes("img")})
	if got := actionOf(p2, "img").Action; got != ActionRun {
		t.Fatalf("指纹变了应重跑，实际 %s", got)
	}
	if !strings.Contains(actionOf(p2, "img").Reason, "变了") {
		t.Fatalf("要说清为什么重跑: %s", actionOf(p2, "img").Reason)
	}
}

// ⚠️ 宁可多跑一次，也不能把一张对不上的图当成本次结果。
func TestBuildPlan_没有指纹的老数据不复用(t *testing.T) {
	g := Graph{Nodes: []Node{{ID: "img", Kind: KindImage, Operation: "text2img",
		ModelCode: "m1", HasSuccess: true}}} // 两个指纹都为空
	p := BuildPlan(PlanInput{Graph: g, Quotes: quotes("img")})
	if got := actionOf(p, "img").Action; got != ActionRun {
		t.Fatalf("没有指纹不该复用，实际 %s", got)
	}
}

func TestBuildPlan_force时连可复用的也重跑(t *testing.T) {
	g := Graph{Nodes: []Node{done("img", KindImage, "text2img", "fp-1")}}
	p := BuildPlan(PlanInput{Graph: g, Quotes: quotes("img"), Force: true})
	if got := actionOf(p, "img").Action; got != ActionRun {
		t.Fatalf("force 应重跑，实际 %s", got)
	}
	if p.TotalCredits != 5 {
		t.Fatalf("重跑要计费，实际 %d", p.TotalCredits)
	}
}

// ⚠️ skip 是"不需要跑"，blocked 是"想跑跑不了"。混成一个状态的话，
// 界面无法区分"这条风格线是正常的"和"这一支因为上游失败瘫了"。
func TestBuildPlan_上游不可用时下游blocked(t *testing.T) {
	g := Graph{
		Nodes: []Node{n("a", KindImage, "text2img"), n("b", KindVideo, "img2video")},
		Edges: []Edge{e("1", "a", "b", 1)},
	}
	// a 报价被拦（模型停用之类），b 因此跑不了。
	q := quotes("b")
	q["a"] = Quote{Blockers: []string{"模型已停用", "没有配价"}}
	p := BuildPlan(PlanInput{Graph: g, Quotes: q})

	pa := actionOf(p, "a")
	if pa.Action != ActionBlocked {
		t.Fatalf("a 应 blocked，实际 %s", pa.Action)
	}
	// ⚠️ 要给**具体原因**：只说"不可用"的话用户不知道去后台改哪一项。
	if len(pa.Blockers) != 2 {
		t.Fatalf("要带上具体阻塞原因: %+v", pa.Blockers)
	}
	pb := actionOf(p, "b")
	if pb.Action != ActionBlocked {
		t.Fatalf("b 应被上游阻塞，实际 %s", pb.Action)
	}
	if !strings.Contains(pb.Reason, "a") {
		t.Fatalf("要说清是哪个上游卡住了: %s", pb.Reason)
	}
	if p.TotalCredits != 0 {
		t.Fatalf("blocked 的节点不该计费，实际 %d", p.TotalCredits)
	}
}

func TestBuildPlan_没选生成方式是blocked不是错误(t *testing.T) {
	// 可修复的配置问题：其余分支照常预览，不该让整张图不可预览。
	g := Graph{
		Nodes: []Node{{ID: "img", Kind: KindImage}, n("txt", KindText, "text2text")},
	}
	p := BuildPlan(PlanInput{Graph: g, Quotes: quotes("txt")})
	if len(p.Issues) != 0 {
		t.Fatalf("不该升级成整图错误: %+v", p.Issues)
	}
	if got := actionOf(p, "img").Action; got != ActionBlocked {
		t.Fatalf("应 blocked，实际 %s", got)
	}
	if got := actionOf(p, "txt").Action; got != ActionRun {
		t.Fatalf("其余节点照常预览，实际 %s", got)
	}
}

// ⚠️ 所有节点耗时之和会把一张两条并行支线的图吹成两倍时间，
// 用户照着那个数去安排等待。
func TestBuildPlan_耗时按关键路径不是求和(t *testing.T) {
	g := Graph{
		Nodes: []Node{n("a", KindText, "t"), n("b", KindImage, "i"),
			n("c", KindImage, "i")},
		Edges: []Edge{e("1", "a", "b", 1), e("2", "a", "c", 1)},
	}
	q := map[string]Quote{
		"a": {Credits: 1, EstimatedSeconds: 5},
		"b": {Credits: 1, EstimatedSeconds: 30},
		"c": {Credits: 1, EstimatedSeconds: 20},
	}
	p := BuildPlan(PlanInput{Graph: g, Quotes: q})
	// 第 0 层 5 秒 + 第 1 层 max(30,20)=30 秒 = 35 秒，而不是 55 秒。
	if p.EstimatedSeconds != 35 {
		t.Fatalf("应按关键路径算 35 秒，实际 %d", p.EstimatedSeconds)
	}
	if p.TotalCredits != 3 {
		t.Fatalf("费用是求和，实际 %d", p.TotalCredits)
	}
}

// ⚠️ NEW-01：校验失败不调用模型或扣分 —— 有 Issue 时连报价都不该被读。
func TestBuildPlan_有问题时不产出任何可执行项(t *testing.T) {
	g := Graph{
		Nodes: []Node{n("a", KindText, "t"), n("b", KindText, "t")},
		Edges: []Edge{e("1", "a", "b", 1), e("2", "b", "a", 1)},
	}
	p := BuildPlan(PlanInput{Graph: g, Quotes: quotes("a", "b")})
	if len(p.Issues) == 0 {
		t.Fatal("有环应报问题")
	}
	if p.RunCount != 0 || p.TotalCredits != 0 || len(p.Nodes) != 0 {
		t.Fatalf("有问题时不该产出可执行项: run=%d credits=%d nodes=%d",
			p.RunCount, p.TotalCredits, len(p.Nodes))
	}
	if p.Executable() {
		t.Fatal("有问题时不可执行")
	}
}

func TestPlan_一个都不跑时不可执行(t *testing.T) {
	// ⚠️ 让用户点一个什么都不会发生的按钮，比禁用它更糟。
	g := Graph{Nodes: []Node{done("img", KindImage, "text2img", "fp-1")}}
	p := BuildPlan(PlanInput{Graph: g, Quotes: quotes("img")})
	if p.Executable() {
		t.Fatal("全部可复用时不该可执行")
	}
}

// ⚠️ 用户选了 12 个节点只扣 3 个的钱，不解释的话他会以为漏跑了，
// 然后去点"强制重新生成" —— 那才是真花钱。
func TestPlan_摘要说出复用与跳过的条数(t *testing.T) {
	g := Graph{
		Nodes: []Node{
			{ID: "style1", Kind: KindStyle},
			done("img", KindImage, "text2img", "fp-1"),
			n("vid", KindVideo, "img2video"),
		},
		Edges: []Edge{e("1", "img", "vid", 1), e("2", "style1", "vid", 2)},
	}
	s := BuildPlan(PlanInput{Graph: g, Quotes: quotes("vid")}).Summary()
	for _, want := range []string{"将执行 1 个节点", "可直接复用", "无需执行"} {
		if !strings.Contains(s, want) {
			t.Fatalf("摘要应包含 %q：%s", want, s)
		}
	}
}

func TestBuildPlan_只跑目标子图(t *testing.T) {
	g := Graph{
		Nodes: []Node{n("a", KindText, "t"), n("b", KindImage, "i"),
			n("c", KindVideo, "v"), n("far", KindVideo, "v")},
		Edges: []Edge{e("1", "a", "b", 1), e("2", "b", "c", 1), e("3", "c", "far", 1)},
	}
	p := BuildPlan(PlanInput{Graph: g, Targets: []string{"b"},
		Quotes: quotes("a", "b", "c", "far")})
	if len(p.Nodes) != 2 {
		t.Fatalf("只应包含 a 与 b，实际 %d 个", len(p.Nodes))
	}
	if p.TotalCredits != 10 {
		t.Fatalf("只为子图计费，实际 %d", p.TotalCredits)
	}
}

// ---------------------------------------------------------------- 指纹

// ⚠️ 前端两次发来的同一份参数可能键序不同。直接对原始字节哈希的话，
// 同一份设置会算出两个指纹，复用永远不成立 —— 用户每跑一次图就多花一次钱，
// 而系统没有任何报错。
func TestFingerprint_键序不同结果相同(t *testing.T) {
	a := Fingerprint("text2img", "m1", []byte(`{"a":1,"b":{"x":2,"y":3}}`), nil)
	b := Fingerprint("text2img", "m1", []byte(`{"b":{"y":3,"x":2},"a":1}`), nil)
	if a != b {
		t.Fatalf("键序不该影响指纹: %s vs %s", a, b)
	}
}

func TestFingerprint_改任意一项都要变(t *testing.T) {
	base := Fingerprint("text2img", "m1", []byte(`{"prompt":"猫"}`),
		[]InputRef{{Order: 1, Kind: "generation", Ref: "n1"}})
	cases := map[string]string{
		"换操作": Fingerprint("img2img", "m1", []byte(`{"prompt":"猫"}`),
			[]InputRef{{Order: 1, Kind: "generation", Ref: "n1"}}),
		"换模型": Fingerprint("text2img", "m2", []byte(`{"prompt":"猫"}`),
			[]InputRef{{Order: 1, Kind: "generation", Ref: "n1"}}),
		"改参数": Fingerprint("text2img", "m1", []byte(`{"prompt":"狗"}`),
			[]InputRef{{Order: 1, Kind: "generation", Ref: "n1"}}),
		"换输入": Fingerprint("text2img", "m1", []byte(`{"prompt":"猫"}`),
			[]InputRef{{Order: 1, Kind: "generation", Ref: "n2"}}),
		"加输入": Fingerprint("text2img", "m1", []byte(`{"prompt":"猫"}`),
			[]InputRef{{Order: 1, Kind: "generation", Ref: "n1"},
				{Order: 2, Kind: "reference", Ref: "r1"}}),
	}
	for name, got := range cases {
		if got == base {
			t.Errorf("%s 之后指纹应当改变", name)
		}
	}
}

// ⚠️ 顺序即首尾帧与参考图的锚定顺序，换了顺序就是另一次生成。
func TestFingerprint_输入顺序参与计算(t *testing.T) {
	a := Fingerprint("first_last_frame", "m1", nil, []InputRef{
		{Order: 1, Kind: "generation", Ref: "n1"},
		{Order: 2, Kind: "generation", Ref: "n2"},
	})
	b := Fingerprint("first_last_frame", "m1", nil, []InputRef{
		{Order: 1, Kind: "generation", Ref: "n2"},
		{Order: 2, Kind: "generation", Ref: "n1"},
	})
	if a == b {
		t.Fatal("首尾帧对调是另一次生成，指纹必须不同")
	}
}

// 同一批输入以不同顺序传进来（调用方没排序）不该改变结果。
func TestFingerprint_调用顺序无关只看order(t *testing.T) {
	a := Fingerprint("ref2video", "m1", nil, []InputRef{
		{Order: 1, Kind: "generation", Ref: "n1"},
		{Order: 2, Kind: "reference", Ref: "r1"},
	})
	b := Fingerprint("ref2video", "m1", nil, []InputRef{
		{Order: 2, Kind: "reference", Ref: "r1"},
		{Order: 1, Kind: "generation", Ref: "n1"},
	})
	if a != b {
		t.Fatal("同一份输入换个传参顺序不该算出不同指纹")
	}
}

// ⚠️ 返回空串会让两份不同的坏参数算出同一个指纹，于是复用一张完全不相干的图。
func TestFingerprint_坏参数不塌缩成同一个值(t *testing.T) {
	a := Fingerprint("text2img", "m1", []byte(`{坏的`), nil)
	b := Fingerprint("text2img", "m1", []byte(`{也是坏的`), nil)
	if a == b {
		t.Fatal("两份不同的坏参数不该算出同一个指纹")
	}
}

func TestFingerprint_空参数稳定(t *testing.T) {
	if Fingerprint("t", "m", nil, nil) != Fingerprint("t", "m", []byte(`{}`), nil) {
		t.Fatal("空参数与空对象应当一致")
	}
}

// ⚠️ 文本节点有两种用法：配了模型的会真跑，**没配模型的是用户手打的提示词**。
// 不认这一条的后果是：每一张带手打提示词的画布都跑不起来 ——
// 那个文本节点因为"没有模型"被判 blocked，下游跟着 blocked。
func TestBuildPlan_没配模型的文本节点是静态输入(t *testing.T) {
	g := Graph{
		Nodes: []Node{
			{ID: "prompt", Kind: KindText}, // 用户手打的一段词，没有模型
			n("img", KindImage, "text2img"),
		},
		Edges: []Edge{e("1", "prompt", "img", 1)},
	}
	p := BuildPlan(PlanInput{Graph: g, Quotes: quotes("img")})
	if got := actionOf(p, "prompt").Action; got != ActionSkip {
		t.Fatalf("静态文本输入应 skip，实际 %s（%s）",
			got, actionOf(p, "prompt").Reason)
	}
	if got := actionOf(p, "img").Action; got != ActionRun {
		t.Fatalf("下游不该被静态文本阻塞，实际 %s（%s）",
			got, actionOf(p, "img").Reason)
	}
}

// ⚠️ 只对文本成立：图片/视频/音频节点没有模型就是真的产不出东西。
// 悄悄跳过的话下游拿不到媒体，跑到一半才失败，而钱已经花在上游了。
func TestBuildPlan_没配模型的媒体节点仍然blocked(t *testing.T) {
	g := Graph{Nodes: []Node{
		{ID: "img", Kind: KindImage, Operation: "text2img"}, // 没有 modelCode
	}}
	p := BuildPlan(PlanInput{Graph: g})
	if got := actionOf(p, "img").Action; got != ActionBlocked {
		t.Fatalf("没配模型的图片节点应 blocked，实际 %s", got)
	}
}

// 配了模型的文本节点照常执行（text2text / 图片反推）。
func TestBuildPlan_配了模型的文本节点照常跑(t *testing.T) {
	g := Graph{Nodes: []Node{n("txt", KindText, "text2text")}}
	p := BuildPlan(PlanInput{Graph: g, Quotes: quotes("txt")})
	if got := actionOf(p, "txt").Action; got != ActionRun {
		t.Fatalf("配了模型的文本节点应 run，实际 %s", got)
	}
}
