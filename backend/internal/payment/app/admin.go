package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/li904338203/tabtv/backend/internal/payment/domain"
	"github.com/li904338203/tabtv/backend/internal/payment/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/platform/secret"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 渠道配置的读写与装配（P11-02）。
//
// ⚠️ 凭据**加密存库**（与 Provider 的 API key 同一套 `platform/secret`），
// 永不进配置响应与请求日志。

// ChannelAdmin 编排渠道配置。
type ChannelAdmin struct {
	repo   *repo.Repo
	cipher *secret.Cipher
	audit  *audit.Logger
	reg    *Registry
	client *http.Client
	log    *slog.Logger
}

func NewChannelAdmin(r *repo.Repo, c *secret.Cipher, reg *Registry,
	client *http.Client, log *slog.Logger) *ChannelAdmin {
	return &ChannelAdmin{repo: r, cipher: c, reg: reg, client: client, log: log}
}

func (a *ChannelAdmin) SetAudit(l *audit.Logger) { a.audit = l }

// ChannelView 见 transport 的同名类型（这里只回纯数据，不引 transport）。
type ChannelView struct {
	Code           string
	Name           string
	MerchantID     string
	AppID          string
	BaseURL        string
	NotifyURL      string
	IsSandbox      bool
	Status         string
	KeyVersion     int
	HasPrivateKey  bool
	HasPlatformKey bool
	HasAPIv3Key    bool
	Ready          bool
	Blocked        string
}

// List 列渠道。
func (a *ChannelAdmin) List(ctx context.Context) ([]ChannelView, error) {
	rows, err := a.repo.ListChannels(ctx)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	out := make([]ChannelView, 0, len(rows))
	for _, r := range rows {
		v := ChannelView{
			Code: r.Code, Name: r.Name, MerchantID: r.MerchantID, AppID: r.AppID,
			BaseURL: r.BaseURL, NotifyURL: r.NotifyURL, IsSandbox: r.IsSandbox,
			Status: r.Status, KeyVersion: r.KeyVersion,
			HasPrivateKey:  r.PrivateKeyCipher != "",
			HasPlatformKey: r.PlatformKeyCipher != "",
			HasAPIv3Key:    r.APIv3KeyCipher != "",
		}
		v.Blocked = blockedReason(r)
		v.Ready = v.Blocked == ""
		out = append(out, v)
	}
	return out, nil
}

// blockedReason 说清这个渠道**缺什么**。
//
// ⚠️ 只回一句"未就绪"的话，运维得逐个字段猜 —— 而支付渠道的字段有七八个，
// 其中三个还是看不见的密钥。与模型发布检查显示具体阻塞原因是同一条。
func blockedReason(r repo.ChannelRow) string {
	var missing []string
	if r.Status != "enabled" {
		missing = append(missing, "未启用")
	}
	if r.BaseURL == "" {
		missing = append(missing, "缺网关地址")
	}
	if r.NotifyURL == "" {
		missing = append(missing, "缺回调地址")
	}
	if r.PrivateKeyCipher == "" {
		missing = append(missing, "缺商户私钥")
	}
	if r.PlatformKeyCipher == "" {
		missing = append(missing, "缺渠道公钥（验签用）")
	}
	switch r.Code {
	case domain.ChannelWechat:
		if r.MerchantID == "" {
			missing = append(missing, "缺商户号")
		}
		if r.APIv3KeyCipher == "" {
			missing = append(missing, "缺 APIv3 密钥（解回调用）")
		}
	case domain.ChannelAlipay:
		if r.AppID == "" {
			missing = append(missing, "缺 appId")
		}
	}
	return strings.Join(missing, "、")
}

