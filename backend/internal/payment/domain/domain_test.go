package domain

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"testing"
	"time"
)

// ⚠️ 这里**没有任何真实凭据**：密钥对由测试自己现生成，
// APIv3 密钥是一串写死的测试字符。商户凭据是 PARAM-03，永远不进仓库。

func newKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func pkcs1PEM(t *testing.T, k *rsa.PrivateKey) string {
	t.Helper()
	return string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k),
	}))
}

func pkcs8PEM(t *testing.T, k *rsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func pubPEM(t *testing.T, k *rsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// ⚠️ 两种私钥格式都要支持：微信给 PKCS#8（`BEGIN PRIVATE KEY`），
// 支付宝控制台常导出 PKCS#1（`BEGIN RSA PRIVATE KEY`）。只认一种的话，
// 运维把另一种贴进来只会得到一句"解析失败"，而 PEM 头部那行没人会逐字比对。
func TestParsePrivateKey_两种PEM格式都认(t *testing.T) {
	k := newKey(t)
	for name, text := range map[string]string{
		"PKCS#1": pkcs1PEM(t, k),
		"PKCS#8": pkcs8PEM(t, k),
	} {
		got, err := ParsePrivateKey(text)
		if err != nil {
			t.Fatalf("%s 应能解析：%v", name, err)
		}
		if got.N.Cmp(k.N) != 0 {
			t.Fatalf("%s 解出来的不是同一把钥匙", name)
		}
	}
	if _, err := ParsePrivateKey("not a pem"); err == nil {
		t.Fatal("非 PEM 应报错")
	}
	// 错误里要提示检查 BEGIN/END —— 运维最常见的失误是只贴了中间那段。
	_, err := ParsePrivateKey("MIIEow...")
	if err == nil || !contains(err.Error(), "BEGIN") {
		t.Fatalf("错误应提示检查 PEM 头尾：%v", err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func TestSignVerifyRSASHA256(t *testing.T) {
	k := newKey(t)
	msg := "GET\n/v3/pay/transactions/id/x\n1700000000\nabc\n\n"
	sig, err := SignRSASHA256(k, msg)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ParsePublicKey(pubPEM(t, k))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyRSASHA256(pub, msg, sig); err != nil {
		t.Fatalf("自签自验应通过：%v", err)
	}
	// ⚠️ 改一个字节就必须失败 —— 这正是验签存在的理由。
	if err := VerifyRSASHA256(pub, msg+"x", sig); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatal("报文被改过应验签失败")
	}
	// 别人的钥匙签的也必须失败。
	other, _ := SignRSASHA256(newKey(t), msg)
	if err := VerifyRSASHA256(pub, msg, other); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatal("换一把钥匙签的应验签失败")
	}
	// ⚠️ 坏 base64 也只回 ErrSignatureInvalid，**不带底层原因**：
	// 区分"base64 坏了"和"签名对不上"会把验签细节泄漏给外网调用方。
	if err := VerifyRSASHA256(pub, msg, "!!!not base64!!!"); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatal("坏 base64 应归到验签失败")
	}
}

// ⚠️ 待签名串**四行、每行以 \n 结尾（最后一行也要）**。
// 漏掉末尾那个 \n 签名一定对不上，而错误信息只会说"签名错误"——
// 这是接微信最常见的一个坑。
func TestWechatMessage_末尾换行不能漏(t *testing.T) {
	got := WechatMessage("1700000000", "nonce1", `{"id":"x"}`)
	want := "1700000000\nnonce1\n{\"id\":\"x\"}\n"
	if got != want {
		t.Fatalf("回调待签串不对：%q", got)
	}
	req := WechatRequestMessage("POST", "/v3/pay/transactions/native",
		"1700000000", "nonce1", `{"a":1}`)
	if req != "POST\n/v3/pay/transactions/native\n1700000000\nnonce1\n{\"a\":1}\n" {
		t.Fatalf("请求待签串不对：%q", req)
	}
	// 空 body 也要有末尾换行（GET 请求的 body 是空串）。
	if WechatRequestMessage("GET", "/x", "1", "n", "") != "GET\n/x\n1\nn\n\n" {
		t.Fatal("空 body 时仍要两个换行")
	}
}

// ⚠️ 用**绝对值**判窗口：时钟快的那一侧会送来"未来"的时间戳，
// 只判"太旧"的话那一侧永远过不了。
func TestCheckTimestamp_两个方向都判(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ok := func(delta time.Duration) error {
		return CheckTimestamp(itoa64(now.Add(delta).Unix()), now)
	}
	if err := ok(0); err != nil {
		t.Fatalf("当前时间应通过：%v", err)
	}
	if err := ok(-4 * time.Minute); err != nil {
		t.Fatalf("4 分钟前应通过：%v", err)
	}
	if err := ok(4 * time.Minute); err != nil {
		t.Fatalf("4 分钟后（对方时钟快）应通过：%v", err)
	}
	if err := ok(-10 * time.Minute); !errors.Is(err, ErrReplayWindow) {
		t.Fatal("10 分钟前应被拒")
	}
	if err := ok(10 * time.Minute); !errors.Is(err, ErrReplayWindow) {
		t.Fatal("10 分钟后应被拒（同样是重放窗口）")
	}
	if err := CheckTimestamp("不是数字", now); !errors.Is(err, ErrMalformed) {
		t.Fatal("非数字时间戳应报格式错")
	}
}

func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

// ---------------------------------------------------------------- 微信回调

const testAPIv3Key = "0123456789abcdef0123456789abcdef" // 32 字符，测试专用

func sealWechat(t *testing.T, plaintext, aad string) (ciphertext, nonce string) {
	t.Helper()
	block, err := aes.NewCipher([]byte(testAPIv3Key))
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	n := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(n); err != nil {
		t.Fatal(err)
	}
	// nonce 在回调里是**明文字符串**传的，所以要是可打印字符。
	for i := range n {
		n[i] = byte('a' + n[i]%26)
	}
	out := gcm.Seal(nil, n, []byte(plaintext), []byte(aad))
	return base64.StdEncoding.EncodeToString(out), string(n)
}

func wechatBody(t *testing.T, eventType, tradeState, outTradeNo string,
	total int64) []byte {
	t.Helper()
	tx := map[string]any{
		"out_trade_no": outTradeNo, "transaction_id": "4200001",
		"trade_state": tradeState, "success_time": "2026-09-22T10:00:00+08:00",
		"amount": map[string]any{"total": total, "payer_total": total},
	}
	plain, _ := json.Marshal(tx)
	ct, nonce := sealWechat(t, string(plain), "transaction")
	env := map[string]any{
		"id": "evt-1", "create_time": "2026-09-22T10:00:01+08:00",
		"event_type": eventType, "resource_type": "encrypt-resource",
		"resource": map[string]any{
			"algorithm": "AEAD_AES_256_GCM", "ciphertext": ct,
			"associated_data": "transaction", "nonce": nonce,
		},
	}
	body, _ := json.Marshal(env)
	return body
}

func TestParseWechatCallback_解密并取出业务事实(t *testing.T) {
	body := wechatBody(t, "TRANSACTION.SUCCESS", "SUCCESS", "ORD-1", 2900)
	ev, err := ParseWechatCallback(testAPIv3Key, body)
	if err != nil {
		t.Fatal(err)
	}
	if ev.EventID != "evt-1" || ev.OutTradeNo != "ORD-1" {
		t.Fatalf("业务事实不对：%+v", ev)
	}
	// ⚠️ 微信的金额单位**就是分**，不用换算 —— 当成元再乘 100 的话放大一百倍。
	if ev.AmountCents != 2900 {
		t.Fatalf("金额应为 2900 分，得到 %d", ev.AmountCents)
	}
	if !ev.Succeeded {
		t.Fatal("SUCCESS 应判为成功")
	}
}

// ⚠️ 回调**不只有成功**（关闭、撤销、退款都会回调）。一律当成功处理的话，
// 一笔"用户取消支付"的回调会发出权益。
func TestParseWechatCallback_非成功状态不算成功(t *testing.T) {
	for _, c := range []struct{ event, state string }{
		{"TRANSACTION.SUCCESS", "CLOSED"},
		{"TRANSACTION.SUCCESS", "PAYERROR"},
		// 外层事件类型与内层状态不一致时按不成功处理。
		{"TRANSACTION.CLOSED", "SUCCESS"},
	} {
		ev, err := ParseWechatCallback(testAPIv3Key,
			wechatBody(t, c.event, c.state, "ORD-1", 2900))
		if err != nil {
			t.Fatal(err)
		}
		if ev.Succeeded {
			t.Fatalf("%s/%s 不该判为成功", c.event, c.state)
		}
	}
}

// ⚠️ 密钥不对 / 报文被改过 —— 解密失败等价于验签失败。
func TestParseWechatCallback_密钥不对时解不开(t *testing.T) {
	body := wechatBody(t, "TRANSACTION.SUCCESS", "SUCCESS", "ORD-1", 2900)
	_, err := ParseWechatCallback("ffffffffffffffffffffffffffffffff", body)
	if !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("密钥不对应归到验签失败，得到 %v", err)
	}
	// APIv3 密钥长度不对要**明说**：它是 32 字符的 ASCII，不是 base64、不是 hex。
	_, err = ParseWechatCallback("太短了", body)
	if err == nil || !contains(err.Error(), "32") {
		t.Fatalf("应提示密钥长度：%v", err)
	}
}

// 没有事件 ID 就没法做幂等 —— 宁可拒收也不能放进去。
func TestParseWechatCallback_缺关键字段一律拒(t *testing.T) {
	if _, err := ParseWechatCallback(testAPIv3Key, []byte(`{"event_type":"x"}`)); err == nil {
		t.Fatal("缺 id 应被拒")
	}
	if _, err := ParseWechatCallback(testAPIv3Key, []byte("不是 json")); !errors.Is(err, ErrMalformed) {
		t.Fatal("坏 JSON 应报格式错")
	}
}

// ⚠️ `associated_data` 必须作为 AAD 参与解密：忽略的话 GCM 校验会失败。
func TestDecryptWechatResource_AAD不对就解不开(t *testing.T) {
	ct, nonce := sealWechat(t, `{"a":1}`, "transaction")
	res := struct {
		Algorithm      string `json:"algorithm"`
		Ciphertext     string `json:"ciphertext"`
		AssociatedData string `json:"associated_data"`
		Nonce          string `json:"nonce"`
	}{"AEAD_AES_256_GCM", ct, "refund", nonce} // AAD 故意写错
	if _, err := DecryptWechatResource(testAPIv3Key, res); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatal("AAD 不对应解密失败")
	}
	res.AssociatedData = "transaction"
	if _, err := DecryptWechatResource(testAPIv3Key, res); err != nil {
		t.Fatalf("AAD 正确时应解得开：%v", err)
	}
}

