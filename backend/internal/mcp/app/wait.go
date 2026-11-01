package app

import (
	"context"
	"time"
)

// tasks.wait 的轮询与心跳（MCPT-11，MCP-F-52、GAP-14）。协议无关：进度通知经 ctx 上的 ProgressFunc 发出
// （transport 在客户端给了 progressToken 时才放进去），时钟注入以便单测。
//
// ⚠️ 到点未结束**不是错误**：返回当前状态（working）。报错或写成 failed 的话，模型会以为任务失败、
// 重新提交一次 —— 再扣一次钱，而原来那个任务其实还在跑（GAP-14：浏览器轮询耗尽 ≠ 任务失败，同一条）。
// ⚠️ 每次都查各域的 Get（单行查询），不查任务中心的 union：那是带计数的列表查询，每秒打一次不划算。
// ⚠️ 客户端断开（ctx 取消）立即停：不停的话一个已经走了的客户端还在每秒查一次库，直到撞上截止。

// Default wait parameters（MCP-PARAM-03 的设计建议值）。
const (
	DefaultWaitMax       = 50 * time.Second
	DefaultWaitPoll      = time.Second
	DefaultWaitHeartbeat = 3 * time.Second
	// WaitDeadlineMargin 是离单次调用截止留出的余量：等满之后还要再查一次、序列化、写审计。
	WaitDeadlineMargin = 5 * time.Second
)

// Clock 是可注入的时钟。
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// RealClock 是系统时钟。
var RealClock Clock = realClock{}

// ProgressFunc 发一条进度通知（progress 单调递增，total 为 0 表示未知）。失败只能忽略：进度是提示不是结果。
type ProgressFunc func(ctx context.Context, progress, total float64, message string)

type progressKey struct{}

// WithProgress / ProgressFrom 在 ctx 上存取进度通知函数。客户端没给 progressToken 时 transport 不放 ——
// 取到 nil 就不发（协议规定没有 token 不得发进度通知）。
func WithProgress(ctx context.Context, f ProgressFunc) context.Context {
	return context.WithValue(ctx, progressKey{}, f)
}

func ProgressFrom(ctx context.Context) ProgressFunc {
	f, _ := ctx.Value(progressKey{}).(ProgressFunc)
	return f
}

// WaitOptions 是一次等待的参数。零值字段取默认值。
type WaitOptions struct {
	// Max 是本次最长等待（已按配置上限与调用截止收窄）。
	Max       time.Duration
	Poll      time.Duration
	Heartbeat time.Duration
	Clock     Clock
}

// WaitProbe 查一次任务：返回当前状态、是否终态。
type WaitProbe func(ctx context.Context) (state string, terminal bool, err error)

// WaitHeartbeat 拼一条心跳文案（state 是 MCP 状态，waited 是已等时长）。
type WaitHeartbeat func(state string, waited time.Duration) string

// WaitOutcome 是等待的结局。
type WaitOutcome struct {
	State    string
	Terminal bool
	// Waited 是实际等待的时长（按注入的时钟）。
	Waited time.Duration
	Polls  int
}

// EffectiveWaitMax 算本次最长等待：请求值（≤0 用上限）、配置上限、调用截止前留余量，三者取小。
// 截止已经近在眼前时返回 0（只查一次就返回）。
func EffectiveWaitMax(ctx context.Context, now time.Time, requested, limit time.Duration) time.Duration {
	if limit <= 0 {
		limit = DefaultWaitMax
	}
	d := limit
	if requested > 0 && requested < d {
		d = requested
	}
	if dl, ok := ctx.Deadline(); ok {
		if left := dl.Sub(now) - WaitDeadlineMargin; left < d {
			d = left
		}
	}
	if d < 0 {
		return 0
	}
	return d
}

// Wait 每 Poll 查一次、每 Heartbeat 发一次进度，直到终态、到点或 ctx 取消。
//
// 返回：终态 → Terminal=true；到点 → Terminal=false 且 err=nil（**不是错误**）；
// 查询出错 → 原样返回（如 NOT_FOUND）；ctx 取消 → ctx.Err()。
func Wait(ctx context.Context, opt WaitOptions, probe WaitProbe, heartbeat WaitHeartbeat) (WaitOutcome, error) {
	clk := opt.Clock
	if clk == nil {
		clk = RealClock
	}
	poll, hb := opt.Poll, opt.Heartbeat
	if poll <= 0 {
		poll = DefaultWaitPoll
	}
	if hb <= 0 {
		hb = DefaultWaitHeartbeat
	}
	progress := ProgressFrom(ctx)
	start := clk.Now()
	nextBeat := hb
	var out WaitOutcome
	for {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		state, terminal, err := probe(ctx)
		out.Polls++
		out.Waited = clk.Now().Sub(start)
		if err != nil {
			return out, err
		}
		out.State, out.Terminal = state, terminal
		if terminal || out.Waited >= opt.Max {
			return out, nil
		}
		if progress != nil && out.Waited >= nextBeat {
			// progress 用已等秒数（单调递增），total 用上限秒数：客户端据此画进度条，而不是任务本身的进度 ——
			// 后者各域口径不同、很多任务根本不报。
			progress(ctx, out.Waited.Seconds(), opt.Max.Seconds(), heartbeat(state, out.Waited))
			for nextBeat <= out.Waited {
				nextBeat += hb
			}
		}
		step := poll
		if left := opt.Max - out.Waited; left < step {
			step = left
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-clk.After(step):
		}
	}
}
