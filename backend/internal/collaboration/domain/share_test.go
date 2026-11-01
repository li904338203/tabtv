package domain

import (
	"errors"
	"testing"
)

func TestNormalizePermissions(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
		err  error
	}{
		{"单项 view", []string{"view"}, "view", nil},
		// 没有 view 的 edit 是自相矛盾的：受让方会在列表里看到这条共享却打不开。
		{"edit 自动补 view", []string{"edit"}, "view,edit", nil},
		{"全量按固定顺序", []string{"export", "generate", "edit", "view"},
			"view,edit,generate,export", nil},
		{"重复项归一", []string{"view", "view"}, "view", nil},
		{"空列表", nil, "", ErrNoPermissions},
		// manage 不能共享：授得出去的话受让方能反过来改成员角色。
		{"manage 不可共享", []string{"view", "manage"}, "", ErrNotShareable},
		{"transfer 不可共享", []string{"transfer"}, "", ErrNotShareable},
		{"未知动作", []string{"delete"}, "", ErrNotShareable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := NormalizePermissions(c.in)
			if !errors.Is(err, c.err) {
				t.Fatalf("err = %v, want %v", err, c.err)
			}
			if err != nil {
				return
			}
			if got.String() != c.want {
				t.Fatalf("got %q, want %q", got.String(), c.want)
			}
		})
	}
}

// 序列化顺序必须稳定：map 遍历顺序每次都不同，入库的字符串会在
// "view,edit" 与 "edit,view" 之间随机跳，审计 diff 里全是噪声。
func TestPermSetStringIsStable(t *testing.T) {
	p, err := NormalizePermissions([]string{"export", "edit", "view", "generate"})
	if err != nil {
		t.Fatal(err)
	}
	first := p.String()
	for i := 0; i < 50; i++ {
		if got := p.String(); got != first {
			t.Fatalf("第 %d 次得到 %q，首次是 %q", i, got, first)
		}
	}
	if first != "view,edit,generate,export" {
		t.Fatalf("顺序应跟随 shareable，得到 %q", first)
	}
}

func TestParsePermSet(t *testing.T) {
	p := ParsePermSet("view,edit")
	if !p.Can(ActionView) || !p.Can(ActionEdit) {
		t.Fatal("应解析出 view 与 edit")
	}
	if p.Can(ActionExport) {
		t.Fatal("不应凭空多出 export")
	}
	// 库里可能有历史脏值；一条授权的权限字段坏了不该让整个共享列表打不开。
	dirty := ParsePermSet("view, manage ,, 乱码,edit")
	if got := dirty.String(); got != "view,edit" {
		t.Fatalf("脏值应被丢弃，得到 %q", got)
	}
	if empty := ParsePermSet(""); !empty.Empty() {
		t.Fatal("空串应解析为空集")
	}
}

// 一个用户可能同时被两条有效授权命中（本人一条 + 所在工作区一条），
// 必须取并集 —— 取"第一条"的话权限取决于 SQL 排序，排序一改权限就变。
func TestPermSetUnion(t *testing.T) {
	a := ParsePermSet("view")
	b := ParsePermSet("view,export")
	u := a.Union(b)
	if u.String() != "view,export" {
		t.Fatalf("并集 = %q", u.String())
	}
	// 并集不得改动任一原集合。
	if a.String() != "view" || b.String() != "view,export" {
		t.Fatal("Union 不应就地修改入参")
	}
}

func TestCheckGrant(t *testing.T) {
	base := GrantAsk{
		OwnerWorkspaceID: 100, Resource: ResourceProject, ResourceID: 7,
		GranteeType: GranteeWorkspace, GranteeID: 200,
		Permissions: ParsePermSet("view"),
	}

	if err := CheckGrant(RoleOwner, base); err != nil {
		t.Fatalf("owner 应可共享：%v", err)
	}
	if err := CheckGrant(RoleAdmin, base); err != nil {
		t.Fatalf("admin 应可共享：%v", err)
	}
	// 共享是一次对外发布决定，不是日常编辑动作。
	if err := CheckGrant(RoleEditor, base); !errors.Is(err, ErrNoShareRight) {
		t.Fatalf("editor 不应能共享，得到 %v", err)
	}
	if err := CheckGrant(RoleViewer, base); !errors.Is(err, ErrNoShareRight) {
		t.Fatalf("viewer 不应能共享，得到 %v", err)
	}

	self := base
	self.GranteeID = self.OwnerWorkspaceID
	if err := CheckGrant(RoleOwner, self); !errors.Is(err, ErrSelfShare) {
		t.Fatalf("共享给自己应被拒，得到 %v", err)
	}
	// 共享给自己**本人**（user 型）不在这条规则里：那是"把资源给某个人"，
	// 即便他恰好也是本工作区成员也不构成循环。
	selfUser := base
	selfUser.GranteeType = GranteeUser
	selfUser.GranteeID = base.OwnerWorkspaceID
	if err := CheckGrant(RoleOwner, selfUser); err != nil {
		t.Fatalf("user 型不应触发 ErrSelfShare：%v", err)
	}

	bad := base
	bad.Resource = "folder"
	if err := CheckGrant(RoleOwner, bad); !errors.Is(err, ErrInvalidResource) {
		t.Fatalf("未知资源类别应被拒，得到 %v", err)
	}
	bad = base
	bad.GranteeType = "role"
	if err := CheckGrant(RoleOwner, bad); !errors.Is(err, ErrInvalidGrantee) {
		t.Fatalf("未知受让方类别应被拒，得到 %v", err)
	}
	bad = base
	bad.Permissions = PermSet{}
	if err := CheckGrant(RoleOwner, bad); !errors.Is(err, ErrNoPermissions) {
		t.Fatalf("空权限应被拒，得到 %v", err)
	}
}

func TestImportable(t *testing.T) {
	if !Importable(ResourceScriptAsset) {
		t.Fatal("资产应可导入")
	}
	// 项目的 canvasData 里存着一串产出 ID，整份搬走后引用全部指向别人的资源 ——
	// 画布能打开但每个节点的产出都是裂的，而这不报错。
	if Importable(ResourceProject) || Importable(ResourceTimeline) {
		t.Fatal("项目与时间线目前不支持导入")
	}
}

func TestCanTransfer(t *testing.T) {
	if err := CanTransfer(RoleOwner, KindTeam); err != nil {
		t.Fatalf("owner 应可转让：%v", err)
	}
	// admin 有 manage，但不该能把 owner 顶掉或归档整个工作区。
	if err := CanTransfer(RoleAdmin, KindTeam); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("admin 不应能转让，得到 %v", err)
	}
	// 个人工作区与账号一一对应，转让/归档之后这个账号就没有落脚点了。
	if err := CanTransfer(RoleOwner, KindPersonal); !errors.Is(err, ErrPersonalLocked) {
		t.Fatalf("个人工作区应被拒，得到 %v", err)
	}
}
