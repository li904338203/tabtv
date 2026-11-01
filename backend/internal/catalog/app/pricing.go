package app

import (
	"context"
	"errors"

	"github.com/li904338203/tabtv/backend/internal/catalog/domain"
	"github.com/li904338203/tabtv/backend/internal/catalog/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/provider/protocol"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// PriceRow 是后台价格列表的一行：价格本身 + 此刻靠它报价的能力。
type PriceRow struct {
	domain.Price
	// InUseBy 去掉（或停用）这条价格之后会**报不出价**的模型；不绑模型的能力记为空串。
	// 后台据此在点之前就把「删除 / 停用」置灰并说明原因，而不是点了收一个 409。
	InUseBy []string
}

func (s *Service) ListPrices(ctx context.Context) ([]PriceRow, error) {
	prices, live, err := s.pricingFacts(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]PriceRow, 0, len(prices))
	for _, p := range prices {
		rows = append(rows, PriceRow{Price: p, InUseBy: domain.QuoteLosers(prices, live, p)})
	}
	return rows, nil
}

// pricingFacts 取齐判定「还有谁在靠这条价格报价」要的两样事实：全部价格与在用的绑定。
//
// ⚠️ 在用 = 模型已启用 ∧ 绑定已启用，外加**不绑模型的能力**（时间线渲染只靠兜底价计费）——
// 少了后者的话，删掉渲染的兜底价时没有任何模型"受影响"，于是放行，
// 而从那一刻起所有人都渲不了片子。
func (s *Service) pricingFacts(ctx context.Context) ([]domain.Price, []domain.LiveBinding, error) {
	prices, err := s.repo.ListPrices(ctx)
	if err != nil {
		return nil, nil, httpx.Wrap(errcode.Internal, err)
	}
	models, err := s.repo.ListModels(ctx)
	if err != nil {
		return nil, nil, httpx.Wrap(errcode.Internal, err)
	}
	bindings, err := s.repo.AllModelOperations(ctx)
	if err != nil {
		return nil, nil, httpx.Wrap(errcode.Internal, err)
	}
	var live []domain.LiveBinding
	for _, m := range models {
		if m.Status != "enabled" {
			continue
		}
		for _, o := range bindings[m.Code] {
			if o.IsEnabled {
				live = append(live, domain.LiveBinding{ModelCode: m.Code, OperationCode: o.OperationCode})
			}
		}
	}
	for _, op := range domain.ModellessOperations {
		live = append(live, domain.LiveBinding{OperationCode: op})
	}
	return prices, live, nil
}

// guardQuoteLoss 停用或删除一条价格之前：还有在用的能力只靠它报价的话拒绝，并**点名是谁**。
//
// ⚠️ 02 §3.3：下架价格不能让已启用的能力失去可用报价。此前只拦了「删兜底价」一种 ——
// 删掉某个模型唯一的精确价、或者把它停用，都会放行，而那之后用户那边的这个模型
// 一点生成就是「该能力缺少可用价格」。
func (s *Service) guardQuoteLoss(ctx context.Context, target domain.Price, verb string) error {
	prices, live, err := s.pricingFacts(ctx)
	if err != nil {
		return err
	}
	losers := domain.QuoteLosers(prices, live, target)
	if len(losers) == 0 {
		return nil
	}
	names := make([]string, 0, len(losers))
	for _, m := range losers {
		if m == "" {
			names = append(names, "不绑模型的「"+target.OperationCode+"」")
			continue
		}
		names = append(names, m)
	}
	return httpx.Errf(errcode.ResourceInUse, "以下已启用的能力正靠这条价格报价，"+verb+
		"后将报不出价："+joinComma(names)+"。请先给它们配上别的价格，或先停用对应的模型或操作。")
}

func (s *Service) SavePrice(ctx context.Context, operatorID int64, p domain.Price) error {
	if err := domain.ValidatePrice(p); err != nil {
		return httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	if !p.Enabled {
		if err := s.guardQuoteLoss(ctx, p, "停用"); err != nil {
			return err
		}
	}
	if err := s.repo.SavePrice(ctx, s.nextID(), p); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return httpx.Errf(errcode.NotFound, "operation 或 model 不存在")
		}
		return httpx.Wrap(errcode.Internal, err)
	}
	scope := "精确价"
	if p.IsFallback() {
		scope = "兜底价"
	}
	return s.bumpScope(ctx, ScopeModel, operatorID, "pricing.save", map[string]any{
		"operation": p.OperationCode, "model": p.ModelCode, "kind": scope,
		"credits": p.Credits, "unit": string(p.Unit), "enabled": p.Enabled,
	})
}

