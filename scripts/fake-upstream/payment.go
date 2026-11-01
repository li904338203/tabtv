package main

import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 假支付渠道（P11-02 联调用）。
//
// ⚠️ **这里的所有密钥都是进程启动时现生成的，只发给本机**。
// 真实商户凭据是 PARAM-03，永远不进仓库。
//
// 它验的是 fixture 测试验不到的三件事：
//  1. 我们拼的 `Authorization` 头**上游认不认**（按微信 APIv3 的规则验签）；
//  2. 回调从**网络另一头**真的送得进来（签名 + AES-GCM 加密的完整形状）；
//  3. 查单链路通不通。
//
// ⚠️ 它仍然**不是那家厂商**（AC-09 的同一条）：真实网关的字段差异、
// 限流、证书轮换都验不到。

type payState struct {
	// merchantPub 我们签请求用的私钥对应的公钥 —— 上游用它验我们的签名。
	merchantPriv *rsa.PrivateKey
	// platformPriv 上游签回调用的私钥；我们配的是它的公钥。
	platformPriv *rsa.PrivateKey
	apiV3Key     string
	// paid 记哪些订单"付过了"，供查单用。
	paid map[string]int64
	// refunded 记退过的订单，验"同一个 out_refund_no 只退一笔"。
	refunded map[string]int64
	// refundFail 让脚本能指名让某一笔退款失败（验失败路径）。
	refundFail map[string]bool
}

func newPayState() *payState {
	m, _ := rsa.GenerateKey(rand.Reader, 2048)
	p, _ := rsa.GenerateKey(rand.Reader, 2048)
	return &payState{
		merchantPriv: m, platformPriv: p,
		// 32 字符，测试专用。
		apiV3Key:   "feedfacedeadbeeffeedfacedeadbeef",
		paid:       map[string]int64{},
		refunded:   map[string]int64{},
		refundFail: map[string]bool{},
	}
}

func privPEM(k *rsa.PrivateKey) string {
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func pubPEM(k *rsa.PrivateKey) string {
	der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// payKeys 把联调要用的密钥发给脚本。
//
// ⚠️ 这个端点只存在于假上游。真实渠道的私钥**永远不会**由上游下发。
func (s *payState) payKeys(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]string{
		// 我们（商户）签请求用的私钥。
		"merchantPrivateKey": privPEM(s.merchantPriv),
		// 我们验回调用的公钥。
		"platformPublicKey": pubPEM(s.platformPriv),
		"apiV3Key":          s.apiV3Key,
	})
}

// verifyWechatAuth 验我们发过来的 Authorization 头。
//
// ⚠️ 这一步是这个假上游存在的核心理由之一：待签名串是**五行、
// 每行以 \n 结尾**（最后一行也要）。漏掉末尾那个换行是接微信最常见的坑，
// 而真实网关只回一句"签名错误"—— 在这里能立刻指出是哪一环。
func (s *payState) verifyWechatAuth(r *http.Request, body string) error {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "WECHATPAY2-SHA256-RSA2048 ") {
		return fmt.Errorf("Authorization 头格式不对：%q", auth)
	}
	fields := map[string]string{}
	for _, part := range strings.Split(strings.TrimPrefix(auth,
		"WECHATPAY2-SHA256-RSA2048 "), ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		fields[k] = strings.Trim(v, `"`)
	}
	for _, k := range []string{"mchid", "nonce_str", "signature", "timestamp"} {
		if fields[k] == "" {
			return fmt.Errorf("Authorization 里缺 %s", k)
		}
	}
	path := r.URL.Path
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	msg := r.Method + "\n" + path + "\n" + fields["timestamp"] + "\n" +
		fields["nonce_str"] + "\n" + body + "\n"
	sig, err := base64.StdEncoding.DecodeString(fields["signature"])
	if err != nil {
		return fmt.Errorf("签名不是合法 base64")
	}
	h := sha256.Sum256([]byte(msg))
	if err := rsa.VerifyPKCS1v15(&s.merchantPriv.PublicKey, crypto.SHA256,
		h[:], sig); err != nil {
		return fmt.Errorf("签名验证失败（检查待签名串是否为五行且每行以 \\n 结尾）")
	}
	return nil
}

