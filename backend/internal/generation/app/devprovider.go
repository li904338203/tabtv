package app

import (
	"context"
	"encoding/json"

	"github.com/li904338203/tabtv/backend/internal/generation/domain"
)

// DevProvider 是开发用的假适配器。
//
// ⚠️ **只在 dev 可用**：production 下 config 会拒绝 `SEEAITV_DEV_FAKE_PROVIDER=true`
// 启动（08 §8）。它存在的意义是让整条闭环（受理 → 投递 → 执行 → 终态 → 退款）
// 在没有真实上游凭据时也能端到端跑通与做故障注入；真实协议在 P06。
type DevProvider struct {
	// FailRate 之外不做随机：故障注入要可复现，随机失败会让一次红色测试无法定位。
	AlwaysFail bool
}

func (p DevProvider) Submit(_ context.Context, job Job) (SubmitResult, error) {
	if p.AlwaysFail {
		return SubmitResult{
			Outcome: domain.SubmitRejected, HTTPStatus: 400, Message: "dev 假适配器：强制失败",
		}, nil
	}
	// 同步完成：开发期不需要轮询也能看到产出流程。
	raw, _ := json.Marshal(map[string]any{
		"provider": "dev_fake", "operation": job.OperationCode, "model": job.ModelCode,
	})
	// 回一个文本产物：让「归档 → 产物列表 → 通知」这条链在开发期也走全。
	// ⚠️ 不造假的图片字节 —— 那会让开发环境出现一批谁也不认识的对象。
	return SubmitResult{
		Outcome: domain.SubmitCompleted, Raw: raw,
		Outputs: []ProviderOutput{{
			Type: "text",
			Text: "dev 假适配器产物：" + job.OperationCode + " / " + job.ModelCode,
		}},
	}, nil
}

func (p DevProvider) Query(_ context.Context, job Job, _ string) (QueryResult, error) {
	return QueryResult{
		Done: true, Success: !p.AlwaysFail, Progress: 100,
		Outputs: []ProviderOutput{{Type: "text", Text: "dev 假适配器产物：" + job.OperationCode}},
	}, nil
}

func (p DevProvider) CanQueryByIdempotencyKey() bool { return false }
