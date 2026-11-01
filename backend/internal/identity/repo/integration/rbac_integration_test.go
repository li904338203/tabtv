//go:build integration

// 后台权限管理的锁死保护（P15-05 补角色 / 管理员编辑表单时加）。
//
// ⚠️ 已有「不能停用自己」一条；同一类的另外两条此前没拦：
//   - 把**自己**的角色改到不再有 rbac:write —— 撤完就再也改不回来；
//   - 撤掉 / 停用**最后一个**启用中的超级管理员 —— 从此没人能管理全部权限。
//
// 两者的后果都只能直接改库恢复，而在后台都只是点一下的事。用真库：判定要跨四张表。
package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	identityapp "github.com/li904338203/tabtv/backend/internal/identity/app"
	"github.com/li904338203/tabtv/backend/internal/identity/repo"
	"github.com/li904338203/tabtv/backend/internal/identity/token"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
)

// seedRBAC 两个角色：super_admin（含 rbac:write）与 ops（只有 order:read）；三个管理员。
func seedRBAC(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		insert into permission (id, code, name, module) values
		  (1, 'rbac:write', '权限管理', 'rbac'), (2, 'order:read', '看订单', 'order');
		insert into role (id, code, name) values (10, 'super_admin', '超级管理员'), (11, 'ops', '运营');
		insert into role_permission (id, role_id, permission_id) values (100, 10, 1), (101, 10, 2), (102, 11, 2);
		insert into admin_user (id, username, password_hash, status) values
		  (201, 'root', 'x', 'active'), (202, 'root2', 'x', 'active'), (203, 'op', 'x', 'active');
		insert into admin_user_role (id, admin_user_id, role_id) values (301, 201, 10), (302, 202, 10), (303, 203, 11);
	`); err != nil {
		t.Fatal(err)
	}
}

func TestRBAC_不能把系统锁死(t *testing.T) {
	pool := testsupport.Fresh(t)
	seedRBAC(t, pool)
	gen, _ := id.New(0)
	svc := identityapp.New(repo.New(pool), nil, nil, nil, gen.Next)
	ctx := context.Background()

	// ① 自己撤掉自己的权限管理权限：拒
	err := svc.SetAdminRoles(ctx, 201, 201, []string{"ops"})
	if err == nil || !strings.Contains(err.Error(), "自己的权限管理权限") {
		t.Fatalf("撤掉自己的 rbac:write 应被拒，得到 %v", err)
	}

	// ② 还有另一个超管时，撤掉别人的超管可以
	if err := svc.SetAdminRoles(ctx, 201, 202, []string{"ops"}); err != nil {
		t.Fatalf("还有别的超管时应允许：%v", err)
	}
	// ③ 现在 201 是最后一个超管：停用它（由 203 操作）或撤掉它的超管，都拒
	if err := svc.SetAdminStatus(ctx, 203, 201, "disabled", "测试"); err == nil || !strings.Contains(err.Error(), "最后一个") {
		t.Fatalf("停用最后一个超管应被拒，得到 %v", err)
	}
	if err := svc.SetAdminRoles(ctx, 203, 201, []string{"ops"}); err == nil || !strings.Contains(err.Error(), "最后一个") {
		t.Fatalf("撤掉最后一个超管应被拒，得到 %v", err)
	}
	// ④ 停用的超管不算数：把 202 恢复成超管但停用，201 仍是唯一**启用中**的超管
	if err := svc.SetAdminRoles(ctx, 201, 202, []string{"super_admin"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetAdminStatus(ctx, 201, 202, "disabled", "测试"); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetAdminStatus(ctx, 203, 201, "disabled", "测试"); err == nil {
		t.Fatal("另一个超管已停用：201 仍是最后一个启用中的超管，停用它应被拒")
	}
	// ⑤ 与超管无关的改动照常：给普通管理员换角色、停用普通管理员
	if err := svc.SetAdminStatus(ctx, 201, 203, "disabled", "测试"); err != nil {
		t.Fatalf("停用普通管理员应允许：%v", err)
	}
}

// 后台登录要记下最近登录时间：此前只有用户端写 last_login_at，管理员列表的「最近登录」
// 对所有人都是「从未登录」—— 而那正是判断「这个账号还有没有人在用」的依据。
func TestAdminLogin_记下最近登录时间(t *testing.T) {
	pool := testsupport.Fresh(t)
	ctx := context.Background()
	hash, err := bcrypt.GenerateFromPassword([]byte("Secret123"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into admin_user (id, username, password_hash, status)
		values (401, 'loginer', $1, 'active')`, string(hash)); err != nil {
		t.Fatal(err)
	}
	tokens, err := token.New(token.DefaultConfig([]byte("0123456789abcdef0123456789abcdef")))
	if err != nil {
		t.Fatal(err)
	}
	gen, _ := id.New(0)
	svc := identityapp.New(repo.New(pool), tokens, nil, nil, gen.Next)
	if _, err := svc.AdminLogin(ctx, identityapp.AdminLoginInput{Username: "loginer", Password: "Secret123"},
		"127.0.0.1", "test"); err != nil {
		t.Fatal(err)
	}
	var seen bool
	if err := pool.QueryRow(ctx, `select last_login_at is not null from admin_user where id = 401`).Scan(&seen); err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("登录成功后 last_login_at 仍为空")
	}
}
