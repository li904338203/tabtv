package domain

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// 微信支付 APIv3 的回调解析。
//
// ⚠️ 回调的**业务数据是加密的**（AES-256-GCM，密钥是商户 APIv3 密钥）。
// 外层是明文信封（事件类型、资源摘要），里层才是订单信息。
// 直接读外层的 `summary` 当业务事实是错的 —— 那是给人看的文案。

// WechatEnvelope 是回调的明文外层。
type WechatEnvelope struct {
	ID           string `json:"id"`
	CreateTime   string `json:"create_time"`
	EventType    string `json:"event_type"`
	ResourceType string `json:"resource_type"`
	Resource     struct {
		Algorithm      string `json:"algorithm"`
		Ciphertext     string `json:"ciphertext"`
		AssociatedData string `json:"associated_data"`
		Nonce          string `json:"nonce"`
	} `json:"resource"`
}

// wechatTransaction 是解密之后的业务体（只取我们要的字段）。
type wechatTransaction struct {
	OutTradeNo    string `json:"out_trade_no"`
	TransactionID string `json:"transaction_id"`
	TradeState    string `json:"trade_state"`
	SuccessTime   string `json:"success_time"`
	Amount        struct {
		// ⚠️ 微信的金额单位**就是分**（int），不用换算 ——
		// 当成元再乘 100 的话每一笔都会放大一百倍。
		Total      int64 `json:"total"`
		PayerTotal int64 `json:"payer_total"`
	} `json:"amount"`
}

// DecryptWechatResource 解开回调里的 resource。
//
// ⚠️ `associated_data` 必须作为 **AAD** 传进去，不能忽略：
// 忽略的话解密会失败（GCM 会校验它），而错误信息只说"解密失败"。
func DecryptWechatResource(apiV3Key string, res struct {
	Algorithm      string `json:"algorithm"`
	Ciphertext     string `json:"ciphertext"`
	AssociatedData string `json:"associated_data"`
	Nonce          string `json:"nonce"`
}) ([]byte, error) {
	if res.Algorithm != "" && res.Algorithm != "AEAD_AES_256_GCM" {
		return nil, fmt.Errorf("%w：不支持的算法 %s", ErrMalformed, res.Algorithm)
	}
	// ⚠️ APIv3 密钥是 **32 字节的 ASCII 字符串**，不是 base64、不是 hex。
	// 当成 base64 解一遍的话长度对不上，报的却是 "invalid key size"。
	if len(apiV3Key) != 32 {
		return nil, errors.New("APIv3 密钥必须是 32 个字符")
	}
	raw, err := base64.StdEncoding.DecodeString(res.Ciphertext)
	if err != nil {
		return nil, ErrMalformed
	}
	block, err := aes.NewCipher([]byte(apiV3Key))
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(res.Nonce) != gcm.NonceSize() {
		return nil, fmt.Errorf("%w：nonce 长度应为 %d", ErrMalformed, gcm.NonceSize())
	}
	out, err := gcm.Open(nil, []byte(res.Nonce), raw, []byte(res.AssociatedData))
	if err != nil {
		// 解密失败等价于验签失败：要么密钥不对，要么报文被改过。
		return nil, ErrSignatureInvalid
	}
	return out, nil
}

// ParseWechatCallback 把已验签的回调报文翻成业务事实。
//
// ⚠️ **必须先验签再调这里**（见 CallbackEvent 的注释）。
func ParseWechatCallback(apiV3Key string, body []byte) (CallbackEvent, error) {
	var env WechatEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return CallbackEvent{}, ErrMalformed
	}
	if env.ID == "" {
		// 没有事件 ID 就没法做幂等 —— 宁可拒收也不能放进去。
		return CallbackEvent{}, fmt.Errorf("%w：缺少事件 id", ErrMalformed)
	}
	plain, err := DecryptWechatResource(apiV3Key, env.Resource)
	if err != nil {
		return CallbackEvent{}, err
	}
	var tx wechatTransaction
	if err := json.Unmarshal(plain, &tx); err != nil {
		return CallbackEvent{}, ErrMalformed
	}
	if tx.OutTradeNo == "" {
		return CallbackEvent{}, fmt.Errorf("%w：缺少 out_trade_no", ErrMalformed)
	}
	paid, _ := time.Parse(time.RFC3339, tx.SuccessTime)
	// ⚠️ 只有 `SUCCESS` 算成功。回调**不只有成功**（关闭、撤销、退款都会回调）——
	// 一律当成功处理的话，一笔"用户取消支付"的回调会发出权益。
	// 同时要求外层事件类型也是支付成功，两者不一致时按不成功处理。
	ok := tx.TradeState == "SUCCESS" && env.EventType == "TRANSACTION.SUCCESS"
	amount := tx.Amount.Total
	if amount == 0 {
		amount = tx.Amount.PayerTotal
	}
	return CallbackEvent{
		EventID: env.ID, OutTradeNo: tx.OutTradeNo,
		ChannelTxnID: tx.TransactionID, AmountCents: amount,
		Succeeded: ok, RawStatus: tx.TradeState, PaidAt: paid,
	}, nil
}

// ParseAlipayCallback 把支付宝的表单回调翻成业务事实。
//
// ⚠️ 支付宝回调是 **form 表单**不是 JSON，且**验签要用原始参数**
// （拼串前不能做任何转义/归一化）。
func ParseAlipayCallback(params map[string]string) (CallbackEvent, error) {
	outTradeNo := params["out_trade_no"]
	if outTradeNo == "" {
		return CallbackEvent{}, fmt.Errorf("%w：缺少 out_trade_no", ErrMalformed)
	}
	// ⚠️ 幂等键用 `notify_id`：支付宝对同一笔交易会推多次
	// （WAIT_BUYER_PAY → TRADE_SUCCESS），每次 notify_id 不同而 trade_no 相同。
	// 用 trade_no 做幂等的话，后一次状态变更会被当成重复丢掉。
	eventID := params["notify_id"]
	if eventID == "" {
		return CallbackEvent{}, fmt.Errorf("%w：缺少 notify_id", ErrMalformed)
	}
	cents, err := AlipayAmountCents(params["total_amount"])
	if err != nil {
		return CallbackEvent{}, err
	}
	status := params["trade_status"]
	paid, _ := time.ParseInLocation("2006-01-02 15:04:05",
		params["gmt_payment"], time.Local)
	return CallbackEvent{
		EventID: eventID, OutTradeNo: outTradeNo,
		ChannelTxnID: params["trade_no"], AmountCents: cents,
		// TRADE_SUCCESS 与 TRADE_FINISHED 都算收到钱；
		// WAIT_BUYER_PAY / TRADE_CLOSED 不是。
		Succeeded: status == "TRADE_SUCCESS" || status == "TRADE_FINISHED",
		RawStatus: status, PaidAt: paid,
	}, nil
}
