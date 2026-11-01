//go:build integration

// 审计的真库测试。
//
// ⚠️ 必须用真库：审计写失败只告警不阻断主流程（这是有意的设计），
// 因此**单测无法发现写不进去**——操作全部成功、审计表却是空的。
// 这个 bug 真实发生过：`nullif($3,0)` 让 PG 把参数推断成 int4，雪花 ID 溢出。
package integration

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
)

func setup(t *testing.T) (*audit.Logger, *pgxpool.Pool) {
	t.Helper()
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	return audit.New(pool, gen.Next, slog.New(slog.NewTextHandler(io.Discard, nil))), pool
}

// 雪花 ID 是 int8 量级，必须能正常写入。
func TestIntegration_雪花ID不溢出(t *testing.T) {
	lg, pool := setup(t)
	ctx := context.Background()

	const bigID = int64(95011712519897088) // 远超 int4 上限
	lg.Write(ctx, audit.Entry{
		ActorType: audit.ActorAdmin, ActorID: bigID,
		WorkspaceID: bigID + 1,
		Action:      "user.set_status", ObjectType: "app_user", ObjectID: bigID + 2,
		Before: map[string]any{"status": "active"},
		After:  map[string]any{"status": "banned"},
		Reason: "违规内容",
	})

	var n int
	if err := pool.QueryRow(ctx, `select count(*) from audit_log`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("审计应写入 1 行，实际 %d 行 —— 写入被静默丢弃了", n)
	}

	var actorID, objectID int64
	var before, after []byte
	var reason string
	if err := pool.QueryRow(ctx, `
		select actor_id, object_id, before_value, after_value, reason from audit_log`).
		Scan(&actorID, &objectID, &before, &after, &reason); err != nil {
		t.Fatal(err)
	}
	if actorID != bigID || objectID != bigID+2 {
		t.Errorf("ID 未正确保存: actor=%d object=%d", actorID, objectID)
	}
	// 前后值是审计的核心：没有它就只知道"改过"，不知道"改成了什么"。
	var b, a map[string]any
	_ = json.Unmarshal(before, &b)
	_ = json.Unmarshal(after, &a)
	if b["status"] != "active" || a["status"] != "banned" {
		t.Errorf("前后值不正确: %v → %v", b, a)
	}
	if reason != "违规内容" {
		t.Errorf("原因未保存: %q", reason)
	}
}

func TestIntegration_按对象查询(t *testing.T) {
	lg, _ := setup(t)
	ctx := context.Background()
	const target = int64(95011712519897088)

	lg.Write(ctx, audit.Entry{ActorType: audit.ActorAdmin, ActorID: 1,
		Action: "user.set_status", ObjectType: "app_user", ObjectID: target})
	lg.Write(ctx, audit.Entry{ActorType: audit.ActorAdmin, ActorID: 1,
		Action: "admin.create", ObjectType: "admin_user", ObjectID: 999})

	got, err := lg.List(ctx, audit.Query{ObjectType: "app_user", ObjectID: target})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("按对象过滤应得 1 条，实际 %d", len(got))
	}
	if got[0].Action != "user.set_status" {
		t.Errorf("action = %q", got[0].Action)
	}

	all, err := lg.List(ctx, audit.Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("无过滤应返回 2 条，实际 %d", len(all))
	}
}

// 可选字段为空时不应写成 0 或空串，便于区分"没有"与"是 0"。
func TestIntegration_空可选字段存为null(t *testing.T) {
	lg, pool := setup(t)
	ctx := context.Background()
	lg.Write(ctx, audit.Entry{
		ActorType: audit.ActorSystem, Action: "cleanup.run", ObjectType: "media_object",
	})
	var actorID, objectID, wsID *int64
	var reason *string
	if err := pool.QueryRow(ctx, `
		select actor_id, object_id, workspace_id, reason from audit_log`).
		Scan(&actorID, &objectID, &wsID, &reason); err != nil {
		t.Fatal(err)
	}
	if actorID != nil || objectID != nil || wsID != nil || reason != nil {
		t.Errorf("空字段应为 null，实际 actor=%v object=%v ws=%v reason=%v",
			actorID, objectID, wsID, reason)
	}
}
