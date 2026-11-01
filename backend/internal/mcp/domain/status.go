package domain

import contentdomain "github.com/li904338203/tabtv/backend/internal/content/domain"

// MCP 对外的任务状态（MCP-F-53，设计方案 §4.4）。
//
// ⚠️ **只映射一处、且叠在任务中心的统一口径上**：四类任务的原始状态词先过
// `content/domain.NormalizeState`（那是唯一认得全部原始词的地方，`pending` / `partial_success`
// 都在那里），这里只把统一状态再投影成 MCP 的词表。在 MCP 侧另写一份原始词映射的话，
// 新增一个状态词时两边分叉，而分叉的表现是某一类任务在外部客户端里永远 "working"。
const (
	TaskWorking   = "working"
	TaskCompleted = "completed"
	TaskFailed    = "failed"
	TaskCancelled = "cancelled"
	// TaskPartial 单列：**不能摊平**成 completed 或 failed（AV-24）——摊成 completed 模型不会去
	// 看那几个失败的节点，摊成 failed 它会把已经成功的也重跑一遍，两种都在花用户的钱。
	TaskPartial = "partial_success"
)

// TaskStatuses 返回全部 MCP 任务状态（outputSchema 的枚举与文档用）。
func TaskStatuses() []string {
	return []string{TaskWorking, TaskCompleted, TaskFailed, TaskCancelled, TaskPartial}
}

// MapTaskState 把任务中心的统一状态投影成 MCP 状态。未知值按 failed（与 NormalizeState 同一取向：
// 「看得见且是终态」好过让客户端永远等一个不会来的结果）。
func MapTaskState(s contentdomain.TaskState) string {
	switch s {
	case contentdomain.StateQueued, contentdomain.StateRunning:
		return TaskWorking
	case contentdomain.StateSuccess:
		return TaskCompleted
	case contentdomain.StatePartial:
		return TaskPartial
	case contentdomain.StateCancelled:
		return TaskCancelled
	}
	return TaskFailed
}

// MapRawTaskState 从某一类任务的原始状态直接得到 MCP 状态（tasks.get 用）。
func MapRawTaskState(kind, raw string) string {
	return MapTaskState(contentdomain.NormalizeState(contentdomain.TaskKind(kind), raw))
}

// TaskTerminal 是否终态（客户端据此停止轮询）。判定仍以任务中心为准。
func TaskTerminal(s contentdomain.TaskState) bool { return contentdomain.TerminalState(s) }