// wechatNative 假的 Native 下单。
func (s *payState) wechatNative(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err := s.verifyWechatAuth(r, string(raw)); err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]string{"code": "SIGN_ERROR", "message": err.Error()})
		return
	}
	var req struct {
		OutTradeNo string `json:"out_trade_no"`
		Amount     struct {
			Total int64 `json:"total"`
		} `json:"amount"`
	}
	_ = json.Unmarshal(raw, &req)
	if req.OutTradeNo == "" {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"code": "PARAM_ERROR", "message": "缺 out_trade_no"})
		return
	}
	writeJSON(w, map[string]string{
		"code_url": "weixin://wxpay/bizpayurl?pr=fake-" + req.OutTradeNo,
	})
}

// wechatQuery 假的查单。
func (s *payState) wechatQuery(w http.ResponseWriter, r *http.Request) {
	if err := s.verifyWechatAuth(r, ""); err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]string{"code": "SIGN_ERROR", "message": err.Error()})
		return
	}
	no := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	amount, ok := s.paid[no]
	if !ok {
		// ⚠️ 没付过回 404 —— 我们这边要把它当成"还没付"而不是错误。
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, map[string]string{"code": "ORDER_NOT_EXIST"})
		return
	}
	writeJSON(w, map[string]any{
		"out_trade_no": no, "transaction_id": "fake-txn-" + no,
		"trade_state": "SUCCESS", "success_time": time.Now().Format(time.RFC3339),
		"amount": map[string]any{"total": amount, "payer_total": amount},
	})
}

// fireWechatCallback 构造一条**真实形状**的微信回调并发给我们的 API。
//
// 参数：`target`（回调地址）· `outTradeNo` · `amount`（分）·
// `state`（trade_state，默认 SUCCESS）· `badSign=1` 故意签坏。
func (s *payState) fireWechatCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	amount, _ := strconv.ParseInt(q.Get("amount"), 10, 64)
	state := q.Get("state")
	if state == "" {
		state = "SUCCESS"
	}
	eventType := q.Get("eventType")
	if eventType == "" {
		eventType = "TRANSACTION.SUCCESS"
	}
	tx, _ := json.Marshal(map[string]any{
		"out_trade_no":   q.Get("outTradeNo"),
		"transaction_id": "fake-txn-" + q.Get("outTradeNo"),
		"trade_state":    state,
		"success_time":   time.Now().Format(time.RFC3339),
		"amount":         map[string]any{"total": amount, "payer_total": amount},
	})
	ct, nonce := s.seal(string(tx), "transaction")
	eventID := q.Get("eventId")
	if eventID == "" {
		eventID = "evt-" + q.Get("outTradeNo")
	}
	body, _ := json.Marshal(map[string]any{
		"id": eventID, "create_time": time.Now().Format(time.RFC3339),
		"event_type": eventType, "resource_type": "encrypt-resource",
		"resource": map[string]any{
			"algorithm": "AEAD_AES_256_GCM", "ciphertext": ct,
			"associated_data": "transaction", "nonce": nonce,
		},
	})
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	n := "nonce" + ts
	msg := ts + "\n" + n + "\n" + string(body) + "\n"
	h := sha256.Sum256([]byte(msg))
	raw, _ := rsa.SignPKCS1v15(rand.Reader, s.platformPriv, crypto.SHA256, h[:])
	sig := base64.StdEncoding.EncodeToString(raw)
	if q.Get("badSign") == "1" {
		// 故意签坏 —— 验"验签失败会被拒且留痕"。
		sig = base64.StdEncoding.EncodeToString([]byte("not a signature"))
	}
	if state == "SUCCESS" && q.Get("badSign") != "1" {
		s.paid[q.Get("outTradeNo")] = amount
	}

	req, _ := http.NewRequest(http.MethodPost, q.Get("target"),
		strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Wechatpay-Timestamp", ts)
	req.Header.Set("Wechatpay-Nonce", n)
	req.Header.Set("Wechatpay-Signature", sig)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	// 把我们那边的应答**原样回**给脚本 —— 脚本要断言状态码与应答体。
	writeJSON(w, map[string]any{
		"status": resp.StatusCode, "body": string(respBody),
	})
}

