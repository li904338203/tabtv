// Package domain 是任务运维的纯规则（P13-03；GAP-07）。
//
// 这一域的每个操作都**直接改变用户的钱或任务状态**，而运维手上只有一个
// 任务 ID 和一屏日志。所以判定全部摊成纯函数：不碰库、不取当前时间
// （`now` 由调用方传进来），每条单独可测。
package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ---------------------------------------------------------------- 死信

// DeadStatus 是一条死信的处置状态。
type DeadStatus string

const (
	// DeadPending 待处理。
	DeadPending DeadStatus = "pending"
	// DeadReplayed 已重投回原队列。
	DeadReplayed DeadStatus = "replayed"
	// DeadDiscarded 已丢弃（人判定这条不需要再处理）。
	DeadDiscarded DeadStatus = "discarded"
)

var (
	// ErrDeadHandled 这条死信已经处置过了。
	ErrDeadHandled = errors.New("这条死信已处置，不能重复操作")
	// ErrNoteRequired 丢弃必须写明理由。
	ErrNoteRequired = errors.New("丢弃死信必须说明理由")
)

// CanReplay 判定能否重投。
//
// ⚠️ **只有 pending 能重投**：已重投的再投一次就是让同一条消息执行两遍，
// 而死信里恰恰有一类是「提交结果不确定，可能已在上游计费」—— 那一类
// 重投两次就是给用户计两次费，而我们只认得一个任务。
func CanReplay(cur DeadStatus) error {
	if cur != DeadPending {
		return ErrDeadHandled
	}
	return nil
}

// CanDiscard 判定能否丢弃。
//
// ⚠️ **理由必填**：丢弃是"决定不再处理这条消息"，而那条消息可能对应一次
// 已经在上游扣过钱的调用。不写理由的话，半年后没人说得清当时为什么丢它 ——
// 而这正是死信落库（而不是留在队列里）的全部意义。
func CanDiscard(cur DeadStatus, note string) error {
	if cur != DeadPending {
		return ErrDeadHandled
	}
	if note == "" {
		return ErrNoteRequired
	}
	return nil
}

// ---------------------------------------------------------------- 人工补偿

// TaskKind 是运维台能处置的任务类型。
//
// ⚠️ 与任务中心的 `content/domain.TaskKind` 取值一致，但**这里只列
// 能补偿的两类**：整图运行由它的子任务推进（补偿单个节点才有意义），
// 打包导出不扣费（补偿它只是改个状态，没有账务可言）。
type TaskKind string

const (
	// KindGeneration 生成任务。
	KindGeneration TaskKind = "generation"
	// KindRender 成片渲染。
	KindRender TaskKind = "render"
)

func (k TaskKind) Valid() bool {
	return k == KindGeneration || k == KindRender
}

var (
	// ErrAlreadyFinished 任务已是终态。
	ErrAlreadyFinished = errors.New("任务已结束，不能再判失败")
	// ErrReasonRequired 补偿必须写明原因。
	ErrReasonRequired = errors.New("人工补偿必须填写原因")
	// ErrBadKind 不支持的任务类型。
	ErrBadKind = errors.New("只能补偿生成任务与成片渲染")
)

// CompensateInput 是一次人工补偿的判定输入。
type CompensateInput struct {
	Kind TaskKind
	// Finished 表示任务已处于终态（success/failed/cancelled）。
	Finished bool
	// DeadlineAt 为空表示还没派发出去（排队中的任务可能没有死线）。
	DeadlineAt *time.Time
	Reason     string
	// Force 是运维明确表示「我知道它还没超时，但上游确实挂了」。
	Force bool
}

// CheckCompensate 判定一次人工补偿能否执行。
//
// ⚠️ **默认要求已过死线**。一个只是慢的任务被人工判失败之后，钱退了，
// 而它随后可能在上游**真的成功**了 —— 用户拿到退款又拿到产物。
// 反过来，确实挂掉的任务不补偿就是钱一直扣着而什么都没有。
// 所以：过了死线随便处置，没过死线必须显式 `force` 并说明原因。
//
// ⚠️ 补偿的方向是**钱退回去、产物可能丢掉**（Worker 那边的 CAS 会拒绝把
// success 写到 failed 上）。这是有意选的一侧：拿不到产物用户看得见，
// 钱没退回去他也看得见，但「钱扣着、任务永远转圈」是三者里最糟的。
func CheckCompensate(in CompensateInput, now time.Time) error {
	if !in.Kind.Valid() {
		return ErrBadKind
	}
	if in.Finished {
		return ErrAlreadyFinished
	}
	if in.Reason == "" {
		return ErrReasonRequired
	}
	if in.Force {
		return nil
	}
	if in.DeadlineAt == nil {
		return fmt.Errorf("这个任务还没有死线（尚未派发），确认要判失败请带 force 并说明原因")
	}
	if in.DeadlineAt.After(now) {
		return fmt.Errorf("任务尚未超时（还有 %s 到死线），确认要强制判失败请带 force",
			shortDuration(in.DeadlineAt.Sub(now)))
	}
	return nil
}

// shortDuration 把时长说成人话。
//
// 直接 `d.String()` 会输出 `1h23m45.6789s` —— 运维要在一屏告警里扫过去，
// 小数点后那串数字只是噪声。
func shortDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	}
	return fmt.Sprintf("%.1f 小时", d.Hours())
}

// SniffAggregate 尽力从死信的消息体里认出「这是哪个任务」。
//
// ⚠️ **解不开时回空值而不是报错**：死信按定义就可能是解不开的字节
// （`AckDead` 的第一种情形就是"消息解不开"）。认不出业务引用是**正常结果**，
// 不是数据缺陷 —— 而那恰恰是运维最该看到的信号：库里没有任何别的地方
// 记着这条消息，只能靠人读原文。
//
// ⚠️ 它是**纯解析**，所以放在 domain 并单独可测。放进编排层的话，
// "认不出来会怎样"就只能靠一次真库测试去验，而那种测试盖不全边界。
func SniffAggregate(body []byte) (string, int64) {
	var env struct {
		AggregateType string `json:"aggregateType"`
		AggregateID   string `json:"aggregateId"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return "", 0
	}
	// ⚠️ ID 对外是字符串（防 JS 精度丢失），这里自己解析而不是 strconv ——
	// 空串、非数字、溢出统统回 0，而**类型仍然保留**：
	// 知道"这是一条生成任务的消息、只是认不出是哪一条"也是有用的。
	if env.AggregateID == "" {
		return env.AggregateType, 0
	}
	var id int64
	for _, c := range env.AggregateID {
		if c < '0' || c > '9' {
			return env.AggregateType, 0
		}
		id = id*10 + int64(c-'0')
		if id < 0 {
			return env.AggregateType, 0
		}
	}
	return env.AggregateType, id
}
