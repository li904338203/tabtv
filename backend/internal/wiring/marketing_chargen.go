package wiring

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	catalogdomain "github.com/li904338203/tabtv/backend/internal/catalog/domain"
	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"
	marketingapp "github.com/li904338203/tabtv/backend/internal/marketing/app"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 生成角色的端口适配（MKT-T-35；dev/05 MKD-28、§4.3）。
//
// 报价与受理**同一份 AcceptInput**（characterAcceptInput）：dryRun 走 generation.Service.Preview
// （输入解析 → 附加段 → 目录报价，不写库），真生成走 Accept。两者都经同一个 Quoter（catalog.QuoteWithFacts），
// 与 `POST /generations/estimate` 用的是同一个报价函数。

// MarketingCharacterGenerator 实现 marketing/app.CharacterImageGenerator。
type MarketingCharacterGenerator struct{ Gen *genapp.Service }

var _ marketingapp.CharacterImageGenerator = MarketingCharacterGenerator{}

// characterAcceptInput 组装受理入参（dev/05 §4.3）。
//
//   - 工作区 / 付费主体 / 操作者 / 会员等级原样取自 actor —— 那是 transport 按 `POST /generations`
//     同一套解析得到的（工作区闸上下文 + 唯一的 memberLevelOf），这里**不另解析**；
//   - ProjectID = 0、NodeID = ""、Origin = 生成角色：不属于任何项目（输入按工作区解析）、
//     成功后不登记待审作品（MKR-17）；
//   - Text = 形象描述（受理写进 params.prompt，CD-81）；张数同时进顶层 Count（计价）与 params.count
//     （上游按它出几张，同图片节点「数量」参数的处理）；
//   - 输入：参考图按顺序 order 1..N，画风作为素材输入排最后（只贡献提示词）。
func characterAcceptInput(ask marketingapp.CharacterGenAsk) genapp.AcceptInput {
	type declared struct {
		Kind         string `json:"kind"`
		Order        int    `json:"order"`
		ReferenceID  string `json:"referenceId,omitempty"`
		MaterialType string `json:"materialType,omitempty"`
		MaterialCode string `json:"materialCode,omitempty"`
	}
	inputs := make([]declared, 0, len(ask.ReferenceMediaIDs)+1)
	for i, id := range ask.ReferenceMediaIDs {
		inputs = append(inputs, declared{Kind: "reference", Order: i + 1,
			ReferenceID: strconv.FormatInt(id, 10)})
	}
	if ask.StyleCode != "" {
		inputs = append(inputs, declared{Kind: "material", Order: len(inputs) + 1,
			MaterialType: "style", MaterialCode: ask.StyleCode})
	}
	rawInputs, _ := json.Marshal(inputs)
	params, _ := json.Marshal(map[string]string{"count": strconv.Itoa(ask.Count)})
	return genapp.AcceptInput{
		WorkspaceID: ask.Actor.WorkspaceID, PayerAccountID: ask.Actor.PayerAccountID,
		UserID: ask.Actor.UserID, MemberLevelID: ask.Actor.MemberLevelID,
		ProjectID: 0, NodeID: "", Origin: gendomain.OriginMarketingCharacter,
		OperationCode: ask.OperationCode, ModelCode: ask.ModelCode,
		Count: ask.Count, Text: ask.Description, Params: params, DeclaredInputs: rawInputs,
		IdempotencyKey: ask.IdempotencyKey,
	}
}

// Quote 实现 CharacterImageGenerator：受理预演，报价阶段的拒绝翻成 blocked + 原因。
func (g MarketingCharacterGenerator) Quote(ctx context.Context, ask marketingapp.CharacterGenAsk) (
	marketingapp.CharacterGenQuote, error) {
	res, err := g.Gen.Preview(ctx, characterAcceptInput(ask))
	if err != nil {
		return marketingapp.CharacterGenQuote{}, err
	}
	if res.QuoteErr != nil {
		var be *httpx.BizError
		if !errors.As(res.QuoteErr, &be) || be.Code == errcode.Internal {
			return marketingapp.CharacterGenQuote{}, res.QuoteErr
		}
		return marketingapp.CharacterGenQuote{Blocked: true, BlockedReason: be.Message,
			UserFixable: quoteBlockUserFixable(be)}, nil
	}
	return marketingapp.CharacterGenQuote{Credits: res.Quote.Credits, Unit: res.Quote.PriceUnit}, nil
}

// quoteBlockUserFixable 按业务码分类（不匹配文案）：输入不符合能力矩阵（参考图过多）、输入超长、
// 会员等级不够是用户自己能解决的；模型下线、没绑操作、缺价格要运营处理。
func quoteBlockUserFixable(be *httpx.BizError) bool {
	switch be.Code {
	case errcode.MemberLevelRequired, errcode.InvalidArgument:
		return true
	case errcode.CapabilityUnsupported:
		reason, _ := be.Details[catalogdomain.QuoteBlockReasonKey].(string)
		return reason == catalogdomain.QuoteBlockInputMismatch
	}
	return false
}

// Accept 实现 CharacterImageGenerator：走统一受理（扣费、写任务、outbox 一个事务）。
func (g MarketingCharacterGenerator) Accept(ctx context.Context, ask marketingapp.CharacterGenAsk) (
	marketingapp.CharacterGenAccepted, error) {
	res, err := g.Gen.Accept(ctx, characterAcceptInput(ask))
	if err != nil {
		return marketingapp.CharacterGenAccepted{}, err
	}
	return marketingapp.CharacterGenAccepted{GenerationID: res.Generation.ID,
		Credits: res.Generation.CreditsCost, Replayed: res.Replayed}, nil
}

// MarketingStyles 实现 marketing/app.StyleLookup：画风码存在且启用。
//
// 与受理的素材校验同一个查询（MaterialPromptSuffixChecked 回「查不到的码」），不另写一份判断。
type MarketingStyles struct{ Catalog *catalogapp.Service }

// StyleExists 实现 StyleLookup。
func (s MarketingStyles) StyleExists(ctx context.Context, code string) (bool, error) {
	_, missing, err := s.Catalog.MaterialPromptSuffixChecked(ctx, []string{code}, nil)
	if err != nil {
		return false, err
	}
	return len(missing) == 0, nil
}

// NewMarketingCharacterGen 装配生成角色服务（MKT-T-35）。
func NewMarketingCharacterGen(gen *genapp.Service, catalog *catalogapp.Service, media MarketingWorkspaceMedia) *marketingapp.CharacterGen {
	return marketingapp.NewCharacterGen(MarketingCharacterGenerator{Gen: gen}, MarketingStyles{Catalog: catalog}, media)
}
