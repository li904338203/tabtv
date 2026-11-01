package domain

import (
	"reflect"
	"testing"
	"time"
)

func rn(id string, status NodeStatus, deps ...string) RunNode {
	return RunNode{NodeID: id, Status: status, DependsOn: deps}
}

// ---------------------------------------------------------------- 就绪判定

func TestReadyNodes_没有依赖的先跑(t *testing.T) {
	got := ReadyNodes([]RunNode{
		rn("b", NodePending, "a"),
		rn("a", NodePending),
	})
	if !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("只有 a 就绪: %v", got)
	}
}

func TestReadyNodes_依赖全部满足才就绪(t *testing.T) {
	nodes := []RunNode{
		rn("a", NodeSuccess),
		rn("b", NodeRunning),
		rn("c", NodePending, "a", "b"),
	}
	if got := ReadyNodes(nodes); len(got) != 0 {
		t.Fatalf("b 还在跑，c 不该就绪: %v", got)
	}
	nodes[1].Status = NodeSuccess
	if got := ReadyNodes(nodes); !reflect.DeepEqual(got, []string{"c"}) {
		t.Fatalf("依赖都好了 c 应就绪: %v", got)
	}
}

// ⚠️ 素材节点、静态文本、复用上次产出的节点都落在 skipped ——
// 它们确实没跑，但下游要的东西是在的。不认这一条的话，
// 任何带风格节点或可复用节点的图都推进不下去。
func TestReadyNodes_skipped算作满足(t *testing.T) {
	got := ReadyNodes([]RunNode{
		rn("style", NodeSkipped),
		rn("reused", NodeSkipped),
		rn("img", NodePending, "style", "reused"),
	})
	if !reflect.DeepEqual(got, []string{"img"}) {
		t.Fatalf("skipped 的上游应算满足: %v", got)
	}
}

func TestReadyNodes_失败的上游不算满足(t *testing.T) {
	got := ReadyNodes([]RunNode{
		rn("a", NodeFailed),
		rn("b", NodePending, "a"),
	})
	if len(got) != 0 {
		t.Fatalf("上游失败时下游不该就绪: %v", got)
	}
}

// ⚠️ 按"未满足"处理的话，任何子图执行都会卡在第一层 ——
// 子图外的依赖要么是上一次跑出来的产出，要么根本不需要跑。
func TestReadyNodes_子图外的依赖按已满足处理(t *testing.T) {
	got := ReadyNodes([]RunNode{
		rn("b", NodePending, "outside"), // outside 不在本次运行里
	})
	if !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("子图外的依赖不该卡住本次运行: %v", got)
	}
}

// ⚠️ 按层号派发的话，同层里有一个跑得慢，另一层里本来可以开跑的节点会白等。
func TestReadyNodes_只看依赖不看层号(t *testing.T) {
	// a(成功) → b；另一支 x(还在跑) → y。b 与 y 分属不同"层"，
	// 但 b 的依赖已满足，应立刻就绪。
	got := ReadyNodes([]RunNode{
		rn("a", NodeSuccess), rn("b", NodePending, "a"),
		rn("x", NodeRunning), rn("y", NodePending, "x"),
	})
	if !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("b 应立刻就绪、y 还得等: %v", got)
	}
}

func TestReadyNodes_已经在跑的不会被再次派发(t *testing.T) {
	got := ReadyNodes([]RunNode{rn("a", NodeRunning)})
	if len(got) != 0 {
		t.Fatalf("running 的节点不该再派发（会重复扣费）: %v", got)
	}
}

func TestReadyNodes_顺序稳定(t *testing.T) {
	nodes := []RunNode{rn("z", NodePending), rn("a", NodePending), rn("m", NodePending)}
	for i := 0; i < 5; i++ {
		if got := ReadyNodes(nodes); !reflect.DeepEqual(got, []string{"a", "m", "z"}) {
			t.Fatalf("顺序应稳定: %v", got)
		}
	}
}

// ---------------------------------------------------------------- 阻塞传播

// ⚠️ 只标直接下游的话，更远的节点会永远停在 pending ——
// 整次运行既不结束也不推进。
func TestBlockedByFailure_传递地标到底(t *testing.T) {
	got := BlockedByFailure([]RunNode{
		rn("a", NodeFailed),
		rn("b", NodePending, "a"),
		rn("c", NodePending, "b"),
		rn("d", NodePending, "c"),
	})
	if !reflect.DeepEqual(got, []string{"b", "c", "d"}) {
		t.Fatalf("应传递地标到底: %v", got)
	}
}

func TestBlockedByFailure_只有一支受影响(t *testing.T) {
	got := BlockedByFailure([]RunNode{
		rn("bad", NodeFailed),
		rn("x", NodePending, "bad"),
		rn("ok", NodeSuccess),
		rn("y", NodePending, "ok"),
	})
	if !reflect.DeepEqual(got, []string{"x"}) {
		t.Fatalf("另一支不该被牵连: %v", got)
	}
}

