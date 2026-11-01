package app

import (
	"context"
	"errors"

	"golang.org/x/crypto/bcrypt"

	"github.com/li904338203/tabtv/backend/internal/identity/domain"
	"github.com/li904338203/tabtv/backend/internal/identity/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

func (s *Service) ListAdmins(ctx context.Context) ([]repo.AdminSummary, error) {
	a, err := s.repo.ListAdmins(ctx)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return a, nil
}

type CreateAdminInput struct {
	Username string
	Password string
	Roles    []string
}

func (s *Service) CreateAdmin(ctx context.Context, operatorID int64, in CreateAdminInput) (int64, error) {
	if in.Username == "" {
		return 0, httpx.Errf(errcode.InvalidArgument, "请填写用户名")
	}
	if err := domain.ValidateAdminUsername(in.Username); err != nil {
		return 0, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	if err := domain.ValidatePassword(in.Password); err != nil {
		return 0, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	if len(in.Roles) == 0 {
		// 不允许建没有角色的管理员：登录进来什么都做不了，只会让人以为系统坏了。
		return 0, httpx.Errf(errcode.InvalidArgument, "至少分配一个角色")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		return 0, httpx.Wrap(errcode.Internal, err)
	}
	id := s.nextID()
	if err := s.repo.CreateAdmin(ctx, id, in.Username, string(hash), in.Roles); err != nil {
		if errors.Is(err, repo.ErrDuplicate) {
			return 0, httpx.Errf(errcode.DuplicateName, "用户名已存在")
		}
		return 0, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	s.writeAudit(ctx, operatorID, "admin.create", "admin_user", id, nil,
		map[string]any{"username": in.Username, "roles": in.Roles}, "")
	return id, nil
}

func (s *Service) SetAdminStatus(ctx context.Context, operatorID, adminID int64, status, reason string) error {
	if status != "active" && status != "disabled" {
		return httpx.Errf(errcode.InvalidArgument, "status 取值非法")
	}
	// ⚠️ 不能停用自己：停完就再也进不来了，只能改库恢复。
	if operatorID == adminID && status == "disabled" {
		return httpx.Errf(errcode.InvalidArgument, "不能停用自己的账号")
	}
	if status == "disabled" {
		if err := s.guardLastSuperAdmin(ctx, adminID, false); err != nil {
			return err
		}
	}
	before, err := s.repo.FindAdminByID(ctx, adminID)
	if err != nil {
		return httpx.Err(errcode.NotFound)
	}
	if err := s.repo.SetAdminStatus(ctx, adminID, status); err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	s.writeAudit(ctx, operatorID, "admin.set_status", "admin_user", adminID,
		map[string]any{"status": before.Status}, map[string]any{"status": status}, reason)
	return nil
}

func (s *Service) SetAdminRoles(ctx context.Context, operatorID, adminID int64, roles []string) error {
	if len(roles) == 0 {
		return httpx.Errf(errcode.InvalidArgument, "至少分配一个角色")
	}
	keepsSuper := false
	for _, r := range roles {
		keepsSuper = keepsSuper || r == superAdminRole
	}
	if err := s.guardLastSuperAdmin(ctx, adminID, keepsSuper); err != nil {
		return err
	}
	// ⚠️ 不能撤掉自己的权限管理权限：与「不能停用自己」同一类 —— 撤完就再也改不回来了，
	// 而这一下是在后台点个勾就能完成的。
	if operatorID == adminID {
		ok, err := s.repo.RolesGrant(ctx, roles, "rbac:write")
		if err != nil {
			return httpx.Wrap(errcode.Internal, err)
		}
		if !ok {
			return httpx.Errf(errcode.InvalidArgument, "不能撤掉自己的权限管理权限（rbac:write）：撤了就再也改不回来")
		}
	}
	before, err := s.repo.AdminPermissions(ctx, adminID)
	if err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	if err := s.repo.SetAdminRoles(ctx, adminID, s.nextID, roles); err != nil {
		return httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	s.writeAudit(ctx, operatorID, "admin.set_roles", "admin_user", adminID,
		map[string]any{"permissionCount": len(before)}, map[string]any{"roles": roles}, "")
	return nil
}

// superAdminRole 超级管理员角色码（它的权限不可编辑，见 SetRolePermissions）。
const superAdminRole = "super_admin"

// guardLastSuperAdmin 保证系统里**始终至少有一个启用中的超级管理员**。
//
// ⚠️ 撤掉或停用最后一个超级管理员之后，剩下的管理员多半没有全部权限 ——
// 没人能再授予缺的那些权限，也没人能改超级管理员角色，只能直接改库恢复。
// keepsSuper=true 表示这次改动之后他仍持有超级管理员，无需检查。
func (s *Service) guardLastSuperAdmin(ctx context.Context, adminID int64, keepsSuper bool) error {
	if keepsSuper {
		return nil
	}
	has, err := s.repo.AdminHasRole(ctx, adminID, superAdminRole)
	if err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	if !has {
		return nil
	}
	n, err := s.repo.CountActiveAdminsWithRole(ctx, superAdminRole, adminID)
	if err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	if n == 0 {
		return httpx.Errf(errcode.InvalidArgument,
			"这是最后一个启用中的超级管理员：撤掉或停用之后就没有人能管理全部权限了，请先让另一个管理员成为超级管理员")
	}
	return nil
}

func (s *Service) ListRoles(ctx context.Context) ([]repo.RoleDetail, error) {
	r, err := s.repo.ListRoles(ctx)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return r, nil
}

func (s *Service) ListPermissions(ctx context.Context) ([]repo.PermissionDetail, error) {
	p, err := s.repo.ListPermissions(ctx)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return p, nil
}

func (s *Service) SetRolePermissions(ctx context.Context, operatorID int64, roleCode string, perms []string) error {
	// ⚠️ super_admin 的权限不可编辑：改坏了就没人能改回来。
	if roleCode == superAdminRole {
		return httpx.Errf(errcode.PermissionDenied, "超级管理员角色的权限不可编辑")
	}
	if err := s.repo.SetRolePermissions(ctx, roleCode, s.nextID, perms); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return httpx.Err(errcode.NotFound)
		}
		return httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	s.writeAudit(ctx, operatorID, "role.set_permissions", "role", 0, nil,
		map[string]any{"role": roleCode, "permissions": perms}, "")
	return nil
}

func (s *Service) SearchUsers(ctx context.Context, keyword, status string, limit int) ([]repo.UserSummary, error) {
	u, err := s.repo.SearchUsers(ctx, keyword, status, limit)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return u, nil
}

type ProfileInput struct {
	Nickname     string
	WatermarkOff *bool
}

func (s *Service) UpdateProfile(ctx context.Context, userID int64, in ProfileInput) error {
	if in.Nickname != "" {
		if err := domain.ValidateNickname(in.Nickname); err != nil {
			return httpx.Errf(errcode.InvalidArgument, err.Error())
		}
	}
	if err := s.repo.UpdateProfile(ctx, userID, in.Nickname, in.WatermarkOff); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return httpx.Err(errcode.NotFound)
		}
		return httpx.Wrap(errcode.Internal, err)
	}
	return nil
}

func (s *Service) writeAudit(ctx context.Context, operatorID int64, action, objType string, objID int64, before, after any, reason string) {
	if s.audit == nil {
		return
	}
	s.audit.Write(ctx, audit.Entry{
		ActorType: audit.ActorAdmin, ActorID: operatorID,
		Action: action, ObjectType: objType, ObjectID: objID,
		Before: before, After: after, Reason: reason,
	})
}

// AuditLog 暴露审计查询给管理端。
func (s *Service) AuditLog(ctx context.Context, q audit.Query) ([]audit.Record, error) {
	if s.audit == nil {
		return nil, httpx.Errf(errcode.Internal, "审计未启用")
	}
	r, err := s.audit.List(ctx, q)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return r, nil
}

// AuditConfigChange 记录认证配置变更。
// ⚠️ 只记版本号，**不记任何密钥内容** —— 审计表本身也不该成为密钥泄漏面。
func (s *Service) AuditConfigChange(ctx context.Context, operatorID int64, version int) {
	s.writeAudit(ctx, operatorID, "auth_setting.update", "auth_setting", 0, nil,
		map[string]any{"version": version}, "")
}
