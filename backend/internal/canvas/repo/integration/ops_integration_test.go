//go:build integration

// 画布操作（AGT-10）的真库测试。
//
// ⚠️ 必须用真库、真并发：行锁、批次幂等、版本连续都由 SQL 决定；
// 顺序调用永远先撞上状态检查，那种测试把 `for update` 删掉照样绿（CLAUDE.md §5.7）。
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	canvasapp "github.com/li904338203/tabtv/backend/internal/canvas/app"
	"github.com/li904338203/tabtv/backend/internal/canvas/domain"
	"github.com/li904338203/tabtv/backend/internal/canvas/repo"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

const (
	opsUser    = int64(1001)
	opsSession = int64(7001)
)

func agentActor() domain.Actor {
	return domain.Actor{Kind: domain.ActorAgent, UserID: opsUser, SessionID: opsSession}
}

func addNodeOp(title string) []domain.Op {
	return []domain.Op{{Kind: domain.OpAddNode,
		Node: json.RawMessage(`{"type":"text","title":"` + title + `","params":{}}`)}}
}

func bizCode(err error) errcode.Code {
	var be *httpx.BizError
	if errors.As(err, &be) {
		return be.Code
	}
	return 0
}

func TestApplyOps_同批次幂等(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	svc := canvasapp.New(r, next)
	id := mustInsert(t, r, next, ws, "幂等")

	in := canvasapp.ApplyOpsInput{WorkspaceID: ws, ProjectID: id, Actor: agentActor(),
		BatchID: "s7001:step1", Ops: addNodeOp("规划")}
	first, err := svc.ApplyOps(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if first.FromVersion != 1 || first.ToVersion != 2 || len(first.NodeIDs) != 1 || first.Replayed {
		t.Fatalf("第一次结果不对: %+v", first)
	}
	if !strings.HasPrefix(first.NodeIDs[0], "a_") {
		t.Errorf("服务端分配的节点 ID 应以 a_ 开头，得到 %s", first.NodeIDs[0])
	}

	// 并发重放同一批：行锁之后查到日志，只写一次。
	var wg sync.WaitGroup
	results := make([]canvasapp.ApplyOpsResult, 6)
	errs := make([]error, 6)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = svc.ApplyOps(ctx, in)
		}(i)
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("重放 %d 失败: %v", i, errs[i])
		}
		got := results[i]
		if !got.Replayed || got.FromVersion != 1 || got.ToVersion != 2 ||
			fmt.Sprint(got.NodeIDs) != fmt.Sprint(first.NodeIDs) {
			t.Errorf("重放 %d 应返回第一次的结果，得到 %+v", i, got)
		}
	}
	var logs, version int
	_ = pool.QueryRow(ctx, `select count(*) from canvas_op_log where project_id = $1`, id).Scan(&logs)
	_ = pool.QueryRow(ctx, `select version from project where id = $1`, id).Scan(&version)
	if logs != 1 || version != 2 {
		t.Fatalf("同批次只该写一次：日志 %d 行、版本 %d", logs, version)
	}
}

// 前端「补取 → 重放 → 重存」的最小实现（只认本测试会出现的 add_node）。
func replay(t *testing.T, doc json.RawMessage, batches []repo.OpLogEntry) json.RawMessage {
	t.Helper()
	var tree map[string]any
	if err := json.Unmarshal(doc, &tree); err != nil {
		t.Fatal(err)
	}
	nodes, _ := tree["nodes"].([]any)
	for _, b := range batches {
		var ops []domain.Op
		if err := json.Unmarshal(b.Ops, &ops); err != nil {
			t.Fatal(err)
		}
		for _, op := range ops {
			if op.Kind != domain.OpAddNode {
				t.Fatalf("测试重放只认 add_node，得到 %s", op.Kind)
			}
			var n map[string]any
			_ = json.Unmarshal(op.Node, &n)
			nodes = append(nodes, n)
		}
	}
	tree["nodes"] = nodes
	out, _ := json.Marshal(tree)
	return out
}

