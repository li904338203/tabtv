//go:build integration

// LatestByNode 的真库测试（AGT-23 read_canvas）：`distinct on` + `left join lateral`
// 只有真跑才证得了「每节点一条、任意状态、没有产物也不丢行」。
package integration

import (
	"context"
	"testing"

	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"
	"github.com/li904338203/tabtv/backend/internal/generation/repo"
)

func TestLatestByNode_含失败与运行中且每节点一条(t *testing.T) {
	svc, r, ledger, pool := setup(t, 5)
	ctx := context.Background()
	fund(t, ledger, 500)

	acc := func(node, key, createdAt string) int64 {
		t.Helper()
		res, err := svc.Accept(ctx, acceptFor(node, key))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `update generation set created_at=$2::timestamptz where id=$1`,
			res.Generation.ID, createdAt); err != nil {
			t.Fatal(err)
		}
		return res.Generation.ID
	}
	// node-1：先成功（带两个产物），后失败 —— 最新的是失败那次，不能显示成上一次的成功。
	ok1 := acc("node-1", "a1", "2026-01-01T00:00:00Z")
	fail1 := acc("node-1", "a2", "2026-01-02T00:00:00Z")
	finish(t, pool, ok1, gendomain.StatusSuccess, "2026-01-01T00:01:00Z")
	if _, err := pool.Exec(ctx, `update generation set status='failed', error_code='provider_rejected',
		finished_at=now() where id=$1`, fail1); err != nil {
		t.Fatal(err)
	}
	// node-2：成功，两个产物，第一个（idx=0）是 777。
	ok2 := acc("node-2", "b1", "2026-01-01T00:00:00Z")
	finish(t, pool, ok2, gendomain.StatusSuccess, "2026-01-01T00:01:00Z")
	for _, o := range []struct {
		idx   int
		media int64
	}{{1, 888}, {0, 777}} {
		if _, err := pool.Exec(ctx, `insert into generation_output (id, generation_id, idx, media_object_id, output_type)
			values ($1,$2,$3,$4,'image')`, ok2*10+int64(o.idx), ok2, o.idx, o.media); err != nil {
			t.Fatal(err)
		}
	}
	// node-3：还在跑（running），没有产物。
	run3 := acc("node-3", "c1", "2026-01-03T00:00:00Z")
	if _, err := pool.Exec(ctx, `update generation set status='running' where id=$1`, run3); err != nil {
		t.Fatal(err)
	}
	// 别的工作区同项目号的任务不能漏进来（CD-04）。
	foreign := acc("node-1", "z1", "2026-01-09T00:00:00Z")
	if _, err := pool.Exec(ctx, `update generation set workspace_id=$2 where id=$1`, foreign, ws+1); err != nil {
		t.Fatal(err)
	}

	rows, err := r.LatestByNode(ctx, ws, proj)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]repo.NodeLatest{}
	for _, n := range rows {
		if _, dup := by[n.NodeID]; dup {
			t.Fatalf("节点 %s 出现两次", n.NodeID)
		}
		by[n.NodeID] = n
	}
	if len(by) != 3 {
		t.Fatalf("应有 3 个节点，实际 %d: %+v", len(by), rows)
	}
	if n := by["node-1"]; n.GenerationID != fail1 || n.Status != "failed" || n.ErrorCode != "provider_rejected" {
		t.Fatalf("node-1 应取最近一次（失败）: %+v", n)
	}
	if n := by["node-2"]; n.GenerationID != ok2 || n.OutputMediaID != 777 || n.OutputType != "image" {
		t.Fatalf("node-2 应带第一个产物 777: %+v", n)
	}
	if n := by["node-3"]; n.GenerationID != run3 || n.Status != "running" || n.OutputMediaID != 0 {
		t.Fatalf("node-3 运行中且无产物也要在: %+v", n)
	}
}
