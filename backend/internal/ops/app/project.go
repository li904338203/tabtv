package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/ops/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 后台项目运维（AD-08）。

func (s *Service) ListProjects(ctx context.Context, f repo.ProjectFilter) (
	[]repo.ProjectRow, error) {
	list, err := s.repo.ListProjects(ctx, f)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return list, nil
}

// ProjectDetail 是后台项目详情：元数据 + 画布原文。
type ProjectDetail struct {
	Project repo.ProjectRow
	Canvas  json.RawMessage
}

func (s *Service) ProjectDetail(ctx context.Context, id int64) (ProjectDetail, error) {
	p, err := s.repo.GetProject(ctx, id)
	if errors.Is(err, repo.ErrNotFound) {
		return ProjectDetail{}, httpx.Err(errcode.NotFound)
	}
	if err != nil {
		return ProjectDetail{}, httpx.Wrap(errcode.Internal, err)
	}
	canvas, err := s.repo.ProjectCanvas(ctx, id)
	if err != nil {
		return ProjectDetail{}, httpx.Wrap(errcode.Internal, err)
	}
	return ProjectDetail{Project: p, Canvas: canvas}, nil
}

// ProjectAction 是后台能对项目做的三件事。
type ProjectAction string

const (
	ProjectRestore ProjectAction = "restore"
	ProjectDelete  ProjectAction = "delete"
	ProjectRename  ProjectAction = "rename"
)

// ActOnProject 执行一次后台项目操作，并进审计。
//
// ⚠️ **理由必填**：这三件事都直接改变用户看得到的东西，而用户本人不在场。
// 不写理由的话，客服事后只能说"系统里显示被删了"，说不出是谁删的、为什么。
//
// ⚠️ 每件事都看「本次是否真的改了」：静默成功的话客服会告诉用户
// "我已经帮你恢复了"，而用户那边什么都没变。
func (s *Service) ActOnProject(ctx context.Context, adminID, id int64,
	action ProjectAction, reason, newName string) (repo.ProjectRow, error) {
	if strings.TrimSpace(reason) == "" {
		return repo.ProjectRow{}, httpx.Errf(errcode.InvalidArgument,
			"后台操作用户项目必须填写原因")
	}
	before, err := s.repo.GetProject(ctx, id)
	if errors.Is(err, repo.ErrNotFound) {
		return repo.ProjectRow{}, httpx.Err(errcode.NotFound)
	}
	if err != nil {
		return repo.ProjectRow{}, httpx.Wrap(errcode.Internal, err)
	}

	var changed bool
	switch action {
	case ProjectRestore:
		changed, err = s.repo.RestoreProject(ctx, id)
		if err == nil && !changed {
			return repo.ProjectRow{}, httpx.Errf(errcode.InvalidStateTransition,
				"这个项目本来就没有被删除，无需恢复")
		}
	case ProjectDelete:
		changed, err = s.repo.DeleteProject(ctx, id)
		if err == nil && !changed {
			return repo.ProjectRow{}, httpx.Errf(errcode.InvalidStateTransition,
				"这个项目已经在回收站里了")
		}
	case ProjectRename:
		name := strings.TrimSpace(newName)
		if name == "" {
			return repo.ProjectRow{}, httpx.Errf(errcode.InvalidArgument, "新名字不能为空")
		}
		if len([]rune(name)) > 100 {
			return repo.ProjectRow{}, httpx.Errf(errcode.InvalidArgument, "名字最多 100 字")
		}
		changed, err = s.repo.RenameProject(ctx, id, name)
	default:
		return repo.ProjectRow{}, httpx.Errf(errcode.InvalidArgument,
			"只支持恢复 / 删除 / 改名三种操作")
	}
	if err != nil {
		return repo.ProjectRow{}, httpx.Wrap(errcode.Internal, err)
	}

	after, err := s.repo.GetProject(ctx, id)
	if err != nil {
		return repo.ProjectRow{}, httpx.Wrap(errcode.Internal, err)
	}
	s.writeAudit(ctx, audit.Entry{
		ActorType: audit.ActorAdmin, ActorID: adminID,
		// ⚠️ 带上 workspaceId：审计里最常问的是"这个团队出了什么事"，
		// 只记对象 ID 的话要再查一次才知道它属于谁。
		WorkspaceID: before.WorkspaceID,
		Action:      "project." + string(action),
		ObjectType:  "project", ObjectID: id,
		Before: map[string]any{"status": before.Status, "name": before.Name},
		After:  map[string]any{"status": after.Status, "name": after.Name},
		Reason: reason,
	})
	return after, nil
}
