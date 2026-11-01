package wiring

import (
	"context"

	billingapp "github.com/li904338203/tabtv/backend/internal/billing/app"
	collabapp "github.com/li904338203/tabtv/backend/internal/collaboration/app"
	mcpapp "github.com/li904338203/tabtv/backend/internal/mcp/app"
)

// 页面审批（MCPT-13）的端口适配器：只做类型转换。

// MCPApprovalWorkspaces 取工作区名称与种类（审批页写清「消耗的是哪个工作区、个人还是团队的积分」）。
type MCPApprovalWorkspaces struct{ Svc *collabapp.Service }

func (w MCPApprovalWorkspaces) Describe(ctx context.Context, workspaceID int64) (mcpapp.WorkspaceInfo, error) {
	ws, err := w.Svc.Describe(ctx, workspaceID)
	if err != nil {
		return mcpapp.WorkspaceInfo{}, err
	}
	return mcpapp.WorkspaceInfo{Name: ws.Name, Kind: string(ws.Kind)}, nil
}

// MCPPayerBalances 读付费主体的当前可用积分（与确认服务、whoami 同一个余额口径）。
type MCPPayerBalances struct{ Svc *billingapp.Service }

func (b MCPPayerBalances) BalanceOf(ctx context.Context, payerAccountID int64) (int, error) {
	return b.Svc.Balance(ctx, payerAccountID)
}