// validateChannelFields 逐列校验（列宽见 payment_channel 表）。
//
// ⚠️ 此前不校验：超长的商户号 / 地址在写库时变成 500；网关与回调地址不是 http(s) 的话
// 存得进去，要到第一笔真实下单或回调时才失败 —— 那时用户已经在付款页上了。
func validateChannelFields(in SaveChannel) error {
	if utf8.RuneCountInString(strings.TrimSpace(in.Name)) == 0 || utf8.RuneCountInString(in.Name) > 64 {
		return errors.New("渠道名称不能为空，且不超过 64 个字")
	}
	if len(in.MerchantID) > 128 || len(in.AppID) > 128 {
		return errors.New("商户号与 appId 都不能超过 128 个字符")
	}
	for _, u := range []struct{ what, v string }{{"网关地址", in.BaseURL}, {"回调地址", in.NotifyURL}} {
		if u.v == "" {
			continue
		}
		parsed, err := url.Parse(u.v)
		if len(u.v) > 512 || err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return errors.New(u.what + "必须是 http:// 或 https:// 开头的完整地址，且不超过 512 个字符")
		}
	}
	return nil
}

// SaveChannel 见 transport 的同名类型。
type SaveChannel struct {
	Code        string
	Name        string
	MerchantID  string
	AppID       string
	BaseURL     string
	NotifyURL   string
	IsSandbox   bool
	Status      string
	PrivateKey  *string
	PlatformKey *string
	APIv3Key    *string
}