func withUserNode(t *testing.T, doc json.RawMessage, nodeID string) json.RawMessage {
	t.Helper()
	var tree map[string]any
	if len(doc) == 0 {
		doc = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(doc, &tree); err != nil {
		t.Fatal(err)
	}
	nodes, _ := tree["nodes"].([]any)
	nodes = append(nodes, map[string]any{"id": nodeID, "type": "image", "title": nodeID,
		"position": map[string]any{"x": 0, "y": 0}, "size": map[string]any{"w": 330, "h": 180},
		"params": map[string]any{}})
	tree["nodes"] = nodes
	out, _ := json.Marshal(tree)
	return out
}

func TestApplyOps_与SaveCanvas并发版本连续且改动都在(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	svc := canvasapp.New(r, next)
	id := mustInsert(t, r, next, ws, "并发")

	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 2*n)
	var reloads, replays sync.Map

	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := svc.ApplyOps(ctx, canvasapp.ApplyOpsInput{WorkspaceID: ws, ProjectID: id,
				Actor: agentActor(), BatchID: fmt.Sprintf("b%d", i), Ops: addNodeOp(fmt.Sprintf("agent-%d", i))})
			if err != nil {
				errs <- fmt.Errorf("ApplyOps %d: %w", i, err)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			<-start
			nodeID := fmt.Sprintf("u_%d", i)
			p, err := r.GetWithCanvas(ctx, ws, id)
			if err != nil {
				errs <- err
				return
			}
			doc, version := withUserNode(t, p.CanvasData, nodeID), p.Version
			for attempt := 0; attempt < 200; attempt++ {
				_, err := svc.SaveCanvas(ctx, ws, id, opsUser+int64(i)+1, collab.RoleEditor, version, doc)
				if err == nil {
					return
				}
				if bizCode(err) != errcode.VersionConflict {
					errs <- fmt.Errorf("SaveCanvas %d: %w", i, err)
					return
				}
				// 补取 → 重放 → 重存；中间夹着别人的真人保存时只能整份重载再把自己的改动做一遍。
				res, err := svc.OpsSince(ctx, ws, id, collab.RoleEditor, version)
				if err != nil {
					errs <- err
					return
				}
				if res.NeedsReload {
					reloads.Store(i, true)
					p, err := r.GetWithCanvas(ctx, ws, id)
					if err != nil {
						errs <- err
						return
					}
					doc, version = withUserNode(t, p.CanvasData, nodeID), p.Version
					continue
				}
				replays.Store(i, true)
				doc = replay(t, doc, res.Batches)
				version = res.CurrentVersion
			}
			errs <- fmt.Errorf("SaveCanvas %d 重试耗尽", i)
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		return
	}

	// 版本严格连续：16 次成功写入，每次恰好 +1。
	p, _ := r.GetWithCanvas(ctx, ws, id)
	if p.Version != 1+2*n {
		t.Fatalf("最终版本应为 %d，实际 %d", 1+2*n, p.Version)
	}
	// 两边的改动都在：8 个 Agent 节点 + 8 个真人节点，一个都没被覆盖掉。
	var tree struct {
		Nodes []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(p.CanvasData, &tree); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, nd := range tree.Nodes {
		seen[nd.Title]++
	}
	for i := 0; i < n; i++ {
		if seen[fmt.Sprintf("agent-%d", i)] != 1 || seen[fmt.Sprintf("u_%d", i)] != 1 {
			t.Errorf("第 %d 组改动丢了或重复了: %v", i, seen)
		}
	}
	if len(tree.Nodes) != 2*n {
		t.Errorf("应有 %d 个节点，实际 %d", 2*n, len(tree.Nodes))
	}

	// 操作日志：每批 to = from + 1，to_version 不重号，且都落在 (1, 17] 之内。
	rows, err := pool.Query(ctx,
		`select from_version, to_version from canvas_op_log where project_id = $1 order by to_version`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var tos []int
	for rows.Next() {
		var from, to int
		if err := rows.Scan(&from, &to); err != nil {
			t.Fatal(err)
		}
		if to != from+1 || to < 2 || to > 1+2*n {
			t.Errorf("日志版本异常: %d → %d", from, to)
		}
		tos = append(tos, to)
	}
	if len(tos) != n {
		t.Fatalf("应有 %d 条日志，实际 %d", n, len(tos))
	}
	if !sort.IntsAreSorted(tos) {
		t.Errorf("to_version 未按序: %v", tos)
	}
	for i := 1; i < len(tos); i++ {
		if tos[i] == tos[i-1] {
			t.Errorf("to_version 重号: %v", tos)
		}
	}
	countMap := func(m *sync.Map) int {
		c := 0
		m.Range(func(_, _ any) bool { c++; return true })
		return c
	}
	t.Logf("真人保存走补取重放 %d 次、整份重载 %d 次", countMap(&replays), countMap(&reloads))
}

func TestApplyOps_跨工作区与回收站项目拒绝(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	svc := canvasapp.New(r, next)
	id := mustInsert(t, r, next, ws, "别人的")

	_, err := svc.ApplyOps(ctx, canvasapp.ApplyOpsInput{WorkspaceID: ws + 1, ProjectID: id,
		Actor: agentActor(), BatchID: "x1", Ops: addNodeOp("越权")})
	if bizCode(err) != errcode.NotFound {
		t.Fatalf("跨工作区应报未找到，得到 %v", err)
	}

	if err := r.SoftDelete(ctx, ws, id); err != nil {
		t.Fatal(err)
	}
	_, err = svc.ApplyOps(ctx, canvasapp.ApplyOpsInput{WorkspaceID: ws, ProjectID: id,
		Actor: agentActor(), BatchID: "x2", Ops: addNodeOp("回收站")})
	if bizCode(err) != errcode.NotFound {
		t.Fatalf("回收站项目应报未找到，得到 %v", err)
	}
	var logs int
	_ = pool.QueryRow(ctx, `select count(*) from canvas_op_log`).Scan(&logs)
	if logs != 0 {
		t.Errorf("被拒绝的写入不该留日志，实际 %d 条", logs)
	}

	// 所有权不符：映射成 CANVAS_OP_NOT_OWNED，且仍能 errors.Is 到领域错误；版本不动。
	id2 := mustInsert(t, r, next, ws, "用户的节点")
	if _, err := svc.SaveCanvas(ctx, ws, id2, opsUser, collab.RoleEditor, 1,
		json.RawMessage(`{"nodes":[{"id":"n_user","type":"text","params":{}}],"edges":[]}`)); err != nil {
		t.Fatal(err)
	}
	_, err = svc.ApplyOps(ctx, canvasapp.ApplyOpsInput{WorkspaceID: ws, ProjectID: id2, Actor: agentActor(),
		BatchID: "x3", Ops: []domain.Op{{Kind: domain.OpUpdateNode, NodeID: "n_user",
			Patch: json.RawMessage(`{"title":"改"}`)}}})
	if bizCode(err) != errcode.CanvasOpNotOwned || !errors.Is(err, domain.ErrNotOwned) {
		t.Fatalf("改用户节点应报 CANVAS_OP_NOT_OWNED，得到 %v", err)
	}
	p, _ := r.Get(ctx, ws, id2)
	if p.Version != 2 {
		t.Errorf("被拒绝的批次不该推进版本，实际 %d", p.Version)
	}
}

func TestOpsSince_中间有人工保存需重载(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	svc := canvasapp.New(r, next)
	id := mustInsert(t, r, next, ws, "补取")

	apply := func(batch string) canvasapp.ApplyOpsResult {
		t.Helper()
		res, err := svc.ApplyOps(ctx, canvasapp.ApplyOpsInput{WorkspaceID: ws, ProjectID: id,
			Actor: agentActor(), BatchID: batch, Ops: addNodeOp(batch)})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	apply("b1") // 1 → 2
	apply("b2") // 2 → 3
	p, _ := r.GetWithCanvas(ctx, ws, id)
	if _, err := svc.SaveCanvas(ctx, ws, id, opsUser, collab.RoleEditor, p.Version, p.CanvasData); err != nil {
		t.Fatal(err) // 3 → 4（真人）
	}
	apply("b3") // 4 → 5

	cases := []struct {
		since   int
		reload  bool
		batches []string
	}{
		{1, true, nil}, // 区间里夹着真人保存
		{3, true, nil}, // 从真人保存那一版之前开始
		{4, false, []string{"b3"}},
		{5, false, nil}, // 已是最新
		{9, true, nil},  // 客户端比服务端新
	}
	for _, c := range cases {
		res, err := svc.OpsSince(ctx, ws, id, collab.RoleViewer, c.since)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, b := range res.Batches {
			got = append(got, b.BatchID)
		}
		if res.NeedsReload != c.reload || fmt.Sprint(got) != fmt.Sprint(c.batches) || res.CurrentVersion != 5 {
			t.Errorf("since=%d: 得到 reload=%v batches=%v current=%d，期望 reload=%v batches=%v",
				c.since, res.NeedsReload, got, res.CurrentVersion, c.reload, c.batches)
		}
	}
	if _, err := svc.OpsSince(ctx, ws+1, id, collab.RoleViewer, 1); bizCode(err) != errcode.NotFound {
		t.Errorf("跨工作区补取应报未找到，得到 %v", err)
	}
}

func TestSaveCanvas_清空Agent编辑者(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	svc := canvasapp.New(r, next)
	id := mustInsert(t, r, next, ws, "编辑者")

	if _, err := svc.ApplyOps(ctx, canvasapp.ApplyOpsInput{WorkspaceID: ws, ProjectID: id,
		Actor: agentActor(), BatchID: "b1", Ops: addNodeOp("a")}); err != nil {
		t.Fatal(err)
	}
	st, err := svc.CanvasState(ctx, ws, id, collab.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	if st.EditorKind != repo.EditorAgent || st.AgentSessionID != opsSession || st.EditorID != opsUser {
		t.Fatalf("Agent 写入后应为 agent/%d/发起人 %d，得到 %+v", opsSession, opsUser, st)
	}
	// 发起人自己的标签页拿着旧版本保存：冲突详情要说是 Agent 改的，且 bySelf=false
	// （否则他会点「以我为准」把 Agent 刚加的节点整份覆盖掉）。
	_, err = svc.SaveCanvas(ctx, ws, id, opsUser, collab.RoleEditor, 1, json.RawMessage(`{"nodes":[]}`))
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.VersionConflict {
		t.Fatalf("应冲突，得到 %v", err)
	}
	if be.Details["editorKind"] != repo.EditorAgent || be.Details["agentSessionId"] != "7001" ||
		be.Details["bySelf"] != false {
		t.Errorf("冲突详情不对: %v", be.Details)
	}

	if _, err := svc.SaveCanvas(ctx, ws, id, opsUser, collab.RoleEditor, 2, json.RawMessage(`{"nodes":[]}`)); err != nil {
		t.Fatal(err)
	}
	st, _ = svc.CanvasState(ctx, ws, id, collab.RoleViewer)
	if st.EditorKind != repo.EditorUser || st.AgentSessionID != 0 || !canvasapp.BySelf(st, opsUser) {
		t.Fatalf("真人保存后应为 user 且清空会话，得到 %+v", st)
	}
}

func TestCleanupOpLog_只删过期且分批(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	svc := canvasapp.New(r, next)
	id := mustInsert(t, r, next, ws, "清理")
	for i := 0; i < 5; i++ {
		if _, err := svc.ApplyOps(ctx, canvasapp.ApplyOpsInput{WorkspaceID: ws, ProjectID: id,
			Actor: agentActor(), BatchID: fmt.Sprintf("b%d", i), Ops: addNodeOp("x")}); err != nil {
			t.Fatal(err)
		}
	}
	// 前 3 批（to_version 2..4）造成 40 天前。
	if _, err := pool.Exec(ctx, `update canvas_op_log set created_at = now() - interval '40 days'
		where project_id = $1 and to_version <= 4`, id); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	n, err := svc.CleanupOpLog(ctx, canvasapp.DefaultOpLogRetention, 2, now)
	if err != nil || n != 2 {
		t.Fatalf("第一批应删 2 行，得到 %d（%v）", n, err)
	}
	n, err = svc.CleanupOpLog(ctx, canvasapp.DefaultOpLogRetention, 2, now)
	if err != nil || n != 1 {
		t.Fatalf("第二批应删剩下的 1 行，得到 %d（%v）", n, err)
	}
	n, _ = svc.CleanupOpLog(ctx, canvasapp.DefaultOpLogRetention, 2, now)
	if n != 0 {
		t.Fatalf("没有过期行时应删 0 行，得到 %d", n)
	}
	var left int
	_ = pool.QueryRow(ctx, `select count(*) from canvas_op_log where project_id = $1`, id).Scan(&left)
	if left != 2 {
		t.Errorf("未过期的 2 行应保留，实际剩 %d", left)
	}
	// 被清掉的区间补取时要求整页重载。
	res, _ := svc.OpsSince(ctx, ws, id, collab.RoleViewer, 1)
	if !res.NeedsReload {
		t.Error("补取跨过已清理的批次应 needsReload")
	}
	res, _ = svc.OpsSince(ctx, ws, id, collab.RoleViewer, 4)
	if res.NeedsReload || len(res.Batches) != 2 {
		t.Errorf("保留期内的区间应能完整补取，得到 %+v", res)
	}
}

// link_generation（AGT-24 缺陷修复）：受理后把任务挂到节点 result 上，走与其他操作同一条
// 「行锁 → 应用 → 版本 +1 → 写日志」的路径；补取接口能拿到它（前端靠它重放）。
func TestApplyOps_LinkGeneration写结果与操作日志(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	svc := canvasapp.New(r, next)
	id := mustInsert(t, r, next, ws, "挂任务")

	created, err := svc.ApplyOps(ctx, canvasapp.ApplyOpsInput{WorkspaceID: ws, ProjectID: id, Actor: agentActor(),
		BatchID: "agent:7001:1:1", Ops: addNodeOp("视频")})
	if err != nil {
		t.Fatal(err)
	}
	nodeID := created.NodeIDs[0]
	link := canvasapp.ApplyOpsInput{WorkspaceID: ws, ProjectID: id, Actor: agentActor(),
		BatchID: "agent:7001:1:1:link", Ops: []domain.Op{{Kind: domain.OpLinkGeneration, NodeID: nodeID,
			Patch: json.RawMessage(`{"generationId":"9007199254740993"}`)}}}
	res, err := svc.ApplyOps(ctx, link)
	if err != nil {
		t.Fatal(err)
	}
	if res.FromVersion != 2 || res.ToVersion != 3 || len(res.NodeIDs) != 0 {
		t.Fatalf("结果不对: %+v", res)
	}
	var result json.RawMessage
	if err := pool.QueryRow(ctx, `select n->'result' from project, jsonb_array_elements(canvas_data->'nodes') n
		where id = $1 and n->>'id' = $2`, id, nodeID).Scan(&result); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(result, &got)
	if got["id"] != "9007199254740993" || got["status"] != "queued" {
		t.Errorf("节点 result 应挂上任务（ID 为字符串、不丢精度）：%s", result)
	}
	since, err := svc.OpsSince(ctx, ws, id, collab.RoleViewer, 2)
	if err != nil || since.NeedsReload || len(since.Batches) != 1 ||
		!strings.Contains(string(since.Batches[0].Ops), "link_generation") {
		t.Fatalf("补取应拿到 link_generation 那一批: %+v %v", since, err)
	}

	// 节点已被删（或从未存在）→ 整批拒绝。
	_, err = svc.ApplyOps(ctx, canvasapp.ApplyOpsInput{WorkspaceID: ws, ProjectID: id, Actor: agentActor(),
		BatchID: "agent:7001:2:1:link", Ops: []domain.Op{{Kind: domain.OpLinkGeneration, NodeID: "n_none",
			Patch: json.RawMessage(`{"generationId":"1"}`)}}})
	if bizCode(err) != errcode.InvalidArgument {
		t.Errorf("不存在的节点应 INVALID_ARGUMENT：%v", err)
	}
}
