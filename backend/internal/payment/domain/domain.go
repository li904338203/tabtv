// Package domain 是支付渠道的纯规则层：签名、验签、回调信封解析。
//
// ⚠️ 这一层**不碰数据库、不发网络**，所以每条规则都能用 fixture 单独测 ——
// 而支付这一域的错误直接等于丢钱或白送权益，靠联调是验不全的
// （真实渠道的沙箱也不会替你构造一个"签名对但金额不对"的回调）。
//
// ⚠️ **这里没有任何真实凭据**。测试用的密钥对由测试自己现生成，
// 商户凭据是 PARAM-03，且**永远不进仓库**。
package domain

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 支付渠道码。与 `purchase_order.channel` 存的值一致。
const (
	// ChannelDev 开发支付。⚠️ **不是真实支付** —— 只在开发环境存在。
	ChannelDev = "dev"
	// ChannelWechat 微信支付 v3（APIv3，RSA-SHA256 签名 + AES-GCM 回调解密）。
	ChannelWechat = "wechat"
	// ChannelAlipay 支付宝（RSA2 签名，参数按键名排序后拼接）。
	ChannelAlipay = "alipay"
)

var (
	ErrSignatureInvalid = errors.New("回调签名验证失败")
	ErrReplayWindow     = errors.New("回调时间戳超出允许窗口")
	ErrAmountMismatch   = errors.New("回调金额与订单不一致")
	ErrUnsupported      = errors.New("不支持的支付渠道")
	ErrMalformed        = errors.New("回调报文格式不合法")
)

// ReplayWindow 回调时间戳的允许偏差。
//
// ⚠️ 这是**纵深防御**不是主要防线：完全相同的重放由
// `uk_callback_event(channel, channel_event_id)` 拦住。这个窗口挡的是
// "攻击者拿到一份历史签名、改掉 body 重放" —— 签名覆盖了时间戳，
// 所以他改不了时间戳；窗口让那份签名过一会儿就作废。
//
// ⚠️ 不能太小：渠道重试可能隔几分钟才来，两边时钟也会有几秒偏差。
// 微信官方建议 5 分钟。
const ReplayWindow = 5 * time.Minute

// CallbackEvent 是验签通过之后从回调里读出来的**业务事实**。
//
// ⚠️ 字段**只在验签之后填**：先解析业务字段再验签的话，
// 攻击者就能控制你解析什么（解析器本身就是攻击面）。
type CallbackEvent struct {
	// EventID 渠道侧的事件唯一号。幂等就靠它。
	EventID string
	// OutTradeNo 我们的订单号（下单时传给渠道的）。
	OutTradeNo string
	// ChannelTxnID 渠道侧的交易号，退款与查单要用。
	ChannelTxnID string
	// AmountCents 渠道说实际收到了多少钱（分）。
	//
	// ⚠️ **必须与订单核对**（见 VerifyAmount）：只看"状态=成功"的话，
	// 有人付 1 分钱就能拿走一年会员，而回调本身完全合法。
	AmountCents int64
	// Succeeded 这笔交易是不是成功了。
	//
	// ⚠️ 回调**不只有成功**：关闭、撤销、退款都会回调。
	// 一律当成功处理的话，一笔"用户取消支付"的回调会发出权益。
	Succeeded bool
	// RawStatus 渠道原文状态，落库用于排查。
	RawStatus string
	// PaidAt 渠道侧的支付时间。
	PaidAt time.Time
}

// VerifyAmount 核对金额。
//
// ⚠️ **只看"状态=成功"是不够的**：渠道只负责告诉你"用户付了多少"，
// "付够没付够"是我们的事。不核对的话，攻击者改掉下单金额（前端可控）
// 或直接用另一笔小额订单的回调，就能白拿权益 —— 而每一步都"合法"。
//
// ⚠️ 多付**不算错**（渠道优惠、用户重复支付都可能），但要能看出来：
// 返回 nil 但由调用方记一条告警。少付一分钱都必须拒。
func VerifyAmount(event CallbackEvent, orderCents int64) error {
	if event.AmountCents < orderCents {
		return fmt.Errorf("%w：订单 %d 分，实收 %d 分",
			ErrAmountMismatch, orderCents, event.AmountCents)
	}
	return nil
}

// ---------------------------------------------------------------- RSA 签名

// ParsePrivateKey 读 PKCS#1 或 PKCS#8 的 PEM 私钥。
//
// ⚠️ 两种格式都要支持：微信给的是 PKCS#8（`BEGIN PRIVATE KEY`），
// 支付宝控制台导出的常是 PKCS#1（`BEGIN RSA PRIVATE KEY`）。
// 只支持一种的话，运维把另一种贴进来会得到一句"解析失败"，
// 而他看不出差别在哪 —— PEM 头部那行没人会逐字比对。
func ParsePrivateKey(pemText string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(pemText)))
	if block == nil {
		return nil, errors.New("不是合法的 PEM（检查是否漏了 BEGIN/END 行）")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	any, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("私钥解析失败（PKCS#1/#8 都不匹配）: %w", err)
	}
	k, ok := any.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("私钥不是 RSA")
	}
	return k, nil
}

