package repo

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// MCPDisabled 读工作区级 MCP 开关（workspace.mcp_disabled，MCPT-14）。
// 工作区不存在或已归档回 ErrNotFound —— 调用方（MCP 的凭据链）在那之前已经因成员资格失败了。
//
// ⚠️ 列在 workspace 表上、却由 MCP 使用：放在协作域是因为「表归谁，查询就归谁」，
// MCP 只经端口调用（dev/01 §4），不直接读 workspace 表。
func (r *Repo) MCPDisabled(ctx context.Context, workspaceID int64) (bool, error) {
	var disabled bool
	err := r.pool.QueryRow(ctx,
		`select mcp_disabled from workspace where id = $1 and status = 'active'`, workspaceID).Scan(&disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	return disabled, err
}

// SetMCPDisabled 写工作区级 MCP 开关，返回写之前的值（审计的 before）。
func (r *Repo) SetMCPDisabled(ctx context.Context, workspaceID int64, disabled bool) (bool, error) {
	var before bool
	err := r.pool.QueryRow(ctx, `
		with old as (select mcp_disabled from workspace where id = $1 and status = 'active' for update)
		update workspace w set mcp_disabled = $2, updated_at = now()
		from old where w.id = $1
		returning old.mcp_disabled`, workspaceID, disabled).Scan(&before)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	return before, err
}
