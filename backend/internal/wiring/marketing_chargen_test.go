package wiring

import (
	"encoding/json"
	"testing"

	catalogdomain "github.com/li904338203/tabtv/backend/internal/catalog/domain"
	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"
	marketingapp "github.com/li904338203/tabtv/backend/internal/marketing/app"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 付费主体、会员等级、操作者原样来自 actor —— 那是 transport 按 `POST /generations` 同一套解析得到的
// （工作区闸上下文的 PayerAccountID + cmd/api 唯一的 memberLevelOf + 登录态），适配器不另解析、不改写。
// 同时钉住受理组装（dev/05 §4.3）：无项目、来源、幂等键前缀、提示词、张数、输入顺序。
func TestCharacterImageGenerator_付费主体解析与generations一致(t *testing.T) {
	ask := marketingapp.CharacterGenAsk{
		Actor:         marketingapp.CharacterGenActor{WorkspaceID: 11, PayerAccountID: 22, UserID: 33, MemberLevelID: 44},
		ModelCode:     "img-m",
		OperationCode: "img2img", Description: "圆脸柚子", StyleCode: "ink",
		ReferenceMediaIDs: []int64{501, 502}, Count: 3, IdempotencyKey: "mkchar:k1",
	}
	in := characterAcceptInput(ask)
	if in.WorkspaceID != 11 || in.PayerAccountID != 22 || in.UserID != 33 || in.MemberLevelID != 44 {
		t.Fatalf("付费主体 / 操作者 / 会员等级应原样透传：%+v", in)
	}
	if in.ProjectID != 0 || in.NodeID != "" || in.Origin != gendomain.OriginMarketingCharacter {
		t.Fatalf("应无项目、无节点、带来源：%+v", in)
	}
	if in.OperationCode != "img2img" || in.ModelCode != "img-m" || in.Count != 3 || in.Text != "圆脸柚子" ||
		in.IdempotencyKey != "mkchar:k1" {
		t.Fatalf("受理字段：%+v", in)
	}
	var params map[string]string
	_ = json.Unmarshal(in.Params, &params)
	if params["count"] != "3" {
		t.Errorf("张数要进 params.count（上游按它出几张）：%v", params)
	}
	var inputs []map[string]any
	_ = json.Unmarshal(in.DeclaredInputs, &inputs)
	if len(inputs) != 3 || inputs[0]["referenceId"] != "501" || inputs[0]["order"] != float64(1) ||
		inputs[1]["referenceId"] != "502" || inputs[2]["kind"] != "material" ||
		inputs[2]["materialType"] != "style" || inputs[2]["materialCode"] != "ink" || inputs[2]["order"] != float64(3) {
		t.Errorf("输入应为参考图按序 + 画风在最后：%v", inputs)
	}
	// dryRun 与真生成用同一份入参：Quote 与 Accept 都调 characterAcceptInput（这里只校验它对同一 ask 稳定）。
	if again := characterAcceptInput(ask); string(again.DeclaredInputs) != string(in.DeclaredInputs) ||
		string(again.Params) != string(in.Params) {
		t.Error("同一请求的受理入参应稳定")
	}
	// 没有参考图与画风：输入是空数组（不是 null）。
	if in0 := characterAcceptInput(marketingapp.CharacterGenAsk{Count: 1}); string(in0.DeclaredInputs) != "[]" {
		t.Errorf("无输入应为 []：%s", in0.DeclaredInputs)
	}
}

func TestQuoteBlockUserFixable(t *testing.T) {
	cases := []struct {
		err  *httpx.BizError
		want bool
	}{
		{httpx.Errf(errcode.CapabilityUnsupported, "x").WithDetails(map[string]any{
			catalogdomain.QuoteBlockReasonKey: catalogdomain.QuoteBlockInputMismatch}), true},
		{httpx.Errf(errcode.CapabilityUnsupported, "x").WithDetails(map[string]any{
			catalogdomain.QuoteBlockReasonKey: catalogdomain.QuoteBlockNotBound}), false},
		{httpx.Errf(errcode.MemberLevelRequired, "x"), true},
		{httpx.Errf(errcode.InvalidArgument, "x"), true},
		{httpx.Errf(errcode.ModelUnavailable, "x"), false},
		{httpx.Errf(errcode.PricingMissing, "x"), false},
	}
	for _, c := range cases {
		if got := quoteBlockUserFixable(c.err); got != c.want {
			t.Errorf("%v → %v，期望 %v", c.err.Code, got, c.want)
		}
	}
}