func TestBlockedByFailure_skipped不算失败(t *testing.T) {
	got := BlockedByFailure([]RunNode{
		rn("style", NodeSkipped),
		rn("img", NodePending, "style"),
	})
	if len(got) != 0 {
		t.Fatalf("skipped 的上游不该阻塞下游: %v", got)
	}
}

func TestBlockedByFailure_还在跑的不算(t *testing.T) {
	got := BlockedByFailure([]RunNode{
		rn("a", NodeRunning),
		rn("b", NodePending, "a"),
	})
	if len(got) != 0 {
		t.Fatalf("上游还在跑，下游只是没轮到，不是 blocked: %v", got)
	}
}

// ---------------------------------------------------------------- 运行状态

func TestAggregateRun(t *testing.T) {
	cases := []struct {
		name  string
		nodes []RunNode
		want  RunStatus
	}{
		{"全成功", []RunNode{rn("a", NodeSuccess), rn("b", NodeSuccess)}, RunSuccess},
		{"还有在跑的", []RunNode{rn("a", NodeSuccess), rn("b", NodeRunning)}, RunRunning},
		{"还有没派发的", []RunNode{rn("a", NodeSuccess), rn("b", NodePending)}, RunRunning},
		// ⚠️ 显示成 success 用户不会去翻那一个失败的；显示成 failed
		// 他会把已经成功的也重跑一遍。
		{"一成一败是部分成功",
			[]RunNode{rn("a", NodeSuccess), rn("b", NodeFailed)}, RunPartialSuccess},
		{"成功 + 被阻塞也是部分成功",
			[]RunNode{rn("a", NodeSuccess), rn("b", NodeBlocked)}, RunPartialSuccess},
		{"全失败", []RunNode{rn("a", NodeFailed), rn("b", NodeBlocked)}, RunFailed},
		{"全取消", []RunNode{rn("a", NodeCancelled)}, RunCancelled},
		// ⚠️ skipped 不算"做成了一件事"，但也不算失败。
		{"全跳过算成功", []RunNode{rn("a", NodeSkipped), rn("b", NodeSkipped)}, RunSuccess},
		{"跳过 + 成功", []RunNode{rn("a", NodeSkipped), rn("b", NodeSuccess)}, RunSuccess},
		{"跳过 + 失败（没有成功项）",
			[]RunNode{rn("a", NodeSkipped), rn("b", NodeFailed)}, RunFailed},
		{"空运行", nil, RunSuccess},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := AggregateRun(c.nodes); got != c.want {
				t.Fatalf("want %s got %s", c.want, got)
			}
		})
	}
}

// ⚠️ reuse 记成 success 会让"这次运行产出了几个"变成一个假数字 ——
// 而那个数字正是用户用来判断"这次跑值不值"的。
func TestActionToNodeStatus_复用落到skipped(t *testing.T) {
	cases := map[Action]NodeStatus{
		ActionRun:     NodePending,
		ActionReuse:   NodeSkipped,
		ActionSkip:    NodeSkipped,
		ActionBlocked: NodeBlocked,
	}
	for a, want := range cases {
		if got := ActionToNodeStatus(a); got != want {
			t.Errorf("%s → want %s got %s", a, want, got)
		}
	}
	// 但对下游来说 skipped 是满足的。
	if !NodeSatisfied(ActionToNodeStatus(ActionReuse)) {
		t.Fatal("复用的节点对下游应算满足，否则整张图推进不下去")
	}
}

// timeoutAfter 给"别死循环"那类用例用。
func timeoutAfter() <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		time.Sleep(2 * time.Second)
		close(ch)
	}()
	return ch
}

// ⚠️ 用户自己按了停止，界面却告诉他"运行失败了" —— 他会以为出了问题，
// 去翻日志找一个根本不存在的故障。取消的连带阻塞不算失败。
func TestAggregateRun_取消的连带阻塞仍算取消(t *testing.T) {
	got := AggregateRun([]RunNode{
		rn("c1", NodeCancelled),
		rn("c2", NodeBlocked, "c1"),
		rn("c3", NodeBlocked, "c2"),
	})
	if got != RunCancelled {
		t.Fatalf("取消 + 连带阻塞应算取消，实际 %s", got)
	}
}

// 但如果真的有节点失败了，那就是失败 —— 取消掩盖不了它。
func TestAggregateRun_有真失败时取消不掩盖(t *testing.T) {
	got := AggregateRun([]RunNode{
		rn("bad", NodeFailed),
		rn("c1", NodeCancelled),
	})
	if got != RunFailed {
		t.Fatalf("有真失败时应算失败，实际 %s", got)
	}
}
