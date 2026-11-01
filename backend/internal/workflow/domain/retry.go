package domain

import (
	"fmt"
	"sort"
)

// 重试、跳过、取消与费用聚合（NEW-05、NEW-06）。

// DownstreamOf 求一个节点的**全部后代**（不含自身）。
//
// ⚠️ 要传递地找全：跳过 A 之后，不只是 B 跑不了，B 的下游也跑不了。
// 只看直接下游的话，更远的节点会永远停在 pending，运行既不结束也不推进。
func DownstreamOf(nodes []RunNode, nodeID string) []string {
	children := map[string][]string{}
	for _, n := range nodes {
		for _, dep := range n.DependsOn {
			children[dep] = append(children[dep], n.NodeID)
		}
	}
	seen := map[string]bool{}
	var walk func(id string)
	walk = func(id string) {
		for _, c := range children[id] {
			if seen[c] {
				continue
			}
			seen[c] = true
			walk(c)
		}
	}
	walk(nodeID)

	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// RetryScope 是一次节点重试要动的范围。
type RetryScope struct {
	// Reset 是要重新跑的节点：目标节点 + 它那些**因为它才跑不了**的后代。
	Reset []string
	// Reason 说明为什么是这个范围，出错时直接给用户看。
	Reason string
}

// PlanRetry 规划一次节点重试（NEW-05：重试失败节点和**必要**下游）。
//
// ⚠️ "必要下游"不是"全部下游"：已经成功的后代**不重跑**。
// 它们的产出还在，重跑要再花一次钱 —— 而用户点的是"重试这个失败的"。
//
// ⚠️ 但被它卡住的后代（blocked）**要一起放开**：只把失败那个放回 pending，
// 它跑完之后下游还停在 blocked（终态），永远不会被调度。
func PlanRetry(nodes []RunNode, nodeID string) (RetryScope, error) {
	byID := map[string]RunNode{}
	for _, n := range nodes {
		byID[n.NodeID] = n
	}
	target, ok := byID[nodeID]
	if !ok {
		return RetryScope{}, fmt.Errorf("节点 %s 不在这次运行里", nodeID)
	}
	// ⚠️ 只允许重试**已经不跑了且没成功**的：还在跑的重试等于重复提交，
	// 已成功的重试等于花钱做他没要的事（要重做有 force 整图）。
	switch target.Status {
	case NodeFailed, NodeBlocked, NodeCancelled:
	case NodeSuccess:
		return RetryScope{}, fmt.Errorf("节点 %s 已经成功，重试会重复扣费；"+
			"想重做请用整图强制执行", nodeID)
	case NodeSkipped:
		return RetryScope{}, fmt.Errorf("节点 %s 被跳过了，先恢复它再重试", nodeID)
	default:
		return RetryScope{}, fmt.Errorf("节点 %s 还在运行中", nodeID)
	}

	reset := []string{nodeID}
	for _, id := range DownstreamOf(nodes, nodeID) {
		// 成功的后代不动（它们的产出还在）；还没跑的后代本来就是 pending。
		if byID[id].Status == NodeBlocked {
			reset = append(reset, id)
		}
	}
	sort.Strings(reset)
	return RetryScope{
		Reset:  reset,
		Reason: fmt.Sprintf("重试 %s，并放开被它卡住的 %d 个下游", nodeID, len(reset)-1),
	}, nil
}

// PlanSkip 规划一次"跳过这个节点"（NEW-06）。
//
// ⚠️ 用户主动跳过与计划器的 skip **不是一回事**：
//
//	计划器的 skip —— 素材节点、静态文本、复用上次产出。
//	                 它们**有产出或不需要产出**，下游照常跑。
//	用户的 skip   —— "这个节点我不跑了"。它**本来会产出而现在没有**，
//	                 所以下游拿不到输入，必须 blocked。
//
// 混为一谈的话，用户跳过一个图片节点，下游视频节点照样被派发 ——
// 拿不到首帧，跑出来的东西完全不是他要的，而钱已经花了。
func PlanSkip(nodes []RunNode, nodeID string) (RetryScope, error) {
	byID := map[string]RunNode{}
	for _, n := range nodes {
		byID[n.NodeID] = n
	}
	target, ok := byID[nodeID]
	if !ok {
		return RetryScope{}, fmt.Errorf("节点 %s 不在这次运行里", nodeID)
	}
	if NodeTerminal(target.Status) && target.Status != NodeFailed &&
		target.Status != NodeBlocked {
		return RetryScope{}, fmt.Errorf("节点 %s 已经是 %s，不能再跳过",
			nodeID, target.Status)
	}
	if target.Status == NodeRunning {
		return RetryScope{}, fmt.Errorf("节点 %s 还在跑，请先取消它", nodeID)
	}

	var blocked []string
	for _, id := range DownstreamOf(nodes, nodeID) {
		// ⚠️ 已经成功的后代不动：它们跑的时候上游是有产出的，
		// 事后跳过上游不该把一张已经出好的图标成"跑不了"。
		if !NodeTerminal(byID[id].Status) {
			blocked = append(blocked, id)
		}
	}
	return RetryScope{
		Reset:  blocked,
		Reason: fmt.Sprintf("跳过 %s，它的 %d 个下游因此拿不到输入", nodeID, len(blocked)),
	}, nil
}

// ---------------------------------------------------------------- 费用聚合

// RunCost 是一次运行的费用对账（NEW-06）。
type RunCost struct {
	// Spent 真正花掉的：成功的 + 还在跑的（预扣即消费）。
	Spent int
	// Refunded 失败/取消的那些扣过又退了。
	//
	// ⚠️ 单列出来而不是从 Spent 里减掉：用户要看到"扣过、退了"，
	// 而不是"好像从来没扣过" —— 后者会让他对不上账本流水。
	Refunded int
	// Saved 因为复用而省下的次数（不是金额 —— 没跑就没有价）。
	Reused    int
	Succeeded int
	Failed    int
	Running   int
	// Failures 逐条说明，部分失败必须可解释。
	Failures []NodeFailure
}

// NodeFailure 是一个节点的失败说明。
type NodeFailure struct {
	NodeID  string
	Code    string
	Message string
}

// CostNode 是费用聚合需要的最小信息。
type CostNode struct {
	NodeID       string
	Status       NodeStatus
	CreditsCost  int
	IsReused     bool
	ErrorCode    string
	ErrorMessage string
}

// AggregateCost 逐节点对账。
//
// ⚠️ 按**节点自己的 creditsCost** 加总，不是"节点数 × 单价" ——
// 同一张图里可以有不同模型、不同时长的节点，乘出来的数只在
// 最简单的情况下碰巧对。
func AggregateCost(nodes []CostNode) RunCost {
	var out RunCost
	for _, n := range nodes {
		if n.IsReused {
			out.Reused++
			continue
		}
		switch n.Status {
		case NodeSuccess:
			out.Spent += n.CreditsCost
			out.Succeeded++
		case NodePending, NodeRunning:
			// 预扣即消费：排队中的钱已经扣了，不算"将要花"。
			out.Spent += n.CreditsCost
			out.Running++
		case NodeFailed, NodeCancelled, NodeBlocked:
			out.Refunded += n.CreditsCost
			if n.Status != NodeBlocked {
				out.Failed++
			}
			if n.ErrorMessage != "" || n.ErrorCode != "" {
				out.Failures = append(out.Failures, NodeFailure{
					NodeID: n.NodeID, Code: n.ErrorCode, Message: n.ErrorMessage,
				})
			}
		}
	}
	sort.Slice(out.Failures, func(i, j int) bool {
		return out.Failures[i].NodeID < out.Failures[j].NodeID
	})
	return out
}
