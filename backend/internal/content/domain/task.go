package domain

import "strings"

// 任务中心的统一口径（P13-02；GAP-05）。
//
// ⚠️ **四类任务的状态词表并不一致**，这是真实存在的差异：
//
//	generation    queued / running / success / failed / cancelled
//	render_job    queued / running / success / failed / cancelled
//	export_task   queued / running / success / failed            （没有 cancelled）
//	workflow_run  pending / running / success / partial_success / failed / cancelled
//	                ↑ 用的是 pending 不是 queued            ↑ 多一个 partial_success
//
// 不统一就直接混在一个列表里的后果**很安静**：用户点"失败"筛选，
// 四类里有一类的状态词对不上，那一类就**整体消失** —— 而列表看起来很正常，
// 他只会以为那天没跑过整图任务。所以有且只有这一处做映射。

// TaskKind 是任务种类。
type TaskKind string

const (
	KindGeneration TaskKind = "generation" // 单节点生成
	KindWorkflow   TaskKind = "workflow"   // 整图运行
	KindRender     TaskKind = "render"     // 成片渲染
	KindExport     TaskKind = "export"     // 打包导出
)

func (k TaskKind) Valid() bool {
	switch k {
	case KindGeneration, KindWorkflow, KindRender, KindExport:
		return true
	}
	return false
}

// TaskState 是**对用户呈现**的统一状态。
//
// ⚠️ 只有五个值，而且**不引入新语义**：它是各域状态的投影，不是第二套状态机。
// 各域的真实状态仍以自己那张表为准（重试、退款、恢复都按各自的规则走）。
type TaskState string

const (
	StateQueued    TaskState = "queued"
	StateRunning   TaskState = "running"
	StateSuccess   TaskState = "success"
	StatePartial   TaskState = "partial_success"
	StateFailed    TaskState = "failed"
	StateCancelled TaskState = "cancelled"
)

// NormalizeState 把某一类任务的原始状态映射成统一状态。
//
// ⚠️ **未知值一律映射成 failed 而不是丢弃、也不是当成 running**：
//   - 丢弃的话那条任务会从列表里消失，用户看不到自己花过的钱；
//   - 当成 running 的话界面会永远转圈，他会一直等一个不会来的结果。
//
// 映射成 failed 至少是"看得见且是终态"，而新增状态词时集成测试会红
// （那张表把每个域的每个取值都列了出来）。
func NormalizeState(kind TaskKind, raw string) TaskState {
	switch strings.TrimSpace(raw) {
	// ⚠️ `pending` 只有整图运行在用 —— 少了这一行，整张图的排队任务
	// 会全部落进 failed，而用户看到的是一排"失败"的运行。
	case "pending", "queued":
		return StateQueued
	case "running":
		return StateRunning
	case "success":
		return StateSuccess
	case "partial_success":
		// ⚠️ **不能摊平成 success 或 failed**（AV-24）：显示成 success 用户不会去
		// 翻那条失败的，显示成 failed 他会把已经成功的也重跑一遍 ——
		// 两种都在花他的钱。只有整图运行会给出这个值。
		return StatePartial
	case "cancelled":
		return StateCancelled
	case "failed":
		return StateFailed
	}
	return StateFailed
}

// TerminalState 判定是否已是终态（前端据此停止轮询）。
//
// ⚠️ `partial_success` **是终态**：它是"有成有败"的最终结论，不是中间态。
// 漏掉它的话前端会一直轮询一个永远不再变的任务。
func TerminalState(s TaskState) bool {
	switch s {
	case StateSuccess, StatePartial, StateFailed, StateCancelled:
		return true
	}
	return false
}

// ParseState 解析筛选参数。空串表示不筛选。
func ParseState(s string) (TaskState, bool) {
	v := TaskState(strings.TrimSpace(s))
	switch v {
	case "":
		return "", true
	case StateQueued, StateRunning, StateSuccess, StatePartial, StateFailed, StateCancelled:
		return v, true
	}
	return "", false
}

// RawStatesFor 反向映射：某个统一状态对应各表里的哪些原始值。
//
// ⚠️ 筛选必须**下推到 SQL**（`where status = any(...)`），不能取回来再过滤 ——
// 后者在分页下是错的：取 20 条再过滤剩 3 条，用户翻一页只看到 3 条，
// 而他以为那就是全部。
//
// ⚠️ 这个函数与 NormalizeState 必须**互为逆运算**，集成测试用
// "每个原始值映射回来还是它自己"把两者钉在一起。
func RawStatesFor(s TaskState) []string {
	switch s {
	case StateQueued:
		return []string{"queued", "pending"}
	case StateRunning:
		return []string{"running"}
	case StateSuccess:
		return []string{"success"}
	case StatePartial:
		return []string{"partial_success"}
	case StateCancelled:
		return []string{"cancelled"}
	case StateFailed:
		return []string{"failed"}
	}
	return nil
}

// AllRawStates 列出四类任务里出现过的全部原始状态，供绊线测试使用。
//
// ⚠️ 新增一个状态词时，要么在这里加一行（并补 NormalizeState），
// 要么测试会红 —— 这正是它存在的理由：状态词是各域自己加的，
// 而任务中心是唯一一个必须认得全部的地方。
func AllRawStates() map[TaskKind][]string {
	return map[TaskKind][]string{
		KindGeneration: {"queued", "running", "success", "failed", "cancelled"},
		KindRender:     {"queued", "running", "success", "failed", "cancelled"},
		KindExport:     {"queued", "running", "success", "failed"},
		KindWorkflow: {"pending", "running", "success", "partial_success",
			"failed", "cancelled"},
	}
}
