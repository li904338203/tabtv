package app

import (
	"context"
	"errors"
	"testing"
)

type fixedPresigner struct {
	url string
	err error
}

func (p fixedPresigner) PresignGet(context.Context, string) (string, error) { return p.url, p.err }

// docs/agent/smartcut SCT-38：只收地址的上游收不收得到参考媒体，与 Worker 发参考媒体时（mediaValue）同一条规则。
func TestMediaDelivery_只收地址的协议才查且与发送规则一致(t *testing.T) {
	public := fixedPresigner{url: "https://oss.example.com/bucket/probe?X-Amz-Signature=1"}
	private := fixedPresigner{url: "http://127.0.0.1:9100/bucket/probe?X-Amz-Signature=1"}
	broken := fixedPresigner{err: errors.New("签名失败")}
	cases := []struct {
		name     string
		protocol string
		d        MediaDelivery
		blocked  bool
	}{
		{"收内联的协议不查", "openai_images", MediaDelivery{Main: private}, false},
		{"没注册的协议不查（不知道就别拦）", "not_a_protocol", MediaDelivery{}, false},
		{"主存储签得出公网地址", "volc_mediakit_erase", MediaDelivery{Main: public}, false},
		{"主存储是内网、配了公网中转", "volc_mediakit_erase", MediaDelivery{Main: private, Relay: public}, false},
		{"主存储是内网、没有中转", "volc_mediakit_erase", MediaDelivery{Main: private}, true},
		{"中转签出的也是内网地址", "uniart_video", MediaDelivery{Main: private, Relay: private}, true},
		{"签名失败且没有中转", "volc_asr_standard", MediaDelivery{Main: broken}, true},
		{"没有对象存储", "volc_mediakit_erase", MediaDelivery{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			why := c.d.Undeliverable(context.Background(), c.protocol)
			if (why != "") != c.blocked {
				t.Fatalf("blocked=%v，得到 %q", c.blocked, why)
			}
		})
	}
}
