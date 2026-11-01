package domain

import (
	"reflect"
	"strings"
	"testing"
)

// a → b → c，另有 a → x。
func chain() []RunNode {
	return []RunNode{
		rn("a", NodeSuccess),
		rn("b", NodePending, "a"),
		rn("c", NodePending, "b"),
		rn("x", NodePending, "a"),
	}
}

// ---------------------------------------------------------------- 后代

// ⚠️ 只看直接下游的话，更远的节点会永远停在 pending，
// 运行既不结束也不推进。
func TestDownstreamOf_传递地找全(t *testing.T) {
	got := DownstreamOf(chain(), "a")
	if !reflect.DeepEqual(got, []string{"b", "c", "x"}) {
		t.Fatalf("应找全后代: %v", got)
	}
	if got := DownstreamOf(chain(), "b"); !reflect.DeepEqual(got, []string{"c"}) {
		t.Fatalf("b 的后代只有 c: %v", got)
	}
	if got := DownstreamOf(chain(), "c"); len(got) != 0 {
		t.Fatalf("c 没有后代: %v", got)
	}
}

func TestDownstreamOf_有环也不死循环(t *testing.T) {
	// 校验会拦下环，但调度器不该因为脏数据挂死。
	nodes := []RunNode{rn("a", NodePending, "b"), rn("b", NodePending, "a")}
	done := make(chan []string, 1)
	go func() { done <- DownstreamOf(nodes, "a") }()
	select {
	case got := <-done:
		if len(got) == 0 {
			t.Fatal("环里的节点互为后代")
		}
	case <-timeoutAfter():
		t.Fatal("有环时死循环了")
	}
}

// ---------------------------------------------------------------- 重试

// ⚠️ 只把失败那个放回 pending 的话，它跑完之后下游还停在 blocked（终态），
// 永远不会被调度。
func TestPlanRetry_放开被卡住的下游(t *testing.T) {
	nodes := []RunNode{
		rn("a", NodeFailed),
		rn("b", NodeBlocked, "a"),
		rn("c", NodeBlocked, "b"),
	}
	scope, err := PlanRetry(nodes, "a")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(scope.Reset, []string{"a", "b", "c"}) {
		t.Fatalf("应放开整条被卡住的链: %v", scope.Reset)
	}
}

// ⚠️ "必要下游"不是"全部下游"：已经成功的后代重跑要再花一次钱，
// 而用户点的是"重试这个失败的"。
func TestPlanRetry_已成功的后代不重跑(t *testing.T) {
	nodes := []RunNode{
		rn("a", NodeFailed),
		rn("ok", NodeSuccess, "a"), // 它是在 a 之前的某一版跑成功的
		rn("stuck", NodeBlocked, "a"),
	}
	scope, err := PlanRetry(nodes, "a")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(scope.Reset, []string{"a", "stuck"}) {
		t.Fatalf("已成功的后代不该被重置: %v", scope.Reset)
	}
}

func TestPlanRetry_只能重试没成功且不在跑的(t *testing.T) {
	cases := map[NodeStatus]string{
		NodeSuccess: "已经成功",
		NodeRunning: "还在运行中",
		NodeSkipped: "被跳过",
		NodePending: "还在运行中",
	}
	for status, want := range cases {
		_, err := PlanRetry([]RunNode{rn("a", status)}, "a")
		if err == nil {
			t.Errorf("%s 不该允许重试", status)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s 的错误应说明原因，实际 %v", status, err)
		}
	}
	// 失败/被卡/被取消的可以重试。
	for _, status := range []NodeStatus{NodeFailed, NodeBlocked, NodeCancelled} {
		if _, err := PlanRetry([]RunNode{rn("a", status)}, "a"); err != nil {
			t.Errorf("%s 应允许重试: %v", status, err)
		}
	}
}

func TestPlanRetry_节点不在运行里(t *testing.T) {
	if _, err := PlanRetry(chain(), "nowhere"); err == nil {
		t.Fatal("不存在的节点应报错")
	}
}

// ---------------------------------------------------------------- 跳过

