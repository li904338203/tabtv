// Package domain 定义协作域的角色与权限规则。
//
// 租户边界（数据模型 §1.3）：一切业务资源挂 workspace_id，
// created_by_user_id 只记"谁干的"，不表示归属。
package domain

import (
	"errors"
	"strings"
)

type Role string

const (
	RoleOwner  Role = "owner"
	RoleAdmin  Role = "admin"
	RoleEditor Role = "editor"
	RoleViewer Role = "viewer"
)

// Action 是工作区内的操作类别（NEW-18 的最小集）。
type Action string

const (
	ActionView     Action = "view"
	ActionEdit     Action = "edit"
	ActionGenerate Action = "generate"
	ActionExport   Action = "export"
	ActionManage   Action = "manage"
	// ActionTransfer 转让/删除工作区，**只有 owner 能做**。
	// admin 虽然有 manage，但不该能把 owner 踢掉或删掉整个工作区。
	ActionTransfer Action = "transfer"
)

// matrix 是角色 → 允许操作的唯一定义（数据模型 §4）。
//
// ⚠️ 这张表是权限判定的**唯一来源**。任何地方都不得直接比较角色字符串，
// 一律走 Can() —— 散落的 `role == "admin"` 判断迟早会漏掉一处。
var matrix = map[Role]map[Action]bool{
	RoleOwner: {
		ActionView: true, ActionEdit: true, ActionGenerate: true,
		ActionExport: true, ActionManage: true, ActionTransfer: true,
	},
	RoleAdmin: {
		ActionView: true, ActionEdit: true, ActionGenerate: true,
		ActionExport: true, ActionManage: true,
		// 不含 Transfer：admin 不能转让或删除工作区。
	},
	RoleEditor: {
		ActionView: true, ActionEdit: true, ActionGenerate: true, ActionExport: true,
	},
	RoleViewer: {
		ActionView: true,
	},
}

func (r Role) Valid() bool {
	_, ok := matrix[r]
	return ok
}

// Can 判定角色能否执行某操作。未知角色一律拒绝（fail closed）。
func (r Role) Can(a Action) bool {
	return matrix[r][a]
}

// Rank 用于比较角色高低：不能授予或修改比自己高的角色。
func (r Role) Rank() int {
	switch r {
	case RoleOwner:
		return 4
	case RoleAdmin:
		return 3
	case RoleEditor:
		return 2
	case RoleViewer:
		return 1
	default:
		return 0
	}
}

type Kind string

const (
	KindPersonal Kind = "personal"
	KindTeam     Kind = "team"
)

var (
	ErrInvalidRole       = errors.New("角色取值非法")
	ErrNameLength        = errors.New("工作区名称需在 1~64 字之间")
	ErrPersonalLocked    = errors.New("个人工作区不支持该操作")
	ErrCannotRemoveOwner = errors.New("不能移除工作区所有者")
	ErrRoleTooHigh       = errors.New("不能授予高于自身的角色")
	// ErrNoManage 与 ErrInvalidRole 必须分开：前者是权限问题（403），
	// 后者是参数问题（400）。混成一个会让 editor 收到"角色取值非法"，
	// 以为自己参数写错了，而真实原因是没有管理权限。
	ErrNoManage        = errors.New("没有成员管理权限")
	ErrNotOwner        = errors.New("只有工作区所有者能执行该操作")
	ErrTransferToSelf  = errors.New("不能把工作区转让给自己")
	ErrTargetNotMember = errors.New("接手人必须是该工作区的成员")
	ErrAlreadyMember   = errors.New("该账号已经是工作区成员")
)

// CanTransfer 判定能否转让或归档工作区。
//
// ⚠️ 要的是 ActionTransfer 而不是 ActionManage：admin 有 manage，
// 但他不该能把 owner 顶掉，也不该能把整个工作区连同别人的项目一起归档。
// 这两件事都是不可逆的（归档可恢复，但转让之后 owner 就换人了），
// 只能由唯一的归属人自己决定。
//
// 个人工作区一律拒绝：它随账号注册自动创建、与账号一一对应
// （uk_workspace_personal），转让或归档之后这个账号就再也没有落脚点了。
func CanTransfer(actor Role, kind Kind) error {
	if !actor.Can(ActionTransfer) {
		return ErrNotOwner
	}
	if kind == KindPersonal {
		return ErrPersonalLocked
	}
	return nil
}

func ValidateName(s string) error {
	n := len([]rune(strings.TrimSpace(s)))
	if n < 1 || n > 64 {
		return ErrNameLength
	}
	return nil
}

// CanAssign 判定操作者能否把目标设为某角色。
//
// 两条规则：
//  1. 必须有 manage 权限；
//  2. **不能授予高于自身的角色** —— 否则 admin 可以把别人提成 owner，
//     再让对方把自己踢掉，等于绕过了 owner 的独占权。
func CanAssign(actor, target Role) error {
	if !actor.Can(ActionManage) {
		return ErrNoManage
	}
	if !target.Valid() {
		return ErrInvalidRole
	}
	if target.Rank() >= actor.Rank() && actor != RoleOwner {
		return ErrRoleTooHigh
	}
	// owner 角色只能通过转让产生，不能直接授予。
	if target == RoleOwner {
		return ErrRoleTooHigh
	}
	return nil
}
