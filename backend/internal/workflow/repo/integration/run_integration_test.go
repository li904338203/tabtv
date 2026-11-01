//go:build integration

// 整图运行的真库测试（P08-02）。
//
// ⚠️ 必须用真库：这里守的不变量全在库里 —— 运行与节点的事务原子性、
// 节点抢占的 CAS、终态不被晚到结果覆盖、`uk_wf_node` 的唯一性。
// 用 mock 仓储的话这些都只是"测试作者相信它们成立"。
package integration

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
	"github.com/li904338203/tabtv/backend/internal/workflow/domain"
	"github.com/li904338203/tabtv/backend/internal/workflow/repo"
)

const (
	ws    = int64(9001)
	proj  = int64(9002)
	payer = int64(9003)
	user  = int64(9004)
)

func setup(t *testing.T) (*repo.Repo, func() int64) {
	t.Helper()
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	return repo.New(pool), gen.Next
}

func makeRun(t *testing.T, r *repo.Repo, next func() int64,
	nodes map[string][]string) repo.Run {
	t.Helper()
	run := repo.Run{
		ID: next(), WorkspaceID: ws, ProjectID: proj, PayerAccountID: payer,
		CreatedByUserID: user, GraphSnapshot: json.RawMessage(`{"Nodes":[]}`),
		TargetNodeIDs: json.RawMessage(`[]`), Status: string(domain.RunRunning),
	}
	rows := make([]repo.NodeRun, 0, len(nodes))
	for nodeID, deps := range nodes {
		raw, err := json.Marshal(deps)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, repo.NodeRun{
			ID: next(), RunID: run.ID, NodeID: nodeID, DependsOn: raw,
			Status: string(domain.NodePending),
		})
	}
	if err := r.InsertRun(context.Background(), run, rows); err != nil {
		t.Fatal(err)
	}
	return run
}

// ---------------------------------------------------------------- 建运行

// ⚠️ 分两次写的话，节点插一半失败会留下一个**空运行** ——
// 它永远不会推进（没有节点可调度），而用户只看到"运行中"卡死在那里。
func TestInsertRun_运行与节点同生共死(t *testing.T) {
	r, next := setup(t)
	ctx := context.Background()
	runID := next()

	// 造一对重复 node_id：`uk_wf_node` 会拒绝第二条，整个事务必须回滚。
	run := repo.Run{
		ID: runID, WorkspaceID: ws, ProjectID: proj, PayerAccountID: payer,
		CreatedByUserID: user, GraphSnapshot: json.RawMessage(`{}`),
		TargetNodeIDs: json.RawMessage(`[]`), Status: string(domain.RunRunning),
	}
	dup := []repo.NodeRun{
		{ID: next(), RunID: runID, NodeID: "a", Status: "pending"},
		{ID: next(), RunID: runID, NodeID: "a", Status: "pending"},
	}
	if err := r.InsertRun(ctx, run, dup); err == nil {
		t.Fatal("重复 node_id 应当失败")
	}
	if _, err := r.GetRunByID(ctx, runID); err == nil {
		t.Fatal("节点写失败时运行也不该留下 —— 那会是一个永远不推进的空运行")
	}
}

// ---------------------------------------------------------------- 抢占

// ⚠️ 两个调度动作同时看到同一个节点就绪时，只有一个能抢到。
// 没有 CAS 就是同一个节点被提交两次 —— 两个生成任务、两份钱。
func TestClaimNode_并发下只有一方抢到(t *testing.T) {
	r, next := setup(t)
	run := makeRun(t, r, next, map[string][]string{"a": nil})

	const racers = 8
	var wg sync.WaitGroup
	var won int64
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, _, err := r.ClaimNode(context.Background(), run.ID, "a")
			if err != nil {
				t.Errorf("抢占出错: %v", err)
				return
			}
			if ok {
				atomic.AddInt64(&won, 1)
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("%d 个并发抢占应只有 1 个成功，实际 %d", racers, won)
	}

	nodes, err := r.ListNodes(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	// attempt 也只该 +1：它是"这个节点被真正派发了几次"的依据。
	if nodes[0].Attempt != 1 {
		t.Fatalf("attempt 应只进位一次，实际 %d", nodes[0].Attempt)
	}
}

func TestClaimNode_已到终态的抢不到(t *testing.T) {
	r, next := setup(t)
	ctx := context.Background()
	run := makeRun(t, r, next, map[string][]string{"a": nil})
	if _, err := r.FinishNode(ctx, run.ID, "a", "success", "", ""); err != nil {
		t.Fatal(err)
	}
	ok, _, err := r.ClaimNode(ctx, run.ID, "a")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("已成功的节点不该被再次派发 —— 那是重复扣费")
	}
}

