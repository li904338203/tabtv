package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/li904338203/tabtv/backend/internal/payment/domain"
)

// 微信支付 APIv3 适配器。
//
// ⚠️ 这一份**没有经过真实商户联调**（PARAM-03：需商户凭据与沙箱环境）。
// 签名、验签、回调解密都有 fixture 测试覆盖；请求形状按官方文档拼，
// 但"文档与真实网关的差异"只有真实联调能发现 —— 与 Provider 那边
// 「假上游的绿色不等于真实上游验收」是同一条。

// WechatConfig 是渠道配置（凭据已解密）。
type WechatConfig struct {
	MerchantID string
	AppID      string
	// PrivateKey 商户私钥，我们用它签名。
	PrivateKey *rsa.PrivateKey
	// SerialNo 商户证书序列号，放在 Authorization 头里。
	SerialNo string
	// PlatformKey 微信平台公钥，我们用它验回调的签名。
	PlatformKey *rsa.PublicKey
	// APIv3Key 对称密钥，解回调里的密文。
	APIv3Key  string
	BaseURL   string
	NotifyURL string
}

// WechatChannel 微信支付。
type WechatChannel struct {
	cfg    WechatConfig
	client *http.Client
}

func NewWechatChannel(cfg WechatConfig, client *http.Client) *WechatChannel {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &WechatChannel{cfg: cfg, client: client}
}

func (c *WechatChannel) Code() string { return domain.ChannelWechat }

// CallbackResponse 微信要 200 + `{"code":"SUCCESS"}`。
func (c *WechatChannel) CallbackResponse() (int, string, []byte) {
	return http.StatusOK, "application/json",
		[]byte(`{"code":"SUCCESS","message":"成功"}`)
}

// nonce 生成随机串。
func nonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// authorization 拼 APIv3 的 Authorization 头。
//
// ⚠️ 待签名串是**五行**（方法、路径、时间戳、随机串、请求体），
// 每行以 `\n` 结尾 —— 最后一行也要。漏掉末尾那个换行是接微信最常见的坑，
// 而上游只回一句"签名错误"。
//
// ⚠️ 签的路径必须是**网关看到的那个路径**，所以要把 `baseUrl` 里的
// 路径前缀也算进去。真实微信的 baseUrl 是纯域名（前缀为空），两种写法
// 结果一样；但接在带前缀的网关/沙箱后面时，只签末段会一律 401，
// 而错误信息只说"签名错误"—— 这一条是联调时实测撞到的。
// ⚠️ 会**改写路径**的反向代理不受支持：那种情况下没有任何一侧算得对。
func (c *WechatChannel) authorization(method, urlPath, body string) (string, error) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	n := nonce()
	msg := domain.WechatRequestMessage(method, c.signPath(urlPath), ts, n, body)
	sig, err := domain.SignRSASHA256(c.cfg.PrivateKey, msg)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(
		`WECHATPAY2-SHA256-RSA2048 mchid="%s",nonce_str="%s",signature="%s",timestamp="%s",serial_no="%s"`,
		c.cfg.MerchantID, n, sig, ts, c.cfg.SerialNo), nil
}

