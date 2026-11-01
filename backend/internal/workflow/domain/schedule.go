package domain

import "sort"

// 调度：谁现在可以跑、谁被卡住了、整次运行到哪一步了（NEW-04、NEW-06）。

// NodeStatus 是节点运行状态。
type NodeStatus string

const (
	NodePending NodeStatus = "pending"
	NodeRunning NodeStatus = "running"
	NodeSuccess NodeStatus = "success"
	NodeFailed  NodeStatus = "failed"
	// NodeSkipped 本来就不需要跑（素材、静态文本、可复用）。
	NodeSkipped NodeStatus = "skipped"
	// NodeBlocked 上游没有产出，这个节点跑不了（NEW-06）。
	NodeBlocked   NodeStatus = "blocked"
	NodeCancelled NodeStatus = "cancelled"
)

// NodeTerminal 判断节点是否已到终态。
func NodeTerminal(s NodeStatus) bool {
	switch s {
	case NodeSuccess, NodeFailed, NodeSkipped, NodeBlocked, NodeCancelled:
		return true
	}
	return false
}

// NodeSatisfied 判断这个上游算不算"已经给出了下游要的东西"。
//
// ⚠️ `skipped` **算满足**：素材节点、静态文本、以及复用了上次产出的节点
// 都落在这里 —— 它们确实没跑，但下游要的东西是在的。
// 不认这一条的话，任何带风格节点或可复用节点的图都推进不下去。
func NodeSatisfied(s NodeStatus) bool {
	return s == NodeSuccess || s == NodeSkipped
}

// RunNode 是调度需要的最小节点信息。
type RunNode struct {
	NodeID    string
	DependsOn []string
	Status    NodeStatus
}

// ReadyNodes 找出**现在可以派发**的节点。
//
// ⚠️ 只看依赖是否**全部满足**，不看层号：层号是预览用的展示信息，
// 真正的就绪条件是"我依赖的都给出结果了"。按层号派发的话，
// 同层里有一个跑得慢，另一层里本来可以开跑的节点会白等。
//
// ⚠️ 返回的顺序**稳定**（按 nodeID）：不稳定的话每次恢复扫描派发的顺序
// 都不同，问题很难复现。
func ReadyNodes(nodes []RunNode) []string {
	status := make(map[string]NodeStatus, len(nodes))
	for _, n := range nodes {
		status[n.NodeID] = n.Status
	}
	var out []string
	for _, n := range nodes {
		if n.Status != NodePending {
			continue
		}
		ok := true
		for _, dep := range n.DependsOn {
			s, known := status[dep]
			// ⚠️ 依赖不在本次运行里（子图外）时按**已满足**处理：
			// 它要么是上一次跑出来的产出，要么根本不需要跑。
			// 按"未满足"处理的话，任何子图执行都会卡在第一层。
			if known && !NodeSatisfied(s) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, n.NodeID)
		}
	}
	sort.Strings(out)
	return out
}

// BlockedByFailure 找出因为上游失败/被卡而**再也跑不了**的节点（NEW-06）。
//
// ⚠️ 要**传递地**找全：A 失败 → B blocked → C 也得 blocked。
// 只标直接下游的话，C 会永远停在 pending，整次运行既不结束也不推进。
func BlockedByFailure(nodes []RunNode) []string {
	status := make(map[string]NodeStatus, len(nodes))
	for _, n := range nodes {
		status[n.NodeID] = n.Status
	}
	changed := true
	blocked := map[string]bool{}
	for changed {
		changed = false
		for _, n := range nodes {
			if n.Status != NodePending || blocked[n.NodeID] {
				continue
			}
			for _, dep := range n.DependsOn {
				s, known := status[dep]
				if !known {
					continue
				}
				if blocked[dep] || (NodeTerminal(s) && !NodeSatisfied(s)) {
					blocked[n.NodeID] = true
					changed = true
					break
				}
			}
		}
	}
	out := make([]string, 0, len(blocked))
	for id := range blocked {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// RunStatus 是整次运行的状态。
type RunStatus string

const (
	RunPending        RunStatus = "pending"
	RunRunning        RunStatus = "running"
	RunSuccess        RunStatus = "success"
	RunPartialSuccess RunStatus = "partial_success"
	RunFailed         RunStatus = "failed"
	RunCancelled      RunStatus = "cancelled"
)

// AggregateRun 按节点算整次运行的状态。
//
// ⚠️ `partial_success` 是**独立状态**（同组任务的 AV-24）：
// 把"10 个节点成了 9 个"显示成 success，用户不会去翻那一个失败的；
// 显示成 failed，他会把 9 个成功的也重跑一遍 —— 两种都在花他的钱。
//
// ⚠️ `skipped` **不算"做成了一件事"**：一张全是素材节点的图不能算成功。
// 但它也不算失败 —— 只有真的产出过东西（success）才够格算成功。
func AggregateRun(nodes []RunNode) RunStatus {
	if len(nodes) == 0 {
		return RunSuccess
	}
	var running, success, failed, blocked, cancelled, skipped int
	for _, n := range nodes {
		switch n.Status {
		case NodePending, NodeRunning:
			running++
		case NodeSuccess:
			success++
		case NodeFailed:
			failed++
		case NodeBlocked:
			blocked++
		case NodeCancelled:
			cancelled++
		case NodeSkipped:
			skipped++
		}
	}
	if running > 0 {
		return RunRunning
	}
	bad := failed + blocked + cancelled
	switch {
	case bad == 0:
		return RunSuccess
	case success > 0:
		return RunPartialSuccess
	// ⚠️ 有取消、没有真正的失败 → 整次算**取消**，哪怕还有一堆 blocked：
	// 那些 blocked 正是取消的连带结果。报成 failed 的话，
	// 用户自己按了停止，界面却告诉他"运行失败了" —— 他会以为出了问题，
	// 去翻日志找一个根本不存在的故障。
	case cancelled > 0 && failed == 0:
		return RunCancelled
	}
	return RunFailed
}

// ActionToNodeStatus 把计划的处置映射成节点的初始状态。
//
// ⚠️ `reuse` 落到 `skipped` 而不是 `success`：它确实没跑，
// 记成 success 会让"这次运行产出了几个"变成一个假数字 ——
// 而那个数字正是用户用来判断"这次跑值不值"的。
// 但它对下游来说是**满足**的（NodeSatisfied 里认了 skipped）。
func ActionToNodeStatus(a Action) NodeStatus {
	switch a {
	case ActionRun:
		return NodePending
	case ActionReuse, ActionSkip:
		return NodeSkipped
	}
	return NodeBlocked
}
