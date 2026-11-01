// Package channel 是验证码发送渠道的适配层。
//
// 真实 SMTP 与短信厂商凭据属 PARAM-03/04，尚未到位（受阻）。
// 但受阻不等于不做：接口与本地实现先落地，真实渠道接上即可切换。
package channel

import (
	"context"
	"fmt"
	"log/slog"
)

type Kind string

const (
	Email Kind = "email"
	SMS   Kind = "sms"
)

// Sender 发送验证码。实现必须尊重 ctx 超时 ——
// 发送渠道卡住不能拖垮整个请求。
type Sender interface {
	Kind() Kind
	Send(ctx context.Context, target, code string) error
}

// LogSender 把验证码打进日志，仅供本地开发。
//
// ⚠️ production 下绝不允许启用：验证码进日志等于任何能看日志的人
// 都能登录任意账号。由 config 的 production fail-fast 拦截。
type LogSender struct {
	K      Kind
	Logger *slog.Logger
}

func (s LogSender) Kind() Kind { return s.K }

func (s LogSender) Send(_ context.Context, target, code string) error {
	s.Logger.Warn("【仅开发】验证码未真实发送，直接输出",
		"channel", string(s.K), "target", target, "code", code)
	return nil
}

// Registry 按渠道分发。
type Registry struct {
	senders map[Kind]Sender
}

func NewRegistry(senders ...Sender) *Registry {
	m := make(map[Kind]Sender, len(senders))
	for _, s := range senders {
		m[s.Kind()] = s
	}
	return &Registry{senders: m}
}

func (r *Registry) Send(ctx context.Context, k Kind, target, code string) error {
	s, ok := r.senders[k]
	if !ok {
		return fmt.Errorf("渠道 %s 未配置", k)
	}
	return s.Send(ctx, target, code)
}

func (r *Registry) Has(k Kind) bool {
	_, ok := r.senders[k]
	return ok
}
