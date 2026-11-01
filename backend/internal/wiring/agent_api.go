package wiring

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"time"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	agentdomain "github.com/li904338203/tabtv/backend/internal/agent/domain"
	agentrepo "github.com/li904338203/tabtv/backend/internal/agent/repo"
	billingapp "github.com/li904338203/tabtv/backend/internal/billing/app"
	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	canvasapp "github.com/li904338203/tabtv/backend/internal/canvas/app"
	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	catalogdomain "github.com/li904338203/tabtv/backend/internal/catalog/domain"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"
	marketingapp "github.com/li904338203/tabtv/backend/internal/marketing/app"
	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	providerapp "github.com/li904338203/tabtv/backend/internal/provider/app"
	scriptapp "github.com/li904338203/tabtv/backend/internal/script/app"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 画布 Agent API 侧服务（AGT-26）的端口适配器：agent 域不 import 其他域的 repo / app 包，
// 由这里把画布、媒体、脚本、目录、账本、生成接成 agent/app/service_ports.go 的端口。

// AgentAPIProjects 把画布域接成 agentapp.APIProjects。
type AgentAPIProjects struct{ Svc *canvasapp.Service }

func (a AgentAPIProjects) EnsureProject(ctx context.Context, ws, userID int64, role collab.Role,
	name, idemKey, origin string) (int64, error) {
	// origin：营销会话传 "marketing"，其余为空（MKD-3）；只在新建时写入，幂等重放不改。
	p, _, err := a.Svc.EnsureAgentProject(ctx, ws, userID, role, name, idemKey, origin)
	return p.ID, err
}

func (a AgentAPIProjects) ProjectNodes(ctx context.Context, ws, projectID int64) (
	map[string]agentapp.ProjectNode, error) {
	nodes, err := a.Svc.AgentProject(ctx, ws, projectID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]agentapp.ProjectNode, len(nodes))
	for id, n := range nodes {
		out[id] = agentapp.ProjectNode{ID: n.ID, Type: n.Type, Title: n.Title}
	}
	return out, nil
}

func (a AgentAPIProjects) DocVersion(ctx context.Context, ws, projectID int64, path string) (int, bool, error) {
	// 读文档只要 view；这里是服务端自己核对版本，用 viewer 角色即可。
	d, err := a.Svc.DocumentByPath(ctx, ws, projectID, collab.RoleViewer, path)
	if err != nil {
		var be *httpx.BizError
		if errors.As(err, &be) && be.Code == errcode.NotFound {
			return 0, false, nil
		}
		return 0, false, err
	}
	return d.Version, true, nil
}

// AgentRefs 把媒体域与脚本域接成 agentapp.RefLookup（按工作区校验归属）。
type AgentRefs struct {
	Media  *mediaapp.Service
	Script *scriptapp.Service
}

func (a AgentRefs) MediaKind(ctx context.Context, ws, mediaID int64) (string, error) {
	info, err := a.Media.LookupInWorkspace(ctx, ws, mediaID)
	if err != nil {
		var be *httpx.BizError
		if errors.As(err, &be) {
			return "", err
		}
		// 媒体域仓储的「不存在」不是业务错误：一律按 404 处理，不泄漏存在性。
		return "", httpx.Errf(errcode.NotFound, "素材不存在")
	}
	return info.Kind, nil
}

func (a AgentRefs) Asset(ctx context.Context, ws, assetID int64) (string, string, error) {
	as, err := a.Script.AssetInWorkspace(ctx, ws, assetID)
	if err != nil {
		return "", "", err
	}
	return as.Name, as.Type, nil
}

// AgentPlanQuoter 按规划确认卡的计划项逐项调 QuoteWithFacts（与节点报价、受理同一条路径）。
//
// ⚠️ submit_plan（AGT-23）发卡时与批准时重新报价（AGT-26）必须用**同一个**实现：
// 两边各写一份映射的话，「卡片上的价」与「批准时的价」会因为映射差异永远对不上，
// 用户每点一次确认都收到「价格已更新」。
type AgentPlanQuoter struct{ Catalog *catalogapp.Service }

// defaultPlanOperation 是计划项没写操作码时按产出类型取的默认操作（seed 的连线默认操作）。
var defaultPlanOperation = map[string]string{
	"video": "text2video",
	"image": "text2img",
	"audio": "text2audio",
	// 多段（AGT-50）：与脚本域 / 资产受理实际用的操作码逐字一致。
	"storyboard": "storyboard_gen",
	"compose":    "prompt_compose",
	"asset":      "asset_gen",
}

