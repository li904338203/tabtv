package app

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/li904338203/tabtv/backend/internal/payment/domain"
)

// 支付宝适配器（RSA2 签名，参数按键名排序拼接）。
//
// ⚠️ 与微信适配器一样：**没有经过真实商户联调**（PARAM-03）。
// 签名/验签/金额换算都有 fixture 测试，请求形状按官方文档拼。

// AlipayConfig 是渠道配置（凭据已解密）。
type AlipayConfig struct {
	AppID string
	// PrivateKey 应用私钥，我们用它签名。
	PrivateKey *rsa.PrivateKey
	// PlatformKey 支付宝公钥，我们用它验回调的签名。
	PlatformKey *rsa.PublicKey
	BaseURL     string
	NotifyURL   string
	ReturnURL   string
}

// AlipayChannel 支付宝。
type AlipayChannel struct {
	cfg    AlipayConfig
	client *http.Client
}

func NewAlipayChannel(cfg AlipayConfig, client *http.Client) *AlipayChannel {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &AlipayChannel{cfg: cfg, client: client}
}

func (c *AlipayChannel) Code() string { return domain.ChannelAlipay }

// CallbackResponse 支付宝要的是**纯文本 `success`**（不是 JSON）。
//
// ⚠️ 答错的话支付宝会认为我们没收到，于是按 25 小时的节奏一直重试 ——
// 而我们每一次都在正确地处理并回一个它不认识的东西。
func (c *AlipayChannel) CallbackResponse() (int, string, []byte) {
	return http.StatusOK, "text/plain; charset=utf-8", []byte("success")
}

// commonParams 公共请求参数。
func (c *AlipayChannel) commonParams(method string, biz any) (map[string]string, error) {
	bizJSON, err := json.Marshal(biz)
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"app_id":    c.cfg.AppID,
		"method":    method,
		"format":    "JSON",
		"charset":   "utf-8",
		"sign_type": "RSA2",
		// ⚠️ 支付宝的时间戳是**本地时间字符串**（`2006-01-02 15:04:05`），
		// 不是 Unix 秒。给 Unix 秒的话上游只回一句"参数错误"。
		"timestamp":   time.Now().Format("2006-01-02 15:04:05"),
		"version":     "1.0",
		"notify_url":  c.cfg.NotifyURL,
		"biz_content": string(bizJSON),
	}, nil
}

// sign 给参数签名并填回 `sign`。
func (c *AlipayChannel) sign(params map[string]string) error {
	sig, err := domain.SignRSASHA256(c.cfg.PrivateKey, domain.AlipayMessage(params))
	if err != nil {
		return err
	}
	params["sign"] = sig
	return nil
}

