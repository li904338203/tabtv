package app

import (
	"crypto/rand"
	"crypto/rsa"
	"testing"
)

// ⚠️ 签的路径必须是**网关看到的那个路径**。真实微信的 baseUrl 是纯域名
// （前缀为空），所以两种写法结果一样；但接在带前缀的网关/沙箱后面时，
// 只签末段会一律 401，而错误信息只说"签名错误"——
// 这一条是 P11-02 联调时实测撞到的（假上游挂在 /payment/wechat 下）。
func TestSignPath_带上baseUrl的路径前缀(t *testing.T) {
	k, _ := rsa.GenerateKey(rand.Reader, 1024)
	cases := map[string]string{
		// 真实微信：纯域名，前缀为空 → 原样。
		"https://api.mch.weixin.qq.com":  "/v3/pay/transactions/native",
		"https://api.mch.weixin.qq.com/": "/v3/pay/transactions/native",
		// 带前缀的网关/沙箱 → 要把前缀拼上。
		"http://127.0.0.1:8195/payment/wechat":  "/payment/wechat/v3/pay/transactions/native",
		"http://127.0.0.1:8195/payment/wechat/": "/payment/wechat/v3/pay/transactions/native",
		// 配坏了的 baseUrl 不该让签名崩掉，回落到原样。
		"::not a url": "/v3/pay/transactions/native",
	}
	for base, want := range cases {
		c := NewWechatChannel(WechatConfig{BaseURL: base, PrivateKey: k}, nil)
		if got := c.signPath("/v3/pay/transactions/native"); got != want {
			t.Fatalf("baseUrl=%q 应签 %q，得到 %q", base, want, got)
		}
	}
}
