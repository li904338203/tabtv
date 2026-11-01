//go:build integration

// 共享授权与工作区生命周期的真库测试。
//
// ⚠️ 必须用真库：这里要守的几条全部由 **SQL 与索引**决定 ——
// 部分唯一索引下的 upsert、撤销后能否重新授权、权限并集、
// 归档时共享与邀请的连带作废、转让的三步一致性。
// mock 仓储只能测"我写的 Go 有没有按我想的调用"，测不出这些。
package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/collaboration/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
)

func setup(t *testing.T) (*repo.Repo, *pgxpool.Pool, func() int64) {
	t.Helper()
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	return repo.New(pool), pool, gen.Next
}

// mkUser 建一个用户并返回 ID。
func mkUser(t *testing.T, pool *pgxpool.Pool, next func() int64, nickname, phone string) int64 {
	t.Helper()
	id := next()
	_, err := pool.Exec(context.Background(), `
		insert into app_user (id, phone, nickname, password_hash)
		values ($1, $2, $3, 'x')`, id, phone, nickname)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// mkWorkspace 建一个团队工作区（含 payer_account 与 owner 成员）。
func mkWorkspace(t *testing.T, r *repo.Repo, next func() int64, name string, owner int64) repo.Workspace {
	t.Helper()
	w, err := r.CreateTeam(context.Background(),
		[3]int64{next(), next(), next()}, name, owner)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestGrantIsUpsertAndRevokeAllowsRegrant(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	owner := mkUser(t, pool, next, "拥有者", "13000000001")
	guest := mkUser(t, pool, next, "外部合作者", "13000000002")
	src := mkWorkspace(t, r, next, "制作组", owner)

	mk := func(perms string) repo.Share {
		return repo.Share{
			WorkspaceID: src.ID, Resource: domain.ResourceProject, ResourceID: 777,
			GranteeType: domain.GranteeUser, GranteeID: guest,
			Permissions: domain.ParsePermSet(perms), GrantedBy: owner,
		}
	}

	first, err := r.Grant(ctx, next(), mk("view"))
	if err != nil {
		t.Fatal(err)
	}

	// 重复授予是**改权限**，不是 409。用户的意图是"让这个人能看到"，
	// 他不关心之前有没有授过；报冲突只会逼他先撤销再授一次。
	second, err := r.Grant(ctx, next(), mk("view,export"))
	if err != nil {
		t.Fatalf("重复授予应走 upsert：%v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("应更新同一行，first=%d second=%d", first.ID, second.ID)
	}
	if got := second.Permissions.String(); got != "view,export" {
		t.Fatalf("权限应被更新，得到 %q", got)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`select count(*) from resource_share where revoked_at is null`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("有效授权应只有一条，实际 %d", n)
	}

	// 撤销后能重新授权：uk_share_active 的条件是 revoked_at is null。
	if _, err := r.Revoke(ctx, src.ID, first.ID, owner); err != nil {
		t.Fatal(err)
	}
	third, err := r.Grant(ctx, next(), mk("view"))
	if err != nil {
		t.Fatalf("撤销后应能重新授权：%v", err)
	}
	if third.ID == first.ID {
		t.Fatal("重新授权应是新行，旧行要保留作为撤销痕迹")
	}
	if err := pool.QueryRow(ctx,
		`select count(*) from resource_share`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("撤销的行要留痕，实际总行数 %d", n)
	}
	// 撤销者要记下来：共享是一次对外授权决定，撤销同样是。
	var revokedBy int64
	if err := pool.QueryRow(ctx,
		`select coalesce(revoked_by,0) from resource_share where id = $1`, first.ID).
		Scan(&revokedBy); err != nil {
		t.Fatal(err)
	}
	if revokedBy != owner {
		t.Fatalf("revoked_by = %d，应为 %d", revokedBy, owner)
	}
}

// 撤销必须带 workspace_id：少了它，任何工作区的管理员只要猜到
// share id 就能撤销别人的授权。
func TestRevokeIsScopedToWorkspace(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	owner := mkUser(t, pool, next, "拥有者", "13000000011")
	other := mkUser(t, pool, next, "旁人", "13000000012")
	src := mkWorkspace(t, r, next, "制作组", owner)
	foreign := mkWorkspace(t, r, next, "别人的组", other)

	sh, err := r.Grant(ctx, next(), repo.Share{
		WorkspaceID: src.ID, Resource: domain.ResourceProject, ResourceID: 1,
		GranteeType: domain.GranteeUser, GranteeID: other,
		Permissions: domain.ParsePermSet("view"), GrantedBy: owner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Revoke(ctx, foreign.ID, sh.ID, other); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("跨工作区撤销应失败，得到 %v", err)
	}
}

// 一个用户可能同时被两条授权命中（本人一条 + 所在工作区一条），
// 必须取并集 —— 取"第一条"的话权限取决于排序，排序一改权限就变。
func TestEffectivePermissionsUnionsBothPaths(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	owner := mkUser(t, pool, next, "拥有者", "13000000021")
	guest := mkUser(t, pool, next, "合作者", "13000000022")
	src := mkWorkspace(t, r, next, "源工作区", owner)
	guestWS := mkWorkspace(t, r, next, "合作方工作区", guest)

	const resourceID = int64(4242)
	if _, err := r.Grant(ctx, next(), repo.Share{
		WorkspaceID: src.ID, Resource: domain.ResourceScriptAsset, ResourceID: resourceID,
		GranteeType: domain.GranteeUser, GranteeID: guest,
		Permissions: domain.ParsePermSet("view"), GrantedBy: owner,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Grant(ctx, next(), repo.Share{
		WorkspaceID: src.ID, Resource: domain.ResourceScriptAsset, ResourceID: resourceID,
		GranteeType: domain.GranteeWorkspace, GranteeID: guestWS.ID,
		Permissions: domain.ParsePermSet("view,export"), GrantedBy: owner,
	}); err != nil {
		t.Fatal(err)
	}

	perms, err := r.EffectivePermissions(ctx, domain.ResourceScriptAsset, resourceID, guest)
	if err != nil {
		t.Fatal(err)
	}
	if got := perms.String(); got != "view,export" {
		t.Fatalf("应取并集，得到 %q", got)
	}

	// 没被共享到的人拿到的是**空集**，不是错误 ——
	// "没有共享给你"与"查询出错"混成一个的话，一次数据库抖动会被
	// 呈现成"权限被撤销了"。
	stranger := mkUser(t, pool, next, "陌生人", "13000000023")
	none, err := r.EffectivePermissions(ctx, domain.ResourceScriptAsset, resourceID, stranger)
	if err != nil {
		t.Fatalf("查不到不该报错：%v", err)
	}
	if !none.Empty() {
		t.Fatalf("陌生人不该有权限，得到 %q", none.String())
	}
}

// 成员被移除后，凭"所在工作区"拿到的共享必须立刻失效 ——
// 这是 NEW-18「撤销同步作用于 API」的核心：权限实时按成员资格算，
// 不能把工作区列表缓存进会话。
func TestRemovedMemberLosesWorkspaceGrantedShare(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	owner := mkUser(t, pool, next, "拥有者", "13000000031")
	partner := mkUser(t, pool, next, "合作方老板", "13000000032")
	member := mkUser(t, pool, next, "合作方员工", "13000000033")
	src := mkWorkspace(t, r, next, "源工作区", owner)
	partnerWS := mkWorkspace(t, r, next, "合作方", partner)

	// 把员工加进合作方工作区。
	if _, err := pool.Exec(ctx, `
		insert into workspace_member (id, workspace_id, user_id, role)
		values ($1,$2,$3,'editor')`, next(), partnerWS.ID, member); err != nil {
		t.Fatal(err)
	}
	const resourceID = int64(555)
	if _, err := r.Grant(ctx, next(), repo.Share{
		WorkspaceID: src.ID, Resource: domain.ResourceProject, ResourceID: resourceID,
		GranteeType: domain.GranteeWorkspace, GranteeID: partnerWS.ID,
		Permissions: domain.ParsePermSet("view"), GrantedBy: owner,
	}); err != nil {
		t.Fatal(err)
	}

	perms, err := r.EffectivePermissions(ctx, domain.ResourceProject, resourceID, member)
	if err != nil || !perms.Can(domain.ActionView) {
		t.Fatalf("在职员工应能看到：perms=%q err=%v", perms.String(), err)
	}

	if err := r.RemoveMember(ctx, partnerWS.ID, member); err != nil {
		t.Fatal(err)
	}
	perms, err = r.EffectivePermissions(ctx, domain.ResourceProject, resourceID, member)
	if err != nil {
		t.Fatal(err)
	}
	if !perms.Empty() {
		t.Fatalf("离职员工应立刻失去访问，得到 %q", perms.String())
	}
}

// 归档只改 workspace.status 的话，被归档工作区的资源仍然共享得好好的 ——
// owner 以为"整个团队已经关掉了"，而外部合作者还在正常打开那些资产。
func TestArchiveRevokesSharesAndInvitations(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	owner := mkUser(t, pool, next, "拥有者", "13000000041")
	guest := mkUser(t, pool, next, "外部合作者", "13000000042")
	src := mkWorkspace(t, r, next, "要关掉的组", owner)

	const resourceID = int64(888)
	if _, err := r.Grant(ctx, next(), repo.Share{
		WorkspaceID: src.ID, Resource: domain.ResourceProject, ResourceID: resourceID,
		GranteeType: domain.GranteeUser, GranteeID: guest,
		Permissions: domain.ParsePermSet("view"), GrantedBy: owner,
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateInvitation(ctx, next(), repo.Invitation{
		WorkspaceID: src.ID, TargetKind: "phone", TargetValue: "13900000000",
		Role: domain.RoleEditor, ExpiresAt: time.Now().Add(time.Hour),
	}, "hash-archive", owner); err != nil {
		t.Fatal(err)
	}

	res, err := r.Archive(ctx, src.ID, owner)
	if err != nil {
		t.Fatal(err)
	}
	if res.RevokedShares != 1 || res.RevokedInvitation != 1 {
		t.Fatalf("归档应连带作废 1 条共享与 1 条邀请，得到 %+v", res)
	}

	perms, err := r.EffectivePermissions(ctx, domain.ResourceProject, resourceID, guest)
	if err != nil {
		t.Fatal(err)
	}
	if !perms.Empty() {
		t.Fatalf("归档后共享应失效，得到 %q", perms.String())
	}
	// 邀请码也不能再用来加入一个已归档的工作区。
	if _, err := r.AcceptInvitation(ctx, "hash-archive", guest, next()); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("归档后邀请码应失效，得到 %v", err)
	}
	// ⚠️ 成员行保留不动：全部置 removed 的话，恢复时无从还原谁是什么角色。
	var members int
	if err := pool.QueryRow(ctx, `
		select count(*) from workspace_member
		where workspace_id = $1 and status = 'active'`, src.ID).Scan(&members); err != nil {
		t.Fatal(err)
	}
	if members != 1 {
		t.Fatalf("成员行应保留，实际 active 成员 %d", members)
	}
	// 二次归档报 404，不是静默成功。
	if _, err := r.Archive(ctx, src.ID, owner); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("重复归档应报不存在，得到 %v", err)
	}
}

// 三件事必须在同一个事务里：分开做的话，中间崩一次就会留下"两个 owner"
// 或"一个 owner 都没有"的工作区，而后者没有任何人能再转让或归档它。
func TestTransferOwner(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	owner := mkUser(t, pool, next, "原所有者", "13000000051")
	successor := mkUser(t, pool, next, "接手人", "13000000052")
	outsider := mkUser(t, pool, next, "局外人", "13000000053")
	w := mkWorkspace(t, r, next, "制作组", owner)

	// 接手人必须已经是成员。
	if err := r.TransferOwner(ctx, w.ID, owner, outsider); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("转给非成员应失败，得到 %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into workspace_member (id, workspace_id, user_id, role)
		values ($1,$2,$3,'editor')`, next(), w.ID, successor); err != nil {
		t.Fatal(err)
	}
	if err := r.TransferOwner(ctx, w.ID, owner, successor); err != nil {
		t.Fatal(err)
	}

	got, err := r.Membership(ctx, w.ID, successor)
	if err != nil || got != domain.RoleOwner {
		t.Fatalf("接手人应成为 owner，得到 %q err=%v", got, err)
	}
	// 原 owner 降为 admin 而不是移除：他多半还要继续在这个团队里干活。
	got, err = r.Membership(ctx, w.ID, owner)
	if err != nil || got != domain.RoleAdmin {
		t.Fatalf("原所有者应降为 admin，得到 %q err=%v", got, err)
	}
	var ownerCol int64
	if err := pool.QueryRow(ctx,
		`select owner_user_id from workspace where id = $1`, w.ID).Scan(&ownerCol); err != nil {
		t.Fatal(err)
	}
	if ownerCol != successor {
		t.Fatalf("workspace.owner_user_id 应同步，得到 %d", ownerCol)
	}
	// payer_account 不跟着换：转让的是管理权不是账目，
	// 跟着换的话历史消费流水会指向一个新主体。
	var payer int64
	if err := pool.QueryRow(ctx,
		`select payer_account_id from workspace where id = $1`, w.ID).Scan(&payer); err != nil {
		t.Fatal(err)
	}
	if payer != w.PayerAccountID {
		t.Fatalf("计费主体不应改变，%d → %d", w.PayerAccountID, payer)
	}
	// 已经不是 owner 的人再转一次要失败（条件带 owner_user_id）。
	if err := r.TransferOwner(ctx, w.ID, owner, successor); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("非所有者不应能转让，得到 %v", err)
	}
}

// 过期的行若永远停在 pending，同一个人就再也邀请不了第二次 ——
// 而用户看到的错误会是「名称已存在」，指向一个完全无关的方向。
func TestInvitationLifecycle(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	owner := mkUser(t, pool, next, "拥有者", "13000000061")
	w := mkWorkspace(t, r, next, "制作组", owner)
	const target = "13900000099"

	mkInvite := func(hash string, expires time.Time) error {
		return r.CreateInvitation(ctx, next(), repo.Invitation{
			WorkspaceID: w.ID, TargetKind: "phone", TargetValue: target,
			Role: domain.RoleEditor, ExpiresAt: expires,
		}, hash, owner)
	}
	if err := mkInvite("h1", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// 同目标第二条 pending 会撞 uk_invite_pending —— 这正是应用层必须
	// 先显式撤销的原因。
	if err := mkInvite("h2", time.Now().Add(time.Hour)); err == nil {
		t.Fatal("同目标第二条 pending 应撞唯一索引")
	}
	n, err := r.RevokePendingForTarget(ctx, w.ID, "phone", target)
	if err != nil || n != 1 {
		t.Fatalf("应撤销 1 条，得到 n=%d err=%v", n, err)
	}
	if err := mkInvite("h2", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("撤销旧邀请后应能重新邀请：%v", err)
	}

	// 过期清理：把过期的 pending 落实成 expired，让下一次邀请能建得出来。
	if _, err := pool.Exec(ctx,
		`update workspace_invitation set expires_at = now() - interval '1 day'
		 where token_hash = 'h2'`); err != nil {
		t.Fatal(err)
	}
	// 清理之前，过期的行仍占着 pending，新邀请建不出来。
	if err := mkInvite("h3", time.Now().Add(time.Hour)); err == nil {
		t.Fatal("过期但仍是 pending 的行应继续占着唯一索引")
	}
	expired, err := r.ExpireInvitations(ctx, 100)
	if err != nil || expired != 1 {
		t.Fatalf("应标记 1 条过期，得到 %d err=%v", expired, err)
	}
	if err := mkInvite("h3", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("过期清理后应能重新邀请：%v", err)
	}

	// 「我收到的邀请」按联系方式反查，且只回未过期的 pending。
	list, err := r.ListInvitationsFor(ctx, "", target)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].WorkspaceName != "制作组" || list[0].Role != domain.RoleEditor {
		t.Fatalf("收件箱应有 1 条有效邀请，得到 %+v", list)
	}
	if list[0].InvitedBy != "拥有者" {
		t.Fatalf("应带上邀请人昵称，得到 %q", list[0].InvitedBy)
	}
	// 空联系方式不得匹配任何行：`$1 <> ''` 这个条件漏掉的话，
	// 没填邮箱的用户会收到**所有**按邮箱发出的邀请。
	none, err := r.ListInvitationsFor(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("空联系方式不应匹配任何邀请，得到 %d 条", len(none))
	}
}

// 邀请一个已经在里面的人会在他接受时**悄悄改掉他的角色**
// （AcceptInvitation 的 on conflict 分支）—— 于是"邀请同事"
// 这个动作把一个 admin 降成了 viewer，而操作者完全没有这个意思。
func TestMemberExistsByContact(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	owner := mkUser(t, pool, next, "拥有者", "13000000071")
	w := mkWorkspace(t, r, next, "制作组", owner)

	ok, err := r.MemberExistsByContact(ctx, w.ID, "phone", "13000000071")
	if err != nil || !ok {
		t.Fatalf("owner 本人应被识别为已是成员：ok=%v err=%v", ok, err)
	}
	ok, err = r.MemberExistsByContact(ctx, w.ID, "phone", "13900001111")
	if err != nil || ok {
		t.Fatalf("陌生号码不应被识别为成员：ok=%v err=%v", ok, err)
	}
	// 邮箱走另一列，别把两种联系方式混成一个查询。
	ok, err = r.MemberExistsByContact(ctx, w.ID, "email", "13000000071")
	if err != nil || ok {
		t.Fatalf("手机号不该命中邮箱列：ok=%v err=%v", ok, err)
	}
}
