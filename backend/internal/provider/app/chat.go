package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	"github.com/li904338203/tabtv/backend/internal/provider/protocol"
	"github.com/li904338203/tabtv/backend/internal/provider/transport"
)

// OpAgentChat 是推理模型调用记进请求日志的操作码（与 seed 的 operation.code 一致）。
const OpAgentChat = "agent_chat"

// ChatInput 是一次多轮对话调用（AGT-04，开发 01 §7.2）。
type ChatInput struct {
	ModelCode string
	System    string
	Messages  []protocol.ChatMessage
	Tools     []protocol.ToolDef
	// Options 透传（temperature / top_p / max_tokens …）。
	Options map[string]any
	// MaxTokensDefault 见 protocol.ChatJob.MaxTokensDefault。
	MaxTokensDefault int
}

// ChatOutput 是一次对话调用的完整结果。
type ChatOutput struct {
	Text      string
	ToolCalls []protocol.ToolCall
	// FinishReason 是归一值（stop / tool_calls / length / error）。
	FinishReason string
	// RawFinishReason 上游原文（未知值被归一成 stop 时，靠它排查）。
	RawFinishReason string
	Usage           protocol.Usage
}

// ChatError 是 Chat 失败时的错误，调用方（Agent 轮次运行器）据此决定本轮怎么结算。
//
// ⚠️ Kind 只有 connect / dns 两类表示「请求根本没到上游」—— 那两类在 Chat 内部已按
// MaxRetries 重试过，走到这里说明重试也用完了。其余（超时 / 空闲超时 / 中途断开 /
// HTTP 错误 / 流里的 error 事件）**都可能已在上游计费**，Chat 不重发，调用方也不该盲目重发。
type ChatError struct {
	Kind transport.ErrorKind
	// HTTPStatus 上游的状态码（没拿到响应时为 0）。
	HTTPStatus int
	// Upstream 是上游报的业务错误（HTTP 错误体或流里的 error 事件），可能为 nil。
	Upstream *protocol.UpstreamError
	Err      error
}

func (e *ChatError) Error() string {
	if e.HTTPStatus > 0 {
		return fmt.Sprintf("推理模型调用失败（%s，HTTP %d）: %v", e.Kind, e.HTTPStatus, e.Err)
	}
	return fmt.Sprintf("推理模型调用失败（%s）: %v", e.Kind, e.Err)
}

func (e *ChatError) Unwrap() error { return e.Err }

// errStreamDone 让传输层在解析器说「流已结束」时停止读取（不是错误）。
var errStreamDone = errors.New("chat stream done")

// errStreamTruncated 流在结束标记之前就断开了（EOF 但解析器没见到结束事件）。
var errStreamTruncated = errors.New("流式响应在结束标记之前中断")

// fallbackBodyLimit 是为「上游没按 SSE 回、直接回了一整段 JSON」保留的原文上限。
const fallbackBodyLimit = 1 << 20

// Chat 对一个配置好的文本模型发起**流式、可带工具**的多轮调用。
//
// 流程：取配置 → 协议须实现 ChatProtocol → BuildChat(Stream) → 流式发送（空闲看门狗）
// → 逐块交给解析器（文本增量回调 onDelta）→ 落一条请求日志（请求体 + 结果摘要）。
//
// ⚠️ 重试只针对「连不上 / DNS 解不出」（请求没到上游、onDelta 一次都没回调过），
// 按 cfg.MaxRetries 重试；超时 / 空闲超时 / 中途断开**一律不重发** —— 上游可能已经
// 在算、已经计费，重发等于让用户为同一轮付两次钱（开发 01 §7.2）。
// ⚠️ 不判断模型是否「支持 Agent」：那是目录域在配置时校验、Agent 侧在选模型时过滤的事，
// 这里只管「这个协议能不能说多轮 + 工具」。
func (p *HTTPProvider) Chat(ctx context.Context, in ChatInput,
	onDelta func(string)) (ChatOutput, error) {
	cfg, err := p.resolver.ResolveForRun(ctx, in.ModelCode)
	if err != nil {
		return ChatOutput{}, fmt.Errorf("取模型配置失败: %w", err)
	}
	proto, ok := protocol.Get(cfg.ProtocolCode)
	if !ok {
		return ChatOutput{}, fmt.Errorf("不支持的模型 API 协议: %s", cfg.ProtocolCode)
	}
	chat, ok := protocol.GetChat(cfg.ProtocolCode)
	if !ok {
		// ⚠️ 说清是哪个模型、哪个协议：目录域保存时就会拦住这种配置，
		// 走到这里多半是模型被改过协议、或者调用方拿错了模型。
		return ChatOutput{}, fmt.Errorf("模型 %s 用的协议 %s 不支持多轮对话与工具调用",
			in.ModelCode, cfg.ProtocolCode)
	}

	req, err := chat.BuildChat(protocol.ChatJob{
		ModelCode: cfg.ModelCode, UpstreamModel: cfg.UpstreamModel,
		System: in.System, Messages: in.Messages, Tools: in.Tools,
		Options: in.Options, MaxTokensDefault: in.MaxTokensDefault, Stream: true,
	})
	if err != nil {
		return ChatOutput{}, fmt.Errorf("构造推理模型请求失败: %w", err)
	}
	path, err := protocol.ResolveSubmitPath(proto,
		protocol.Job{OperationCode: OpAgentChat, ModelCode: cfg.ModelCode}, submitPathOf(cfg, req))
	if err != nil {
		return ChatOutput{}, err
	}

	retries := cfg.MaxRetries
	if retries < 0 {
		retries = 0
	}
	for attempt := 0; ; attempt++ {
		out, retryable, err := p.chatOnce(ctx, cfg, chat, req, path, onDelta)
		if err == nil || !retryable || attempt >= retries {
			return out, err
		}
		// 连不上：稍等再试（请求没到上游，重发安全）。
		select {
		case <-ctx.Done():
			return out, err
		case <-time.After(chatRetryBackoff(attempt)):
		}
	}
}