func (s *payState) seal(plaintext, aad string) (ciphertext, nonce string) {
	block, _ := aes.NewCipher([]byte(s.apiV3Key))
	gcm, _ := cipher.NewGCM(block)
	n := make([]byte, gcm.NonceSize())
	_, _ = rand.Read(n)
	for i := range n {
		n[i] = byte('a' + n[i]%26)
	}
	out := gcm.Seal(nil, n, []byte(plaintext), []byte(aad))
	return base64.StdEncoding.EncodeToString(out), string(n)
}

// alipayGateway 假的支付宝网关（precreate + query 共用一个地址）。
func (s *payState) alipayGateway(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	params := map[string]string{}
	for k := range r.Form {
		params[k] = r.Form.Get(k)
	}
	// ⚠️ 验我们的签名：排序 + 跳过空值 + 跳过 sign/sign_type。
	if err := s.verifyAlipaySign(params); err != nil {
		writeJSON(w, map[string]any{
			"alipay_trade_precreate_response": map[string]string{
				"code": "40002", "msg": "Invalid Arguments", "sub_msg": err.Error(),
			},
		})
		return
	}
	var biz struct {
		OutTradeNo string `json:"out_trade_no"`
	}
	_ = json.Unmarshal([]byte(params["biz_content"]), &biz)
	switch params["method"] {
	case "alipay.trade.precreate":
		writeJSON(w, map[string]any{
			"alipay_trade_precreate_response": map[string]string{
				"code": "10000", "msg": "Success",
				"out_trade_no": biz.OutTradeNo,
				"qr_code":      "https://qr.alipay.com/fake-" + biz.OutTradeNo,
			},
		})
	case "alipay.trade.query":
		amount, ok := s.paid[biz.OutTradeNo]
		if !ok {
			writeJSON(w, map[string]any{
				"alipay_trade_query_response": map[string]string{
					"code": "40004", "sub_code": "ACQ.TRADE_NOT_EXIST",
				},
			})
			return
		}
		writeJSON(w, map[string]any{
			"alipay_trade_query_response": map[string]string{
				"code": "10000", "trade_no": "fake-" + biz.OutTradeNo,
				"trade_status":  "TRADE_SUCCESS",
				"total_amount":  fmt.Sprintf("%d.%02d", amount/100, amount%100),
				"send_pay_date": time.Now().Format("2006-01-02 15:04:05"),
			},
		})
	case "alipay.trade.refund":
		// ⚠️ 支付宝的退款是**同步**的（与微信相反）：回 10000 即已退。
		// 两家的语义差异要在适配器里抹平，这里如实模拟差异本身。
		if s.refundFail[biz.OutTradeNo] {
			writeJSON(w, map[string]any{
				"alipay_trade_refund_response": map[string]string{
					"code": "40004", "msg": "Business Failed",
					"sub_msg": "余额不足（测试注入）",
				},
			})
			return
		}
		paidCents, ok := s.paid[biz.OutTradeNo]
		if !ok {
			writeJSON(w, map[string]any{
				"alipay_trade_refund_response": map[string]string{
					"code": "40004", "sub_msg": "ACQ.TRADE_NOT_EXIST",
				},
			})
			return
		}
		var refundBiz struct {
			RefundAmount string `json:"refund_amount"`
			OutRequestNo string `json:"out_request_no"`
		}
		_ = json.Unmarshal([]byte(params["biz_content"]), &refundBiz)
		s.refunded[refundBiz.OutRequestNo] = paidCents
		writeJSON(w, map[string]any{
			"alipay_trade_refund_response": map[string]string{
				"code": "10000", "msg": "Success",
				"trade_no": "fake-" + biz.OutTradeNo, "fund_change": "Y",
				"refund_fee": refundBiz.RefundAmount,
			},
		})
	default:
		writeJSON(w, map[string]any{"error": "未知 method " + params["method"]})
	}
}

func alipayMessage(params map[string]string) string {
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
		b.WriteString(k + "=" + params[k])
	}
	return b.String()
}

func (s *payState) verifyAlipaySign(params map[string]string) error {
	sig, err := base64.StdEncoding.DecodeString(params["sign"])
	if err != nil {
		return fmt.Errorf("sign 不是合法 base64")
	}
	h := sha256.Sum256([]byte(alipayMessage(params)))
	if err := rsa.VerifyPKCS1v15(&s.merchantPriv.PublicKey, crypto.SHA256,
		h[:], sig); err != nil {
		return fmt.Errorf("验签失败（检查排序、跳过空值与 sign/sign_type）")
	}
	return nil
}

