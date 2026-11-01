package domain

import (
	"errors"
	"testing"
)

// 角色矩阵是权限判定的唯一来源，逐格验证。
func TestRole_权限矩阵(t *testing.T) {
	cases := []struct {
		role Role
		act  Action
		want bool
	}{
		{RoleOwner, ActionTransfer, true},
		// ⚠️ admin 有 manage 但**没有** transfer：
		// 否则 admin 能把 owner 踢掉或删掉整个工作区。
		{RoleAdmin, ActionManage, true},
		{RoleAdmin, ActionTransfer, false},
		{RoleEditor, ActionGenerate, true},
		{RoleEditor, ActionManage, false},
		{RoleViewer, ActionView, true},
		{RoleViewer, ActionEdit, false},
		{RoleViewer, ActionGenerate, false},
		{RoleViewer, ActionExport, false},
		// 未知角色一律拒绝（fail closed）。
		{Role("hacker"), ActionView, false},
		{Role(""), ActionView, false},
	}
	for _, c := range cases {
		if got := c.role.Can(c.act); got != c.want {
			t.Errorf("%s.Can(%s) = %v，期望 %v", c.role, c.act, got, c.want)
		}
	}
}

// 不能授予高于自身的角色：否则 admin 可以把别人提成 owner，
// 再让对方把自己踢掉，绕过 owner 的独占权。
func TestCanAssign_不得提权(t *testing.T) {
	if err := CanAssign(RoleAdmin, RoleAdmin); err == nil {
		t.Error("admin 不应能授予同级 admin")
	}
	if err := CanAssign(RoleAdmin, RoleOwner); err == nil {
		t.Error("admin 不应能授予 owner")
	}
	if err := CanAssign(RoleOwner, RoleOwner); err == nil {
		t.Error("owner 角色只能通过转让产生，不能直接授予")
	}
	if err := CanAssign(RoleAdmin, RoleEditor); err != nil {
		t.Errorf("admin 应能授予 editor: %v", err)
	}
	if err := CanAssign(RoleOwner, RoleAdmin); err != nil {
		t.Errorf("owner 应能授予 admin: %v", err)
	}
	// ⚠️ 无 manage 权限要报 ErrNoManage（→403），不是 ErrInvalidRole（→400）：
	// 混成一个会让 editor 收到"角色取值非法"，以为是自己参数写错了。
	if err := CanAssign(RoleEditor, RoleViewer); !errors.Is(err, ErrNoManage) {
		t.Errorf("editor 无 manage 权限应报 ErrNoManage，实际 %v", err)
	}
	if err := CanAssign(RoleViewer, RoleViewer); !errors.Is(err, ErrNoManage) {
		t.Errorf("viewer 应报 ErrNoManage，实际 %v", err)
	}
}

func TestValidateName(t *testing.T) {
	if err := ValidateName(""); err == nil {
		t.Error("空名称应被拒绝")
	}
	if err := ValidateName("   "); err == nil {
		t.Error("纯空白名称应被拒绝")
	}
	if err := ValidateName("我的工作区"); err != nil {
		t.Errorf("正常名称不应被拒绝: %v", err)
	}
	long := make([]rune, 65)
	for i := range long {
		long[i] = 'a'
	}
	if err := ValidateName(string(long)); err == nil {
		t.Error("超长名称应被拒绝")
	}
}
