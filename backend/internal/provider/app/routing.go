package app

import (
	"context"

	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
)

// DevProtocol 是开发假适配器的协议码。
const DevProtocol = "dev_fake"

// Routing 按任务快照里的协议码在假适配器与真实适配器之间分派。
//
// ⚠️ 为什么要有这层：闭环测试、故障注入与本地开发都需要一个不花钱、
// 不依赖外部凭据的上游，而真实模型必须走真实协议。按**协议码**分派
// 而不是按全局开关分派，好处是两者能在同一个环境里共存 ——
// 否则打开假适配器就等于所有模型都变假，真实联调根本没法做。
//
// ⚠️ `dev` 在 production 必须为 nil：config 已在启动时拒绝
// `SEEAITV_DEV_FAKE_PROVIDER=true`，这里再兜一层 —— 真到了线上，
// 一个配错协议码的模型会得到"协议不可用"而不是一个假产物。
type Routing struct {
	http *HTTPProvider
	dev  genapp.Provider
}

func NewRouting(httpProvider *HTTPProvider, dev genapp.Provider) *Routing {
	return &Routing{http: httpProvider, dev: dev}
}

func (r *Routing) pick(job genapp.Job) genapp.Provider {
	if parseSnapshot(job.Params).APIProtocol == DevProtocol && r.dev != nil {
		return r.dev
	}
	return r.http
}

func (r *Routing) Submit(ctx context.Context, job genapp.Job) (genapp.SubmitResult, error) {
	return r.pick(job).Submit(ctx, job)
}

func (r *Routing) Query(ctx context.Context, job genapp.Job, taskID string) (
	genapp.QueryResult, error) {
	return r.pick(job).Query(ctx, job, taskID)
}

// CanQueryByIdempotencyKey 取真实适配器的能力。
//
// ⚠️ 不能按任务分派：这个方法也用在**恢复扫描**里，那时手上只有一批
// 状态不确定的任务、没有具体 job。给出偏乐观的答案（说支持反查但其实不支持）
// 会让恢复扫描去重发，那正是最不该做的事。
func (r *Routing) CanQueryByIdempotencyKey() bool {
	return r.http.CanQueryByIdempotencyKey()
}