func chatRetryBackoff(attempt int) time.Duration {
	return time.Duration(attempt+1) * 200 * time.Millisecond
}

// chatOnce 发一次流式请求。第二个返回值表示这次失败能不能安全重发。
func (p *HTTPProvider) chatOnce(ctx context.Context, cfg RunConfig, chat protocol.ChatProtocol,
	req protocol.Request, path string, onDelta func(string)) (ChatOutput, bool, error) {
	parser := chat.NewChatStream()
	var (
		parserErr error
		done      bool
		raw       []byte
		rawOver   bool
	)
	resp, full, reqBody, sendErr := p.sendStream(ctx, cfg, req, path, func(chunk []byte) error {
		if !rawOver {
			if len(raw)+len(chunk) > fallbackBodyLimit {
				rawOver = true
				raw = nil
			} else {
				raw = append(raw, chunk...)
			}
		}
		d, err := parser.Feed(chunk, onDelta)
		if err != nil {
			parserErr = err
			return err
		}
		if d {
			done = true
			return errStreamDone
		}
		return nil
	})
	logJob := genapp.Job{OperationCode: OpAgentChat, ModelCode: cfg.ModelCode}

	switch {
	case sendErr != nil && errors.Is(sendErr, errStreamDone):
		// 解析器看到了结束标记：正常结束。
		sendErr = nil
		resp.Kind = transport.ErrorNone
	case sendErr != nil && parserErr != nil && errors.Is(sendErr, parserErr):
		// 流里的 error 事件（中转常在 HTTP 200 的流里报错）。
		ce := &ChatError{Kind: transport.ErrorStream, HTTPStatus: resp.StatusCode, Err: parserErr}
		_ = errors.As(parserErr, &ce.Upstream)
		p.writeChatLog(ctx, cfg, logJob, full, reqBody, resp, ce.Kind, parserErr.Error())
		return ChatOutput{}, false, ce
	case sendErr != nil:
		kind := resp.Kind
		if kind == transport.ErrorNone || kind == "" {
			kind = transport.ClassifyError(sendErr)
		}
		if errors.Is(sendErr, transport.ErrIdleTimeout) {
			kind = transport.ErrorStream
		}
		p.writeChatLog(ctx, cfg, logJob, full, reqBody, resp, kind, sendErr.Error())
		// ⚠️ 只有「连不上 / DNS」且**没拿到任何响应**才算没到上游。
		// 读到一半的 connection reset 也会被归成 connect，但那时已有状态码 —— 不能重发。
		retryable := resp.StatusCode == 0 &&
			(kind == transport.ErrorConnect || kind == transport.ErrorDNS)
		return ChatOutput{}, retryable, &ChatError{Kind: kind, HTTPStatus: resp.StatusCode, Err: sendErr}
	}

	if resp.StatusCode >= 400 {
		// HTTP 错误：传输层已把错误体读回来了，交给协议认出上游的错误信息。
		_, perr := chat.ParseChat(resp.StatusCode, resp.Body)
		if perr == nil {
			perr = fmt.Errorf("上游返回 HTTP %d", resp.StatusCode)
		}
		ce := &ChatError{Kind: transport.ErrorHTTP, HTTPStatus: resp.StatusCode, Err: perr}
		if !errors.As(perr, &ce.Upstream) {
			ce.Upstream = protocol.ParseUpstreamError(resp.Body)
		}
		p.writeChatLog(ctx, cfg, logJob, full, reqBody, resp, ce.Kind, string(resp.Body))
		return ChatOutput{}, false, ce
	}

	result := parser.Result()
	if !done {
		// EOF 但没见到结束标记。两种情况分开：
		//   · 上游没按 SSE 回、直接回了一整段 JSON（个别中转忽略 stream:true）→ 按非流式解析；
		//   · 真的断在半路 → 判失败，**不把半截回复当成功**（工具参数可能只拼了一半）。
		if fb, ok, ferr := fallbackParse(chat, resp.StatusCode, raw, rawOver, result); ok {
			if ferr != nil {
				ce := &ChatError{Kind: transport.ErrorStream, HTTPStatus: resp.StatusCode, Err: ferr}
				_ = errors.As(ferr, &ce.Upstream)
				p.writeChatLog(ctx, cfg, logJob, full, reqBody, resp, ce.Kind, ferr.Error())
				return ChatOutput{}, false, ce
			}
			if onDelta != nil && fb.Text != "" {
				onDelta(fb.Text)
			}
			result = fb
		} else if result.RawFinishReason == "" {
			ce := &ChatError{Kind: transport.ErrorStream, HTTPStatus: resp.StatusCode,
				Err: errStreamTruncated}
			p.writeChatLog(ctx, cfg, logJob, full, reqBody, resp, ce.Kind,
				errStreamTruncated.Error()+"；已收到 "+chatSummary(result))
			return ChatOutput{}, false, ce
		}
	}

	out := ChatOutput{
		Text: result.Text, ToolCalls: result.ToolCalls, FinishReason: result.FinishReason,
		RawFinishReason: result.RawFinishReason, Usage: result.Usage,
	}
	p.writeChatLog(ctx, cfg, logJob, full, reqBody, resp, transport.ErrorNone, chatSummary(result))
	return out, false, nil
}

