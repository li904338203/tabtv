package wiring

import (
	"context"

	agentdomain "github.com/li904338203/tabtv/backend/internal/agent/domain"
	billingapp "github.com/li904338203/tabtv/backend/internal/billing/app"
	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	mcpapp "github.com/li904338203/tabtv/backend/internal/mcp/app"
	mcpdomain "github.com/li904338203/tabtv/backend/internal/mcp/domain"
)

// MCPQuoter 把目录与账本接成 MCP 的报价端口（MCPT-05）。**MCP 不自己算价**（C-1、C-4）：
//
//   - 单项：与 Web 端 `POST /generations/estimate`（catalog/transport/pricing.go estimate）同一个
//     catalogapp.Service.Quote，QuoteRequest 的构造逐字段相同、**不填默认值**（MCP-AC-05）；
//     会员等级取 Principal（凭据解析时实时查出）。
//   - 计划项：经 AgentPlanQuoter.PriceItem → QuotePlanItem（render 走兜底价、storyboard 等按「单次 × 数量」），
//     与站内 Agent 规划卡同价。只复用 PlanQuoteRequest 映射的话渲染与多段的价格会对不上。
//   - 业务上不可用与 Agent 同一个分类（quoteBlock）。
type MCPQuoter struct {
	Catalog *catalogapp.Service
	Billing *billingapp.Service
}

func (a MCPQuoter) QuoteItem(ctx context.Context, p mcpapp.Principal, in mcpapp.QuoteItem) (mcpdomain.QuoteLine, error) {
	q, err := a.Catalog.Quote(ctx, catalogapp.QuoteRequest{
		ModelCode: in.ModelCode, OperationCode: in.Operation,
		Count: in.Count, DurationSec: in.DurationSec, Text: in.Text,
		MemberLevelID: p.MemberLevelID, RoleCounts: toRoleCount(in.Inputs),
	})
	if err != nil {
		return blockedLine(err)
	}
	return mcpdomain.QuoteLine{Credits: q.Credits, Unit: q.Unit, EstimatedSeconds: q.EstimatedSeconds, Available: true}, nil
}

func (a MCPQuoter) QuotePlanItem(ctx context.Context, p mcpapp.Principal, in mcpapp.PlanQuoteItem) (
	mcpdomain.QuoteLine, error) {
	pi, err := AgentPlanQuoter{Catalog: a.Catalog}.PriceItem(ctx, agentdomain.PlanItem{
		Kind: in.Kind, ModelCode: in.ModelCode, OperationCode: in.OperationCode, Inputs: in.Inputs,
		Quantity: in.Quantity, Unit: in.Unit,
	}, p.MemberLevelID)
	if err != nil {
		return mcpdomain.QuoteLine{}, err
	}
	if !pi.Available {
		return mcpdomain.QuoteLine{BlockedCode: pi.BlockedCode, BlockedReason: pi.BlockedReason,
			UserFixable: pi.UserFixable}, nil
	}
	return mcpdomain.QuoteLine{Credits: pi.Item.Credits, Unit: pi.Unit, Available: true}, nil
}

func (a MCPQuoter) Balance(ctx context.Context, p mcpapp.Principal) (int, error) {
	return a.Billing.Balance(ctx, p.PayerAccountID)
}

func blockedLine(err error) (mcpdomain.QuoteLine, error) {
	b, internal := quoteBlock(err)
	if internal != nil {
		return mcpdomain.QuoteLine{}, internal
	}
	return mcpdomain.QuoteLine{BlockedCode: b.Code, BlockedReason: b.Reason, UserFixable: b.UserFixable}, nil
}

var _ mcpapp.Quoter = MCPQuoter{}