// fireAlipayCallback 构造一条**真实形状**的支付宝表单回调。
func (s *payState) fireAlipayCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	amount, _ := strconv.ParseInt(q.Get("amount"), 10, 64)
	status := q.Get("status")
	if status == "" {
		status = "TRADE_SUCCESS"
	}
	notifyID := q.Get("notifyId")
	if notifyID == "" {
		notifyID = "notify-" + q.Get("outTradeNo")
	}
	params := map[string]string{
		"app_id": q.Get("appId"), "notify_id": notifyID,
		"out_trade_no": q.Get("outTradeNo"),
		"trade_no":     "fake-" + q.Get("outTradeNo"),
		"total_amount": fmt.Sprintf("%d.%02d", amount/100, amount%100),
		"trade_status": status,
		"gmt_payment":  time.Now().Format("2006-01-02 15:04:05"),
		"notify_time":  time.Now().Format("2006-01-02 15:04:05"),
	}
	h := sha256.Sum256([]byte(alipayMessage(params)))
	raw, _ := rsa.SignPKCS1v15(rand.Reader, s.platformPriv, crypto.SHA256, h[:])
	params["sign"] = base64.StdEncoding.EncodeToString(raw)
	params["sign_type"] = "RSA2"
	if q.Get("badSign") == "1" {
		params["sign"] = base64.StdEncoding.EncodeToString([]byte("bad"))
	}
	if status == "TRADE_SUCCESS" && q.Get("badSign") != "1" {
		s.paid[q.Get("outTradeNo")] = amount
	}

	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}
	resp, err := http.Post(q.Get("target"),
		"application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	writeJSON(w, map[string]any{"status": resp.StatusCode, "body": string(body)})
}

// wechatRefund 假的微信退款。
//
// ⚠️ 它也**验我们的签名** —— 退款与下单走同一套 Authorization，
// 少验一处的话"退款路径的签名写错了"要到真实联调才发现。
func (s *payState) wechatRefund(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err := s.verifyWechatAuth(r, string(raw)); err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]string{"code": "SIGN_ERROR", "message": err.Error()})
		return
	}
	var req struct {
		OutTradeNo  string `json:"out_trade_no"`
		OutRefundNo string `json:"out_refund_no"`
		Amount      struct {
			Refund int64 `json:"refund"`
			Total  int64 `json:"total"`
		} `json:"amount"`
	}
	_ = json.Unmarshal(raw, &req)
	// 让脚本能指名让某一笔失败（验"渠道拒绝"那条路）。
	if s.refundFail[req.OutTradeNo] {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{
			"code": "NOT_ENOUGH", "message": "商户余额不足（测试注入）",
		})
		return
	}
	paidCents, ok := s.paid[req.OutTradeNo]
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{
			"code": "ORDERNOTEXIST", "message": "找不到这笔交易",
		})
		return
	}
	// ⚠️ **退的不能比收的多** —— 真实网关会校验，假上游也要校验，
	// 否则"算错退款额"这类 bug 只能等到真实联调才暴露。
	if req.Amount.Refund > paidCents || req.Amount.Total != paidCents {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{
			"code": "PARAM_ERROR",
			"message": fmt.Sprintf("金额不对：实收 %d，请求退 %d / 原单 %d",
				paidCents, req.Amount.Refund, req.Amount.Total),
		})
		return
	}
	// ⚠️ 幂等：同一个 `out_refund_no` 再来一次回同一个结果，**不退第二笔**。
	s.refunded[req.OutRefundNo] = req.Amount.Refund
	writeJSON(w, map[string]any{
		"refund_id":     "fake-refund-" + req.OutRefundNo,
		"out_refund_no": req.OutRefundNo, "status": "SUCCESS",
	})
}

// payFailRefund 让脚本指名某一单的退款失败。
func (s *payState) payFailRefund(w http.ResponseWriter, r *http.Request) {
	no := r.URL.Query().Get("outTradeNo")
	s.refundFail[no] = r.URL.Query().Get("off") != "1"
	writeJSON(w, map[string]any{"outTradeNo": no, "fail": s.refundFail[no]})
}

// payRefundCount 供脚本核对"到底退了几笔"。
func (s *payState) payRefundCount(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"count": len(s.refunded)})
}
