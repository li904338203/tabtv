package wiring

// 画布 Agent 付费工具的端口适配器（AGT-24）：生成受理 / 查状态 / 取消、带输入解析的报价，
// 以及三个工具（canvas_apply / generate / cancel_task）的装配。
//
// ⚠️ 受理走 generation/app.Service.Accept —— 与画布节点生成、「直接生成」同一条路，
// 不另写一份（否则 Agent 报的价与账本扣的是两个答案，AG-20）。

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	agentrepo "github.com/li904338203/tabtv/backend/internal/agent/repo"
	agenttools "github.com/li904338203/tabtv/backend/internal/agent/tools"
	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	canvasapp "github.com/li904338203/tabtv/backend/internal/canvas/app"
	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	catalogrepo "github.com/li904338203/tabtv/backend/internal/catalog/repo"
	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"
	marketingapp "github.com/li904338203/tabtv/backend/internal/marketing/app"
	scriptapp "github.com/li904338203/tabtv/backend/internal/script/app"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// AgentGenerator 把生成域的受理 / 查询 / 取消接成 agent 的 Generator 端口。
type AgentGenerator struct {
	Gen *genapp.Service
	// Ledger 余额不足时取当前余额，好让模型说出「还差多少」。可为 nil。
	Ledger *billingrepo.Repo
}

func (a AgentGenerator) Accept(ctx context.Context, in agentapp.GenerateInput) (agentapp.GenerateResult, error) {
	res, err := a.Gen.Accept(ctx, genapp.AcceptInput{
		WorkspaceID: in.WorkspaceID, PayerAccountID: in.PayerAccountID, UserID: in.UserID,
		ProjectID: in.ProjectID, NodeID: in.NodeID,
		OperationCode: in.OperationCode, ModelCode: in.ModelCode,
		IdempotencyKey: in.IdempotencyKey, MemberLevelID: in.MemberLevelID,
		Count: in.Count, DurationSec: in.DurationSec, Text: in.Text,
		Params: in.Params, DeclaredInputs: in.Inputs,
	})
	if err != nil {
		if bizCode(err) == errcode.InsufficientCredits {
			bal := -1
			if a.Ledger != nil {
				if b, e := a.Ledger.Balance(ctx, in.PayerAccountID); e == nil {
					bal = b
				}
			}
			return agentapp.GenerateResult{}, &agentapp.InsufficientCreditsError{Balance: bal, Cause: err}
		}
		return agentapp.GenerateResult{}, err
	}
	return agentapp.GenerateResult{GenerationID: res.Generation.ID, Credits: res.Generation.CreditsCost,
		Status: res.Generation.Status, Replayed: res.Replayed}, nil
}

func (a AgentGenerator) Status(ctx context.Context, ws, id int64) (agentapp.TaskStatus, error) {
	g, err := a.Gen.Get(ctx, ws, id)
	if err != nil {
		return agentapp.TaskStatus{}, err
	}
	return agentapp.TaskStatus{Status: g.Status, Terminal: gendomain.IsTerminal(g.Status)}, nil
}

// Cancel 取消任务。已结束的任务翻成 agentapp.ErrTaskFinished（第二次取消不是错误，退款也只发生一次）。
func (a AgentGenerator) Cancel(ctx context.Context, ws, id int64, reason string) error {
	err := a.Gen.Cancel(ctx, ws, id, reason)
	if bizCode(err) == errcode.InvalidStateTransition {
		return errors.Join(agentapp.ErrTaskFinished, err)
	}
	return err
}

// AgentGenQuoter 是付费工具用的报价：给了输入就先按受理同一条解析数出角色计数，再 QuoteWithFacts。
//
// ⚠️ 不按真实输入数的话，能力矩阵（img2video 恰好 1 图、首尾帧恰好 2 图）会在报价时把
// 合法调用拒掉；信模型报的数又等于让 min/max 形同虚设。
type AgentGenQuoter struct {
	Catalog *catalogapp.Service
	Canvas  *canvasapp.Service
}

func (a AgentGenQuoter) QuoteTurn(ctx context.Context, modelCode string, level int64) (agentapp.Quote, error) {
	return AgentQuoter{a.Catalog}.QuoteTurn(ctx, modelCode, level)
}

func (a AgentGenQuoter) QuoteGeneration(ctx context.Context, in agentapp.QuoteInput) (agentapp.Quote, error) {
	d, err := quoteWithResolvedInputs(ctx, a.Catalog, a.Canvas, genQuoteArgs{
		ModelCode: in.ModelCode, OperationCode: in.OperationCode, RoleCount: in.RoleCount,
		Text: in.Text, MemberLevelID: in.MemberLevelID, Count: in.Count, DurationSec: in.DurationSec,
		WorkspaceID: in.WorkspaceID, ProjectID: in.ProjectID, NodeID: in.NodeID, DeclaredInputs: in.DeclaredInputs,
	})
	if err != nil {
		return agentapp.Quote{}, err
	}
	return agentapp.Quote{Credits: d.Credits, ModelID: d.ModelID}, nil
}

// genQuoteArgs 是「画布上生成」报价的入参（Agent 与 canvasgen 两个报价适配器共用）。
type genQuoteArgs struct {
	ModelCode      string
	OperationCode  string
	RoleCount      map[string]int
	Text           string
	MemberLevelID  int64
	Count          int
	DurationSec    int
	WorkspaceID    int64
	ProjectID      int64
	NodeID         string
	DeclaredInputs json.RawMessage
}