// ⚠️ 用户跳过与计划器的 skip 不是一回事。混为一谈的话，
// 用户跳过一个图片节点，下游视频节点照样被派发 —— 拿不到首帧，
// 跑出来的东西完全不是他要的，而钱已经花了。
func TestPlanSkip_下游要被卡住(t *testing.T) {
	nodes := []RunNode{
		rn("img", NodePending),
		rn("vid", NodePending, "img"),
		rn("far", NodePending, "vid"),
	}
	scope, err := PlanSkip(nodes, "img")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(scope.Reset, []string{"far", "vid"}) {
		t.Fatalf("下游应传递地被卡住: %v", scope.Reset)
	}
}

// ⚠️ 已经成功的后代跑的时候上游是有产出的，
// 事后跳过上游不该把一张已经出好的图标成"跑不了"。
func TestPlanSkip_已成功的后代不受影响(t *testing.T) {
	nodes := []RunNode{
		rn("img", NodeFailed),
		rn("done", NodeSuccess, "img"),
		rn("todo", NodePending, "img"),
	}
	scope, err := PlanSkip(nodes, "img")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(scope.Reset, []string{"todo"}) {
		t.Fatalf("只该卡住还没跑的: %v", scope.Reset)
	}
}

func TestPlanSkip_运行中的要先取消(t *testing.T) {
	_, err := PlanSkip([]RunNode{rn("a", NodeRunning)}, "a")
	if err == nil || !strings.Contains(err.Error(), "取消") {
		t.Fatalf("运行中应提示先取消，实际 %v", err)
	}
}

func TestPlanSkip_已成功的不能再跳过(t *testing.T) {
	if _, err := PlanSkip([]RunNode{rn("a", NodeSuccess)}, "a"); err == nil {
		t.Fatal("已成功的节点不该还能跳过")
	}
}

// ---------------------------------------------------------------- 费用

func costNode(id string, s NodeStatus, credits int) CostNode {
	return CostNode{NodeID: id, Status: s, CreditsCost: credits}
}

// ⚠️ 用户要看到"扣过、退了"，而不是"好像从来没扣过" ——
// 后者会让他对不上账本流水。
func TestAggregateCost_失败的费用单列不从花掉的里减(t *testing.T) {
	c := AggregateCost([]CostNode{
		costNode("a", NodeSuccess, 5),
		costNode("b", NodeFailed, 5),
		costNode("c", NodeRunning, 8),
	})
	if c.Spent != 13 {
		t.Fatalf("成功 5 + 在跑 8 = 13，实际 %d", c.Spent)
	}
	if c.Refunded != 5 {
		t.Fatalf("失败的 5 应记进退回，实际 %d", c.Refunded)
	}
}

// ⚠️ 复用的节点没跑就没有价，只数次数不算金额。
func TestAggregateCost_复用只数次数(t *testing.T) {
	c := AggregateCost([]CostNode{
		{NodeID: "a", Status: NodeSkipped, IsReused: true, CreditsCost: 99},
		costNode("b", NodeSuccess, 5),
	})
	if c.Reused != 1 {
		t.Fatalf("应数出 1 个复用，实际 %d", c.Reused)
	}
	if c.Spent != 5 {
		t.Fatalf("复用的不该计入花费，实际 %d", c.Spent)
	}
}

func TestAggregateCost_逐条给出失败说明(t *testing.T) {
	c := AggregateCost([]CostNode{
		{NodeID: "z", Status: NodeFailed, CreditsCost: 1,
			ErrorCode: "upstream_error", ErrorMessage: "上游 500"},
		{NodeID: "a", Status: NodeBlocked, CreditsCost: 0,
			ErrorMessage: "上游没有产出"},
	})
	if len(c.Failures) != 2 {
		t.Fatalf("两条都该有说明: %+v", c.Failures)
	}
	// 按节点 ID 排序，界面才不跳。
	if c.Failures[0].NodeID != "a" {
		t.Fatalf("应按节点 ID 排序: %+v", c.Failures)
	}
	// blocked 不计入"失败个数"：它不是自己失败的。
	if c.Failed != 1 {
		t.Fatalf("blocked 不该算作失败，实际 %d", c.Failed)
	}
}

func TestAggregateCost_跳过的不计费也不算失败(t *testing.T) {
	c := AggregateCost([]CostNode{costNode("style", NodeSkipped, 0)})
	if c.Spent != 0 || c.Refunded != 0 || c.Failed != 0 {
		t.Fatalf("素材节点不该出现在任何一栏: %+v", c)
	}
}