// perCallPlanKinds 是「一条计划项 = 若干次独立调用」的类型：每次调用单独受理、单独计价，
// 所以按「单次报价 × 数量」报。直接把数量当 count 交给 QuoteWithFacts 的话，per_kchar 计价
// 不乘 count（§5.2），十条提示词会被报成一条的钱。
var perCallPlanKinds = map[string]bool{"storyboard": true, "compose": true, "asset": true}

// QuotePlanItem 报一条计划项的价，与真实扣费同一个口径：
//   - render：不绑模型，走 timeline_render 的兜底价（同 RenderQuoter，渲染受理用的就是它）；
//   - storyboard / compose / asset：单次报价 × 数量；
//   - 其余：PlanQuoteRequest → QuoteWithFacts。
//
// 导出给对外 MCP 的 quote（MCPT-05）：真正保证与受理同价的是这个函数而不只是 PlanQuoteRequest 映射 ——
// 只复用映射的话，MCP 的渲染与多段（storyboard × N）价格会与 Agent 规划卡对不上。
func QuotePlanItem(ctx context.Context, c *catalogapp.Service, it agentdomain.PlanItem,
	memberLevelID int64) (credits int, unit string, err error) {
	if it.Kind == "render" {
		q := it.Quantity
		if q <= 0 {
			q = 1
		}
		n, err := RenderQuoter{c}.QuoteRender(ctx, q, memberLevelID)
		return n, "per_second", err
	}
	req := PlanQuoteRequest(it, memberLevelID)
	times := 1
	if perCallPlanKinds[it.Kind] {
		times = req.Count
		if times <= 0 {
			times = 1
		}
		req.Count = 1
	}
	q, err := c.QuoteWithFacts(ctx, req)
	if err != nil {
		return 0, "", err
	}
	if times > 1 && q.Credits > math.MaxInt32/times {
		return 0, "", httpx.Errf(errcode.InvalidArgument, "数量过大，金额溢出")
	}
	return q.Credits * times, q.Unit, nil
}

// PlanQuoteRequest 把一条计划项换成报价请求（导出给 submit_plan 复用）。
func PlanQuoteRequest(it agentdomain.PlanItem, memberLevelID int64) catalogapp.QuoteRequest {
	op := it.OperationCode
	if op == "" {
		op = defaultPlanOperation[it.Kind]
	}
	req := catalogapp.QuoteRequest{
		ModelCode: it.ModelCode, OperationCode: op, MemberLevelID: memberLevelID, Count: 1,
		RoleCounts: toRoleCount(it.Inputs),
	}
	q := it.Quantity
	if q <= 0 {
		q = 1
	}
	if it.Unit == "second" {
		req.DurationSec = q
	} else {
		req.Count = q
	}
	return req
}

func (a AgentPlanQuoter) QuotePlan(ctx context.Context, items []agentdomain.PlanItem,
	memberLevelID int64) ([]agentdomain.PlanItem, error) {
	out := make([]agentdomain.PlanItem, len(items))
	for i, it := range items {
		credits, _, err := QuotePlanItem(ctx, a.Catalog, it, memberLevelID)
		if err != nil {
			return nil, err
		}
		it.Credits = credits
		// 卡片上这一项发卡时可能是「不可用」：这次报得出价就把原因清掉，否则改写卡片时带着旧原因。
		it.BlockedCode, it.BlockedReason, it.UserFixable = "", "", false
		out[i] = it
	}
	return out, nil
}

// PriceItem 报一项价（quote / submit_plan，AGT-23），与 QuotePlan 同一个 PlanQuoteRequest。
//
// 业务上的「不可用」（模型下线、能力不支持、会员等级不够、缺价…）放在结果里而不是返回错误：
// 一项不可用不该让整张规划卡发不出来。只有内部错误（查库失败）才返回 error。
// ArgFault 说明这个「不可用」是不是模型自己写错的参数（submit_plan 据此退回给模型改）。
func (a AgentPlanQuoter) PriceItem(ctx context.Context, it agentdomain.PlanItem,
	memberLevelID int64) (agentapp.PricedItem, error) {
	it.Credits, it.BlockedCode, it.BlockedReason, it.UserFixable = 0, "", "", false
	op := ""
	if it.Kind != "render" {
		op = PlanQuoteRequest(it, memberLevelID).OperationCode
	}
	credits, unit, err := QuotePlanItem(ctx, a.Catalog, it, memberLevelID)
	if err != nil {
		b, internal := quoteBlock(err)
		if internal != nil {
			return agentapp.PricedItem{}, internal
		}
		it.BlockedCode, it.BlockedReason, it.UserFixable = b.Code, b.Reason, b.UserFixable
		return agentapp.PricedItem{Item: it, BlockedCode: it.BlockedCode, BlockedReason: it.BlockedReason,
			UserFixable: it.UserFixable, ArgFault: b.ArgFault, Operation: op}, nil
	}
	it.Credits = credits
	return agentapp.PricedItem{Item: it, Unit: unit, Available: true, Operation: op}, nil
}

