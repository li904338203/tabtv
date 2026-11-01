package app

import (
	"context"
	"errors"

	"github.com/li904338203/tabtv/backend/internal/collaboration/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// MCPDisabled 读工作区级 MCP 开关（MCPT-14）。只做读取：权限判定（manage）在调用方的路由闸，
// 而 MCP 凭据链里的读取发生在成员资格校验之后。
func (s *Service) MCPDisabled(ctx context.Context, workspaceID int64) (bool, error) {
	d, err := s.repo.MCPDisabled(ctx, workspaceID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return false, httpx.Err(errcode.NotFound)
		}
		return false, httpx.Wrap(errcode.Internal, err)
	}
	return d, nil
}

// SetMCPDisabled 写工作区级 MCP 开关，返回写之前的值。审计由调用方（MCP 域）写 ——
// 它知道这次操作的语义（谁、为什么、对外部客户端意味着什么）。
func (s *Service) SetMCPDisabled(ctx context.Context, workspaceID int64, disabled bool) (bool, error) {
	before, err := s.repo.SetMCPDisabled(ctx, workspaceID, disabled)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return false, httpx.Err(errcode.NotFound)
		}
		return false, httpx.Wrap(errcode.Internal, err)
	}
	return before, nil
}
