package app

import (
	"context"

	"github.com/li904338203/tabtv/backend/internal/canvas/repo"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
)

// DirectorSceneForBuild 取一个要被 AI 搭景的场景（docs/agent/director，DRT-19）：
// 角色要有 edit、项目不在回收站（40905）、场景属于本项目（否则 404）。
//
// 与 GetDirectorScene 的区别是**按写入的口径**校验：搭景的结果要写回场景（网页路径由前端 PUT，
// Agent / MCP 路径由服务端写），回收站里的项目搭出来也存不进去，不如在调模型之前就拒。
func (s *Service) DirectorSceneForBuild(ctx context.Context, ws, projectID, sceneID int64, role collab.Role) (
	repo.DirectorScene, error) {
	if err := requireRole(role, collab.ActionEdit); err != nil {
		return repo.DirectorScene{}, err
	}
	if err := s.projectWritable(ctx, ws, projectID); err != nil {
		return repo.DirectorScene{}, directorProjectErr(err)
	}
	sc, err := s.repo.GetDirectorScene(ctx, ws, projectID, sceneID)
	if err != nil {
		return repo.DirectorScene{}, wrapDirectorRepo(err)
	}
	return sc, nil
}