// quoteBlocked 是报价阻塞的分类结果。
type quoteBlocked struct {
	Code   string
	Reason string
	// UserFixable 用户自己能解决（升级会员 / 调参数）。
	UserFixable bool
	// ArgFault 非空 = 调用方给错了参数（agentapp.PlanFault*），见 planArgFault。
	ArgFault string
}

// quoteBlock 把报价错误分成「业务上不可用」（原因码 / 原因 / 是否用户可解决 / 是否参数错误）与内部错误
// （查库失败等，原样放在 internal 里返回）。Agent 的 PriceItem 与 MCP 的 quote（MCPT-05）共用：
// 分类各写一份的话，同一个模型在两边一个说「升级会员可用」、一个说「需要运营处理」。
func quoteBlock(err error) (quoteBlocked, error) {
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code == errcode.Internal {
		return quoteBlocked{}, err
	}
	return quoteBlocked{Code: be.Code.Name(), Reason: be.Message, UserFixable: planBlockUserFixable(be.Code),
		ArgFault: planArgFault(be)}, nil
}

// planBlockUserFixable：报价阻塞里只有两类是用户这边能解决的 —— 会员等级不够（升级）与
// 参数超范围（换时长 / 张数）；模型下线、操作未启用、缺价、能力不支持都要运营处理。
// ⚠️ 按业务码分类，不匹配文案（§5.9：文案改一个字匹配就失效）。
func planBlockUserFixable(code errcode.Code) bool {
	return code == errcode.MemberLevelRequired || code == errcode.InvalidArgument
}

// planArgFault 判断报价阻塞是不是「调用方写错了参数」——对 Agent 来说就是模型自己能改的：
//
//   - InvalidArgument：时长 / 张数 / 输入长度超范围、数量溢出 → quantity；
//   - CapabilityUnsupported + 模型×操作查不到（模型码 / 操作码不存在，或该模型没绑这个操作）→ operation；
//   - CapabilityUnsupported + 输入角色 / 数量不符合能力矩阵（如 text2video 写了 image）→ inputs。
//
// 不算参数错误的：模型下线（ModelUnavailable）、操作未启用 / 没声明能力矩阵（运营没配好）、
// 缺价（PricingMissing）、会员等级不够（MemberLevelRequired，用户升级）—— 这些照常发卡说明原因，
// 换参数也解决不了，退回给模型只会让它乱换模型。余额不足根本不走报价（卡片上算差额）。
// ⚠️ 细分原因取 Details 里的机器可读字段（catalog/domain.QuoteBlock*），不匹配文案。
func planArgFault(be *httpx.BizError) string {
	switch be.Code {
	case errcode.InvalidArgument:
		return agentapp.PlanFaultQuantity
	case errcode.CapabilityUnsupported:
		reason, _ := be.Details[catalogdomain.QuoteBlockReasonKey].(string)
		switch reason {
		case catalogdomain.QuoteBlockNotBound:
			return agentapp.PlanFaultOperation
		case catalogdomain.QuoteBlockInputMismatch:
			return agentapp.PlanFaultInputs
		}
	}
	return ""
}

// AgentAccounts 把账本接成 agentapp.Accounts（只读）。
type AgentAccounts struct{ Billing *billingapp.Service }

func (a AgentAccounts) Balance(ctx context.Context, payerID int64) (int, error) {
	return a.Billing.Balance(ctx, payerID)
}

func (a AgentAccounts) TransactionsByRefs(ctx context.Context, payerID int64,
	refs []agentapp.LedgerRef) ([]agentapp.LedgerEntry, error) {
	in := make([]billingrepo.TxRef, len(refs))
	for i, r := range refs {
		in[i] = billingrepo.TxRef{Type: r.Type, ID: r.ID}
	}
	rows, err := a.Billing.TransactionsByRefs(ctx, payerID, in)
	if err != nil {
		return nil, err
	}
	out := make([]agentapp.LedgerEntry, len(rows))
	for i, t := range rows {
		out[i] = agentapp.LedgerEntry{ID: t.ID, Type: t.Type, Change: t.Change, RefType: t.RefType,
			RefID: t.RefID, Reason: t.Reason, CreatedAt: t.CreatedAt}
	}
	return out, nil
}

// AgentTasks 把生成域接成 agentapp.TaskControl（查状态 / 取消；取消的退款在生成域里，只退一次）。
type AgentTasks struct{ Gen *genapp.Service }

