package app

import (
	"context"
	"errors"

	"github.com/li904338203/tabtv/backend/internal/catalog/domain"
	"github.com/li904338203/tabtv/backend/internal/provider/protocol"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// ---------------------------------------------------------------- 智能剪辑（docs/agent/smartcut）

// OpSmartcutPlan 是 AI 剪辑（规划、追加指令、翻译）的操作码。它不是画布节点的操作（repo.nonCanvasOperations）。
const OpSmartcutPlan = "smartcut_plan"

// smartcutPlanMaxImages 是 AI 剪辑一次最多带几张拼图（SC-PARAM-24，设计建议值）：还要与模型的 image 上限取小。
const smartcutPlanMaxImages = 4

// SmartcutPlanChoice 取 AI 剪辑此刻可用的第一个模型（SCD-24）：与 AI 搭景同一套规则（freeTextChoice）——
// 绑定了 smartcut_plan 的模型按排序逐个报价，取第一个报得出 0 积分的；报价 > 0 时显式拒绝（AI 剪辑按约定免费，SCK-2）。
// ⚠️ /plan 与 /smartcut/config 都调这一个函数（SC-F-88）：各写一份的话会出现「入口说能用、点下去 422」。
func (s *Service) SmartcutPlanChoice(ctx context.Context, memberLevelID int64, imageCount int) (
	SceneBuildChoice, *SceneBuildUnavailable, error) {
	return s.freeTextChoice(ctx, OpSmartcutPlan, "AI 剪辑", memberLevelID, imageCount, smartcutPlanMaxImages)
}

// OpSpeech2Text 是语音识别的操作码（SCT-33）。同 smartcut_plan，不是画布节点的操作。
const OpSpeech2Text = "speech2text"

// SpeechModel 一个可选的识别模型（智能剪辑 `/smartcut/config` 的 models.speech2text）。
type SpeechModel struct {
	Code, Name string
	// Granularity 时间戳粒度（char / word / segment / text）：口播精剪只认字级或词级。
	Granularity string
	UnitPrice   int
	Unit        string
}

// SpeechModels 列出此刻可用的识别模型：绑定了 speech2text、按 1 秒报得出价的（与受理同一个报价函数）。
// 报不出价的不列（会员等级不够、没配价都不该出现在下拉里，点下去也是 422）。
func (s *Service) SpeechModels(ctx context.Context, memberLevelID int64) ([]SpeechModel, error) {
	codes, err := s.repo.ListModelsForOperation(ctx, OpSpeech2Text)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	out := make([]SpeechModel, 0, len(codes))
	for _, code := range codes {
		q, err := s.QuoteWithFacts(ctx, speechQuote(code, memberLevelID, 1))
		if err != nil {
			continue
		}
		m, err := s.repo.GetModel(ctx, code)
		if err != nil {
			continue
		}
		out = append(out, SpeechModel{Code: code, Name: m.Name,
			Granularity: protocol.ASRGranularity(m.APIProtocol, m.UpstreamModelCode),
			UnitPrice:   q.PriceCredits, Unit: q.PriceUnit})
	}
	return out, nil
}

// QuoteSpeech 报一次识别的价：与生成受理同一个 QuoteWithFacts（输入是一段音频、按秒）。
// 不可用（没绑、没价、会员等级不够）时第二个返回值说明原因，不当成错误。
func (s *Service) QuoteSpeech(ctx context.Context, modelCode string, memberLevelID int64, durationSec int) (
	QuoteDetail, *SceneBuildUnavailable, error) {
	q, err := s.QuoteWithFacts(ctx, speechQuote(modelCode, memberLevelID, durationSec))
	if err != nil {
		var be *httpx.BizError
		if errors.As(err, &be) && be.Code != errcode.Internal {
			return QuoteDetail{}, quoteUnavailableFor("语音识别", err), nil
		}
		return QuoteDetail{}, nil, err
	}
	return q, nil, nil
}

func speechQuote(modelCode string, memberLevelID int64, durationSec int) QuoteRequest {
	return QuoteRequest{ModelCode: modelCode, OperationCode: OpSpeech2Text, Count: 1, DurationSec: durationSec,
		MemberLevelID: memberLevelID, RoleCounts: domain.RoleCount{domain.RoleAudio: 1}}
}

// ClipModel 剪辑台片段级工具的一个可选模型（去字幕 / 配乐 / 音效 / 补镜，docs/agent/smartcut SCT-38~40）。
type ClipModel struct {
	Code, Name string
	UnitPrice  int
	Unit       string
	// MinSec / MaxSec 是协议声明的单次时长范围（VideoLimits / DurationRange；0 = 没声明、不限）。
	MinSec, MaxSec int
}

// ClipModels 列出某个片段级操作此刻可用的模型：绑定了这个操作、按给定的输入角色报得出价的（与受理同一个
// QuoteWithFacts）。报不出价的不列（同 SpeechModels：会员等级不够、没配价的出现在下拉里，点下去也是 422）。
// 时长按缺省报价（列表只用单价）；具体一次多少钱由各工具的 dryRun 报。
func (s *Service) ClipModels(ctx context.Context, operation string, memberLevelID int64, roles domain.RoleCount) (
	[]ClipModel, error) {
	codes, err := s.repo.ListModelsForOperation(ctx, operation)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	out := make([]ClipModel, 0, len(codes))
	for _, code := range codes {
		q, err := s.QuoteWithFacts(ctx, QuoteRequest{ModelCode: code, OperationCode: operation, Count: 1,
			MemberLevelID: memberLevelID, RoleCounts: roles})
		if err != nil {
			continue
		}
		m, err := s.repo.GetModel(ctx, code)
		if err != nil {
			continue
		}
		c := ClipModel{Code: code, Name: m.Name, UnitPrice: q.PriceCredits, Unit: q.PriceUnit}
		if lo, hi, ok := protocol.DurationRangeOf(m.APIProtocol, operation); ok {
			c.MinSec, c.MaxSec = lo, hi
		}
		out = append(out, c)
	}
	return out, nil
}
