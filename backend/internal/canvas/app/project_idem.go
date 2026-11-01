package app

import (
	"context"
	"encoding/json"
	"errors"

	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"

	"github.com/li904338203/tabtv/backend/internal/canvas/domain"
	"github.com/li904338203/tabtv/backend/internal/canvas/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// CreateWithIdemKey 按创建幂等键建项目（MCPT-11 的 projects.create，开发 01 §12 R15）：
// 同一工作区同一个键已建过时返回那一个（replayed=true），**不再建第二个**。
//
// 规则与 Create 相同（名称校验、文件夹归属、模板），只多一个幂等键：外部客户端的重试
// （宿主超时、连接中断）是常态，不带键的话每重试一次就多一个空项目，而用户只想要一个。
//
// ⚠️ key 由调用方**加前缀**（如 `mcp:`）且不超过 64 字符（列宽）：与「直接生成」「Agent 建项目」
// 共用 `uk_project_create_idem`，同一个客户端键用在两条路上不能互相命中。这里只校验长度。
// ⚠️ 已建的那个即使已进回收站也原样返回（FindByCreateIdemKey 不过滤 status）：
// 否则同键重放会再建一个新项目。调用方看 Status 决定怎么说。
func (s *Service) CreateWithIdemKey(ctx context.Context, ws, userID int64, role collab.Role,
	in CreateInput, key string) (repo.Project, bool, error) {
	if err := requireRole(role, collab.ActionEdit); err != nil {
		return repo.Project{}, false, err
	}
	if key == "" || len(key) > 64 {
		return repo.Project{}, false, httpx.Errf(errcode.InvalidArgument, "创建幂等键必填且不超过 64 字符")
	}
	if p, err := s.repo.FindByCreateIdemKey(ctx, ws, key); err == nil {
		return p, true, nil
	} else if !errors.Is(err, repo.ErrNotFound) {
		return repo.Project{}, false, httpx.Wrap(errcode.Internal, err)
	}
	name, err := domain.ValidateProjectName(in.Name)
	if err != nil {
		return repo.Project{}, false, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	if err := s.checkFolder(ctx, ws, in.FolderID); err != nil {
		return repo.Project{}, false, err
	}
	canvas := json.RawMessage(`{}`)
	if in.TemplateCode != "" {
		tpl, err := s.repo.GetTemplate(ctx, in.TemplateCode)
		if err != nil {
			return repo.Project{}, false, httpx.Errf(errcode.NotFound, "模板不存在或已停用: "+in.TemplateCode)
		}
		canvas = tpl.CanvasData
	}
	p := repo.Project{ID: s.nextID(), WorkspaceID: ws, CreatedBy: userID, Name: name, FolderID: in.FolderID,
		CanvasData: canvas}
	if err := s.repo.InsertWithIdemKey(ctx, p, key); err != nil {
		if errors.Is(err, repo.ErrDuplicate) {
			// 并发的同键请求都没查到、都来插入：只有一个成功，另一个回那一个。
			existing, e2 := s.repo.FindByCreateIdemKey(ctx, ws, key)
			if e2 != nil {
				return repo.Project{}, false, httpx.Errf(errcode.Conflict, "并发创建冲突，请重试")
			}
			return existing, true, nil
		}
		return repo.Project{}, false, httpx.Wrap(errcode.Internal, err)
	}
	created, err := s.repo.Get(ctx, ws, p.ID)
	return created, false, wrapRepo(err)
}
