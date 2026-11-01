package app

import (
	"context"
	"errors"

	"github.com/li904338203/tabtv/backend/internal/script/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// AssetInWorkspace 按工作区取一个资产（画布 Agent 解析消息里的资产引用，AGT-26）。
//
// ⚠️ 带工作区条件：引用来自客户端，只认本工作区的资产（GO-27）；
// 不存在与跨工作区回同一个 404，不泄漏存在性。
func (s *Service) AssetInWorkspace(ctx context.Context, workspaceID, id int64) (repo.Asset, error) {
	a, err := s.repo.GetAsset(ctx, workspaceID, id)
	if errors.Is(err, repo.ErrNotFound) {
		return repo.Asset{}, httpx.Errf(errcode.NotFound, "资产不存在")
	}
	if err != nil {
		return repo.Asset{}, httpx.Wrap(errcode.Internal, err)
	}
	return a, nil
}
