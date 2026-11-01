package app

import (
	"strings"
	"testing"
)

func TestValidateChannelFields(t *testing.T) {
	ok := SaveChannel{Code: "wechat", Name: "微信支付", MerchantID: "1900000109",
		BaseURL: "https://api.mch.weixin.qq.com", NotifyURL: "https://example.com/api/v1/payments/callback/wechat"}
	if err := validateChannelFields(ok); err != nil {
		t.Fatalf("合法的渠道应通过：%v", err)
	}
	// 本地联调的假网关在 127.0.0.1 上，http 也要放行
	local := ok
	local.BaseURL = "http://127.0.0.1:8195/payment/wechat"
	if err := validateChannelFields(local); err != nil {
		t.Fatalf("本地 http 网关应通过：%v", err)
	}
	bad := map[string]func(*SaveChannel){
		"名称为空":   func(c *SaveChannel) { c.Name = " " },
		"名称超长":   func(c *SaveChannel) { c.Name = strings.Repeat("字", 65) },
		"商户号超长":  func(c *SaveChannel) { c.MerchantID = strings.Repeat("1", 129) },
		"网关不是地址": func(c *SaveChannel) { c.BaseURL = "api.mch.weixin.qq.com" },
		"回调是伪协议": func(c *SaveChannel) { c.NotifyURL = "javascript:alert(1)" },
		"回调超长":   func(c *SaveChannel) { c.NotifyURL = "https://x.com/" + strings.Repeat("a", 512) },
	}
	for name, mutate := range bad {
		c := ok
		mutate(&c)
		if err := validateChannelFields(c); err == nil {
			t.Errorf("%s 应被拒", name)
		}
	}
}