// Save 保存渠道配置并**立即重新装配适配器**。
//
// ⚠️ 不重新装配的话，运维改完要重启进程才生效 —— 而支付凭据出问题时
// 正是最不能重启的时候（在途订单会一起中断）。
func (a *ChannelAdmin) Save(ctx context.Context, adminID int64, in SaveChannel) error {
	if in.Code != domain.ChannelWechat && in.Code != domain.ChannelAlipay {
		return httpx.Errf(errcode.InvalidArgument,
			"渠道码只能是 wechat 或 alipay（dev 是内置的开发支付，不在这里配）")
	}
	if in.Status != "enabled" && in.Status != "disabled" {
		in.Status = "disabled"
	}
	if err := validateChannelFields(in); err != nil {
		return httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	// ⚠️ **密钥在这里就要验一遍能不能解析**：存进去之后才发现格式不对的话，
	// 症状是"配好了但一到真实回调就验签失败"，而那时钱已经在路上了。
	if in.PrivateKey != nil && *in.PrivateKey != "" {
		if _, err := domain.ParsePrivateKey(*in.PrivateKey); err != nil {
			return httpx.Errf(errcode.InvalidArgument, "商户私钥无法解析："+err.Error())
		}
	}
	if in.PlatformKey != nil && *in.PlatformKey != "" {
		if _, err := domain.ParsePublicKey(*in.PlatformKey); err != nil {
			return httpx.Errf(errcode.InvalidArgument, "渠道公钥/证书无法解析："+err.Error())
		}
	}
	if in.APIv3Key != nil && *in.APIv3Key != "" && len(*in.APIv3Key) != 32 {
		return httpx.Errf(errcode.InvalidArgument,
			"微信 APIv3 密钥必须是 32 个字符（它是 ASCII 字符串，不是 base64）")
	}

	enc := func(p *string) (*string, error) {
		if p == nil {
			return nil, nil
		}
		if *p == "" {
			empty := ""
			return &empty, nil
		}
		c, err := a.cipher.Encrypt(*p)
		if err != nil {
			return nil, err
		}
		return &c, nil
	}
	priv, err := enc(in.PrivateKey)
	if err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	plat, err := enc(in.PlatformKey)
	if err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	v3, err := enc(in.APIv3Key)
	if err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	if err := a.repo.SaveChannel(ctx, repo.SaveChannelInput{
		Code: in.Code, Name: in.Name, MerchantID: in.MerchantID, AppID: in.AppID,
		BaseURL: in.BaseURL, NotifyURL: in.NotifyURL, IsSandbox: in.IsSandbox,
		Status: in.Status, PrivateKey: priv, PlatformKey: plat, APIv3Key: v3,
		KeyVersion: a.cipher.CurrentVersion(),
	}); err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	if a.audit != nil {
		// ⚠️ 审计里**绝不含密钥**：只记"改了哪个渠道、是不是沙箱、
		// 密钥换没换"。三个布尔就够了，值一个字都不能进去。
		a.audit.Write(ctx, audit.Entry{
			ActorType: audit.ActorAdmin, ActorID: adminID,
			Action: "payment_channel.save", ObjectType: "payment_channel",
			After: map[string]any{
				"code": in.Code, "status": in.Status, "isSandbox": in.IsSandbox,
				"privateKeyChanged":  in.PrivateKey != nil,
				"platformKeyChanged": in.PlatformKey != nil,
				"apiV3KeyChanged":    in.APIv3Key != nil,
			},
		})
	}
	return a.Reload(ctx)
}

// Callbacks 回调留痕（排障入口）。
func (a *ChannelAdmin) Callbacks(ctx context.Context, outTradeNo string,
	limit int) ([]repo.CallbackRow, error) {
	rows, err := a.repo.ListCallbacks(ctx, outTradeNo, limit)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return rows, nil
}

// Reload 按库里的配置重建适配器。
//
// ⚠️ 配置坏掉的渠道**跳过并告警，不让整个重载失败**：一个渠道的证书过期
// 不该把另一个渠道也一起关掉 —— 那会把故障面从一家放大到全部。
func (a *ChannelAdmin) Reload(ctx context.Context) error {
	rows, err := a.repo.EnabledChannels(ctx)
	if err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	for _, r := range rows {
		if reason := blockedReason(r); reason != "" {
			a.warn("支付渠道配置不完整，已跳过", "channel", r.Code, "blocked", reason)
			continue
		}
		ch, err := a.build(r)
		if err != nil {
			a.warn("支付渠道装配失败，已跳过", "channel", r.Code, "error", err)
			continue
		}
		a.reg.Register(ch)
		a.info("支付渠道已装配", "channel", r.Code, "sandbox", r.IsSandbox)
	}
	return nil
}

func (a *ChannelAdmin) build(r repo.ChannelRow) (Channel, error) {
	privPEM, err := a.cipher.Decrypt(r.PrivateKeyCipher)
	if err != nil {
		return nil, err
	}
	platPEM, err := a.cipher.Decrypt(r.PlatformKeyCipher)
	if err != nil {
		return nil, err
	}
	priv, err := domain.ParsePrivateKey(privPEM)
	if err != nil {
		return nil, err
	}
	plat, err := domain.ParsePublicKey(platPEM)
	if err != nil {
		return nil, err
	}
	switch r.Code {
	case domain.ChannelWechat:
		v3, err := a.cipher.Decrypt(r.APIv3KeyCipher)
		if err != nil {
			return nil, err
		}
		return NewWechatChannel(WechatConfig{
			MerchantID: r.MerchantID, AppID: r.AppID, PrivateKey: priv,
			PlatformKey: plat, APIv3Key: v3, BaseURL: r.BaseURL,
			NotifyURL: r.NotifyURL,
		}, a.client), nil
	case domain.ChannelAlipay:
		return NewAlipayChannel(AlipayConfig{
			AppID: r.AppID, PrivateKey: priv, PlatformKey: plat,
			BaseURL: r.BaseURL, NotifyURL: r.NotifyURL,
		}, a.client), nil
	}
	return nil, ErrChannelNotConfigured
}

// Channels 已装配的渠道码（供前端列可选渠道）。
func (a *ChannelAdmin) Channels() []string { return a.reg.Codes() }

func (a *ChannelAdmin) warn(msg string, args ...any) {
	if a.log != nil {
		a.log.Warn(msg, args...)
	}
}

func (a *ChannelAdmin) info(msg string, args ...any) {
	if a.log != nil {
		a.log.Info(msg, args...)
	}
}