// CreatePayment 调 Native 下单，拿回二维码链接。
func (c *WechatChannel) CreatePayment(ctx context.Context, ref OrderRef) (
	PayParams, error) {
	const path = "/v3/pay/transactions/native"
	payload := map[string]any{
		"appid": c.cfg.AppID, "mchid": c.cfg.MerchantID,
		"description": ref.Subject, "out_trade_no": ref.OutTradeNo,
		"notify_url": c.cfg.NotifyURL,
		// ⚠️ 微信的金额单位**就是分**，不要再乘 100。
		"amount": map[string]any{"total": ref.AmountCents, "currency": "CNY"},
		// ⚠️ 下单时就把失效时间告诉渠道：不告诉的话渠道那边的单
		// 比我们的订单活得久，用户在我们已经关单之后还能付钱 ——
		// 那笔钱进来了而订单是 expired，只能人工退。
		"time_expire": ref.ExpireAt.Format(time.RFC3339),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return PayParams{}, err
	}
	auth, err := c.authorization(http.MethodPost, path, string(body))
	if err != nil {
		return PayParams{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.cfg.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return PayParams{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", auth)
	resp, err := c.client.Do(req)
	if err != nil {
		return PayParams{}, fmt.Errorf("微信下单请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		// ⚠️ 状态码与渠道错误码**原样带回**（截断后）：判成一句
		// "下单失败"的话，运维查不出是证书不对、金额超限还是订单号重复。
		return PayParams{}, fmt.Errorf("微信下单失败 HTTP %d: %s",
			resp.StatusCode, truncate(string(raw), 300))
	}
	var out struct {
		CodeURL string `json:"code_url"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.CodeURL == "" {
		return PayParams{}, fmt.Errorf("微信下单返回里没有 code_url: %s",
			truncate(string(raw), 300))
	}
	return PayParams{
		Channel: c.Code(), Kind: "qrcode",
		Values: map[string]string{"codeUrl": out.CodeURL}, ExpireAt: ref.ExpireAt,
	}, nil
}

// VerifyAndParse 验签 → 查重放窗口 → 解密解析。
func (c *WechatChannel) VerifyAndParse(header http.Header, body []byte,
	now time.Time) (domain.CallbackEvent, error) {
	ts := header.Get("Wechatpay-Timestamp")
	n := header.Get("Wechatpay-Nonce")
	sig := header.Get("Wechatpay-Signature")
	if ts == "" || n == "" || sig == "" {
		return domain.CallbackEvent{}, domain.ErrMalformed
	}
	// ⚠️ **先验签，再看里面写了什么**。
	if c.cfg.PlatformKey == nil {
		return domain.CallbackEvent{}, ErrChannelNotConfigured
	}
	msg := domain.WechatMessage(ts, n, string(body))
	if err := domain.VerifyRSASHA256(c.cfg.PlatformKey, msg, sig); err != nil {
		return domain.CallbackEvent{}, err
	}
	// 重放窗口是纵深防御：完全相同的重放由 uk_callback_event 拦住，
	// 这里挡的是"拿一份历史签名改 body 重放"。
	if err := domain.CheckTimestamp(ts, now); err != nil {
		return domain.CallbackEvent{}, err
	}
	return domain.ParseWechatCallback(c.cfg.APIv3Key, body)
}

// QueryOrder 按商户订单号查单。
func (c *WechatChannel) QueryOrder(ctx context.Context, outTradeNo string) (
	domain.CallbackEvent, error) {
	path := "/v3/pay/transactions/out-trade-no/" + outTradeNo +
		"?mchid=" + c.cfg.MerchantID
	auth, err := c.authorization(http.MethodGet, path, "")
	if err != nil {
		return domain.CallbackEvent{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.BaseURL+path, nil)
	if err != nil {
		return domain.CallbackEvent{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", auth)
	resp, err := c.client.Do(req)
	if err != nil {
		return domain.CallbackEvent{}, fmt.Errorf("微信查单请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound {
		// ⚠️ 查不到**不是错误**：用户可能根本没去付。
		// 判成错误的话对账页上全是红字，而绝大多数只是"还没付"。
		return domain.CallbackEvent{
			OutTradeNo: outTradeNo, Succeeded: false, RawStatus: "NOTPAY",
		}, nil
	}
	if resp.StatusCode >= 300 {
		return domain.CallbackEvent{}, fmt.Errorf("微信查单失败 HTTP %d: %s",
			resp.StatusCode, truncate(string(raw), 300))
	}
	var tx struct {
		OutTradeNo    string `json:"out_trade_no"`
		TransactionID string `json:"transaction_id"`
		TradeState    string `json:"trade_state"`
		SuccessTime   string `json:"success_time"`
		Amount        struct {
			Total      int64 `json:"total"`
			PayerTotal int64 `json:"payer_total"`
		} `json:"amount"`
	}
	if err := json.Unmarshal(raw, &tx); err != nil {
		return domain.CallbackEvent{}, domain.ErrMalformed
	}
	paid, _ := time.Parse(time.RFC3339, tx.SuccessTime)
	amount := tx.Amount.Total
	if amount == 0 {
		amount = tx.Amount.PayerTotal
	}
	return domain.CallbackEvent{
		// ⚠️ 查单结果的幂等键用 `query:订单号`：它和回调走同一张
		// `payment_callback` 表，不给键的话两者会互相覆盖，
		// 而"回调来过没有"是对账时最要紧的一条信息。
		EventID: "query:" + outTradeNo, OutTradeNo: tx.OutTradeNo,
		ChannelTxnID: tx.TransactionID, AmountCents: amount,
		Succeeded: tx.TradeState == "SUCCESS", RawStatus: tx.TradeState,
		PaidAt: paid,
	}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// signPath 把 baseUrl 的路径前缀拼到端点路径前面 —— 见 authorization 的注释。
func (c *WechatChannel) signPath(urlPath string) string {
	u, err := url.Parse(c.cfg.BaseURL)
	if err != nil || u.Path == "" || u.Path == "/" {
		return urlPath
	}
	return strings.TrimSuffix(u.Path, "/") + urlPath
}

// Refund 发起退款。
//
// ⚠️ 微信的退款是**异步**的：这里的成功只表示渠道受理了。
// 真正到账要等退款回调（`REFUND.SUCCESS`）—— 当前把受理即当成功是
// 设计取舍，见 `RefundResult.Succeeded` 的注释。
func (c *WechatChannel) Refund(ctx context.Context, in RefundRequest) (
	RefundResult, error) {
	const path = "/v3/refund/domestic/refunds"
	payload := map[string]any{
		"out_trade_no":  in.OutTradeNo,
		"out_refund_no": in.OutRefundNo,
		"reason":        truncate(in.Reason, 80),
		// ⚠️ `total` 是**原订单金额**、`refund` 是本次退多少。
		// 只传后者的话上游一律报参数错误，而错误信息指不出少了哪个。
		"amount": map[string]any{
			"refund": in.RefundCents, "total": in.TotalCents, "currency": "CNY",
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return RefundResult{}, err
	}
	auth, err := c.authorization(http.MethodPost, path, string(body))
	if err != nil {
		return RefundResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.cfg.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return RefundResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", auth)
	resp, err := c.client.Do(req)
	if err != nil {
		// ⚠️ 连不上/超时时**不能当成失败**：请求可能已经到了上游。
		// 判成失败并让用户重试的话，渠道那边会退第二笔 ——
		// 靠 `out_refund_no` 的幂等兜底，但状态要如实报"不确定"。
		return RefundResult{}, fmt.Errorf("微信退款请求失败（结果不确定，勿重复发起）: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return RefundResult{
			Succeeded: false, RawStatus: strconv.Itoa(resp.StatusCode),
			Detail: truncate(string(raw), 300),
		}, nil
	}
	var out struct {
		RefundID string `json:"refund_id"`
		Status   string `json:"status"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return RefundResult{}, domain.ErrMalformed
	}
	// 微信的退款状态：SUCCESS / PROCESSING / CLOSED / ABNORMAL。
	// ⚠️ `PROCESSING` 也算受理成功 —— 它就是异步退款的正常状态。
	ok := out.Status == "SUCCESS" || out.Status == "PROCESSING"
	return RefundResult{
		ChannelRefundID: out.RefundID, Succeeded: ok, RawStatus: out.Status,
		Detail: ifFailed(ok, truncate(string(raw), 300)),
	}, nil
}

func ifFailed(ok bool, detail string) string {
	if ok {
		return ""
	}
	return detail
}
