package wiring

import (
	"errors"
	"testing"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	catalogdomain "github.com/li904338203/tabtv/backend/internal/catalog/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 报价阻塞的「是不是模型写错的参数」分类（submit_plan 据此退回给模型而不发卡）。
// ⚠️ 只看业务码 + Details 里的细分原因，不看文案：下面同一个码、同一句文案，细分原因不同结论就不同。
func TestQuoteBlock_参数错误分类(t *testing.T) {
	withReason := func(code errcode.Code, reason string) error {
		e := httpx.Errf(code, "某句文案")
		if reason != "" {
			e = e.WithDetails(map[string]any{catalogdomain.QuoteBlockReasonKey: reason})
		}
		return e
	}
	cases := []struct {
		name    string
		err     error
		fault   string
		fixable bool
	}{
		{"输入不符", withReason(errcode.CapabilityUnsupported, catalogdomain.QuoteBlockInputMismatch), agentapp.PlanFaultInputs, false},
		{"模型没有这个操作", withReason(errcode.CapabilityUnsupported, catalogdomain.QuoteBlockNotBound), agentapp.PlanFaultOperation, false},
		{"时长非法", withReason(errcode.InvalidArgument, ""), agentapp.PlanFaultQuantity, true},
		{"操作被停用（运营）", withReason(errcode.CapabilityUnsupported, catalogdomain.QuoteBlockOperationDisabled), "", false},
		{"没声明能力矩阵（运营）", withReason(errcode.CapabilityUnsupported, catalogdomain.QuoteBlockCapabilityUndeclared), "", false},
		{"能力不支持但没有细分原因", withReason(errcode.CapabilityUnsupported, ""), "", false},
		{"模型下线", withReason(errcode.ModelUnavailable, ""), "", false},
		{"缺价", withReason(errcode.PricingMissing, ""), "", false},
		{"会员等级不够", withReason(errcode.MemberLevelRequired, ""), "", true},
	}
	for _, c := range cases {
		b, internal := quoteBlock(c.err)
		if internal != nil {
			t.Fatalf("%s：不该是内部错误: %v", c.name, internal)
		}
		if b.ArgFault != c.fault || b.UserFixable != c.fixable || b.Reason != "某句文案" {
			t.Fatalf("%s：分类不对: %+v", c.name, b)
		}
	}
	if _, internal := quoteBlock(errors.New("db down")); internal == nil {
		t.Fatal("非业务错误应原样作为内部错误返回")
	}
	if _, internal := quoteBlock(httpx.Wrap(errcode.Internal, errors.New("x"))); internal == nil {
		t.Fatal("Internal 业务码应作为内部错误返回")
	}
}
