package domain

import (
	"fmt"
	"strconv"
	"time"
)

// 执行期规则：状态转换、重试分类、提交不确定窗口。

// Transition 判定一次状态转换是否合法。
//
// ⚠️ **终态不可再变**（success/failed/cancelled）。这不是洁癖：
// 晚到的 Provider 回调、重复的 MQ 消息、恢复扫描都会试图再写一次状态，
// 允许覆盖终态就意味着「已经退过款的任务又变回 running」，或者
// 「用户看到成功的产物被一个晚到的失败覆盖掉」（GO-18）。
func Transition(from, to string) error {
	if from == to {
		// 同态重复不是错误，是重复消息的正常表现；调用方据此静默 ack。
		return ErrSameState
	}
	if IsTerminal(from) {
		return fmt.Errorf("%w: %s 是终态，不能再变为 %s", ErrTerminal, from, to)
	}
	switch from {
	case StatusQueued:
		switch to {
		case StatusRunning, StatusFailed, StatusCancelled:
			return nil
		}
	case StatusRunning:
		switch to {
		case StatusSuccess, StatusFailed, StatusCancelled:
			return nil
		}
	}
	return fmt.Errorf("%w: 不允许 %s → %s", ErrTransition, from, to)
}

// IsTerminal 判定终态。
func IsTerminal(s string) bool {
	return s == StatusSuccess || s == StatusFailed || s == StatusCancelled
}

type transitionError struct{ msg string }

func (e transitionError) Error() string { return e.msg }

var (
	ErrTransition = transitionError{"状态转换不合法"}
	ErrTerminal   = transitionError{"任务已是终态"}
	ErrSameState  = transitionError{"状态未变化"}
)

// ---------------------------------------------------------------- 重试分类

// RetryClass 是一次失败的可重试性。
type RetryClass string

const (
	// RetryTransient 可重试：限流、超时、上游 5xx。
	RetryTransient RetryClass = "transient"
	// RetryPermanent 不可重试：鉴权失败、参数非法、上游 4xx。
	// ⚠️ 对这类重试是纯粹的浪费，而且会把一次配置错误放大成几十次上游调用。
	RetryPermanent RetryClass = "permanent"
)

// ClassifyHTTP 按 HTTP 状态码分类。
//
// ⚠️ 这张表必须有测试钉住（08 §7）：分错一类的后果在两个方向上都很糟 ——
// 把永久错误判成可重试会反复打上游，把临时错误判成永久会让用户白白失败一次。
func ClassifyHTTP(status int) RetryClass {
	switch {
	case status == 408, status == 429:
		return RetryTransient
	case status >= 500:
		return RetryTransient
	case status >= 400:
		return RetryPermanent
	}
	return RetryPermanent
}

// MaxAttempts 单个任务的最大尝试次数。
//
// ⚠️ 这是**设计建议值**：太小会让一次上游抖动直接判失败，
// 太大会让一个必然失败的任务占着并发额度不放。
const MaxAttempts = 3

// BackoffFor 计算第 n 次重试前的等待。指数退避 + 上限。
//
// ⚠️ 没有退避的重试在上游限流时是火上浇油：它会让本来几秒能恢复的限流
// 一直续命。上限 5 分钟是为了不让单个任务的重试拖过它自己的死线。
func BackoffFor(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := time.Duration(1<<uint(attempt-1)) * 10 * time.Second
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}

// ---------------------------------------------------------------- 提交不确定窗口

// SubmitOutcome 描述一次 Provider 提交的结果。
type SubmitOutcome string

const (
	// SubmitAccepted 上游已受理并给了 task ID。
	SubmitAccepted SubmitOutcome = "accepted"
	// SubmitCompleted 同步协议：一次调用就拿到了结果。
	SubmitCompleted SubmitOutcome = "completed"
	// SubmitUncertain **提交可能已经到达上游，但我们没拿到 task ID**。
	SubmitUncertain SubmitOutcome = "uncertain"
	// SubmitRejected 上游明确拒绝。
	SubmitRejected SubmitOutcome = "rejected"
)

// ErrorCodeSubmitUncertain 是不确定窗口的错误码。
//
// 单独一个码而不是混进 `provider_error`：运维要能一眼捞出这批任务 ——
// 它们**可能已经在上游产生了真实消费**，处理方式与普通失败不同。
const ErrorCodeSubmitUncertain = "submit_uncertain"

// ResolveUncertain 决定「提交成功但 task ID 未落库」时怎么办（GO-14、FLOW-03）。
//
// ⚠️ **绝不无条件重发**：重发可能在上游创建第二个任务，于是用户被上游计费两次，
// 而我们这边只认得其中一个。三条出路按优先级：
//
//  1. 协议支持按**我们的幂等键**反查 → 查一次，查到就接管，查不到才算没提交成功；
//  2. 不支持反查 → 判失败并退款，错误码标 `submit_uncertain`，
//     让运维能捞出这批「可能已在上游消费」的任务人工核对；
//  3. **不允许**的做法：当作没发生过直接重试。
func ResolveUncertain(canQueryByIdempotencyKey bool) (retry bool, errCode string) {
	if canQueryByIdempotencyKey {
		return true, ""
	}
	return false, ErrorCodeSubmitUncertain
}

// ExpiredBy 判定任务是否已过死线。
//
// ⚠️ 用**存下来的** deadline_at，不按当前配置重算（GO-24）。
func ExpiredBy(deadlineAt *time.Time, now time.Time) bool {
	return deadlineAt != nil && !deadlineAt.After(now)
}

// ErrorCodeTimeout 超时判失败时的错误码。
const ErrorCodeTimeout = "deadline_exceeded"

// ErrorCodeCancelled 用户取消。
//
// ⚠️ 新系统用**独立的 cancelled 状态**，不是旧系统的 `failed + errorCode=cancelled`（CD-06）。
// 既然改了，就必须在 API、统计、退款与前端映射里**一次性统一**，不能部分混用。
const ErrorCodeCancelled = "cancelled_by_user"

// ErrorCodeOpsCompensated 运维补偿判失败时的错误码（统计与排障要能把它与真实的上游失败分开）。
const ErrorCodeOpsCompensated = "ops_compensated"

// OutputMediaURL 是媒体产物的**受保护端点**（媒体域，真正出字节的那个），不是对象存储直链（CD-16）。
//
// ⚠️ 产物列表与状态事件都要给这个地址，只许这一处拼：两处各拼一份的话，
// 改了一边另一边照旧，前端拿着事件里的地址取到的是 404 —— 而界面上只是一张裂图。
func OutputMediaURL(mediaID int64) string {
	return "/api/v1/media/" + strconv.FormatInt(mediaID, 10)
}