// ---------------------------------------------------------------- 支付宝

// ⚠️ 排序 + 跳过空值 + 跳过 sign/sign_type。任何一条漏了，
// 支付宝只回一句"验签失败"，而那句话指向不了具体原因。
func TestAlipayMessage_排序与跳过规则(t *testing.T) {
	got := AlipayMessage(map[string]string{
		"out_trade_no": "ORD-1",
		"trade_status": "TRADE_SUCCESS",
		"app_id":       "2021",
		"empty":        "",
		"sign":         "should-not-appear",
		"sign_type":    "RSA2",
	})
	want := "app_id=2021&out_trade_no=ORD-1&trade_status=TRADE_SUCCESS"
	if got != want {
		t.Fatalf("待签串不对：\n得到 %q\n期望 %q", got, want)
	}
}

func TestAlipayMessage_自签自验(t *testing.T) {
	k := newKey(t)
	params := map[string]string{
		"out_trade_no": "ORD-1", "total_amount": "29.00",
		"trade_status": "TRADE_SUCCESS", "notify_id": "n-1",
	}
	sig, err := SignRSASHA256(k, AlipayMessage(params))
	if err != nil {
		t.Fatal(err)
	}
	params["sign"] = sig
	params["sign_type"] = "RSA2"
	pub, _ := ParsePublicKey(pubPEM(t, k))
	// 验签时要把 sign/sign_type 排除掉再拼 —— AlipayMessage 已经做了。
	if err := VerifyRSASHA256(pub, AlipayMessage(params), params["sign"]); err != nil {
		t.Fatalf("自签自验应通过：%v", err)
	}
	// 改金额必须验不过 —— 这是"改金额白拿权益"那条攻击的第一道防线。
	params["total_amount"] = "0.01"
	if err := VerifyRSASHA256(pub, AlipayMessage(params), params["sign"]); err == nil {
		t.Fatal("改了金额应验签失败")
	}
}