// fallbackParse 处理「流里一个事件都没有、但响应体是一整段 JSON」的情形。
// 第二个返回值为 false 表示不适用（应按流处理）。
func fallbackParse(chat protocol.ChatProtocol, status int, raw []byte, rawOver bool,
	streamed protocol.ChatResult) (protocol.ChatResult, bool, error) {
	if rawOver || len(raw) == 0 {
		return protocol.ChatResult{}, false, nil
	}
	if streamed.Text != "" || len(streamed.ToolCalls) > 0 || streamed.RawFinishReason != "" {
		return protocol.ChatResult{}, false, nil
	}
	t := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(t, "{") {
		return protocol.ChatResult{}, false, nil
	}
	r, err := chat.ParseChat(status, []byte(t))
	return r, true, err
}

// chatSummary 是写进请求日志的结果摘要。
//
// ⚠️ 不记整条流：一轮长回复的 SSE 原文是正文的好几倍（每个字都包着一层 JSON），
// 而 provider_request_log 是会无限增长的表。排障要的是「怎么结束的、说了多少、调了哪些工具」。
func chatSummary(r protocol.ChatResult) string {
	names := make([]string, 0, len(r.ToolCalls))
	for _, tc := range r.ToolCalls {
		names = append(names, tc.Name)
	}
	finish := r.FinishReason
	if r.RawFinishReason != "" && r.RawFinishReason != finish {
		finish += "(" + r.RawFinishReason + ")"
	}
	return fmt.Sprintf("finish=%s, text=%d 字, tools=[%s], usage=%d/%d",
		finish, utf8.RuneCountInString(r.Text), strings.Join(names, ","),
		r.Usage.InputTokens, r.Usage.OutputTokens)
}

// writeChatLog 落一条推理模型调用日志：请求体照常脱敏截断，响应只记摘要或错误原文。
func (p *HTTPProvider) writeChatLog(ctx context.Context, cfg RunConfig, job genapp.Job,
	fullURL string, reqBody []byte, resp transport.Response, kind transport.ErrorKind,
	respText string) {
	logged := resp
	logged.Kind = kind
	logged.Body = []byte(respText)
	// sendErr 传 nil：错误类别已在 kind 里定好，响应体就是摘要或错误原文。
	p.writeLog(ctx, cfg, job, fullURL, reqBody, logged, nil)
}