// quoteWithResolvedInputs 是「画布上生成」报价口径的唯一实现（MCPT-08 自 AgentGenQuoter 抽出）：
// 给了输入就先按受理同一条解析数出角色计数，再 QuoteWithFacts；没给输入时用调用方的 RoleCount。
func quoteWithResolvedInputs(ctx context.Context, cat *catalogapp.Service, cv *canvasapp.Service,
	in genQuoteArgs) (catalogapp.QuoteDetail, error) {
	roles := in.RoleCount
	if len(in.DeclaredInputs) > 0 && cv != nil {
		out, err := cv.ResolveForAccept(ctx, in.WorkspaceID, in.ProjectID, in.NodeID,
			in.OperationCode, in.DeclaredInputs)
		if err != nil {
			return catalogapp.QuoteDetail{}, err
		}
		if len(out.Issues) > 0 {
			return catalogapp.QuoteDetail{}, httpx.Errf(errcode.InvalidArgument, out.Issues[0])
		}
		roles = out.RoleCounts
	}
	return cat.QuoteWithFacts(ctx, catalogapp.QuoteRequest{
		ModelCode: in.ModelCode, OperationCode: in.OperationCode,
		Count: in.Count, DurationSec: in.DurationSec, Text: in.Text,
		MemberLevelID: in.MemberLevelID, RoleCounts: toRoleCount(roles),
	})
}

// AgentNodeParams 把目录的画布配置接成 agent 的 NodeParams 端口（generate 归一参数名用）。
//
// 走 Config（与 GET /canvas/config 同一份合成结果、按 revision 缓存），不另写一条查 node_param 的 SQL：
// 两份查询早晚对「哪些参数绑在哪类节点上」给出不同答案。
type AgentNodeParams struct{ Catalog *catalogapp.Service }

func (a AgentNodeParams) NodeParamCodes(ctx context.Context, nodeType string) ([]string, error) {
	params, err := a.nodeTypeParams(ctx, nodeType)
	if err != nil || params == nil {
		return nil, err
	}
	codes := make([]string, 0, len(params))
	for _, p := range params {
		codes = append(codes, p.Code)
	}
	return codes, nil
}

// NodeParamDefaults 实现 canvasgen.NodeParamDefaulter：各参数的 defaultValue（空的不列），
// 就是参数面板给新节点补的那一份 —— Agent / MCP 建的节点据此补默认值。
func (a AgentNodeParams) NodeParamDefaults(ctx context.Context, nodeType string) (map[string]string, error) {
	params, err := a.nodeTypeParams(ctx, nodeType)
	if err != nil || params == nil {
		return nil, err
	}
	out := map[string]string{}
	for _, p := range params {
		if p.DefaultValue != "" {
			out[p.Code] = p.DefaultValue
		}
	}
	return out, nil
}

// nodeTypeParams 取 Config 快照里该节点类型的参数；目录未接入或没有这类节点时返回 nil。
func (a AgentNodeParams) nodeTypeParams(ctx context.Context, nodeType string) ([]catalogrepo.NodeParamView, error) {
	if a.Catalog == nil {
		return nil, nil
	}
	snap, err := a.Catalog.Config(ctx)
	if err != nil {
		return nil, err
	}
	for _, nt := range snap.NodeTypes {
		if nt.Code == nodeType {
			return nt.Params, nil
		}
	}
	return nil, nil
}

// AgentPaidToolsInput 是付费工具装配的入参。
type AgentPaidToolsInput struct {
	AgentRepo *agentrepo.Repo
	Gen       *genapp.Service
	Catalog   *catalogapp.Service
	// Canvas 写画布 + 报价时的输入解析。⚠️ 要与受理侧（Gen 的输入解析）读同一个库。
	Canvas  *canvasapp.Service
	Billing *billingrepo.Repo
	// Bus 推追加额度卡的 agent.card 事件；可为 nil。
	Bus    BusPublisher
	NextID func() int64
	Logger *slog.Logger
	// Script 营销会话 generate 把角色资产换成图（MKT-T-22）；可为 nil（资产角色届时报错）。
	Script *scriptapp.Service
	// Marketing 营销视频节点模型没写画幅时取所选创意的推荐画幅；可为 nil（按创作偏好 / 节点默认补）。
	Marketing *marketingapp.Service
}

// NewAgentPaidTools 装出 canvas_apply / generate / cancel_task。
func NewAgentPaidTools(in AgentPaidToolsInput) []agentapp.Tool {
	canvas := AgentCanvas{in.Canvas}
	gen := AgentGenerator{Gen: in.Gen, Ledger: in.Billing}
	budget := agentapp.NewBudget(in.AgentRepo, in.NextID, in.Logger)
	if in.Bus != nil {
		budget.SetEvents(AgentEvents{in.Bus})
	}
	return []agentapp.Tool{
		agenttools.CanvasApply{Canvas: canvas},
		agenttools.Generate{Canvas: canvas, Quoter: AgentGenQuoter{Catalog: in.Catalog, Canvas: in.Canvas},
			Generator: gen, Budget: budget, Params: AgentNodeParams{in.Catalog},
			Outputs: CatalogOperationOutputs{in.Catalog}, Logger: in.Logger,
			Plans: AgentPlanSource{Repo: in.AgentRepo, Script: in.Script, Marketing: in.Marketing},
			Ops:   AgentOperationCatalog{Catalog: in.Catalog}},
		agenttools.CancelTask{Generator: gen, Steps: in.AgentRepo},
	}
}

func bizCode(err error) errcode.Code {
	var be *httpx.BizError
	if errors.As(err, &be) {
		return be.Code
	}
	return 0
}

var (
	_ agentapp.Generator  = AgentGenerator{}
	_ agentapp.Quoter     = AgentGenQuoter{}
	_ agentapp.NodeParams = AgentNodeParams{}
)
