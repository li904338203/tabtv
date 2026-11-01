package wiring

import (
	"context"

	collabapp "github.com/li904338203/tabtv/backend/internal/collaboration/app"
	identityapp "github.com/li904338203/tabtv/backend/internal/identity/app"
	mcpapp "github.com/li904338203/tabtv/backend/internal/mcp/app"
)

// 对外 MCP 的端口适配器（MCPT-03，开发 01 §4）。MCP 不 import 其他域的 repo，成员资格、账号状态、
// 会员等级各自只有一份判定，这里只做类型转换。

// MCPMembers 把协作域的成员资格接成 mcp/app.Members。
// 不是成员时协作域返回 NOT_WORKSPACE_MEMBER（403），原样透传。
type MCPMembers struct{ Svc *collabapp.Service }

func (m MCPMembers) Resolve(ctx context.Context, userID, workspaceID int64) (mcpapp.Membership, error) {
	role, payer, err := m.Svc.MembershipWithPayer(ctx, workspaceID, userID)
	if err != nil {
		return mcpapp.Membership{}, err
	}
	return mcpapp.Membership{Role: role, PayerAccountID: payer}, nil
}

// MCPUsers 把身份域的账号状态接成 mcp/app.Users（与用户端 RequireUser 同一个查询）。
type MCPUsers struct{ Svc *identityapp.Service }

func (u MCPUsers) Active(ctx context.Context, userID int64) (bool, error) {
	st, err := u.Svc.UserAuthState(ctx, userID)
	if err != nil {
		return false, err
	}
	return st.Active, nil
}

// 会员等级：billing/app.Service.MemberLevelID 直接满足 mcp/app.MemberLevels，不需要适配器。
