//go:build integration

// 在线状态的真 Redis + 真库测试（P12-02、NEW-19）。
//
// ⚠️ 必须用真 Redis：这里要守的几条全由 **Redis 的行为**决定 ——
// hash + TTL 的续期语义、HGETALL 的返回形状、TTL 到期后键整体消失。
// 用假实现测的只是"我写的 Go 有没有按我想的调用"。
package integration

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	collabapp "github.com/li904338203/tabtv/backend/internal/collaboration/app"
	"github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/collaboration/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/cache"
	"github.com/li904338203/tabtv/backend/internal/platform/config"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
)

// presenceSvc 装一套「真库 + 真 Redis」的协作域服务。
func presenceSvc(t *testing.T) (*collabapp.Service, *repo.Repo, *pgxpool.Pool, func() int64) {
	t.Helper()
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	r := repo.New(pool)
	svc := collabapp.New(r, gen.Next)

	c, err := cache.Open(context.Background(), config.Redis{
		Addr: testsupport.RedisAddr(t),
		// ⚠️ 命名空间带上测试名：多个用例共用一个 Redis 实例，
		// 不隔离的话前一个用例留下的在线记录会让后一个凭空多出一个人。
		Namespace: "it-" + t.Name(),
	})
	if err != nil {
		t.Fatalf("连接测试 Redis 失败: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	svc.SetPresence(presenceAdapter{c})
	return svc, r, pool, gen.Next
}

type presenceAdapter struct{ c *cache.Client }

func (p presenceAdapter) Touch(ctx context.Context, key, field, value string,
	ttl time.Duration) error {
	return p.c.PresenceTouch(ctx, key, field, value, ttl)
}
func (p presenceAdapter) Entries(ctx context.Context, key string) (map[string]string, error) {
	return p.c.PresenceEntries(ctx, key)
}
func (p presenceAdapter) Drop(ctx context.Context, key, field string) error {
	return p.c.PresenceDrop(ctx, key, field)
}

func TestPresenceHeartbeatMergesTabsAndDropsOnLeave(t *testing.T) {
	svc, r, pool, next := presenceSvc(t)
	ctx := context.Background()
	owner := mkUser(t, pool, next, "组长", "13100000001")
	mate := mkUser(t, pool, next, "组员", "13100000002")
	w := mkWorkspace(t, r, next, "协作组", owner)
	if _, err := pool.Exec(ctx, `
		insert into workspace_member (id, workspace_id, user_id, role)
		values ($1,$2,$3,'editor')`, next(), w.ID, mate); err != nil {
		t.Fatal(err)
	}

	beat := func(user int64, client string) []domain.OnlineUser {
		t.Helper()
		got, err := svc.Heartbeat(ctx, collabapp.PresenceInput{
			WorkspaceID: w.ID, ResourceType: "project", ResourceID: 4242,
			UserID: user, ClientID: client,
		})
		if err != nil {
			t.Fatalf("心跳失败: %v", err)
		}
		return got
	}

	// 同一个人开两个标签页：合并成一条，但端数是 2。
	// 不合并的话成员条上会出现两个"组长"，用户会以为团队里真有两个人在改。
	beat(owner, "tab-1")
	got := beat(owner, "tab-2")
	if len(got) != 1 || got[0].Clients != 2 {
		t.Fatalf("同一人两个端应合并成 1 条 2 端，得到 %+v", got)
	}
	// 昵称由**服务端**查（不采信请求体）—— 上面的心跳一个字都没带昵称。
	if got[0].Nickname != "组长" {
		t.Fatalf("昵称应由服务端补上，得到 %q", got[0].Nickname)
	}

	// 第二个人进来：自己排最前。
	got = beat(mate, "tab-3")
	if len(got) != 2 {
		t.Fatalf("应有 2 个人，得到 %+v", got)
	}
	if !got[0].IsSelf || got[0].UserID != mate {
		t.Fatalf("自己应排最前，得到 %+v", got[0])
	}

	// 主动离开只掉一个端，人还在。
	svc.Leave(ctx, collabapp.PresenceInput{
		WorkspaceID: w.ID, ResourceType: "project", ResourceID: 4242,
		UserID: owner, ClientID: "tab-1",
	})
	got, err := svc.Online(ctx, w.ID, "project", 4242, mate)
	if err != nil {
		t.Fatal(err)
	}
	var ownerRow *domain.OnlineUser
	for i := range got {
		if got[i].UserID == owner {
			ownerRow = &got[i]
		}
	}
	if ownerRow == nil || ownerRow.Clients != 1 {
		t.Fatalf("关掉一个标签页后应还剩 1 端，得到 %+v", got)
	}
}

// 成员被移出工作区之后，他在 Redis 里的记录还在 ——
// 显示一个已经进不来的人在线，比不显示更糟：别人会以为他正在改。
func TestPresenceFiltersRemovedMember(t *testing.T) {
	svc, r, pool, next := presenceSvc(t)
	ctx := context.Background()
	owner := mkUser(t, pool, next, "组长", "13100000011")
	mate := mkUser(t, pool, next, "离职的", "13100000012")
	w := mkWorkspace(t, r, next, "协作组", owner)
	if _, err := pool.Exec(ctx, `
		insert into workspace_member (id, workspace_id, user_id, role)
		values ($1,$2,$3,'editor')`, next(), w.ID, mate); err != nil {
		t.Fatal(err)
	}
	in := collabapp.PresenceInput{WorkspaceID: w.ID, ResourceType: "project",
		ResourceID: 777, UserID: mate, ClientID: "tab-x"}
	if _, err := svc.Heartbeat(ctx, in); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.Online(ctx, w.ID, "project", 777, owner); len(got) != 1 {
		t.Fatalf("移除前应看得到，得到 %+v", got)
	}

	if err := r.RemoveMember(ctx, w.ID, mate); err != nil {
		t.Fatal(err)
	}
	// Redis 里那条记录一个字没动，但他已经不该出现在列表里。
	got, err := svc.Online(ctx, w.ID, "project", 777, owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("被移除的成员不该显示在线，得到 %+v", got)
	}
}

// presence 的键必须带 workspaceID：两个工作区看不到对方的在线列表。
func TestPresenceIsScopedToWorkspace(t *testing.T) {
	svc, r, pool, next := presenceSvc(t)
	ctx := context.Background()
	a := mkUser(t, pool, next, "甲", "13100000021")
	b := mkUser(t, pool, next, "乙", "13100000022")
	wa := mkWorkspace(t, r, next, "甲的组", a)
	wb := mkWorkspace(t, r, next, "乙的组", b)

	if _, err := svc.Heartbeat(ctx, collabapp.PresenceInput{
		WorkspaceID: wa.ID, ResourceType: "project", ResourceID: 9,
		UserID: a, ClientID: "t1",
	}); err != nil {
		t.Fatal(err)
	}
	// 同一个 resourceID，另一个工作区什么都看不到。
	got, err := svc.Online(ctx, wb.ID, "project", 9, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("跨工作区不该看到在线状态，得到 %+v", got)
	}
}

// Redis 不可用时 presence 只能让提示消失，**不能让画布打不开**（GO-11）。
func TestPresenceDegradesWhenStoreFails(t *testing.T) {
	svc, r, pool, next := presenceSvc(t)
	ctx := context.Background()
	owner := mkUser(t, pool, next, "组长", "13100000031")
	w := mkWorkspace(t, r, next, "协作组", owner)

	svc.SetPresence(brokenStore{})
	got, err := svc.Heartbeat(ctx, collabapp.PresenceInput{
		WorkspaceID: w.ID, ResourceType: "project", ResourceID: 1,
		UserID: owner, ClientID: "t1",
	})
	if err != nil {
		t.Fatalf("Redis 故障不该让心跳报错：%v", err)
	}
	// 退化成"只有我自己"，而不是一个错误弹窗。
	if len(got) != 1 || !got[0].IsSelf {
		t.Fatalf("应退化成只有自己，得到 %+v", got)
	}
	if list, err := svc.Online(ctx, w.ID, "project", 1, owner); err != nil || len(list) != 0 {
		t.Fatalf("只读列表应退化成空，得到 %+v err=%v", list, err)
	}
}

type brokenStore struct{}

func (brokenStore) Touch(context.Context, string, string, string, time.Duration) error {
	return redis.ErrClosed
}
func (brokenStore) Entries(context.Context, string) (map[string]string, error) {
	return nil, redis.ErrClosed
}
func (brokenStore) Drop(context.Context, string, string) error { return redis.ErrClosed }