// ParsePublicKey 从 PEM 公钥或 X.509 证书里取 RSA 公钥。
//
// ⚠️ 证书也要支持：微信下发的平台公钥是一张**证书**，
// 而支付宝给的是裸公钥。只认一种的话另一种会被判成"格式不合法"。
func ParsePublicKey(pemText string) (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(pemText)))
	if block == nil {
		return nil, errors.New("不是合法的 PEM")
	}
	if cert, err := x509.ParseCertificate(block.Bytes); err == nil {
		k, ok := cert.PublicKey.(*rsa.PublicKey)
		if !ok {
			return nil, errors.New("证书里的公钥不是 RSA")
		}
		return k, nil
	}
	any, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("公钥解析失败: %w", err)
	}
	k, ok := any.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("公钥不是 RSA")
	}
	return k, nil
}

// SignRSASHA256 用 RSASSA-PKCS1-v1_5 + SHA256 签名，返回 base64。
func SignRSASHA256(key *rsa.PrivateKey, message string) (string, error) {
	h := sha256.Sum256([]byte(message))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h[:])
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

// VerifyRSASHA256 验签。
//
// ⚠️ 失败一律回 `ErrSignatureInvalid` 而**不带底层原因**：
// 区分"base64 坏了"和"签名对不上"会把验签过程的细节泄漏给调用方，
// 而调用方是外网。日志里可以记详细原因。
func VerifyRSASHA256(key *rsa.PublicKey, message, signatureB64 string) error {
	sig, err := base64.StdEncoding.DecodeString(signatureB64)
	if err != nil {
		return ErrSignatureInvalid
	}
	h := sha256.Sum256([]byte(message))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, h[:], sig); err != nil {
		return ErrSignatureInvalid
	}
	return nil
}

// ---------------------------------------------------------------- 微信 v3

// WechatMessage 拼微信 APIv3 的待签名串。
//
// 格式是**四行，每行以 \n 结尾**（最后一行也要）：
//
//	HTTP方法\nURL路径\n时间戳\n随机串\n请求体\n
//
// ⚠️ 最后那个 `\n` 漏了的话签名一定对不上，而错误信息只会说"签名错误"——
// 这是接微信最常见的一个坑。回调验签的格式是三行 + body：
// 时间戳\n随机串\n报文主体\n
func WechatMessage(timestamp, nonce, body string) string {
	return timestamp + "\n" + nonce + "\n" + body + "\n"
}

// WechatRequestMessage 拼**请求**方向的待签名串（我们调微信时用）。
func WechatRequestMessage(method, urlPath, timestamp, nonce, body string) string {
	return method + "\n" + urlPath + "\n" + timestamp + "\n" + nonce + "\n" + body + "\n"
}

// CheckTimestamp 校验回调时间戳在允许窗口内。
//
// ⚠️ 用**绝对值**：时钟快的那一侧会送来"未来"的时间戳，
// 只判"太旧"的话那一侧永远过不了。
func CheckTimestamp(tsUnix string, now time.Time) error {
	sec, err := strconv.ParseInt(strings.TrimSpace(tsUnix), 10, 64)
	if err != nil {
		return ErrMalformed
	}
	d := now.Sub(time.Unix(sec, 0))
	if d < 0 {
		d = -d
	}
	if d > ReplayWindow {
		return ErrReplayWindow
	}
	return nil
}

// ---------------------------------------------------------------- 支付宝

// AlipayMessage 拼支付宝的待签名串：参数按**键名字典序**排序后
// `k=v&k=v` 拼接，跳过空值与 `sign` / `sign_type` 本身。
//
// ⚠️ 跳过空值是规则的一部分：把空值也拼进去的话签名一定对不上，
// 而支付宝只回一句"验签失败"。
//
// ⚠️ 跳过 `sign_type`：它是历史遗留，老文档里有两种说法 ——
// 实测 RSA2 下**不参与**签名。参与了的话同样只得到"验签失败"。
func AlipayMessage(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k, v := range params {
		if v == "" || k == "sign" || k == "sign_type" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(params[k])
	}
	return b.String()
}

// AlipayAmountCents 把支付宝的"元"字符串翻成整数分。
//
// ⚠️ **不能用 `strconv.ParseFloat` 再乘 100**：`"0.29"` 乘出来是
// 28.999999999999996，截断后成了 28 分 —— 每一笔都少算一分，
// 而对账时谁也说不清差额从哪来。按字符串切小数点。
func AlipayAmountCents(yuan string) (int64, error) {
	s := strings.TrimSpace(yuan)
	if s == "" {
		return 0, ErrMalformed
	}
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	intPart, frac, _ := strings.Cut(s, ".")
	if intPart == "" {
		intPart = "0"
	}
	// 补齐/截断到两位小数。
	frac = (frac + "00")[:2]
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, ErrMalformed
	}
	cents, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, ErrMalformed
	}
	v := whole*100 + cents
	if neg {
		v = -v
	}
	return v, nil
}

// CentsToYuan 整数分翻成支付宝要的"元"字符串（两位小数）。
func CentsToYuan(cents int64) string {
	neg := cents < 0
	if neg {
		cents = -cents
	}
	s := fmt.Sprintf("%d.%02d", cents/100, cents%100)
	if neg {
		return "-" + s
	}
	return s
}

// CallbackRecord 是一条回调的落库内容。
//
// ⚠️ 放在 domain 而不是 app：仓储要用它，而**仓储不能 import app**
// （分层是 `app → repo 接口`、`repo impl → platform/db`）。
// 放 app 里的话会出现 import 环，而那时的修法多半是再造一个转换类型
// —— 两份形状早晚不一致且没有编译错误。
type CallbackRecord struct {
	Channel      string
	EventID      string
	RawPayload   []byte
	SignatureOK  bool
	Stage        string
	OutTradeNo   string
	AmountCents  int64
	ErrorMessage string
}