// DeletePrice 删价格。
//
// ⚠️ 下架价格不能让已启用的能力失去可用报价（02 §3.3）：兜底价与精确价同一条判定
// （guardQuoteLoss → domain.QuoteLosers），不再只拦兜底价。
func (s *Service) DeletePrice(ctx context.Context, operatorID int64, operationCode, modelCode string) error {
	if err := s.guardQuoteLoss(ctx, domain.Price{OperationCode: operationCode, ModelCode: modelCode},
		"删除"); err != nil {
		return err
	}
	if err := s.repo.DeletePrice(ctx, operationCode, modelCode); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return httpx.Err(errcode.NotFound)
		}
		return httpx.Wrap(errcode.Internal, err)
	}
	return s.bumpScope(ctx, ScopeModel, operatorID, "pricing.delete", map[string]any{
		"operation": operationCode, "model": modelCode,
	})
}

// QuoteRequest 是报价请求。
type QuoteRequest struct {
	ModelCode     string
	OperationCode string
	Count         int
	DurationSec   int
	Text          string
	// MemberLevelID 调用者当前会员等级；0 表示无会员。
	MemberLevelID int64
	// RoleCounts 各输入角色的数量，用于能力矩阵校验。
	RoleCounts domain.RoleCount
	// Params 本次生成的节点参数（清晰度、画幅等）；nil = 只做与参数无关的校验（规划报价拿不到参数）。
	// 用来在扣费之前按协议范围校验（04 文档 P-61）。
	Params map[string]any
	// AppendixChars 是服务端追加在提示词末尾的段落（营销片段 + 场景约束段）的码点数
	// （MKT-T-21、MKD-17）。⚠️ **只参与 max_input_chars 判定，不参与计价**：
	// 不算进长度的话超长要等上游回 400（那时已扣费）；算进计价的话 per_kchar 的文本模型
	// 会因为平台加的约束段多收用户的钱。
	AppendixChars int
}

// QuoteDetail 是给**生成受理**用的报价：除费用外还带上要冻结进任务快照的事实。
//
// ⚠️ 与 Quote 分开而不是给 Quote 加字段：Quote 是下发给前端的，
// 而 provider code、api protocol 这些属于内部配置，不该出现在用户端响应里。
type QuoteDetail struct {
	Quote
	ModelID      int64
	OperationID  int64
	ProviderCode string
	APIProtocol  string
	ResultType   string
	PriceUnit    string
	PriceCredits int
}

// Quote 是报价结果。
type Quote struct {
	Credits          int    `json:"credits"`
	Unit             string `json:"unit"`
	EstimatedSeconds int    `json:"estimatedSeconds"`
	DeadlineSeconds  int    `json:"deadlineSeconds"`
	OutputNodeType   string `json:"outputNodeType"`
}

// Quote 计算报价并做**扣费前的全部校验**。
//
// ⚠️ 校验顺序固定（AC-10、FLOW-07）：
//
//	能力矩阵 → 参数范围 → 会员等级 → 计价
//
// 连线规则在更外层（画布侧）已校验。这里的每一条都必须在
// **调用上游与扣费之前**给出明确错误，而不是等上游返回 400。
// Quote 只回费用与耗时，供前端展示。
func (s *Service) Quote(ctx context.Context, req QuoteRequest) (Quote, error) {
	d, err := s.QuoteWithFacts(ctx, req)
	return d.Quote, err
}

