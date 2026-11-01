package wiring

import (
	"context"

	collabapp "github.com/li904338203/tabtv/backend/internal/collaboration/app"
)

// MCPWorkspaces 把协作域接成 mcp/app.Workspaces（MCPT-14）：工作区级 MCP 开关与成员昵称。
// 开关列在 workspace 表上，读写都在协作域，MCP 只经这个端口调用。
type MCPWorkspaces struct{ Svc *collabapp.Service }

func (w MCPWorkspaces) MCPDisabled(ctx context.Context, workspaceID int64) (bool, error) {
	return w.Svc.MCPDisabled(ctx, workspaceID)
}

func (w MCPWorkspaces) SetMCPDisabled(ctx context.Context, workspaceID int64, disabled bool) (bool, error) {
	return w.Svc.SetMCPDisabled(ctx, workspaceID, disabled)
}

func (w MCPWorkspaces) MemberNames(ctx context.Context, workspaceID int64) (map[int64]string, error) {
	ms, err := w.Svc.Members(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]string, len(ms))
	for _, m := range ms {
		out[m.UserID] = m.Nickname
	}
	return out, nil
}
