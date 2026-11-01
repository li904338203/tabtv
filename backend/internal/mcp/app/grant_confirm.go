package app

import (
	"context"

	"github.com/li904338203/tabtv/backend/internal/mcp/repo"
)

// AuditGrantPageConfirm 是切换「花钱需在 seeAitv 页面确认」的平台审计动作（MCPT-13）。
const AuditGrantPageConfirm = "mcp.grant.page_confirm"

// SetRequirePageConfirm 是 PATCH /me/mcp/grants/{grantId}：切换本人一条 active 授权的页面确认开关。
//
// 立即生效：确认服务每次调用都从授权上实时取（Resolver 不缓存），已签发未用的报价也按**调用当时**的开关判 ——
// 打开之后，开关打开之前签发的 quoteId 也必须到审批页批准；关闭之后，回到客户端里确认即可。
// ⚠️ 关闭这一开关要二次确认是前端的事（关掉之后客户端被设成自动批准工具时，花钱不再经过任何人）；
// 这里只保证改的是本人的授权、写审计（含改前改后）。不存在 / 不是本人 → 404；已撤销 → 409。
func (s *Keys) SetRequirePageConfirm(ctx context.Context, userID, grantID int64, on bool) (repo.Grant, error) {
	before, after, err := s.store.SetGrantPageConfirm(ctx, userID, grantID, on)
	if err != nil {
		return repo.Grant{}, mapRepoErr(err, "授权")
	}
	if before.RequirePageConfirm != after.RequirePageConfirm {
		s.write(ctx, userID, after, AuditGrantPageConfirm,
			map[string]any{"grantId": after.ID, "requirePageConfirm": before.RequirePageConfirm},
			map[string]any{"grantId": after.ID, "requirePageConfirm": after.RequirePageConfirm})
	}
	return after, nil
}
