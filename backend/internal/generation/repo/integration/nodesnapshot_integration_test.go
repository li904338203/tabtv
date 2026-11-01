//go:build integration

// 节点快照查询的真库测试（P08-01 的复用依据）。
//
// ⚠️ 必须用真库：`distinct on` 是 PG 方言，"每个节点取最近一次成功"
// 这条语义只有真跑才证得了 —— 写错成"取最早一次"或"取最近一次（不分成败）"，
// 编译和 mock 都看不出来。
package integration

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"
	"github.com/li904338203/tabtv/backend/internal/generation/repo"
	workflowdomain "github.com/li904338203/tabtv/backend/internal/workflow/domain"
)

// acceptFor 在指定节点上造一次受理请求。
func acceptFor(nodeID, key string) genapp.AcceptInput {
	return genapp.AcceptInput{
		WorkspaceID: ws, PayerAccountID: payer, UserID: user, ProjectID: proj,
		NodeID: nodeID, OperationCode: "text2img", ModelCode: "dev_image",
		IdempotencyKey: key, Count: 1,
		Params: json.RawMessage(`{"prompt":"一只猫","未来参数":1}`),
	}
}

// finish 直接改库把任务推到终态 —— 这里要测的是查询语义，
// 不是执行链路（那有自己的用例）。
func finish(t *testing.T, pool *pgxpool.Pool, id int64, status, at string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		update generation set status=$2, finished_at=$3::timestamptz where id=$1`,
		id, status, at); err != nil {
		t.Fatal(err)
	}
}

func TestLatestSuccessByNode_每个节点只取最近一次成功(t *testing.T) {
	svc, r, ledger, pool := setup(t, 5)
	ctx := context.Background()
	fund(t, ledger, 500)

	// 同一个节点连跑三次：失败、成功、成功。
	var ids []int64
	for _, key := range []string{"n1-a", "n1-b", "n1-c"} {
		res, err := svc.Accept(ctx, acceptFor("node-1", key))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, res.Generation.ID)
	}
	// 第 1 次失败、第 2、3 次成功（第 3 次最新）。
	finish(t, pool, ids[0], gendomain.StatusFailed, "2026-01-01T00:00:00Z")
	finish(t, pool, ids[1], gendomain.StatusSuccess, "2026-01-02T00:00:00Z")
	finish(t, pool, ids[2], gendomain.StatusSuccess, "2026-01-03T00:00:00Z")

	// 另一个节点也来一条，验证是按节点分组的。
	other, err := svc.Accept(ctx, acceptFor("node-2", "n2-a"))
	if err != nil {
		t.Fatal(err)
	}
	finish(t, pool, other.Generation.ID, gendomain.StatusSuccess, "2026-01-02T00:00:00Z")

	rows, err := r.LatestSuccessByNode(ctx, proj)
	if err != nil {
		t.Fatal(err)
	}
	byNode := map[string]repo.NodeSnapshot{}
	for _, s := range rows {
		byNode[s.NodeID] = s
	}
	if len(byNode) != 2 {
		t.Fatalf("应有 2 个节点，实际 %d: %+v", len(byNode), byNode)
	}
	if got := byNode["node-1"].GenerationID; got != ids[2] {
		t.Fatalf("node-1 应取最近一次成功（%d），实际 %d", ids[2], got)
	}
	if byNode["node-1"].GenerationID == ids[0] {
		t.Fatal("失败的那次不该被当成可复用的产出")
	}
}

func TestLatestSuccessByNode_没有成功任务时不返回(t *testing.T) {
	// ⚠️ 失败的任务没有产出。把它算进来，计划器会以为"已经有图了"，
	// 于是整张图全部 reuse —— 用户点运行什么都没发生。
	svc, r, ledger, pool := setup(t, 5)
	ctx := context.Background()
	fund(t, ledger, 100)

	res, err := svc.Accept(ctx, acceptFor("node-x", "x-1"))
	if err != nil {
		t.Fatal(err)
	}
	finish(t, pool, res.Generation.ID, gendomain.StatusFailed, "2026-01-01T00:00:00Z")

	rows, err := r.LatestSuccessByNode(ctx, proj)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("只有失败任务时不该返回可复用产出: %+v", rows)
	}
}

// ⚠️ 这条守的是整个复用机制的接缝：计划器算"现在的设置"的指纹，
// 这里算"上次产出用的"指纹 —— **两边必须用同一个函数**。
// 各写一份的结果是它们永远对不上，于是每次运行都把所有节点重跑一遍，
// 用户每跑一次图就多花一次钱，而系统没有任何报错。
func TestLatestSuccessByNode_快照能算出与计划器一致的指纹(t *testing.T) {
	svc, r, ledger, pool := setup(t, 5)
	ctx := context.Background()
	fund(t, ledger, 100)

	res, err := svc.Accept(ctx, acceptFor("node-fp", "fp-1"))
	if err != nil {
		t.Fatal(err)
	}
	finish(t, pool, res.Generation.ID, gendomain.StatusSuccess, "2026-01-01T00:00:00Z")

	rows, err := r.LatestSuccessByNode(ctx, proj)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("应有 1 条快照，实际 %d", len(rows))
	}
	s := rows[0]

	// 快照里存的就是那次任务真正用过的参数与输入 ——
	// 它是"上次产出对应什么设置"的唯一权威来源，不需要再加一列。
	if len(s.ParamsSnapshot) == 0 {
		t.Fatal("参数快照应当落库")
	}
	fp := workflowdomain.Fingerprint(s.OperationCode, s.ModelCode,
		s.ParamsSnapshot, nil)
	if fp == "" {
		t.Fatal("指纹不该为空")
	}
	// 同一份快照算两次必须一致（否则复用永远不成立）。
	if again := workflowdomain.Fingerprint(s.OperationCode, s.ModelCode,
		s.ParamsSnapshot, nil); again != fp {
		t.Fatalf("同一份快照两次算出不同指纹: %s vs %s", fp, again)
	}
	// 快照里的未知字段也参与 —— 它们是设置的一部分。
	var m map[string]any
	if err := json.Unmarshal(s.ParamsSnapshot, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["未来参数"]; !ok {
		t.Fatal("快照应保留未知字段（GO-35），否则改了它指纹却不变")
	}
}

// ⚠️ 模型或操作被删掉之后，内连接会把这些行**整条丢掉** ——
// 表现为"复用突然失效、每次都重跑"，而没有任何报错。
// 左连接让码值为空，指纹因此与当前设置对不上，那个节点重跑：
// 宁可多跑一次，也不能把一张对不上的图当成本次结果。
func TestLatestSuccessByNode_模型被删也不丢行(t *testing.T) {
	svc, r, ledger, pool := setup(t, 5)
	ctx := context.Background()
	fund(t, ledger, 100)

	res, err := svc.Accept(ctx, acceptFor("node-del", "del-1"))
	if err != nil {
		t.Fatal(err)
	}
	finish(t, pool, res.Generation.ID, gendomain.StatusSuccess, "2026-01-01T00:00:00Z")

	// 这次受理用的 modelID/operationID 本来就不在目录里（fakeQuoter 造的），
	// 等价于"模型事后被删掉"。
	rows, err := r.LatestSuccessByNode(ctx, proj)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("目录里查不到模型也不该丢掉这一行，实际 %d 行", len(rows))
	}
	if rows[0].ModelCode != "" {
		t.Fatalf("查不到就该是空码，实际 %q", rows[0].ModelCode)
	}
	// 空码算出来的指纹与真实设置不同 → 计划器会判定重跑，这是安全方向。
	empty := workflowdomain.Fingerprint("", "", rows[0].ParamsSnapshot, nil)
	real := workflowdomain.Fingerprint("text2img", "dev_image", rows[0].ParamsSnapshot, nil)
	if empty == real {
		t.Fatal("空码不该与真实设置算出同一个指纹")
	}
}