// ⚠️ **不能 ParseFloat 再乘 100**：`0.29` 乘出来是 28.999999999999996，
// 截断后成了 28 分 —— 每一笔都少算一分，而对账时谁也说不清差额从哪来。
func TestAlipayAmountCents_按字符串切不走浮点(t *testing.T) {
	cases := map[string]int64{
		"29.00": 2900, "0.29": 29, "0.01": 1, "1": 100,
		"1.5": 150, "1.999": 199, "1234.56": 123456, "0": 0,
	}
	for in, want := range cases {
		got, err := AlipayAmountCents(in)
		if err != nil {
			t.Fatalf("%q 应能解析：%v", in, err)
		}
		if got != want {
			t.Fatalf("%q 应为 %d 分，得到 %d", in, want, got)
		}
	}
	if _, err := AlipayAmountCents(""); !errors.Is(err, ErrMalformed) {
		t.Fatal("空串应报格式错")
	}
	if _, err := AlipayAmountCents("abc"); !errors.Is(err, ErrMalformed) {
		t.Fatal("非数字应报格式错")
	}
}

func TestCentsToYuan(t *testing.T) {
	cases := map[int64]string{2900: "29.00", 1: "0.01", 0: "0.00", 123456: "1234.56"}
	for in, want := range cases {
		if got := CentsToYuan(in); got != want {
			t.Fatalf("%d 应为 %q，得到 %q", in, want, got)
		}
	}
	// 往返一致 —— 对账时两个方向都要用。
	for _, v := range []int64{0, 1, 29, 2900, 999999} {
		back, err := AlipayAmountCents(CentsToYuan(v))
		if err != nil || back != v {
			t.Fatalf("往返不一致：%d → %q → %d (%v)", v, CentsToYuan(v), back, err)
		}
	}
}