// QuoteWithFacts 报价并回带配置事实，供受理冻结快照。
func (s *Service) QuoteWithFacts(ctx context.Context, req QuoteRequest) (QuoteDetail, error) {
	facts, err := s.repo.LoadQuoteFacts(ctx, req.ModelCode, req.OperationCode)
	if err != nil {
		return QuoteDetail{}, httpx.Wrap(errcode.Internal, err)
	}
	if !facts.Found {
		return QuoteDetail{}, quoteBlocked("该模型不支持操作 "+req.OperationCode, domain.QuoteBlockNotBound)
	}
	if !facts.ModelEnabled || !facts.ProviderReady {
		return QuoteDetail{}, httpx.Errf(errcode.ModelUnavailable, "模型当前不可用")
	}
	if !facts.OperationEnabled || !facts.ModelOpEnabled {
		return QuoteDetail{}, quoteBlocked("该模型的操作 "+req.OperationCode+" 未启用",
			domain.QuoteBlockOperationDisabled)
	}

	// ① 能力矩阵（GAP-22）
	caps, err := s.capabilitiesFor(ctx, facts.ModelID, req.OperationCode)
	if err != nil {
		return QuoteDetail{}, err
	}
	if len(caps) == 0 {
		return QuoteDetail{}, quoteBlocked("该模型此操作未声明输入能力，无法校验输入",
			domain.QuoteBlockCapabilityUndeclared)
	}
	if err := domain.NewCapabilitySet(caps).Check(req.RoleCounts); err != nil {
		return QuoteDetail{}, quoteBlocked(err.Error(), domain.QuoteBlockInputMismatch)
	}

	// ①' 视频参数的协议范围（04 文档 P-61）：时长、清晰度、画幅。
	// 原先只在 Worker 构造上游请求时才查 —— 一条 3 秒的视频（最短 4 秒）照样报价、扣费、排队，
	// 再失败退款。规划报价没有参数时只查时长。
	if facts.ResultType == "video" {
		if err := domain.CheckVideoParams(facts.APIProtocol, req.OperationCode, req.DurationSec, req.Params); err != nil {
			return QuoteDetail{}, httpx.Errf(errcode.InvalidArgument, err.Error())
		}
	}
	// ①'' 其余协议在扣费之前要查的参数（docs/agent/smartcut SCT-39）：配乐与配音效的单次时长、
	// 只出纯音乐的模型不收「带人声」。原先只在 Worker 构造请求时才查，那时已经扣费、排队，失败再退款。
	// 协议没声明时放行（不知道就别拦）。
	if err := protocol.CheckParamsOf(facts.APIProtocol, req.OperationCode, req.DurationSec, req.Params); err != nil {
		return QuoteDetail{}, quoteBlocked(err.Error(), domain.QuoteBlockInputMismatch)
	}

	// ② 输入长度上限。**按字符不按 token**：
	// 调用方手上只有字符串，token 数依分词器而异（03 §2.3）。
	if facts.MaxInputChars != nil && *facts.MaxInputChars > 0 {
		appendix := max(req.AppendixChars, 0)
		if n := runeLen(req.Text) + appendix; n > *facts.MaxInputChars {
			msg := "输入长度 " + itoa(n) + " 超过该模型上限 " + itoa(*facts.MaxInputChars) + " 字符"
			if appendix > 0 {
				// 说清附加段占了多少：用户只看得到自己写的那部分，不说的话他会以为
				// "我才写了两百字怎么就超了"。
				msg += "（其中所选营销片段与约束段 " + itoa(appendix) + " 字）"
			}
			return QuoteDetail{}, httpx.Errf(errcode.InvalidArgument, msg)
		}
	}

	// ③ 会员门槛
	if facts.MinMemberLevelID != nil && *facts.MinMemberLevelID > 0 {
		if req.MemberLevelID < *facts.MinMemberLevelID {
			return QuoteDetail{}, httpx.Errf(errcode.MemberLevelRequired,
				"该模型需要更高的会员等级")
		}
	}

	// ④ 计价：精确价优先，回落兜底价
	prices, err := s.repo.ListPrices(ctx)
	if err != nil {
		return QuoteDetail{}, httpx.Wrap(errcode.Internal, err)
	}
	price, ok := domain.ResolvePrice(prices, req.OperationCode, req.ModelCode)
	if !ok {
		return QuoteDetail{}, httpx.Errf(errcode.PricingMissing,
			"该能力缺少可用价格，请联系管理员")
	}
	credits, err := domain.CalcCredits(domain.QuoteInput{
		Unit: price.Unit, Credits: price.Credits, Count: req.Count,
		DurationSec: req.DurationSec, Text: req.Text,
	})
	if err != nil {
		return QuoteDetail{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}

	est := domain.ResolveEstSeconds(facts.OperationEst, facts.ModelEst)
	return QuoteDetail{
		Quote: Quote{
			Credits: credits, Unit: string(price.Unit),
			EstimatedSeconds: est, DeadlineSeconds: domain.CalcDeadlineSec(est),
			// 产出类型查 operation.out_node_type，**不靠操作码拼字符串猜**。
			OutputNodeType: facts.OutNodeType,
		},
		ModelID: facts.ModelID, OperationID: facts.OperationID,
		ProviderCode: facts.ProviderCode, APIProtocol: facts.APIProtocol,
		ResultType: facts.ResultType,
		PriceUnit:  string(price.Unit), PriceCredits: price.Credits,
	}, nil
}

// quoteBlocked 构造带细分原因的 CapabilityUnsupported（见 domain.QuoteBlockReasonKey）。
func quoteBlocked(msg, reason string) *httpx.BizError {
	return httpx.Errf(errcode.CapabilityUnsupported, msg).
		WithDetails(map[string]any{domain.QuoteBlockReasonKey: reason})
}

// capabilitiesFor 取某模型某操作的输入能力矩阵（含继承，见 domain.CapabilityInheritance）。
// 报价与 ModelsWithAvailability 共用 —— 后者要按能力矩阵的下限构造探测输入，两处的
// 「这个操作声明了什么能力」必须是同一个答案。
func (s *Service) capabilitiesFor(ctx context.Context, modelID int64, op string) ([]domain.Capability, error) {
	capsByOp, err := s.repo.ListCapabilities(ctx, modelID, op)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	caps := capsByOp[op]
	if len(caps) == 0 {
		// 推理操作沿用同一模型在画布里的输入能力（agent_chat ← text2text），
		// 不要求运营为 Agent 再配一份能力矩阵（2026-10-16 实测：只绑了 agent_chat 时报价一律 42203）。
		if base, ok := domain.CapabilityInheritance[op]; ok {
			inherited, err := s.repo.ListCapabilities(ctx, modelID, base)
			if err != nil {
				return nil, httpx.Wrap(errcode.Internal, err)
			}
			caps = inherited[base]
		}
	}
	return caps, nil
}

// ValidateMemberLevel 供发布检查使用。
func (s *Service) validateMemberLevel(ctx context.Context, levelID *int64) (bool, error) {
	if levelID == nil || *levelID == 0 {
		return true, nil
	}
	return s.repo.MemberLevelExists(ctx, *levelID)
}

func (s *Service) auditPricing(ctx context.Context, operatorID int64, action string, after any) {
	if s.audit != nil {
		s.audit.Write(ctx, audit.Entry{
			ActorType: audit.ActorAdmin, ActorID: operatorID,
			Action: action, ObjectType: "pricing", After: after,
		})
	}
}

func runeLen(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// QuoteOperation 给**不绑模型**的能力报价（时间线渲染）。
//
// ⚠️ 渲染没有模型，所以它走不了 `QuoteWithFacts` ——
// 那条路径是从 `model_operation` 的绑定开始的，没有模型时
// 第一步就报「该模型不支持操作 timeline_render」（42203），
// 而那句话对渲染完全不成立（P10-03 实测踩到）。
//
// 但它仍然用**同一张 pricing 表**与同一个 `CalcCredits`：
// 复用的是"价格从哪来、怎么算"，不是"模型能不能做"。
// 另写一份价格的话"渲染多少钱"会有两个答案，而运营只会改其中一个。
func (s *Service) QuoteOperation(ctx context.Context, operationCode string,
	count, durationSec int) (int, error) {
	// ⚠️ 只给登记过的「不绑模型」能力报价：拿一个绑模型的操作来问，这里只会取兜底价、
	// 把精确价整个忽略掉 —— 报出来的是错的价。登记表同时是删价保护的依据（pricingFacts）。
	if !domain.IsModelless(operationCode) {
		return 0, httpx.Errf(errcode.Internal, "操作 "+operationCode+" 不是不绑模型的能力，不能走兜底价报价")
	}
	prices, err := s.repo.ListPrices(ctx)
	if err != nil {
		return 0, httpx.Wrap(errcode.Internal, err)
	}
	// 只找兜底价（model 为空）：不绑模型的能力没有"精确价"那一档。
	price, ok := domain.ResolvePrice(prices, operationCode, "")
	if !ok {
		return 0, httpx.Errf(errcode.PricingMissing,
			"能力 "+operationCode+" 缺少可用价格，请联系管理员")
	}
	credits, err := domain.CalcCredits(domain.QuoteInput{
		Unit: price.Unit, Credits: price.Credits,
		Count: count, DurationSec: durationSec,
	})
	if err != nil {
		return 0, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	return credits, nil
}
