package app

import (
	"context"
	"fmt"
	"strings"

	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	"github.com/li904338203/tabtv/backend/internal/platform/usererr"
	"github.com/li904338203/tabtv/backend/internal/provider/protocol"
)

// TextInput 是一次同步文本调用。
type TextInput struct {
	ModelCode string
	System    string
	User      string
	// Images 参考图（已是上游能用的形态）。图片反推走这里。
	Images []string
	// Options 透传 temperature / top_p / max_tokens。
	Options map[string]any
	// Operation 是调用方的操作码，记进请求日志（provider_request_log.operation）。空时记 text2text。
	// ⚠️ 只影响日志归类，不影响请求体：AI 搭景（scene_build）发出去的仍是 text2text 形状的请求。
	// 日志写死 text2text 的话，排查时按操作筛不出搭景的请求。
	Operation string
}

// CompleteText 同步调一次文本模型。
//
// ⚠️ 这是**文字模型的唯一入口**：脚本分镜、提示词合成、以及将来任何
// 需要同步文本产出的地方都走它。老系统同步与异步各写了一套，
// 流式开关、错误处理、超时语义三处都不一致 ——
// 同一个上游故障在两条路上表现完全不同，排查时永远在问"走的是哪条"。
//
// ⚠️ 它**不走 generation 队列**：产出是结构化文本不是媒体，
// 走 MQ 只多一层状态同步（06 §4.1）。费用记在调用方自己的表里
// （脚本域是 `script_llm_call`），与普通生成分开追踪。
//
// ⚠️ 返回的错误一律经 usererr.Wrap：调用方（脚本分镜、智能剪辑、导演台）会把它原样写进调用记录、
// 回给前端，上游原话（报文、内部地址、模型名）不能给用户看。原话进日志与 provider_request_log。
func (p *HTTPProvider) CompleteText(ctx context.Context, in TextInput) (string, error) {
	text, err := p.completeText(ctx, in)
	if err != nil {
		p.logf("同步文本调用失败（上游原话）", "model", in.ModelCode, "operation", in.Operation, "detail", err.Error())
		return "", usererr.Wrap(err)
	}
	return text, nil
}

func (p *HTTPProvider) completeText(ctx context.Context, in TextInput) (string, error) {
	cfg, err := p.resolver.ResolveForRun(ctx, in.ModelCode)
	if err != nil {
		return "", fmt.Errorf("取模型配置失败: %w", err)
	}
	proto, ok := protocol.Get(cfg.ProtocolCode)
	if !ok {
		return "", fmt.Errorf("不支持的模型 API 协议: %s", cfg.ProtocolCode)
	}
	if proto.Async() {
		// ⚠️ 异步协议没法同步用：它只回一个任务 ID。让调用方换一个文本模型，
		// 而不是在这里偷偷起一个轮询 —— 那会让一个"同步"方法阻塞几分钟。
		return "", fmt.Errorf("模型 %s 用的是异步协议 %s，不能用于同步文本调用",
			in.ModelCode, cfg.ProtocolCode)
	}

	// 请求体照旧按 text2text 构造：这次调用的形状就是「文本（+ 图）进、文本出」。把调用方的操作码
	// 塞进请求的话，通用信封（generic_*）会把 scene_build 当成路由依据发给中转，而中转只认画布操作。
	// 调用方的操作码只进请求日志（见下面的 send）。
	op := in.Operation
	if op == "" {
		op = "text2text"
	}
	job := protocol.Job{
		OperationCode: "text2text", ModelCode: cfg.ModelCode,
		UpstreamModel: cfg.UpstreamModel, ResultType: "text",
		Prompt: in.User, System: in.System, Images: in.Images,
		Params: map[string]any{}, Options: in.Options,
	}
	req, err := proto.BuildSubmit(job)
	if err != nil {
		return "", err
	}
	path, err := protocol.ResolveSubmitPath(proto, job, submitPathOf(cfg, req))
	if err != nil {
		return "", err
	}

	// 请求日志照常落：同步文本调用同样会失败、同样需要排查。
	// generationId 为 0（它不属于任何 generation），按 model + operation 找。
	resp, sendErr := p.send(ctx, cfg, genapp.Job{
		OperationCode: op, ModelCode: in.ModelCode,
	}, req, path)
	if sendErr != nil {
		return "", sendErr
	}
	result, err := protocol.ParseSubmitResponse(proto, resp.StatusCode, resp.Header, resp.Body)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, o := range result.Outputs {
		if o.Kind == "text" {
			sb.WriteString(o.Value)
		}
	}
	text := strings.TrimSpace(sb.String())
	if text == "" {
		// ⚠️ 空产出不是"成功但为空"：调用方会拿它去解析，解析失败的报错
		// 指向的是解析器而不是这里。在源头说清楚。
		return "", fmt.Errorf("模型返回了空文本")
	}
	return text, nil
}