// CreatePayment 预创建（扫码支付），拿回二维码链接。
func (c *AlipayChannel) CreatePayment(ctx context.Context, ref OrderRef) (
	PayParams, error) {
	params, err := c.commonParams("alipay.trade.precreate", map[string]any{
		"out_trade_no": ref.OutTradeNo,
		// ⚠️ 支付宝的金额单位是**元**的字符串，且必须两位小数。
		// 用浮点算的话 0.29 会变成 0.29000000000000004 —— 上游直接拒。
		"total_amount":    domain.CentsToYuan(ref.AmountCents),
		"subject":         ref.Subject,
		"timeout_express": "30m",
	})
	if err != nil {
		return PayParams{}, err
	}
	if err := c.sign(params); err != nil {
		return PayParams{}, err
	}
	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return PayParams{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")
	resp, err := c.client.Do(req)
	if err != nil {
		return PayParams{}, fmt.Errorf("支付宝下单请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		Response struct {
			Code   string `json:"code"`
			Msg    string `json:"msg"`
			SubMsg string `json:"sub_msg"`
			QRCode string `json:"qr_code"`
		} `json:"alipay_trade_precreate_response"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return PayParams{}, fmt.Errorf("支付宝下单返回解析失败: %s", truncate(string(raw), 300))
	}
	// ⚠️ 支付宝的业务错误**藏在 HTTP 200 里**（`code != "10000"`）——
	// 只看 HTTP 状态码的话，一次失败的下单会被当成成功，
	// 然后前端展示一个空二维码（与可灵把错误码放在 `code` 里是同一类）。
	if out.Response.Code != "10000" || out.Response.QRCode == "" {
		return PayParams{}, fmt.Errorf("支付宝下单失败 %s: %s %s",
			out.Response.Code, out.Response.Msg, out.Response.SubMsg)
	}
	return PayParams{
		Channel: c.Code(), Kind: "qrcode",
		Values:   map[string]string{"codeUrl": out.Response.QRCode},
		ExpireAt: ref.ExpireAt,
	}, nil
}

// VerifyAndParse 验签并解析表单回调。
func (c *AlipayChannel) VerifyAndParse(header http.Header, body []byte,
	_ time.Time) (domain.CallbackEvent, error) {
	// ⚠️ 支付宝回调是 **form 表单**不是 JSON。
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return domain.CallbackEvent{}, domain.ErrMalformed
	}
	params := map[string]string{}
	for k := range values {
		params[k] = values.Get(k)
	}
	sig := params["sign"]
	if sig == "" {
		return domain.CallbackEvent{}, domain.ErrMalformed
	}
	if c.cfg.PlatformKey == nil {
		return domain.CallbackEvent{}, ErrChannelNotConfigured
	}
	// ⚠️ **先验签再解析业务字段**。`AlipayMessage` 会跳过 sign/sign_type。
	if err := domain.VerifyRSASHA256(c.cfg.PlatformKey,
		domain.AlipayMessage(params), sig); err != nil {
		return domain.CallbackEvent{}, err
	}
	// ⚠️ 还要核对 `app_id`：验签只证明"这是支付宝发的"，
	// 不证明"这是发给我们这个应用的"。不核对的话，
	// 同一把支付宝公钥下别的应用的回调也能进来。
	if got := params["app_id"]; got != "" && got != c.cfg.AppID {
		return domain.CallbackEvent{}, fmt.Errorf(
			"%w：回调的 app_id 是 %s，不是本应用", domain.ErrSignatureInvalid, got)
	}
	return domain.ParseAlipayCallback(params)
}

// QueryOrder 主动查单。
func (c *AlipayChannel) QueryOrder(ctx context.Context, outTradeNo string) (
	domain.CallbackEvent, error) {
	params, err := c.commonParams("alipay.trade.query",
		map[string]any{"out_trade_no": outTradeNo})
	if err != nil {
		return domain.CallbackEvent{}, err
	}
	if err := c.sign(params); err != nil {
		return domain.CallbackEvent{}, err
	}
	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return domain.CallbackEvent{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")
	resp, err := c.client.Do(req)
	if err != nil {
		return domain.CallbackEvent{}, fmt.Errorf("支付宝查单请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		Response struct {
			Code        string `json:"code"`
			SubCode     string `json:"sub_code"`
			TradeNo     string `json:"trade_no"`
			TradeStatus string `json:"trade_status"`
			TotalAmount string `json:"total_amount"`
			SendPayDate string `json:"send_pay_date"`
		} `json:"alipay_trade_query_response"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return domain.CallbackEvent{}, domain.ErrMalformed
	}
	r := out.Response
	// ⚠️ "交易不存在"**不是错误**：用户可能根本没去付。
	// 判成错误的话对账页上全是红字，而绝大多数只是"还没付"。
	if r.SubCode == "ACQ.TRADE_NOT_EXIST" {
		return domain.CallbackEvent{
			OutTradeNo: outTradeNo, Succeeded: false, RawStatus: "TRADE_NOT_EXIST",
		}, nil
	}
	if r.Code != "10000" {
		return domain.CallbackEvent{}, fmt.Errorf("支付宝查单失败 %s/%s", r.Code, r.SubCode)
	}
	cents, err := domain.AlipayAmountCents(r.TotalAmount)
	if err != nil {
		return domain.CallbackEvent{}, err
	}
	paid, _ := time.ParseInLocation("2006-01-02 15:04:05", r.SendPayDate, time.Local)
	return domain.CallbackEvent{
		EventID: "query:" + outTradeNo, OutTradeNo: outTradeNo,
		ChannelTxnID: r.TradeNo, AmountCents: cents,
		Succeeded: r.TradeStatus == "TRADE_SUCCESS" || r.TradeStatus == "TRADE_FINISHED",
		RawStatus: r.TradeStatus, PaidAt: paid,
	}, nil
}

// Refund 发起退款。
//
// ⚠️ 支付宝的退款是**同步**的（与微信相反）：返回 `code=10000` 即已退。
// 两家的语义差异必须在适配器里抹平 —— 让上层去判"这家是同步那家是异步"的话，
// 加第三家时那段判断会再抄一遍。
func (c *AlipayChannel) Refund(ctx context.Context, in RefundRequest) (
	RefundResult, error) {
	params, err := c.commonParams("alipay.trade.refund", map[string]any{
		"out_trade_no":   in.OutTradeNo,
		"refund_amount":  domain.CentsToYuan(in.RefundCents),
		"out_request_no": in.OutRefundNo,
		"refund_reason":  in.Reason,
	})
	if err != nil {
		return RefundResult{}, err
	}
	if err := c.sign(params); err != nil {
		return RefundResult{}, err
	}
	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return RefundResult{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")
	resp, err := c.client.Do(req)
	if err != nil {
		// 同微信：结果不确定，不能当失败。幂等靠 `out_request_no`。
		return RefundResult{}, fmt.Errorf("支付宝退款请求失败（结果不确定，勿重复发起）: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		Response struct {
			Code       string `json:"code"`
			Msg        string `json:"msg"`
			SubMsg     string `json:"sub_msg"`
			TradeNo    string `json:"trade_no"`
			FundChange string `json:"fund_change"`
		} `json:"alipay_trade_refund_response"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return RefundResult{}, domain.ErrMalformed
	}
	r := out.Response
	// ⚠️ 业务错误**藏在 HTTP 200 里**（同下单与查单）。
	ok := r.Code == "10000"
	return RefundResult{
		// ⚠️ 支付宝不回独立的退款单号，用 `out_request_no` 兜底 ——
		// 留空的话对账时这一笔在两边对不上。
		ChannelRefundID: in.OutRefundNo, Succeeded: ok,
		RawStatus: r.Code + "/" + r.FundChange,
		Detail:    ifFailed(ok, r.Msg+" "+r.SubMsg),
	}, nil
}