// ---------------------------------------------------------------- 终态

// ⚠️ 晚到的结果不能把已经取消的节点改成成功，也不能把重试过的
// 旧任务结果盖到新的上面。
func TestFinishNode_终态不被晚到的结果覆盖(t *testing.T) {
	r, next := setup(t)
	ctx := context.Background()
	run := makeRun(t, r, next, map[string][]string{"a": nil})

	ok, err := r.FinishNode(ctx, run.ID, "a", "cancelled", "cancelled", "用户取消")
	if err != nil || !ok {
		t.Fatalf("首次写终态应成功: ok=%v err=%v", ok, err)
	}
	// Worker 慢了一步，现在才回写成功。
	ok, err = r.FinishNode(ctx, run.ID, "a", "success", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("晚到的成功不该覆盖已取消的节点")
	}
	nodes, _ := r.ListNodes(ctx, run.ID)
	if nodes[0].Status != "cancelled" {
		t.Fatalf("状态应保持 cancelled，实际 %s", nodes[0].Status)
	}
}

func TestMarkNodes_只动未到终态的(t *testing.T) {
	r, next := setup(t)
	ctx := context.Background()
	run := makeRun(t, r, next, map[string][]string{"a": nil, "b": {"a"}, "c": {"b"}})
	if _, err := r.FinishNode(ctx, run.ID, "a", "success", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkNodes(ctx, run.ID, []string{"a", "b", "c"},
		"blocked", "上游没有产出"); err != nil {
		t.Fatal(err)
	}
	byID := map[string]repo.NodeRun{}
	nodes, _ := r.ListNodes(ctx, run.ID)
	for _, n := range nodes {
		byID[n.NodeID] = n
	}
	if byID["a"].Status != "success" {
		t.Fatalf("已成功的不该被批量改掉，实际 %s", byID["a"].Status)
	}
	if byID["b"].Status != "blocked" || byID["c"].Status != "blocked" {
		t.Fatalf("未到终态的应被标 blocked: b=%s c=%s",
			byID["b"].Status, byID["c"].Status)
	}
}

// ---------------------------------------------------------------- 反查与恢复

func TestNodeByGeneration_按子任务反查(t *testing.T) {
	r, next := setup(t)
	ctx := context.Background()
	run := makeRun(t, r, next, map[string][]string{"a": nil})
	if err := r.AttachGeneration(ctx, run.ID, "a", 777888, 12); err != nil {
		t.Fatal(err)
	}
	n, err := r.NodeByGeneration(ctx, 777888)
	if err != nil {
		t.Fatal(err)
	}
	if n.NodeID != "a" || n.RunID != run.ID || n.CreditsCost != 12 {
		t.Fatalf("反查结果不对: %+v", n)
	}
	// 不是整图任务时要给出可区分的错误，调用方据此安静返回。
	if _, err := r.NodeByGeneration(ctx, 999999); err == nil {
		t.Fatal("查不到时应报 ErrNodeNotFd")
	}
}

// ⚠️ 进程重启后内存里什么都没有，唯一可靠的事实是库里的状态。
// 恢复扫描只捞还在跑的 —— 把已经结束的也捞回来会无限重扫。
func TestStaleRuns_只捞还在跑的(t *testing.T) {
	r, next := setup(t)
	ctx := context.Background()
	live := makeRun(t, r, next, map[string][]string{"a": nil})
	done := makeRun(t, r, next, map[string][]string{"b": nil})
	if err := r.FinishRun(ctx, done.ID, "success", 10); err != nil {
		t.Fatal(err)
	}

	runs, err := r.StaleRuns(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != live.ID {
		t.Fatalf("只该捞到还在跑的那个: %+v", runs)
	}
}

// ⚠️ 晚到的回写不能把已经取消的运行改成成功。
func TestFinishRun_终态不被再次改写(t *testing.T) {
	r, next := setup(t)
	ctx := context.Background()
	run := makeRun(t, r, next, map[string][]string{"a": nil})

	if err := r.FinishRun(ctx, run.ID, "cancelled", 5); err != nil {
		t.Fatal(err)
	}
	if err := r.FinishRun(ctx, run.ID, "success", 99); err != nil {
		t.Fatal(err)
	}
	got, err := r.GetRunByID(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "cancelled" || got.TotalCredits != 5 {
		t.Fatalf("终态不该被改写: %s / %d", got.Status, got.TotalCredits)
	}
}

// ---------------------------------------------------------------- 快照

// ⚠️ NEW-03：运行期间用户改画布，本次运行必须按**建运行时**那份图跑。
// 不固定的话后半段按新图跑、前半段按旧图跑，产出对不上、事后也复现不了。
func TestGraphSnapshot_原样存原样取(t *testing.T) {
	r, next := setup(t)
	ctx := context.Background()
	runID := next()
	snap := json.RawMessage(`{"Nodes":[{"ID":"a","Params":{"prompt":"猫","未来字段":1}}]}`)
	if err := r.InsertRun(ctx, repo.Run{
		ID: runID, WorkspaceID: ws, ProjectID: proj, PayerAccountID: payer,
		CreatedByUserID: user, GraphSnapshot: snap,
		TargetNodeIDs: json.RawMessage(`["a"]`), Status: "running",
	}, nil); err != nil {
		t.Fatal(err)
	}
	got, err := r.GetRunByID(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	var tree map[string]any
	if err := json.Unmarshal(got.GraphSnapshot, &tree); err != nil {
		t.Fatalf("快照应是合法 JSON: %s", got.GraphSnapshot)
	}
	nodes := tree["Nodes"].([]any)
	params := nodes[0].(map[string]any)["Params"].(map[string]any)
	// 未知字段也要留着：快照是"这次运行到底执行了什么"的唯一答案。
	if _, ok := params["未来字段"]; !ok {
		t.Fatalf("快照丢了字段: %v", params)
	}
}

// ---------------------------------------------------------------- 重试

// ⚠️ 把 running 的放回 pending 会让它被再次派发 —— 同一个节点两个任务、
// 两份钱；把 success 的放回去则是在花用户的钱做他没要的事。
func TestResetNodes_只动已经不跑了且没成功的(t *testing.T) {
	r, next := setup(t)
	ctx := context.Background()
	run := makeRun(t, r, next, map[string][]string{
		"ok": nil, "bad": nil, "stuck": nil, "busy": nil,
	})
	if _, err := r.FinishNode(ctx, run.ID, "ok", "success", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.FinishNode(ctx, run.ID, "bad", "failed", "x", "上游 500"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.FinishNode(ctx, run.ID, "stuck", "blocked", "", "上游没有产出"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.ClaimNode(ctx, run.ID, "busy"); err != nil {
		t.Fatal(err)
	}

	n, err := r.ResetNodes(ctx, run.ID, []string{"ok", "bad", "stuck", "busy"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("只该重置 bad 与 stuck，实际 %d 个", n)
	}
	byID := map[string]repo.NodeRun{}
	rows, _ := r.ListNodes(ctx, run.ID)
	for _, x := range rows {
		byID[x.NodeID] = x
	}
	if byID["ok"].Status != "success" || byID["busy"].Status != "running" {
		t.Fatalf("已成功/在跑的不该被动: ok=%s busy=%s",
			byID["ok"].Status, byID["busy"].Status)
	}
	// ⚠️ 上一次的错误要清掉：留着的话界面上一个"排队中"的节点
	// 还挂着上次的失败原因，用户以为又失败了。
	if byID["bad"].ErrorMessage != "" || byID["bad"].GenerationID != 0 {
		t.Fatalf("重置应清掉上次的错误与任务引用: %+v", byID["bad"])
	}
}

// ⚠️ 不把运行放回 running 的话，恢复扫描捞不到它（只捞 pending/running），
// 那几个 pending 节点永远等不到派发。
func TestReopenRun_让已结束的运行能继续(t *testing.T) {
	r, next := setup(t)
	ctx := context.Background()
	run := makeRun(t, r, next, map[string][]string{"a": nil})
	if err := r.FinishRun(ctx, run.ID, "failed", 0); err != nil {
		t.Fatal(err)
	}
	if runs, _ := r.StaleRuns(ctx, 10); len(runs) != 0 {
		t.Fatal("前置条件：已结束的运行不该被恢复扫描捞到")
	}
	if err := r.ReopenRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	runs, err := r.StaleRuns(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("放回 running 后应能被捞到: %+v", runs)
	}
}

func TestReopenRun_已取消的不复活(t *testing.T) {
	// ⚠️ 用户取消了就是取消了 —— 重试某个节点不该把整次取消撤销。
	r, next := setup(t)
	ctx := context.Background()
	run := makeRun(t, r, next, map[string][]string{"a": nil})
	if err := r.CancelRun(ctx, run.ID, "用户取消"); err != nil {
		t.Fatal(err)
	}
	if err := r.ReopenRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := r.GetRunByID(ctx, run.ID)
	if got.Status != "cancelled" {
		t.Fatalf("已取消的运行不该被重开，实际 %s", got.Status)
	}
}

// ⚠️ 不带 attempt 的话重试会命中上一次那条失败的 generation，
// 用户点多少次"重试"都是同一个失败结果。
func TestClaimNode_attempt逐次进位(t *testing.T) {
	r, next := setup(t)
	ctx := context.Background()
	run := makeRun(t, r, next, map[string][]string{"a": nil})

	ok, attempt, err := r.ClaimNode(ctx, run.ID, "a")
	if err != nil || !ok || attempt != 1 {
		t.Fatalf("首次抢占 attempt 应为 1: ok=%v attempt=%d err=%v", ok, attempt, err)
	}
	if _, err := r.FinishNode(ctx, run.ID, "a", "failed", "x", "失败了"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ResetNodes(ctx, run.ID, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	ok, attempt, err = r.ClaimNode(ctx, run.ID, "a")
	if err != nil || !ok || attempt != 2 {
		t.Fatalf("重试后 attempt 应为 2: ok=%v attempt=%d err=%v", ok, attempt, err)
	}
}

// ---------------------------------------------------------------- 取消

// ⚠️ 先查再逐条更新的话，中间那一刻新派发的节点会漏网 ——
// 于是"取消了但还有任务在跑"。
func TestCancelRun_一次把未完成的都标掉(t *testing.T) {
	r, next := setup(t)
	ctx := context.Background()
	run := makeRun(t, r, next, map[string][]string{
		"done": nil, "busy": nil, "waiting": nil,
	})
	if _, err := r.FinishNode(ctx, run.ID, "done", "success", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.ClaimNode(ctx, run.ID, "busy"); err != nil {
		t.Fatal(err)
	}
	if err := r.CancelRun(ctx, run.ID, "用户取消"); err != nil {
		t.Fatal(err)
	}

	byID := map[string]repo.NodeRun{}
	rows, _ := r.ListNodes(ctx, run.ID)
	for _, x := range rows {
		byID[x.NodeID] = x
	}
	if byID["done"].Status != "success" {
		t.Fatalf("已成功的不该被取消改掉，实际 %s", byID["done"].Status)
	}
	if byID["busy"].Status != "cancelled" || byID["waiting"].Status != "cancelled" {
		t.Fatalf("在跑与待跑的都该取消: busy=%s waiting=%s",
			byID["busy"].Status, byID["waiting"].Status)
	}
	got, _ := r.GetRunByID(ctx, run.ID)
	if got.Status != "cancelled" {
		t.Fatalf("运行应为 cancelled，实际 %s", got.Status)
	}
}

func TestActiveGenerations_只列还在跑的(t *testing.T) {
	r, next := setup(t)
	ctx := context.Background()
	run := makeRun(t, r, next, map[string][]string{"busy": nil, "done": nil})
	if err := r.AttachGeneration(ctx, run.ID, "busy", 111, 5); err != nil {
		t.Fatal(err)
	}
	if err := r.AttachGeneration(ctx, run.ID, "done", 222, 5); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.ClaimNode(ctx, run.ID, "busy"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.FinishNode(ctx, run.ID, "done", "success", "", ""); err != nil {
		t.Fatal(err)
	}
	ids, err := r.ActiveGenerations(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	// ⚠️ 已经跑完的不能再取消：那时钱已经花在上游了，
	// 假装取消成功只会让用户以为不用付费。
	if len(ids) != 1 || ids[0] != 111 {
		t.Fatalf("只该列出还在跑的: %v", ids)
	}
}

// ---------------------------------------------------------------- 跳过

func TestMarkSkipped_只跳过没跑完的(t *testing.T) {
	r, next := setup(t)
	ctx := context.Background()
	run := makeRun(t, r, next, map[string][]string{"a": nil, "b": nil})
	if _, err := r.FinishNode(ctx, run.ID, "a", "success", "", ""); err != nil {
		t.Fatal(err)
	}
	if ok, err := r.MarkSkipped(ctx, run.ID, "a"); err != nil || ok {
		t.Fatalf("已成功的不该还能跳过: ok=%v err=%v", ok, err)
	}
	if ok, err := r.MarkSkipped(ctx, run.ID, "b"); err != nil || !ok {
		t.Fatalf("待跑的应可跳过: ok=%v err=%v", ok, err)
	}
}