func (a AgentTasks) Status(ctx context.Context, ws, generationID int64) (agentapp.TaskStatus, error) {
	g, err := a.Gen.Get(ctx, ws, generationID)
	if err != nil {
		return agentapp.TaskStatus{}, err
	}
	return agentapp.TaskStatus{Status: g.Status, Terminal: gendomain.IsTerminal(g.Status)}, nil
}

func (a AgentTasks) Cancel(ctx context.Context, ws, generationID int64, reason string) error {
	return a.Gen.Cancel(ctx, ws, generationID, reason)
}

// AgentAPIInput 是 NewAgentService 的入参。
type AgentAPIInput struct {
	Repo    *agentrepo.Repo
	NextID  func() int64
	Canvas  *canvasapp.Service
	Media   *mediaapp.Service
	Script  *scriptapp.Service
	Catalog *catalogapp.Service
	Billing *billingapp.Service
	Gen     *genapp.Service
	// Events 可为 nil（不推事件，前端靠 HTTP 对账）。
	Events agentapp.Events
	Logger *slog.Logger
	// 一键优化提示词（AGT-43）：API 进程里同步调推理模型。Chat 或 Ledger 为 nil 时不接入
	// （接口回「推理模型链路未接入」）。与 Worker 运行器用同一组适配器，计费口径一致。
	Chat       *providerapp.HTTPProvider
	Ledger     *billingrepo.Repo
	Compensate time.Duration
	// Scene 营销场景建会话（MKT-T-17）；nil = 营销场景未开放，scene=marketing 一律 400。
	Scene agentapp.SceneResolver
	// MaxVariants 单次批量变体上限（MKT-T-27）：批准规划时兜底；≤ 0 按缺省 4。
	MaxVariants int
	// Library 建营销会话时取商品 / 角色快照（MKT-T-37）；nil = 未接入（营销关着时不接）。
	Library *marketingapp.Library
}

// NewAgentService 装出 API 侧的 Agent 服务。
func NewAgentService(in AgentAPIInput) *agentapp.Service {
	var optimize *agentapp.OptimizePorts
	if in.Chat != nil && in.Ledger != nil && in.Catalog != nil {
		optimize = &agentapp.OptimizePorts{
			Chat:      AgentChatModel{in.Chat},
			Quoter:    AgentQuoter{in.Catalog},
			Ledger:    AgentLedger{Repo: in.Ledger, Compensate: in.Compensate},
			Reasoning: AgentVideoModels{in.Catalog},
		}
	}
	var skillModels agentapp.SkillModels
	if in.Catalog != nil {
		skillModels = AgentSkillModels{Catalog: in.Catalog}
	}
	var sceneLib agentapp.SceneLibrary
	if in.Library != nil {
		sceneLib = AgentSceneLibrary{Lib: in.Library}
	}
	return agentapp.NewService(in.Repo, in.NextID, agentapp.ServicePorts{
		Projects: AgentAPIProjects{Svc: in.Canvas},
		Refs:     AgentRefs{Media: in.Media, Script: in.Script},
		Plans:    AgentPlanQuoter{Catalog: in.Catalog},
		Accounts: AgentAccounts{Billing: in.Billing},
		Tasks:    AgentTasks{Gen: in.Gen},
		Events:   in.Events,
		Optimize: optimize,
		// 技能列表的模型显示名（MKT-T-05）。
		SkillModels: skillModels,
		Scene:       in.Scene,
		MaxVariants: in.MaxVariants,
		Library:     sceneLib,
		// 技能下发前把输入槽声明换成新形状（MKD-26）：纯函数，不看营销开关。
		SlotDecls: AgentSkillSlots{},
	}, in.Logger)
}

// AgentOperationCatalog 把目录的「模型可用生成方式」接成 agent 的 OperationCatalog 端口。
type AgentOperationCatalog struct{ Catalog *catalogapp.Service }

func (a AgentOperationCatalog) ModelOperations(ctx context.Context, modelCode string) ([]agentdomain.OperationFit, error) {
	if a.Catalog == nil {
		return nil, nil
	}
	fits, err := a.Catalog.ModelOperationFits(ctx, modelCode)
	if err != nil {
		return nil, err
	}
	out := make([]agentdomain.OperationFit, 0, len(fits))
	for _, f := range fits {
		roles := make(map[string]agentdomain.RoleRange, len(f.Caps))
		for _, c := range f.Caps {
			roles[string(c.Role)] = agentdomain.RoleRange{Min: c.MinCount, Max: c.MaxCount}
		}
		out = append(out, agentdomain.OperationFit{Code: f.Code, Output: f.Output, Roles: roles, Limits: f.Limits})
	}
	return out, nil
}

var _ agentapp.OperationCatalog = AgentOperationCatalog{}
