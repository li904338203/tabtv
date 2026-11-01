package app

import (
	"context"
	"errors"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// docs/agent/smartcut SCT-38：只收地址的上游收不到参考媒体时，受理在扣费之前就拒。
// 预览与受理共用 prepare；「受理不写库、不扣费」由真库的 smartcut TestDesubtitle_无可达地址时受理即失败不扣费 验。

type fixedQuoter struct{ ans QuoteAnswer }

func (q fixedQuoter) QuoteForAccept(context.Context, QuoteAsk) (QuoteAnswer, error) {
	return q.ans, nil
}

type stubDelivery struct {
	why   string
	asked []string
}

func (d *stubDelivery) Undeliverable(_ context.Context, apiProtocol string) string {
	d.asked = append(d.asked, apiProtocol)
	return d.why
}

func deliveryService(why string) (*Service, *stubDelivery) {
	s := &Service{}
	s.SetQuoter(fixedQuoter{QuoteAnswer{Credits: 9, APIProtocol: "volc_mediakit_erase", ResultType: "video"}})
	d := &stubDelivery{why: why}
	s.SetMediaURLDelivery(d)
	return s, d
}

func TestPrepare_只收地址的上游收不到素材时报价阶段就拒(t *testing.T) {
	s, d := deliveryService("当前存储签出的地址它访问不到")
	res, err := s.Preview(context.Background(), AcceptInput{ModelCode: "erase-m", OperationCode: "video_desubtitle",
		RoleCounts: map[string]int{"video": 1}})
	if err != nil {
		t.Fatal(err)
	}
	var be *httpx.BizError
	if !errors.As(res.QuoteErr, &be) || be.Code != errcode.ModelUnavailable || be.Error() != "当前存储签出的地址它访问不到" {
		t.Fatalf("应在报价阶段报 42204 并带原因: %v", res.QuoteErr)
	}
	if len(d.asked) != 1 || d.asked[0] != "volc_mediakit_erase" {
		t.Fatalf("按报价给出的协议问: %v", d.asked)
	}
}

func TestPrepare_没有媒体输入或收得到时不拦(t *testing.T) {
	s, d := deliveryService("访问不到")
	res, err := s.Preview(context.Background(), AcceptInput{ModelCode: "m", OperationCode: "text2video",
		RoleCounts: map[string]int{"text": 1}})
	if err != nil || res.QuoteErr != nil || len(d.asked) != 0 {
		t.Fatalf("纯文本的生成不发参考媒体，不问也不拦: %v %v %v", err, res.QuoteErr, d.asked)
	}
	s, _ = deliveryService("")
	if res, _ := s.Preview(context.Background(), AcceptInput{ModelCode: "m", OperationCode: "video_desubtitle",
		RoleCounts: map[string]int{"video": 1}}); res.QuoteErr != nil || res.Quote.Credits != 9 {
		t.Fatalf("收得到时照常报价: %+v", res)
	}
}