func TestParseAlipayCallback(t *testing.T) {
	ev, err := ParseAlipayCallback(map[string]string{
		"notify_id": "n-1", "out_trade_no": "ORD-1", "trade_no": "2026092200",
		"total_amount": "29.00", "trade_status": "TRADE_SUCCESS",
		"gmt_payment": "2026-09-22 10:00:00",
	})
	if err != nil {
		t.Fatal(err)
	}
	if ev.EventID != "n-1" || ev.AmountCents != 2900 || !ev.Succeeded {
		t.Fatalf("业务事实不对：%+v", ev)
	}
	// ⚠️ 幂等键用 `notify_id` 不是 `trade_no`：支付宝对同一笔交易会推多次
	// （WAIT_BUYER_PAY → TRADE_SUCCESS），用 trade_no 的话后一次会被当成重复丢掉。
	ev2, _ := ParseAlipayCallback(map[string]string{
		"notify_id": "n-2", "out_trade_no": "ORD-1", "trade_no": "2026092200",
		"total_amount": "29.00", "trade_status": "TRADE_SUCCESS",
	})
	if ev2.EventID == ev.EventID {
		t.Fatal("同一笔交易的两次通知应有不同的幂等键")
	}
	// 等待付款不算成功。
	wait, _ := ParseAlipayCallback(map[string]string{
		"notify_id": "n-3", "out_trade_no": "ORD-1", "total_amount": "29.00",
		"trade_status": "WAIT_BUYER_PAY",
	})
	if wait.Succeeded {
		t.Fatal("WAIT_BUYER_PAY 不该判为成功")
	}
	if _, err := ParseAlipayCallback(map[string]string{"notify_id": "n"}); err == nil {
		t.Fatal("缺 out_trade_no 应被拒")
	}
}

// ---------------------------------------------------------------- 金额核对

// ⚠️ **只看"状态=成功"是不够的**：渠道只负责告诉你"用户付了多少"，
// "付够没付够"是我们的事。不核对的话，有人付 1 分钱就能拿走一年会员，
// 而那个回调完全合法、验签也过。
func TestVerifyAmount_少付一分都要拒(t *testing.T) {
	ev := CallbackEvent{AmountCents: 2900}
	if err := VerifyAmount(ev, 2900); err != nil {
		t.Fatalf("足额应通过：%v", err)
	}
	if err := VerifyAmount(CallbackEvent{AmountCents: 2899}, 2900); !errors.Is(err, ErrAmountMismatch) {
		t.Fatal("少一分应被拒")
	}
	if err := VerifyAmount(CallbackEvent{AmountCents: 1}, 2900); !errors.Is(err, ErrAmountMismatch) {
		t.Fatal("付 1 分不该拿到权益")
	}
	// 错误里要带两边的数字，否则对账时看不出差多少。
	err := VerifyAmount(CallbackEvent{AmountCents: 1}, 2900)
	if !contains(err.Error(), "2900") || !contains(err.Error(), "1 分") {
		t.Fatalf("错误应带上两边金额：%v", err)
	}
	// 多付**不算错**（渠道优惠、重复支付都可能），由调用方记告警。
	if err := VerifyAmount(CallbackEvent{AmountCents: 3000}, 2900); err != nil {
		t.Fatalf("多付不该被拒：%v", err)
	}
}
